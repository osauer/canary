package daemon

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// gammaZeroCache holds the current and most-recent zero-gamma compute
// for each scope (combined / SPY / SPX), indexed by NY trading-session
// date. Three concerns intersect here that the existing ttlMap-style
// caches don't handle:
//
//  1. Singleflight per scope. The compute is a multi-minute fan-out
//     across hundreds of option legs against a shared market-data
//     slot pool. Two concurrent callers requesting the SAME scope
//     must share one in-flight job; two concurrent callers requesting
//     DIFFERENT scopes (e.g. combined + --only=spy) must NOT collide
//     on the cache and silently overwrite each other's results.
//
//  2. Background lifetime. The compute outlives any single RPC
//     context — the first caller of the day kicks off a job that the
//     daemon completes regardless of whether the original client
//     hangs around. Subsequent pollers see the in-flight job's
//     progress and pick up the result when it's done.
//
//  3. Scope isolation. The result envelope shape differs across
//     scopes (combined carries PerIndex / RegimeAgreement; SPY-only
//     and SPX-only carry single-underlying-shaped payloads). A
//     scope-mixed cache slot would surface the wrong shape to the
//     wrong caller.
//
// Session key is derived in America/New_York: the result is cached
// for the rest of the same NY trading day and rolls over at midnight
// NY. DST is handled by time.LoadLocation; if the zone fails to load
// the cache falls back to UTC date, which is safe but slightly less
// useful for international callers.
//
// Soft-TTL refresh-while-stale: when a kickOrJoin caller hits a
// cached successful result that's older than the current regular
// option-data session's softTTL (15 min RTH, infinite closed), the
// cache serves the stale value immediately AND kicks a refresh in the
// background. The refresh is stored in the slot's `refresh` field,
// distinct from `current`, so further callers during the refresh keep
// seeing the stable served value rather than blocking on the new
// in-flight job. On completion the refresh promotes to current; on
// error it's discarded so a transient compute failure can't poison a
// known-good cached value.
type gammaZeroCache struct {
	mu sync.Mutex
	// slots holds one entry per scope. Key is the scope string
	slots map[string]*gammaSlot

	// store is the optional on-disk persistence layer. nil = pure
	// newGammaZeroCacheWithStore — never modified after, so reads
	store *gammaZeroStore
	// skewDiag is the optional skew-fit calibration journal. nil = no
	// journaling (tests, store-less constructions). Set once before
	// the cache serves callers and never modified after, so the
	// spawnJob goroutine reads it lock-less like store.
	skewDiag *gammaSkewDiagJournal
	// log is the logger used for persistence warnings. nil-safe via
	// gammaLogf wrapper.
	log gammaLogger
	// onPublication is the daemon-owned cross-authority hook. It is invoked
	// Failed, canceled, or superseded work never reaches it.
	onPublication func(scope string)

	// loadOnce gates the one-shot persisted-result read (see
	loadOnce sync.Once
	loadNow  time.Time
}

const gammaColdCacheAction = "Check `canary status --json` during the next regular U.S. options session; an off-hours forced recompute is an internal diagnostic."

// gammaSlot is the per-scope cache cell. Mirrors the original
type gammaSlot struct {
	current *gammaComputation // nil until first kickOrJoin for this scope
	refresh *gammaComputation // soft-TTL refresh in flight behind current; nil otherwise
	// coldReason* explains why this slot has no serveable result when
	// the daemon can tell the difference between "never computed" and
	// "a persisted cache existed but was unusable." Snapshot surfaces
	// these fields on Status=cold so users do not have to grep logs.
	coldReasonCode string
	coldReason     string
	coldAction     string
	// lastErr / lastErrAt / lastErrSummary retain the prior failure
	// with no indication that the previous compute failed — the
	lastErr        error
	lastErrAt      time.Time
	lastErrSummary string // shortened single-line summary for rendering
	lastErrResult  *rpc.GammaZeroComputed
	// errStreak / lastFailAt drive the escalating retry gate
	// (retryAllowed). Every finished computation bumps or resets them in
	// noteJobOutcome; cancelled jobs are excluded there so a force()
	// supersede or daemon shutdown doesn't count as gateway sickness.
	// lastFailAt carries the failed job's startedAt, matching the
	// startedAt-based age semantics gammaErrorRetryTTL documents.
	errStreak  int
	lastFailAt time.Time
}

// retryAllowed reports whether the slot's failure streak permits another
// automatic compute/refresh attempt at now. Gates ALL non-force spawn
// paths in kickOrJoin — the same-session error retry, the prior-session
// rollover refresh, and the soft-TTL/boundary refresh. The latter two had
// no time gate at all, which is what turned the June 9 secdef-farm outage
// into a respawn storm: the daemon's 1-minute refresh scheduler reaped
// each failed refresh and immediately spawned the next ~35 s burn,
// ~60 times an hour, for the rest of the session. force() stays exempt
// by design.
func (s *gammaSlot) retryAllowed(now time.Time) bool {
	if s.errStreak == 0 {
		return true
	}
	return now.Sub(s.lastFailAt) >= gammaRetryBackoff(s.errStreak)
}

// gammaRetryBackoff converts a consecutive-failure count into the quiet
func gammaRetryBackoff(streak int) time.Duration {
	if streak <= 1 {
		return gammaErrorRetryTTL
	}
	d := gammaErrorRetryTTL << (streak - 1)
	if d <= 0 || d > gammaErrorRetryMaxTTL {
		return gammaErrorRetryMaxTTL
	}
	return d
}

// getOrCreateSlotLocked returns the slot for scope, creating an empty
// one on first access. Caller must hold c.mu.
func (c *gammaZeroCache) getOrCreateSlotLocked(scope string) *gammaSlot {
	if c.slots == nil {
		c.slots = make(map[string]*gammaSlot, 3)
	}
	if s, ok := c.slots[scope]; ok {
		return s
	}
	s := &gammaSlot{}
	c.slots[scope] = s
	return s
}

func (s *gammaSlot) setColdReason(code, reason, action string) {
	s.coldReasonCode = code
	s.coldReason = reason
	s.coldAction = action
}

func (s *gammaSlot) clearColdReason() {
	s.coldReasonCode = ""
	s.coldReason = ""
	s.coldAction = ""
}

