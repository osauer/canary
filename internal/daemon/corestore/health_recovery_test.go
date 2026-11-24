package corestore

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type healthTransition struct {
	health Health
	cause  error
}

// healthRecorder is a HealthObserver that records every transition and proves
// the store delivers it outside writeMu and healthMu.
type healthRecorder struct {
	t     *testing.T
	store atomic.Pointer[Store]
	mu    sync.Mutex
	seen  []healthTransition
}

func (r *healthRecorder) observe(health Health, cause error) {
	store := r.store.Load()
	if store == nil {
		r.t.Error("health observer fired before the store was published")
		return
	}
	if store.writeMu.TryLock() {
		store.writeMu.Unlock()
	} else {
		r.t.Error("health observer invoked while writeMu is held")
	}
	if store.healthMu.TryLock() {
		store.healthMu.Unlock()
	} else {
		r.t.Error("health observer invoked while healthMu is held")
	}
	if got := store.Health(); got != health {
		r.t.Errorf("observer health=%+v, store health=%+v", health, got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, healthTransition{health: health, cause: cause})
}

func (r *healthRecorder) transitions(t *testing.T, want int) []healthTransition {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) != want {
		t.Fatalf("health transitions=%d want %d: %+v", len(r.seen), want, r.seen)
	}
	return append([]healthTransition(nil), r.seen...)
}

type recoveryFixture struct {
	store     *Store
	recorder  *healthRecorder
	remaining atomic.Int64 // consecutive CommitObserver failures still to inject
}

func openRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	f := &recoveryFixture{recorder: &healthRecorder{t: t}}
	store, err := Open(t.Context(), Options{
		Path: filepath.Join(privateTempDir(t), "daemon.db"),
		CommitObserver: func(AuthorityHead) error {
			if f.remaining.Load() > 0 {
				f.remaining.Add(-1)
				return errors.New("watermark unavailable")
			}
			return nil
		},
		HealthObserver: f.recorder.observe,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	f.recorder.store.Store(store)
	return f
}

func (f *recoveryFixture) mutate(t *testing.T, kind string) error {
	t.Helper()
	_, err := f.store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{
		ScopeKey: "test", Kind: kind, JSON: []byte(`{}`),
	})
	return err
}

