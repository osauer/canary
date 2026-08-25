package daemon

import (
	"strings"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestHistoricalDataFarmReadinessRequiresExplicitFailure(t *testing.T) {
	t.Parallel()

	if got := historicalDataFarmReadiness(true, nil); got.status != "ready" {
		t.Fatalf("missing informational notice = %+v, want ready", got)
	}

	asOf := time.Date(2026, time.August, 25, 9, 0, 0, 0, time.UTC)
	got := historicalDataFarmReadiness(true, []ibkrlib.DataFarmStatus{{
		Name:    "ushmds",
		Type:    "historical",
		Status:  "disconnected",
		Code:    2105,
		Message: "HMDS data farm connection is broken:ushmds",
		AsOf:    asOf,
	}})
	if got.status != "degraded" || got.lastError != "IBKR 2105 disconnected" || got.lastErrorAt != asOf {
		t.Fatalf("explicit historical failure = %+v, want degraded typed evidence", got)
	}
	if !strings.Contains(got.message, "ushmds disconnected") {
		t.Fatalf("explicit historical failure message = %q, want farm detail", got.message)
	}
}

func TestHistoricalDataFarmReadinessPreservesGatewayFailure(t *testing.T) {
	t.Parallel()

	asOf := time.Date(2026, time.August, 25, 9, 0, 0, 0, time.UTC)
	got := historicalDataFarmReadiness(true, []ibkrlib.DataFarmStatus{{
		Name:    "tws-server",
		Type:    "connectivity",
		Status:  "broken",
		Code:    2110,
		Message: "Connectivity between TWS and server is broken",
		AsOf:    asOf,
	}})
	if got.status != "unavailable" || got.lastError != "IBKR 2110 broken" || got.lastErrorAt != asOf {
		t.Fatalf("explicit gateway failure = %+v, want unavailable typed evidence", got)
	}
}