// gammaComputation is one zero-gamma run from kickoff through result
// retrieval. The done channel is closed exactly once when the result
type gammaComputation struct {
	sessionKey  string        // e.g., "2026-05-16"
	scope       string        // rpc.GammaZeroScope* — which slot owns this job
	startedAt   time.Time     // kickoff wall-clock
	completedAt time.Time     // successful publication wall-clock; zero until success
	done        chan struct{} // closed once result or err is set
	result      *rpc.GammaZeroComputed
	err         error
	cancel      context.CancelFunc // bounds the bg goroutine; called on superseding compute
	progress    atomic.Int32       // 0–100, best-effort
	etaSeconds  int                // static estimate captured at kickoff
}

// gammaErrorRetryTTL is the minimum age of a cached error before
// kickOrJoin re-attempts. Before this fix a single transient
// gateway-side timeout (e.g. cold-start SPX contract-details race)
// would stick in cache and poison every regime/gamma call for the
// rest of the NY trading session — confirmed observed at v0.22.0.
//
// "Age" is measured against startedAt rather than the deferred
// finishedAt: the cache's now parameter and startedAt share a clock,
// so the TTL check stays testable with synthetic times, and the
// production semantic ("60 s since we kicked the failing attempt") is
// the right one — long error paths shouldn't have an extra 60-s
// quiet period on top of their own duration.
//
// 60 s is long enough to dampen retry storms against a genuinely down
// gateway while short enough that a one-shot blip clears on the
// user's next normal poll. Consecutive failures escalate from this
// base via gammaRetryBackoff — the flat 60 s alone proved insufficient
// against a daylong farm outage (2026-06-09: secdef farm broken from
// 09:33 ET; periodic pollers re-kicked a doomed ~35 s compute every
// poll for hours).
const gammaErrorRetryTTL = 60 * time.Second

// gammaErrorRetryMaxTTL caps the escalating retry gate. Matches
const gammaErrorRetryMaxTTL = 15 * time.Minute

// Session-aware soft TTL: the age at which a cached successful
// FAILED compute on the next call; soft-TTL rolls a SUCCESSFUL
const (
	softTTLRTH    = 15 * time.Minute
	softTTLClosed = time.Duration(math.MaxInt64)
)

// softTTL returns the soft-TTL appropriate for the regular option-data
func softTTL(now time.Time) time.Duration {
	if gammaClassifySession(now) == rpc.SessionClosed {
		return softTTLClosed
	}
	return softTTLRTH
}

// isDone reports whether the compute has finished (success or error).
func (g *gammaComputation) isDone() bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

// successfulAt is the refresh-age anchor for a completed successful job.
func (g *gammaComputation) successfulAt() time.Time {
	if g != nil && !g.completedAt.IsZero() {
		return g.completedAt
	}
	if g == nil {
		return time.Time{}
	}
	return g.startedAt
}

func newGammaZeroCache() *gammaZeroCache {
	return &gammaZeroCache{}
}

// setPublicationCallback installs the daemon-owned successful-publication
// narrow concurrency tests race-free.
func (c *gammaZeroCache) setPublicationCallback(callback func(string)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onPublication = callback
	c.mu.Unlock()
}

// knownGammaScopes enumerates the scopes the cache will look for
// persisted state on startup. Adding a new scope means appending here;
// the cache treats `scope` as an opaque string at the data layer, so
// nothing outside this list breaks.
var knownGammaScopes = []string{
	rpc.GammaZeroScopeCombined,
	rpc.GammaZeroScopeSPY,
	rpc.GammaZeroScopeSPX,
}

// newGammaZeroCacheWithStore returns a cache wired to an on-disk
// acquires the single-instance lock: every autospawn race loser builds
// winning daemon still reads each scope exactly once.
// Persistence errors during load are warnings, not failures —
func newGammaZeroCacheWithStore(store *gammaZeroStore, now time.Time, log gammaLogger) *gammaZeroCache {
	return &gammaZeroCache{
		slots:   make(map[string]*gammaSlot, len(knownGammaScopes)),
		store:   store,
		log:     log,
		loadNow: now,
	}
}

// ensureLoaded runs the one-shot persisted read. Every externally
// promote/outcome goroutines are only reachable after one of those
// has run, so they never need the gate. Must not be called with c.mu
func (c *gammaZeroCache) ensureLoaded() {
	c.loadOnce.Do(c.loadPersisted)
}

func (c *gammaZeroCache) loadPersisted() {
	if c.store == nil {
		return
	}
	now := c.loadNow
	c.mu.Lock()
	defer c.mu.Unlock()
	wrap := gammaLogf{inner: c.log}
	for _, scope := range knownGammaScopes {
		slot := c.getOrCreateSlotLocked(scope)
		persisted, err := c.store.Load(scope, now)
		if err != nil {
			slot.setColdReason(
				"persisted_cache_load_error",
				fmt.Sprintf("persisted gamma cache for %s could not be read: %v", scope, err),
				gammaColdCacheAction,
			)
			wrap.Warnf("gamma cache: load persisted scope=%s: %v (cold start for this scope)", scope, err)
			continue
		}
		if persisted == nil {
			// Last-known-good fallback: today's session-key gate didn't
			stale, stErr := c.store.LoadStale(scope)
			if stErr != nil {
				slot.setColdReason(
					"persisted_stale_cache_load_error",
					fmt.Sprintf("persisted stale gamma cache for %s could not be read: %v", scope, stErr),
					gammaColdCacheAction,
				)
				wrap.Warnf("gamma cache: load stale scope=%s: %v (cold start for this scope)", scope, stErr)
				continue
			}
			if stale == nil {
				// Commonest cold of all — a desk that has never completed a
				// gamma compute. It was also the only one with no reason
				// attached, so the rare load failures below explained
				slot.setColdReason(
					"no_persisted_cache",
					fmt.Sprintf("no gamma computation has completed yet for %s on this desk", scope),
					gammaColdCacheAction,
				)
				continue
			}
			persisted = stale
		}
		if persisted == nil {
			continue
		}
		if err := validateGammaComputed(persisted); err != nil {
			slot.setColdReason(
				"persisted_cache_rejected",
				fmt.Sprintf("persisted gamma cache for %s was rejected: %v", scope, err),
				gammaColdCacheAction,
			)
			wrap.Warnf("gamma cache: discard persisted scope=%s: %v", scope, err)
			continue
		}
		hydrateGammaComputed(persisted)
		slot.current = newPersistedComputation(persisted, scope, now)
		slot.clearColdReason()
		wrap.Infof("gamma cache: loaded persisted result scope=%s session=%s as_of=%s",
			scope, nySessionKey(now), persisted.AsOf.Format(time.RFC3339))
	}
}

