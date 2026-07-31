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
	// (rpc.GammaZeroScope* constants). A nil/missing entry means no
	// compute has ever been kicked for that scope this session.
	// Each slot owns its own current/refresh/lastErr lifecycle so
	// the three scopes can't step on each other's state.
	slots map[string]*gammaSlot

	// store is the optional on-disk persistence layer. nil = pure
	// in-memory (tests use this). Set once at construction by
	// newGammaZeroCacheWithStore — never modified after, so reads
	// from the spawnJob goroutine don't need the cache mutex.
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
	// once after a successful job becomes the served result for its scope.
	// Failed, canceled, or superseded work never reaches it.
	onPublication func(scope string)

	// loadOnce gates the one-shot persisted-result read (see
	// newGammaZeroCacheWithStore for why it is deferred to first use).
	// loadNow is the construction wall time the load's session-key gate
	// evaluates against — the same instant the pre-lazy eager load used.
	loadOnce sync.Once
	loadNow  time.Time
}

const gammaColdCacheAction = "Run `canary gamma --force` for a diagnostic off-hours recompute, or call again during the next regular U.S. options session."

// gammaSlot is the per-scope cache cell. Mirrors the original
// single-slot fields of gammaZeroCache: current, refresh, plus the
// last-error retention that powers the "retry of X" rendering across
// the gammaErrorRetryTTL boundary. Each scope owns one slot.
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
	// context across the gammaErrorRetryTTL boundary. Without this,
	// a caller polling during a retry window sees Status=Computing
	// with no indication that the previous compute failed — the
	// error was silently discarded the moment kickOrJoin kicked a
	// fresh attempt. Cleared on a successful compute landing.
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
// period required before the next automatic attempt: 60 s, 2 m, 4 m,
// 8 m, then capped at gammaErrorRetryMaxTTL. The cap equals softTTLRTH,
// so post-outage recovery latency is no worse than the system's normal
// refresh cadence. The d <= 0 branch guards shift overflow on absurd
// streaks.
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
// (success or error) is finalised; readers Select on it to wait.
//
// progress is updated atomically by the compute goroutine; readers
// load it without holding any cache lock so a poller during a long
// compute doesn't block on the cache mutex.
//
// scope identifies which cache slot this computation belongs to, so
// snapshot() can look up the correct lastErr context without the
// caller having to pass it again. Mirrors sessionKey in being a
// stable property of the compute that the surrounding cache reads.
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
// softTTLRTH so a recovered gateway is picked up within one normal
// refresh window even at full escalation.
const gammaErrorRetryMaxTTL = 15 * time.Minute

// Session-aware soft TTL: the age at which a cached successful
// compute triggers a background refresh. The served value is still
// returned immediately; the refresh runs behind it and the next
// caller picks up the new result.
//
// Per-class values:
//   - softTTLRTH (15 min): during the regular U.S. listed-options session,
//     dealer gamma should be refreshed often enough for regime/Stress reads
//     to see intraday positioning changes without overlapping the
//     several-minute option fan-out.
//   - softTTLClosed (math.MaxInt64): outside that option-data session, a
//     non-force refresh is not expected to improve a good last-known snapshot.
//     The persisted result stays canonical until the next regular session.
//     Effectively infinite (math.MaxInt64 ~= 292 years as a time.Duration).
//
// Class transitions trigger an additional refresh path in kickOrJoin:
// a snapshot computed outside the regular option-data session is
// treated as stale at the RTH open even if its absolute age is below
// softTTL. This lets a forced pre-open diagnostic yield to a regular
// session refresh without blocking the first caller after the open.
//
// Distinct from gammaErrorRetryTTL (60 s): that one re-attempts a
// FAILED compute on the next call; soft-TTL rolls a SUCCESSFUL
// compute forward while still serving the prior value.
const (
	softTTLRTH    = 15 * time.Minute
	softTTLClosed = time.Duration(math.MaxInt64)
)

// softTTL returns the soft-TTL appropriate for the regular option-data
// session containing now. Caller passes the same now used for the age
// comparison so a single instant drives both the TTL choice and the age
// check, avoiding boundary-flap when the two sample times straddle a
// session edge.
func softTTL(now time.Time) time.Duration {
	if gammaClassifySession(now) == rpc.SessionClosed {
		return softTTLClosed
	}
	return softTTLRTH
}

