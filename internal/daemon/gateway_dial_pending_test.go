package daemon

import (
	"bytes"
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
