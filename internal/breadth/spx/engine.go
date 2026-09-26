package spx

import (
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// windowCheckpointBatchSize bounds how much successful fan-out work a daemon
// restart can discard. At the production 0.1 request/second paced rate, ten
// names are roughly 100 seconds of progress. Checkpoints replace only the
// current state document; finalise records the canonical observation once.
const windowCheckpointBatchSize = 10

// Logger is the minimal logging surface the engine needs. The daemon
type Logger interface {
	Warnf(format string, args ...any)
	Infof(format string, args ...any)
}

// Options configures Engine construction. All fields are optional —
type Options struct {
	// Logger receives non-fatal refresh events (per-symbol fetch
	// errors, persistence failures). nil silences all logging — fine
	Logger Logger
	// Clock injects a synthetic time source for tests. Production
	Clock func() time.Time
	// Workers caps refresh concurrency. Each worker calls
	// matching the IBKR-side historical-data pacing headroom. Setting
	Workers int
	// ColdLookbackDays is a calendar-day request for uncached history.
	// The default 400 days covers 253 sessions; the broker rounds it to 2 Y.
	ColdLookbackDays int
	// WarmLookbackDays is the minimum calendar-day request for
	// a name whose cached window is current except for today.
	// Defaults to 2 — today's bar plus one for duplicate-detection
	// during the same-session retry path.
	WarmLookbackDays int
	// Members lets the caller seed the engine with a non-embedded
	Members []string
	// MembersFn defers constituent-list resolution to first actual
	// calls it exactly once — behind a sync.Once, from the first
	// single-instance lock: autospawn race losers build a full Server
	// but never serve a call, so a deferred load keeps them off the
	// persistence authority and out of the shared log. An empty return falls back
	MembersFn func() []string
	// DeferStoreLoad constructs the engine cold without reading Store. The
	// Engine.UseCoreStore to attach and load daemon.db before serving. Legacy
	DeferStoreLoad bool
	// HealthGate is consulted before and during refresh. Ordinary errors
	// refuse every read; RecoveryGateError allows bounded serial recovery
	// reads while preserving the warning. The gate must be cheap.
	HealthGate func() error
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
	lastCoverage    int
	lastMemberCount int

	// refreshMu serialises concurrent Refresh() calls. The second
	refreshMu sync.Mutex
	// refreshing is set true while a Refresh is in flight. Readers
	refreshing bool
	// retryPending is set while Run is sleeping between below-threshold
	// bootstrap/catch-up refresh attempts. No fetch is in flight during
	// that wait, but the scheduler is still actively trying to converge
	// the withheld snapshot; daemon idle shutdown must not kill the
	// process in that gap.
	retryPending bool
	// progress is the current or most recently completed refresh attempt. It
	// normally advancing paced pass from a stuck or failed one.
	progress RefreshProgress
	// nextAttempt is when the scheduler will next try a refresh; zero until
	// Run's first sleep.
	nextAttempt time.Time
	// definitionMisses maps a symbol the broker refused with ErrNoDefinition
	// to the session key that refresh targeted; planFetches skips the name
	// while that session is still the target.
	definitionMisses map[string]string

	healthGate func() error
	// Only Refresh holds these, under refreshMu. A failure admits one new
	// trial after backoff; success permits the next serial request, never
	// certifying all constituents or clearing the provider's warning.
	recoveryRetryAt  time.Time
	recoveryFailures int
	recoveryAfter    string
	// kick wakes a sleeping Run immediately (capacity 1, non-blocking send).
	// The daemon fires it when the bulk lane finishes a rebuild, so recovery
	// does not wait out whatever delay the scheduler was sleeping on.
	kick chan struct{}
}

// New constructs an Engine. Loads any persisted state from store
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

		definitionMisses: map[string]string{},
		membersFn:        membersFn,
		healthGate:       opts.HealthGate,
		kick:             make(chan struct{}, 1),
	}
	if e.clock == nil {
		e.clock = time.Now
	}
	if e.workers <= 0 {
		e.workers = 6
	}
	if e.coldLookback <= 0 {
		// HMDS duration is calendar days. Cover 253 sessions including
		// weekends, exchange holidays and a modest publication margin.
		e.coldLookback = 400
	}
	if e.warmLookback <= 0 {
		e.warmLookback = 2
	}

	if !opts.DeferStoreLoad {
		// Best-effort legacy/standalone load. Errors are logged but never
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
func (e *Engine) IsRefreshing() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.refreshing
}

