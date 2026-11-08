package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/register"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A strategy's contract (internal-docs/design/core-sleeve.md, "Instrument
// and series"; every register entry since the evaluator). The policy names
// the fund and its currency; IBKR's contract record names the rest (owner
// decision 2026-10-18 08:06 CEST). The daemon asks IBKR for every line it
// lists under the ISIN in that currency and uses the one it answers: the
// series is read for that contract id with IBKR's symbol on the listing's
// primary exchange, and sessions, freshness and the decision day are judged
// on that exchange's calendar. It holds, naming every candidate, when IBKR
// lists more than one line, and holds when the line differs from the one
// the strategy last used; in both cases the owner chooses by writing
// con_id. A written con_id must be one of the ISIN's lines, and a written
// exchange a venue IBKR lists it on. One pin per instrument request, so
// the core and every later entry are resolved alike.

const (
	// coreResolveFormTimeout bounds one contract-details form.
	coreResolveFormTimeout = 8 * time.Second
	// corePinResolveEvery is how often IBKR is asked again after an
	// answer; corePinRetryAfter spaces lookups that got none.
	corePinResolveEvery = 6 * time.Hour
	corePinRetryAfter   = 15 * time.Minute
	// stateKindCoreContract keeps the contract the core last used, per
	// ISIN and currency.
	stateKindCoreContract = "core_contract"
	coreContractDocVer    = 1
)

// coreResolveVenues are the venues `canary core resolve` asks the rule's
// own symbol on, beside the ISIN: Xetra (IBIS), Xetra's second segment
// (IBIS2) and SMART with Xetra as the primary exchange, where the owner
// first named the fund.
var coreResolveVenues = []ibkrlib.ListingVenue{{Exchange: "IBIS"}, {Exchange: "IBIS2"}, {Exchange: "SMART", PrimaryExch: "IBIS"}}

// coreContract is the line the core reads and would trade, as IBKR's
// contract record names it, with the exchange calendar of its primary
// exchange.
type coreContract struct {
	Version      int              `json:"version"`
	ISIN         string           `json:"isin"`
	ConID        int              `json:"con_id"`
	Symbol       string           `json:"symbol"`
	LocalSymbol  string           `json:"local_symbol,omitempty"`
	TradingClass string           `json:"trading_class,omitempty"`
	PrimaryExch  string           `json:"primary_exchange"`
	Currency     string           `json:"currency"`
	Market       marketcal.Market `json:"market"`
	// At is when IBKR answered it.
	At time.Time `json:"at"`
}

// sameLine reports whether two records name one line on one venue.
func (k *coreContract) sameLine(o *coreContract) bool {
	return k != nil && o != nil && k.ConID == o.ConID && strings.EqualFold(k.PrimaryExch, o.PrimaryExch)
}

func (k *coreContract) label() string {
	if k == nil {
		return ""
	}
	return coreListingLabel(ibkrlib.Listing{ConID: k.ConID, Symbol: k.Symbol, TradingClass: k.TradingClass, PrimaryExch: k.PrimaryExch})
}

// coreListingLabel names a line for a hold: "contract 9031 IUSE (class
// EUET) on LSEETF".
func coreListingLabel(l ibkrlib.Listing) string {
	out := fmt.Sprintf("contract %d %s", l.ConID, nonEmptyString(l.Symbol, "(no symbol)"))
	if l.TradingClass != "" && !strings.EqualFold(l.TradingClass, l.Symbol) {
		out += " (class " + l.TradingClass + ")"
	}
	return out + " on " + nonEmptyString(l.PrimaryExch, nonEmptyString(l.Exchange, "no exchange"))
}

// corePin is the last resolution of the policy's request (ISIN, currency
// and any con_id).
type corePin struct {
	request string
	at      time.Time
	// contract is the line in use; nil while the core holds on it.
	contract *coreContract
	// lines are every line IBKR listed under the ISIN in the currency, the
	// same fund whatever their symbol.
	lines []int
	// hold says why IBKR's answer cannot be used; err is the last lookup
	// that got no answer (what the one before found is kept).
	hold string
	err  string
}

