package history

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoldenIngestCapital(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	golden := readTestdata(t, "capital-events.jsonl")
	writeJournal(t, opts.CapitalJournalPath, golden)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	if got := countRows(t, s, "capital_events"); got != 3 {
		t.Fatalf("capital_events rows = %d, want 3", got)
	}
	lines := strings.Split(strings.TrimSuffix(golden, "\n"), "\n")
	var at, typ, note, origin, raw string
	var amount *float64
	var atMS int64
	err := s.db.QueryRow(`SELECT at, at_unix_ms, type, amount_base, note, origin, raw_json FROM capital_events WHERE src_offset = 0`).
		Scan(&at, &atMS, &typ, &amount, &note, &origin, &raw)
	if err != nil {
		t.Fatal(err)
	}
	wantAt, _ := time.Parse(time.RFC3339Nano, "2026-05-02T10:00:00.5Z")
	if at != "2026-05-02T10:00:00.5Z" || atMS != wantAt.UnixMilli() {
		t.Errorf("at = %q (%d), want verbatim + normalized", at, atMS)
	}
	if typ != "deposit" || amount == nil || *amount != 10000 || note != "seed" || origin != "cli" {
		t.Errorf("capital fields = %q/%v/%q/%q", typ, amount, note, origin)
	}
	if raw != lines[0] {
		t.Errorf("raw_json not byte-equal")
	}
	// Reconcile line: no amount in the journal (omitted) → NULL column,
	// report id and coverage extracted.
	var recAmount *float64
	var reportID, coverage string
	if err := s.db.QueryRow(`SELECT amount_base, report_id, coverage_to FROM capital_events WHERE type = 'reconcile'`).Scan(&recAmount, &reportID, &coverage); err != nil {
		t.Fatal(err)
	}
	if recAmount != nil || reportID != "rr-golden-1" || coverage != "2026-06-30T00:00:00Z" {
		t.Errorf("reconcile row = %v/%q/%q", recAmount, reportID, coverage)
	}
}

