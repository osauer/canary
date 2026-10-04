package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func readSetupCoverage(t *testing.T, s *Server, params rpc.SetupCoverageParams) *rpc.SetupCoverageResult {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.handleSetupsCoverage(t.Context(), &rpc.Request{Method: rpc.MethodSetupsCoverage, Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func coverageRow(t *testing.T, out *rpc.SetupCoverageResult, symbol string) rpc.SetupCoverageContract {
	t.Helper()
	for _, row := range out.Contracts {
		if row.Symbol == symbol {
			return row
		}
	}
	t.Fatalf("no coverage row for %s in %+v", symbol, out.Contracts)
	return rpc.SetupCoverageContract{}
}

func TestSetupCoverageCountsLiveEvaluationsBySlotAndState(t *testing.T) {
	ny := setupNY(t)
	first := time.Date(2026, 9, 30, 10, 17, 0, 0, ny)
	clock := &setupTestClock{t: first}
	src := newFakeSetupSource()
	src.setFail("BBB", errors.New("historical data farm returned no data"))
	s := setupTestServer(src, clock)
	if out := readSetupCoverage(t, s, rpc.SetupCoverageParams{}); out.SessionDate != "" || out.Contracts == nil || len(out.Contracts) != 0 || out.Sessions == nil {
		t.Fatalf("empty coverage: %+v", out)
	}
	mustEvaluateSetup(t, s, "AAA", time.Time{}) // slot 9: 5 baseline reads + 1 current read
	clock.set(time.Date(2026, 9, 30, 10, 19, 0, 0, ny))
	mustEvaluateSetup(t, s, "AAA", time.Time{}) // slot 9 again, retained bars
	last := time.Date(2026, 9, 30, 10, 31, 0, 0, ny)
	clock.set(last)
	mustEvaluateSetup(t, s, "AAA", time.Time{}) // slot 12: one current read
	mustEvaluateSetup(t, s, "BBB", time.Time{}) // one failed baseline read
	// Replays and evaluations outside the session are not coverage.
	mustEvaluateSetup(t, s, "AAA", time.Date(2026, 9, 30, 10, 0, 0, 0, ny))
	mustEvaluateSetup(t, s, "AAA", time.Date(2026, 9, 29, 16, 30, 0, 0, ny))

	clock.set(time.Date(2026, 9, 30, 10, 34, 0, 0, ny))
	out := readSetupCoverage(t, s, rpc.SetupCoverageParams{})
	if out.SessionDate != "2026-09-30" || len(out.Sessions) != 1 || out.SlotsCompleted != 12 || len(out.Contracts) != 2 {
		t.Fatalf("session coverage: %+v", out)
	}
	if !out.SessionOpen.Equal(time.Date(2026, 9, 30, 9, 30, 0, 0, ny)) || !out.SessionClose.Equal(time.Date(2026, 9, 30, 16, 0, 0, 0, ny)) {
		t.Fatalf("session bounds: %s %s", out.SessionOpen, out.SessionClose)
	}
	aaa := coverageRow(t, out, "AAA")
	// First evaluated in slot 9, so slots 9..12 were scheduled; 9 and 12 were
	// covered.
	if aaa.ConID == 0 || aaa.Evaluations != 3 || aaa.SlotsScheduled != 4 || aaa.SlotsCovered != 2 || aaa.HistoryRequests != 7 ||
		!maps.Equal(aaa.States, map[string]int{"watching": 3}) || !aaa.FirstEvaluatedAt.Equal(first) || !aaa.LastEvaluatedAt.Equal(last) {
		t.Fatalf("AAA coverage: %+v", aaa)
	}
	bbb := coverageRow(t, out, "BBB")
	if bbb.Evaluations != 1 || bbb.HistoryRequests != 1 || bbb.SlotsScheduled != 1 || bbb.SlotsCovered != 1 ||
		!maps.Equal(bbb.States, map[string]int{"unavailable:baseline_history_unavailable": 1}) {
		t.Fatalf("BBB coverage: %+v", bbb)
	}
	if filtered := readSetupCoverage(t, s, rpc.SetupCoverageParams{Symbol: " bbb "}); len(filtered.Contracts) != 1 || filtered.Contracts[0].Symbol != "BBB" {
		t.Fatalf("symbol filter: %+v", filtered.Contracts)
	}
	if other := readSetupCoverage(t, s, rpc.SetupCoverageParams{Session: "2026-09-29"}); other.SessionDate != "2026-09-29" || len(other.Contracts) != 0 {
		t.Fatalf("unrecorded session: %+v", other)
	}
	// After the close a regular session offers 77 evaluable slots.
	clock.set(time.Date(2026, 9, 30, 17, 0, 0, 0, ny))
	if closed := readSetupCoverage(t, s, rpc.SetupCoverageParams{}); closed.SlotsCompleted != 77 || coverageRow(t, closed, "AAA").SlotsScheduled != 69 {
		t.Fatalf("closed session: %+v", closed)
	}
	for _, bad := range []rpc.SetupCoverageParams{{Session: "2026-9-30"}, {Session: "today"}, {Symbol: "AAA,BBB"}} {
		raw, _ := json.Marshal(bad)
		if _, err := s.handleSetupsCoverage(t.Context(), &rpc.Request{Params: raw}); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestSetupCoverageStateKeysUseTheStableReasonCode(t *testing.T) {
	for reasons, want := range map[string]string{
		"baseline_history_unavailable: baseline_bars_incomplete (retry after 10:33)": "unavailable:baseline_history_unavailable",
		"current_history_unavailable: context deadline exceeded":                     "unavailable:current_history_unavailable",
		"current_bars_stale": "unavailable:current_bars_stale",
		"":                   "unavailable:unknown",
	} {
		r := &rpc.SetupResult{State: "unavailable", Reasons: []string{reasons}}
		if reasons == "" {
			r.Reasons = nil
		}
		if got := setupCoverageState(r); got != want {
			t.Fatalf("%q -> %q, want %q", reasons, got, want)
		}
	}
	if got := setupCoverageState(&rpc.SetupResult{State: "confirmed", Reasons: []string{"volume_spike_price_rising"}}); got != "confirmed" {
		t.Fatal(got)
	}
	row := &setupCoverageRow{}
	for i := range setupCoverageStateLimit + 5 {
		row.addState("unavailable:code"+string(rune('a'+i)), 1)
	}
	if len(row.States) != setupCoverageStateLimit || row.States["unavailable:other"] != 6 {
		t.Fatalf("state bound: %d keys, other=%d", len(row.States), row.States["unavailable:other"])
	}
}

func openSetupCoverageStore(t *testing.T, path string) *corestore.Store {
	t.Helper()
	core, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return core
}

func waitForSetupCoverageFlush(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.setupCoverage.mu.Lock()
		idle := !s.setupCoverage.flushing && len(s.setupCoverage.dirty) == 0
		s.setupCoverage.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("coverage flush did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSetupCoveragePersistsAndMergesAcrossRestart(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	path := filepath.Join(privateTestDir(t), "daemon.db")
	core := openSetupCoverageStore(t, path)
	s := setupTestServer(newFakeSetupSource(), clock)
	s.coreStore = core
	mustEvaluateSetup(t, s, "AAA", time.Time{})
	clock.set(time.Date(2026, 9, 30, 10, 21, 0, 0, ny))
	mustEvaluateSetup(t, s, "AAA", time.Time{})
	waitForSetupCoverageFlush(t, s)
	s.drainSetupCoverage()
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}

	// A restarted daemon starts with empty memory; its first write merges the
	// persisted session instead of replacing it.
	core = openSetupCoverageStore(t, path)
	defer core.Close()
	restarted := setupTestServer(newFakeSetupSource(), clock)
	restarted.coreStore = core
	if out := readSetupCoverage(t, restarted, rpc.SetupCoverageParams{}); coverageRow(t, out, "AAA").Evaluations != 2 {
		t.Fatalf("persisted coverage: %+v", out)
	}
	clock.set(time.Date(2026, 9, 30, 10, 26, 0, 0, ny))
	mustEvaluateSetup(t, restarted, "AAA", time.Time{})
	waitForSetupCoverageFlush(t, restarted)
	out := readSetupCoverage(t, restarted, rpc.SetupCoverageParams{Symbol: "AAA"})
	aaa := coverageRow(t, out, "AAA")
	if aaa.Evaluations != 3 || aaa.SlotsCovered != 3 || aaa.SlotsScheduled != 3 || aaa.States["watching"] != 3 {
		t.Fatalf("merged coverage: %+v", aaa)
	}
	doc, ok, err := core.LoadSetupCoverage(t.Context(), "2026-09-30")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	var persisted setupCoverageSession
	if err := json.Unmarshal(doc.JSON, &persisted); err != nil || len(persisted.Contracts) != 1 || persisted.Contracts[0].Evaluations != 3 {
		t.Fatalf("persisted document: %s %v", doc.JSON, err)
	}
	// The next session's first write keeps the previous one on disk and lets
	// it leave memory.
	clock.set(time.Date(2026, 10, 1, 10, 17, 0, 0, ny))
	mustEvaluateSetup(t, restarted, "AAA", time.Time{})
	waitForSetupCoverageFlush(t, restarted)
	if out := readSetupCoverage(t, restarted, rpc.SetupCoverageParams{}); out.SessionDate != "2026-10-01" || len(out.Sessions) != 2 || out.Sessions[1] != "2026-09-30" {
		t.Fatalf("sessions: %+v", out)
	}
	restarted.setupCoverage.mu.Lock()
	inMemory := len(restarted.setupCoverage.sessions)
	restarted.setupCoverage.mu.Unlock()
	if inMemory != 1 {
		t.Fatalf("%d sessions kept in memory", inMemory)
	}
	if old := readSetupCoverage(t, restarted, rpc.SetupCoverageParams{Session: "2026-09-30"}); coverageRow(t, old, "AAA").Evaluations != 3 {
		t.Fatalf("previous session: %+v", old)
	}
}

// blockingSetupCoverageStore parks every read and write until released.
type blockingSetupCoverageStore struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingSetupCoverageStore) wait(ctx context.Context) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingSetupCoverageStore) LoadSetupCoverage(ctx context.Context, _ string) (corestore.StateDocument, bool, error) {
	return corestore.StateDocument{}, false, b.wait(ctx)
}

func (b *blockingSetupCoverageStore) SaveSetupCoverage(ctx context.Context, _ string, _ []byte) (corestore.StateDocument, error) {
	return corestore.StateDocument{}, b.wait(ctx)
}

func (b *blockingSetupCoverageStore) ListSetupCoverageSessions(context.Context) ([]string, error) {
	return nil, nil
}

func TestSetupCoverageWriteNeverDelaysAnEvaluation(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	s := setupTestServer(newFakeSetupSource(), clock)
	store := &blockingSetupCoverageStore{entered: make(chan struct{}, 1), release: make(chan struct{})}
	s.setupCoverage.storeForTest = store
	mustEvaluateSetup(t, s, "AAA", time.Time{})
	within(t, store.entered, "the background coverage write")
	// The flusher is parked inside daemon.db; evaluations still answer.
	for _, at := range []time.Time{time.Date(2026, 9, 30, 10, 21, 0, 0, ny), time.Date(2026, 9, 30, 10, 26, 0, 0, ny)} {
		clock.set(at)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		r, err := evaluateSetupForTest(ctx, s, "AAA", 0, time.Time{})
		cancel()
		if err != nil || r.State != "watching" {
			t.Fatalf("evaluation waited on the coverage write: %+v %v", r, err)
		}
	}
	close(store.release)
	waitForSetupCoverageFlush(t, s)
	s.setupCoverage.mu.Lock()
	evaluations := s.setupCoverage.sessions["2026-09-30"].Contracts[0].Evaluations
	s.setupCoverage.mu.Unlock()
	if evaluations != 3 {
		t.Fatalf("evaluations = %d", evaluations)
	}
}
