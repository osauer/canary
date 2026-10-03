package corestore

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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

func TestHeadReadTimeoutLatchSharesTheRecoveryProof(t *testing.T) {
	f := openRecoveryFixture(t)
	if err := f.mutate(t, "warm"); err != nil {
		t.Fatal(err)
	}
	observed := f.head(t)
	realRead := f.store.readHead
	f.store.readHead = func(context.Context) (AuthorityHead, error) { return AuthorityHead{}, context.DeadlineExceeded }
	err := f.mutate(t, "latch")
	if err == nil || !strings.Contains(err.Error(), "read committed authority head") {
		t.Fatalf("timeout error=%v", err)
	}
	// The committed head is unknown here, so the floor is the last head whose
	// watermark was proven before this mutation.
	requireEligibleHeadWatermarkLatch(t, f.store, observed)
	seen := f.recorder.transitions(t, 1)
	if seen[0].cause != err {
		t.Fatalf("block transition cause=%v, want %v", seen[0].cause, err)
	}
	f.store.readHead = realRead
	if recovered, err := f.store.RecoverTransientHeadWatermark(t.Context()); !recovered || err != nil {
		t.Fatalf("timeout recovery=(%v,%v)", recovered, err)
	}
	if err := f.mutate(t, "after"); err != nil {
		t.Fatalf("mutation after recovery: %v", err)
	}
	seen = f.recorder.transitions(t, 2)
	if !seen[1].health.Ready || seen[1].cause != nil {
		t.Fatalf("recovery transition=%+v", seen[1])
	}
}
