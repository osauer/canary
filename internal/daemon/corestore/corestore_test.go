package corestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(privateTempDir(t), "daemon.db")
	s, err := Open(t.Context(), Options{Path: path})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func testScope(key string) BrokerScope {
	return BrokerScope{ScopeKey: key, Endpoint: "127.0.0.1:7497", ClientID: 71, Account: "UTEST", Mode: "paper"}
}

func orderEvent(scope BrokerScope, key, token string, floor int64) OrderEventRecord {
	return OrderEventRecord{Scope: scope, EventKey: key, AtMS: time.Now().UnixMilli(), Type: "pre-transmit", Action: ActionPlace, Origin: OriginAgentCLI, PreviewTokenID: token, ReservedOrderID: floor, RawJSON: []byte(`{"version":1,"type":"pre-transmit"}`)}
}

func TestOpenCreatesPrivateAuthoritativeSchema(t *testing.T) {
	s, path := openTestStore(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode=%o want 600", got)
	}
	for pragma, want := range map[string]int64{"synchronous": 2, "foreign_keys": 1, "busy_timeout": 5000, "fullfsync": 1, "checkpoint_fullfsync": 1} {
		var got int64
		if err := s.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s=%d want %d", pragma, got, want)
		}
	}
	var journal string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal=%q err=%v", journal, err)
	}
	expected := []string{"store_meta", "schema_migrations", "legacy_imports", "state_documents", "event_log", "regime_decisions", "regime_indicators", "rule_transitions", "stress_transitions", "capital_events", "risk_policy_events", "proposal_outcomes", "order_events", "consumed_preview_tokens", "order_id_floors", "statement_files", "statement_file_versions", "statement_equity_days", "statement_equity_day_versions", "observations"}
	for _, table := range expected {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("required table %s count=%d err=%v", table, n, err)
		}
	}
	report, err := s.CheckIntegrity(t.Context())
	if err != nil || !report.OK() {
		t.Fatalf("integrity=%+v err=%v", report, err)
	}
	head, err := s.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(head.AuthorityEpoch) != 32 || head.HeadGeneration != 0 || head.LastEventSeq != 0 || head.SignerGeneration != 1 {
		t.Fatalf("unexpected initial authority head: %+v", head)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), Options{Path: path, MinimumHead: &head})
	if err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
	defer reopened.Close()
	var migrationsCount int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&migrationsCount); err != nil || migrationsCount != len(migrations) {
		t.Fatalf("migration rows=%d err=%v", migrationsCount, err)
	}
}

