package daemon

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
)

// cashInterestTestSide is an available broker credit rate for a sweep test.
func cashInterestTestSide(rate float64) *rpc.CashInterestSide {
	return &rpc.CashInterestSide{State: rpc.CashInterestStateAvailable, Rate: new(rate), Tier: "2", ValueDate: "2026-09-29",
		Balance: new(30000.0), BalanceSource: rpc.CashInterestBalanceLedger}
}

// cashInterestFixtureStatements parses the synthetic tiers statement the
// parser tests use: USD credit tiers on 2026-10-08 and 2026-10-09, an EUR
// debit tier and a negative CHF credit tier on 2026-10-09.
func cashInterestFixtureStatements(t *testing.T) []flexstmt.Statement {
	t.Helper()
	data, err := os.ReadFile("../flexstmt/testdata/interest-tiers-synthetic.xml")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := flexstmt.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return statements
}

// Monday after the fixture's Friday value date: one business day apart.
var cashInterestTestNow = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

func TestCashInterestRatesPickTheTierOfTheCurrentBalance(t *testing.T) {
	rows, seen := cashInterestTiersFrom(cashInterestFixtureStatements(t))
	if !seen || len(rows) != 6 {
		t.Fatalf("rows=%d seen=%v", len(rows), seen)
	}
	readAt := cashInterestTestNow.Add(-time.Minute)
	usd := cashInterestRatesFor(rows, seen, "", "USD", new(52000.0), readAt, cashInterestTestNow)
	c := usd.Credit
	if usd.Source != rpc.CashInterestSourceFlexTiers || !usd.ReadAt.Equal(readAt) || c.State != rpc.CashInterestStateAvailable || c.Rate == nil || math.Abs(*c.Rate-0.0338) > 1e-12 ||
		c.Tier != "2" || c.ValueDate != "2026-10-09" || c.LastValueDate != "" || c.BalanceSource != rpc.CashInterestBalanceLedger || *c.Balance != 52000 ||
		!c.GeneratedAt.Equal(time.Date(2026, 10, 10, 5, 10, 0, 0, time.UTC)) || len(c.Tiers) != 2 || *c.Tiers[1].Threshold != 10000 || *c.Tiers[1].Principal != 42000 {
		t.Fatalf("USD credit at 52,000: %+v", c)
	}
	if d := usd.Debit; d.State != rpc.CashInterestStateUnavailable || !strings.Contains(d.Reason, "has not borrowed USD") || d.Rate != nil || d.LastValueDate != "" {
		t.Fatalf("USD debit with no loan: %+v", d)
	}
	// The first slice earns the broker's real zero, not an unavailable.
	if c := cashInterestRatesFor(rows, seen, "", "USD", new(5000.0), readAt, cashInterestTestNow).Credit; c.State != rpc.CashInterestStateAvailable || *c.Rate != 0 || c.Tier != "1" {
		t.Fatalf("USD credit at 5,000: %+v", c)
	}
	// Without an observed balance the statement's own balance picks the tier.
	if c := cashInterestRatesFor(rows, seen, "", "USD", nil, readAt, cashInterestTestNow).Credit; c.State != rpc.CashInterestStateAvailable || c.Tier != "2" || c.BalanceSource != rpc.CashInterestBalanceStatement || *c.Balance != 52000 {
		t.Fatalf("USD credit without a ledger balance: %+v", c)
	}
	// A loan reads the debit side for the amount borrowed; the credit side
	// has no row to read.
	eur := cashInterestRatesFor(rows, seen, "", "EUR", new(-30000.0), readAt, cashInterestTestNow)
	if d := eur.Debit; d.State != rpc.CashInterestStateAvailable || math.Abs(*d.Rate-0.03482) > 1e-12 || *d.Balance != 30000 || d.Tier != "1" || *d.Tiers[0].Principal != -30000 {
		t.Fatalf("EUR debit: %+v", d)
	}
	if c := eur.Credit; c.State != rpc.CashInterestStateUnavailable || !strings.Contains(c.Reason, "held no EUR cash") {
		t.Fatalf("EUR credit: %+v", c)
	}
	// A negative broker rate is kept as the broker wrote it; a balance below
	// every listed threshold reads the lowest listed tier.
	if c := cashInterestRatesFor(rows, seen, "", "CHF", new(5000.0), readAt, cashInterestTestNow).Credit; c.State != rpc.CashInterestStateAvailable || math.Abs(*c.Rate+0.00206) > 1e-12 || c.Tier != "2" {
		t.Fatalf("CHF credit: %+v", c)
	}
	// Another account's currency is not this account's evidence.
	if c := cashInterestRatesFor(rows, seen, "", "GBP", new(1000.0), readAt, cashInterestTestNow).Credit; c.State != rpc.CashInterestStateUnavailable || !strings.Contains(c.Reason, "no GBP credit row") {
		t.Fatalf("GBP credit: %+v", c)
	}
}

