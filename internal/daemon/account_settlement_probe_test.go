package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestSettlementComparisonWireHasNoFinancialAuthority(t *testing.T) {
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	normal := func(context.Context, time.Duration) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error) {
		return &ibkrlib.RawAccountSummary{AccountID: "DU1234567", AsOf: now, NetLiquidation: new(7654321.), TotalCashValue: new(9876543.), CurrencyLedger: map[string]ibkrlib.CurrencyLedger{"EUR": {SettledCash: 1234567, SettledCashObserved: true}}, Raw: map[string]string{"private": "account balances"}, SettlementObservation: &ibkrlib.AccountSettlementObservation{AsOf: now, Callbacks: 1, Rows: []ibkrlib.AccountSettlementRow{{Currency: "EUR", Source: "account_total", Finite: true}}}}, ibkrlib.AccountSummaryProvenanceRequest, nil
	}
	probeCalls := 0
	probe := func(context.Context, time.Duration) ibkrlib.AccountSettlementProbe {
		probeCalls++
		return ibkrlib.AccountSettlementProbe{Status: "completed_empty", CancelStatus: "sent", Observation: &ibkrlib.AccountSettlementObservation{AsOf: now, Rows: []ibkrlib.AccountSettlementRow{}}}
	}
	got, err := buildSettlementComparison(ctx, normal, probe, func() bool { return true })
	if err != nil || probeCalls != 1 || got.NormalObservation.Callbacks != 1 || got.Probe.Status != "completed_empty" {
		t.Fatal(got, err, probeCalls)
	}
	// This is the daemon's raw wire value, before any CLI or UI projection.
	encoded, err := json.Marshal(map[string]any{"ok": true, "data": got})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"account_id", "net_liquidation", "available_funds", "total_cash", "base_currency_ledger", "currency_exposure", "authority", "cash_balance", "settled_cash", "DU1234567", "7654321", "9876543", "1234567", "account balances"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("financial data/authority in raw diagnostic response: %s", encoded)
		}
	}
}

func TestSettlementComparisonBudgetAndScopeStopBeforeProbe(t *testing.T) {
	for _, mode := range []string{"budget", "normal_error", "scope_before_probe", "scope_after_probe", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			duration := 10 * time.Second
			if mode == "budget" {
				duration = time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), duration)
			defer cancel()
			normalCalls, probeCalls, currentCalls := 0, 0, 0
			normal := func(context.Context, time.Duration) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error) {
				normalCalls++
				if mode == "normal_error" {
					return nil, "", context.DeadlineExceeded
				}
				provenance := ibkrlib.AccountSummaryProvenanceRequest
				if mode == "fallback" {
					provenance = ibkrlib.AccountSummaryProvenanceCachedFallback
				}
				return &ibkrlib.RawAccountSummary{SettlementObservation: &ibkrlib.AccountSettlementObservation{}}, provenance, nil
			}
			probe := func(context.Context, time.Duration) ibkrlib.AccountSettlementProbe {
				probeCalls++
				return ibkrlib.AccountSettlementProbe{Status: "completed_empty", CancelStatus: "sent"}
			}
			current := func() bool {
				currentCalls++
				return mode != "scope_before_probe" && (mode != "scope_after_probe" || currentCalls == 1)
			}
			got, err := buildSettlementComparison(ctx, normal, probe, current)
			switch mode {
			case "budget":
				if err != nil || got.NormalStatus != "skipped_budget" || normalCalls != 0 || probeCalls != 0 {
					t.Fatal(got, err)
				}
			case "normal_error":
				if !errors.Is(err, context.DeadlineExceeded) || got != nil || probeCalls != 0 {
					t.Fatal(got, err)
				}
			case "scope_before_probe":
				if !errors.Is(err, ibkrlib.ErrIBKRUnavailable) || got != nil || probeCalls != 0 {
					t.Fatal(got, err)
				}
			case "scope_after_probe":
				if !errors.Is(err, ibkrlib.ErrIBKRUnavailable) || got != nil || probeCalls != 1 {
					t.Fatal(got, err)
				}
			case "fallback":
				if err != nil || got.NormalStatus != "completed_without_request_rows" || got.NormalObservation != nil {
					t.Fatal(got, err)
				}
			}
		})
	}
}