// isDone reports whether the compute has finished (success or error).
// Safe for concurrent readers — the done channel close is the
// happens-before signal that the result / err fields are stable.
func (g *gammaComputation) isDone() bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

// successfulAt is the refresh-age anchor for a completed successful job.
// Fresh work gets its full soft-TTL quiet period after publication, not after
// kickoff. Persisted and older synthetic fixtures fall back to startedAt when
// no completion timestamp is available.
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
// hook. Production calls this before the server starts; the lock also keeps
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
// store. The store is consulted on first cache use — not at
// construction — for each known scope: any scope holding a result
// keyed to today's NY trading session has its slot seeded with a
// synthetic already-done gammaComputation, so the first caller for
// that scope skips the multi-minute compute.
//
// Lazy, not eager, because daemon.New runs before Server.Start
// acquires the single-instance lock: every autospawn race loser builds
// a full Server, and an eager load made each loser re-read the store
// and re-log "loaded persisted result" into the shared daemon log
// (2026-06-09: ~10 interleaved triples per spawn burst). Losers never
// serve a cache call, so the lazy gate keeps them off the store; the
// winning daemon still reads each scope exactly once.
//
// Per-scope independence is the load-bearing guarantee — a stale
// SPY-only file doesn't block a combined call from kicking, and vice
// versa.
//
// Persistence errors during load are warnings, not failures —
// a cache with empty slots is fine, the next caller for that scope
// kicks a fresh compute as it would with a pure in-memory cache.
//
// log may be nil; gammaLogf nil-safe-wraps it.
func newGammaZeroCacheWithStore(store *gammaZeroStore, now time.Time, log gammaLogger) *gammaZeroCache {
	return &gammaZeroCache{
		slots:   make(map[string]*gammaSlot, len(knownGammaScopes)),
		store:   store,
		log:     log,
		loadNow: now,
	}
}

