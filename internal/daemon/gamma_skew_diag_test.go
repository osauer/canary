package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestGammaSkewDiagJournalAppendsAnnotatedSlices(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	combined := rankableCombinedGammaFixture(now.Add(-5 * time.Minute))
	j := &gammaSkewDiagJournal{path: filepath.Join(t.TempDir(), "diag.jsonl")}

	if err := j.append(now, rpc.GammaZeroScopeCombined, "2026-06-02", combined); err != nil {
		t.Fatalf("append: %v", err)
	}
	if combined.Quality != nil {
		t.Fatal("append must annotate a clone, not the served result")
	}

	lines := readGammaSkewDiagLines(t, j.path)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want combined + SPX + SPY", len(lines))
	}
	head := lines[0]
	if head.V != 1 || head.Scope != rpc.GammaZeroScopeCombined || head.Slice != "SPY+SPX" || head.SessionKey != "2026-06-02" {
		t.Fatalf("combined line header = %+v", head)
	}
	// The regression this pins: quality must come from an annotated
	// clone. Annotating the raw combined result finds nil sub-slice
	// quality and journals "blocked: SPX quality missing".
	if head.Rankability != rpc.GammaRankabilityRankable {
		t.Fatalf("combined line rankability = %q (%s), want rankable", head.Rankability, head.Reason)
	}
	if head.MedianR2 <= 0 || head.PricedLegs != 400 {
		t.Fatalf("combined line coverage = %+v, want pooled numbers", head)
	}
	slices := []string{lines[1].Slice, lines[2].Slice}
	if slices[0] != "SPX" || slices[1] != "SPY" {
		t.Fatalf("sub-slice order = %v, want [SPX SPY]", slices)
	}
	if lines[1].FitExpiries != 3 || len(lines[1].Expiries) != 3 {
		t.Fatalf("SPX line fit detail = %+v", lines[1])
	}

	if err := j.append(now.Add(15*time.Minute), rpc.GammaZeroScopeCombined, "2026-06-02", combined); err != nil {
		t.Fatalf("second append: %v", err)
	}
	if got := len(readGammaSkewDiagLines(t, j.path)); got != 6 {
		t.Fatalf("got %d lines after second append, want 6 (append, not truncate)", got)
	}
}

func TestGammaSkewDiagJournalUsesSQLiteWithoutLegacyFallback(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "diag.jsonl")
	authority := openMarketTestCoreStore(t)
	j := &gammaSkewDiagJournal{path: legacyPath}
	if err := j.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	combined := rankableCombinedGammaFixture(now.Add(-5 * time.Minute))
	if err := j.append(now, rpc.GammaZeroScopeCombined, "2026-06-02", combined); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("runtime append touched legacy file: %v", err)
	}
	for _, slice := range []string{"SPY+SPX", "SPX", "SPY"} {
		observations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
			ScopeKey: gammaSkewDiagScopeKey(rpc.GammaZeroScopeCombined, slice),
			Source:   "ibkr.gamma.compute", Kind: gammaSkewDiagObservationKind,
		})
		if err != nil || len(observations) != 0 {
			t.Fatalf("slice %s observations=%d err=%v", slice, len(observations), err)
		}
	}
}

func TestGammaSkewDiagSuccessfulComputeJournals(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	c := newGammaZeroCache()
	c.skewDiag = &gammaSkewDiagJournal{path: filepath.Join(t.TempDir(), "diag.jsonl")}
	job := c.force(context.Background(), rpc.GammaZeroScopeCombined, now, computeETA, func(context.Context, *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return rankableCombinedGammaFixture(now), nil
	})
	<-job.done

	if got := len(readGammaSkewDiagLines(t, c.skewDiag.path)); got != 3 {
		t.Fatalf("got %d journal lines after successful compute, want 3", got)
	}
}

func TestGammaSkewDiagCancelledJobDoesNotJournal(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	c := newGammaZeroCache()
	c.skewDiag = &gammaSkewDiagJournal{path: filepath.Join(t.TempDir(), "diag.jsonl")}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	job, _ := c.kickOrJoin(ctx, rpc.GammaZeroScopeCombined, now, computeETA, func(jobCtx context.Context, _ *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		close(started)
		<-jobCtx.Done()
		// A result finalised during teardown must not enter the
		// calibration set — same poisoning class the persist guard
		// suppresses.
		return rankableCombinedGammaFixture(now), nil
	})
	<-started
	cancel()
	<-job.done

	if _, err := os.Stat(c.skewDiag.path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cancelled job journaled diagnostics: stat err = %v", err)
	}
}

func readGammaSkewDiagLines(t *testing.T, path string) []gammaSkewDiagLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var out []gammaSkewDiagLine
	for raw := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if raw == "" {
			continue
		}
		var line gammaSkewDiagLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("decode journal line %q: %v", raw, err)
		}
		out = append(out, line)
	}
	return out
}
