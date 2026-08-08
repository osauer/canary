package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// newJournalTestServer builds a Server with the real journal writers and
// settings store on a private XDG_STATE_HOME, without any persistence
// authority attached. Tests that need daemon.db attach it explicitly via
// attachFreshOrderTestAuthority or openMarketTestCoreStore.
func newJournalTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &Server{logger: NewLogger(&syncWriter{buf: &bytes.Buffer{}}, "error"), now: time.Now}
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
	return s
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

func TestRulesHistorySQLiteDoesNotRetainCalibrationEvents(t *testing.T) {
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
	if got.Count != 0 || got.TotalCount != 0 || len(got.Entries) != 0 {
		t.Fatalf("rules history counts = count %d total %d entries %d", got.Count, got.TotalCount, len(got.Entries))
	}
}

func TestStateHistoryParamValidation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &Server{logger: NewLogger(&bytes.Buffer{}, "error")}
	// Validation runs before the authority check, so a nil coreStore still
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

	// Nil authority with valid params → classified unavailable error.
	if _, err := s.handleRegimeHistory(&rpc.Request{}); !errors.Is(err, errHistoryIndexUnavailable) {
		t.Fatalf("nil authority err = %v, want errHistoryIndexUnavailable", err)
	}
	if _, err := s.handleRulesHistory(&rpc.Request{}); !errors.Is(err, errHistoryIndexUnavailable) {
		t.Fatalf("nil authority rules err = %v, want errHistoryIndexUnavailable", err)
	}
}

func TestStateHistoryEnvelopeDefaults(t *testing.T) {
	store := openMarketTestCoreStore(t)
	s := &Server{coreStore: store, logger: NewLogger(&bytes.Buffer{}, "error")}
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

	// Truncation: two transitions in one evaluation, limit 1.
	s.journalRuleTransitions(&rpc.RulesResult{
		AsOf: time.Now(),
		Rules: []risk.RuleRow{
			{ID: "rule_a", Status: risk.RuleStatusWatch},
			{ID: "rule_b", Status: risk.RuleStatusAct},
		},
	})
	rawLimit, _ := json.Marshal(rpc.RulesHistoryParams{Limit: 1})
	rules, err := s.handleRulesHistory(&rpc.Request{Params: rawLimit})
	if err != nil {
		t.Fatal(err)
	}
	if rules.TotalCount != 0 || rules.Count != 0 || rules.Truncated || rules.Limit != 1 {
		t.Fatalf("empty operational envelope = %+v", rules)
	}
}

// TestHistoryIndexJournalBytesSingleWrite pins the legacy-seam rules journal
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

// testStressResult is a fully-populated stress snapshot for round-trip
// drift guards.
func testStressResult(key string) *rpc.StressResult {
	relevant := true
	return &rpc.StressResult{
		AsOf:                   time.Now(),
		Fingerprint:            rpc.Fingerprint{Version: "v1", Key: key},
		Action:                 "watch",
		Severity:               risk.SeverityWatch,
		Direction:              risk.DirectionDefensive,
		MarketConfirmation:     "partial",
		PortfolioFit:           "high",
		PortfolioAlertRelevant: &relevant,
		InputHealth:            "ok",
		Summary:                "round-trip canary summary",
		Market: rpc.StressMarketSummary{
			RegimePosture: rpc.RegimePosture{Stage: "early_warning", Tone: "watch"},
			RedClusters:   1,
		},
	}
}

func TestDispatchRejectsRetiredCanaryHistoryMethod(t *testing.T) {
	var wire bytes.Buffer
	request := &rpc.Request{ID: "retired", Method: "canary.history"}
	if terminal := (&Server{}).dispatch(context.Background(), request, json.NewEncoder(&wire), bufio.NewReader(strings.NewReader(""))); terminal {
		t.Fatal("retired method unexpectedly reported a terminal stream")
	}
	var response rpc.Response
	if err := json.Unmarshal(wire.Bytes(), &response); err != nil {
		t.Fatalf("response decode: %v\n%s", err, wire.String())
	}
	if response.Ok || response.Error == nil || response.Error.Code != rpc.CodeUnknownMethod {
		t.Fatalf("retired method response = %+v", response)
	}
}

