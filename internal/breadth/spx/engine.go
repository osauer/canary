package spx

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"
)

// windowCheckpointBatchSize bounds how much successful fan-out work a daemon
// restart can discard. At the production 0.1 request/second paced rate, ten
// names are roughly 100 seconds of progress. Checkpoints replace only the
// current state document; finalise records the canonical observation once.
const windowCheckpointBatchSize = 10

// Logger is the minimal logging surface the engine needs. The daemon
// passes its standard logger; tests can pass nil to silence output.
type Logger interface {
	Warnf(format string, args ...any)
	Infof(format string, args ...any)
}

// Options configures Engine construction. All fields are optional —
// the zero value picks sensible defaults documented per-field.
type Options struct {
	// Logger receives non-fatal refresh events (per-symbol fetch
	// errors, persistence failures). nil silences all logging — fine
	// for tests, not recommended for production.
	Logger Logger
	// Clock injects a synthetic time source for tests. Production
	// callers pass nil and get time.Now.
	Clock func() time.Time
	// Workers caps refresh concurrency. Each worker calls
	// BarFetcher.FetchDaily for one symbol at a time. Defaults to 6,
	// matching the IBKR-side historical-data pacing headroom. Setting
	// to 1 serialises fetches — useful in tests that want
	// deterministic ordering.
	Workers int
	// ColdLookbackDays is how many trailing daily bars to fetch for
	// a name with no cached history. Defaults to WindowSize + 10 to
	// absorb holiday gaps in the trailing 50 trading days.
	ColdLookbackDays int
	// WarmLookbackDays is how many trailing daily bars to fetch for
	// a name whose cached window is current except for today.
	// Defaults to 2 — today's bar plus one for duplicate-detection
	// during the same-session retry path.
	WarmLookbackDays int
	// Members lets the caller seed the engine with a non-embedded
	// constituent list. nil/empty falls back to MembersFn when set,
	// else to MemberList()'s embedded list, preserving every existing
	// caller.
	Members []string
	// MembersFn defers constituent-list resolution to first actual
	// use. When Members is empty and MembersFn is non-nil, the engine
	// calls it exactly once — behind a sync.Once, from the first
	// operation that touches the list (Refresh, Members, SetMembers) —
	// instead of resolving at construction. The daemon uses this to
	// keep the persisted-members read and its INFO log line out of
	// daemon.New, which runs before Server.Start acquires the
	// single-instance lock: autospawn race losers build a full Server
	// but never serve a call, so a deferred load keeps them off the
	// persistence authority and out of the shared log. An empty return falls back
	// to MemberList()'s embedded list. Ignored when Members is set.
	MembersFn func() []string
	// DeferStoreLoad constructs the engine cold without reading Store. The
	// daemon uses this before it owns the persistence lock, then calls
	// Engine.UseCoreStore to attach and load daemon.db before serving. Legacy
	// and isolated callers keep the historical eager-load default.
	DeferStoreLoad bool
}