// newPersistedComputation wraps a persisted result in a
// cancel are zero-valued (cancel is only used to abort in-flight
func newPersistedComputation(r *rpc.GammaZeroComputed, scope string, now time.Time) *gammaComputation {
	sessionKey := nySessionKey(now)
	if r != nil && !r.AsOf.IsZero() {
		sessionKey = nySessionKey(r.AsOf)
	}
	job := &gammaComputation{
		sessionKey:  sessionKey,
		scope:       scope,
		startedAt:   r.AsOf,
		completedAt: gammaPersistedCompletionAt(r),
		done:        make(chan struct{}),
		result:      r,
	}
	close(job.done)
	return job
}

func gammaPersistedCompletionAt(r *rpc.GammaZeroComputed) time.Time {
	if r == nil {
		return time.Time{}
	}
	latest := r.AsOf
	for _, sub := range r.PerIndex {
		if sub != nil && sub.AsOf.After(latest) {
			latest = sub.AsOf
		}
	}
	return latest
}

func validateGammaComputed(r *rpc.GammaZeroComputed) error {
	if r == nil {
		return fmt.Errorf("zero-gamma compute returned nil result")
	}
	if r.PricedLegCount > 0 && r.LegCount == 0 {
		return fmt.Errorf("zero-gamma invalid result: %d priced legs but no usable GEX legs", r.PricedLegCount)
	}
	if r.LegCount > 0 && r.GammaTotalAbs == 0 && len(r.TopStrikes) == 0 && gammaProfileAllZero(r.Profile) {
		return fmt.Errorf("zero-gamma invalid result: %d GEX legs but zero gamma_total_abs/profile/top_strikes", r.LegCount)
	}
	for key, sub := range r.PerIndex {
		if err := validateGammaComputed(sub); err != nil {
			return fmt.Errorf("per_index[%s]: %w", key, err)
		}
	}
	return nil
}

func gammaProfileAllZero(profile []rpc.GammaProfilePoint) bool {
	if len(profile) == 0 {
		return true
	}
	for _, p := range profile {
		if math.Abs(p.GEX) > 1e-9 {
			return false
		}
	}
	return true
}

// IsComputing reports whether a gamma compute is currently in flight.
func (c *gammaZeroCache) IsComputing() bool {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, slot := range c.slots {
		if slot.current != nil && !slot.current.isDone() {
			return true
		}
		if slot.refresh != nil && !slot.refresh.isDone() {
			return true
		}
	}
	return false
}

// nySessionKey returns the NY-tz date string that identifies the
// cache key. Falls back to UTC date if the zone fails to load —
func nySessionKey(now time.Time) string {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return now.In(loc).Format("2006-01-02")
	}
	return now.UTC().Format("2006-01-02")
}

// computeFn is the contract the cache calls when it needs to kick a
// fresh compute. It runs on the daemon's goroutine, not the caller's,
// and gets a context bounded by the bg goroutine's lifetime (cancelled
// when the compute is superseded or the daemon shuts down).
//
// Implementations should update progress periodically (the cache
// stamps the etaSeconds once at kickoff but doesn't touch progress —
// the compute owns it).
type computeFn func(ctx context.Context, progress *atomic.Int32) (*rpc.GammaZeroComputed, error)

// kickOrJoin returns the active or most-recent computation for the
// current NY session. If a fresh compute is needed, it's started in
// a background goroutine via compute(); the returned gammaComputation
// may be in-flight or already complete. Concurrent callers always
// share — only one fan-out per session per non-force call.
//
// fresh is true when this call started the compute, false when an
// existing computation was returned (in-flight or finished).
//
// etaSeconds is the static initial estimate the cache stamps on a
// fresh kickoff. The compute reports refined progress via its
// atomic counter.
//
// Soft-TTL refresh: when the served result is past the regular
// option-data session's softTTL, or was computed outside RTH and is
// first served during RTH, kickOrJoin kicks a background refresh while
// still returning the stale value. fresh stays false for the caller
// (they got the cached envelope, not a new in-flight job). The next
// caller after the refresh lands sees the new value. Closed option-data
// sessions never trigger refresh — see the kickOrJoin body comment for
// the gating logic.
func (c *gammaZeroCache) kickOrJoin(parent context.Context, scope string, now time.Time, etaSeconds int, compute computeFn) (job *gammaComputation, fresh bool) {
	c.ensureLoaded()
	key := nySessionKey(now)

	c.mu.Lock()
	defer c.mu.Unlock()

	slot := c.getOrCreateSlotLocked(scope)

	// Promote a landed soft-TTL refresh; discard a failed one. A
	// failed refresh must NOT poison a known-good cached value —
	if slot.refresh != nil && slot.refresh.isDone() {
		if slot.refresh.err == nil && slot.refresh.sessionKey == key {
			slot.current = slot.refresh
		} else if slot.refresh.err != nil {
			slot.rememberError(slot.refresh)
		}
		slot.refresh = nil
	}

	// SessionClosed gate: outside the regular U.S. listed-options
	// session we never kick a non-force compute. Dealer gamma needs
	// Subsequent non-force callers must be able to join the existing job
	// force error must remain visible instead of collapsing back to Cold.
	// Only fully idle + no successful/error cache returns (nil, false).
	if gammaClassifySession(now) == rpc.SessionClosed {
		if slot.current != nil {
			if !slot.current.isDone() {
				return slot.current, false
			}
			if slot.current.err != nil {
				return slot.current, false
			}
			if slot.current.err == nil && slot.current.result != nil {
				return slot.current, false
			}
		}
		return nil, false
	}

	// Active-session rollover with a known-good prior snapshot: keep serving
	// This avoids turning a production signal into "computing" at the exact
	if slot.current != nil && slot.current.sessionKey != key &&
		slot.current.isDone() && slot.current.err == nil && slot.current.result != nil {
		// retryAllowed: without it, a failed refresh reaped above is
		// respawned by the very same call, at poll rate, for as long as
		// the gateway stays sick (observed June 9 post-restart: the
		// LoadStale-seeded prior-day result kept this path hot).
		if slot.refresh == nil && slot.retryAllowed(now) {
			slot.refresh = c.spawnJob(parent, scope, key, now, etaSeconds, compute)
		}
		return slot.current, false
	}

	if slot.current != nil && slot.current.sessionKey == key {
		// Same session — but a cached error past the retry gate must NOT
		// gate escalates with the slot's consecutive-failure streak
		if slot.current.isDone() && slot.current.err != nil && slot.retryAllowed(now) {
			// Retain the failure context so the next render of the
			// "computing" row can surface "retry of <error> at HH:MM:SS"
			// instead of silently switching to a clean Computing state.
			slot.rememberError(slot.current)
			job = c.startLocked(parent, scope, key, now, etaSeconds, compute)
			return job, true
		}
		// Soft-TTL: if the served value is stale and no refresh is
		// age check can never fire there, and the explicit
		// refreshes keep failing (a failed refresh never advances
		if slot.current.isDone() && slot.current.err == nil && slot.refresh == nil && slot.retryAllowed(now) {
			currentClass := gammaClassifySession(now)
			if currentClass != rpc.SessionClosed {
				publishedAt := slot.current.successfulAt()
				cachedClass := gammaClassifySession(publishedAt)
				classChanged := cachedClass != currentClass
				if classChanged || now.Sub(publishedAt) >= softTTL(now) {
					slot.refresh = c.spawnJob(parent, scope, key, now, etaSeconds, compute)
				}
			}
		}
		// Same session: serve the in-flight or recently-completed job.
		return slot.current, false
	}

	job = c.startLocked(parent, scope, key, now, etaSeconds, compute)
	return job, true
}