// IsBusy reports whether the engine has refresh work in progress or a
// IBKR's contract-details bucket refills. The refresh itself may finish
func (e *Engine) IsBusy() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.refreshing || e.retryPending
}

// Progress returns the current or most recently completed refresh attempt.
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
		e.progress.Failed++
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

// CoverageShort reports whether the most recently completed refresh pass
// stayed below the publication threshold — the engine is serving its last
// good snapshot while the scheduler keeps trying to converge a fresher one.
func (e *Engine) CoverageShort() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastMemberCount > 0 && e.lastCoverage < int(MinCoverageFraction*float64(e.lastMemberCount))
}

func (e *Engine) setNextAttempt(at time.Time) {
	e.mu.Lock()
	e.nextAttempt = at
	e.mu.Unlock()
}

// NextAttempt reports when the scheduler will next try a refresh. Zero/false
// until Run has slept once.
func (e *Engine) NextAttempt() (time.Time, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.nextAttempt, !e.nextAttempt.IsZero()
}

// Kick wakes a sleeping Run immediately instead of letting it finish a
// transport or retry delay. Non-blocking; safe from any goroutine.
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// MarkPendingBootstrap pre-sets refreshing=true if Run() would fire a
// against the current snapshot and clock. The caller MUST spawn Run()
// The point is to close the race in postConnectSetup where the daemon
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
// Cold start is ~74 min wall-clock: IBKR's historical-data pacing
// ~1–10 min: only today's bar per name needs fetching.
// error means the compute didn't complete; partial fetch failures
func (e *Engine) Refresh(ctx context.Context) error {
	e.ensureMembers()
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()

	var initialGate error
	if e.healthGate != nil {
		initialGate = e.healthGate()
		if initialGate != nil && (!recoverableGate(initialGate) || e.clock().Before(e.recoveryRetryAt)) {
			e.beginRefreshProgress(0)
			e.recordRefreshFailure(RefreshFailureTransport)
			return initialGate
		}
	}

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
	if recoverableGate(initialGate) {
		plan = e.rotateRecoveryPlan(plan)
	}
	e.beginRefreshProgress(len(plan))
	if len(plan) == 0 {
		// Nothing to fetch — recompute against cached windows so the
		// snapshot timestamp moves forward even on a no-op refresh.
		return e.finalise(members, cached)
	}

	fetchErrs, transportErr := e.execute(ctx, plan, cached)
	if ctx.Err() != nil {
		e.recordRefreshFailure(RefreshFailureCancelled)
		return ctx.Err()
	}

	e.logFetchErrors(fetchErrs)

	if transportErr != nil {
		if progress, _ := e.Progress(); recoverableGate(transportErr) && progress.Processed > progress.Failed {
			// One constituent's failure cannot hide an otherwise converged
			// observation. finalise still requires the ordinary current-session
			// coverage threshold, and the transport warning remains active.
			if err := e.finalise(members, cached); err != nil {
				return err
			}
		}
		// The gate turned red mid-fan-out. Progress so far is
		// checkpointed; classify the attempt as transport, not coverage,
		// so the scheduler retries on its short cadence.
		e.recordRefreshFailure(RefreshFailureTransport)
		return transportErr
	}

	return e.finalise(members, cached)
}

// fetchErrorSampleSize bounds how many symbols one cause names before
// the line switches to a count plus an example set.
const fetchErrorSampleSize = 5

// logFetchErrors collapses per-symbol fetch failures into one line per
// distinct cause. A single dead bulk connector fails every planned name
func (e *Engine) logFetchErrors(fetchErrs map[string]error) {
	byCause := make(map[string][]string, len(fetchErrs))
	for sym, err := range fetchErrs {
		cause := err.Error()
		byCause[cause] = append(byCause[cause], sym)
	}
	for _, cause := range slices.Sorted(maps.Keys(byCause)) {
		names := byCause[cause]
		slices.Sort(names)
		if len(names) <= fetchErrorSampleSize {
			e.warnf("breadth: fetch %s: %s", strings.Join(names, ", "), cause)
			continue
		}
		e.warnf("breadth: fetch failed for %d names (e.g. %s): %s",
			len(names), strings.Join(names[:fetchErrorSampleSize], ", "), cause)
	}
}

// fetchPlan is the per-symbol decision the refresh planner makes.
type fetchPlan struct {
	Symbol       string
	LookbackDays int
	Rebuild      bool
}