// Engine is the breadth-spx state machine: it loads persisted state, drives a
// background refresh against a BarFetcher when
// asked, and serves the most recent Snapshot to callers. Safe for
// concurrent use.
//
// Lifecycle:
//   - New() loads persisted state. If the cache is fresh, Get()
//     returns it immediately and no fetch is needed.
//   - Refresh(ctx) is the long-running operation. Serialised against
//     concurrent calls (the second caller waits behind the first).
//   - Get() / Status() are fast read-only views; safe to call during
//     a Refresh in progress.
//
// State is held in memory and successful window progress is persisted in
// bounded batches during a refresh, then once more when the pass completes. A
// crash mid-refresh therefore resumes from the last committed daemon.db
// checkpoint without publishing an incomplete snapshot.
type Engine struct {
	store   *Store
	fetcher BarFetcher
	logger  Logger
	clock   func() time.Time

	workers      int
	coldLookback int
	warmLookback int

	// mu protects the in-memory state below. Held briefly for read
	// (Get) or for the swap-after-refresh; never held during a
	// long-running fetch.
	mu       sync.RWMutex
	snapshot *Snapshot
	windows  map[string]ConstituentWindow
	history  []HistoryPoint
	members  []string
	// membersFn / membersOnce implement the deferred resolution
	// documented on Options.MembersFn. membersFn is set once at
	// construction and never modified after; membersOnce gates its
	// single invocation (see ensureMembers). nil membersFn means the
	// list was resolved eagerly and e.members is already final.
	membersFn   func() []string
	membersOnce sync.Once
	// lastCoverage / lastMemberCount record the result of the most
	// recent finalise(). The scheduler reads these to decide whether
	// to retry sooner than the daily tick when coverage is below
	// MinCoverageFraction. Both stay at zero until the first refresh
	// completes — Engine.LastRefreshCoverage() returns these directly.
	lastCoverage    int
	lastMemberCount int

	// refreshMu serialises concurrent Refresh() calls. The second
	// caller waits behind the first rather than launching a
	// duplicate fan-out, which would just compete for the same
	// fetcher slots.
	refreshMu sync.Mutex
	// refreshing is set true while a Refresh is in flight. Readers
	// take mu.RLock to inspect — distinct from refreshMu so a poller
	// during a long refresh doesn't block on the fetch loop.
	refreshing bool
	// retryPending is set while Run is sleeping between below-threshold
	// bootstrap/catch-up refresh attempts. No fetch is in flight during
	// that wait, but the scheduler is still actively trying to converge
	// the withheld snapshot; daemon idle shutdown must not kill the
	// process in that gap.
	retryPending bool
	// progress is the current or most recently completed refresh attempt. It
	// is served as redacted typed metadata so operators can distinguish a
	// normally advancing paced pass from a stuck or failed one.
	progress RefreshProgress
}

// New constructs an Engine. Loads any persisted state from store
// (best effort — a corrupted or missing cache results in a cold
// start, not an error). Members come from Options: an explicit
// Members list, a deferred MembersFn resolver, or the checked-in
// embedded list in members_data.go as the fallback. Runtime updates
// arrive only via SetMembers (the daemon's members refresher).
func New(store *Store, fetcher BarFetcher, opts Options) *Engine {
	if store == nil {
		panic("spx.New: store is required")
	}
	if fetcher == nil {
		panic("spx.New: fetcher is required")
	}
	members := opts.Members
	membersFn := opts.MembersFn
	if len(members) > 0 {
		membersFn = nil // explicit list wins; nothing left to defer
	} else if membersFn == nil {
		members, _ = MemberList()
	}
	members = slices.Clone(members)
	e := &Engine{
		store:        store,
		fetcher:      fetcher,
		logger:       opts.Logger,
		clock:        opts.Clock,
		workers:      opts.Workers,
		coldLookback: opts.ColdLookbackDays,
		warmLookback: opts.WarmLookbackDays,
		windows:      map[string]ConstituentWindow{},
		members:      members,
		membersFn:    membersFn,
	}
	if e.clock == nil {
		e.clock = time.Now
	}
	if e.workers <= 0 {
		e.workers = 6
	}
	if e.coldLookback <= 0 {
		// RollingMaxBars + 10 trading-day pad. Pulling 262 bars in one
		// fetch costs the same per-IBKR-request as pulling 60 (the
		// pacing limit is 60 historical requests per 10-minute window,
		// independent of the bar count), so v2 fetches enough history
		// to seed all three reads — 50-DMA, 200-DMA, and the rolling
		// 252-bar max/min — from a single per-constituent fetch. Names
		// with thinner real-world history (recent IPOs, recent index
		// adds) come back short and are skipped from the appropriate
		// numerator-denominator pairs in Compute.
		e.coldLookback = RollingMaxBars + 10
	}
	if e.warmLookback <= 0 {
		e.warmLookback = 2
	}

	if !opts.DeferStoreLoad {
		// Best-effort legacy/standalone load. Errors are logged but never
		// propagated — the engine remains useful on cold start.
		if snap, err := store.LoadSnapshot(); err != nil {
			e.warnf("breadth: load snapshot: %v", err)
		} else if snap != nil {
			e.snapshot = snap
		}
		if windows, err := store.LoadWindows(); err != nil {
			e.warnf("breadth: load windows: %v", err)
		} else if windows != nil {
			e.windows = windows
		}
		if hist, err := store.LoadHistory(); err != nil {
			e.warnf("breadth: load history: %v", err)
		} else {
			e.history = hist
		}
	}
	return e
}

