package daemon

import (
	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
	"testing"
	"time"
)

func TestAccountSettlementObservationIsDetachedAndRetired(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	raw := &broker.AccountSummary{Settlement: &broker.SettlementObservation{AsOf: now, Callbacks: 1, Rows: []broker.SettlementRow{{Currency: "EUR", Source: "account_total", Finite: true}}}}
	if got := accountSettlementObservation(raw.Settlement, broker.ProvenanceCachedFallback); got != nil {
		t.Fatal("cache published request receipt")
	}
	cloned := cloneAccountSummary(raw)
	cloned.Settlement.Rows[0].Currency = "GBP"
	if raw.Settlement.Rows[0].Currency != "EUR" {
		t.Fatal("snapshot receipt aliased")
	}
	got := accountSettlementObservation(raw.Settlement, broker.ProvenanceRequest)
	got.Rows[0].Currency = "USD"
	if raw.Settlement.Rows[0].Currency != "EUR" {
		t.Fatal("RPC receipt aliased")
	}
	res := &rpc.AccountResult{SettlementObservation: got}
	finalizeAccountSummarySession(res, accountSummaryAuthority{}, false)
	if res.SettlementObservation != nil {
		t.Fatal("retired session retained witness")
	}
}
