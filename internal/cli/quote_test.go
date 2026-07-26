package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// runQuoteRenderer with rate=0 must emit every frame exactly once, then
// return nil when the stream goroutine signals end-of-stream by closing
// frames. Single-goroutine state ownership keeps -race clean even under
// high frame churn — pre-fix this test would either race or drop frames.
func TestQuoteRenderer_Rate0EmitsEveryFrame(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}

	frames := make(chan rpc.Frame, 8)
	done := make(chan error, 1)

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := runQuoteRenderer(env, frames, done, 0, false); err != nil {
			t.Errorf("renderer returned: %v", err)
		}
	})

	const n = 50
	bid, ask, last := 100.0, 100.5, 100.25
	for range n {
		frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &last}
	}
	done <- nil
	close(frames)
	wg.Wait()

	got := strings.Count(stdout.String(), "100.00")
	if got != n {
		t.Fatalf("rate=0 should render every frame exactly once: got %d, want %d", got, n)
	}
}

// rate>0 throttles output. Over a window of N frames pushed faster than the
// rate, at most ceil(window/rate)+1 renders happen, plus one final flush
// when frames close.
func TestQuoteRenderer_RateThrottlesEmits(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}

	frames := make(chan rpc.Frame, 256)
	done := make(chan error, 1)

	rate := 30 * time.Millisecond
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = runQuoteRenderer(env, frames, done, rate, false)
	})

	bid, ask, last := 1.0, 2.0, 3.0
	start := time.Now()
	for i := 0; i < 200 && time.Since(start) < 90*time.Millisecond; i++ {
		frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &last}
		time.Sleep(time.Millisecond)
	}
	done <- nil
	close(frames)
	wg.Wait()

	emits := strings.Count(stdout.String(), "1.00")
	if emits == 0 {
		t.Fatalf("expected at least one render, got 0:\n%s", stdout.String())
	}
	// Throttle window ~90ms / rate 30ms = 3 ticks; allow generous slack for
	// scheduler jitter but assert we're nowhere near unthrottled (200).
	if emits > 25 {
		t.Fatalf("rate=%s should throttle, got %d emits in ~90ms", rate, emits)
	}
}

// A frozen DataType frame triggers the banner before the row AND the
// renderer auto-exits cleanly afterward — the gateway sends a single
// snapshot under MarketDataType=2 and then goes silent (per IBKR docs),
// so the only honest UX is one row + clean exit, not a "Ctrl-C to stop"
// pseudo-stream.
//
// Pre-fix (the user-reported bug): one row, silent UI, Ctrl+C does
// nothing visible. Commit 4-A: clear banner above the row but the
// session still hangs. Commit 4-B (this test): banner + auto-exit.
func TestQuoteRenderer_FrozenAutoExitsAfterFirstFrame(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}

	// Buffered enough for the test to push without blocking before the
	// renderer's auto-exit kicks in.
	frames := make(chan rpc.Frame, 4)
	done := make(chan error, 1)

	bid, ask, last := 461.04, 461.20, 461.20
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &last, DataType: "frozen"}
	// These two would normally be processed; auto-exit must short-circuit
	// before they're touched (otherwise rate=0 would render them too).
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &last, DataType: "frozen"}
	done <- nil

	start := time.Now()
	if err := runQuoteRenderer(env, frames, done, 0, false); err != nil {
		t.Fatalf("renderer: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("auto-exit took %s — should be near-instant after first frozen frame", elapsed)
	}

	out := stdout.String()
	if got := strings.Count(out, "data=frozen"); got != 1 {
		t.Errorf("frozen banner should print exactly once, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "no further updates expected") {
		t.Errorf("frozen banner missing the 'no further updates' hint:\n%s", out)
	}
	if !strings.Contains(out, "stream ended") {
		t.Errorf("auto-exit message missing:\n%s", out)
	}
	if got := strings.Count(out, "461.04"); got != 1 {
		t.Errorf("expected exactly one row before auto-exit, got %d:\n%s", got, out)
	}
}

// Live DataType does NOT trigger auto-exit — only frozen / delayed-frozen
// do, since they're the snapshot-only modes per IBKR docs. Live and
// delayed both stream continuously.
func TestQuoteRenderer_LiveDoesNotAutoExit(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}

	frames := make(chan rpc.Frame, 4)
	done := make(chan error, 1)

	bid, ask, last := 200.0, 200.5, 200.25
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &last, DataType: "live"}
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &last, DataType: "live"}
	done <- nil
	close(frames)

	if err := runQuoteRenderer(env, frames, done, 0, false); err != nil {
		t.Fatalf("renderer: %v", err)
	}
	out := stdout.String()
	if strings.Contains(out, "stream ended") {
		t.Errorf("live stream should not auto-exit:\n%s", out)
	}
	if strings.Contains(out, "data=live") {
		t.Errorf("live transition should not print a banner:\n%s", out)
	}
	// Both frames render (rate=0 + identical content still emits both
	// because the renderer doesn't dedup on the CLI side; that's the
	// daemon's job).
	if got := strings.Count(out, "200.00"); got != 2 {
		t.Errorf("expected 2 rows for two live frames, got %d:\n%s", got, out)
	}
}