// Get returns the most recent successful snapshot, or (nil, false) if
// the engine hasn't computed one yet (cold start). Fast: holds only
// a read lock; safe during an in-flight Refresh.
//
// The returned snapshot is a defensive copy — the Excluded slice is
// cloned so a caller iterating its result cannot race against an
// in-flight refresh that's appending exclusions to the engine's
// canonical state.
func (e *Engine) Get() (*Snapshot, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.snapshot == nil {
		return nil, false
	}
	snap := *e.snapshot
	snap.Excluded = slices.Clone(e.snapshot.Excluded)
	return &snap, true
}

// IsRefreshing reports whether a Refresh is currently in flight. The
// daemon's handleBreadthSPX uses this to decide between returning a
// cached snapshot and surfacing status="computing".
func (e *Engine) IsRefreshing() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.refreshing
}

// IsBusy reports whether the engine has refresh work in progress or a
// scheduled below-threshold retry that should keep the owning daemon alive.
//
// A breadth cold-start often converges over multiple refresh attempts as
// IBKR's contract-details bucket refills. The refresh itself may finish
// quickly, then Run sleeps belowThresholdRetryDelay before continuing. From
// the daemon's lifecycle point of view that sleep is still active bootstrap
// work, even though IsRefreshing is false.
func (e *Engine) IsBusy() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.refreshing || e.retryPending
}

// Progress returns the current or most recently completed refresh attempt.
// The bool is false before the engine has started its first pass.
func (e *Engine) Progress() (RefreshProgress, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.progress, !e.progress.StartedAt.IsZero()
}

func (e *Engine) beginRefreshProgress(total int) {
	now := e.clock()
	progress := RefreshProgress{
		SessionKey: CompletedSessionKey(now),
		StartedAt:  now,
		Total:      total,
	}
	progress.Deadline, _ = PublicationDeadline(progress.SessionKey)
	e.mu.Lock()
	e.progress = progress
	e.mu.Unlock()
}

func (e *Engine) recordRefreshProcessed(failure RefreshFailure) {
	e.mu.Lock()
	e.progress.Processed++
	if failure != "" {
		e.progress.LastFailure = failure
	}
	e.mu.Unlock()
}

func (e *Engine) recordRefreshFailure(failure RefreshFailure) {
	if failure == "" {
		return
	}
	e.mu.Lock()
	e.progress.LastFailure = failure
	e.mu.Unlock()
}

func (e *Engine) setRetryPending(pending bool) {
	e.mu.Lock()
	e.retryPending = pending
	e.mu.Unlock()
}

// MarkPendingBootstrap pre-sets refreshing=true if Run() would fire a
// bootstrap refresh on entry — i.e. iff shouldRefreshOnStartup is true
// against the current snapshot and clock. The caller MUST spawn Run()
// immediately after; otherwise the flag stays true forever.
//
// The point is to close the race in postConnectSetup where the daemon
// reports Connected=true (handshake done) before `go e.Run()` has
// scheduled and called Refresh — a status RPC in that window would
// otherwise see Connected=true but no breadth-spx background task,
// even though one is about to fire. Refresh() itself sets refreshing
// to true again under e.mu (idempotent) and clears it via defer, so
// the canonical lifetime tracking inside Refresh stays unchanged.
//
// No-op when no bootstrap would fire (snapshot is fresh) — that's
// also the correctness guard against a stuck flag.
func (e *Engine) MarkPendingBootstrap() {
	cur, _ := e.Get()
	if !shouldRefreshOnStartup(cur, e.clock()) {
		return
	}
	e.mu.Lock()
	e.refreshing = true
	e.mu.Unlock()
}

