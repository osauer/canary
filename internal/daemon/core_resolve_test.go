package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// coreFakeListings is a fake broker lookup: it records every request and
// answers with the listings given (or the error).
type coreFakeListings struct {
	asked    []ibkrlib.ListingRequest
	listings []ibkrlib.Listing
	trace    *ibkrlib.BondLookupTrace
	err      error
}

func (f *coreFakeListings) lookup(_ context.Context, r ibkrlib.ListingRequest) ([]ibkrlib.Listing, *ibkrlib.BondLookupTrace, error) {
	f.asked = append(f.asked, r)
	return f.listings, f.trace, f.err
}

func coreFakeBroker() *coreFakeListings {
	return &coreFakeListings{
		listings: []ibkrlib.Listing{
			{ConID: coreTestConID, Symbol: "IBCF1", LocalSymbol: "IBCF", SecType: "STK", Exchange: "IBIS", PrimaryExch: "IBIS", Currency: "EUR", TradingClass: "IBCF", LongName: "Synthetic Index Fund", ISIN: core.ISIN, Forms: []string{"STK by ISIN on SMART"}},
			{ConID: 9031, Symbol: "IUSE", SecType: "STK", Exchange: "SMART", PrimaryExch: "LSEETF", Currency: "EUR", ISIN: core.ISIN, Forms: []string{"STK by ISIN on SMART"}},
		},
		trace: &ibkrlib.BondLookupTrace{Attempts: []ibkrlib.BondLookupAttempt{{Form: "STK by ISIN on SMART", Outcome: ibkrlib.BondAttemptLine, Lines: 2}, {Form: "STK IBCF on IBIS2", Outcome: ibkrlib.BondAttemptRejected, Code: 200, Message: "No security definition has been found for the request"}}},
	}
}

// core.resolve asks by the rule's ISIN with the symbol fallbacks on IBIS,
// IBIS2 and SMART with primary IBIS, lists every listing without choosing,
// marks the pinned one, and takes the ISIN from the flag, else the policy,
// else the rule. An invalid ISIN is refused; no listing is an answer with
// every form's answer, not an error.
func TestCoreResolveListsEveryListing(t *testing.T) {
	c := testCoreConstitution()
	c.Mandate.Core.ConID = new(coreTestConID)
	srv, _ := mandateTestServer(t, c)
	fake := coreFakeBroker()
	srv.coreListingsForTest = fake.lookup
	res, err := srv.handleCoreResolve(t.Context(), &rpc.Request{})
	if err != nil || len(res.Listings) != 2 || res.ISIN != core.ISIN || res.ISINSource != "policy" || !res.PinnedFound || !res.Listings[0].Pinned || res.Listings[1].Pinned || len(res.Attempts) != 2 {
		t.Fatalf("resolve %+v err %v", res, err)
	}
	asked := fake.asked[0]
	if asked.ISIN != core.ISIN || !slices.Equal(asked.Symbols, []string{core.Symbol}) || len(asked.Venues) != 3 || asked.Venues[2] != (ibkrlib.ListingVenue{Exchange: "SMART", PrimaryExch: "IBIS"}) {
		t.Fatalf("asked %+v", asked)
	}
	if !strings.Contains(res.Message, "nothing to write") {
		t.Fatalf("message %q", res.Message)
	}
	// Unpinned: several listings, none chosen.
	c.Mandate.Core.ConID = new(0)
	res, _ = srv.handleCoreResolve(t.Context(), &rpc.Request{})
	if res.PinnedConID != nil || res.PinnedFound || !strings.Contains(res.Message, "none is chosen for you") {
		t.Fatalf("unpinned %+v", res)
	}
	// The flag's ISIN, another fund: no symbol fallback, the currency sent.
	res, err = srv.handleCoreResolve(t.Context(), &rpc.Request{Params: []byte(`{"isin":"IE00B5BMR087","currency":"eur"}`)})
	if err != nil || res.ISINSource != "flag" || fake.asked[2].ISIN != "IE00B5BMR087" || len(fake.asked[2].Symbols) != 0 || fake.asked[2].Currency != "EUR" {
		t.Fatalf("flag %+v %+v err %v", res, fake.asked[2], err)
	}
	if _, err := srv.handleCoreResolve(t.Context(), &rpc.Request{Params: []byte(`{"isin":"IE00B3ZW0K17"}`)}); err == nil || !strings.Contains(err.Error(), "not an ISIN") {
		t.Fatalf("bad ISIN: %v", err)
	}
	// No listing: the answers are the result.
	fake.listings, fake.err = nil, ibkrlib.ErrNoListing
	res, err = srv.handleCoreResolve(t.Context(), &rpc.Request{})
	if err != nil || len(res.Listings) != 0 || len(res.Attempts) != 2 || !strings.Contains(res.Message, "found no listing") {
		t.Fatalf("none %+v err %v", res, err)
	}
	fake.err = ibkrlib.ErrIBKRUnavailable
	if _, err := srv.handleCoreResolve(t.Context(), &rpc.Request{}); !errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
		t.Fatalf("gateway down: %v", err)
	}
}

