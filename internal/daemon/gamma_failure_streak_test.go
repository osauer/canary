package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// A gamma retry streak warns once: the first failing run writes one prewarm
// line per class with the expiry count folded in and the cause kept, repeats
// stay in debug, and the first success writes the one recovery line. A
// failure during a backend-link outage logs at debug and opens no streak.
// Owner instruction (Oliver, in chat, 2026-10-13 09:04 CEST): dependents of
// an owned outage log through the owner.
func TestGammaPrewarmFailureStreakWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)
	link := ibkrlib.BackendLinkReport{}
	s := &Server{logger: NewLogger(&buf, "debug"), now: func() time.Time { return now }}
	s.backendLink = func() ibkrlib.BackendLinkReport { return link }
	log := gammaLogf{inner: gammaServerLog{s}}
	expiries := []string{"20261016", "20261019", "20261020", "20261021", "20261022", "20261023"}
	cause := errors.New("IBKR 200 on contract details")
	warns := func() []string {
		var out []string
		for line := range strings.SplitSeq(buf.String(), "\n") {
			if strings.Contains(line, "level=WARN") {
				out = append(out, line)
			}
		}
		return out
	}

	for range 7 {
		logGammaPrewarmOutcome(log, "SYNCLS", expiries, len(expiries), cause)
		now = now.Add(time.Minute)
	}
	got := warns()
	if len(got) != 1 || !strings.Contains(got[0], "failed=6/6") || !strings.Contains(got[0], "IBKR 200 on contract details") {
		t.Fatalf("streak warnings = %q, want one folded line naming the cause", got)
	}
	if n := strings.Count(buf.String(), "of this streak"); n != 6 {
		t.Fatalf("debug repeats = %d, want 6", n)
	}
	logGammaPrewarmOutcome(log, "SYNCLS", nil, len(expiries), nil)
	logGammaPrewarmOutcome(log, "SYNCLS", nil, len(expiries), nil)
	got = warns()
	if len(got) != 2 || !strings.Contains(got[1], "gamma.prewarm class=SYNCLS recovered after 7 failed attempts") {
		t.Fatalf("recovery lines = %q, want one", got)
	}

	// Inside a backend-link outage the failure is the outage's symptom.
	buf.Reset()
	link = ibkrlib.BackendLinkReport{Down: true, ChangedAt: now}
	logGammaPrewarmOutcome(log, "SYNCLS", expiries, len(expiries), cause)
	if got := warns(); len(got) != 0 {
		t.Fatalf("failure warned inside the owned outage: %q", got)
	}
	link = ibkrlib.BackendLinkReport{ChangedAt: now}
	now = now.Add(2 * ibkrlib.BackendRestoreSettle)
	logGammaPrewarmOutcome(log, "SYNCLS", expiries, len(expiries), cause)
	if got := warns(); len(got) != 1 || !strings.Contains(got[0], "failed=6/6") {
		t.Fatalf("first failure after the outage = %q, want a new streak's warning", got)
	}
}

// The combine fallback line is a streak too: SPY failing on every retry warns
// once, not once per retry.
func TestGammaCombineSPYUnavailableWarnsOncePerStreak(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: NewLogger(&buf, "debug")}
	old := runGammaUnderlyingPhase
	t.Cleanup(func() { runGammaUnderlyingPhase = old })
	runGammaUnderlyingPhase = func(_ context.Context, _ *Server, _ *ibkrlib.Connector, underlying string, _ rpc.GammaZeroParams, _ *atomic.Int32, _ int32) (*rpc.GammaZeroComputed, error) {
		return nil, errors.New("zero-gamma: synthetic " + underlying + " chain refused")
	}
	for range 4 {
		if _, err := computeGammaCombined(t.Context(), s, nil, rpc.GammaZeroParams{}, &atomic.Int32{}); err == nil {
			t.Fatal("combined compute succeeded with both phases failing")
		}
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 || !strings.Contains(buf.String(), "gamma.combine.spy_unavailable") {
		t.Fatalf("spy_unavailable warnings = %d, want 1:\n%s", n, buf.String())
	}
}