// ensureLoaded runs the one-shot persisted read. Every externally
// called entry point (kickOrJoin, force, IsComputing, snapshot*)
// invokes it before taking c.mu; internal helpers and the
// promote/outcome goroutines are only reachable after one of those
// has run, so they never need the gate. Must not be called with c.mu
// held — loadPersisted takes the lock itself.
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
			// match, but serving the prior good result as explicit
			// context is better than going cold. During regular option
			// hours kickOrJoin refreshes behind this value; outside
			// regular option hours it is served without a non-force
			// refresh.
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
// gammaComputation that looks already-done to the rest of the cache.
// The done channel is closed immediately; result is set; err and
// cancel are zero-valued (cancel is only used to abort in-flight
// computes, which this isn't).
//
// scope is stored on the gammaComputation so snapshot() can route
// lastErr reads to the right slot without needing the caller to pass
// it again.
//
// Persisted results predate the in-memory completion timestamp. startedAt uses
// the result's decision AsOf, while completedAt uses the latest per-index AsOf
// as the closest retained publication boundary for a serial combined run. This
// makes the soft-TTL refresh trigger fire on the correct elapsed window — a
// result persisted 40 min ago should be refreshed in another ~20 min under
// the RTH TTL,
// not start a fresh countdown from boot time. Also lets the
// boundary-refresh path detect cached snapshots whose AsOf belongs
// to a different option-data session class than the current one (e.g.
// a forced pre-open diagnostic reloaded at 10:00 ET).
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
// Used by Server.isBusy() so the daemon's idle watcher doesn't shut
// down while a multi-minute compute is still running (a fresh
// `canary regime` call kicks gamma and returns immediately; the user
// can walk away and the compute should still complete). Safe for
// concurrent callers — read under c.mu, follows the same lock
// discipline as kickOrJoin.
//
// A soft-TTL refresh counts as in-flight even when current is done:
// the user can see "computing dealer zero-gamma" in `canary status`
// whenever any compute is happening, so they understand why the
// next call may briefly carry a stale AsOf.
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
// trading session a compute belongs to. Computed at every cache
// lookup so DST transitions and timezone surprises don't poison a
// cache key. Falls back to UTC date if the zone fails to load —
// guarantees a stable string under all conditions.
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
	// the existing current stays in place and the next caller can
	// trigger another refresh attempt past the soft TTL.
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
	// option OI plus model/IV ticks; during closed or thin extended
	// phases an automatic fan-out is not reliably better than a good
	// last-known snapshot. Serve any successful cached result we have,
	// even one whose sessionKey belongs to a prior NY trading date.
	// Freshness is the renderer's problem (snapshot stamps
	// cache_stale_off_hours past 24h so the user sees the age).
	//
	// No usable cache + closed gateway → return (nil, false) and
	// snapshot reports Cold rather than starting a doomed compute.
	//
	// Exception: a force()-kicked compute can be in flight even on a
	// closed session (force bypasses this gate by design — see force()).
	// Subsequent non-force callers must be able to join the existing job
	// instead of being told Cold while the compute runs, and a completed
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
	// last-known-good while the better same-session result computes behind it.
	// This avoids turning a production signal into "computing" at the exact
	// moment a cached context read is still better than no read. Errors and
	// in-flight prior-session jobs do not get this preservation path because
	// they are not known-good values.
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
		// block fresh attempts. Without this check, a one-shot
		// gateway-side timeout poisons every regime/gamma call for the
		// rest of the NY trading session. In-flight jobs (isDone=false)
		// always pass through to the shared singleflight regardless of
		// age; success results stay sticky for the whole session. The
		// gate escalates with the slot's consecutive-failure streak
		// (60 s first retry, doubling to a 15-min cap) so a daylong
		// outage costs ~30 attempts instead of ~700.
		if slot.current.isDone() && slot.current.err != nil && slot.retryAllowed(now) {
			// Retain the failure context so the next render of the
			// "computing" row can surface "retry of <error> at HH:MM:SS"
			// instead of silently switching to a clean Computing state.
			slot.rememberError(slot.current)
			job = c.startLocked(parent, scope, key, now, etaSeconds, compute)
			return job, true
		}
		// Soft-TTL: if the served value is stale and no refresh is
		// already running, kick one behind it. The caller still gets
		// slot.current immediately — refresh is fire-and-forget.
		//
		// Two triggers, both gated on the cached value being a clean
		// success with no refresh already in flight:
		//
		//  1. Boundary refresh: cached value was computed outside the
		//     regular option-data session and is first served during
		//     RTH. E.g. a forced pre-open diagnostic served at 09:31
		//     ET should yield to a regular-session refresh.
		//
		//  2. Age refresh: absolute age exceeds the per-class
		//     softTTL — 15 min in regular option hours.
		//
		// Both triggers are skipped when the current class is
		// SessionClosed: automatic refresh is not expected to improve
		// the signal. softTTL returns math.MaxInt64 for Closed so the
		// age check can never fire there, and the explicit
		// currentClass check below skips the boundary path.
		// retryAllowed also gates this path: once current is past the
		// soft TTL, the trigger condition stays true on every poll while
		// refreshes keep failing (a failed refresh never advances
		// current's successful publication) — the streak gate is what stops that from
		// respawning a doomed ~35 s compute per scheduler tick.
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
		// Callers that need to bypass cache (diagnostics) use force().
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
		// (no Save call on the err != nil path) — mirrors breadth's
		// MinCoverageFraction policy of "do not persist runs that
		// did not converge." Cancelled jobs do not persist either: a
		// result finalised while the job was being torn down (daemon
		// shutdown, force() supersede) must not overwrite the last
		// cleanly-computed snapshot on disk.
		//
		// Persistence runs off the cache mutex; c.store is set once
		// at construction and never modified, so reading it lock-
		// less is safe. Save errors degrade to warnings only — the
		// in-memory cache still serves callers correctly.
		if c.store != nil && bgCtx.Err() == nil {
			if saveErr := c.store.Save(scope, key, res); saveErr != nil {
				gammaLogf{inner: c.log}.Warnf("gamma cache: persist scope=%s: %v", scope, saveErr)
			}
		}
		// Skew-fit calibration journal, under the same cancellation
		// guard as Save: a force()-superseded or shutdown-torn result
		// must not enter the calibration set either.
		if c.skewDiag != nil && bgCtx.Err() == nil {
			if diagErr := c.skewDiag.append(time.Now(), scope, key, job.result); diagErr != nil {
				gammaLogf{inner: c.log}.Warnf("gamma skew diag: append scope=%s: %v", scope, diagErr)
			}
		}
		// Use the caller's clock origin plus elapsed wall time. Production gets
		// the real publication time, while tests that use synthetic dates stay
		// deterministic. This is deliberately after persistence/journaling: a
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
// job's kickoff time (matching the startedAt-based age semantics the
// gammaErrorRetryTTL comment documents); a success resets both. The
// completion goroutine is the single place a job transitions to done,
// so the accounting runs exactly once per job with no dedupe — and it
// counts current, refresh, and force jobs uniformly: each is one
// observation of gateway health. Cancelled jobs are skipped entirely;
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
// wrapper over spawnJob — kept distinct because most kick paths want
// the new job to become the canonical served value for the scope,
// while the soft-TTL refresh path holds the new job in slot.refresh
// until it lands cleanly.
func (c *gammaZeroCache) startLocked(parent context.Context, scope, key string, now time.Time, etaSeconds int, compute computeFn) *gammaComputation {
	job := c.spawnJob(parent, scope, key, now, etaSeconds, compute)
	slot := c.getOrCreateSlotLocked(scope)
	slot.current = job
	return job
}