// Refresh runs one pass of the constituent-fanout compute: decide
// which names need new bars, fetch them in parallel, slide each
// window forward, recompute S5FI, persist.
//
// Cold start is ~74 min wall-clock: IBKR's historical-data pacing
// limit caps each gateway connection at 60 requests per 10-minute
// sliding window, so 503 constituents land at ~6 names/min sustained
// after the initial 60-name burst. Adding workers above the default
// 6 doesn't help — the gateway throttles the second any pacing
// budget is exceeded. Warm refresh (same daemon, populated cache) is
// ~1–10 min: only today's bar per name needs fetching.
//
// Concurrent calls serialise via refreshMu — the second caller waits
// behind the first and then sees the updated snapshot. A returned
// error means the compute didn't complete; partial fetch failures
// (some names succeeded, some didn't) do NOT return an error — they
// surface as Excluded entries in the resulting Snapshot.
func (e *Engine) Refresh(ctx context.Context) error {
	e.ensureMembers()
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()

	e.mu.Lock()
	e.refreshing = true
	members := slices.Clone(e.members)
	cached := maps.Clone(e.windows)
	if cached == nil {
		cached = map[string]ConstituentWindow{}
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.refreshing = false
		e.mu.Unlock()
	}()

	plan := e.planFetches(members, cached)
	e.beginRefreshProgress(len(plan))
	if len(plan) == 0 {
		// Nothing to fetch — recompute against cached windows so the
		// snapshot timestamp moves forward even on a no-op refresh.
		return e.finalise(members, cached)
	}

	fetchErrs := e.execute(ctx, plan, cached)
	if ctx.Err() != nil {
		e.recordRefreshFailure(RefreshFailureCancelled)
		return ctx.Err()
	}

	for sym, err := range fetchErrs {
		e.warnf("breadth: fetch %s: %v", sym, err)
	}

	return e.finalise(members, cached)
}

// fetchPlan is the per-symbol decision the refresh planner makes.
type fetchPlan struct {
	Symbol       string
	LookbackDays int
}

// planFetches walks the membership list and decides what to fetch
// for each name based on the cached window. A name absent from the
// cache or with no closes triggers a cold fetch (ColdLookbackDays);
// a name whose latest bar is the latest completed US-equity session is
// skipped entirely; everything else gets the warm two-bar increment.
//
// Date comparison uses calendar-backed session keying so weekends,
// holidays, pre-close hours, and UTC-midnight drift don't trigger a
// same-data fanout under a new wall-clock date.
func (e *Engine) planFetches(members []string, cached map[string]ConstituentWindow) []fetchPlan {
	targetSession := CompletedSessionKey(e.clock())
	plan := make([]fetchPlan, 0, len(members))
	for _, sym := range members {
		w, ok := cached[sym]
		if !ok || len(w.Closes) == 0 {
			plan = append(plan, fetchPlan{Symbol: sym, LookbackDays: e.coldLookback})
			continue
		}
		if w.LastBarAt == targetSession {
			// Already have the latest completed close — nothing to fetch.
			continue
		}
		plan = append(plan, fetchPlan{Symbol: sym, LookbackDays: e.warmLookback})
	}
	return plan
}