func TestCommitObserverTracksDurableHeadAndFailureBlocksStore(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "daemon.db")
	var (
		mu       sync.Mutex
		observed []AuthorityHead
		fail     atomic.Bool
	)
	store, err := Open(t.Context(), Options{
		Path: path,
		CommitObserver: func(head AuthorityHead) error {
			if fail.Load() {
				return errors.New("watermark unavailable")
			}
			mu.Lock()
			observed = append(observed, head)
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{
		ScopeKey: "test", Kind: "observer", JSON: []byte(`{"v":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	head, err := store.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(observed) != 1 || observed[0] != head {
		t.Fatalf("observed heads=%+v, live=%+v", observed, head)
	}
	mu.Unlock()

	fail.Store(true)
	_, err = store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{
		ScopeKey: "test", Kind: "observer", ExpectedRevision: doc.Revision, JSON: []byte(`{"v":2}`),
	})
	if err == nil || !strings.Contains(err.Error(), "persist committed authority head") {
		t.Fatalf("observer failure error=%v", err)
	}
	if health := store.Health(); health.Ready || health.Code != "head_watermark" {
		t.Fatalf("health after observer failure=%+v", health)
	}
	if _, err := store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{
		ScopeKey: "test", Kind: "blocked", JSON: []byte(`{}`),
	}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("mutation after observer failure=%v, want ErrBlocked", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(t.Context(), Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	committed, ok, err := reopened.GetStateDocument(t.Context(), "test", "observer")
	if err != nil || !ok || committed.Revision != 2 || string(committed.JSON) != `{"v":2}` {
		t.Fatalf("committed mutation after observer failure: doc=%+v ok=%v err=%v", committed, ok, err)
	}
}

func TestOpenRefusesCorruptAndFutureWithoutReplacement(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(privateTempDir(t), "daemon.db")
		original := []byte("not-a-sqlite-database")
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Open(t.Context(), Options{Path: path}); err == nil {
			t.Fatal("corrupt database opened")
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal("database was removed")
		}
		if !os.SameFile(before, after) {
			t.Fatal("database was replaced")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, original) {
			t.Fatal("corrupt database bytes changed")
		}
	})
	t.Run("future", func(t *testing.T) {
		s, path := openTestStore(t)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		db := rawDB(t, path)
		if _, err := db.Exec(`PRAGMA user_version=99`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		before, _ := os.Stat(path)
		_, err := Open(t.Context(), Options{Path: path})
		if err == nil || !strings.Contains(err.Error(), "future schema version") {
			t.Fatalf("future open error=%v", err)
		}
		after, e := os.Stat(path)
		if e != nil || !os.SameFile(before, after) {
			t.Fatal("future database was removed or replaced")
		}
	})
}

func TestMigrationChecksumDriftAndFailureRefuse(t *testing.T) {
	t.Run("checksum", func(t *testing.T) {
		s, path := openTestStore(t)
		s.Close()
		db := rawDB(t, path)
		if _, err := db.Exec(`DROP TRIGGER schema_migrations_no_update`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE schema_migrations SET checksum='drift' WHERE version=1`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		if _, err := Open(t.Context(), Options{Path: path}); err == nil || !strings.Contains(err.Error(), "checksum drift") {
			t.Fatalf("checksum open error=%v", err)
		}
	})
	t.Run("transactional failure", func(t *testing.T) {
		s, path := openTestStore(t)
		s.Close()
		db := rawDB(t, path)
		defer db.Close()
		plan := append([]migration(nil), migrations...)
		plan = append(plan, migration{version: len(plan) + 1, name: "failing", statements: []string{`CREATE TABLE migration_probe(id INTEGER) STRICT`, `this is not sql`}})
		if _, err := migrate(t.Context(), db, plan, time.Now().UTC()); err == nil {
			t.Fatal("failing migration succeeded")
		}
		var version int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
			t.Fatalf("version=%d err=%v", version, err)
		}
		var probe int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='migration_probe'`).Scan(&probe); err != nil || probe != 0 {
			t.Fatalf("partial migration survived count=%d err=%v", probe, err)
		}
		db.Close()
		if _, err := Open(t.Context(), Options{Path: path}); err != nil {
			t.Fatalf("canonical reopen after failed delta: %v", err)
		}
	})
}

func TestStateCASObservationsAndAppendOnly(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := t.Context()
	created, err := s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "market", Kind: "regime.current", JSON: []byte(`{"stage":"calm"}`)})
	if err != nil || created.Revision != 1 {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	updated, err := s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "market", Kind: "regime.current", ExpectedRevision: 1, JSON: []byte(`{"stage":"watch"}`)})
	if err != nil || updated.Revision != 2 {
		t.Fatalf("update=%+v err=%v", updated, err)
	}
	if _, err := s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "market", Kind: "regime.current", ExpectedRevision: 1, JSON: []byte(`{}`)}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("CAS error=%v", err)
	}
	payload := []byte{0, 1, 2, 0xff}
	metadata := []byte(`{"quality":"delayed"}`)
	at := time.Now().UTC()
	receipt, err := s.AppendObservation(ctx, ObservationInput{ScopeKey: "market", Source: "gateway", Kind: "quote", ObservedAt: at, ContentType: "application/octet-stream", Payload: payload, MetadataJSON: metadata})
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := s.LatestObservation(ctx, "market", "gateway", "quote")
	if err != nil || !ok {
		t.Fatalf("latest ok=%v err=%v", ok, err)
	}
	if latest.ID != receipt.ID || latest.DecisionEligible || !bytes.Equal(latest.Payload, payload) || !bytes.Equal(latest.MetadataJSON, metadata) {
		t.Fatal("lossless observation did not round trip")
	}
	if _, ok, err := s.LatestDecisionEligibleObservation(ctx, "market", "gateway", "quote"); err != nil || ok {
		t.Fatalf("research-only observation crossed eligible reader: ok=%v err=%v", ok, err)
	}
	eligibleReceipt, err := s.AppendObservation(ctx, ObservationInput{
		ScopeKey: "market", Source: "gateway", Kind: "quote", ObservedAt: at.Add(time.Second),
		ContentType: "application/json", Payload: []byte(`{"eligible":true}`), DecisionEligible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	eligible, ok, err := s.LatestDecisionEligibleObservation(ctx, "market", "gateway", "quote")
	if err != nil || !ok || eligible.ID != eligibleReceipt.ID || !eligible.DecisionEligible {
		t.Fatalf("eligible observation = %+v ok=%v err=%v", eligible, ok, err)
	}
	exact, ok, err := s.ExactDecisionEligibleObservation(ctx, eligibleReceipt.ID, "market", "gateway", "quote", at.Add(time.Second))
	if err != nil || !ok || exact.ID != eligibleReceipt.ID || !exact.DecisionEligible || !bytes.Equal(exact.Payload, []byte(`{"eligible":true}`)) {
		t.Fatal("exact decision-eligible observation did not round trip")
	}
	for _, mismatch := range []struct {
		id       int64
		scope    string
		source   string
		kind     string
		observed time.Time
	}{
		{id: receipt.ID, scope: "market", source: "gateway", kind: "quote", observed: at},
		{id: eligibleReceipt.ID + 1, scope: "market", source: "gateway", kind: "quote", observed: at.Add(time.Second)},
		{id: eligibleReceipt.ID, scope: "other", source: "gateway", kind: "quote", observed: at.Add(time.Second)},
		{id: eligibleReceipt.ID, scope: "market", source: "other", kind: "quote", observed: at.Add(time.Second)},
		{id: eligibleReceipt.ID, scope: "market", source: "gateway", kind: "other", observed: at.Add(time.Second)},
		{id: eligibleReceipt.ID, scope: "market", source: "gateway", kind: "quote", observed: at.Add(2 * time.Second)},
	} {
		if _, ok, err := s.ExactDecisionEligibleObservation(ctx, mismatch.id, mismatch.scope, mismatch.source, mismatch.kind, mismatch.observed); err != nil || ok {
			t.Fatal("exact decision-eligible reader accepted mismatched coordinates")
		}
	}
	falseValue := false
	research, err := s.ListObservations(ctx, ObservationQuery{ScopeKey: "market", DecisionEligible: &falseValue, Limit: 10})
	if err != nil || len(research) != 1 || research[0].ID != receipt.ID {
		t.Fatalf("research-only filter = %+v err=%v", research, err)
	}
	before := countRows(t, s, "observations")
	_, _, err = s.CompareAndSwapStateDocumentWithObservations(ctx, StateDocumentCAS{ScopeKey: "market", Kind: "regime.current", ExpectedRevision: 1, JSON: []byte(`{}`)}, []ObservationInput{{ScopeKey: "market", Source: "gateway", Kind: "quote", ObservedAt: at, ContentType: "application/json", Payload: []byte(`{}`)}})
	if !errors.Is(err, ErrRevisionConflict) || countRows(t, s, "observations") != before {
		t.Fatalf("atomic CAS rollback err=%v", err)
	}
	for _, stmt := range []string{`UPDATE observations SET kind='changed' WHERE observation_id=1`, `DELETE FROM observations WHERE observation_id=1`} {
		if _, err := s.db.Exec(stmt); err == nil {
			t.Fatalf("append-only statement succeeded: %s", stmt)
		}
	}
	for _, table := range appendOnlyTables {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name IN (?,?)`, table+"_no_update", table+"_no_delete").Scan(&n); err != nil || n != 2 {
			t.Errorf("append-only triggers %s=%d err=%v", table, n, err)
		}
	}
}

func TestReceiptBoundStateCASCommitsOrRollsBackAsOneMutation(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := t.Context()
	created, err := s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{
		ScopeKey: "market", Kind: "receipt-bound.current", JSON: []byte(`{"version":1}`),
	})
	if err != nil || created.Revision != 1 {
		t.Fatal("receipt-bound state fixture creation failed")
	}
	at := time.Now().UTC()
	payload := []byte(`{"typed":true}`)
	expectedDigest := sha256.Sum256(payload)
	input := ObservationInput{
		ScopeKey: "market", Source: "gateway", Kind: "typed-proof", ObservedAt: at,
		ContentType: "application/json", Payload: payload, DecisionEligible: true,
	}
	updated, receipts, err := s.CompareAndSwapStateDocumentWithBoundObservations(ctx, StateDocumentCAS{
		ScopeKey: "market", Kind: "receipt-bound.current", ExpectedRevision: created.Revision,
	}, []ObservationInput{input}, func(nextRevision int64, receipts []ObservationReceipt) ([]byte, error) {
		if nextRevision != 2 || len(receipts) != 1 || receipts[0].ID <= 0 || receipts[0].PayloadSHA256 != expectedDigest {
			return nil, errors.New("builder did not receive the exact receipt binding")
		}
		return fmt.Appendf(nil, `{"revision":%d,"observation_id":%d,"digest":"%x"}`,
			nextRevision, receipts[0].ID, receipts[0].PayloadSHA256), nil
	})
	if err != nil || updated.Revision != 2 || len(receipts) != 1 {
		t.Fatal("receipt-bound state and observation did not commit together")
	}
	if !bytes.Contains(updated.JSON, fmt.Appendf(nil, `"observation_id":%d`, receipts[0].ID)) ||
		!bytes.Contains(updated.JSON, fmt.Appendf(nil, `"digest":"%x"`, expectedDigest)) {
		t.Fatal("committed state did not bind the exact receipt")
	}
	exact, ok, err := s.ExactDecisionEligibleObservation(ctx, receipts[0].ID, input.ScopeKey, input.Source, input.Kind, input.ObservedAt)
	if err != nil || !ok || exact.PayloadSHA256 != expectedDigest {
		t.Fatal("bound observation was not committed with state")
	}

	before := countRows(t, s, "observations")
	_, _, err = s.CompareAndSwapStateDocumentWithBoundObservations(ctx, StateDocumentCAS{
		ScopeKey: "market", Kind: "receipt-bound.current", ExpectedRevision: 1,
	}, []ObservationInput{input}, func(int64, []ObservationReceipt) ([]byte, error) {
		return []byte(`{"must_not_commit":true}`), nil
	})
	if !errors.Is(err, ErrRevisionConflict) || countRows(t, s, "observations") != before {
		t.Fatal("stale receipt-bound CAS did not roll its observation back")
	}

	_, _, err = s.CompareAndSwapStateDocumentWithBoundObservations(ctx, StateDocumentCAS{
		ScopeKey: "market", Kind: "receipt-bound.current", ExpectedRevision: updated.Revision,
	}, []ObservationInput{input}, func(int64, []ObservationReceipt) ([]byte, error) {
		return nil, errors.New("synthetic builder failure")
	})
	if err == nil || countRows(t, s, "observations") != before {
		t.Fatal("builder failure did not roll its observation back")
	}
	retained, ok, err := s.GetStateDocument(ctx, "market", "receipt-bound.current")
	if err != nil || !ok || retained.Revision != updated.Revision || !bytes.Equal(retained.JSON, updated.JSON) {
		t.Fatal("failed receipt-bound mutation changed current state")
	}
}