func (f *recoveryFixture) head(t *testing.T) AuthorityHead {
	t.Helper()
	head, err := f.store.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// latchByObserverFailure commits one mutation whose watermark persistence
// fails and returns the committed head together with the mutation error.
func (f *recoveryFixture) latchByObserverFailure(t *testing.T, failures int64) (AuthorityHead, error) {
	t.Helper()
	if err := f.mutate(t, "warm"); err != nil {
		t.Fatal(err)
	}
	before := f.head(t)
	f.remaining.Store(failures)
	err := f.mutate(t, "latch")
	if err == nil || !strings.Contains(err.Error(), "persist committed authority head") {
		t.Fatalf("observer failure error=%v", err)
	}
	committed := f.head(t)
	if committed.HeadGeneration <= before.HeadGeneration {
		t.Fatalf("mutation must be durable despite the observer failure: before=%+v after=%+v", before, committed)
	}
	return committed, err
}

func requireEligibleHeadWatermarkLatch(t *testing.T, store *Store, floor AuthorityHead) {
	t.Helper()
	health := store.Health()
	if health.Ready || health.Code != "head_watermark" || !health.RecoveryEligible || health.BlockedAt.IsZero() {
		t.Fatalf("health=%+v, want an eligible head_watermark latch", health)
	}
	if store.recoveryMinimumHead != floor {
		t.Fatalf("recovery floor=%+v, want %+v", store.recoveryMinimumHead, floor)
	}
}

func TestObserverFailureLatchIsRecoveryEligible(t *testing.T) {
	cases := []struct {
		name          string
		failures      int64
		wantRecovered bool
	}{
		{name: "observer fails once then succeeds", failures: 1, wantRecovered: true},
		{name: "observer keeps failing", failures: math.MaxInt64, wantRecovered: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := openRecoveryFixture(t)
			committed, mutationErr := f.latchByObserverFailure(t, tc.failures)
			requireEligibleHeadWatermarkLatch(t, f.store, committed)
			if err := f.mutate(t, "blocked"); !errors.Is(err, ErrBlocked) {
				t.Fatalf("mutation while latched=%v, want ErrBlocked", err)
			}
			seen := f.recorder.transitions(t, 1)
			if seen[0].health.Ready || seen[0].cause != mutationErr {
				t.Fatalf("block transition=%+v, want the mutation error %v", seen[0], mutationErr)
			}

			recovered, err := f.store.RecoverTransientHeadWatermark(t.Context())
			if recovered != tc.wantRecovered {
				t.Fatalf("recovered=%v err=%v, want recovered=%v", recovered, err, tc.wantRecovered)
			}
			if !tc.wantRecovered {
				if err == nil || errors.Is(err, ErrRecoveryNotEligible) || !strings.Contains(err.Error(), "persist recovered authority head") {
					t.Fatalf("failed proof error=%v", err)
				}
				requireEligibleHeadWatermarkLatch(t, f.store, committed)
				if err := f.mutate(t, "still-blocked"); !errors.Is(err, ErrBlocked) {
					t.Fatalf("mutation after failed proof=%v, want ErrBlocked", err)
				}
				f.recorder.transitions(t, 1)
				// Once the watermark heals, the unchanged proof reopens writes.
				f.remaining.Store(0)
				if recovered, err := f.store.RecoverTransientHeadWatermark(t.Context()); !recovered || err != nil {
					t.Fatalf("recovery after heal=(%v,%v)", recovered, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if health := f.store.Health(); !health.Ready {
				t.Fatalf("health after recovery=%+v", health)
			}
			if f.store.recoveryMinimumHead != (AuthorityHead{}) || f.store.lastObservedHead != committed {
				t.Fatalf("recovered floor=%+v observed=%+v, want cleared floor and head %+v", f.store.recoveryMinimumHead, f.store.lastObservedHead, committed)
			}
			if err := f.mutate(t, "after"); err != nil {
				t.Fatalf("mutation after recovery: %v", err)
			}
			seen = f.recorder.transitions(t, 2)
			if seen[1].health != (Health{Ready: true}) || seen[1].cause != nil {
				t.Fatalf("recovery transition=%+v, want Ready with nil cause", seen[1])
			}
		})
	}
}

func TestObserverFailureRecoveryRefusesOlderOrForeignHead(t *testing.T) {
	cases := []struct {
		name  string
		drift func(AuthorityHead) AuthorityHead
	}{
		{name: "older head generation", drift: func(h AuthorityHead) AuthorityHead { h.HeadGeneration--; return h }},
		{name: "older event sequence", drift: func(h AuthorityHead) AuthorityHead { h.LastEventSeq--; return h }},
		{name: "different authority epoch", drift: func(h AuthorityHead) AuthorityHead { h.AuthorityEpoch = "foreign"; return h }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := openRecoveryFixture(t)
			committed, _ := f.latchByObserverFailure(t, 1)
			requireEligibleHeadWatermarkLatch(t, f.store, committed)
			// The watermark is available again, but the authority on disk no
			// longer carries the head this process committed.
			f.remaining.Store(0)
			f.store.readHead = func(context.Context) (AuthorityHead, error) { return tc.drift(committed), nil }
			recovered, err := f.store.RecoverTransientHeadWatermark(t.Context())
			if recovered || !errors.Is(err, ErrRollback) {
				t.Fatalf("recovery against a drifted head=(%v,%v), want ErrRollback", recovered, err)
			}
			if health := f.store.Health(); health.Ready || health.Code != "rollback" || health.RecoveryEligible {
				t.Fatalf("health after rollback refusal=%+v", health)
			}
			if recovered, err := f.store.RecoverTransientHeadWatermark(t.Context()); recovered || !errors.Is(err, ErrRecoveryNotEligible) {
				t.Fatalf("second recovery=(%v,%v), want not eligible", recovered, err)
			}
			if err := f.mutate(t, "blocked"); !errors.Is(err, ErrBlocked) {
				t.Fatalf("mutation after refusal=%v, want ErrBlocked", err)
			}
			f.recorder.transitions(t, 1)
		})
	}
}

// A mutation whose commit lands while a reader waits for the store's one
// connection stays Ready. The head used to be read after the commit through
// the pool, which handed the released connection to the waiting reader first;
// under load that wait passed the busy timeout and latched writes for a commit
// that was durable (2026-11-24 11:28 CET, 22m48s).
func TestACommitStaysReadyWhileAReaderWaitsForTheConnection(t *testing.T) {
	var observed []AuthorityHead
	store, err := Open(t.Context(), Options{
		Path:        filepath.Join(privateTempDir(t), "daemon.db"),
		BusyTimeout: 50 * time.Millisecond,
		CommitObserver: func(head AuthorityHead) error {
			observed = append(observed, head)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	release := make(chan struct{})
	readerDone := make(chan error, 1)
	var committed AuthorityHead
	err = store.criticalMutation(t.Context(), func(tx *sql.Tx) error {
		waits := store.db.Stats().WaitCount
		go func() {
			// A reader that does not take writeMu queues for the connection
			// and, once it has it, holds it past the busy timeout.
			conn, err := store.db.Conn(context.Background())
			if err == nil {
				<-release
				err = conn.Close()
			}
			readerDone <- err
		}()
		for store.db.Stats().WaitCount == waits {
			runtime.Gosched()
		}
		var err error
		committed, err = advanceHeadTx(t.Context(), tx, 0, time.Now())
		return err
	})
	close(release)
	if readerErr := <-readerDone; readerErr != nil {
		t.Fatal(readerErr)
	}
	if err != nil {
		t.Fatalf("mutation with a waiting reader = %v, want nil", err)
	}
	if health := store.Health(); !health.Ready {
		t.Fatalf("health after a durable commit = %+v, want Ready", health)
	}
	if len(observed) != 1 || observed[0] != committed || store.lastObservedHead != committed {
		t.Fatalf("observed=%+v lastObserved=%+v, want the committed head %+v once", observed, store.lastObservedHead, committed)
	}
}

// A commit that fails publishes nothing: the head read inside the transaction
// never reaches the watermark, the store keeps the last head it saw, and the
// database keeps the old head. A watermark ahead of the database would read as
// a rollback at the next open.
func TestAFailedCommitPublishesNoHead(t *testing.T) {
	var observed []AuthorityHead
	store, err := Open(t.Context(), Options{
		Path: filepath.Join(privateTempDir(t), "daemon.db"),
		CommitObserver: func(head AuthorityHead) error {
			observed = append(observed, head)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	before, err := store.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// A deferred foreign key is checked by COMMIT itself, so the statements
	// all succeed and the commit is what fails.
	statementsDone := false
	err = store.criticalMutation(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `PRAGMA defer_foreign_keys=ON`); err != nil {
			return err
		}
		if _, err := advanceHeadTx(t.Context(), tx, 0, time.Now()); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `INSERT INTO consumed_preview_tokens
(token_digest, scope_key, authority_epoch, signer_generation, head_generation, consumed_at)
VALUES (?, 'no-such-scope', 'epoch', 1, 1, 'now')`, make([]byte, 32))
		statementsDone = err == nil
		return err
	})
	if !statementsDone {
		t.Fatalf("the mutation failed before its commit: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("commit with a dangling deferred key = %v, want the commit's foreign key failure", err)
	}
	if len(observed) != 0 || store.lastObservedHead != before {
		t.Fatalf("a failed commit published observed=%+v lastObserved=%+v, want nothing past %+v", observed, store.lastObservedHead, before)
	}
	after, err := store.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("database head after a failed commit = %+v, want %+v", after, before)
	}
	if health := store.Health(); !health.Ready {
		t.Fatalf("health after a refused commit = %+v, want Ready", health)
	}
	var next AuthorityHead
	if err := store.criticalMutation(t.Context(), func(tx *sql.Tx) error {
		var err error
		next, err = advanceHeadTx(t.Context(), tx, 0, time.Now())
		return err
	}); err != nil {
		t.Fatalf("mutation after a refused commit = %v", err)
	}
	if len(observed) != 1 || observed[0] != next || next.HeadGeneration != before.HeadGeneration+1 {
		t.Fatalf("observed=%+v after the next commit %+v, want that head once", observed, next)
	}
}

// A permanent latch that arrives while the store waits in the recoverable
// head-watermark latch replaces it. It used to be dropped, because a latch
// only moved a Ready store, and the watermark proof then reopened writes over
// an outside opener the daemon had seen (found 2026-11-14).
func TestAPermanentLatchReplacesTheRecoverableOne(t *testing.T) {
	f := openRecoveryFixture(t)
	committed, _ := f.latchByObserverFailure(t, 1)
	requireEligibleHeadWatermarkLatch(t, f.store, committed)

	f.store.LatchDamaged("outside_opener", errors.New("daemon.db-shm appeared"))
	if h := f.store.Health(); h.Ready || h.Code != "outside_opener" || h.RecoveryEligible {
		t.Fatalf("health after the outside opener = %+v, want a permanent outside_opener latch", h)
	}
	seen := f.recorder.transitions(t, 2)
	if seen[1].health.Code != "outside_opener" {
		t.Fatalf("second transition = %+v, want the outside opener announced", seen[1])
	}
	if recovered, err := f.store.RecoverTransientHeadWatermark(t.Context()); recovered {
		t.Fatalf("the watermark proof reopened a permanent latch (err %v)", err)
	}
	if err := f.mutate(t, "after"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("mutation after the outside opener = %v, want ErrBlocked", err)
	}
}
