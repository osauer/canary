package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// newHistoryIndexServer builds a Server whose history index is opened on
// the test's private XDG_STATE_HOME and whose ingest goroutine runs until
// the test ends. Journal writes go through the REAL daemon writers so
// these tests double as writer/parser drift guards.
func newHistoryIndexServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newHistoryIndexServerLogged(t, "error")
	return s
}

// newHistoryIndexServerLogged is newHistoryIndexServer with a race-safe
// log sink at the given level (fallback-disclosure tests read warns).
func newHistoryIndexServerLogged(t *testing.T, level string) (*Server, *syncWriter) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sink := &syncWriter{buf: &bytes.Buffer{}}
	s := &Server{logger: NewLogger(sink, level), now: time.Now}
	if path, err := regimeDecisionsDefaultPath(); err == nil {
		s.regimeDecisions = &regimeDecisionJournal{path: path}
	} else {
		t.Fatalf("resolve regime journal path: %v", err)
	}
	s.installStressDecisionJournal()
	s.installOrderJournalStore()
	s.installProposalOutcomeStore()
	s.installRiskCapitalStore()
	s.installPlatformSettingsStore()
	s.installHistoryIndex()
	if s.historyIndexOpts == nil {
		t.Fatal("installHistoryIndex left opts nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.startHistoryIndex(ctx)
	if s.historyIndex.Load() == nil {
		t.Fatal("startHistoryIndex left store nil")
	}
	t.Cleanup(func() {
		cancel()
		_ = s.historyIndex.Load().Close()
	})
	return s, sink
}

// syncWriter makes a bytes.Buffer safe for the daemon logger under -race.
type syncWriter struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// waitForHistory polls fn until it reports done or the deadline passes.
func waitForHistory(t *testing.T, fn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		done, err := fn()
		lastErr = err
		if err == nil && done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("history index never converged (last err: %v)", lastErr)
}

// TestHistoryIndexRegimeRoundTrip is the writer→parser drift guard: a
// snapshot journaled by the daemon's own regimeDecisionJournal must come
// back from the index with the same decision fields. If the journal line
// shape changes, this test fails before the index silently misparses.
func TestHistoryIndexRegimeRoundTrip(t *testing.T) {
	s := newHistoryIndexServer(t)
	now := time.Now()
	res := &rpc.RegimeSnapshotResult{
		AsOf:             now,
		TapeSessionState: "trading_date",
		Fingerprint:      rpc.Fingerprint{Version: "v1", Key: "sha256:roundtrip"},
		Lifecycle: rpc.LifecycleState{
			Stage:      "early_warning",
			Severity:   "watch",
			Readiness:  "ready",
			Confidence: "high",
		},
		Composite: rpc.RegimeComposite{
			Verdict:                 "Stress signal present",
			ClusterRedCount:         2,
			ClusterYellowCount:      1,
			ClusterEligibleRedCount: 1,
		},
	}
	res.VIXTermStructure.Status = "ok"
	res.VIXTermStructure.Band = "green"
	res.VIXTermStructure.Streak = &rpc.StreakInfo{Band: "green", Sessions: 3, Since: "2026-07-16"}
	s.journalRegimeDecision(res)

	var got rpc.RegimeHistoryResult
	waitForHistory(t, func() (bool, error) {
		out, err := s.handleRegimeHistory(&rpc.Request{})
		if err != nil {
			return false, err
		}
		got = *out
		return out.Count == 1, nil
	})
	e := got.Entries[0]
	if e.Stage != "early_warning" || e.Severity != "watch" || e.Readiness != "ready" || e.Confidence != "high" {
		t.Fatalf("lifecycle fields did not round-trip: %+v", e)
	}
	if e.Verdict != "Stress signal present" || e.Fingerprint != "sha256:roundtrip" || e.TapeSession != "trading_date" {
		t.Fatalf("verdict/fingerprint/tape did not round-trip: %+v", e)
	}
	if e.ClusterRed != 2 || e.ClusterYellow != 1 || e.ClusterEligibleRed != 1 {
		t.Fatalf("cluster counts did not round-trip: %+v", e)
	}
	if e.SessionKey != nyTradingSessionKey(nyTime(now)) {
		t.Fatalf("session key = %q, want writer's %q", e.SessionKey, nyTradingSessionKey(nyTime(now)))
	}
	// journalRegimeDecision stamps its own publication time, so `now` and the
	// stored timestamp are two independent clock reads. Comparing them at
	// millisecond granularity fails whenever they straddle a millisecond
	// boundary (observed: now=…45.146981, stored=…45.147004 — 23µs apart, one
	// millisecond apart after truncation). Assert what the round-trip actually
	// owes us: a real write time at or shortly after the write, not a zero
	// value, an epoch default, or some other field.
	if delta := e.At.Sub(now); delta < 0 || delta > time.Minute {
		t.Fatalf("at = %v, want a journal write time at or shortly after %v (delta %v)", e.At, now, delta)
	}
	if got.Index.IngestedBytes == 0 || got.Index.JournalBytes != got.Index.IngestedBytes {
		t.Fatalf("index health = %+v, want fully ingested", got.Index)
	}

	// Indicator sub-rows are not on the RPC surface; read the index file
	// directly to pin the writer's per-indicator JSON names too.
	db, err := sql.Open("sqlite", "file:"+s.historyIndexOpts.DBPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open index read-only: %v", err)
	}
	defer db.Close()
	var band string
	var sessions int
	err = db.QueryRow(`SELECT band, streak_sessions FROM regime_indicators WHERE indicator = 'vix_term'`).
		Scan(&band, &sessions)
	if err != nil {
		t.Fatalf("read vix_term indicator row: %v", err)
	}
	if band != "green" || sessions != 3 {
		t.Fatalf("vix_term indicator = band %q sessions %d, want green/3", band, sessions)
	}
}

// TestHistoryIndexRulesRoundTrip drives the REAL journalRuleTransitions
// writer (same pattern as TestJournalRuleTransitionsCarriesPolicyFingerprint)
// and proves the index parser tracks its map-key line shape.
func TestHistoryIndexRulesRoundTrip(t *testing.T) {
	s := newHistoryIndexServer(t)
	pol := risk.DefaultRulebookPolicy()
	asOf := time.Now()
	s.journalRuleTransitions(&rpc.RulesResult{
		AsOf:          asOf,
		PolicyID:      pol.ID,
		PolicyVersion: pol.Version,
		PolicyFingerprint: &rpc.Fingerprint{
			Version: rpc.RulebookPolicyFingerprintVersion,
			Key:     pol.FingerprintKey(),
		},
		Rules: []risk.RuleRow{{
			ID:       risk.RuleSingleNameExposure,
			Status:   risk.RuleStatusWatch,
			Evidence: "synthetic round-trip evidence",
		}},
	})

	var got rpc.RulesHistoryResult
	waitForHistory(t, func() (bool, error) {
		out, err := s.handleRulesHistory(&rpc.Request{})
		if err != nil {
			return false, err
		}
		got = *out
		return out.Count == 1, nil
	})
	e := got.Entries[0]
	if e.Rule != risk.RuleSingleNameExposure || e.Status != risk.RuleStatusWatch || e.Was != "" {
		t.Fatalf("transition fields did not round-trip: %+v", e)
	}
	if e.Evidence != "synthetic round-trip evidence" {
		t.Fatalf("evidence did not round-trip: %q", e.Evidence)
	}
	if e.PolicyID != pol.ID || e.PolicyVersion != pol.Version || e.PolicyFingerprint != pol.FingerprintKey() {
		t.Fatalf("policy fields did not round-trip: %+v", e)
	}
	if e.At.UnixMilli() != asOf.UnixMilli() {
		t.Fatalf("at = %v, want %v", e.At, asOf)
	}
}

func TestRulesHistorySQLiteRoundTrip(t *testing.T) {
	store := openMarketTestCoreStore(t)
	s := &Server{coreStore: store, logger: NewLogger(&bytes.Buffer{}, "error")}
	pol := risk.DefaultRulebookPolicy()
	asOf := time.Now().UTC()
	s.journalRuleTransitions(&rpc.RulesResult{
		AsOf:          asOf,
		PolicyID:      pol.ID,
		PolicyVersion: pol.Version,
		PolicyFingerprint: &rpc.Fingerprint{
			Version: rpc.RulebookPolicyFingerprintVersion,
			Key:     pol.FingerprintKey(),
		},
		Rules: []risk.RuleRow{{
			ID: risk.RuleOptionLinePremium, Status: risk.RuleStatusWatch,
			Evidence: "hedge tier drives the current state",
		}},
	})

	got, err := s.handleRulesHistory(&rpc.Request{})
	if err != nil {
		t.Fatalf("rules history: %v", err)
	}
	if got.Count != 1 || got.TotalCount != 1 || len(got.Entries) != 1 {
		t.Fatalf("rules history counts = count %d total %d entries %d", got.Count, got.TotalCount, len(got.Entries))
	}
	entry := got.Entries[0]
	if entry.Rule != risk.RuleOptionLinePremium || entry.Status != risk.RuleStatusWatch ||
		entry.Evidence != "hedge tier drives the current state" ||
		entry.PolicyFingerprint != pol.FingerprintKey() {
		t.Fatalf("SQLite rule transition did not round-trip: %+v", entry)
	}
}

func TestHistoryIndexParamValidation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &Server{logger: NewLogger(&bytes.Buffer{}, "error")}
	// Validation runs before the store-nil check, so a nil index still
	// classifies bad params as bad requests.
	for name, params := range map[string]rpc.RegimeHistoryParams{
		"bad since":       {Since: "not-a-time"},
		"bad until":       {Until: "2026-13-99"},
		"inverted window": {Since: "2026-07-10", Until: "2026-07-01"},
		"limit over max":  {Limit: 501},
		"negative limit":  {Limit: -1},
	} {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.handleRegimeHistory(&rpc.Request{Params: raw})
		if _, ok := errors.AsType[*badRequestError](err); !ok {
			t.Errorf("%s: err = %v, want bad request", name, err)
		}
	}

	// Nil store with valid params → classified unavailable error.
	if _, err := s.handleRegimeHistory(&rpc.Request{}); !errors.Is(err, errHistoryIndexUnavailable) {
		t.Fatalf("nil store err = %v, want errHistoryIndexUnavailable", err)
	}
	if _, err := s.handleRulesHistory(&rpc.Request{}); !errors.Is(err, errHistoryIndexUnavailable) {
		t.Fatalf("nil store rules err = %v, want errHistoryIndexUnavailable", err)
	}
}

