package spx

import (
	"context"
	"slices"
	"sync"
	"time"
)

// RefreshState reflects the current health of the members-list
// refresher. Surfaced on the wire by the daemon's status handler so
// `canary status` can flag silent parser rot or a long-disabled
// auto-refresh.
type RefreshState string

const (
	// RefreshHealthy is the steady-state: the most recent fetch
	RefreshHealthy RefreshState = "healthy"
	// RefreshNetworkFailed means the most recent fetch failed at the
	// transport layer (DNS, connect, timeout). Wikipedia
	// unreachable, captive portal, etc.
	RefreshNetworkFailed RefreshState = "network_failed"
	// RefreshParseFailed means we fetched but couldn't extract a
	// usable list — the HTML didn't contain the constituents table
	// or the parse landed outside the sanity bounds. Surfaces a
	// Wikipedia-side restructure or a regex regression.
	RefreshParseFailed RefreshState = "parse_failed"
	// RefreshDisabledConfig means the daemon's config.toml has
	// `[spx] members_auto_refresh = false`.
	RefreshDisabledConfig RefreshState = "disabled (config)"
	// RefreshDisabledEnv means the CANARY_SPX_MEMBERS_AUTO_REFRESH env
	RefreshDisabledEnv RefreshState = "disabled (env)"
)

// IsHealthy reports whether the state is the steady-state. Wraps the
// constant comparison so external callers don't depend on string
// equality.
func (s RefreshState) IsHealthy() bool { return s == RefreshHealthy }

// IsDisabled reports whether the refresher is intentionally off (via
// the failure states.
func (s RefreshState) IsDisabled() bool {
	return s == RefreshDisabledConfig || s == RefreshDisabledEnv
}

// FetchFunc abstracts the Wikipedia round-trip so tests can inject a
// canned response without standing up an httptest server. Production
// passes a closure around FetchAndParse with the daemon's version
// stamp.
type FetchFunc func(ctx context.Context) ([]string, time.Time, error)

// Refresher manages the daemon's runtime membership refresh: three
// triggers (daily 02:30 ET ticker, startup catch-up, opportunistic
// post-rollover) all converge on one singleflighted fetch goroutine.
// On a successful fetch the new list is written atomically to disk
// and pushed into the engine; failures fall back to whatever's
// already loaded — breadth never goes silent because the network is
// down.
//
// Construction is via NewRefresher; the daemon stands one of these up
// per Server lifetime and runs Run() in a goroutine. Tests can drive
// it via TriggerNow() and inspect via State().
type Refresher struct {
	engine    *Engine
	cachePath string
	fetch     FetchFunc
	logger    Logger
	clock     func() time.Time
	state     RefreshState

	// mu guards lastFetch / lastErr / state. Held briefly for read
	// (State) or for the post-fetch update; never held during the
	// long network round-trip.
	mu        sync.Mutex
	lastFetch time.Time

	// singleflight serialises concurrent triggers. The first fetch
	flightMu     sync.Mutex
	flightActive bool
}

// RefresherOptions configures NewRefresher. The Pinned* fields are
// resolved by the caller (config layer) before construction so the
// refresher doesn't have to know about TOML / env semantics.
type RefresherOptions struct {
	// Engine is the breadth engine whose members list this refresher
	Engine *Engine
	// CachePath is where the refresher writes / reads the cached
	CachePath string
	// Fetch is the Wikipedia round-trip. Required.
	Fetch FetchFunc
	// Logger receives non-fatal events. nil silences output.
	Logger Logger
	// Clock injects a synthetic time source for tests. nil → time.Now.
	Clock func() time.Time
	// PinnedByConfig is true when config.toml has
	// [spx] members_auto_refresh = false AND the env var did not
	// force-enable. The refresher renders the state as
	// "disabled (config)" and Run() returns immediately.
	PinnedByConfig bool
	// PinnedByEnv is true when CANARY_SPX_MEMBERS_AUTO_REFRESH=0 is
	PinnedByEnv bool
}

// NewRefresher constructs a refresher. Engine and Fetch are
// required; everything else is optional with sensible defaults.
func NewRefresher(opts RefresherOptions) *Refresher {
	if opts.Engine == nil {
		panic("spx.NewRefresher: Engine is required")
	}
	if opts.Fetch == nil {
		panic("spx.NewRefresher: Fetch is required")
	}
	r := &Refresher{
		engine:    opts.Engine,
		cachePath: opts.CachePath,
		fetch:     opts.Fetch,
		logger:    opts.Logger,
		clock:     opts.Clock,
		state:     RefreshHealthy,
	}
	if r.clock == nil {
		r.clock = time.Now
	}
	// Resolve disabled state. Env wins over config per design;
	switch {
	case opts.PinnedByEnv:
		r.state = RefreshDisabledEnv
	case opts.PinnedByConfig:
		r.state = RefreshDisabledConfig
	}
	return r
}