// force starts a fresh compute for the current NY session. With no
// successful cached value it supersedes the current job. With a successful
// cached value already serving, it runs as a diagnostic refresh behind that
// value and promotes only on success; failed diagnostics must not poison the
// cache callers rely on outside market hours.
//
// An in-flight current job is still cancelled and superseded: there is no
// stable value to preserve, and the caller explicitly requested force.
//
// Any in-flight soft-TTL refresh is also cancelled and discarded —
// force is the active diagnostic attempt for this scope.
func (c *gammaZeroCache) force(parent context.Context, scope string, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
	c.ensureLoaded()
	key := nySessionKey(now)

	c.mu.Lock()
	defer c.mu.Unlock()

	slot := c.getOrCreateSlotLocked(scope)
	preserveCurrent := slot.current != nil &&
		slot.current.isDone() &&
		slot.current.err == nil &&
		slot.current.result != nil
	if slot.current != nil && !slot.current.isDone() {
		// Cancel the superseded compute — it stops fanning out and the
		// next time the gateway responds to one of its in-flight legs
		// the worker returns immediately. The done channel doesn't
		// close (the goroutine returns without setting result), so
		// any caller still waiting on the old job will block until
		// its own ctx times out. That's the documented force tradeoff.
		slot.current.cancel()
	}
	if slot.refresh != nil && !slot.refresh.isDone() {
		slot.refresh.cancel()
	}
	slot.refresh = nil
	if preserveCurrent {
		job := c.spawnJob(parent, scope, key, now, etaSeconds, compute)
		slot.refresh = job
		c.promoteRefreshOnDone(scope, key, job)
		return job
	}
	return c.startLocked(parent, scope, key, now, etaSeconds, compute)
}

func (c *gammaZeroCache) promoteRefreshOnDone(scope, key string, job *gammaComputation) {
	go func() {
		<-job.done
		c.mu.Lock()
		defer c.mu.Unlock()
		slot := c.slots[scope]
		if slot == nil || slot.refresh != job {
			return
		}
		if job.err == nil && job.result != nil && job.sessionKey == key {
			slot.current = job
		} else if job.err != nil {
			slot.rememberError(job)
		}
		slot.refresh = nil
	}()
}

// spawnJob allocates a fresh computation and launches its background
// goroutine. Caller must hold c.mu. Does NOT assign the job into any
// slot — the caller decides whether to install it as `current`
// (startLocked) or hold it separately as the soft-TTL refresh.
//
// scope is captured on the gammaComputation so the persist call can
// route the save to the right per-scope file.
func (c *gammaZeroCache) spawnJob(parent context.Context, scope, key string, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
	// Decouple the compute's lifetime from any single RPC ctx. Use the
	// daemon's parent context (typically Background) as the upstream
	// signal so daemon shutdown still cancels the compute, but a
	// client disconnect mid-compute doesn't kill a job that other
	// pollers are waiting on.
	bgCtx, cancel := context.WithCancel(parent)
	job := &gammaComputation{
		sessionKey: key,
		scope:      scope,
		startedAt:  now,
		done:       make(chan struct{}),
		cancel:     cancel,
		etaSeconds: etaSeconds,
	}
	wallStartedAt := time.Now()

	go func() {
		// Publication notification runs after done closes, so downstream readers
		// can observe Status=ready, and after outcome accounting has reset the
		// retry streak. It remains outside the cache lock while calling daemon
		// glue so Regime acquisition may read this same cache.
		defer func() { c.notifyJobPublication(job, bgCtx.Err() != nil) }()
		defer close(job.done)
		// Failure-streak accounting. Deliberately registered between the
		// done-close and the panic guard (LIFO order: guard finalises
		// job.err first, then this observes it, then done closes).
		// bgCtx.Err() != nil marks cancellation — force() supersede or
		// daemon shutdown — which says nothing about gateway health.
		defer func() { c.noteJobOutcome(job, bgCtx.Err() != nil) }()
		// Best-effort panic guard: a math bug or nil pointer deep in
		// the compute pipeline shouldn't take down the daemon. The
		// recovered error becomes job.err, which surfaces to callers
		// as Status=error on the next poll.
		defer func() {
			if r := recover(); r != nil {
				job.err = fmt.Errorf("zero-gamma compute panicked: %v", r)
				gammaLogf{inner: c.log}.Warnf("gamma compute: scope=%s failed: %v", scope, job.err)
			}
		}()
		res, err := compute(bgCtx, &job.progress)
		if err != nil {
			job.err = err
			job.result = hydrateGammaDiagnosticResult(res, time.Now())
			gammaLogf{inner: c.log}.Warnf("gamma compute: scope=%s failed: %v", scope, err)
			return
		}
		if err := validateGammaComputed(res); err != nil {
			job.err = err
			gammaLogf{inner: c.log}.Warnf("gamma compute: scope=%s failed: %v", scope, err)
			return
		}
		job.result = hydrateGammaComputed(res)
		// Persist to disk on success. Failed computes do not persist
		// shutdown, force() supersede) must not overwrite the last
		// at construction and never modified, so reading it lock-
		// less is safe. Save errors degrade to warnings only — the
		if c.store != nil && bgCtx.Err() == nil {
			if saveErr := c.store.Save(scope, key, res); saveErr != nil {
				gammaLogf{inner: c.log}.Warnf("gamma cache: persist scope=%s: %v", scope, saveErr)
			}
		}
		// Skew-fit calibration journal, under the same cancellation
		// must not enter the calibration set either.
		if c.skewDiag != nil && bgCtx.Err() == nil {
			if diagErr := c.skewDiag.append(time.Now(), scope, key, job.result); diagErr != nil {
				gammaLogf{inner: c.log}.Warnf("gamma skew diag: append scope=%s: %v", scope, diagErr)
			}
		}
		// Use the caller's clock origin plus elapsed wall time. Production gets
		// successful result earns its full quiet period only once it is publishable.
		job.completedAt = now.Add(time.Since(wallStartedAt))
	}()

	return job
}

