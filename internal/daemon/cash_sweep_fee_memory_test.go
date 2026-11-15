package daemon

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The fee per face from an accepted sweep WhatIf survives a daemon restart:
// after the store reopens, bill selection still subtracts it, so a bill that
// beats the cash only before the fee holds the currency and lists no row
// (owner decision 2026-10-23: a losing sweep is never listed).
func TestCashSweepFeeRateSurvivesRestart(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	openEngine := func() (*proposalEngine, *corestore.Store) {
		t.Helper()
		core, err := corestore.Open(t.Context(), corestore.Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err != nil {
			t.Fatal(err)
		}
		e := &proposalEngine{store: &proposalStore{}}
		if err := e.bindCore(t.Context(), core); err != nil {
			t.Fatal(err)
		}
		return e, core
	}
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	plan := func(e *proposalEngine) cashSweepCurrencyPlan {
		t.Helper()
		in := cashSweepTestInput(map[string]float64{"USD": 60000})
		row := in.Ledger["USD"]
		row.Interest = &rpc.CashInterestRates{Credit: *cashInterestTestSide(0.01)}
		in.Ledger["USD"] = row
		src := usBillSource(cashSweepTestNow())
		src.quotes[7102] = synthLiveQuote(99.0)
		now := cashSweepTestNow()
		p := cashSweepPlanFor(policy, in, now)
		p.feeRates = e.sweepFees.snapshot()
		cashSweepResolveBills(t.Context(), src, policy.Cash.Sweep, &p, now)
		for _, cp := range p.currencies {
			if cp.status.Currency == "USD" {
				return cp
			}
		}
		t.Fatal("no USD plan")
		return cashSweepCurrencyPlan{}
	}

	e, core := openEngine()
	if usd := plan(e); usd.side != rpc.CashSweepSideInvest || usd.bill == nil {
		t.Fatalf("before any fee the best bill beats the cash, want a row: side %q (%s)", usd.side, usd.status.Reason)
	}
	// A 2% fee per face turns every listed bill into a loss against cash.
	const units = 10
	fee := 0.02 * units * 1000
	e.noteSweepFee(t.Context(),
		rpc.TradeProposal{CashSweep: &rpc.TradeProposalCashSweep{Side: rpc.CashSweepSideInvest, Currency: "USD", Instrument: cashSweepInstrumentUSTBill}},
		&rpc.OrderPreviewResult{Draft: rpc.OrderDraft{Quantity: units, Bond: &rpc.OrderBondTerms{FacePerUnit: 1000}},
			WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true, Margin: &rpc.OrderMarginImpact{CommissionCurrency: "USD", MaxCommission: new(fee)}}},
		cashSweepTestNow())
	if usd := plan(e); usd.side != "" || usd.bill != nil || usd.status.State != rpc.CashSweepStateHold {
		t.Fatalf("with the fee known the sweep loses, want hold: side %q bill %+v (%s)", usd.side, usd.bill, usd.status.Reason)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, core := openEngine()
	defer core.Close()
	if usd := plan(restarted); usd.side != "" || usd.bill != nil || usd.status.State != rpc.CashSweepStateHold {
		t.Fatalf("after a restart the fee was forgotten and a losing sweep listed: side %q bill %+v (%s)", usd.side, usd.bill, usd.status.Reason)
	}
}

// An implausible rate is never stored, and one found in the document is
// dropped on load rather than ranking bills.
func TestCashSweepFeeRateRefusesImplausibleValues(t *testing.T) {
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	var m cashSweepFeeMemory
	if err := m.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 23, 15, 0, 0, 0, time.UTC)
	for _, bad := range []cashSweepFeeRate{{PerFace: -0.001, ObservedAt: at}, {PerFace: math.NaN(), ObservedAt: at}, {PerFace: math.Inf(1), ObservedAt: at}, {PerFace: 0.001}} {
		if err := m.note(t.Context(), "USD|us_tbill", bad); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.snapshot(); len(got) != 0 {
		t.Fatalf("implausible rates kept: %v", got)
	}
	if err := m.note(t.Context(), "EUR|de_bubill", cashSweepFeeRate{PerFace: 0.0004, ObservedAt: at}); err != nil {
		t.Fatal(err)
	}
	doc, _, err := core.GetStateDocument(t.Context(), daemonStateScope, cashSweepFeeRatesKind)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"EUR|de_bubill":{"fee_per_face":0.0004,"observed_at":"2026-10-23T15:00:00Z"},"USD|us_tbill":{"fee_per_face":-1,"observed_at":"2026-10-23T15:00:00Z"}}`)
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: cashSweepFeeRatesKind, ExpectedRevision: doc.Revision, JSON: raw}); err != nil {
		t.Fatal(err)
	}
	var reloaded cashSweepFeeMemory
	if err := reloaded.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.snapshot(); len(got) != 1 || got["EUR|de_bubill"] != 0.0004 {
		t.Fatalf("reloaded rates %v, want only the plausible EUR rate", got)
	}
}