// snapshot extracts a wire-shape envelope from the current job state.
// Always returns a populated Status; Result is set only on success.
//
// g == nil means no compute has ever been kicked this NY trading
// session — distinct from Computing (compute in flight). Pre-v0.27.9
// snapshot conflated these two under Computing, so a consumer
// couldn't tell "first caller of the day must kick" from "the kick
// is already running." Mirror of the v0.27.3 breadth Cold state.
//
// nowFn is injectable for tests — production callers pass time.Now.
func (c *gammaZeroCache) snapshot(g *gammaComputation, nowFn func() time.Time) rpc.GammaZeroSPXResult {
	return c.snapshotForScope("", g, nowFn)
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
			return env
		}
		env.Status = rpc.GammaZeroStatusReady
		env.Result = g.result
		// A served last-good says nothing about whether its replacement is
		// running: StartedAt belongs to the compute that produced Result. The
		// typed flag is what lets a consumer tell a current-session compute in
		// flight from one that never started.
		env.Refreshing = c.refreshInFlight(gammaSnapshotScope(scope, g))
		// Off-hours stale tag: when we're serving a cached result
		// outside trading hours and it's more than 24h old, append
		// the `cache_stale_off_hours` warning so the renderer can
		// say "computed Nh ago" loudly instead of presenting an
		// overnight-old reading as if it were fresh. Copy-on-write
		// to avoid mutating the shared cache pointer that other
		// concurrent snapshots may still be reading.
		now := nowFn()
		if g.result != nil && gammaClassifySession(now) == rpc.SessionClosed && now.Sub(g.result.AsOf) > 24*time.Hour {
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
	// uses this to display "computing · retry of <summary> at
	// HH:MM:SS" instead of dropping the prior error silently. Reads
	// from the slot keyed by g.scope so the spy/spx/combined retry
	// contexts stay separate.
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
// which the signed GEX crosses zero, via linear interpolation between
// the two adjacent points that bracket the crossing. The profile is
// assumed sorted by Spot ascending — the caller is responsible for
// that invariant.
//
// Returns:
//   - (price, "") on a clean crossing.
//   - (nil, "positive") when GEX is positive across at least one sample
//     and never negative (dealer book is long-gamma in every scenario
//     considered).
//   - (nil, "negative") when GEX is negative across at least one sample
//     and never positive (short-gamma regime).
//   - (nil, "no_data") when the profile is empty, has fewer than two
//     points, or every sample is exactly zero. The all-zero case usually
//     means landed IV legs carried no usable OI/gamma magnitude; treating
//     it as long-gamma would be a false regime call.
//
// The interpolation is intentionally linear: dealer GEX is smooth
// over a 30 %-wide spot sweep, but the sweep itself is sampled at
// 60 evenly-spaced points, so a finer-grain interp model would imply
// precision the sampling doesn't support. Linear is honest.
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
			// through (prev.Spot, prev.GEX) and (curr.Spot, curr.GEX).
			//   x = prev.Spot - prev.GEX × (curr.Spot - prev.Spot) / (curr.GEX - prev.GEX)
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
	// above, but defensive: no crossing found despite mixed signs.
	return nil, "no_data"
}