// execute runs the fetch plan in parallel with bounded concurrency. Successful
// bars are merged into windows immediately and the full window projection is
// checkpointed after each small batch. The snapshot and history are not
// touched here: finalise remains the only publication gate, so a checkpoint
// can resume after restart without exposing below-threshold breadth.
//
// The final partial batch is checkpointed even when ctx was cancelled. A
// graceful daemon stop therefore keeps every completed name; an abrupt crash
// loses at most windowCheckpointBatchSize-1 names.
func (e *Engine) execute(ctx context.Context, plan []fetchPlan, windows map[string]ConstituentWindow) map[string]error {
	errs := make(map[string]error)

	var mu sync.Mutex
	dirty := 0
	checkpointLocked := func() {
		if dirty == 0 {
			return
		}
		checkpoint := maps.Clone(windows)
		if err := e.store.checkpointWindows(checkpoint, e.clock()); err != nil {
			e.warnf("breadth: checkpoint windows: %v", err)
			e.recordRefreshFailure(RefreshFailurePersist)
			return
		}
		e.mu.Lock()
		e.windows = checkpoint
		e.mu.Unlock()
		dirty = 0
	}
	sem := make(chan struct{}, e.workers)
	var wg sync.WaitGroup

dispatch:
	for _, item := range plan {
		// Acquire one slot or bail if ctx fires first. Labelled break
		// because plain `break` would only exit the select.
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-sem }()
			bars, err := e.fetcher.FetchDaily(ctx, item.Symbol, item.LookbackDays)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[item.Symbol] = err
				failure := RefreshFailureFetch
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					failure = RefreshFailureCancelled
				}
				e.recordRefreshProcessed(failure)
				return
			}
			merged := mergeBars(windows[item.Symbol], bars, item.Symbol)
			if constituentWindowsEqual(windows[item.Symbol], merged) {
				e.recordRefreshProcessed("")
				return
			}
			windows[item.Symbol] = merged
			dirty++
			if dirty >= windowCheckpointBatchSize {
				checkpointLocked()
			}
			e.recordRefreshProcessed("")
		})
	}
	wg.Wait()
	mu.Lock()
	checkpointLocked()
	mu.Unlock()
	return errs
}

func constituentWindowsEqual(a, b ConstituentWindow) bool {
	return a.Symbol == b.Symbol &&
		a.LastBarAt == b.LastBarAt &&
		a.HighRollingMax == b.HighRollingMax &&
		a.HighRollingBarsHad == b.HighRollingBarsHad &&
		a.LowRollingMin == b.LowRollingMin &&
		a.LowRollingBarsHad == b.LowRollingBarsHad &&
		slices.Equal(a.Closes, b.Closes)
}