func TestHistoryIndexEnvelopeDefaults(t *testing.T) {
	s := newHistoryIndexServer(t)
	before := time.Now().UTC()
	out, err := s.handleRegimeHistory(&rpc.Request{})
	if err != nil {
		t.Fatalf("handleRegimeHistory: %v", err)
	}
	if out.Limit != historyIndexDefaultLimit || out.Count != 0 || out.TotalCount != 0 || out.Truncated {
		t.Fatalf("empty envelope = %+v", out)
	}
	if out.AsOf.Before(before) || !out.Until.Equal(out.AsOf) {
		t.Fatalf("as_of/until = %v/%v, want now", out.AsOf, out.Until)
	}
	if got := out.Until.Sub(out.Since); got != historyIndexDefaultLookback {
		t.Fatalf("default lookback = %v, want %v", got, historyIndexDefaultLookback)
	}

	// Whole-day until: 2026-07-10 as until must include that entire UTC day.
	raw, _ := json.Marshal(rpc.RegimeHistoryParams{Since: "2026-07-01", Until: "2026-07-10"})
	out, err = s.handleRegimeHistory(&rpc.Request{Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	wantUntil := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	wantSince := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !out.Until.Equal(wantUntil) || !out.Since.Equal(wantSince) {
		t.Fatalf("day grammar window = %v → %v, want %v → %v", out.Since, out.Until, wantSince, wantUntil)
	}

	// Truncation: two journal lines, limit 1.
	s.journalRuleTransitions(&rpc.RulesResult{
		AsOf: time.Now(),
		Rules: []risk.RuleRow{
			{ID: "rule_a", Status: risk.RuleStatusWatch},
			{ID: "rule_b", Status: risk.RuleStatusAct},
		},
	})
	rawLimit, _ := json.Marshal(rpc.RulesHistoryParams{Limit: 1})
	var rules rpc.RulesHistoryResult
	waitForHistory(t, func() (bool, error) {
		out, err := s.handleRulesHistory(&rpc.Request{Params: rawLimit})
		if err != nil {
			return false, err
		}
		rules = *out
		return out.TotalCount == 2, nil
	})
	if rules.Count != 1 || !rules.Truncated || rules.Limit != 1 {
		t.Fatalf("truncated envelope = %+v", rules)
	}
}

// TestHistoryIndexConcurrentAppendsAndQueries exercises journal appends +
// kicks racing the ingest goroutine and RPC reads under -race.
func TestHistoryIndexConcurrentAppendsAndQueries(t *testing.T) {
	s := newHistoryIndexServer(t)
	const writers = 4
	const perWriter = 25
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				res := &rpc.RegimeSnapshotResult{
					AsOf:        time.Now(),
					Fingerprint: rpc.Fingerprint{Key: fmt.Sprintf("fp-%d-%d", w, i)},
					Lifecycle:   rpc.LifecycleState{Stage: "calm"},
				}
				s.journalRegimeDecision(res)
			}
		})
		wg.Go(func() {
			for range perWriter {
				if _, err := s.handleRegimeHistory(&rpc.Request{}); err != nil {
					t.Errorf("query during appends: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	waitForHistory(t, func() (bool, error) {
		out, err := s.handleRegimeHistory(&rpc.Request{})
		if err != nil {
			return false, err
		}
		return out.TotalCount == writers*perWriter && out.Index.IngestedBytes == out.Index.JournalBytes, nil
	})
}

// TestHistoryIndexJournalBytesSingleWrite pins the rules journal
// consolidation: all transition lines from one evaluation land through one
// write, and the bytes are the same shape line-per-line as before.
func TestHistoryIndexJournalBytesSingleWrite(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &Server{}
	s.journalRuleTransitions(&rpc.RulesResult{
		AsOf: time.Now(),
		Rules: []risk.RuleRow{
			{ID: "rule_a", Status: risk.RuleStatusWatch},
			{ID: "rule_b", Status: risk.RuleStatusAct},
		},
	})
	path, err := defaultTradingStatePath("rules-decisions.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("journal lines = %d, want 2", len(lines))
	}
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line is not standalone JSON: %v", err)
		}
		if entry["rule"] == "" || entry["status"] == "" {
			t.Fatalf("line lost fields: %q", line)
		}
	}
}