func TestCashInterestRatesAreUnavailableWhenStaleMissingOrUnread(t *testing.T) {
	rows, seen := cashInterestTiersFrom(cashInterestFixtureStatements(t))
	readAt := cashInterestTestNow
	// Wednesday 2026-10-14 is three business days after Friday 2026-10-09.
	stale := cashInterestRatesFor(rows, seen, "", "USD", new(52000.0), readAt, time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)).Credit
	if stale.State != rpc.CashInterestStateUnavailable || stale.Rate != nil || stale.LastValueDate != "2026-10-09" || !strings.Contains(stale.Reason, "3 business days ago") || len(stale.Tiers) != 0 {
		t.Fatalf("stale value date was served as live: %+v", stale)
	}
	// Two business days (the Tuesday after a Friday) still counts as live.
	if c := cashInterestRatesFor(rows, seen, "", "USD", new(52000.0), readAt, time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)).Credit; c.State != rpc.CashInterestStateAvailable {
		t.Fatalf("two business days read stale: %+v", c)
	}
	// Statements without the section cannot certify any rate.
	plain := cashInterestFixtureStatements(t)
	plain[0].InterestTiers = &flexstmt.InterestTiersStatement{}
	rows, seen = cashInterestTiersFrom(plain)
	if seen || len(rows) != 0 {
		t.Fatal("an absent section was read as evidence")
	}
	if c := cashInterestRatesFor(rows, seen, "", "USD", new(52000.0), readAt, cashInterestTestNow).Credit; c.State != rpc.CashInterestStateUnavailable || !strings.Contains(c.Reason, "no retained statement carries the Interest Details (Tiers) section") {
		t.Fatalf("missing section: %+v", c)
	}
	// An unreadable statement set carries its reason to every currency.
	if c := cashInterestRatesFor(nil, false, "the broker's statements are unavailable (reporting_scope_unavailable)", "USD", new(1.0), readAt, cashInterestTestNow).Debit; c.State != rpc.CashInterestStateUnavailable || !strings.Contains(c.Reason, "reporting_scope_unavailable") {
		t.Fatalf("unreadable statements: %+v", c)
	}
}