func TestStateCASCommitClockFloorRejectsBeforeMutation(t *testing.T) {
	s, _ := openTestStore(t)
	floor := time.Now().UTC().Add(time.Hour)
	_, err := s.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{
		ScopeKey: "market", Kind: "clock-floor", JSON: []byte(`{"ok":true}`), UpdatedAtNotBefore: floor,
	})
	if !errors.Is(err, ErrRollback) {
		t.Fatalf("clock-floor error=%v, want ErrRollback", err)
	}
	if _, ok, readErr := s.GetStateDocument(t.Context(), "market", "clock-floor"); readErr != nil || ok {
		t.Fatalf("clock-floor mutation survived: ok=%v err=%v", ok, readErr)
	}
	if health := s.Health(); !health.Ready {
		t.Fatalf("expected clock floor must not poison SQLite health: %+v", health)
	}
}

func TestPreviewTokenSingleWinnerAndMonotonicFloors(t *testing.T) {
	s, path := openTestStore(t)
	ctx := context.Background()
	scope := testScope("paper-primary")
	s2, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	head, _ := s.AuthorityHead(ctx)
	tokenID := "preview-concurrency"
	digest := HashPreviewTokenID(tokenID)
	const workers = 24
	var wins atomic.Int64
	var consumed atomic.Int64
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			event := orderEvent(scope, fmt.Sprintf("attempt-%d", i), tokenID, int64(100+i))
			store := s
			if i%2 == 1 {
				store = s2
			}
			_, err := store.StagePreTransmit(ctx, PreTransmitRequest{Scope: scope, TokenDigest: digest, AuthorityEpoch: head.AuthorityEpoch, SignerGeneration: head.SignerGeneration, RequestedOrderIDFloor: int64(100 + i), ReservedOrderID: int64(100 + i), Action: ActionPlace, Origin: OriginAgentCLI, Events: []OrderEventRecord{event}})
			if err == nil {
				wins.Add(1)
			} else if errors.Is(err, ErrPreviewTokenConsumed) {
				consumed.Add(1)
			} else {
				t.Errorf("unexpected stage error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || consumed.Load() != workers-1 {
		t.Fatalf("wins=%d consumed=%d", wins.Load(), consumed.Load())
	}
	if countRows(t, s, "consumed_preview_tokens") != 1 || countRows(t, s, "order_events") != 1 {
		t.Fatal("single winner did not produce exactly one tombstone/event")
	}
	floor1, err := s.GlobalOrderIDFloor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rowsBefore := countRows(t, s, "order_events")
	badTokenID := "floor-reuse"
	badEvent := orderEvent(scope, "floor-reuse-event", badTokenID, floor1)
	_, err = s.StagePreTransmit(ctx, PreTransmitRequest{Scope: scope, TokenDigest: HashPreviewTokenID(badTokenID), AuthorityEpoch: head.AuthorityEpoch, SignerGeneration: head.SignerGeneration, RequestedOrderIDFloor: floor1, ReservedOrderID: floor1, Action: ActionPlace, Origin: OriginAgentCLI, Events: []OrderEventRecord{badEvent}})
	if !errors.Is(err, ErrOrderIDFloor) {
		t.Fatalf("reused placement floor error=%v", err)
	}
	if countRows(t, s, "order_events") != rowsBefore || countRows(t, s, "consumed_preview_tokens") != 1 {
		t.Fatal("failed floor check committed token or event")
	}
	stageWithToken(t, s, scope, "floor-low", "floor-low-event", floor1-1)
	floor2, _ := s.GlobalOrderIDFloor(ctx)
	if floor2 != floor1 {
		t.Fatalf("floor decreased %d -> %d", floor1, floor2)
	}
	stageWithToken(t, s, scope, "floor-high", "floor-high-event", floor1+100)
	floor3, _ := s.GlobalOrderIDFloor(ctx)
	scoped, _ := s.ScopedOrderIDFloor(ctx, scope.ScopeKey)
	if floor3 != floor1+100 || scoped != floor3 {
		t.Fatalf("floors global=%d scoped=%d", floor3, scoped)
	}
	other := scope
	other.Account = "OTHER"
	event := orderEvent(other, "collision", "", floor3)
	_, err = s.StagePreTransmit(ctx, PreTransmitRequest{Scope: other, RequestedOrderIDFloor: floor3, Action: ActionPlace, Origin: OriginAgentCLI, Events: []OrderEventRecord{event}})
	if !errors.Is(err, ErrBrokerScopeCollision) {
		t.Fatalf("scope rebind error=%v", err)
	}
	alias := scope
	alias.ScopeKey = "alias"
	event = orderEvent(alias, "alias-event", "", floor3)
	_, err = s.StagePreTransmit(ctx, PreTransmitRequest{Scope: alias, RequestedOrderIDFloor: floor3, Action: ActionPlace, Origin: OriginAgentCLI, Events: []OrderEventRecord{event}})
	if !errors.Is(err, ErrBrokerScopeCollision) {
		t.Fatalf("binding alias error=%v", err)
	}
	loaded, err := s.LoadOrderEvents(ctx, OrderQuery{ScopeKey: scope.ScopeKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 {
		t.Fatalf("loaded events=%d want 3", len(loaded))
	}
	for i := 1; i < len(loaded); i++ {
		if loaded[i].EventSeq <= loaded[i-1].EventSeq {
			t.Fatal("events not in event_seq order")
		}
	}
}

func TestIntegrityForeignKeyRefusalAndHealthLatch(t *testing.T) {
	t.Run("foreign key", func(t *testing.T) {
		s, path := openTestStore(t)
		s.Close()
		db := rawDB(t, path)
		if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO regime_decisions(event_seq,scope_key,decision_key,stage) VALUES(999,'market','bad','bad')`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		if _, err := Open(t.Context(), Options{Path: path}); err == nil || !strings.Contains(err.Error(), "integrity failed") {
			t.Fatalf("foreign key open err=%v", err)
		}
	})
	t.Run("busy latch", func(t *testing.T) {
		path := filepath.Join(privateTempDir(t), "daemon.db")
		s, err := Open(t.Context(), Options{Path: path, BusyTimeout: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		locker := rawDB(t, path)
		defer locker.Close()
		if _, err := locker.Exec(`BEGIN IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
		_, err = s.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "x", Kind: "y", JSON: []byte(`{}`)})
		if err == nil {
			t.Fatal("busy mutation succeeded")
		}
		health := s.Health()
		if health.Ready || health.Code != "busy" {
			t.Fatalf("health=%+v", health)
		}
		_, _ = locker.Exec(`ROLLBACK`)
		if _, err := s.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "x", Kind: "y", JSON: []byte(`{}`)}); !errors.Is(err, ErrBlocked) {
			t.Fatalf("blocked latch error=%v", err)
		}
	})
}