// finalise computes a snapshot from the (possibly updated) windows
// and persists what's safe to persist for the achieved coverage:
//
//   - Windows are persisted unconditionally (in-memory and on-disk).
//     Per-name daily closes are authoritative even when partial — the
//     mergeBars step in Refresh never overwrites valid closes with
//     empty ones — and persisting them lets each refresh attempt
//     build on the previous one's gains. Without this, a 503-name
//     cold start that's bottlenecked on IBKR's per-account
//     reqContractDetails budget (~50 / 10 min, observed) can never
//     converge: each refresh tick starts from zero and re-attempts
//     the same names while IBKR's bucket is still draining.
//
//   - Snapshot and history are gated at MinCoverageFraction. Those
//     are the published surface (Get() reads snapshot, History()
//     reads history); a partial snapshot would mislead any consumer
//     that reads the cached value and would poison the scheduler's
//     "today's snapshot exists, skip the next bootstrap" check. The
//     0.80 threshold tolerates ordinary per-name fetch errors
//     (delisted tickers, transient pacing) while rejecting
//     catastrophic fan-out failures.
//
// History is appended idempotently — a re-refresh in the same NY
// session overwrites the existing point rather than duplicating it,
// matching the SlideWindow same-day semantics. Persistence failures
// are logged but not fatal.
//
// finalise returns nil on a below-threshold result. The scheduler
// reads e.LastRefreshCoverage to decide whether to retry sooner than
// the daily cadence, but the contract here is "no error means I
// finished — whether convergence happened is a separate signal."
func (e *Engine) finalise(members []string, windows map[string]ConstituentWindow) error {
	now := e.clock()
	sessionKey := CompletedSessionKey(now)
	snap := Compute(members, windows, sessionKey, now)

	minCoverage := int(MinCoverageFraction * float64(snap.MemberCount))

	// Persist windows unconditionally — see docstring above for why.
	if err := e.store.SaveWindows(windows, now); err != nil {
		e.warnf("breadth: save windows: %v", err)
		e.recordRefreshFailure(RefreshFailurePersist)
	}
	e.mu.Lock()
	e.windows = windows
	e.lastCoverage = snap.Coverage
	e.lastMemberCount = snap.MemberCount
	e.mu.Unlock()

	if snap.Coverage < minCoverage {
		e.warnf("breadth: refresh coverage %d/%d below threshold %d (%.0f%% of %d); windows persisted for next-tick continuation, snapshot withheld until convergence",
			snap.Coverage, snap.MemberCount, minCoverage, MinCoverageFraction*100, snap.MemberCount)
		return nil
	}

	// Convergence — publish the snapshot and history.
	e.mu.Lock()
	history := appendHistory(e.history, HistoryPoint{
		Date:           sessionKey,
		PctAbove50DMA:  snap.PctAbove50DMA,
		PctAbove200DMA: snap.PctAbove200DMA,
		NewHighs:       snap.NewHighsToday,
		NewLows:        snap.NewLowsToday,
	})
	e.mu.Unlock()

	if err := e.store.SaveSnapshot(snap); err != nil {
		e.warnf("breadth: save snapshot: %v", err)
		e.recordRefreshFailure(RefreshFailurePersist)
	}
	if err := e.store.SaveHistory(history); err != nil {
		e.warnf("breadth: save history: %v", err)
		e.recordRefreshFailure(RefreshFailurePersist)
	}

	e.mu.Lock()
	e.snapshot = &snap
	e.history = history
	e.mu.Unlock()
	return nil
}

// LastRefreshCoverage returns (coverage, memberCount) from the most
// recent finalise. Zero values indicate "no refresh has completed yet"
// — distinct from "refresh completed with zero coverage" because the
// scheduler treats the latter as a signal to retry, not give up.
//
// The scheduler reads this to decide whether the previous refresh
// converged (coverage ≥ MinCoverageFraction × memberCount) or whether
// to schedule a retry sooner than the daily cadence.
func (e *Engine) LastRefreshCoverage() (coverage, memberCount int) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastCoverage, e.lastMemberCount
}

// appendHistory adds today's point to the rolling series, collapsing
// the same-session retry case (already saw this date, overwrite the
// existing entry) and trimming to MaxHistoryPoints from the head.
// Idempotent — calling twice with the same point leaves the series
// unchanged.
func appendHistory(existing []HistoryPoint, point HistoryPoint) []HistoryPoint {
	out := slices.Clone(existing)
	if n := len(out); n > 0 && out[n-1].Date == point.Date {
		// Same-session re-refresh: overwrite the tail rather than
		// appending. Late prints or a forced re-run shouldn't widen
		// the series.
		out[n-1] = point
		return out
	}
	out = append(out, point)
	if len(out) > MaxHistoryPoints {
		out = out[len(out)-MaxHistoryPoints:]
	}
	return out
}

// mergeBars folds a list of fetched bars into an existing window.
// Each bar slides the window forward one day (or overwrites the
// tail if same-day). Duplicate dates between the fetched series and
// the existing window are collapsed — the cache stays clean even
// when the daily warm-fetch overlaps yesterday's bar.
func mergeBars(w ConstituentWindow, bars []Bar, symbol string) ConstituentWindow {
	if w.Symbol == "" {
		w.Symbol = symbol
	}
	for _, b := range bars {
		if w.LastBarAt != "" && b.Date <= w.LastBarAt && b.Date != w.LastBarAt {
			// Older than the last cached bar — ignore. The cache is
			// the source of truth for historical closes; a fetcher
			// that re-emits past dates shouldn't rewrite history.
			continue
		}
		w = SlideWindow(w, b.Close, b.Date)
	}
	return w
}

