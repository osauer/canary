package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// regimeSnapshotStateKind is versioned in the authoritative key rather than
// inside the document. The document bytes are exactly one complete
// RegimeSnapshotResult with response-only AuthorityHealth removed.
const regimeSnapshotStateKind = "regime_snapshot.current.v1"

var (
	errRegimeSnapshotRefreshIncomplete = errors.New("regime snapshot refresh was incomplete")
	errRegimeSnapshotRefreshMissing    = errors.New("regime snapshot refresh function is required")
	errRegimeSnapshotRefreshSuppressed = errors.New("regime snapshot refresh retry is not due")
	errRegimeSnapshotProjectionPending = errors.New("regime snapshot projection repair is pending")
	errRegimeSnapshotClockInvalid      = errors.New("regime snapshot authority clock is behind its last successful commit")
)

// regimeSnapshotStateStore is deliberately the narrow corestore surface the
// faulting adapter without introducing a second persistence authority.
type regimeSnapshotStateStore interface {
	GetStateDocument(context.Context, string, string) (corestore.StateDocument, bool, error)
	CompareAndSwapStateDocument(context.Context, corestore.StateDocumentCAS) (corestore.StateDocument, error)
}

// regimeSnapshotAfterPublishFunc commits volatile daemon-side state that was
// quality, the rule-stage latch, and decision journals). It runs exactly once,
// and before cold joiners are released. It never runs on an incomplete or
// failed refresh.
type regimeSnapshotPublication struct {
	Revision    int64
	PublishedAt time.Time
	Fingerprint rpc.Fingerprint
}

type regimeSnapshotAfterPublishFunc func(context.Context, regimeSnapshotPublication) error

// regimeSnapshotRefreshFunc acquires and fully composes one candidate regime
// error or unavailable rows. The cache never publishes when Complete is false.
// The optional afterPublish closure is the only safe place to commit staged
// daemon-side evidence that must not get ahead of daemon.db authority.
type regimeSnapshotRefreshFunc func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error)

type regimeSnapshotCacheOptions struct {
	// FreshFor is an operational cache window supplied by the daemon. It is
	FreshFor time.Duration
	// RefreshTimeout is the upper bound for acquisition plus the authoritative
	// never from an observing RPC request.
	RefreshTimeout time.Duration
	// FailureRetryAfter is the minimum quiet period after a failed attempt.
	// failure into sequential fan-outs, and so this cache does not invent a
	FailureRetryAfter time.Duration
	// Now is optional and exists for deterministic tests.
	Now func() time.Time
}

// regimeSnapshotCacheView is an immutable serve projection. Snapshot is a
// AuthorityHealth field. Revision and Fingerprint are useful to daemon-local
type regimeSnapshotCacheView struct {
	Snapshot    *rpc.RegimeSnapshotResult
	Health      rpc.RegimeAuthorityHealth
	Revision    int64
	Fingerprint rpc.Fingerprint
}

// regimeSnapshotCacheUnavailableError distinguishes a cold authority from a
type regimeSnapshotCacheUnavailableError struct {
	cause error
}

func (e *regimeSnapshotCacheUnavailableError) Error() string {
	return "regime snapshot last-good is unavailable"
}

func (e *regimeSnapshotCacheUnavailableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type regimeSnapshotRefresh struct {
	done chan struct{}
	err  error
}

// regimeSnapshotCache owns the sole in-process current-regime authority. Raw
type regimeSnapshotCache struct {
	mu sync.Mutex

	store             regimeSnapshotStateStore
	daemonContext     context.Context
	freshFor          time.Duration
	refreshTimeout    time.Duration
	failureRetryAfter time.Duration
	now               func() time.Time

	raw           []byte
	revision      int64
	lastSuccessAt time.Time
	fingerprint   rpc.Fingerprint
	refresh       *regimeSnapshotRefresh
	failureCode   rpc.RegimeAuthorityFailureCode
	lastAttemptAt time.Time
	// dependencyRefreshPending records a successful publication by a direct
	// only the refresh schedule: the prior immutable Regime publication stays
	dependencyRefreshPending bool

	// projectionPending means the snapshot CAS committed but at least one
	// derived projector or the final projection receipt did not. It blocks a
	// newer market refresh until the daemon repairs that exact revision.
	projectionPending  bool
	projectionRevision int64
}

// loadRegimeSnapshotCache constructs the cache and strictly hydrates the one
// Malformed state is a startup error; this function never consults a legacy
func loadRegimeSnapshotCache(
	startupContext context.Context,
	daemonContext context.Context,
	store regimeSnapshotStateStore,
	options regimeSnapshotCacheOptions,
) (*regimeSnapshotCache, error) {
	if startupContext == nil {
		return nil, errors.New("load regime snapshot cache: startup context is required")
	}
	if daemonContext == nil {
		return nil, errors.New("load regime snapshot cache: daemon context is required")
	}
	if store == nil {
		return nil, errors.New("load regime snapshot cache: daemon SQLite authority is required")
	}
	if options.FreshFor <= 0 {
		return nil, errors.New("load regime snapshot cache: fresh window must be positive")
	}
	if options.RefreshTimeout <= 0 {
		return nil, errors.New("load regime snapshot cache: refresh timeout must be positive")
	}
	if options.FailureRetryAfter <= 0 {
		return nil, errors.New("load regime snapshot cache: failure retry window must be positive")
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	cache := &regimeSnapshotCache{
		store:             store,
		daemonContext:     daemonContext,
		freshFor:          options.FreshFor,
		refreshTimeout:    options.RefreshTimeout,
		failureRetryAfter: options.FailureRetryAfter,
		now:               now,
		failureCode:       rpc.RegimeAuthorityFailureNoLastGood,
	}

	document, ok, err := store.GetStateDocument(startupContext, daemonStateScope, regimeSnapshotStateKind)
	if err != nil {
		return nil, fmt.Errorf("load authoritative regime snapshot: %w", err)
	}
	if !ok {
		return cache, nil
	}
	if document.ScopeKey != daemonStateScope || document.Kind != regimeSnapshotStateKind {
		return nil, errors.New("load authoritative regime snapshot: state-document identity mismatch")
	}
	if document.Revision <= 0 {
		return nil, errors.New("load authoritative regime snapshot: revision must be positive")
	}
	if document.UpdatedAt.IsZero() {
		return nil, errors.New("load authoritative regime snapshot: updated_at is required")
	}
	snapshot, err := decodeRegimeSnapshotDocument(document.JSON)
	if err != nil {
		return nil, fmt.Errorf("load authoritative regime snapshot: %w", err)
	}
	cache.raw = bytes.Clone(document.JSON)
	cache.revision = document.Revision
	cache.lastSuccessAt = document.UpdatedAt.UTC()
	cache.fingerprint = snapshot.Fingerprint
	cache.failureCode = rpc.RegimeAuthorityFailureNone
	return cache, nil
}

// serve returns fresh last-good state immediately, serves stale last-good
// joins that same refresh when the authority is cold. Cancellation of a warm
// caller never cancels the refresh.
func (cache *regimeSnapshotCache) serve(
	callerContext context.Context,
	refresh regimeSnapshotRefreshFunc,
) (regimeSnapshotCacheView, error) {
	if callerContext == nil {
		callerContext = context.Background()
	}

	cache.mu.Lock()
	if len(cache.raw) != 0 {
		now := cache.now()
		cache.recoverClockLocked(now)
		if cache.projectionPending {
			view, raw := cache.viewStateLocked(now)
			cache.mu.Unlock()
			view, viewErr := materializeRegimeSnapshotCacheView(view, raw)
			if viewErr != nil {
				return view, viewErr
			}
			return view, &regimeSnapshotCacheUnavailableError{cause: errRegimeSnapshotProjectionPending}
		}
		if cache.isStaleLocked(now) && !cache.clockInvalidLocked(now) && cache.refresh == nil && refresh != nil && cache.daemonContext.Err() == nil && cache.refreshDueLocked(now) {
			cache.startRefreshLocked(refresh)
		}
		view, raw := cache.viewStateLocked(now)
		cache.mu.Unlock()
		return materializeRegimeSnapshotCacheView(view, raw)
	}
	if refresh == nil {
		view, raw := cache.viewStateLocked(cache.now())
		cache.mu.Unlock()
		view, viewErr := materializeRegimeSnapshotCacheView(view, raw)
		if viewErr != nil {
			return view, viewErr
		}
		return view, &regimeSnapshotCacheUnavailableError{cause: errRegimeSnapshotRefreshMissing}
	}
	job := cache.refresh
	if job == nil {
		if err := cache.daemonContext.Err(); err != nil {
			view, raw := cache.viewStateLocked(cache.now())
			cache.mu.Unlock()
			view, viewErr := materializeRegimeSnapshotCacheView(view, raw)
			if viewErr != nil {
				return view, viewErr
			}
			return view, &regimeSnapshotCacheUnavailableError{cause: err}
		}
		if !cache.refreshDueLocked(cache.now()) {
			view, raw := cache.viewStateLocked(cache.now())
			cache.mu.Unlock()
			view, viewErr := materializeRegimeSnapshotCacheView(view, raw)
			if viewErr != nil {
				return view, viewErr
			}
			return view, &regimeSnapshotCacheUnavailableError{cause: errRegimeSnapshotRefreshSuppressed}
		}
		job = cache.startRefreshLocked(refresh)
	}
	cache.mu.Unlock()

	select {
	case <-job.done:
		cache.mu.Lock()
		view, raw := cache.viewStateLocked(cache.now())
		cache.mu.Unlock()
		view, viewErr := materializeRegimeSnapshotCacheView(view, raw)
		if viewErr != nil {
			return view, viewErr
		}
		if job.err != nil {
			return view, &regimeSnapshotCacheUnavailableError{cause: job.err}
		}
		if view.Snapshot == nil {
			return view, &regimeSnapshotCacheUnavailableError{cause: errors.New("refresh completed without last-good state")}
		}
		return view, nil
	case <-callerContext.Done():
		return regimeSnapshotCacheView{}, callerContext.Err()
	}
}

// current is a non-triggering read for daemon-local consumers such as a brief
// or status projection. It never starts market-data work.
func (cache *regimeSnapshotCache) current() (regimeSnapshotCacheView, error) {
	cache.mu.Lock()
	view, raw := cache.viewStateLocked(cache.now())
	cache.mu.Unlock()
	return materializeRegimeSnapshotCacheView(view, raw)
}

// refreshing is the allocation-free status hook used by backgroundTasks. It
func (cache *regimeSnapshotCache) refreshing() bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.refresh != nil
}

// wait drains the one daemon-owned refresh, if present. Cancellation must be
func (cache *regimeSnapshotCache) wait() {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	job := cache.refresh
	cache.mu.Unlock()
	if job != nil {
		<-job.done
	}
}

// allowRefreshNow clears only the failed-attempt backoff timestamp. A
// successful gateway reconnect can call this once so cold or stale authority
// retries immediately, while the prior stable failure code and every
func (cache *regimeSnapshotCache) allowRefreshNow() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.failureCode != rpc.RegimeAuthorityFailureNone &&
		cache.failureCode != rpc.RegimeAuthorityFailureNoLastGood {
		cache.lastAttemptAt = time.Time{}
	}
}