// notifyJobPublication promotes a successful refresh before notifying the
// daemon. The slot identity checks exclude force-superseded work, and the
// callback runs only after the result is both complete and canonical for its
// scope. Soft-TTL and forced refreshes therefore share one publication edge.
func (c *gammaZeroCache) notifyJobPublication(job *gammaComputation, cancelled bool) {
	if c == nil || job == nil || cancelled || job.err != nil || job.result == nil {
		return
	}
	c.mu.Lock()
	slot := c.slots[job.scope]
	published := false
	if slot != nil {
		switch {
		case slot.current == job:
			published = true
		case slot.refresh == job:
			slot.current = job
			slot.refresh = nil
			published = true
		}
	}
	callback := c.onPublication
	c.mu.Unlock()
	if published && callback != nil {
		callback(job.scope)
	}
}

// noteJobOutcome records a finished computation in its slot's failure
// streak: an error bumps the streak and stamps lastFailAt with the
// so the accounting runs exactly once per job with no dedupe — and it
// a force() supersede or daemon shutdown is not a gateway failure.
func (c *gammaZeroCache) noteJobOutcome(job *gammaComputation, cancelled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	slot := c.slots[job.scope]
	if slot == nil {
		return
	}
	if job.err != nil {
		if cancelled {
			return
		}
		slot.errStreak++
		slot.lastFailAt = job.startedAt
		return
	}
	slot.errStreak = 0
	slot.lastFailAt = time.Time{}
}

// resetRetryBackoff zeroes every slot's failure streak. Called on
// gateway (re)connect: farm outages end with a reconnect handshake, so
// the first attempt after one shouldn't sit out a 15-minute escalated
// quiet period earned against the dead connection.
func (c *gammaZeroCache) resetRetryBackoff() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, slot := range c.slots {
		slot.errStreak = 0
		slot.lastFailAt = time.Time{}
	}
}

// startLocked allocates and launches a fresh compute, assigning the
// new job to slot.current for the scope. Caller must hold c.mu. Thin
func (c *gammaZeroCache) startLocked(parent context.Context, scope, key string, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
	job := c.spawnJob(parent, scope, key, now, etaSeconds, compute)
	slot := c.getOrCreateSlotLocked(scope)
	slot.current = job
	return job
}

func (c *gammaZeroCache) snapshotCombinedSlice(scope string, nowFn func() time.Time) (rpc.GammaZeroSPXResult, bool) {
	c.ensureLoaded()
	key := ""
	switch scope {
	case rpc.GammaZeroScopeSPY:
		key = "SPY"
	case rpc.GammaZeroScopeSPX:
		key = "SPX"
	default:
		return rpc.GammaZeroSPXResult{}, false
	}

	c.mu.Lock()
	slot := c.slots[rpc.GammaZeroScopeCombined]
	var job *gammaComputation
	if slot != nil {
		job = slot.current
	}
	c.mu.Unlock()
	if job == nil {
		return rpc.GammaZeroSPXResult{}, false
	}

	now := nowFn()
	env := c.snapshotForScope(rpc.GammaZeroScopeCombined, job, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady || env.Result == nil {
		return rpc.GammaZeroSPXResult{}, false
	}
	if env.Result.Scope == scope {
		return env, true
	}
	sub := env.Result.PerIndex[key]
	if sub == nil {
		return rpc.GammaZeroSPXResult{}, false
	}
	env.Result = sub
	return env, true
}

func (c *gammaZeroCache) snapshotCurrent(scope string, nowFn func() time.Time) rpc.GammaZeroSPXResult {
	c.ensureLoaded()
	c.mu.Lock()
	slot := c.slots[scope]
	var job *gammaComputation
	if slot != nil {
		job = slot.current
	}
	c.mu.Unlock()
	return c.snapshotForScope(scope, job, nowFn)
}

// refreshInFlight reports whether a soft-TTL or session-rollover refresh is
// running behind the result this scope currently serves.
func (c *gammaZeroCache) refreshInFlight(scope string) bool {
	if scope == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	slot, ok := c.slots[scope]
	return ok && slot != nil && slot.refresh != nil && !slot.refresh.isDone()
}

func gammaSnapshotScope(scope string, g *gammaComputation) string {
	if scope != "" {
		return scope
	}
	if g == nil {
		return ""
	}
	return g.scope
}

func (c *gammaZeroCache) snapshotForScope(scope string, g *gammaComputation, nowFn func() time.Time) rpc.GammaZeroSPXResult {
	c.ensureLoaded()
	if g == nil {
		env := rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusCold}
		if scope != "" {
			c.mu.Lock()
			if slot, ok := c.slots[scope]; ok {
				env.ColdReasonCode = slot.coldReasonCode
				env.ColdReason = slot.coldReason
				env.ColdAction = slot.coldAction
			}
			c.mu.Unlock()
		}
		return env
	}
	started := g.startedAt
	env := rpc.GammaZeroSPXResult{
		StartedAt: &started,
	}
	if g.isDone() {
		if g.err != nil {
			env.Status = rpc.GammaZeroStatusError
			env.Error = g.err.Error()
			env.DiagnosticResult = hydrateGammaDiagnosticResult(g.result, nowFn())
			// A failed attempt outside the regular options session cannot
			// did fail — but the reason says when, and what happens next.
			if now := nowFn(); gammaClassifySession(now) == rpc.SessionClosed {
				env.ColdReasonCode = "closed_session_last_attempt_failed"
				env.ColdReason = fmt.Sprintf(
					"the last gamma attempt failed at %s and the regular U.S. options session is closed, so no automatic retry runs until it reopens",
					nyTime(started).Format("2006-01-02 15:04 MST"))
				env.ColdAction = gammaColdCacheAction
			}
			return env
		}
		env.Status = rpc.GammaZeroStatusReady
		env.Result = g.result
		// A served last-good says nothing about whether its replacement is
		// flight from one that never started.
		env.Refreshing = c.refreshInFlight(gammaSnapshotScope(scope, g))
		// Off-hours stale tag: when we're serving a cached result outside
		// trading hours that predates the last completed options session,
		// append the `cache_stale_off_hours` warning so the renderer can
		// say "computed Nh ago" loudly instead of presenting a
		// missed-session reading as if it were fresh. The quality gates
		// treat this tag as a rankability block, so it must not fire on a
		// weekend serving Friday's close — that cache is the expected
		// evidence, however many wall-clock hours old. Copy-on-write
		// to avoid mutating the shared cache pointer that other
		// concurrent snapshots may still be reading.
		now := nowFn()
		if g.result != nil && gammaClassifySession(now) == rpc.SessionClosed && gammaClosedSessionCacheStale(g.result.AsOf, now) {
			r := *g.result
			r.Warnings = dedupeStrings(append(append([]string{}, g.result.Warnings...), "cache_stale_off_hours"))
			hydrateGammaComputed(&r)
			env.Result = &r
		}
		env = c.withCachedSPXFallback(scope, env, now)
		env = c.withLatestSingleScopeSlices(scope, env, now)
		env = c.finalizeReadyGammaSnapshot(g.scope, env, now)
		// Clear stale prior-error context for this scope — a successful
		// compute means the previous failure is no longer informative.
		c.mu.Lock()
		if slot, ok := c.slots[g.scope]; ok {
			if slot.lastErrAt.IsZero() || !slot.lastErrAt.After(g.startedAt) {
				slot.lastErr = nil
				slot.lastErrSummary = ""
				slot.lastErrResult = nil
			}
		}
		c.mu.Unlock()
		return env
	}
	progress := g.progress.Load()
	env.Status = rpc.GammaZeroStatusComputing
	env.EtaSeconds = remainingEta(g, nowFn(), progress)
	env.Progress = int(progress)
	// Attach prior-failure context for THIS scope if the current
	// in-flight compute is a retry of a recent failure. The renderer
	c.mu.Lock()
	if slot, ok := c.slots[g.scope]; ok && slot.lastErr != nil {
		retryAt := slot.lastErrAt
		env.RetryOfErrorAt = &retryAt
		env.RetryOfErrorSummary = slot.lastErrSummary
		env.DiagnosticResult = hydrateGammaDiagnosticResult(slot.lastErrResult, nowFn())
	}
	c.mu.Unlock()
	return env
}

