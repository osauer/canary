package daemon

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// TestClassifyErrorMarketDataAbsence pins the RPC mapping for the
// entitlement-absence suppression error: callers get symbol_inactive
// ("will not produce data right now; do not hot-retry") with the precise
// rejection and retry time in the message — including when subManager
// wraps it ("subscribe X: %w").
func TestClassifyErrorMarketDataAbsence(t *testing.T) {
	t.Parallel()
	absent := &ibkrlib.MarketDataAbsenceError{
		Key:        "ZVZZT",
		Code:       354,
		Message:    "Requested market data is not subscribed.",
		ObservedAt: time.Date(2026, 6, 11, 19, 14, 0, 0, time.UTC),
		RetryAt:    time.Date(2026, 6, 11, 19, 44, 0, 0, time.UTC),
	}
	for _, err := range []error{absent, fmt.Errorf("subscribe ZVZZT: %w", absent)} {
		code, msg := classifyError(err)
		if code != rpc.CodeSymbolInactive {
			t.Errorf("classifyError(%v) code = %s, want %s", err, code, rpc.CodeSymbolInactive)
		}
		if msg == "" {
			t.Errorf("classifyError(%v) returned empty message", err)
		}
	}
}

// TestAbsentQuoteShell pins the quote-snapshot behavior under absence
// suppression: the handler returns the same shell shape callers received
// before the absence memory existed (then: after burning the full poll
// budget), with the stored gateway rejection as the stale reason. A hard
// RPC error would red-flag the app's whole market_quotes source over one
// dead symbol.
func TestAbsentQuoteShell(t *testing.T) {
	t.Parallel()
	s := &Server{}
	absent := &ibkrlib.MarketDataAbsenceError{
		Key:        "ZVZZT",
		Code:       354,
		Message:    "Requested market data is not subscribed.",
		ObservedAt: time.Date(2026, 6, 11, 19, 14, 0, 0, time.UTC),
		RetryAt:    time.Date(2026, 6, 11, 19, 44, 0, 0, time.UTC),
	}

	q := &rpc.Quote{Symbol: "ZVZZT", IVStatus: "unavailable"}
	shell := s.absentQuoteShell(q, fmt.Errorf("subscribe ZVZZT: %w", absent), marketcal.MarketUSEquity, false)
	if shell == nil {
		t.Fatal("absence error must produce a shell quote")
	}
	if !shell.Stale || shell.StaleReason == "" {
		t.Fatalf("shell must be stale with a reason, got stale=%v reason=%q", shell.Stale, shell.StaleReason)
	}
	if shell.Price != nil || shell.Bid != nil || shell.Ask != nil || shell.Last != nil {
		t.Fatalf("shell must not fabricate prices: %+v", shell)
	}

	if got := s.absentQuoteShell(&rpc.Quote{Symbol: "AMD"}, errors.New("socket closed"), marketcal.MarketUSEquity, false); got != nil {
		t.Fatalf("non-absence errors must propagate, got shell %+v", got)
	}
}
