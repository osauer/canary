package daemon

import (
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// statusMarketDataAccess projects the connector's active market-data absence
// records onto the status.health wire shape. It is an observation surface: the
// records are in-memory, time-windowed, and route-keyed, so the projection
// reports what was refused and when the window lifts, and never derives an
// entitlement verdict or a gate from it.
//
// The absence Message is dropped rather than forwarded. It is broker free text,
// it does not survive as a typed field, and Reason is classified from the
// numeric code alone.
func statusMarketDataAccess(absences []ibkrlib.MarketDataAbsenceError) []rpc.MarketDataAccessHealth {
	out := make([]rpc.MarketDataAccessHealth, 0, len(absences))
	for _, absence := range absences {
		key := strings.TrimSpace(absence.Key)
		if key == "" {
			continue
		}
		out = append(out, rpc.MarketDataAccessHealth{
			RouteKey:   key,
			Symbol:     routeKeySymbol(key),
			Code:       absence.Code,
			Reason:     rpc.MarketDataAccessReason(absence.Code),
			ObservedAt: absence.ObservedAt,
			RetryAt:    absence.RetryAt,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// routeKeySymbol extracts the leading symbol component of a connector route
// key. A bare symbol is its own key; an explicitly routed contract joins its
// components with "|" (see ibkr.MarketDataKeyForContract).
func routeKeySymbol(key string) string {
	symbol, _, _ := strings.Cut(key, "|")
	return strings.TrimSpace(symbol)
}
