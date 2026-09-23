package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// TestShortPortfolioDownloadReadsAsUnprimed pins the daemon side of a download
// whose end marker arrived before its rows accounted for the gross position
// value: every consumer must read it as an incomplete initial download, never
// as a current book or as an invalid broker payload.
func TestShortPortfolioDownloadReadsAsUnprimed(t *testing.T) {
	now := time.Date(2026, 9, 23, 5, 55, 1, 0, time.UTC)
	scope := brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}
	health := ibkrlib.PortfolioStreamHealth{
		Account: scope.Account, RequestedAt: now.Add(-90 * time.Second), LastUpdateAt: now.Add(-30 * time.Second),
		DownloadShortAt: now.Add(-89 * time.Second), ProjectionGeneration: 3,
	}

	authority := positionsResultDataAuthority(scope, health, now)
	if authority.Availability != rpc.AccountDataUnavailable || authority.Freshness == rpc.AccountDataFreshnessCurrent || authority.Reason != rpc.AccountDataReasonUnprimed {
		t.Fatalf("short download authority = %+v, want unavailable/unprimed", authority)
	}
	if asOf := positionsResultAuthorityAsOf(scope, health, now); !asOf.IsZero() {
		t.Fatalf("short download stamped positions as_of %v", asOf)
	}
	if state, _ := rulebookPortfolioSourceHealth(scope, health, now); state.Healthy || state.Reason != "positions_pending" {
		t.Fatalf("rulebook positions source = %+v, want pending", state)
	}
}