// nySessionKey returns the NY-tz date string identifying today's
// trading session. Mirrors gammaZeroCache.nySessionKey to keep
// session keys consistent across both indicator caches — they
// should agree on "what session am I in?" for the same wall-clock
// time. Falls back to UTC date if the zone fails to load.
func nySessionKey(now time.Time) string {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return now.In(loc).Format("2006-01-02")
	}
	return now.UTC().Format("2006-01-02")
}

// warnf is a nil-safe Logger.Warnf wrapper. The engine's logger is
// optional; nil silences all output (used in tests).
func (e *Engine) warnf(format string, args ...any) {
	if e.logger != nil {
		e.logger.Warnf(format, args...)
	}
}

// ensureMembers runs the deferred Options.MembersFn resolution. Every
// operation that touches the constituent list (Refresh, Members,
// SetMembers) invokes it before taking e.mu; the read-only paths
// (Get, Status, IsBusy, History) never need the list, so they stay
// gate-free and a daemon that only polls state never pays the load.
// No-op when the list was resolved eagerly at construction. Must not
// be called with e.mu held — the resolved list is installed under the
// lock here.
func (e *Engine) ensureMembers() {
	if e.membersFn == nil {
		return
	}
	e.membersOnce.Do(func() {
		members := e.membersFn()
		if len(members) == 0 {
			members, _ = MemberList()
		}
		e.mu.Lock()
		e.members = slices.Clone(members)
		e.mu.Unlock()
	})
}

// Members returns the constituent list the engine is currently using.
// Defensive copy: callers cannot mutate engine state by editing the
// returned slice. Used by the daemon's status renderer to surface
// member count, and by the runtime refresher to compare its newly
// fetched list against the in-process snapshot before swapping.
func (e *Engine) Members() []string {
	e.ensureMembers()
	e.mu.RLock()
	defer e.mu.RUnlock()
	return slices.Clone(e.members)
}

// SetMembers swaps the constituent list. Returns true when the new
// list differs from the existing one (caller may want to invalidate
// downstream state); returns false when the lists are identical
// (no-op, no need to touch the cache or kick a recompute).
//
// On change, the in-memory windows map is NOT cleared: names dropped
// from the list become irrelevant to Compute (which iterates over
// members), and names added are picked up by the next Refresh which
// sees them missing from cached and triggers a cold fetch. The
// existing windows for surviving names stay warm — a reconstitution
// of 1-3 names per quarter shouldn't invalidate ~500 cached windows.
//
// New constituents will be excluded from the next Compute pass with
// Reason="thin_history" until their cold-fetch lands and their
// window populates. Per design decision (b) — "pending until 50d
// accrue" — full inclusion in the breadth reading is deferred until
// the new name has 50 trading days of post-inclusion history. Today
// the engine doesn't track per-symbol inclusion dates, so the
// approximation we ship is: the name appears in the exclusion list
// as "thin_history" until its window naturally exceeds WindowSize.
// A follow-up can add per-symbol inclusion dates and the strict
// "exclude for 50d regardless of bar count" semantics.
func (e *Engine) SetMembers(members []string) bool {
	// Resolve the deferred list first so a refresher push can't be
	// clobbered by a later lazy load — once the Once has fired, the
	// swap below is the newest write and stays final.
	e.ensureMembers()
	e.mu.Lock()
	defer e.mu.Unlock()
	if slices.Equal(e.members, members) {
		return false
	}
	e.members = slices.Clone(members)
	return true
}

// History returns up to `limit` trailing history points, oldest first.
// A non-positive limit returns the full retained series (bounded by
// MaxHistoryPoints). The returned slice is a defensive copy — callers
// can mutate freely without affecting engine state.
func (e *Engine) History(limit int) []HistoryPoint {
	e.mu.RLock()
	defer e.mu.RUnlock()
	src := e.history
	if limit > 0 && limit < len(src) {
		src = src[len(src)-limit:]
	}
	return slices.Clone(src)
}