// The core's contract comes from IBKR's record for the ISIN in the
// policy's currency (owner decision 2026-10-18): asked by ISIN with no
// venue and no symbol, the one line IBKR lists is used, its series asked
// by contract id with IBKR's symbol on the listing's primary exchange
// (routed there, never SMART) and judged on that exchange's calendar. The
// resolution is kept; the contract is remembered.
func TestCoreContractFromIBKRsRecordForTheISIN(t *testing.T) {
	c := testCoreConstitution()
	srv, _ := mandateTestServer(t, c)
	fake := &coreFakeListings{listings: []ibkrlib.Listing{coreTestLondonListing()}}
	srv.coreListingsForTest = fake.lookup
	k := srv.coreResolve(t.Context(), c)
	if k == nil {
		t.Fatalf("no contract: %+v", srv.coreTargets.pins)
	}
	asked := fake.asked[0]
	if asked.ISIN != core.ISIN || asked.Currency != core.Currency || asked.ConID != 0 || len(asked.Venues) != 0 || len(asked.Symbols) != 0 {
		t.Fatalf("asked %+v", asked)
	}
	if k.ConID != 9031 || k.Symbol != "IXSE" || k.PrimaryExch != "LSEETF" || k.TradingClass != "EXXT" || k.Market != marketcal.MarketUKLSE || k.ISIN != core.ISIN {
		t.Fatalf("contract %+v", k)
	}
	need := coreSeriesNeed(k)
	want := rpc.ContractParams{ConID: 9031, Symbol: "IXSE", SecType: "STK", Currency: "EUR", Exchange: "LSEETF", PrimaryExch: "LSEETF"}
	if need.Key != coreSeriesKey(9031) || need.Contract != want || need.Market != marketcal.MarketUKLSE {
		t.Fatalf("need %+v", need)
	}
	var got rpc.MarketHistoryParams
	srv.mandateHistoryFetchForTest = func(_ context.Context, p rpc.MarketHistoryParams, _ int, _ time.Time) (*rpc.MarketHistoryResult, error) {
		got = p
		return nil, errors.New("synthetic stop")
	}
	_, _ = srv.fetchMandateSeries(t.Context(), need, mandateTestNow)
	contract, _, routed, err := normaliseStockQuoteContract(got.Contract)
	if err != nil || !routed || contract.ConID != 9031 || contract.Symbol != "IXSE" || contract.Exchange != "LSEETF" || contract.PrimaryExch != "LSEETF" || contract.Currency != "EUR" {
		t.Fatalf("broker contract %+v routed %v err %v", contract, routed, err)
	}
	// Kept: the next pass asks nothing; remembered as the line used.
	if again := srv.coreResolve(t.Context(), c); again == nil || again.ConID != 9031 || len(fake.asked) != 1 {
		t.Fatalf("asked again: %d", len(fake.asked))
	}
	if used := srv.coreUsed(t.Context(), core.ISIN, core.Currency); used == nil || used.ConID != 9031 {
		t.Fatalf("remembered %+v", used)
	}
	// The mandate knows the line as the fund; protection leaves it out.
	if lines := srv.registerFundLines(t.Context()); lines["con:9031"] != core.ISIN {
		t.Fatalf("fund lines %v", lines)
	}
	if ids := srv.coreContractIDs(t.Context()); !slices.Equal(ids, []int{9031}) {
		t.Fatalf("protection exclusion %v", ids)
	}
	// After six hours IBKR is asked again.
	srv.now = func() time.Time { return mandateTestNow.Add(corePinResolveEvery) }
	srv.coreResolve(t.Context(), c)
	if len(fake.asked) != 2 {
		t.Fatalf("not asked again after %s: %d", corePinResolveEvery, len(fake.asked))
	}
}