// coreRequest is what the policy asks IBKR for: the ISIN and currency it
// names (the compiled rule's while unwritten, which holds on its own) and
// a positive con_id when written.
func coreRequest(p *risk.ConstitutionMandateCore) (isin, ccy string, pinned int) {
	isin, ccy = core.ISIN, core.Currency
	if p == nil {
		return isin, ccy, 0
	}
	isin = nonEmptyString(strings.ToUpper(strings.TrimSpace(p.ISIN)), isin)
	ccy = nonEmptyString(normCcy(p.Currency), ccy)
	if p.ConID != nil && *p.ConID > 0 {
		pinned = *p.ConID
	}
	return isin, ccy, pinned
}

// coreInstrument is the core's instrument as the policy requests it: its
// fund and currency (the compiled ones while unwritten) and its pins.
func coreInstrument(p *risk.ConstitutionMandateCore) register.Instrument {
	isin, ccy, pinned := coreRequest(p)
	inst := register.Instrument{ISIN: isin, Currency: ccy, Symbol: core.Symbol, ConID: pinned}
	if p != nil {
		inst.Exchange = strings.ToUpper(strings.TrimSpace(p.Exchange))
	}
	return inst
}

// instrumentRequestKey keys a resolution by what IBKR is asked: the ISIN,
// the currency and any pinned contract id.
func instrumentRequestKey(inst register.Instrument) string {
	return fmt.Sprintf("%s|%s|%d", inst.ISIN, inst.Currency, inst.ConID)
}

// coreListings asks the broker for listings: the gateway's lookup, or the
// test's.
func (s *Server) coreListings(ctx context.Context, request ibkrlib.ListingRequest) ([]ibkrlib.Listing, *ibkrlib.BondLookupTrace, error) {
	if s.coreListingsForTest != nil {
		return s.coreListingsForTest(ctx, request)
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, nil, s.gatewayUnavailableError()
	}
	return c.Listings(ctx, request, coreResolveFormTimeout)
}