func TestCashInterestTiersLatestGenerationWins(t *testing.T) {
	first := cashInterestFixtureStatements(t)
	data, _ := os.ReadFile("../flexstmt/testdata/interest-tiers-synthetic.xml")
	revised, err := flexstmt.Parse([]byte(strings.Replace(strings.Replace(string(data), `whenGenerated="20261010;051000"`, `whenGenerated="20261010;091000"`, 1), `rate="3.38"`, `rate="3.30"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]flexstmt.Statement{{first[0], revised[0]}, {revised[0], first[0]}} {
		rows, seen := cashInterestTiersFrom(order)
		c := cashInterestRatesFor(rows, seen, "", "USD", new(52000.0), cashInterestTestNow, cashInterestTestNow).Credit
		if len(rows) != 6 || math.Abs(*c.Rate-0.033) > 1e-12 || !c.GeneratedAt.Equal(time.Date(2026, 10, 10, 9, 10, 0, 0, time.UTC)) {
			t.Fatalf("an older generation won: %+v", c)
		}
	}
}

func TestCashInterestTierSideFollowsTheBrokersSigns(t *testing.T) {
	for name, tc := range map[string]struct {
		tier flexstmt.InterestTier
		side string
	}{
		"positive principal":   {flexstmt.InterestTier{Type: "IBKR Interest", Principal: new(10.0)}, cashInterestSideCredit},
		"negative principal":   {flexstmt.InterestTier{Type: "IBKR Interest", Principal: new(-10.0)}, cashInterestSideDebit},
		"negative interest":    {flexstmt.InterestTier{Type: "IBKR Interest", Principal: new(0.0), Interest: new(-0.5)}, cashInterestSideDebit},
		"type names the side":  {flexstmt.InterestTier{Type: "IBKR Debit Interest"}, cashInterestSideDebit},
		"short sale interest":  {flexstmt.InterestTier{Type: "Short Credit Interest", Principal: new(10.0)}, ""},
		"nothing to classify":  {flexstmt.InterestTier{Type: "IBKR Interest"}, ""},
		"zero credit tier one": {flexstmt.InterestTier{Type: "IBKR Credit Interest", Principal: new(10000.0), Interest: new(0.0)}, cashInterestSideCredit},
	} {
		if got := cashInterestTierSide(tc.tier); got != tc.side {
			t.Errorf("%s: side %q, want %q", name, got, tc.side)
		}
	}
}

// The account read carries every currency's rates, picked for the ledger's
// observed cash; the cache serves the last read while it refreshes and says
// so before the first read, and a Flex fetch marks it for a re-read.
func TestAccountReadCarriesBrokerCashInterestRates(t *testing.T) {
	srv := &Server{}
	srv.now = func() time.Time { return cashInterestTestNow }
	var reads atomic.Int32
	fixture := cashInterestFixtureStatements(t)
	srv.cashInterest.statements = func(context.Context) ([]flexstmt.Statement, error) {
		reads.Add(1)
		return fixture, nil
	}
	settled := func() {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			srv.cashInterest.mu.Lock()
			busy := srv.cashInterest.busy
			srv.cashInterest.mu.Unlock()
			if !busy {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the background read did not finish")
			}
		}
	}
	res := &rpc.AccountResult{BaseCurrency: "EUR",
		BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", CashObserved: true, CashCcy: -30000, ExchangeRate: 1},
		CurrencyExposure:   []rpc.CurrencyExposure{{Currency: "CHF", ExchangeRate: 1.06}, {Currency: "USD", CashObserved: true, CashCcy: 52000, ExchangeRate: 0.85}}}
	// The first read starts in the background; the read in hand says so.
	srv.annotateCashInterest(res)
	if c := res.CurrencyExposure[1].Interest; c == nil || c.Credit.State != rpc.CashInterestStateUnavailable || !strings.Contains(c.Credit.Reason, "not been read") || !c.ReadAt.IsZero() {
		t.Fatalf("before the first read: %+v", c)
	}
	settled()
	srv.annotateCashInterest(res)
	usd, chf, eur := res.CurrencyExposure[1].Interest, res.CurrencyExposure[0].Interest, res.BaseCurrencyLedger.Interest
	if usd == nil || usd.Credit.State != rpc.CashInterestStateAvailable || *usd.Credit.Rate != 0.0338 || *usd.Credit.Balance != 52000 || !usd.ReadAt.Equal(cashInterestTestNow) {
		t.Fatalf("USD: %+v", usd)
	}
	if chf == nil || chf.Credit.State != rpc.CashInterestStateAvailable || chf.Credit.BalanceSource != rpc.CashInterestBalanceStatement {
		t.Fatalf("CHF without observed cash: %+v", chf)
	}
	if eur == nil || eur.Debit.State != rpc.CashInterestStateAvailable || *eur.Debit.Balance != 30000 || eur.Credit.State != rpc.CashInterestStateUnavailable {
		t.Fatalf("EUR base ledger: %+v", eur)
	}
	// A new statement marks the cache for a re-read; the last rates stay
	// served meanwhile.
	srv.cashInterest.invalidate()
	srv.cashInterest.mu.Lock()
	srv.cashInterest.busy = true // a read is in flight: no second one starts
	srv.cashInterest.mu.Unlock()
	before := reads.Load()
	srv.annotateCashInterest(res)
	if c := res.CurrencyExposure[1].Interest; c.Credit.State != rpc.CashInterestStateAvailable || reads.Load() != before {
		t.Fatalf("stale cache was not served while refreshing: %+v reads=%d", c, reads.Load()-before)
	}
	srv.cashInterest.mu.Lock()
	srv.cashInterest.busy = false
	srv.cashInterest.mu.Unlock()
	// An unreadable statement set reads unavailable with its reason, never a
	// remembered rate.
	srv.cashInterest.statements = func(context.Context) ([]flexstmt.Statement, error) {
		return nil, errors.New("accepted_inventory_changed")
	}
	srv.readCashInterestTiers(context.Background(), srv.currentBrokerStateScope())
	srv.annotateCashInterest(res)
	if c := res.CurrencyExposure[1].Interest; c.Credit.State != rpc.CashInterestStateUnavailable || !strings.Contains(c.Credit.Reason, "accepted_inventory_changed") || c.Credit.Rate != nil {
		t.Fatalf("unreadable statements kept a rate: %+v", c)
	}
}

// The sweep's benefit estimate and policy check read the credit side; a
// negative or missing rate makes no claim.
func TestCashInterestRateReadsOnlyAnAvailableNonnegativeRate(t *testing.T) {
	if rate, reason := cashInterestRate(nil, "USD"); rate != nil || !strings.Contains(reason, "not read with the account") {
		t.Fatalf("nil side: %v %q", rate, reason)
	}
	if rate, reason := cashInterestRate(&rpc.CashInterestSide{State: rpc.CashInterestStateUnavailable, Reason: "no USD credit row"}, "USD"); rate != nil || reason != "no USD credit row" {
		t.Fatalf("unavailable side: %v %q", rate, reason)
	}
	if rate, reason := cashInterestRate(cashInterestTestSide(-0.002), "CHF"); rate != nil || !strings.Contains(reason, "negative") {
		t.Fatalf("negative rate: %v %q", rate, reason)
	}
	if rate, reason := cashInterestRate(cashInterestTestSide(0), "USD"); rate == nil || *rate != 0 || reason != "" {
		t.Fatalf("a real zero was refused: %v %q", rate, reason)
	}
	if words := cashInterestWords(cashInterestTestSide(0.0338), "USD"); words != "cash rate 3.38% a year (broker, tier 2, value date 2026-09-29)" {
		t.Fatalf("words: %q", words)
	}
	if words := cashInterestWords(nil, "USD"); !strings.HasPrefix(words, "cash rate unavailable: ") {
		t.Fatalf("words: %q", words)
	}
}