// planFetches walks the membership list and decides what to fetch
func (e *Engine) planFetches(members []string, cached map[string]ConstituentWindow) []fetchPlan {
	targetSession := CompletedSessionKey(e.clock())
	e.mu.RLock()
	refused := maps.Clone(e.definitionMisses)
	e.mu.RUnlock()
	plan := make([]fetchPlan, 0, len(members))
	for _, sym := range members {
		if refused[sym] == targetSession {
			continue
		}
		w, ok := cached[sym]
		if !ok || len(w.Closes) == 0 {
			plan = append(plan, fetchPlan{Symbol: sym, LookbackDays: e.coldLookback})
			continue
		}
		if w.LastBarAt == targetSession {
			// Already have the latest completed close — nothing to fetch.
			continue
		}
		last, err := time.Parse("2006-01-02", w.LastBarAt)
		elapsed := int(e.clock().Sub(last).Hours()/24) + 7
		if err != nil || elapsed >= e.coldLookback {
			plan = append(plan, fetchPlan{Symbol: sym, LookbackDays: e.coldLookback, Rebuild: true})
		} else {
			plan = append(plan, fetchPlan{Symbol: sym, LookbackDays: max(e.warmLookback, elapsed)})
		}
	}
	return plan
}

// execute runs the fetch plan in parallel with bounded concurrency. Successful
// touched here: finalise remains the only publication gate, so a checkpoint
// graceful daemon stop therefore keeps every completed name; an abrupt crash
func (e *Engine) execute(ctx context.Context, plan []fetchPlan, windows map[string]ConstituentWindow) (map[string]error, error) {
	errs := make(map[string]error)
	var transportErr error

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
	readOne := func(readCtx context.Context, item fetchPlan, recovery bool) error {
		bars, err := e.fetcher.FetchDaily(readCtx, item.Symbol, item.LookbackDays)
		if err == nil {
			err = validateBars(bars)
		}
		if err == nil && recovery && len(bars) == 0 {
			err = errors.New("recovery read returned no observed bars")
		}
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs[item.Symbol] = err
			failure := RefreshFailureFetch
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				failure = RefreshFailureCancelled
			}
			if errors.Is(err, ErrNoDefinition) {
				e.rememberDefinitionMiss(item.Symbol)
			}
			e.recordRefreshProcessed(failure)
			return err
		}
		base := windows[item.Symbol]
		observedAt := e.clock()
		for i := range bars {
			bars[i].ObservedAt = observedAt
		}
		if item.Rebuild {
			base = ConstituentWindow{}
		}
		merged := mergeBars(base, bars, item.Symbol)
		if constituentWindowsEqual(windows[item.Symbol], merged) {
			e.recordRefreshProcessed("")
			return nil
		}
		windows[item.Symbol] = merged
		dirty++
		if dirty >= windowCheckpointBatchSize {
			checkpointLocked()
		}
		e.recordRefreshProcessed("")
		return nil
	}
	sem := make(chan struct{}, e.workers)
	var wg sync.WaitGroup

dispatch:
	for _, item := range plan {
		if ctx.Err() != nil {
			break
		}
		var gateErr error
		if e.healthGate != nil {
			gateErr = e.healthGate()
		}
		if gateErr != nil {
			if !recoverableGate(gateErr) {
				transportErr = gateErr
				break
			}
			// Drain work admitted before the notice. While it remains, only
			// one bounded request can run; a failure stops this pass.
			wg.Wait()
			if e.clock().Before(e.recoveryRetryAt) {
				transportErr = gateErr
				break
			}
			// The link can change while draining. A hard failure still vetoes.
			if current := e.healthGate(); current != nil && !recoverableGate(current) {
				transportErr = current
				break
			}
			readCtx, cancel := context.WithTimeout(ctx, recoveryReadBudget)
			err := readOne(readCtx, item, true)
			cancel()
			e.recordRecoveryResult(err)
			if err != nil {
				e.recoveryAfter = item.Symbol
				transportErr = gateErr
				break
			}
			continue
		}
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-sem }()
			_ = readOne(ctx, item, false)
		})
	}
	wg.Wait()
	mu.Lock()
	checkpointLocked()
	mu.Unlock()
	return errs, transportErr
}

