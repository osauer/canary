package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Each daemon start on 2026-10-05 logged five to seven "IBKR connection
// unavailable" chart warnings in the instant before its first connect. A
// pending dial owns that outcome: it connects, or logGatewayUnavailable
// reports the failure.
func TestGatewayDependencyJoinsAPendingDial(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: NewLogger(&buf, "debug")}
	saved := &storedMarketHistory{}
	saved.Result.End = time.Now()
	s.connectInFlight = true
	for range 7 {
		s.logMarketHistoryFallback(rpc.MarketHistoryParams{Range: "6M"}, saved, ibkrlib.ErrIBKRUnavailable)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("chart reads warned behind a pending dial:\n%s", buf.String())
	}
	if strings.Count(buf.String(), "Gateway dependency awaiting dial") != 7 {
		t.Fatal("debug diagnostics lost")
	}
	buf.Reset()
	s.connectInFlight = false
	s.logMarketHistoryFallback(rpc.MarketHistoryParams{Range: "6M"}, saved, ibkrlib.ErrIBKRUnavailable)
	if strings.Count(buf.String(), "level=WARN") != 1 {
		t.Fatalf("a failure with no dial and no outage must warn:\n%s", buf.String())
	}
}

// Inside an announced bar-farm outage each timed-out history read logged its
// own WARN (about 50 within one ushmds break on 2026-10-05); it now joins the
// farm's announcement. Other causes, and timeouts outside one, still warn.
func TestHistoryTimeoutsJoinAnAnnouncedBarFarmOutage(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: NewLogger(&buf, "warn")}
	outage := true
	s.historicalFarmOutage = func() bool { return outage }
	saved := &storedMarketHistory{}
	saved.Result.End = time.Now()
	timeout := fmt.Errorf("historical data timeout for SPY after 25s: %w", context.DeadlineExceeded)
	for range 50 {
		s.logMarketHistoryFallback(rpc.MarketHistoryParams{Range: "1D"}, saved, timeout)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("timeouts warned inside an announced farm outage:\n%s", buf.String())
	}
	s.logMarketHistoryFallback(rpc.MarketHistoryParams{Range: "1Y"}, saved, errors.New("invalid historical observation"))
	outage = false
	s.logMarketHistoryFallback(rpc.MarketHistoryParams{Range: "1D"}, saved, timeout)
	if n := strings.Count(buf.String(), "level=WARN"); n != 2 {
		t.Fatalf("WARN lines = %d, want 2 (another cause; a timeout with no outage):\n%s", n, buf.String())
	}
}
