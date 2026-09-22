package daemon

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestGatewayRetryAndHistoryFanoutShareIncident(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: NewLogger(&buf, "warn")}
	s.logGatewayUnavailable("listener unavailable")
	saved := &storedMarketHistory{}
	saved.Result.End = time.Now()
	for range 100 {
		s.logGatewayUnavailable("handshake failure")
		s.logMarketHistoryFallback(rpc.MarketHistoryParams{}, saved, ibkrlib.ErrIBKRUnavailable)
	}
	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Fatalf("warnings=%d: %s", got, buf.String())
	}
	// An independent history defect must not disappear behind the broker outage.
	s.logMarketHistoryFallback(rpc.MarketHistoryParams{}, saved, errors.New("response lacks sessions"))
	if !strings.Contains(buf.String(), "response lacks sessions") {
		t.Fatal("independent defect hidden")
	}
	s.logGatewayRecovered()
	s.logGatewayRecovered()
	if strings.Count(buf.String(), "Gateway connection recovered") != 1 {
		t.Fatal(buf.String())
	}
	s.logGatewayUnavailable("new outage")
	if !strings.Contains(buf.String(), "new outage") {
		t.Fatal("second incident suppressed")
	}
}

func BenchmarkSuppressedGatewayDiagnostic(b *testing.B) {
	s := &Server{logger: NewLogger(io.Discard, "warn"), now: time.Now}
	s.logGatewayUnavailable("synthetic unavailable")
	b.ReportAllocs()
	for b.Loop() {
		s.logGatewayUnavailable("synthetic unavailable")
	}
}

type countedLogString struct{ calls *int }

func (s countedLogString) String() string { *s.calls++; return "formatted" }

func TestSuppressedDebugDoesNotFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, "warn")
	calls := 0
	logger.Debugf("%s", countedLogString{&calls})
	if calls != 0 || buf.Len() != 0 {
		t.Fatal("disabled diagnostics formatted")
	}
}

func TestHistoryCannotOpenTransportIncident(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: NewLogger(&buf, "warn")}
	saved := &storedMarketHistory{}
	s.logMarketHistoryFallback(rpc.MarketHistoryParams{}, saved, ibkrlib.ErrIBKRUnavailable)
	if !strings.Contains(buf.String(), "IBKR refresh failed") {
		t.Fatal("standalone history outage hidden")
	}
	s.logGatewayRecovered()
	if strings.Contains(buf.String(), "connection recovered") {
		t.Fatal("history failure claimed transport recovery")
	}
}