// rememberDefinitionMiss keeps a name the broker refused out of the fetch
// plan for the rest of the session this refresh targets.
func (e *Engine) rememberDefinitionMiss(symbol string) {
	e.mu.Lock()
	if e.definitionMisses == nil {
		e.definitionMisses = map[string]string{}
	}
	e.definitionMisses[symbol] = e.progress.SessionKey
	e.mu.Unlock()
}

func constituentWindowsEqual(a, b ConstituentWindow) bool {
	return a.Symbol == b.Symbol &&
		a.LastBarAt == b.LastBarAt &&
		a.HighRollingMax == b.HighRollingMax &&
		a.HighRollingBarsHad == b.HighRollingBarsHad &&
		a.LowRollingMin == b.LowRollingMin &&
		a.LowRollingBarsHad == b.LowRollingBarsHad &&
		slices.Equal(a.Closes, b.Closes) && equalParticipationBars(a.Bars, b.Bars)
}

// finalise computes a snapshot from the (possibly updated) windows
//
//	mergeBars step in Refresh never overwrites valid closes with
//	cold start that's bottlenecked on IBKR's per-account
//	the same names while IBKR's bucket is still draining.
//	catastrophic fan-out failures.
//
// matching the SlideWindow same-day semantics. Persistence failures
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
	var above200 *float64
	if snap.Coverage200 > 0 {
		above200 = new(snap.PctAbove200DMA)
	}
	var highs, lows *int
	if snap.CoverageHighsLows > 0 {
		highs, lows = new(snap.NewHighsToday), new(snap.NewLowsToday)
	}
	e.mu.Lock()
	history := appendHistory(e.history, HistoryPoint{
		Participation:     snap.Participation,
		Date:              sessionKey,
		PctAbove50DMA:     snap.PctAbove50DMA,
		PctAbove200DMA:    above200,
		NewHighs:          highs,
		NewLows:           lows,
		MemberCount:       snap.MemberCount,
		Coverage50:        snap.Coverage,
		Coverage200:       snap.Coverage200,
		CoverageHighsLows: snap.CoverageHighsLows,
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
func (e *Engine) LastRefreshCoverage() (coverage, memberCount int) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastCoverage, e.lastMemberCount
}

// appendHistory adds today's point to the rolling series, collapsing
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

// validateBars rejects the entire response before any cached window changes.
// Dropping individual bad rows would compress the trading-session window.
func validateBars(bars []Bar) error {
	previous := ""
	for _, b := range bars {
		if _, err := time.Parse("2006-01-02", b.Date); err != nil || len(b.Date) != 10 || (previous != "" && b.Date <= previous) {
			return errors.New("invalid or unordered daily bar date")
		}
		if b.Close <= 0 || math.IsNaN(b.Close) || math.IsInf(b.Close, 0) {
			return errors.New("invalid daily bar close")
		}
		previous = b.Date
	}
	return nil
}

// mergeBars folds a validated, chronological batch into an existing window.
func mergeBars(w ConstituentWindow, bars []Bar, symbol string) ConstituentWindow {
	if w.Symbol == "" {
		w.Symbol = symbol
	}
	dated := mergeParticipationBars(w.Bars, bars)
	for _, b := range bars {
		if w.LastBarAt != "" && b.Date <= w.LastBarAt && b.Date != w.LastBarAt {
			// Older than the last cached bar — ignore. The cache is
			// the source of truth for historical closes; a fetcher
			// that re-emits past dates shouldn't rewrite history.
			continue
		}
		w = SlideWindow(w, b.Close, b.Date)
	}
	w.Bars = dated
	return w
}

// nySessionKey returns the NY-tz date string identifying today's
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
func (e *Engine) Members() []string {
	e.ensureMembers()
	e.mu.RLock()
	defer e.mu.RUnlock()
	return slices.Clone(e.members)
}

// SetMembers swaps the constituent list. Returns true when the new
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
func (e *Engine) History(limit int) []HistoryPoint {
	e.mu.RLock()
	defer e.mu.RUnlock()
	src := e.history
	if limit > 0 && limit < len(src) {
		src = src[len(src)-limit:]
	}
	out := slices.Clone(src)
	for i := range out {
		if v := out[i].PctAbove200DMA; v != nil {
			out[i].PctAbove200DMA = new(*v)
		}
		if v := out[i].NewHighs; v != nil {
			out[i].NewHighs = new(*v)
		}
		if v := out[i].NewLows; v != nil {
			out[i].NewLows = new(*v)
		}
	}
	return out
}
