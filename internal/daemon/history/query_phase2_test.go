package history

import (
	"context"
	"testing"
	"time"
)

func TestStressHistoryQuerySemantics(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeJournal(t, opts.StressJournalPath, readTestdata(t, "canary-decisions.jsonl"))
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	window := StressQuery{
		Since: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC),
		Limit: 50,
	}
	entries, total, err := s.StressHistory(window)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(entries) != 3 {
		t.Fatalf("unfiltered = %d/%d, want 3/3", len(entries), total)
	}
	// Newest first.
	if !entries[0].At.After(entries[1].At) || !entries[1].At.After(entries[2].At) {
		t.Fatalf("entries not newest-first: %v", entries)
	}

	window.Severity = "act"
	entries, total, err = s.StressHistory(window)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(entries) != 1 || entries[0].Action != "defend" {
		t.Fatalf("severity filter = %d/%d (%+v)", len(entries), total, entries)
	}
	window.Severity = ""
	window.Action = "watch"
	entries, total, err = s.StressHistory(window)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(entries) != 1 || entries[0].Severity != "watch" {
		t.Fatalf("action filter = %d/%d", len(entries), total)
	}
	// Limit cut keeps total honest.
	window.Action = ""
	window.Limit = 1
	entries, total, err = s.StressHistory(window)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(entries) != 1 {
		t.Fatalf("limited = %d/%d, want 1/3", len(entries), total)
	}
}