func (c *gammaZeroCache) finalizeReadyGammaSnapshot(scope string, env rpc.GammaZeroSPXResult, now time.Time) rpc.GammaZeroSPXResult {
	if env.Status != rpc.GammaZeroStatusReady || env.Result == nil {
		return env
	}
	result := cloneGammaComputed(env.Result)
	if warning := c.refreshFailureWarning(scope, result); warning != "" {
		result.Warnings = dedupeStrings(append(result.Warnings, warning))
		env.DiagnosticResult = c.refreshFailureDiagnostic(scope, result, now)
	}
	hydrateGammaComputed(result)
	annotateGammaQuality(result, now)
	refreshGammaSummaries(result)
	env.Result = result
	return env
}

func (s *gammaSlot) rememberError(job *gammaComputation) {
	if s == nil || job == nil || job.err == nil {
		return
	}
	s.lastErr = job.err
	s.lastErrAt = job.startedAt
	s.lastErrSummary = summarizeGammaErr(job.err)
	s.lastErrResult = cloneGammaComputed(job.result)
}

func hydrateGammaDiagnosticResult(result *rpc.GammaZeroComputed, now time.Time) *rpc.GammaZeroComputed {
	if result == nil {
		return nil
	}
	out := cloneGammaComputed(result)
	hydrateGammaComputed(out)
	annotateGammaQuality(out, now)
	refreshGammaSummaries(out)
	return out
}

func (c *gammaZeroCache) refreshFailureWarning(scope string, result *rpc.GammaZeroComputed) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	slot := c.slots[scope]
	if slot == nil || slot.lastErr == nil || slot.lastErrAt.IsZero() {
		return ""
	}
	if result != nil && !result.AsOf.IsZero() && !slot.lastErrAt.After(result.AsOf) {
		return ""
	}
	summary := summarizeGammaPhaseFailure(slot.lastErr)
	if summary == "" {
		summary = "unavailable"
	}
	return "refresh_failed:" + strings.ReplaceAll(summary, " ", "_")
}

func (c *gammaZeroCache) refreshFailureDiagnostic(scope string, result *rpc.GammaZeroComputed, now time.Time) *rpc.GammaZeroComputed {
	c.mu.Lock()
	slot := c.slots[scope]
	var diag *rpc.GammaZeroComputed
	if slot != nil && slot.lastErr != nil && !slot.lastErrAt.IsZero() &&
		(result == nil || result.AsOf.IsZero() || slot.lastErrAt.After(result.AsOf)) {
		diag = cloneGammaComputed(slot.lastErrResult)
	}
	c.mu.Unlock()
	return hydrateGammaDiagnosticResult(diag, now)
}

func (c *gammaZeroCache) withLatestSingleScopeSlices(scope string, env rpc.GammaZeroSPXResult, now time.Time) rpc.GammaZeroSPXResult {
	if scope != rpc.GammaZeroScopeCombined || env.Status != rpc.GammaZeroStatusReady || env.Result == nil {
		return env
	}
	origSPY := gammaSliceForLabel(env.Result, "SPY")
	origSPX := gammaSliceForLabel(env.Result, "SPX")
	spy := newestGammaSlice(origSPY, c.readySingleScopeSlice(rpc.GammaZeroScopeSPY, now))
	spx := newestGammaSlice(origSPX, c.readySingleScopeSlice(rpc.GammaZeroScopeSPX, now))
	if gammaIndexUnavailable(env.Result, "SPY") && (spy == nil || !spy.AsOf.After(env.Result.AsOf)) {
		return env
	}
	if spy == nil || spx == nil {
		return env
	}
	if env.Result.Scope == rpc.GammaZeroScopeCombined && spy == origSPY && spx == origSPX {
		return env
	}

	spyCopy := cloneGammaComputed(spy)
	spxCopy := cloneGammaComputed(spx)
	stripSPYUnavailableWarning(spxCopy)
	stripSPXUnavailableWarning(spyCopy)

	combined := combineGammaResults(spyCopy, spxCopy)
	if combined == nil {
		return env
	}
	env.Result = hydrateGammaComputed(combined)
	return env
}