// With Color enabled, runQuoteRenderer paints successive Last values:
// green when the new tick is above the previous, red when below, dim when
// unchanged. The very first tick has no prior, so it stays uncolored.
func TestQuoteRenderer_ColorsLastByDirection(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}

	frames := make(chan rpc.Frame, 4)
	done := make(chan error, 1)

	bid, ask := 100.0, 100.5
	l1, l2, l3, l4 := 100.10, 100.25, 100.05, 100.05
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &l1}
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &l2}
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &l3}
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &l4}
	done <- nil
	close(frames)

	if err := runQuoteRenderer(env, frames, done, 0, false); err != nil {
		t.Fatalf("renderer: %v", err)
	}
	out := stdout.String()
	// Expect at least one green (uptick l1→l2) and one red (downtick l2→l3)
	// and one dim (unchanged l3→l4). First tick must NOT be wrapped.
	if !strings.Contains(out, ansiGreen) {
		t.Errorf("expected green wrap on uptick:\n%s", out)
	}
	if !strings.Contains(out, ansiRed) {
		t.Errorf("expected red wrap on downtick:\n%s", out)
	}
	if !strings.Contains(out, ansiDim) {
		t.Errorf("expected dim wrap on unchanged tick:\n%s", out)
	}
}

// Color OFF emits no ANSI from the streaming renderer, regardless of
// tick direction. Pre-existing tests in this file all assume this.
func TestQuoteRenderer_NoColorWhenDisabled(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: false}

	frames := make(chan rpc.Frame, 2)
	done := make(chan error, 1)

	bid, ask := 100.0, 100.5
	l1, l2 := 100.10, 100.25
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &l1}
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &l2}
	done <- nil
	close(frames)

	if err := runQuoteRenderer(env, frames, done, 0, false); err != nil {
		t.Fatalf("renderer: %v", err)
	}
	if strings.Contains(stdout.String(), "\x1b[") {
		t.Fatalf("unexpected ANSI escapes in non-color output:\n%s", stdout.String())
	}
}

// Snapshot table renders the PREV CLOSE / CHG / CHG% columns with
// sign-painted values when the daemon delivered all three. The +sign
// prefix on positive changes mirrors how every retail platform shows
// "up vs prev close" so the reader's eye lands on direction first.
func TestRenderQuoteSnapshot_ChangeColumnsPainted(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}

	bid, ask, last, prev, chg, pct := 144.95, 145.05, 145.00, 143.50, 1.50, 1.0453
	qs := []rpc.Quote{{
		Symbol:    "AMD",
		Bid:       &bid,
		Ask:       &ask,
		Last:      &last,
		PrevClose: &prev,
		Change:    &chg,
		ChangePct: &pct,
		DataType:  "live",
	}}
	if code := renderQuoteSnapshotText(env, qs); code != 0 {
		t.Fatalf("renderQuoteSnapshotText returned %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"PREV CLOSE", "CHG", "CHG%", "143.50", "+1.50", "+1.05%"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
	if !strings.Contains(out, ansiGreen) {
		t.Errorf("expected green wrap on positive change:\n%s", out)
	}
}

// Negative change paints red; no change at zero paints dim.
func TestRenderQuoteSnapshot_NegativeAndZeroChange(t *testing.T) {
	t.Parallel()

	t.Run("negative", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
		last, prev, chg, pct := 882.15, 890.50, -8.35, -0.9377
		qs := []rpc.Quote{{Symbol: "NVDA", Last: &last, PrevClose: &prev, Change: &chg, ChangePct: &pct}}
		_ = renderQuoteSnapshotText(env, qs)
		out := stdout.String()
		if !strings.Contains(out, "-8.35") || !strings.Contains(out, "-0.94%") {
			t.Errorf("missing negative values:\n%s", out)
		}
		if !strings.Contains(out, ansiRed) {
			t.Errorf("expected red wrap on negative change:\n%s", out)
		}
	})

	t.Run("zero", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
		last, prev, chg, pct := 100.0, 100.0, 0.0, 0.0
		qs := []rpc.Quote{{Symbol: "FLAT", Last: &last, PrevClose: &prev, Change: &chg, ChangePct: &pct}}
		_ = renderQuoteSnapshotText(env, qs)
		if !strings.Contains(stdout.String(), ansiDim) {
			t.Errorf("expected dim wrap on zero change:\n%s", stdout.String())
		}
	})
}

