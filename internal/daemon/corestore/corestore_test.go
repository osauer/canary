package corestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
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
	if store.Health().RecoveryEligible {
		t.Fatal("observer persistence failure must not be eligible for in-process recovery")
	}
	if recovered, err := store.RecoverTransientHeadWatermark(t.Context()); recovered || !errors.Is(err, ErrRecoveryNotEligible) {
		t.Fatalf("observer failure recovery=(%v,%v), want not eligible", recovered, err)
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
		if health.RecoveryEligible {
			t.Fatal("critical busy failure must not be eligible for in-process recovery")
		}
		_, _ = locker.Exec(`ROLLBACK`)
		if recovered, err := s.RecoverTransientHeadWatermark(t.Context()); recovered || !errors.Is(err, ErrRecoveryNotEligible) {
			t.Fatalf("critical busy recovery=(%v,%v), want not eligible", recovered, err)
		}
		if _, err := s.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "x", Kind: "y", JSON: []byte(`{}`)}); !errors.Is(err, ErrBlocked) {
			t.Fatalf("blocked latch error=%v", err)
		}
	})
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

func TestStagePreTransmitModifyCancelAttemptAdmission(t *testing.T) {
	tests := []struct {
		name               string
		outcomeType        string
		outcomeAttemptID   string
		sendDisposition    string
		wantModifyAccepted bool
	}{
		{name: "definitely unsent", outcomeType: "send-error", outcomeAttemptID: "cancel-1", sendDisposition: "definitely_unsent", wantModifyAccepted: true},
		{name: "may have written", outcomeType: "send-error", outcomeAttemptID: "cancel-1", sendDisposition: "may_have_written"},
		{name: "unknown", outcomeType: "send-error", outcomeAttemptID: "cancel-1", sendDisposition: "unknown"},
		{name: "incomplete send error", outcomeType: "send-error", outcomeAttemptID: "cancel-1"},
		{name: "uncorrelated definite send error", outcomeType: "send-error", outcomeAttemptID: "cancel-other", sendDisposition: "definitely_unsent"},
		{name: "send completed", outcomeType: "send-completed", outcomeAttemptID: "cancel-1"},
		{name: "pending"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _ := openTestStore(t)
			scope := testScope("modify-cancel-admission")
			const orderID int64 = 1001

			events := []OrderEventRecord{
				modifyAdmissionEvent(t, scope, orderID, "working", "broker-acknowledged", ActionPlace, "", "", "Submitted"),
				modifyAdmissionEvent(t, scope, orderID, "cancel-requested", "cancel-requested", ActionCancel, "cancel-1", "", ""),
			}
			if test.outcomeType != "" {
				events = append(events, modifyAdmissionEvent(t, scope, orderID, "cancel-outcome", test.outcomeType, ActionCancel, test.outcomeAttemptID, test.sendDisposition, ""))
			}
			seqs, err := store.AppendOrderEvents(t.Context(), events)
			if err != nil {
				t.Fatal(err)
			}
			expected := seqs[len(seqs)-1]
			modify := modifyAdmissionEvent(t, scope, orderID, "modify-requested", "modify-requested", ActionModify, "modify-1", "", "")
			result, err := store.StagePreTransmit(t.Context(), PreTransmitRequest{
				Scope: scope, RequestedOrderIDFloor: orderID, ReservedOrderID: orderID,
				ExpectedOrderEventSeq: &expected, Action: ActionModify, Origin: OriginAgentCLI,
				Events: []OrderEventRecord{modify},
			})

			if test.wantModifyAccepted {
				if err != nil {
					t.Fatalf("modify after definitely-unsent cancel: %v", err)
				}
				if len(result.EventSeqs) != 1 || result.EventSeqs[0] <= expected {
					t.Fatalf("modify result = %+v, prior frontier = %d", result, expected)
				}
				return
			}
			if !errors.Is(err, ErrOrderNotModifiable) {
				t.Fatalf("modify error = %v, want %v", err, ErrOrderNotModifiable)
			}
			var got int64
			if err := store.db.QueryRowContext(t.Context(), `SELECT MAX(event_seq) FROM order_events WHERE scope_key=? AND reserved_order_id=?`, scope.ScopeKey, orderID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != expected {
				t.Fatalf("rejected modify changed frontier: got %d want %d", got, expected)
			}
		})
	}
}

func modifyAdmissionEvent(t *testing.T, scope BrokerScope, orderID int64, eventKey, eventType string, action ActionKind, attemptID, disposition, status string) OrderEventRecord {
	t.Helper()
	payload := map[string]any{
		"version": 1, "type": eventType, "action_kind": action,
	}
	if attemptID != "" {
		payload["attempt_id"] = attemptID
	}
	if disposition != "" {
		payload["send_disposition"] = disposition
	}
	rawJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return OrderEventRecord{
		Scope: scope, EventKey: eventKey, AtMS: time.Now().UnixMilli(), Type: eventType,
		Action: action, Origin: OriginAgentCLI, ReservedOrderID: orderID, Status: status,
		RawJSON: rawJSON,
	}
}

func TestQuiesceForReplacementRecoversCommittedWALAndRejectsUnsafeSidecars(t *testing.T) {
	t.Run("committed WAL", func(t *testing.T) {
		dir := privateTempDir(t)
		livePath := filepath.Join(dir, "live.db")
		crashPath := filepath.Join(dir, "crash.db")
		store, err := Open(t.Context(), Options{Path: livePath})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "x", Kind: "wal", JSON: []byte(`{"committed":true}`)}); err != nil {
			t.Fatal(err)
		}
		head, err := store.AuthorityHead(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		copyTestFile(t, livePath, crashPath)
		copyTestFile(t, livePath+"-wal", crashPath+"-wal")
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		inspection, err := QuiesceForReplacement(t.Context(), QuiesceOptions{Path: crashPath, ExpectedSchemaVersion: len(migrations), ExpectedHead: head})
		if err != nil {
			t.Fatal(err)
		}
		if inspection.Head != head || inspection.SchemaVersion != len(migrations) {
			t.Fatalf("quiesced inspection=%+v", inspection)
		}
		assertNoSQLiteSidecars(t, crashPath)
		if _, err := Inspect(t.Context(), InspectOptions{Path: crashPath, MinimumHead: &head}); err != nil {
			t.Fatalf("reopen quiesced authority: %v", err)
		}
	})

	t.Run("symlink sidecar", func(t *testing.T) {
		dir := privateTempDir(t)
		path := filepath.Join(dir, "daemon.db")
		store, err := Open(t.Context(), Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		head, err := store.AuthorityHead(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "unrelated")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path+"-wal"); err != nil {
			t.Fatal(err)
		}
		if _, err := QuiesceForReplacement(t.Context(), QuiesceOptions{Path: path, ExpectedSchemaVersion: 1, ExpectedHead: head}); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("unsafe sidecar error=%v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "keep" {
			t.Fatalf("symlink target changed: %q err=%v", got, err)
		}
	})
}

func assertNoSQLiteSidecars(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected SQLite sidecar %s error=%v", path+suffix, err)
		}
	}
}

func copyTestFile(t *testing.T, source, destination string) {
	t.Helper()
	payload, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}