func gammaIndexUnavailable(c *rpc.GammaZeroComputed, label string) bool {
	if c == nil {
		return false
	}
	prefix := strings.ToLower(label) + "_unavailable:"
	for _, code := range c.Warnings {
		if strings.HasPrefix(code, prefix) {
			return true
		}
	}
	for _, d := range c.WarningDetails {
		if strings.HasPrefix(d.Code, prefix) {
			return true
		}
	}
	return false
}

func (c *gammaZeroCache) readySingleScopeSlice(scope string, now time.Time) *rpc.GammaZeroComputed {
	if scope != rpc.GammaZeroScopeSPY && scope != rpc.GammaZeroScopeSPX {
		return nil
	}
	c.mu.Lock()
	slot := c.slots[scope]
	var job *gammaComputation
	if slot != nil {
		job = slot.current
	}
	c.mu.Unlock()
	if job == nil || !job.isDone() || job.err != nil || job.result == nil {
		return nil
	}
	if job.sessionKey != nySessionKey(now) && gammaClassifySession(now) != rpc.SessionClosed {
		return nil
	}
	if !gammaSliceEligibleForCombined(job.result, now) {
		return nil
	}
	return job.result
}

func gammaSliceEligibleForCombined(c *rpc.GammaZeroComputed, now time.Time) bool {
	if c == nil {
		return false
	}
	if gammaClassifySession(now) != rpc.SessionClosed {
		if c.AsOf.IsZero() || nySessionKey(c.AsOf) != nySessionKey(now) {
			return false
		}
	}
	quality := c.Quality
	if quality == nil {
		copy := cloneGammaComputed(c)
		hydrateGammaComputed(copy)
		annotateGammaQuality(copy, now)
		quality = copy.Quality
	}
	if quality == nil || quality.Rankability != rpc.GammaRankabilityRankable {
		return false
	}
	return true
}

func gammaSliceForLabel(c *rpc.GammaZeroComputed, label string) *rpc.GammaZeroComputed {
	if c == nil {
		return nil
	}
	switch label {
	case "SPY":
		return gammaSPYForFallback(c)
	case "SPX":
		return gammaSPXForFallback(c)
	default:
		return nil
	}
}

func newestGammaSlice(a, b *rpc.GammaZeroComputed) *rpc.GammaZeroComputed {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if b.AsOf.After(a.AsOf) {
		return b
	}
	return a
}

func (c *gammaZeroCache) withCachedSPXFallback(scope string, env rpc.GammaZeroSPXResult, now time.Time) rpc.GammaZeroSPXResult {
	if scope != rpc.GammaZeroScopeCombined || env.Status != rpc.GammaZeroStatusReady || env.Result == nil {
		return env
	}
	if env.Result.Scope == rpc.GammaZeroScopeCombined && env.Result.PerIndex["SPX"] != nil {
		return env
	}

	spy := gammaSPYForFallback(env.Result)
	if spy == nil {
		return env
	}

	c.mu.Lock()
	slot := c.slots[rpc.GammaZeroScopeSPX]
	var spxJob *gammaComputation
	if slot != nil {
		spxJob = slot.current
	}
	c.mu.Unlock()
	if spxJob == nil {
		return env
	}

	spxEnv := c.snapshotForScope(rpc.GammaZeroScopeSPX, spxJob, func() time.Time { return now })
	if spxEnv.Status != rpc.GammaZeroStatusReady || spxEnv.Result == nil {
		return env
	}
	if !gammaSliceFreshEnoughForFallback(spxEnv.Result, now) {
		return env
	}

	spyCopy := cloneGammaComputed(spy)
	spxCopy := cloneGammaComputed(spxEnv.Result)
	stripSPXUnavailableWarning(spyCopy)

	reason := spxFallbackReason(env.Result)
	warning := "spx_cache_fallback"
	if reason != "" {
		warning += ":" + reason
	}
	spxCopy.Warnings = dedupeStrings(append(spxCopy.Warnings, warning))

	combined := combineGammaResults(spyCopy, spxCopy)
	if combined == nil {
		return env
	}
	combined.Warnings = dedupeStrings(append(combined.Warnings, warning))
	combined.Source = "computed from IBKR SPY option chain plus cached IBKR SPX option chain fallback"
	if !spyCopy.AsOf.IsZero() && !spxCopy.AsOf.IsZero() && spxCopy.AsOf.Before(spyCopy.AsOf) {
		combined.AsOf = spxCopy.AsOf
	}
	env.Result = hydrateGammaComputed(combined)
	return env
}

func gammaSPYForFallback(c *rpc.GammaZeroComputed) *rpc.GammaZeroComputed {
	if c == nil {
		return nil
	}
	if c.Scope == rpc.GammaZeroScopeSPY {
		return c
	}
	if c.Scope == rpc.GammaZeroScopeCombined && c.PerIndex != nil {
		return c.PerIndex["SPY"]
	}
	return nil
}

func gammaSliceFreshEnoughForFallback(c *rpc.GammaZeroComputed, now time.Time) bool {
	if c == nil {
		return false
	}
	copy := cloneGammaComputed(c)
	hydrateGammaComputed(copy)
	annotateGammaQuality(copy, now)
	if copy.Quality == nil {
		return false
	}
	switch copy.Quality.Freshness {
	case "fresh", "closed_session_cache":
		return true
	default:
		return false
	}
}

func gammaSPXForFallback(c *rpc.GammaZeroComputed) *rpc.GammaZeroComputed {
	if c == nil {
		return nil
	}
	if c.Scope == rpc.GammaZeroScopeSPX {
		return c
	}
	if c.Scope == rpc.GammaZeroScopeCombined && c.PerIndex != nil {
		return c.PerIndex["SPX"]
	}
	return nil
}

