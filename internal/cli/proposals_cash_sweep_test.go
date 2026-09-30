package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Sweep rows sit under their own "Cash sweep" heading after every other row,
// never under the budget governor's shadow heading, and the heading carries
// the mode, the owner's numbers and one band line per currency.
func TestRenderProposalsListsTheCashSweepUnderItsOwnHeading(t *testing.T) {
	cash, committed, free, eq := 60000.0, 2000.0, 53000.0, 0.0
	chf := 90000.0
	snap := &rpc.TradeProposalSnapshot{
		Revision: "rev-1", PolicyID: "protection-mvp", PolicyVersion: 9,
		Counts: rpc.TradeProposalCounts{Total: 3, CashSweep: 1, CashSweepShadow: 1, BudgetReduction: 1, BudgetReductionShadow: 1},
		CashSweep: &rpc.TradeProposalCashSweepStatus{Mode: rpc.CashSweepModeShadow, Shadow: true, BaseCurrency: "EUR", Rows: 1,
			Currencies: []rpc.TradeProposalCashSweepCurrency{
				{Currency: "CHF", State: rpc.CashSweepStateNoInstrument, Cash: &chf, Reason: "no instrument is declared for CHF; its cash stays cash"},
				{Currency: "EUR", State: rpc.CashSweepStateSettlementUnknown, Reason: "the order journal is unreadable", NeedsYourNumber: []string{"etf_symbol", "etf_exchange"}},
				{Currency: "USD", State: rpc.CashSweepStateInvest, KeepCash: 5000, Cash: &cash, Committed: &committed, Free: &free, CashEquivalents: &eq, Reason: "free cash is above min_tranche"},
			}},
		Proposals: []rpc.TradeProposal{
			{Key: "cash_sweep:1", Bucket: rpc.TradeProposalBucketCashSweep, Action: "BUY", Quantity: 53, Symbol: "SYNTHB", OrderType: "LMT", Shadow: true, NeverSkipVeto: true,
				Reason: "free cash is above min_tranche", Details: []string{"rung 1 of 4 targets 28 days", "buy 53 × face_1000 = $ 53,000.00 of face"},
				Blockers: []rpc.TradingBlocker{{Code: "shadow_mode", Message: "the cash sweep runs in shadow mode"}, {Code: rpc.CashSweepBlockerFreshQuote, Message: "the bill's quote is not a live bid or ask"}}},
			{Key: "budget_reduction:1", Bucket: rpc.TradeProposalBucketBudgetReduction, Action: "SELL", Quantity: 1, Symbol: "AAA", Shadow: true, Reason: "over budget"},
			{Key: "trailing_stop:2", Bucket: rpc.TradeProposalBucketTrailingStop, Action: "SELL", Quantity: 100, Symbol: "BBB", OrderType: "TRAIL", Reason: "protective stop"},
		},
	}
	var buf bytes.Buffer
	renderProposalsText(&Env{Stdout: &buf, Stderr: &buf}, snap)
	out := buf.String()
	stop, budget := strings.Index(out, "trailing_stop:2"), strings.Index(out, "Shadow (budget reduction)  1 row")
	heading, row := strings.Index(out, "Cash sweep  shadow · 1 row · listed for observation"), strings.Index(out, "cash_sweep:1")
	if stop < 0 || budget < 0 || heading < 0 || row < 0 || !(stop < budget && budget < heading && heading < row) {
		t.Fatalf("sections out of order:\n%s", out)
	}
	for _, want := range []string{
		"tax treatment not yet confirmed (advisory)",
		"CHF  no instrument",
		"EUR  settlement unknown",
		"needs your number: etf_symbol, etf_exchange",
		"USD  invest",
		"free $ 53,000.00",
		"rung 1 of 4 targets 28 days",
		"BUY 53 SYNTHB  LMT",
		"buy 53 × face_1000",
		rpc.CashSweepBlockerFreshQuote,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// The sweep status without rows still renders its bands; without a
	// status and rows the section is absent.
	snap.Proposals = snap.Proposals[2:]
	buf.Reset()
	renderProposalsText(&Env{Stdout: &buf, Stderr: &buf}, snap)
	if !strings.Contains(buf.String(), "Cash sweep  shadow · 0 rows") {
		t.Fatalf("status without rows:\n%s", buf.String())
	}
	snap.CashSweep = nil
	buf.Reset()
	renderProposalsText(&Env{Stdout: &buf, Stderr: &buf}, snap)
	if strings.Contains(buf.String(), "Cash sweep") {
		t.Fatalf("sweep section without a sweep:\n%s", buf.String())
	}
}

// The brief's cash line reads cash plus equivalents per currency and says
// "unavailable" rather than zero.
func TestBriefCashValue(t *testing.T) {
	cash, eq, sum := 8000.0, 0.0, 8000.0
	usd := 12000.0
	got := briefCashValue(&rpc.BriefCashRow{Currencies: []rpc.BriefCashCurrency{
		{Currency: "EUR", Cash: &cash, CashEquivalents: &eq, CashLike: &sum},
		{Currency: "USD", Cash: &usd},
	}})
	if !strings.Contains(got, "EUR cash € 8,000.00 + equivalents € 0.00 = € 8,000.00") || !strings.Contains(got, "USD cash $ 12,000.00 + equivalents unavailable = unavailable") {
		t.Fatalf("cash line = %q", got)
	}
	if briefCashValue(nil) != "" {
		t.Fatal("nil row rendered")
	}
}