// handleCoreResolve lists every listing of an ISIN: the flag's, else the
// one [mandate.core] names, else the compiled rule's. It marks the line
// the core uses, never chooses one and writes nothing.
func (s *Server) handleCoreResolve(ctx context.Context, req *rpc.Request) (*rpc.CoreResolveResult, error) {
	var p rpc.CoreResolveParams
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := decodeFor(api.CoreResolve, req.Params, &p); err != nil {
			return nil, err
		}
	}
	var pinned *risk.ConstitutionMandateCore
	if s.riskPolicies != nil {
		if c := s.riskPolicies.snapshot().policy; c != nil && c.Mandate != nil {
			pinned = c.Mandate.Core
		}
	}
	res := &rpc.CoreResolveResult{AsOf: s.nowUTC(), ISIN: core.ISIN, ISINSource: "rule", Currency: strings.ToUpper(strings.TrimSpace(p.Currency)), Listings: []rpc.CoreListing{}, Attempts: []rpc.CoreResolveAttempt{}}
	switch {
	case strings.TrimSpace(p.ISIN) != "":
		res.ISIN, res.ISINSource = strings.ToUpper(strings.TrimSpace(p.ISIN)), "flag"
	case pinned != nil && strings.TrimSpace(pinned.ISIN) != "":
		res.ISIN, res.ISINSource = strings.ToUpper(strings.TrimSpace(pinned.ISIN)), "policy"
	}
	if !ibkrlib.ValidISIN(res.ISIN) {
		return nil, errBadRequest(fmt.Sprintf("%q is not an ISIN (two letters, nine letters or digits and a check digit)", res.ISIN))
	}
	if pinned != nil && pinned.ConID != nil && *pinned.ConID > 0 {
		res.PinnedConID = new(*pinned.ConID)
	}
	request := ibkrlib.ListingRequest{ISIN: res.ISIN, Currency: res.Currency, Venues: coreResolveVenues}
	if res.ISIN == core.ISIN {
		request.Symbols = []string{core.Symbol}
	}
	listings, trace, err := s.coreListings(ctx, request)
	if err != nil && !errors.Is(err, ibkrlib.ErrNoListing) {
		return nil, err
	}
	if trace != nil {
		for _, a := range trace.Attempts {
			res.Attempts = append(res.Attempts, rpc.CoreResolveAttempt{Form: a.Form, Outcome: a.Outcome, Lines: a.Lines, Code: a.Code, Message: a.Message})
		}
	}
	used := 0
	if k := s.instrumentInUse(res.ISIN); k != nil {
		used = k.ConID
	}
	for _, l := range listings {
		row := rpc.CoreListing{ConID: l.ConID, Symbol: l.Symbol, LocalSymbol: l.LocalSymbol, SecType: l.SecType, Exchange: l.Exchange, PrimaryExch: l.PrimaryExch,
			Currency: l.Currency, TradingClass: l.TradingClass, LongName: l.LongName, ISIN: l.ISIN, Forms: slices.Clone(l.Forms), Used: used > 0 && l.ConID == used}
		if res.PinnedConID != nil && *res.PinnedConID == l.ConID {
			row.Pinned, res.PinnedFound = true, true
		}
		res.Listings = append(res.Listings, row)
	}
	switch {
	case len(res.Listings) == 0:
		res.Message = "IBKR found no listing for " + res.ISIN + "; every form's answer is listed. Check the ISIN, or ask again with --currency."
	case res.PinnedFound:
		res.Message = fmt.Sprintf("[mandate.core] con_id %d is among the listings; nothing to write.", *res.PinnedConID)
	case used > 0:
		res.Message = fmt.Sprintf("The core uses contract %d, the one line IBKR lists under the ISIN in the policy's currency; nothing to write.", used)
	case len(res.Listings) == 1:
		res.Message = "One listing: the core uses it when it is in the policy's currency; con_id is needed only to choose among several."
	default:
		res.Message = fmt.Sprintf("%d listings match; none is chosen for you. While more than one is in the policy's currency, the core holds until you write the con_id of its line into [mandate.core] with a higher policy_version.", len(res.Listings))
	}
	if res.PinnedConID != nil && !res.PinnedFound && len(res.Listings) > 0 {
		res.Message = fmt.Sprintf("[mandate.core] con_id %d is not among these listings. ", *res.PinnedConID) + res.Message
	}
	return res, nil
}

// coreResolve finds the core's contract on the background lane:
// resolveInstrument on the policy's fund and currency.
func (s *Server) coreResolve(ctx context.Context, c *risk.Constitution) *coreContract {
	if c == nil || c.Mandate == nil || c.Mandate.Core == nil {
		return nil
	}
	return s.resolveInstrument(ctx, coreInstrument(c.Mandate.Core), "the core", "[mandate.core]")
}

// resolveInstrument finds a strategy instrument's contract on the
// background lane: IBKR is asked by ISIN and currency (and by the pinned id
// when written) every corePinResolveEvery; a lookup that gets no answer
// keeps what the last one found and is retried after corePinRetryAfter. It
// returns the contract in use, nil while the strategy holds on it. name is
// how the strategy is named in holds ("the core"), table its policy table.
func (s *Server) resolveInstrument(ctx context.Context, inst register.Instrument, name, table string) *coreContract {
	if inst.ISIN == "" {
		return nil
	}
	request := instrumentRequestKey(inst)
	now := s.nowUTC()
	pin := s.pinOf(request)
	if pin.request == request {
		switch {
		case pin.err == "" && now.Sub(pin.at) < corePinResolveEvery:
			return pin.contract
		case pin.err != "" && now.Sub(pin.at) < corePinRetryAfter:
			return pin.contract
		}
	}
	listings, _, err := s.coreListings(ctx, ibkrlib.ListingRequest{ISIN: inst.ISIN, ConID: inst.ConID, Currency: inst.Currency})
	next := corePin{request: request, at: now}
	if pin.request == request {
		next.contract, next.lines, next.hold = pin.contract, pin.lines, pin.hold
	}
	if err != nil && !errors.Is(err, ibkrlib.ErrNoListing) {
		next.err = errText(err)
		if s.logger != nil && ctx.Err() == nil {
			s.logger.Warnf("%s's contract for %s in %s: IBKR gave no answer: %s; asked again in %s", name, inst.ISIN, inst.Currency, next.err, corePinRetryAfter)
		}
	} else {
		next.contract, next.lines, next.hold = s.chooseLine(ctx, inst, name, table, listings, now)
		if next.hold != "" && next.hold != pin.hold && s.logger != nil && ctx.Err() == nil {
			s.logger.Warnf("%s's contract: %s; %s holds", name, next.hold, name)
		}
	}
	st := &s.coreTargets
	st.mu.Lock()
	if st.pins == nil {
		st.pins = map[string]corePin{}
	}
	st.pins[request] = next
	st.mu.Unlock()
	return next.contract
}