// State returns the refresher's current health. Read by the daemon's
func (r *Refresher) State() RefreshState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Run starts the daemon-internal refresh loop: a daily 02:30 ET
func (r *Refresher) Run(ctx context.Context) {
	if r.State().IsDisabled() {
		return
	}

	// Startup catch-up: if the on-disk file's as_of is earlier than
	// laptop-closed-at-02:30 case. Failure is logged and ignored;
	if r.needsCatchup() {
		r.triggerAsync(ctx, "startup catch-up")
	}

	for {
		wait := r.nextTickWait()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		r.triggerAsync(ctx, "daily 02:30 ET")
	}
}

// TriggerIfRolledOver is the opportunistic third trigger: the
func (r *Refresher) TriggerIfRolledOver(ctx context.Context) {
	if r.State().IsDisabled() {
		return
	}
	if !r.needsCatchup() {
		return
	}
	r.triggerAsync(ctx, "post-rollover")
}

// TriggerNow forces an immediate fetch attempt for tests. Skips the
// disabled check so the caller can verify pinned-mode behaviour
// directly; in production the disabled check in Run() and
// TriggerIfRolledOver gates entry.
func (r *Refresher) TriggerNow(ctx context.Context) {
	r.triggerAsync(ctx, "manual")
}

// triggerAsync kicks a fetch via the singleflight gate. Spawns a
// goroutine so the caller (ticker, startup, opportunistic) returns
// promptly. The gate ensures at most one fetch is in flight per
// daemon at a time.
func (r *Refresher) triggerAsync(ctx context.Context, reason string) {
	r.flightMu.Lock()
	if r.flightActive {
		r.flightMu.Unlock()
		return
	}
	r.flightActive = true
	r.flightMu.Unlock()

	go func() {
		defer func() {
			r.flightMu.Lock()
			r.flightActive = false
			r.flightMu.Unlock()
		}()
		r.fetchAndSwap(ctx, reason)
	}()
}

// fetchAndSwap performs the fetch, validates the result, persists it, and
//   - Network/transport error → RefreshNetworkFailed.
//   - Parse error (table missing, regex broke) → RefreshParseFailed.
//   - Sanity bounds violated → RefreshParseFailed (same bucket;
//
// On any failure the engine's current members list is untouched —
func (r *Refresher) fetchAndSwap(ctx context.Context, reason string) {
	r.infof("members: fetching from Wikipedia (%s)", reason)
	symbols, asOf, err := r.fetch(ctx)
	if err != nil {
		r.warnf("members: fetch failed (%s): %v — keeping current list", reason, err)
		r.setState(RefreshNetworkFailed)
		return
	}
	if n := len(symbols); n < MinMembers || n > MaxMembers {
		r.warnf("members: parsed %d symbols (want %d-%d) (%s) — keeping current list", n, MinMembers, MaxMembers, reason)
		r.setState(RefreshParseFailed)
		return
	}

	// Compare against the in-process snapshot before doing any work.
	current := r.engine.Members()
	if slices.Equal(current, symbols) {
		if r.needsCatchup() {
			if err := SaveExternal(r.cachePath, symbols, asOf); err != nil {
				r.warnf("members: save to %s: %v (catch-up date bump skipped)", r.cachePath, err)
			}
		}
		r.mu.Lock()
		r.lastFetch = r.clock()
		r.state = RefreshHealthy
		r.mu.Unlock()
		r.infof("members: unchanged (%d names)", len(symbols))
		return
	}

	// Persist before swap. Failure to persist is logged but doesn't
	if err := SaveExternal(r.cachePath, symbols, asOf); err != nil {
		r.warnf("members: save to %s: %v (in-memory list still updated)", r.cachePath, err)
	}

	if changed := r.engine.SetMembers(symbols); changed {
		r.infof("members: list updated (%d → %d names)", len(current), len(symbols))
	}
	r.mu.Lock()
	r.lastFetch = r.clock()
	r.state = RefreshHealthy
	r.mu.Unlock()
}

// setState transitions the refresher's health under r.mu. Wrapper so
// the lock discipline is consistent across error branches.
func (r *Refresher) setState(s RefreshState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = s
}

// needsCatchup reports whether the on-disk file's as_of is earlier
func (r *Refresher) needsCatchup() bool {
	_, asOf, ok := LoadExternal(r.cachePath)
	if !ok {
		// No file → nothing to "catch up". The first scheduled tick
		return false
	}
	today := nySessionKey(r.clock())
	return nySessionKey(asOf) < today
}

// nextTickWait returns how long Run() should sleep until the next
func (r *Refresher) nextTickWait() time.Duration {
	loc := nyLocation()
	now := r.clock().In(loc)
	target := time.Date(now.Year(), now.Month(), now.Day(), 2, 30, 0, 0, loc)
	if !target.After(now) {
		target = target.AddDate(0, 0, 1)
	}
	return target.Sub(now)
}

func (r *Refresher) infof(format string, args ...any) {
	if r.logger != nil {
		r.logger.Infof(format, args...)
	}
}

func (r *Refresher) warnf(format string, args ...any) {
	if r.logger != nil {
		r.logger.Warnf(format, args...)
	}
}