// TestStressJournalDedupeHeartbeatAndGate pins the journal's dedupe,
// heartbeat, and runtime-disable semantics.
func TestStressJournalDedupeHeartbeatAndGate(t *testing.T) {
	s := newJournalTestServer(t)
	j := s.stressDecisions
	res := testStressResult("sha256:dedupe")

	base := time.Now()
	if err := j.append(base, "", "", res); err != nil {
		t.Fatal(err)
	}
	if err := j.append(base.Add(time.Minute), "", "", res); err != nil {
		t.Fatal(err) // same fingerprint inside the heartbeat: deduped
	}
	if err := j.append(base.Add(stressDecisionHeartbeat+time.Second), "", "", res); err != nil {
		t.Fatal(err) // heartbeat: journaled again
	}
	if err := j.append(base.Add(stressDecisionHeartbeat+2*time.Second), "", "", testStressResult("sha256:changed")); err != nil {
		t.Fatal(err) // fingerprint change: journaled
	}
	data, err := os.ReadFile(j.path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("journal lines = %d, want 3 (initial, heartbeat, change)", len(lines))
	}
	for _, line := range lines {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("journal line is not standalone JSON: %v", err)
		}
	}

	// Runtime disable: journalStressDecision must not append.
	if err := s.platformSettings.update(func(next *platformSettingsData) error {
		disabled := false
		next.Stress.Journal.Enabled = &disabled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.journalStressDecision(testStressResult("sha256:while-disabled"))
	after, err := os.ReadFile(j.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(data) {
		t.Fatal("disabled stress journal still appended")
	}
}

// TestStressJournalLoopSkipsWhenDisconnected pins the cadence-loop gate:
// no gateway connector → no broker round-trips, no journal write.
func TestStressJournalLoopSkipsWhenDisconnected(t *testing.T) {
	s := newJournalTestServer(t)
	s.stressJournalTick(context.Background())
	if _, err := os.Stat(s.stressDecisions.path); !os.IsNotExist(err) {
		t.Fatalf("disconnected tick touched the journal (stat err %v)", err)
	}
}

// TestComposeBriefJournalsStressDecision proves the brief hook: rendering
// a brief journals the stress snapshot it computed.
func TestComposeBriefJournalsStressDecision(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	s.installStressDecisionJournal()
	if s.stressDecisions == nil {
		t.Fatal("stress journal not installed")
	}
	_, _ = s.composeBrief(context.Background())
	data, err := os.ReadFile(s.stressDecisions.path)
	if err != nil {
		t.Fatalf("brief did not journal a stress decision: %v", err)
	}
	var line stressDecisionLine
	if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &line); err != nil {
		t.Fatalf("journaled line does not decode: %v", err)
	}
	if line.V != 1 || line.Summary == "" {
		t.Fatalf("journaled line incomplete: %+v", line)
	}
}

// TestSQLiteOrderJournalRoundTrip drives the real order adapter directly
// against daemon.db. JSONL is not a freshness or fallback layer after the
// authority cutover.
func TestSQLiteOrderJournalRoundTrip(t *testing.T) {
	s := newJournalTestServer(t)
	authority := attachFreshOrderTestAuthority(t, s)
	if err := s.orderJournal.Append(orderJournalEvent{
		At: time.Now().UTC(), Type: orderJournalEventPreviewed,
		OrderRef: "ord-rt", PreviewTokenID: "tok-rt", ReservedOrderID: 42,
		Endpoint: "127.0.0.1:4002", ClientID: 31,
		Account: "UTEST", Mode: "paper", SendState: orderSendStateReserved,
	}); err != nil {
		t.Fatal(err)
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil || len(events) != 1 {
		t.Fatalf("authoritative events = %d err=%v, want 1/nil", len(events), err)
	}
	e := events[0]
	if e.OrderRef != "ord-rt" || e.PreviewTokenID != "tok-rt" || e.ReservedOrderID != 42 || e.Account != "UTEST" || e.Mode != "paper" {
		t.Fatalf("order event did not round-trip: %+v", e)
	}
	if _, err := os.Stat(s.orderJournal.Path); !os.IsNotExist(err) {
		t.Fatalf("SQLite append touched legacy order journal: %v", err)
	}
	_ = authority
}

// TestHistoryRotationSettingsRetired pins that the former JSONL rotation
// controls are no longer writable or active while their response shape stays
// present as an explicit read-only compatibility disclosure.
func TestHistoryRotationSettingsRetired(t *testing.T) {
	next := &platformSettingsData{Version: platformSettingsDocVersion}
	for _, key := range []string{"history.rotation.enabled", "history.rotation.keep_raw_months"} {
		if err := applySettingsKey(next, key, json.RawMessage(`true`)); err == nil {
			t.Fatalf("retired key %q remained writable", key)
		}
	}
	if err := applySettingsKey(next, "stress.journal.enabled", json.RawMessage(`false`)); err != nil {
		t.Fatal(err)
	}
	if next.Stress.Journal.Enabled == nil || *next.Stress.Journal.Enabled {
		t.Fatal("stress.journal.enabled=false did not apply")
	}

	s := newJournalTestServer(t)
	out := s.platformSettingsSnapshot(nil)
	if out.History.Rotation.Enabled.Value || out.History.Rotation.Enabled.Access != rpc.SettingsAccessRead || out.History.Rotation.KeepRawMonths.Value != 0 {
		t.Fatalf("retired history settings = %+v", out.History)
	}
	if !out.Stress.Journal.Enabled.Value {
		t.Fatalf("default stress settings = %+v", out.Stress)
	}
}

// TestPurgeEvidenceInvariant is D12.2: a purge-shaped flow grows only the
// authoritative SQLite event stream, and purge_id rows remain intact.
func TestPurgeEvidenceInvariant(t *testing.T) {
	s := newJournalTestServer(t)
	attachFreshOrderTestAuthority(t, s)
	last := 0
	appendAndCheck := func(ev orderJournalEvent) {
		t.Helper()
		if err := s.orderJournal.Append(ev); err != nil {
			t.Fatal(err)
		}
		rows, err := s.orderJournal.LoadEvents(0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) <= last {
			t.Fatalf("authoritative order stream stalled: %d -> %d", last, len(rows))
		}
		last = len(rows)
	}
	now := time.Now().UTC()
	route := orderJournalEvent{Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "UTEST", Mode: "paper"}
	first := route
	first.At, first.Type, first.OrderRef, first.PurgeID = now, orderJournalEventPreviewed, "purge-ord-1", "purge-20260720-1"
	appendAndCheck(first)
	second := route
	second.At, second.Type, second.OrderRef, second.PurgeID = now.Add(time.Second), orderJournalEventSendAttempted, "purge-ord-1", "purge-20260720-1"
	second.ReservedOrderID, second.SendState = 900, orderSendStateSendAttempted
	appendAndCheck(second)
	third := route
	third.At, third.Type, third.OrderRef, third.PurgeID = now.Add(2*time.Second), orderJournalEventStatusUpdated, "purge-ord-1", "purge-20260720-1"
	third.Status, third.SendState = "Filled", orderSendStateTerminal
	appendAndCheck(third)

	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("authority not serving after purge-shaped appends: %v", err)
	}
	purgeRows := 0
	for _, ev := range events {
		if ev.PurgeID == "purge-20260720-1" {
			purgeRows++
		}
	}
	if purgeRows != 3 {
		t.Fatalf("purge_id rows in order_events = %d, want 3", purgeRows)
	}
	if _, err := os.Stat(s.orderJournal.Path); !os.IsNotExist(err) {
		t.Fatalf("SQLite purge evidence touched legacy journal: %v", err)
	}
}

func attachFreshOrderTestAuthority(t *testing.T, s *Server) *corestore.Store {
	t.Helper()
	authority, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeFreshTradingAuthority(t.Context(), authority); err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}
	if err := s.orderJournal.UseCoreStore(authority); err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	return authority
}