// pinOf is the last resolution of a request, or an empty pin.
func (s *Server) pinOf(request string) corePin {
	st := &s.coreTargets
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.pins[request]
}

// chooseLine picks a strategy's line among IBKR's answer: the pinned id when
// written, else the one line IBKR lists under the ISIN in the currency. It
// holds on none, on several, on a line Canary has no calendar for, on a
// written exchange IBKR does not list it on, and, without a pin, on a line
// other than the one the strategy last used. The line in use is remembered.
func (s *Server) chooseLine(ctx context.Context, inst register.Instrument, name, table string, listings []ibkrlib.Listing, now time.Time) (*coreContract, []int, string) {
	isin, ccy, pinned := inst.ISIN, inst.Currency, inst.ConID
	var lines []ibkrlib.Listing
	var ids []int
	for _, l := range listings {
		if strings.EqualFold(l.Currency, ccy) && (l.FoundByISIN() || l.ISIN == isin) {
			lines = append(lines, l)
			ids = append(ids, l.ConID)
		}
	}
	listed := func() string {
		labels := make([]string, len(lines))
		for i, l := range lines {
			labels[i] = coreListingLabel(l)
		}
		return strings.Join(labels, "; ")
	}
	var chosen *ibkrlib.Listing
	switch {
	case pinned > 0:
		for i := range lines {
			if lines[i].ConID == pinned {
				chosen = &lines[i]
			}
		}
		if chosen == nil && len(lines) == 0 {
			return nil, ids, fmt.Sprintf("IBKR lists no %s line under %s, so the pinned contract %d is not the fund's; check it with `canary core resolve`", ccy, isin, pinned)
		}
		if chosen == nil {
			return nil, ids, fmt.Sprintf("the pinned contract %d is not among the %s lines IBKR lists under %s (%s); check it with `canary core resolve`", pinned, ccy, isin, listed())
		}
	case len(lines) == 0:
		return nil, ids, fmt.Sprintf("IBKR lists no %s line under %s; check it with `canary core resolve`", ccy, isin)
	case len(lines) > 1:
		return nil, ids, fmt.Sprintf("IBKR lists %d %s lines under %s: %s. None is chosen for you: write the con_id of %s's line into %s with a higher policy_version", len(lines), ccy, isin, listed(), name, table)
	default:
		chosen = &lines[0]
	}
	k := &coreContract{Version: coreContractDocVer, ISIN: isin, ConID: chosen.ConID, Symbol: strings.ToUpper(strings.TrimSpace(chosen.Symbol)), LocalSymbol: chosen.LocalSymbol,
		TradingClass: chosen.TradingClass, PrimaryExch: strings.ToUpper(strings.TrimSpace(chosen.PrimaryExch)), Currency: normCcy(chosen.Currency), At: now}
	if t := strings.ToUpper(strings.TrimSpace(chosen.SecType)); t != "" && t != "STK" {
		return nil, ids, fmt.Sprintf("IBKR lists %s as %s, not a fund line", k.label(), t)
	}
	if k.Symbol == "" {
		return nil, ids, fmt.Sprintf("IBKR's record of contract %d names no symbol to read its series by", k.ConID)
	}
	market, ok := venueMarket(k.PrimaryExch)
	if !ok {
		return nil, ids, fmt.Sprintf("IBKR lists %s, and Canary carries no exchange calendar for %s", k.label(), nonEmptyString(k.PrimaryExch, "a line without a primary exchange"))
	}
	k.Market = market
	if want := strings.ToUpper(strings.TrimSpace(inst.Exchange)); want != "" && want != k.PrimaryExch && !coreListedOn(*chosen, want) {
		return nil, ids, fmt.Sprintf("the policy names exchange %s, and IBKR does not list %s there; remove exchange from %s or write one IBKR lists, with a higher policy_version", want, k.label(), table)
	}
	used := s.coreUsed(ctx, isin, ccy)
	if pinned == 0 && used != nil && !used.sameLine(k) {
		return nil, ids, fmt.Sprintf("IBKR now lists %s under %s in %s; %s last used %s. Write con_id = %d into %s with a higher policy_version to use the new line", k.label(), isin, ccy, name, used.label(), k.ConID, table)
	}
	if !used.sameLine(k) || used.Symbol != k.Symbol || used.TradingClass != k.TradingClass || used.Market != k.Market {
		if err := s.rememberCore(ctx, k); err != nil && s.logger != nil {
			s.logger.Warnf("core contract %d: kept in memory, the database save failed: %v", k.ConID, err)
		}
	}
	return k, ids, ""
}

