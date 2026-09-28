package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func stalledHistoryError() error {
	return fmt.Errorf("%w since 06:31:55; next probe after 06:36:55", ibkrlib.ErrHistoricalServiceStalled)
}

// No farm notice reports a Gateway that holds history unanswered; the stall
// verdict turns the history row degraded, and a disconnected gateway keeps
// its own, worse verdict.
func TestHistoryStallTurnsHistoryRowDegraded(t *testing.T) {
	since := time.Date(2026, 9, 28, 6, 31, 55, 0, time.Local)
	ready := farmReadiness{status: "ready"}

	row := statusSubsystemFromReadiness("history", historyStallReadiness(ready, since, true))
	if row.Status != "degraded" || row.LastError != "gateway_history_stalled" || !strings.Contains(row.Message, "06:31:55") {
		t.Fatalf("stalled history row = %+v", row)
	}
	if got := historyStallReadiness(ready, time.Time{}, false); got != ready {
		t.Fatalf("unstalled readiness = %+v, want unchanged", got)
	}
	unavailable := farmReadiness{status: "unavailable"}
	if got := historyStallReadiness(unavailable, since, true); got != unavailable {
		t.Fatalf("a stall overrode an unavailable gateway: %+v", got)
	}
	// Without a connector the row keeps the farm verdict.
	s := &Server{}
	if got := s.historyReadiness(true, nil); got.status != "ready" {
		t.Fatalf("history readiness without a connector = %+v", got)
	}
}

// A refused request never reached the wire. Data health must still see the
// history source failing; an unclassified error would drop the observation.
func TestHistoryStallIsATimeoutForDataHealth(t *testing.T) {
	failure := quoteHealthFailure(stalledHistoryError(), time.Now())
	if failure == nil || failure.Code != rpc.SourceFailureTimeout {
		t.Fatalf("failure = %+v, want %s", failure, rpc.SourceFailureTimeout)
	}
}

// The connector announces a stall once; the per-series fallback lines that
// repeated on every refresh stay out of the WARN log. Other causes still warn.
func TestHistoryStallFallbacksStayQuiet(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: NewLogger(&buf, "warn")}
	saved := &storedMarketHistory{}
	saved.Result.End = time.Now()
	s.logMarketHistoryFallback(rpc.MarketHistoryParams{Range: "1D"}, saved, stalledHistoryError())
	if buf.Len() != 0 {
		t.Fatalf("market history fallback warned for a stall: %s", buf.String())
	}
	s.logMarketHistoryFallback(rpc.MarketHistoryParams{Range: "1D"}, saved, errors.New("response lacks sessions"))
	if !strings.Contains(buf.String(), "response lacks sessions") {
		t.Fatal("an independent history defect was hidden")
	}

	var warns []string
	cache := newRegimeHistoryCache(t.TempDir(), func(format string, args ...any) {
		warns = append(warns, fmt.Sprintf(format, args...))
	})
	cache.put("HYG", 365, []ibkrlib.HistoricalBar{{Date: "20260925", Close: 80}}, time.Now())
	cache.warnFallback("HYG", 365, nil, stalledHistoryError())
	if len(warns) != 0 {
		t.Fatalf("regime fallback warned for a stall: %q", warns)
	}
	cache.warnFallback("HYG", 365, nil, errors.New("historical data timeout for HYG after 20s"))
	if len(warns) != 1 {
		t.Fatalf("regime fallback warnings = %q, want the timeout", warns)
	}
}