// Every answer the core cannot use holds it and names why: several lines
// in the currency (each named, none chosen), none, a pinned id that is not
// among the ISIN's lines, a line on a venue Canary has no calendar for, a
// written exchange IBKR does not list it on, and a line other than the one
// the core last used. A written con_id chooses among several and accepts a
// changed line; a lookup that gets no answer keeps the line it had.
func TestCoreContractHoldsOnWhatItCannotUse(t *testing.T) {
	c := testCoreConstitution()
	srv, _ := mandateTestServer(t, c)
	resolve := func(listings []ibkrlib.Listing, err error) (*coreContract, string) {
		t.Helper()
		srv.coreListingsForTest = (&coreFakeListings{listings: listings, err: err}).lookup
		srv.coreTargets.mu.Lock()
		srv.coreTargets.pins = nil
		srv.coreTargets.mu.Unlock()
		k := srv.coreResolve(t.Context(), c)
		_, hold, _, _ := srv.corePinState(c.Mandate.Core)
		return k, hold
	}
	xetra, london := coreTestListing(), coreTestLondonListing()
	if k, hold := resolve([]ibkrlib.Listing{xetra, london}, nil); k != nil || !strings.Contains(hold, "IBKR lists 2 EUR lines under IE00B3ZW0K18: contract 9030 IBCF on IBIS; contract 9031 IXSE (class EXXT) on LSEETF") || !strings.Contains(hold, "write the con_id") {
		t.Fatalf("several: %+v %q", k, hold)
	}
	if srv.coreUsed(t.Context(), core.ISIN, core.Currency) != nil {
		t.Fatal("an ambiguous answer was remembered")
	}
	// Lines in another currency, or found only by symbol, are not the ISIN's.
	usd := london
	usd.ConID, usd.Currency = 9033, "USD"
	bySymbol := xetra
	bySymbol.ConID, bySymbol.ISIN, bySymbol.Forms = 9034, "", []string{"STK IBCF on IBIS"}
	if k, hold := resolve([]ibkrlib.Listing{london, usd, bySymbol}, nil); k == nil || k.ConID != 9031 || hold != "" {
		t.Fatalf("one EUR line by ISIN: %+v %q", k, hold)
	}
	// IBKR now lists another line: the core holds and names both.
	if k, hold := resolve([]ibkrlib.Listing{xetra}, nil); k != nil || !strings.Contains(hold, "IBKR now lists contract 9030 IBCF on IBIS under IE00B3ZW0K18 in EUR; the core last used contract 9031 IXSE (class EXXT) on LSEETF") {
		t.Fatalf("changed: %+v %q", k, hold)
	}
	// The owner's pin accepts it, and it is remembered.
	c.Mandate.Core.ConID = new(coreTestConID)
	if k, hold := resolve([]ibkrlib.Listing{xetra, london}, nil); k == nil || k.ConID != coreTestConID || hold != "" {
		t.Fatalf("pinned among two: %+v %q", k, hold)
	}
	if used := srv.coreUsed(t.Context(), core.ISIN, core.Currency); used == nil || used.ConID != coreTestConID {
		t.Fatalf("remembered %+v", used)
	}
	c.Mandate.Core.ConID = new(9099)
	if k, hold := resolve([]ibkrlib.Listing{xetra, london}, nil); k != nil || !strings.Contains(hold, "the pinned contract 9099 is not among the EUR lines IBKR lists under IE00B3ZW0K18") {
		t.Fatalf("pin not listed: %+v %q", k, hold)
	}
	c.Mandate.Core.ConID = nil
	if k, hold := resolve(nil, ibkrlib.ErrNoListing); k != nil || !strings.Contains(hold, "IBKR lists no EUR line under IE00B3ZW0K18") {
		t.Fatalf("none: %+v %q", k, hold)
	}
	unlisted := xetra
	unlisted.PrimaryExch = "XYZEX"
	if k, hold := resolve([]ibkrlib.Listing{unlisted}, nil); k != nil || !strings.Contains(hold, "Canary carries no exchange calendar for XYZEX") {
		t.Fatalf("no calendar: %+v %q", k, hold)
	}
	// A written exchange must be one IBKR lists the line on; IBIS for the
	// London line is the policy of 2026-10-16.
	c.Mandate.Core.ConID = new(9031)
	c.Mandate.Core.Exchange = "IBIS"
	if k, hold := resolve([]ibkrlib.Listing{london}, nil); k != nil || !strings.Contains(hold, "the policy names exchange IBIS, and IBKR does not list contract 9031 IXSE (class EXXT) on LSEETF there") {
		t.Fatalf("exchange not listed: %+v %q", k, hold)
	}
	c.Mandate.Core.Exchange = "SMART"
	if k, hold := resolve([]ibkrlib.Listing{london}, nil); k == nil || hold != "" || k.PrimaryExch != "LSEETF" {
		t.Fatalf("a written exchange IBKR lists: %+v %q", k, hold)
	}
	// No answer keeps the line, says so, and is asked again after 15 minutes.
	fake := &coreFakeListings{err: ibkrlib.ErrIBKRUnavailable}
	srv.coreListingsForTest = fake.lookup
	srv.now = func() time.Time { return mandateTestNow.Add(corePinResolveEvery) }
	if k := srv.coreResolve(t.Context(), c); k == nil || k.ConID != 9031 {
		t.Fatalf("a lookup without an answer dropped the line: %+v", k)
	}
	if _, _, lookupErr, _ := srv.corePinState(c.Mandate.Core); lookupErr == "" {
		t.Fatal("the failed lookup is not named")
	}
	srv.coreResolve(t.Context(), c)
	srv.now = func() time.Time { return mandateTestNow.Add(corePinResolveEvery + corePinRetryAfter) }
	srv.coreResolve(t.Context(), c)
	if len(fake.asked) != 2 {
		t.Fatalf("asked %d times, want 2", len(fake.asked))
	}
}

// The line the core last used survives a restart: after one, IBKR
// answering another line holds the core.
func TestCoreContractRememberedAcrossARestart(t *testing.T) {
	c := testCoreConstitution()
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	srv.coreTargets = coreTargetStore{}
	other := coreTestLondonListing()
	coreAnswer(t, srv, other)
	if _, hold, _, _ := srv.corePinState(c.Mandate.Core); !strings.Contains(hold, "the core last used contract 9030 IBCF on IBIS") {
		t.Fatalf("after a restart: %q", hold)
	}
	if res := coreEvaluate(t, srv); res.Status != rpc.CoreStatusHold || res.Instrument.Resolved || !strings.Contains(strings.Join(res.Reasons, " "), "IBKR now lists contract 9031") {
		t.Fatalf("evaluation: %s %v", res.Status, res.Reasons)
	}
	// D5 still leaves out the line the core last used.
	if ids := srv.coreContractIDs(t.Context()); !slices.Equal(ids, []int{coreTestConID}) {
		t.Fatalf("protection exclusion %v", ids)
	}
}