func TestGoldenIngestRiskPolicyAllKinds(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	golden := readTestdata(t, "risk-policy-journal.jsonl")
	writeJournal(t, opts.RiskPolicyJournalPath, golden)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	// The fixture enumerates every kind observed in the tree (12).
	if got := countRows(t, s, "risk_policy_events"); got != 12 {
		t.Fatalf("risk_policy_events rows = %d, want 12", got)
	}
	wantKinds := []string{
		"adjusted_peak_advanced", "adjusted_peak_corrected", "artefact_completed", "capital_state_scoped",
		"capital_tier", "drawdown_block_latched", "drawdown_reset", "equity_observation_rejected",
		"override_expired", "override_granted", "policy_status", "recon_dismiss",
	}
	for _, kind := range wantKinds {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM risk_policy_events WHERE kind = ?`, kind).Scan(&n); err != nil || n != 1 {
			t.Errorf("kind %s rows = %d (%v), want 1", kind, n, err)
		}
	}
	var policyID, fingerprint string
	var version *int
	if err := s.db.QueryRow(`SELECT policy_id, policy_version, policy_fingerprint FROM risk_policy_events WHERE kind = 'policy_status'`).
		Scan(&policyID, &version, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if policyID != "risk-v3" || version == nil || *version != 3 || fingerprint != "sha256:golden-policy" {
		t.Errorf("policy_status fields = %q/%v/%q", policyID, version, fingerprint)
	}
	// raw_json byte-equality for every line.
	lines := strings.Split(strings.TrimSuffix(golden, "\n"), "\n")
	offset := 0
	for _, line := range lines {
		var raw string
		if err := s.db.QueryRow(`SELECT raw_json FROM risk_policy_events WHERE src_offset = ?`, offset).Scan(&raw); err != nil {
			t.Fatalf("row at offset %d: %v", offset, err)
		}
		if raw != line {
			t.Errorf("raw_json at offset %d not byte-equal", offset)
		}
		offset += len(line) + 1
	}
}

func TestGoldenIngestProposalOutcomes(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	golden := readTestdata(t, "trade-proposal-outcomes.jsonl")
	writeJournal(t, opts.ProposalOutcomesPath, golden)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	if got := countRows(t, s, "proposal_outcomes"); got != 3 {
		t.Fatalf("proposal_outcomes rows = %d, want 3", got)
	}
	var state, key, symbol, fingerprint, benchmark string
	var qty, baseline float64
	err := s.db.QueryRow(`SELECT state, proposal_key, symbol, policy_fingerprint, benchmark_symbol, quantity, baseline_price
FROM proposal_outcomes WHERE src_offset = 0`).Scan(&state, &key, &symbol, &fingerprint, &benchmark, &qty, &baseline)
	if err != nil {
		t.Fatal(err)
	}
	if state != "submitted" || key != "pk-1" || symbol != "TSYM" || benchmark != "SPY" || qty != 10 || baseline != 101.5 {
		t.Errorf("submitted row = %q/%q/%q/%q/%v/%v", state, key, symbol, benchmark, qty, baseline)
	}
	// The fingerprint column is the {version,key} object's key (D5).
	if fingerprint != "sha256:golden-prot" {
		t.Errorf("policy_fingerprint = %q, want the object key", fingerprint)
	}
	var pnl float64
	var execID string
	if err := s.db.QueryRow(`SELECT execution_pnl, exec_id FROM proposal_outcomes WHERE state = 'filled'`).Scan(&pnl, &execID); err != nil {
		t.Fatal(err)
	}
	if pnl != -7.5 || execID != "x-1" {
		t.Errorf("filled row = %v/%q", pnl, execID)
	}
}

func TestGoldenIngestStress(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	golden := readTestdata(t, "canary-decisions.jsonl")
	writeJournal(t, opts.StressJournalPath, golden)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	if got := countRows(t, s, "stress_transitions"); got != 3 {
		t.Fatalf("stress_transitions rows = %d, want 3", got)
	}
	lines := strings.Split(strings.TrimSuffix(golden, "\n"), "\n")
	var at, sessionKey, fingerprint, account, mode, action, severity, direction, stage, health, summary, raw string
	var alertRelevant *bool
	err := s.db.QueryRow(`SELECT at, session_key, fingerprint, account, account_mode, action, severity, direction, market_stage,
 portfolio_alert_relevant, input_health, summary, raw_json FROM stress_transitions WHERE src_offset = 0`).
		Scan(&at, &sessionKey, &fingerprint, &account, &mode, &action, &severity, &direction, &stage, &alertRelevant, &health, &summary, &raw)
	if err != nil {
		t.Fatal(err)
	}
	if at != "2026-07-05T13:30:00.75+02:00" || sessionKey != "2026-07-05" || fingerprint != "sha256:golden-canary" {
		t.Errorf("identity fields = %q/%q/%q", at, sessionKey, fingerprint)
	}
	if account != "UGOLDEN" || mode != "live" || action != "watch" || severity != "watch" || direction != "defensive" {
		t.Errorf("decision fields = %q/%q/%q/%q/%q", account, mode, action, severity, direction)
	}
	if stage != "early_warning" || health != "ok" || summary != "golden canary summary line" {
		t.Errorf("evidence fields = %q/%q/%q", stage, health, summary)
	}
	if alertRelevant == nil || !*alertRelevant {
		t.Errorf("portfolio_alert_relevant = %v, want true", alertRelevant)
	}
	if raw != lines[0] {
		t.Errorf("raw_json not byte-equal")
	}
	// Unstamped line → NULL.
	var minimalRelevant *bool
	if err := s.db.QueryRow(`SELECT portfolio_alert_relevant FROM stress_transitions WHERE fingerprint = 'sha256:calm'`).Scan(&minimalRelevant); err != nil {
		t.Fatal(err)
	}
	if minimalRelevant != nil {
		t.Errorf("unstamped portfolio_alert_relevant = %v, want NULL", minimalRelevant)
	}
	var falseRelevant *bool
	if err := s.db.QueryRow(`SELECT portfolio_alert_relevant FROM stress_transitions WHERE fingerprint = 'sha256:defend'`).Scan(&falseRelevant); err != nil {
		t.Fatal(err)
	}
	if falseRelevant == nil || *falseRelevant {
		t.Errorf("stamped-false portfolio_alert_relevant = %v, want false", falseRelevant)
	}
}

func TestGoldenIngestOrdersParseOK(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	golden := readTestdata(t, "order-journal.jsonl")
	writeJournal(t, opts.OrderJournalPath, golden)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	// Every line stored — including the JSON-bad and version≠1 lines.
	lines := strings.Split(strings.TrimSuffix(golden, "\n"), "\n")
	if got := countRows(t, s, "order_events"); got != len(lines) {
		t.Fatalf("order_events rows = %d, want %d (nothing skipped)", got, len(lines))
	}
	var bad int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM order_events WHERE parse_ok = 0`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 2 {
		t.Fatalf("parse_ok=0 rows = %d, want 2 (bad JSON + version 2)", bad)
	}
	// Bad lines keep their verbatim bytes.
	var raw string
	if err := s.db.QueryRow(`SELECT raw_json FROM order_events WHERE parse_ok = 0 ORDER BY id LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "this line is not JSON at all" {
		t.Errorf("bad line raw_json = %q", raw)
	}
	// Parsed columns on a good line.
	var typ, ref, token, sendState string
	var reserved, permID int
	if err := s.db.QueryRow(`SELECT type, order_ref, preview_token_id, send_state, reserved_order_id, perm_id
FROM order_events WHERE type = 'broker-acknowledged'`).Scan(&typ, &ref, &token, &sendState, &reserved, &permID); err != nil {
		t.Fatal(err)
	}
	if ref != "ord-1" || sendState != "broker_acknowledged" || reserved != 501 || permID != 9001 || token != "" {
		t.Errorf("broker-acknowledged row = %q/%q/%q/%d/%d", ref, token, sendState, reserved, permID)
	}
	// Parse-marker flag is cached for freshness checks.
	if !s.ordersParseBad() {
		t.Fatal("ordersParseBad not set after ingesting marker lines")
	}
	if s.OrdersFresh() {
		t.Fatal("OrdersFresh must be false while parse markers exist")
	}
	if _, err := s.OrderEventLines(nil, nil); err == nil {
		t.Fatal("OrderEventLines must refuse to serve over parse markers")
	}
}

const stmtFixtureA = `<FlexQueryResponse queryName="recon" type="AF">
 <FlexStatements count="1">
  <FlexStatement accountId="U1234567" fromDate="20260706" toDate="20260712" whenGenerated="20260713;063000">
   <EquitySummaryInBase>
    <EquitySummaryByReportDateInBase reportDate="20260708" total="261234.56" />
    <EquitySummaryByReportDateInBase reportDate="20260709" total="259100.10" />
   </EquitySummaryInBase>
  </FlexStatement>
 </FlexStatements>
</FlexQueryResponse>`

const stmtFixtureB = `<FlexQueryResponse queryName="recon" type="AF">
 <FlexStatements count="1">
  <FlexStatement accountId="U1234567" fromDate="20260707" toDate="20260713" whenGenerated="20260714;063000">
   <EquitySummaryInBase>
    <EquitySummaryByReportDateInBase reportDate="20260709" total="259999.99" />
    <EquitySummaryByReportDateInBase reportDate="20260710" total="260500.00" />
   </EquitySummaryInBase>
  </FlexStatement>
 </FlexStatements>
</FlexQueryResponse>`

func writeStatement(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStatementIngestNewestWins(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeStatement(t, opts.StatementsDir, "flex-20260713-063000.xml", stmtFixtureA)
	writeStatement(t, opts.StatementsDir, "flex-20260714-063000.xml", stmtFixtureB)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	if got := countRows(t, s, "statement_files"); got != 2 {
		t.Fatalf("statement_files rows = %d, want 2", got)
	}
	if got := countRows(t, s, "statement_equity_days"); got != 3 {
		t.Fatalf("statement_equity_days rows = %d, want 3 (0708, 0709, 0710)", got)
	}
	// Overlapping day 2026-07-09: the newer whenGenerated wins.
	var equity float64
	var source string
	if err := s.db.QueryRow(`SELECT equity_base, source_stmt FROM statement_equity_days WHERE day = '2026-07-09'`).Scan(&equity, &source); err != nil {
		t.Fatal(err)
	}
	if equity != 259999.99 || source != "flex-20260714-063000.xml" {
		t.Fatalf("restated day = %v from %q, want newest statement", equity, source)
	}

	// Idempotent re-run.
	s.ingestAll(context.Background())
	if got := countRows(t, s, "statement_equity_days"); got != 3 {
		t.Fatalf("re-run changed equity days: %d", got)
	}

	// A new statement restating a day upserts it.
	restated := strings.ReplaceAll(stmtFixtureB, "20260714;063000", "20260715;063000")
	restated = strings.ReplaceAll(restated, "259999.99", "258000.00")
	writeStatement(t, opts.StatementsDir, "flex-20260715-063000.xml", restated)
	s.ingestAll(context.Background())
	if err := s.db.QueryRow(`SELECT equity_base, source_stmt FROM statement_equity_days WHERE day = '2026-07-09'`).Scan(&equity, &source); err != nil {
		t.Fatal(err)
	}
	if equity != 258000.00 || source != "flex-20260715-063000.xml" {
		t.Fatalf("restatement did not win: %v from %q", equity, source)
	}
	// An OLDER statement can never overwrite a newer day value.
	older := strings.ReplaceAll(stmtFixtureB, "20260714;063000", "20260712;063000")
	older = strings.ReplaceAll(older, "259999.99", "111111.11")
	writeStatement(t, opts.StatementsDir, "flex-20260712-063000.xml", older)
	s.ingestAll(context.Background())
	if err := s.db.QueryRow(`SELECT equity_base FROM statement_equity_days WHERE day = '2026-07-09'`).Scan(&equity); err != nil {
		t.Fatal(err)
	}
	if equity != 258000.00 {
		t.Fatalf("older statement overwrote a newer value: %v", equity)
	}
}

func TestStatementIngestParseFailureRetries(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeStatement(t, opts.StatementsDir, "flex-broken.xml", "<FlexQueryResponse><oops")
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	if got := countRows(t, s, "statement_files"); got != 0 {
		t.Fatalf("broken statement recorded (%d rows); it must retry next pass", got)
	}
	// Fix the file in place: the size change triggers re-parse.
	writeStatement(t, opts.StatementsDir, "flex-broken.xml", stmtFixtureA)
	s.ingestAll(context.Background())
	if got := countRows(t, s, "statement_files"); got != 1 {
		t.Fatalf("fixed statement not ingested: %d rows", got)
	}
}

func TestPhase2SourcesIdempotencyAndTornTail(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	complete := `{"version":1,"at":"2026-07-01T10:00:00Z","type":"deposit","amount_base":5}` + "\n"
	torn := `{"version":1,"at":"2026-07-01T11:00:00Z","type":"withdr`
	writeJournal(t, opts.CapitalJournalPath, complete+torn)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	if got := countRows(t, s, "capital_events"); got != 1 {
		t.Fatalf("rows with torn tail = %d, want 1", got)
	}
	if got, want := sourceOffset(t, s, sourceCapital), int64(len(complete)); got != want {
		t.Fatalf("offset = %d, want %d", got, want)
	}
	appendJournal(t, opts.CapitalJournalPath, `awal","amount_base":6}`+"\n")
	s.ingestAll(context.Background())
	s.ingestAll(context.Background())
	if got := countRows(t, s, "capital_events"); got != 2 {
		t.Fatalf("rows after completing tail = %d, want exactly 2", got)
	}
}