// invalidateForDependencyPublication makes the next scheduler kick due now
// without clearing last-good bytes, health, failure detail, or revision. A
func (cache *regimeSnapshotCache) invalidateForDependencyPublication() bool {
	if cache == nil {
		return false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.daemonContext.Err() != nil {
		return false
	}
	cache.dependencyRefreshPending = true
	return true
}

// startRefreshAhead starts one daemon-owned refresh before last-good reaches
// publication invalidates the schedule. It never changes health or freshness
// already-stale authority is due immediately. Single-flight, clock, and
// older failed-attempt quiet period because it is fresh recovery evidence.
func (cache *regimeSnapshotCache) startRefreshAhead(refresh regimeSnapshotRefreshFunc, ahead time.Duration) bool {
	if cache == nil || refresh == nil || ahead <= 0 {
		return false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()

	now := cache.now()
	cache.recoverClockLocked(now)
	dependencyDue := cache.dependencyRefreshPending
	if cache.projectionPending || cache.clockInvalidLocked(now) || cache.refresh != nil ||
		cache.daemonContext.Err() != nil || (!dependencyDue && !cache.refreshDueLocked(now)) {
		return false
	}
	if !dependencyDue && len(cache.raw) != 0 && !cache.isStaleLocked(now) {
		refreshAt := cache.lastSuccessAt.Add(cache.freshFor - ahead)
		if now.Before(refreshAt) {
			return false
		}
	}
	cache.dependencyRefreshPending = false
	cache.startRefreshLocked(refresh)
	return true
}

func (cache *regimeSnapshotCache) startRefreshLocked(refresh regimeSnapshotRefreshFunc) *regimeSnapshotRefresh {
	job := &regimeSnapshotRefresh{done: make(chan struct{})}
	cache.refresh = job
	go cache.runRefresh(job, refresh)
	return job
}

func (cache *regimeSnapshotCache) runRefresh(job *regimeSnapshotRefresh, refresh regimeSnapshotRefreshFunc) {
	var runErr error
	failureCode := rpc.RegimeAuthorityFailureRefreshFailed
	defer func() {
		if recover() != nil {
			runErr = errors.New("regime snapshot refresh panicked")
			failureCode = rpc.RegimeAuthorityFailureRefreshFailed
		}
		cache.mu.Lock()
		if runErr != nil {
			cache.failureCode = failureCode
			cache.lastAttemptAt = cache.now().UTC()
		}
		job.err = runErr
		if cache.refresh == job {
			cache.refresh = nil
		}
		cache.mu.Unlock()
		close(job.done)
	}()

	refreshContext, cancel := context.WithTimeout(cache.daemonContext, cache.refreshTimeout)
	defer cancel()

	snapshot, complete, afterPublish, err := refresh(refreshContext)
	runErr = err
	failureCode = rpc.RegimeAuthorityFailureNone
	if runErr != nil {
		failureCode = rpc.RegimeAuthorityFailureRefreshFailed
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(refreshContext.Err(), context.DeadlineExceeded) {
			failureCode = rpc.RegimeAuthorityFailureRefreshTimeout
		}
	} else if !complete {
		runErr = errRegimeSnapshotRefreshIncomplete
		failureCode = rpc.RegimeAuthorityFailureRefreshIncomplete
	}
	if runErr == nil {
		cache.mu.Lock()
		clockInvalid := cache.clockInvalidLocked(cache.now())
		cache.mu.Unlock()
		if clockInvalid {
			runErr = errRegimeSnapshotClockInvalid
			failureCode = rpc.RegimeAuthorityFailureClockInvalid
		}
	}

	var raw []byte
	var fingerprint rpc.Fingerprint
	if runErr == nil {
		raw, fingerprint, runErr = encodeRegimeSnapshotDocument(snapshot)
		if runErr != nil {
			failureCode = rpc.RegimeAuthorityFailurePublishFailed
		}
	}

	var saved corestore.StateDocument
	committed := false
	var commitErr error
	if runErr == nil {
		cache.mu.Lock()
		expectedRevision := cache.revision
		updatedAtFloor := cache.lastSuccessAt
		cache.mu.Unlock()
		prepared, casErr := cache.store.CompareAndSwapStateDocument(refreshContext, corestore.StateDocumentCAS{
			ScopeKey:           daemonStateScope,
			Kind:               regimeSnapshotStateKind,
			ExpectedRevision:   expectedRevision,
			JSON:               raw,
			UpdatedAtNotBefore: updatedAtFloor,
		})
		saved, committed, commitErr = cache.resolveSnapshotCommit(expectedRevision, raw, prepared, casErr)
		if commitErr != nil {
			failureCode = rpc.RegimeAuthorityFailurePublishFailed
			if errors.Is(commitErr, corestore.ErrRollback) {
				failureCode = rpc.RegimeAuthorityFailureClockInvalid
			}
		}
		if !committed {
			runErr = commitErr
		}
	}

	// The callback runs after authoritative CAS but before in-memory promotion,
	// revision aligned, but retain publish_failed health and release cold joiners
	var projectionErr error
	if committed && afterPublish != nil {
		projectionErr = invokeRegimeSnapshotAfterPublish(refreshContext, regimeSnapshotPublication{
			Revision: saved.Revision, PublishedAt: saved.UpdatedAt.UTC(), Fingerprint: fingerprint,
		}, afterPublish)
		if projectionErr != nil {
			failureCode = rpc.RegimeAuthorityFailurePublishFailed
		}
	}
	if committed {
		runErr = errors.Join(commitErr, projectionErr)
	}

	if committed {
		cache.mu.Lock()
		cache.raw = bytes.Clone(saved.JSON)
		cache.revision = saved.Revision
		// In production Now and corestore's commit clock are the same wall
		cache.lastSuccessAt = saved.UpdatedAt.UTC()
		cache.fingerprint = fingerprint
		if runErr == nil {
			cache.failureCode = rpc.RegimeAuthorityFailureNone
			cache.projectionPending = false
			cache.projectionRevision = 0
		} else {
			cache.failureCode = failureCode
			cache.projectionPending = true
			cache.projectionRevision = saved.Revision
		}
		cache.mu.Unlock()
	}
}

// resolveSnapshotCommit distinguishes the two error-bearing outcomes allowed
// and roll back before commit, or it can commit durably and then fail while
// recording the external authority-head watermark. The returned error remains
// visible in both cases, but only exact SQLite readback may authorize in-memory
func (cache *regimeSnapshotCache) resolveSnapshotCommit(
	expectedRevision int64,
	raw []byte,
	prepared corestore.StateDocument,
	casErr error,
) (corestore.StateDocument, bool, error) {
	exact := func(document corestore.StateDocument) bool {
		return document.ScopeKey == daemonStateScope && document.Kind == regimeSnapshotStateKind &&
			document.Revision == expectedRevision+1 && !document.UpdatedAt.IsZero() && bytes.Equal(document.JSON, raw)
	}
	if casErr == nil && exact(prepared) {
		return prepared, true, nil
	}
	cause := casErr
	if cause == nil {
		cause = errors.New("regime snapshot publish returned an invalid commit receipt")
	}

	readContext, cancel := context.WithTimeout(cache.daemonContext, cache.refreshTimeout)
	defer cancel()
	persisted, ok, readErr := cache.store.GetStateDocument(readContext, daemonStateScope, regimeSnapshotStateKind)
	if readErr != nil {
		return corestore.StateDocument{}, false, errors.Join(cause, fmt.Errorf("resolve regime snapshot commit by readback: %w", readErr))
	}
	if ok && exact(persisted) {
		return persisted, true, cause
	}
	if !ok || persisted.Revision == expectedRevision {
		return corestore.StateDocument{}, false, cause
	}
	return corestore.StateDocument{}, false, errors.Join(cause,
		fmt.Errorf("resolve regime snapshot commit: persisted revision %d does not match expected %d", persisted.Revision, expectedRevision+1))
}

func (cache *regimeSnapshotCache) projectionFailure() (bool, int64) {
	if cache == nil {
		return false, 0
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.projectionPending, cache.projectionRevision
}

func (cache *regimeSnapshotCache) markProjectionRepaired(revision int64) error {
	if cache == nil {
		return errors.New("regime snapshot cache is unavailable")
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if !cache.projectionPending {
		return nil
	}
	if cache.revision != revision || cache.projectionRevision != revision {
		return fmt.Errorf("regime projection repair revision changed from %d to %d", revision, cache.revision)
	}
	cache.projectionPending = false
	cache.projectionRevision = 0
	cache.failureCode = rpc.RegimeAuthorityFailureNone
	cache.lastAttemptAt = time.Time{}
	return nil
}

func invokeRegimeSnapshotAfterPublish(ctx context.Context, publication regimeSnapshotPublication, afterPublish regimeSnapshotAfterPublishFunc) (err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	defer func() {
		if recover() != nil {
			err = errors.New("regime snapshot after-publish callback panicked")
		}
	}()
	if err := afterPublish(ctx, publication); err != nil {
		return err
	}
	return ctx.Err()
}

func (cache *regimeSnapshotCache) viewStateLocked(now time.Time) (regimeSnapshotCacheView, []byte) {
	health := cache.healthLocked(now)
	view := regimeSnapshotCacheView{
		Health:      health,
		Revision:    cache.revision,
		Fingerprint: cache.fingerprint,
	}
	return view, bytes.Clone(cache.raw)
}

func materializeRegimeSnapshotCacheView(view regimeSnapshotCacheView, raw []byte) (regimeSnapshotCacheView, error) {
	if len(raw) == 0 {
		return view, nil
	}
	snapshot, err := decodeRegimeSnapshotDocument(raw)
	if err != nil {
		return regimeSnapshotCacheView{}, fmt.Errorf("decode in-memory authoritative regime snapshot: %w", err)
	}
	snapshot.AuthorityHealth = &view.Health
	view.Snapshot = snapshot
	return view, nil
}

func (cache *regimeSnapshotCache) healthLocked(now time.Time) rpc.RegimeAuthorityHealth {
	refreshing := cache.refresh != nil
	if len(cache.raw) == 0 {
		failureCode := cache.failureCode
		if failureCode == rpc.RegimeAuthorityFailureNone && !refreshing {
			failureCode = rpc.RegimeAuthorityFailureNoLastGood
		}
		return rpc.RegimeAuthorityHealth{
			Status:      rpc.RegimeAuthorityUnavailable,
			Refreshing:  refreshing,
			FailureCode: failureCode,
		}
	}

	lastSuccess := cache.lastSuccessAt
	age := now.Sub(lastSuccess)
	failureCode := cache.failureCode
	if age < 0 {
		age = 0
		failureCode = rpc.RegimeAuthorityFailureClockInvalid
	} else if failureCode == rpc.RegimeAuthorityFailureClockInvalid {
		// A rollback classification is self-healing once wall time again reaches
		// keep reporting a historical clock fault while the authority is valid.
		failureCode = rpc.RegimeAuthorityFailureNone
	}
	ageSeconds := int64(age / time.Second)
	status := rpc.RegimeAuthorityFresh
	if cache.isStaleLocked(now) {
		status = rpc.RegimeAuthorityStale
	}
	return rpc.RegimeAuthorityHealth{
		Status:                status,
		Refreshing:            refreshing,
		LastSuccessAt:         &lastSuccess,
		LastSuccessAgeSeconds: &ageSeconds,
		FailureCode:           failureCode,
	}
}

func (cache *regimeSnapshotCache) isStaleLocked(now time.Time) bool {
	return len(cache.raw) != 0 && (cache.fingerprint.Version != rpc.RegimeFingerprintVersion ||
		cache.clockInvalidLocked(now) || !now.Before(cache.lastSuccessAt.Add(cache.freshFor)))
}

func (cache *regimeSnapshotCache) clockInvalidLocked(now time.Time) bool {
	return len(cache.raw) != 0 && now.Before(cache.lastSuccessAt)
}

func (cache *regimeSnapshotCache) recoverClockLocked(now time.Time) {
	if cache.failureCode == rpc.RegimeAuthorityFailureClockInvalid && !cache.clockInvalidLocked(now) {
		cache.failureCode = rpc.RegimeAuthorityFailureNone
		cache.lastAttemptAt = time.Time{}
	}
}

func (cache *regimeSnapshotCache) refreshDueLocked(now time.Time) bool {
	if cache.failureCode == rpc.RegimeAuthorityFailureNone ||
		cache.failureCode == rpc.RegimeAuthorityFailureNoLastGood ||
		cache.lastAttemptAt.IsZero() {
		return true
	}
	return !now.Before(cache.lastAttemptAt.Add(cache.failureRetryAfter))
}

func encodeRegimeSnapshotDocument(snapshot *rpc.RegimeSnapshotResult) ([]byte, rpc.Fingerprint, error) {
	if snapshot == nil {
		return nil, rpc.Fingerprint{}, errors.New("regime snapshot is nil")
	}
	// Strip response-only cache metadata on a shallow value copy, then
	withoutResponseMetadata := *snapshot
	withoutResponseMetadata.AuthorityHealth = nil
	raw, err := json.Marshal(&withoutResponseMetadata)
	if err != nil {
		return nil, rpc.Fingerprint{}, fmt.Errorf("encode regime snapshot: %w", err)
	}
	var detached rpc.RegimeSnapshotResult
	if err := json.Unmarshal(raw, &detached); err != nil {
		return nil, rpc.Fingerprint{}, fmt.Errorf("detach regime snapshot: %w", err)
	}
	if err := validateCompleteRegimeSnapshot(&detached); err != nil {
		return nil, rpc.Fingerprint{}, err
	}
	raw, err = json.Marshal(&detached)
	if err != nil {
		return nil, rpc.Fingerprint{}, fmt.Errorf("encode detached regime snapshot: %w", err)
	}
	return raw, detached.Fingerprint, nil
}

func decodeRegimeSnapshotDocument(raw []byte) (*rpc.RegimeSnapshotResult, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var snapshot rpc.RegimeSnapshotResult
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("decode regime snapshot state document: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if snapshot.AuthorityHealth != nil {
		return nil, errors.New("regime snapshot state document contains response-only authority health")
	}
	if err := validatePersistedRegimeSnapshot(&snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("regime snapshot state document has trailing JSON")
		}
		return fmt.Errorf("decode trailing regime snapshot state: %w", err)
	}
	return nil
}

func validateCompleteRegimeSnapshot(snapshot *rpc.RegimeSnapshotResult) error {
	if err := validateRegimeSnapshotShape(snapshot); err != nil {
		return err
	}
	want := rpc.BuildRegimeFingerprint(snapshot)
	if snapshot.Fingerprint != want {
		return fmt.Errorf("regime snapshot fingerprint mismatch: version/key do not match classified state")
	}
	return nil
}

func validatePersistedRegimeSnapshot(snapshot *rpc.RegimeSnapshotResult) error {
	if err := validateRegimeSnapshotShape(snapshot); err != nil {
		return err
	}
	if !rpc.RegimeFingerprintMatchesSnapshot(snapshot) {
		return fmt.Errorf("regime snapshot fingerprint mismatch: version/key do not match classified state")
	}
	return nil
}

func validateRegimeSnapshotShape(snapshot *rpc.RegimeSnapshotResult) error {
	if snapshot == nil {
		return errors.New("regime snapshot is nil")
	}
	if snapshot.AsOf.IsZero() {
		return errors.New("regime snapshot as_of is required")
	}
	statuses := []struct {
		name   string
		status string
	}{
		{"vix_term_structure", snapshot.VIXTermStructure.Status},
		{"vol_of_vol", snapshot.VolOfVol.Status},
		{"hyg_spy_divergence", snapshot.HYGSPYDivergence.Status},
		{"credit_spreads", snapshot.CreditSpreads.Status},
		{"funding_stress", snapshot.FundingStress.Status},
		{"usd_jpy", snapshot.USDJPY.Status},
		{"gamma_zero", snapshot.GammaZero.Status},
		{"breadth", snapshot.Breadth.Status},
	}
	for _, row := range statuses {
		status := strings.TrimSpace(row.status)
		if status == "" {
			return fmt.Errorf("regime snapshot %s status is required", row.name)
		}
		switch status {
		case rpc.RegimeStatusOK,
			rpc.RegimeStatusStale,
			rpc.RegimeStatusComputing,
			rpc.RegimeStatusUnavailable,
			rpc.RegimeStatusError:
		default:
			return fmt.Errorf("regime snapshot %s status %q is invalid", row.name, status)
		}
	}
	return nil
}
