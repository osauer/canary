package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// testCanaryResult is a fully-populated canary snapshot for round-trip
// drift guards.
func testCanaryResult(key string) *rpc.StressResult {
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

// TestHistoryIndexCanaryRoundTrip is the canary writer→parser drift
// guard: a decision journaled by the REAL canaryDecisionJournal must come
// back from stress.history with the same fields.
func TestHistoryIndexCanaryRoundTrip(t *testing.T) {
	s := newHistoryIndexServer(t)
	now := time.Now()
	s.journalCanaryDecision(testCanaryResult("sha256:canary-roundtrip"))

	var got rpc.StressHistoryResult
	waitForHistory(t, func() (bool, error) {
		out, err := s.handleStressHistory(&rpc.Request{})
		if err != nil {
			return false, err
		}
		got = *out
		return out.Count == 1, nil
	})
	e := got.Entries[0]
	if e.Fingerprint != "sha256:canary-roundtrip" || e.Action != "watch" || e.Severity != "watch" || e.Direction != "defensive" {
		t.Fatalf("decision fields did not round-trip: %+v", e)
	}
	if e.MarketStage != "early_warning" || e.InputHealth != "ok" || e.Summary != "round-trip canary summary" {
		t.Fatalf("evidence fields did not round-trip: %+v", e)
	}
	if e.PortfolioAlertRelevant == nil || !*e.PortfolioAlertRelevant {
		t.Fatalf("portfolio_alert_relevant did not round-trip: %+v", e.PortfolioAlertRelevant)
	}
	if e.SessionKey != nyTradingSessionKey(nyTime(now)) {
		t.Fatalf("session key = %q, want writer's", e.SessionKey)
	}
	if got.Index.IngestedBytes == 0 || got.Index.JournalBytes != got.Index.IngestedBytes {
		t.Fatalf("index health = %+v, want fully ingested", got.Index)
	}
}

// TestCanaryJournalDedupeHeartbeatAndGate pins the journal's dedupe,
// heartbeat, and runtime-disable semantics.
func TestCanaryJournalDedupeHeartbeatAndGate(t *testing.T) {
	s := newHistoryIndexServer(t)
	j := s.canaryDecisions
	res := testCanaryResult("sha256:dedupe")

	base := time.Now()
	if err := j.append(base, "", "", res); err != nil {
		t.Fatal(err)
	}
	if err := j.append(base.Add(time.Minute), "", "", res); err != nil {
		t.Fatal(err) // same fingerprint inside the heartbeat: deduped
	}
	if err := j.append(base.Add(canaryDecisionHeartbeat+time.Second), "", "", res); err != nil {
		t.Fatal(err) // heartbeat: journaled again
	}
	if err := j.append(base.Add(canaryDecisionHeartbeat+2*time.Second), "", "", testCanaryResult("sha256:changed")); err != nil {
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

	// Runtime disable: journalCanaryDecision must not append. Mutate the
	// installed store through its own lock (the maintenance loop reads it).
	if err := s.platformSettings.update(func(next *platformSettingsData) error {
		disabled := false
		next.Stress.Journal.Enabled = &disabled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.journalCanaryDecision(testCanaryResult("sha256:while-disabled"))
	after, err := os.ReadFile(j.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(data) {
		t.Fatal("disabled canary journal still appended")
	}
}

// TestCanaryJournalLoopSkipsWhenDisconnected pins the cadence-loop gate:
// no gateway connector → no broker round-trips, no journal write.
func TestCanaryJournalLoopSkipsWhenDisconnected(t *testing.T) {
	s := newHistoryIndexServer(t)
	s.canaryJournalTick(context.Background())
	if _, err := os.Stat(s.canaryDecisions.path); !os.IsNotExist(err) {
		t.Fatalf("disconnected tick touched the journal (stat err %v)", err)
	}
}

// TestComposeBriefJournalsCanaryDecision proves the brief hook: rendering
// a brief journals the canary it computed.
func TestComposeBriefJournalsCanaryDecision(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	s.installCanaryDecisionJournal()
	if s.canaryDecisions == nil {
		t.Fatal("canary journal not installed")
	}
	_, _ = s.composeBrief(context.Background())
	data, err := os.ReadFile(s.canaryDecisions.path)
	if err != nil {
		t.Fatalf("brief did not journal a canary decision: %v", err)
	}
	var line canaryDecisionLine
	if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &line); err != nil {
		t.Fatalf("journaled line does not decode: %v", err)
	}
	if line.V != 1 || line.Summary == "" {
		t.Fatalf("journaled line incomplete: %+v", line)
	}
}

// TestHistoryIndexCapitalAndRiskPolicyRoundTrip drives the REAL capital
// and governance journal writers and proves the index parsers track them.
func TestHistoryIndexCapitalAndRiskPolicyRoundTrip(t *testing.T) {
	s := newHistoryIndexServer(t)
	ev, err := s.riskCapital.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "deposit", AmountBase: 1234.5, Note: "round-trip"}, "human-tty")
	if err != nil {
		t.Fatal(err)
	}
	s.journalRiskPolicyTransition("absent", "active", nil)

	var got rpc.ReconEquityResult
	waitForHistory(t, func() (bool, error) {
		out, err := s.handleReconEquity(&rpc.Request{})
		if err != nil {
			return false, err
		}
		got = *out
		return len(out.Events) == 1, nil
	})
	e := got.Events[0]
	if e.Type != "deposit" || e.AmountBase != 1234.5 || e.Note != "round-trip" || e.Origin != "human-tty" {
		t.Fatalf("capital event did not round-trip: %+v", e)
	}
	if e.At.UnixMilli() != ev.At.UnixMilli() {
		t.Fatalf("at = %v, want %v", e.At, ev.At)
	}
	if len(got.Days) != 0 || got.Count != 0 {
		t.Fatalf("no statements retained, but days = %+v", got.Days)
	}

	// risk_policy_events row via direct read (no RPC surface for it).
	db, err := sql.Open("sqlite", "file:"+s.historyIndexOpts.DBPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	waitForHistory(t, func() (bool, error) {
		var kind string
		err := db.QueryRow(`SELECT kind FROM risk_policy_events WHERE kind = 'policy_status'`).Scan(&kind)
		if err != nil {
			return false, nil //nolint:nilerr // row may not be ingested yet
		}
		return kind == "policy_status", nil
	})
}

// TestHistoryIndexProposalOutcomeRoundTrip drives the REAL AppendMark
// writer.
func TestHistoryIndexProposalOutcomeRoundTrip(t *testing.T) {
	s := newHistoryIndexServer(t)
	mark := proposalOutcomeMark{
		State:             proposalOutcomeStateMarked,
		ProposalKey:       "pk-roundtrip",
		Symbol:            "TSYM",
		Quantity:          4,
		BaselinePrice:     10.5,
		MarkPrice:         9.75,
		PolicyFingerprint: rpc.Fingerprint{Version: "v1", Key: "sha256:prot-roundtrip"},
		BenchmarkSymbol:   "SPY",
	}
	if err := s.proposalOutcomes.AppendMark(mark); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+s.historyIndexOpts.DBPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	waitForHistory(t, func() (bool, error) {
		var key, fingerprint string
		var markPrice float64
		err := db.QueryRow(`SELECT proposal_key, policy_fingerprint, mark_price FROM proposal_outcomes WHERE proposal_key = 'pk-roundtrip'`).
			Scan(&key, &fingerprint, &markPrice)
		if err != nil {
			return false, nil //nolint:nilerr // not ingested yet
		}
		if fingerprint != "sha256:prot-roundtrip" || markPrice != 9.75 {
			t.Fatalf("outcome fields did not round-trip: %q %v", fingerprint, markPrice)
		}
		return true, nil
	})
}

// TestSQLiteOrderJournalRoundTrip drives the real order adapter directly
// against daemon.db. history.db and JSONL are not freshness or fallback
// layers after the authority cutover.
func TestSQLiteOrderJournalRoundTrip(t *testing.T) {
	s := newHistoryIndexServer(t)
	authority := attachFreshOrderTestAuthority(t, s)
	if err := s.orderJournal.Append(orderJournalEvent{
		At: time.Now().UTC(), Type: orderJournalEventPreviewed,
		OrderRef: "ord-rt", PreviewTokenID: "tok-rt", ReservedOrderID: 42,
		Endpoint: "127.0.0.1:4002", ClientID: 31,
		Account: "UTEST", Mode: "paper", SendState: orderSendStateReserved,
	}); err != nil {
		t.Fatal(err)
	}
	events, ok := s.indexedOrderEvents("test", nil, nil)
	if !ok || len(events) != 1 {
		t.Fatalf("indexed events = %d ok=%v, want 1/true", len(events), ok)
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

	s := newHistoryIndexServer(t)
	out := s.platformSettingsSnapshot(nil)
	if out.History.Rotation.Enabled.Value || out.History.Rotation.Enabled.Access != rpc.SettingsAccessRead || out.History.Rotation.KeepRawMonths.Value != 0 {
		t.Fatalf("retired history settings = %+v", out.History)
	}
	if !out.Stress.Journal.Enabled.Value {
		t.Fatalf("default stress settings = %+v", out.Stress)
	}
	if s.historyMaintenancePass(context.Background()) {
		t.Fatal("retired history maintenance pass ran")
	}
}

// TestPurgeEvidenceInvariant is D12.2: a purge-shaped flow grows only the
// authoritative SQLite event stream, and purge_id rows remain intact.
func TestPurgeEvidenceInvariant(t *testing.T) {
	s := newHistoryIndexServer(t)
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

	events, ok := s.indexedOrderEvents("test", nil, nil)
	if !ok {
		t.Fatal("index not serving after purge-shaped appends")
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
	if s.purgeLedger != nil {
		if err := s.purgeLedger.UseCoreStore(authority); err != nil {
			_ = authority.Close()
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = authority.Close() })
	return authority
}
