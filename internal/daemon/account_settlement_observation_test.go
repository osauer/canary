package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"testing"
	"time"
)

func TestAccountSettlementObservationIsDetachedAndRetired(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	raw := &ibkrlib.RawAccountSummary{SettlementObservation: &ibkrlib.AccountSettlementObservation{AsOf: now, Callbacks: 1, Rows: []ibkrlib.AccountSettlementRow{{Currency: "EUR", Source: "account_total", Finite: true}}}}
	if got := accountSettlementObservation(raw, ibkrlib.AccountSummaryProvenanceCachedFallback); got != nil {
		t.Fatal("cache published request receipt")
	}
	cloned := cloneRawAccountSummary(raw)
	cloned.SettlementObservation.Rows[0].Currency = "GBP"
	if raw.SettlementObservation.Rows[0].Currency != "EUR" {
		t.Fatal("snapshot receipt aliased")
	}
	got := accountSettlementObservation(raw, ibkrlib.AccountSummaryProvenanceRequest)
	got.Rows[0].Currency = "USD"
	if raw.SettlementObservation.Rows[0].Currency != "EUR" {
		t.Fatal("RPC receipt aliased")
	}
	res := &rpc.AccountResult{SettlementObservation: got}
	finalizeAccountSummarySession(res, accountSummaryAuthority{}, false)
	if res.SettlementObservation != nil {
		t.Fatal("retired session retained witness")
	}
}