// coreListedOn reports whether IBKR lists the line on an exchange: its
// answered venue or one of its valid exchanges.
func coreListedOn(l ibkrlib.Listing, exchange string) bool {
	if strings.EqualFold(l.Exchange, exchange) {
		return true
	}
	for v := range strings.SplitSeq(l.ValidExchanges, ",") {
		if strings.EqualFold(strings.TrimSpace(v), exchange) {
			return true
		}
	}
	return false
}

func coreUsedScope(isin, ccy string) string {
	sum := sha256.Sum256([]byte(isin + "|" + ccy))
	return "core/contract/" + hex.EncodeToString(sum[:])
}

// coreUsed is the contract the core last used for an ISIN and currency,
// loaded once from the database.
func (s *Server) coreUsed(ctx context.Context, isin, ccy string) *coreContract {
	key := isin + "|" + ccy
	st := &s.coreTargets
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.used == nil {
		st.used, st.usedRevisions, st.usedLoaded = map[string]*coreContract{}, map[string]int64{}, map[string]bool{}
	}
	if !st.usedLoaded[key] {
		st.usedLoaded[key] = true
		if s.coreStore != nil {
			doc, ok, err := s.coreStore.GetStateDocument(ctx, coreUsedScope(isin, ccy), stateKindCoreContract)
			if err == nil && ok {
				var saved coreContract
				if json.Unmarshal(doc.JSON, &saved) == nil && saved.Version == coreContractDocVer && saved.ISIN == isin && saved.Currency == ccy && saved.ConID > 0 {
					st.used[key] = &saved
					st.usedRevisions[key] = doc.Revision
				}
			}
		}
	}
	if k := st.used[key]; k != nil {
		out := *k
		return &out
	}
	return nil
}

// rememberCore keeps the contract in use; a failed write keeps it in memory
// for this process and says so.
func (s *Server) rememberCore(ctx context.Context, k *coreContract) error {
	key := k.ISIN + "|" + k.Currency
	st := &s.coreTargets
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.used == nil {
		st.used, st.usedRevisions, st.usedLoaded = map[string]*coreContract{}, map[string]int64{}, map[string]bool{}
	}
	saved := *k
	st.used[key], st.usedLoaded[key] = &saved, true
	if s.coreStore == nil {
		return nil
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	doc, err := s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: coreUsedScope(k.ISIN, k.Currency), Kind: stateKindCoreContract, ExpectedRevision: st.usedRevisions[key], JSON: raw})
	if err != nil {
		var conflict *corestore.RevisionConflictError
		if errors.As(err, &conflict) && conflict.Exists {
			st.usedRevisions[key] = conflict.Actual
		}
		return err
	}
	st.usedRevisions[key] = doc.Revision
	return nil
}