func TestVerifiedBackupReopensAndRejectsRollback(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := t.Context()
	doc, err := s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "market", Kind: "current", JSON: []byte(`{"ok":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(privateTempDir(t), "daemon-backup.db")
	info, err := s.Backup(ctx, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Integrity.OK() {
		t.Fatal("backup integrity not verified")
	}
	mode, _ := os.Stat(backupPath)
	if mode.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode=%o", mode.Mode().Perm())
	}
	verified, err := VerifyBackup(ctx, backupPath, info.Head)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Head != info.Head {
		t.Fatal("backup head changed between verification reads")
	}
	copyStore, err := Open(ctx, Options{Path: backupPath, MinimumHead: &info.Head})
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := copyStore.GetStateDocument(ctx, "market", "current")
	copyStore.Close()
	if err != nil || !ok || loaded.Revision != doc.Revision {
		t.Fatalf("backup state ok=%v doc=%+v err=%v", ok, loaded, err)
	}
	if _, err := s.Backup(ctx, backupPath); err == nil {
		t.Fatal("backup overwrote existing destination")
	}
	newHeadDoc, err := s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "market", Kind: "current", ExpectedRevision: 1, JSON: []byte(`{"ok":false}`)})
	if err != nil || newHeadDoc.Revision != 2 {
		t.Fatal(err)
	}
	newHead, _ := s.AuthorityHead(ctx)
	if _, err := VerifyBackup(ctx, backupPath, newHead); !errors.Is(err, ErrRollback) {
		t.Fatalf("old backup verify error=%v", err)
	}
	checkpoint, err := s.Checkpoint(ctx)
	if err != nil || checkpoint.Busy != 0 {
		t.Fatalf("checkpoint=%+v err=%v", checkpoint, err)
	}
}

func TestLegacyOrderAuthorityImportIsOneEpochAndLossless(t *testing.T) {
	s, _ := openTestStore(t)
	scope := testScope("legacy-paper")
	tokenID := "legacy-preview-id"
	input := LegacyOrderImport{SourceFingerprint: "source-a", GlobalFloor: 70, ScopedFloors: []LegacyOrderFloor{{Scope: scope, Floor: 60}}, ConsumedTokens: []LegacyConsumedToken{{Scope: scope, PreviewTokenID: tokenID, ConsumedAt: time.Unix(1_700_000_000, 0).UTC()}}, Events: []OrderEventRecord{orderEvent(scope, "source-a:17", tokenID, 60)}}
	result, err := s.ImportLegacyOrderAuthority(t.Context(), input)
	if err != nil || !result.Imported {
		t.Fatalf("import=%+v err=%v", result, err)
	}
	if got, _ := s.GlobalOrderIDFloor(t.Context()); got != 70 {
		t.Fatalf("global floor=%d want 70", got)
	}
	if countRows(t, s, "consumed_preview_tokens") != 1 || countRows(t, s, "order_events") != 1 {
		t.Fatal("legacy authority rows missing")
	}
	retry, err := s.ImportLegacyOrderAuthority(t.Context(), input)
	if err != nil || retry.Imported {
		t.Fatalf("idempotent retry=%+v err=%v", retry, err)
	}
	changed := input
	changed.SourceFingerprint = "source-b"
	if _, err := s.ImportLegacyOrderAuthority(t.Context(), changed); !errors.Is(err, ErrLegacyImportConflict) {
		t.Fatalf("changed-source import error=%v", err)
	}
	if countRows(t, s, "consumed_preview_tokens") != 1 || countRows(t, s, "order_events") != 1 {
		t.Fatal("conflicting import changed authority rows")
	}
}

func TestTypedEventsAndStatementProjection(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := t.Context()
	value := 12.5
	receipts, err := s.AppendEvents(ctx, []EventInput{{ScopeKey: "market", EventKey: "regime:1", Type: "regime.decision", Action: "observe", Origin: "daemon", OccurredAt: time.Now().UTC(), PayloadJSON: []byte(`{"stage":"watch"}`), Projection: EventProjection{RegimeDecision: &RegimeDecisionProjection{DecisionKey: "d1", Stage: "watch", Indicators: []RegimeIndicatorProjection{{Indicator: "breadth", Value: &value}}}}}})
	if err != nil || len(receipts) != 1 {
		t.Fatalf("append typed event receipts=%+v err=%v", receipts, err)
	}
	loaded, err := s.LoadEvents(ctx, EventQuery{ScopeKey: "market", Type: "regime.decision"})
	if err != nil || len(loaded) != 1 || loaded[0].EventSeq != receipts[0].EventSeq {
		t.Fatalf("load typed events=%+v err=%v", loaded, err)
	}
	if _, err := s.db.Exec(`UPDATE regime_decisions SET stage='changed'`); err == nil {
		t.Fatal("typed projection update succeeded")
	}

	digest := sha256.Sum256([]byte("statement-a"))
	generated := time.Unix(1_700_000_000, 0).UTC()
	file := StatementFileRecord{FileKey: "statement-a.xml", SizeBytes: 11, SHA256: digest, Status: "ingested", StatementGeneratedAt: &generated}
	day := StatementEquityDayRecord{AccountKey: "account-key", Day: "2026-07-20", EquityBaseText: "100.00", StatementFileKey: file.FileKey, GeneratedAt: generated, RawJSON: []byte(`{"equity":"100.00"}`)}
	if err := s.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{file}, []StatementEquityDayRecord{day}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{file}, []StatementEquityDayRecord{day}); err != nil {
		t.Fatalf("idempotent statement projection: %v", err)
	}
	files, err := s.LoadStatementFiles(ctx, "statements")
	if err != nil || len(files) != 1 || files[0].SHA256 != digest {
		t.Fatalf("statement files=%+v err=%v", files, err)
	}
	days, err := s.LoadStatementEquityDays(ctx, "statements", "2026-07-01", "2026-07-31", 10)
	if err != nil || len(days) != 1 || !bytes.Equal(days[0].RawJSON, day.RawJSON) {
		t.Fatalf("statement days=%+v err=%v", days, err)
	}
	changed := file
	changed.SHA256 = sha256.Sum256([]byte("statement-b"))
	changedDay := day
	changedDay.EquityBaseText = "90.00"
	changedDay.RawJSON = []byte(`{"equity":"90.00"}`)
	if err := s.ReplaceStatementProjection(ctx, "statements", []StatementFileRecord{changed}, []StatementEquityDayRecord{changedDay}); err != nil {
		t.Fatalf("restatement: %v", err)
	}
	files, _ = s.LoadStatementFiles(ctx, "statements")
	days, _ = s.LoadStatementEquityDays(ctx, "statements", "", "", 10)
	if len(files) != 1 || files[0].SHA256 != changed.SHA256 || len(days) != 1 || days[0].EquityBaseText != "90.00" || days[0].StatementFileSHA256 != changed.SHA256 {
		t.Fatalf("current restatement files=%+v days=%+v", files, days)
	}
	if countRows(t, s, "statement_file_versions") != 2 || countRows(t, s, "statement_equity_day_versions") != 2 {
		t.Fatal("immutable statement versions were not retained")
	}
	if err := s.ReplaceStatementProjection(ctx, "statements", nil, nil); err != nil {
		t.Fatalf("empty current projection: %v", err)
	}
	files, _ = s.LoadStatementFiles(ctx, "statements")
	days, _ = s.LoadStatementEquityDays(ctx, "statements", "", "", 10)
	if len(files) != 0 || len(days) != 0 {
		t.Fatalf("removed current projection survived files=%d days=%d", len(files), len(days))
	}
	if countRows(t, s, "statement_file_versions") != 2 || countRows(t, s, "statement_equity_day_versions") != 2 {
		t.Fatal("removing current projection deleted immutable evidence")
	}
}

func stageWithToken(t *testing.T, s *Store, scope BrokerScope, tokenID, eventKey string, floor int64) {
	t.Helper()
	head, err := s.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	event := orderEvent(scope, eventKey, tokenID, floor)
	action := ActionPlace
	current, floorErr := s.GlobalOrderIDFloor(t.Context())
	if floorErr != nil {
		t.Fatal(floorErr)
	}
	if floor <= current {
		action = ActionModify
		event.Action = action
	}
	_, err = s.StagePreTransmit(t.Context(), PreTransmitRequest{Scope: scope, TokenDigest: HashPreviewTokenID(tokenID), AuthorityEpoch: head.AuthorityEpoch, SignerGeneration: head.SignerGeneration, RequestedOrderIDFloor: floor, ReservedOrderID: floor, Action: action, Origin: OriginAgentCLI, Events: []OrderEventRecord{event}})
	if err != nil {
		t.Fatal(err)
	}
}

// Open skips its post-migration integrity pass when migrate applied nothing,
// which is every ordinary restart. That skip is only sound while the count is
// exact, so pin both ends of it.
func TestMigrateReportsAppliedCount(t *testing.T) {
	plan := append([]migration(nil), migrations...)

	fresh, err := sql.Open("sqlite", sqliteDSN(filepath.Join(privateTempDir(t), "fresh.db"), defaultBusyTimeout, false))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	fresh.SetMaxOpenConns(1)
	applied, err := migrate(t.Context(), fresh, plan, time.Now().UTC())
	if err != nil {
		t.Fatalf("migrate a fresh authority: %v", err)
	}
	if applied != len(plan) {
		t.Fatalf("fresh authority applied %d migrations, want %d", applied, len(plan))
	}

	s, path := openTestStore(t)
	s.Close()
	current := rawDB(t, path)
	defer current.Close()
	applied, err = migrate(t.Context(), current, plan, time.Now().UTC())
	if err != nil {
		t.Fatalf("migrate a current authority: %v", err)
	}
	if applied != 0 {
		t.Fatalf("current authority applied %d migrations, want 0", applied)
	}
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}
func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