func cloneGammaComputed(c *rpc.GammaZeroComputed) *rpc.GammaZeroComputed {
	if c == nil {
		return nil
	}
	out := *c
	out.Warnings = append([]string(nil), c.Warnings...)
	out.WarningDetails = append([]rpc.GammaWarningDetail(nil), c.WarningDetails...)
	out.Expirations = append([]string(nil), c.Expirations...)
	out.TopStrikes = append([]rpc.StrikeConcentration(nil), c.TopStrikes...)
	out.Profile = append([]rpc.GammaProfilePoint(nil), c.Profile...)
	out.Profile0DTE = append([]rpc.GammaProfilePoint(nil), c.Profile0DTE...)
	out.Profile1to7 = append([]rpc.GammaProfilePoint(nil), c.Profile1to7...)
	out.ProfileTerm = append([]rpc.GammaProfilePoint(nil), c.ProfileTerm...)
	if c.SkewFitQuality != nil {
		out.SkewFitQuality = make(map[string]rpc.SkewFitInfo, len(c.SkewFitQuality))
		maps.Copy(out.SkewFitQuality, c.SkewFitQuality)
	}
	if c.PartialClasses != nil {
		out.PartialClasses = make(map[string]string, len(c.PartialClasses))
		maps.Copy(out.PartialClasses, c.PartialClasses)
	}
	if c.Quality != nil {
		q := *c.Quality
		q.Gates = append([]rpc.GammaQualityGate(nil), c.Quality.Gates...)
		q.Blockers = append([]string(nil), c.Quality.Blockers...)
		q.Context = append([]string(nil), c.Quality.Context...)
		if c.Quality.ByUnderlying != nil {
			q.ByUnderlying = make(map[string]rpc.GammaSignalQuality, len(c.Quality.ByUnderlying))
			maps.Copy(q.ByUnderlying, c.Quality.ByUnderlying)
		}
		out.Quality = &q
	}
	if c.PerIndex != nil {
		out.PerIndex = make(map[string]*rpc.GammaZeroComputed, len(c.PerIndex))
		for k, v := range c.PerIndex {
			out.PerIndex[k] = cloneGammaComputed(v)
		}
	}
	return &out
}

func stripSPYUnavailableWarning(c *rpc.GammaZeroComputed) {
	if c == nil {
		return
	}
	c.Warnings = filterGammaWarnings(c.Warnings, func(code string) bool {
		return !strings.HasPrefix(code, "spy_unavailable:")
	})
	if len(c.WarningDetails) > 0 {
		out := c.WarningDetails[:0]
		for _, d := range c.WarningDetails {
			if !strings.HasPrefix(d.Code, "spy_unavailable:") {
				out = append(out, d)
			}
		}
		c.WarningDetails = out
	}
}

func stripSPXUnavailableWarning(c *rpc.GammaZeroComputed) {
	if c == nil {
		return
	}
	c.Warnings = filterGammaWarnings(c.Warnings, func(code string) bool {
		return !strings.HasPrefix(code, "spx_unavailable:")
	})
	if len(c.WarningDetails) > 0 {
		out := c.WarningDetails[:0]
		for _, d := range c.WarningDetails {
			if !strings.HasPrefix(d.Code, "spx_unavailable:") {
				out = append(out, d)
			}
		}
		c.WarningDetails = out
	}
}

func filterGammaWarnings(in []string, keep func(string) bool) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:0]
	for _, code := range in {
		if keep(code) {
			out = append(out, code)
		}
	}
	return out
}

func spxFallbackReason(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	for _, code := range c.Warnings {
		if reason, ok := strings.CutPrefix(code, "spx_unavailable:"); ok {
			return reason
		}
	}
	for _, d := range c.WarningDetails {
		if reason, ok := strings.CutPrefix(d.Code, "spx_unavailable:"); ok {
			return reason
		}
	}
	return "previous_success"
}

// summarizeGammaErr returns browser-safe, allowlisted failure copy. Compute
// errors can contain broker and transport free text; raw causes remain in the
// daemon log and never become retry or warning payloads.
func summarizeGammaErr(err error) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(summarizeGammaPhaseFailure(err), "_", " ")
}

// remainingEta returns a refined ETA in seconds. Once enough work has
// landed (progress > 5), the estimate is projected from elapsed time:
// remaining ≈ elapsed × (100 - progress) / progress. Before then the
// projection is meaningless on tiny samples, so we fall back to the
// static initial estimate minus elapsed.
//
// Capped at 4× the static estimate so a stalled compute (progress
// frozen at 10 % after 10 minutes) doesn't surface absurd
// projections. Floor at 5s so the renderer doesn't flicker between
// "0s" and "computing" near the end of the run.
func remainingEta(g *gammaComputation, now time.Time, progress int32) int {
	elapsed := int(now.Sub(g.startedAt).Seconds())
	cap := 4 * g.etaSeconds
	var remaining int
	if progress > 5 {
		remaining = int(float64(elapsed) * float64(100-progress) / float64(progress))
	} else {
		remaining = g.etaSeconds - elapsed
	}
	remaining = min(remaining, cap)
	return max(remaining, 5)
}

// findZeroCrossing scans the sweep profile and returns the spot at
// that invariant.
//
//	and never negative (dealer book is long-gamma in every scenario
//	and never positive (short-gamma regime).
//	points, or every sample is exactly zero. The all-zero case usually
func findZeroCrossing(profile []rpc.GammaProfilePoint) (zeroGamma *float64, sign string) {
	if len(profile) < 2 {
		return nil, "no_data"
	}
	allPositive := true
	allNegative := true
	nonZero := false
	for _, p := range profile {
		if p.GEX < 0 {
			allPositive = false
			nonZero = true
		}
		if p.GEX > 0 {
			allNegative = false
			nonZero = true
		}
	}
	if !nonZero {
		return nil, "no_data"
	}
	if allPositive {
		return nil, "positive"
	}
	if allNegative {
		return nil, "negative"
	}
	// At this point at least one pair brackets the zero. Walk and
	// interpolate on the FIRST bracketing pair — for dealer-gamma the
	// sign function is monotone across the sweep range in practice
	// (no multi-cross), but if it ever isn't, the renderer's profile
	// chart will surface the anomaly and the user can investigate.
	for i := 1; i < len(profile); i++ {
		prev := profile[i-1]
		curr := profile[i]
		if (prev.GEX > 0 && curr.GEX < 0) || (prev.GEX < 0 && curr.GEX > 0) {
			// Linear interpolation: solve GEX(x) = 0 for x on the line
			x := prev.Spot - prev.GEX*(curr.Spot-prev.Spot)/(curr.GEX-prev.GEX)
			return &x, ""
		}
		// Exact zero at a sample point — interpolate degenerates to
		// the sample's own spot.
		if prev.GEX == 0 {
			x := prev.Spot
			return &x, ""
		}
		if i == len(profile)-1 && curr.GEX == 0 {
			x := curr.Spot
			return &x, ""
		}
	}
	// Shouldn't reach here given the allPositive/allNegative gates
	return nil, "no_data"
}