// Missing change pair (PrevClose nil, no daemon-side compute) renders
// em-dash placeholders so the column width stays stable. Pre-market with
// no tick 9 yet lands here.
func TestRenderQuoteSnapshot_MissingChangeRendersEmDash(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}

	last := 145.0
	qs := []rpc.Quote{{Symbol: "AMD", Last: &last}}
	_ = renderQuoteSnapshotText(env, qs)
	out := stdout.String()
	// PREV CLOSE / CHG / CHG% all em-dash; the row stays balanced.
	if strings.Count(out, "—") < 3 {
		t.Errorf("expected at least 3 em-dash placeholders (prev close, chg, chg%%), got:\n%s", out)
	}
	if strings.Contains(out, "+0.00") || strings.Contains(out, "0.00%") {
		t.Errorf("nil deltas must not render as zero:\n%s", out)
	}
}

func TestRenderQuoteSnapshot_UsesMarkWhenLastMissing(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}

	mark := 743.73
	qs := []rpc.Quote{{Symbol: "SPY", Mark: &mark, DataType: rpc.MarketDataFrozen}}
	_ = renderQuoteSnapshotText(env, qs)
	out := stdout.String()
	for _, want := range []string{"PRICE", "743.73", "frozen"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// A non-cancellation error from Stream surfaces to the caller; the final
// flush still runs so any pending frame is rendered before exit.
func TestQuoteRenderer_StreamErrorBubbles(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}

	frames := make(chan rpc.Frame, 1)
	done := make(chan error, 1)

	bid, ask, last := 4.0, 5.0, 6.0
	frames <- rpc.Frame{T: time.Now(), Bid: &bid, Ask: &ask, Last: &last}
	want := errors.New("daemon went away")
	done <- want
	close(frames)

	err := runQuoteRenderer(env, frames, done, 250*time.Millisecond, false)
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
	if !strings.Contains(stdout.String(), "4.00") {
		t.Fatalf("expected pending frame to flush before error return:\n%s", stdout.String())
	}
}

func TestOptionExpiryYMD(t *testing.T) {
	got, err := optionExpiryYMD("260619")
	if err != nil {
		t.Fatalf("optionExpiryYMD: %v", err)
	}
	if got != "20260619" {
		t.Fatalf("optionExpiryYMD = %q, want 20260619", got)
	}
	if _, err := optionExpiryYMD("20260619"); err == nil {
		t.Fatal("optionExpiryYMD accepted YYYYMMDD; CLI option shorthand must be YYMMDD")
	}
	if _, err := optionExpiryYMD("26x619"); err == nil {
		t.Fatal("optionExpiryYMD accepted non-digits")
	}
}

func TestQuoteOptionWatchRejectedBeforeDial(t *testing.T) {
	var stderr bytes.Buffer
	env := &Env{Stdout: &bytes.Buffer{}, Stderr: &stderr}
	code := Run(context.Background(), env, "quote", []string{"SPY", "260619", "C", "600", "--watch"})
	if code == 0 {
		t.Fatal("quote option --watch returned success")
	}
	if !strings.Contains(stderr.String(), "option streaming is not supported") {
		t.Fatalf("stderr = %q, want unsupported option streaming message", stderr.String())
	}
}