func TestEquityDaysAndCapitalEventsQuery(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeStatement(t, opts.StatementsDir, "flex-20260713-063000.xml", stmtFixtureA)
	writeStatement(t, opts.StatementsDir, "flex-20260714-063000.xml", stmtFixtureB)
	writeJournal(t, opts.CapitalJournalPath, readTestdata(t, "capital-events.jsonl"))
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	days, total, err := s.EquityDays(EquityQuery{
		Since: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
		Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(days) != 3 {
		t.Fatalf("equity days = %d/%d, want 3/3", len(days), total)
	}
	if days[0].Day != "2026-07-10" || days[2].Day != "2026-07-08" {
		t.Fatalf("days not newest-first: %+v", days)
	}
	if days[1].Day != "2026-07-09" || days[1].EquityBase != 259999.99 {
		t.Fatalf("restated day wrong: %+v", days[1])
	}
	if days[1].WhenGenerated.IsZero() {
		t.Fatal("when_generated not surfaced")
	}
	// Day-boundary semantics: an until of exactly midnight excludes that day.
	_, total, err = s.EquityDays(EquityQuery{
		Since: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
		Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("exclusive-until day count = %d, want 2", total)
	}

	events, truncated, err := s.CapitalEvents(
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || truncated {
		t.Fatalf("capital events = %d (truncated %v), want 3/false", len(events), truncated)
	}
	if events[0].Type != "reconcile" || events[0].ReportID != "rr-golden-1" {
		t.Fatalf("newest event = %+v", events[0])
	}
	if events[2].Type != "deposit" || events[2].AmountBase != 10000 || events[2].EffectiveAt.IsZero() {
		t.Fatalf("oldest event = %+v", events[2])
	}
	// Cap + truncation flag.
	events, truncated, err = s.CapitalEvents(
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || !truncated {
		t.Fatalf("capped events = %d (truncated %v), want 2/true", len(events), truncated)
	}
}

func TestOrdersFreshWatermarkTransitions(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	line := `{"version":1,"at":"2026-07-01T10:00:00Z","type":"previewed","order_ref":"o1","reserved_order_id":7}` + "\n"
	writeJournal(t, opts.OrderJournalPath, line)
	s := openTestStore(t, opts)

	// Cold store: no committed watermark yet → not fresh (the fallback
	// path is exercised on every cold start by design).
	if s.OrdersFresh() {
		t.Fatal("cold store reported fresh")
	}
	s.ingestAll(context.Background())
	if !s.OrdersFresh() {
		t.Fatal("fully-ingested store not fresh")
	}
	raws, err := s.OrderEventLines(nil, nil)
	if err != nil || len(raws) != 1 {
		t.Fatalf("OrderEventLines = %d lines (%v), want 1", len(raws), err)
	}
	maxID, err := s.MaxReservedOrderID()
	if err != nil || maxID != 7 {
		t.Fatalf("MaxReservedOrderID = %d (%v), want 7", maxID, err)
	}

	// Journal grows → stale until the next ingest.
	appendJournal(t, opts.OrderJournalPath, `{"version":1,"at":"2026-07-01T11:00:00Z","type":"send-attempted","order_ref":"o1","preview_token_id":"tok-z"}`+"\n")
	if s.OrdersFresh() {
		t.Fatal("stale store reported fresh after journal growth")
	}
	s.ingestAll(context.Background())
	if !s.OrdersFresh() {
		t.Fatal("re-ingested store not fresh")
	}
	tokenRaws, err := s.OrderEventsForToken(context.Background(), "tok-z")
	if err != nil || len(tokenRaws) != 1 {
		t.Fatalf("OrderEventsForToken = %d (%v), want 1", len(tokenRaws), err)
	}

	// A restart forgets the watermark: not fresh until the first pass.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openTestStore(t, opts)
	if s2.OrdersFresh() {
		t.Fatal("restarted store reported fresh before its first ingest pass")
	}
	s2.ingestAll(context.Background())
	if !s2.OrdersFresh() {
		t.Fatal("restarted store not fresh after ingest")
	}
}

func TestOrdersFreshRequiresCanonicalValidatorAfterReopen(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeJournal(t, opts.OrderJournalPath,
		`{"version":1,"at":"2026-07-01T10:00:00Z","type":"previewed","order_ref":"o1"}`+"\n")
	s := openTestStore(t, opts)
	s.ingestAll(t.Context())
	if !s.OrdersFresh() {
		t.Fatal("validated index is not fresh")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	opts.ValidateOrderLine = nil
	s2 := openTestStore(t, opts)
	s2.ingestAll(t.Context())
	if s2.OrdersFresh() {
		t.Fatal("index reported fresh without the daemon's canonical validator")
	}
}

func TestOrdersEqualSizeReplacementRebuildsAcrossRestart(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	first := `{"version":1,"at":"2026-07-01T10:00:00Z","type":"previewed","order_ref":"same"}` + "\n"
	oldSecond := `{"version":1,"at":"2026-07-01T11:00:00Z","type":"send-attempted","reserved_order_id":111}` + "\n"
	newSecond := `{"version":1,"at":"2026-07-01T11:00:00Z","type":"send-attempted","reserved_order_id":999}` + "\n"
	if len(oldSecond) != len(newSecond) {
		t.Fatal("test replacement must preserve byte length")
	}
	writeJournal(t, opts.OrderJournalPath, first+oldSecond)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	if !s.OrdersFresh() {
		t.Fatal("initial journal not fresh")
	}
	if id, err := s.MaxReservedOrderID(); err != nil || id != 111 {
		t.Fatalf("initial max id = %d (%v), want 111", id, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Preserve size and genesis, the two facts previously used by the
	// no-op path. A cold store must compare the actual indexed evidence
	// before accepting this new generation.
	writeJournal(t, opts.OrderJournalPath, first+newSecond)
	s2 := openTestStore(t, opts)
	if s2.OrdersFresh() {
		t.Fatal("cold replacement reported fresh before validation")
	}
	s2.ingestAll(context.Background())
	if !s2.OrdersFresh() {
		t.Fatal("rebuilt replacement not fresh")
	}
	if id, err := s2.MaxReservedOrderID(); err != nil || id != 999 {
		t.Fatalf("replacement max id = %d (%v), want 999", id, err)
	}
}

func TestMaxReservedOrderIDIgnoresNonPositiveValues(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeJournal(t, opts.OrderJournalPath,
		`{"version":1,"at":"2026-07-01T11:00:00Z","type":"send-attempted","reserved_order_id":-7}`+"\n")
	s := openTestStore(t, opts)
	s.ingestAll(t.Context())
	if !s.OrdersFresh() {
		t.Fatal("orders index is not fresh")
	}
	if got, err := s.MaxReservedOrderID(); err != nil || got != 0 {
		t.Fatalf("MaxReservedOrderID = %d (%v), want 0", got, err)
	}
}

func TestOrdersParseBadSeededAcrossRestart(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeJournal(t, opts.OrderJournalPath, "not json\n")
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	if !s.ordersParseBad() {
		t.Fatal("parse marker not cached")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openTestStore(t, opts)
	if !s2.ordersParseBad() {
		t.Fatal("parse marker not seeded from the reopened DB")
	}
	s2.ingestAll(context.Background())
	if s2.OrdersFresh() {
		t.Fatal("fresh reported over a parse-marker file after restart")
	}
}

func TestHealthPhase2Sources(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeJournal(t, opts.CapitalJournalPath, readTestdata(t, "capital-events.jsonl"))
	writeStatement(t, opts.StatementsDir, "flex-20260713-063000.xml", stmtFixtureA)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	for _, source := range []string{"regime", "rules", "stress", "capital", "risk_policy", "proposal_outcomes", "orders"} {
		if _, err := s.Health(source); err != nil {
			t.Errorf("Health(%s): %v", source, err)
		}
	}
	h, err := s.Health("capital")
	if err != nil {
		t.Fatal(err)
	}
	if h.IngestedBytes == 0 || h.JournalBytes != h.IngestedBytes {
		t.Fatalf("capital health = %+v, want fully ingested", h)
	}
	sh, err := s.StatementsHealth()
	if err != nil {
		t.Fatal(err)
	}
	if sh.IngestedBytes == 0 || sh.JournalBytes != sh.IngestedBytes || sh.LastIngestAt.IsZero() {
		t.Fatalf("statements health = %+v, want fully ingested", sh)
	}
}

// TestHealthPhysicalBytesAfterRotation pins the rotation-aware health
// semantics: IngestedBytes stays comparable to the live JournalBytes
// (offset - base), so the CLI backlog footer stays exact.
func TestHealthPhysicalBytesAfterRotation(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-06"}, 2)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
	h, err := s.Health("regime")
	if err != nil {
		t.Fatal(err)
	}
	if h.JournalBytes == 0 || h.IngestedBytes != h.JournalBytes {
		t.Fatalf("post-rotation health = %+v, want physical bytes fully ingested", h)
	}
}