// coreContractInUse is the core's line of its last resolution, nil while
// the core holds on its contract or none was resolved.
func (s *Server) coreContractInUse() *coreContract {
	if s.riskPolicies == nil {
		return nil
	}
	c := s.riskPolicies.snapshot().policy
	if c == nil || c.Mandate == nil || c.Mandate.Core == nil {
		return nil
	}
	k, _, _, _ := s.instrumentPinState(coreInstrument(c.Mandate.Core))
	return k
}

// instrumentInUse is the line any strategy's last resolution uses for an
// ISIN, nil while none does.
func (s *Server) instrumentInUse(isin string) *coreContract {
	st := &s.coreTargets
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, pin := range st.pins {
		if pin.contract != nil && pin.contract.ISIN == isin {
			k := *pin.contract
			return &k
		}
	}
	return nil
}

// corePinState is the last resolution of the core's request
// (instrumentPinState on the policy's fund).
func (s *Server) corePinState(p *risk.ConstitutionMandateCore) (contract *coreContract, hold, lookupErr string, asked bool) {
	return s.instrumentPinState(coreInstrument(p))
}

// instrumentPinState is the last resolution of an instrument's request: the
// line in use, why the strategy holds on its contract, the last lookup that
// got no answer, and whether IBKR was asked for this request at all.
func (s *Server) instrumentPinState(inst register.Instrument) (contract *coreContract, hold, lookupErr string, asked bool) {
	if inst.ISIN == "" {
		return nil, "", "", false
	}
	pin := s.pinOf(instrumentRequestKey(inst))
	if pin.request == "" {
		return nil, "", "", false
	}
	if pin.contract != nil {
		k := *pin.contract
		contract = &k
	}
	return contract, pin.hold, pin.err, true
}

// registerFundLines names, by mandate leg ID, every contract IBKR listed
// under a registered strategy's ISIN (the line in use, the one the strategy
// last used and the other lines of the ISIN in the currency): the same fund
// whatever its broker symbol, so the mandate stresses the core's as a broad
// index fund and moves it with its returns proxy (mandate.Input.LineISINs).
// Nil while the register names no instrument.
func (s *Server) registerFundLines(ctx context.Context) map[string]string {
	if s == nil || s.riskPolicies == nil {
		return nil
	}
	c := s.riskPolicies.snapshot().policy
	if c == nil || c.Mandate == nil {
		return nil
	}
	out := map[string]string{}
	for _, r := range s.resolvedRegister(c) {
		inst := r.Instrument
		if r.Entry.Form == risk.StrategyFormCore {
			inst = coreInstrument(c.Mandate.Core)
		}
		if inst.ISIN == "" {
			continue
		}
		if used := s.coreUsed(ctx, inst.ISIN, inst.Currency); used != nil {
			out[mandateConLegID(used.ConID)] = inst.ISIN
		}
		pin := s.pinOf(instrumentRequestKey(inst))
		if pin.contract != nil {
			out[mandateConLegID(pin.contract.ConID)] = inst.ISIN
		}
		for _, id := range pin.lines {
			out[mandateConLegID(id)] = inst.ISIN
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mandateConLegID is the mandate leg ID of a contract.
func mandateConLegID(conID int) string { return fmt.Sprintf("con:%d", conID) }

// coreContractIDs are the contracts protection's automatic sales leave out
// (owner decision D5, 2026-10-18 08:06 CEST): the core's line in use and
// the one it last used. The mandate's forced reduction and the owner's own
// orders still reach them.
func (s *Server) coreContractIDs(ctx context.Context) []int {
	if s == nil || s.riskPolicies == nil {
		return nil
	}
	c := s.riskPolicies.snapshot().policy
	if c == nil || c.Mandate == nil || c.Mandate.Core == nil {
		return nil
	}
	isin, ccy, _ := coreRequest(c.Mandate.Core)
	var out []int
	if used := s.coreUsed(ctx, isin, ccy); used != nil {
		out = append(out, used.ConID)
	}
	if k := s.coreContractInUse(); k != nil && k.ISIN == isin && k.Currency == ccy && !slices.Contains(out, k.ConID) {
		out = append(out, k.ConID)
	}
	return out
}
