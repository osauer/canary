package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// handleRegimeSnapshot fans out fetches for all risk-regime
// dashboard indicators in parallel and assembles one consolidated
// envelope. Per-indicator failures are localised — a stale VIX feed
// doesn't fail the whole call; the affected row carries
// Status="error" or "unavailable" with a notes string the consumer
// can render.
//
// This is the surface the dashboard generator and the MCP
// natural-language interface call. The daemon attaches compact
// spec-default band metadata so JSON/MCP clients can read one stable
// agent surface, while the raw measurements remain present for
// renderers that want to apply their own thresholds.
//
// Dealer gamma is daemon-prewarmed after gateway startup. During option RTH,
// a successful last-good refreshes behind the served value on the cache's
// 15-minute soft TTL; outside RTH automatic refresh is not due. A cold cache
// still returns Status="computing" plus an ETA once work is in flight.
//
// USD/JPY and breadth may surface
// Status="unavailable" depending on classifySymbol coverage at
// snapshot time — see the per-indicator notes for the disposition.
func (s *Server) handleRegimeSnapshot(ctx context.Context, _ *rpc.Request) (*rpc.RegimeSnapshotResult, error) {
	brokerScope := s.currentBrokerStateScope()
	if s.regimeSnapshots != nil {
		if err := s.repairRegimeSnapshotProjections(ctx); err != nil {
			return nil, &regimeSnapshotCacheUnavailableError{cause: err}
		}
		view, err := s.regimeSnapshots.serve(ctx, s.acquireRegimeSnapshot)
		if err != nil {
			return nil, err
		}
		if view.Snapshot == nil {
			return nil, &regimeSnapshotCacheUnavailableError{cause: errRegimeSnapshotRefreshIncomplete}
		}
		s.observeRegimeAlertShadow(ctx, view.Snapshot, brokerScope)
		return view.Snapshot, nil
	}

	// Legacy unit fixtures may construct Server directly without attaching
	// daemon.db. They still exercise acquisition, but production never reaches
	// this branch because Start binds the authority before publishing the RPC
	// socket.
	res, complete, afterPublish, err := s.acquireRegimeSnapshot(ctx)
	if err == nil && complete && afterPublish != nil {
		err = afterPublish(ctx, regimeSnapshotPublication{PublishedAt: res.AsOf, Fingerprint: res.Fingerprint})
	}
	if err == nil && res != nil {
		s.observeRegimeAlertShadow(ctx, res, brokerScope)
	}
	return res, err
}

func (s *Server) repairRegimeSnapshotProjections(ctx context.Context) error {
	pending, _ := s.regimeSnapshots.projectionFailure()
	if !pending {
		return nil
	}
	s.regimeProjectionRepairMu.Lock()
	defer s.regimeProjectionRepairMu.Unlock()
	pending, revision := s.regimeSnapshots.projectionFailure()
	if !pending {
		return nil
	}
	if err := s.reconcileRegimeSnapshotProjections(ctx, s.regimeSnapshots); err != nil {
		return fmt.Errorf("repair regime snapshot projection revision %d: %w", revision, err)
	}
	return s.regimeSnapshots.markProjectionRepaired(revision)
}

// acquireRegimeSnapshot performs one market-data fan-out and finalizes its
// classified semantics. It is the only path allowed to tick streaks, latch
// rulebook state, journal a decision, or update status quality, and it does so
// only after all eight workers returned before the acquisition deadline.
func (s *Server) acquireRegimeSnapshot(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
	c := s.gatewayConnector()
	if c == nil {
		return nil, false, nil, s.gatewayUnavailableError()
	}

	deps := productionRegimeDeps(c, s.logger.Warnf, s.regimeSeries, s.regimeHistory)
	outcome := runRegimeFanoutOutcome(
		ctx,
		func(c context.Context) rpc.RegimeVIXTerm { return fetchRegimeVIXTerm(c, deps) },
		func(c context.Context) rpc.RegimeVolOfVol { return fetchRegimeVolOfVol(c, deps) },
		func(c context.Context) rpc.RegimeHYGSPYDivergence { return fetchRegimeHYGSPY(c, deps) },
		func(c context.Context) rpc.RegimeCreditSpreads { return fetchRegimeCreditSpreads(c, deps) },
		func(c context.Context) rpc.RegimeFundingStress { return fetchRegimeFundingStress(c, deps) },
		func(c context.Context) rpc.RegimeUSDJPY { return fetchRegimeUSDJPY(c, deps) },
		func(c context.Context) rpc.RegimeGammaZero { return fetchRegimeGamma(c, s) },
		func(c context.Context) rpc.RegimeBreadth { return fetchRegimeBreadth(c, s) },
		s.regimeContentionMessage,
	)
	res := outcome.Snapshot
	if !outcome.Complete {
		return res, false, nil, nil
	}
	if s.regimeSnapshots != nil {
		if view, err := s.regimeSnapshots.current(); err == nil {
			carryVIXTermFromLastGood(res, view.Snapshot, res.AsOf)
		}
	}
	// Official-calendar tape session, stamped once from the snapshot clock so
	// lifecycle gating, the decisions journal, and every serve surface read
	// the same classification. Closed dates bar frozen SPY/VIX prints from
	// entering or holding tape-driven lifecycle stages (fail-open when empty).
	res.TapeSessionState, res.TapeSessionReason, res.TapeNextOpen = rpc.TapeSessionFor(res.AsOf)
	// Classification + confirmation policy run once, here: band (with
	// red-exit hysteresis), streak tick, cadence freshness, and
	// eligibility per row. Every downstream consumer — composite,
	// lifecycle, CLI, Stress, SPA — reads the served results
	// (internal-docs/design/regime-calibration.md).
	evaluatedStreaks := s.streaks
	if s.streaks != nil {
		evaluatedStreaks = s.streaks.cloneForRegimeEvaluation()
	}
	policies := s.populateStreaksWithStore(res, evaluatedStreaks)
	annotateRegimeMetadata(res, policies)
	// Cluster tallies via the shared rpc combination. The verdict needs
	// the lifecycle stage (single wording table), so it is assigned after
	// the lifecycle below.
	res.Composite = buildRegimeComposite(res)
	res.Summary = buildRegimeSummary(res)
	res.WarningDetails = buildRegimeWarnings(res)
	res.DataQuality = regimeSnapshotDataQuality(res)
	res.SourceHealth = rpc.BuildRegimeSourceHealth(res, res.AsOf)
	res.Lifecycle = rpc.BuildRegimeLifecycle(res)
	res.Composite.Verdict = rpc.RegimeHeadline(res.Composite, res.Lifecycle.Stage)
	res.Summary.Label = res.Composite.Verdict
	res.Posture = rpc.BuildRegimePosture(res)
	res.Fingerprint = rpc.BuildRegimeFingerprint(res)
	afterPublish := func(publishContext context.Context, publication regimeSnapshotPublication) error {
		if publication.Revision > 0 {
			return s.commitRegimeSnapshotProjections(publishContext, res, evaluatedStreaks, publication)
		}
		// Revision zero is confined to legacy unit/import helpers which do not
		// own the SQLite snapshot/receipt barrier.
		var projectionErrors []error
		if s.streaks != nil && evaluatedStreaks != nil {
			projectionErrors = append(projectionErrors, s.streaks.commitLegacyRegimeEvaluation(publishContext, evaluatedStreaks, publication.PublishedAt))
		}
		projectionErrors = append(projectionErrors,
			s.projectRulesRegimeStageAt(publishContext, res, publication.PublishedAt),
			s.journalRegimeDecisionPublicationContext(publishContext, res, publication),
		)
		return errors.Join(projectionErrors...)
	}
	return res, true, afterPublish, nil
}

// regimeRowPolicy is one row's classification + confirmation-policy output:
// the post-hysteresis band the row serves, plus the eligibility and
// freshness verdicts that decide whether a red may confirm stress.
type regimeRowPolicy struct {
	band        string
	eligibility *rpc.RegimeEligibility
	freshness   *rpc.RegimeFreshness
}

// populateStreaks runs classification (with red-exit hysteresis), ticks the
// streak counter, evaluates cadence freshness and confirmation eligibility
// for each regime row, attaches the *rpc.StreakInfo, and returns the
// per-indicator policy for annotateRegimeMetadata to serve. Nil-safe on the
// store side (no persistence ⇒ no hysteresis/latch, eligibility evaluated at
// sessions=1) and on the band side (Tick freezes the counter when band="").
func (s *Server) populateStreaks(res *rpc.RegimeSnapshotResult) map[string]regimeRowPolicy {
	return s.populateStreaksWithStore(res, s.streaks)
}

func (s *Server) populateStreaksWithStore(res *rpc.RegimeSnapshotResult, streaks *StreakStore) map[string]regimeRowPolicy {
	policies := make(map[string]regimeRowPolicy, len(streakIndicators))
	if res == nil {
		return policies
	}
	now := nyTime(res.AsOf)
	if res.AsOf.IsZero() {
		now = nyDateNow()
	}
	for _, ind := range streakIndicators {
		key := ind.key()
		band, value := ind.bandAndValue(res)
		display := ind.displayBand(res)
		// Red-exit hysteresis: a red streak holds until the indicator's
		// exit threshold clears, so boundary wobble can't flap the band
		// (and reset the streak) at the entry threshold.
		held := false
		if streaks != nil && band != "" && band != "red" &&
			streaks.PrevBand(key) == "red" && ind.exitHoldsRed(res) {
			band = "red"
			held = true
		}
		if held || (display != "" && band == "red") {
			display = "red"
		}
		fresh := ind.fresh(res, now)
		freshnessClass := rpc.RegimeFreshnessOverdue
		if fresh {
			freshnessClass = rpc.RegimeFreshnessFresh
		}
		switch key {
		case rpc.RegimeIndicatorVIXTerm:
			freshnessClass = vixTermCadenceClass(res, now)
		case rpc.RegimeIndicatorUSDJPY:
			freshnessClass = usdJpyCadenceClass(res, now)
		case rpc.RegimeIndicatorGammaZero:
			freshnessClass = gammaCadenceClass(res, now)
		case rpc.RegimeIndicatorBreadth:
			freshnessClass = breadthCadenceClass(res, now)
		}
		// A session may only be banked from evidence that is current under the
		// indicator's own schedule. Persistence is the gate that stops one
		// reading from confirming stress, so a row measured off its cadence —
		// a mixed-vintage VIX/VIX3M ratio pre-open, a closed-venue FX tick —
		// must not spend a session toward it. Freezing here rather than in
		// bandAndValue keeps the band and its hysteresis hold on display;
		// gammaZeroStreaks has always done the same thing one layer up.
		tickBand := band
		if freshnessClass != rpc.RegimeFreshnessFresh {
			tickBand = ""
		}
		var streak *rpc.StreakInfo
		if streaks != nil {
			streak = streaks.Tick(key, value, tickBand, now)
			if band == "" {
				// Freeze the persisted counter internally, but do not attach a
				// stale prior-band streak to today's unranked row. JSON/MCP
				// consumers otherwise read "status:error" beside "streak:green",
				// which looks like usable evidence when it is only historical
				// memory.
				ind.attachStreak(res, nil)
			} else {
				ind.attachStreak(res, streak)
			}
		}
		freshness := &rpc.RegimeFreshness{
			Class:         freshnessClass,
			MaxAgeSeconds: rpc.RegimeSourceMaxAgeSeconds(rpc.RegimeIndicatorCluster(key)),
		}
		var elig *rpc.RegimeEligibility
		if display == "red" {
			sessions := 1
			latched := false
			if streak != nil && streak.Band == "red" {
				sessions = streak.Sessions
			} else if streaks != nil {
				if prev := streaks.Get(key); prev != nil && prev.Band == "red" {
					sessions = prev.Sessions
				}
			}
			if streaks != nil {
				latched = streaks.Latched(key)
			}
			elig = rpc.EvaluateRegimeEligibility(rpc.RegimeEligibilityInput{
				Indicator:      key,
				Band:           "red",
				Depth:          ind.depth(res),
				StreakSessions: sessions,
				Fresh:          fresh,
				FreshnessClass: freshnessClass,
				Latched:        latched,
			})
			if elig != nil && elig.Eligible && streaks != nil && band == "red" {
				streaks.Latch(key)
			}
		}
		policies[key] = regimeRowPolicy{band: display, eligibility: elig, freshness: freshness}
	}
	return policies
}

// regimeContentionMessage produces the partial-envelope ErrorMessage
// for the regime fan-out's deadline-fired branch. Reads
// s.backgroundTasks() so the message names the daemon-internal task
// that was running when the deadline fired, rather than the generic
// v0.27.6 hedge "concurrent breadth/gamma work".
//
// Called fresh at deadline-fired time so the names reflect the state
// at that moment, not a stale snapshot from handler entry. The
// empty-list case falls through to a gateway-side hedge — the
// daemon couldn't identify an internal cause, so the contention is
// somewhere else (rate-limit headroom, market-data farm).
func (s *Server) regimeContentionMessage() string {
	tasks := s.backgroundTasks()
	names := make([]string, 0, len(tasks))
	for _, task := range tasks {
		// The current acquisition is itself advertised so the idle watcher
		// keeps the daemon alive. It is not contention with itself and must not
		// appear in its own timeout diagnosis.
		if task.Name != "regime-refresh" {
			names = append(names, task.Name)
		}
	}
	if len(names) == 0 {
		return "regime fan-out exceeded handler deadline (gateway-side timeout; no daemon-internal contention detected)"
	}
	return fmt.Sprintf("regime fan-out exceeded handler deadline (contended with daemon-internal task(s): %s)", strings.Join(names, ", "))
}

// runRegimeFanout drives the regime fetchers in parallel and
// returns a consolidated envelope. The function honours ctx's deadline:
// any fetcher that hasn't returned by ctx.Done is surfaced as
// Status=error in the envelope rather than blocking the handler.
//
// Why this exists — pre-v0.27.6 the orchestration used a plain
// wg.Wait() which would hang the handler indefinitely if any one
// fetcher's goroutine blocked past the ctx deadline (e.g. an HMDS
// history fetch queued behind breadth's cold-start fan-out, since the
// pre-collapse FetchHistoricalDailyBars didn't honour parent ctx). The CLI
// then timed out at its own 60 s budget and the user saw
// "regime: context deadline exceeded" — the symptom reported on
// 2026-05-19 that motivated v0.27.6.
//
// Lingering goroutines exit cleanly: the buffered results channel
// (cap equals fetcher count) accepts late sends without blocking; the late values are
// garbage-collected once the caller has returned. Gateway slots stay
// held only as long as the per-call timeouts the fetchers already set
// on their own derived contexts (productionRegimeDeps uses
// FetchHistoricalDailyBars, which respects them).
//
// contentionMsg is called fresh at the deadline-fired branch to
// produce the partial-envelope ErrorMessage. Production wires it to
// Server.regimeContentionMessage so the message names the daemon-
// internal task(s) running at deadline time; tests pass a fixed
// closure.
//
// The function is package-private and takes the closures so tests
// can drive it without constructing a full Server fixture — see
// TestRunRegimeFanout_ReturnsOnCtxDoneWithPartialEnvelope and
// TestRunRegimeFanout_PartialEnvelopeUsesContentionMessage.
type regimeFanoutOutcome struct {
	Snapshot *rpc.RegimeSnapshotResult
	Complete bool
}

// runRegimeFanout preserves the package-private test seam used by the row
// orchestration tests. Publication paths must use runRegimeFanoutOutcome so a
// deadline-filled envelope can never be mistaken for a complete observation.
func runRegimeFanout(
	ctx context.Context,
	vix func(context.Context) rpc.RegimeVIXTerm,
	volOfVol func(context.Context) rpc.RegimeVolOfVol,
	hyg func(context.Context) rpc.RegimeHYGSPYDivergence,
	creditSpreads func(context.Context) rpc.RegimeCreditSpreads,
	fundingStress func(context.Context) rpc.RegimeFundingStress,
	usdjpy func(context.Context) rpc.RegimeUSDJPY,
	gamma func(context.Context) rpc.RegimeGammaZero,
	breadth func(context.Context) rpc.RegimeBreadth,
	contentionMsg func() string,
) *rpc.RegimeSnapshotResult {
	return runRegimeFanoutOutcome(ctx, vix, volOfVol, hyg, creditSpreads, fundingStress, usdjpy, gamma, breadth, contentionMsg).Snapshot
}

func runRegimeFanoutOutcome(
	ctx context.Context,
	vix func(context.Context) rpc.RegimeVIXTerm,
	volOfVol func(context.Context) rpc.RegimeVolOfVol,
	hyg func(context.Context) rpc.RegimeHYGSPYDivergence,
	creditSpreads func(context.Context) rpc.RegimeCreditSpreads,
	fundingStress func(context.Context) rpc.RegimeFundingStress,
	usdjpy func(context.Context) rpc.RegimeUSDJPY,
	gamma func(context.Context) rpc.RegimeGammaZero,
	breadth func(context.Context) rpc.RegimeBreadth,
	contentionMsg func() string,
) regimeFanoutOutcome {
	res := &rpc.RegimeSnapshotResult{
		SpecDoc: "https://osauer.dev/canary/docs/internals/regime-dashboard.html",
	}

	type regimeRow struct {
		kind string
		v    any
	}
	results := make(chan regimeRow, 8)
	go func() { results <- regimeRow{"vix", vix(ctx)} }()
	go func() { results <- regimeRow{"vol_of_vol", volOfVol(ctx)} }()
	go func() { results <- regimeRow{"hyg", hyg(ctx)} }()
	go func() { results <- regimeRow{"credit_spreads", creditSpreads(ctx)} }()
	go func() { results <- regimeRow{"funding_stress", fundingStress(ctx)} }()
	go func() { results <- regimeRow{"usdjpy", usdjpy(ctx)} }()
	go func() { results <- regimeRow{"gamma", gamma(ctx)} }()
	go func() { results <- regimeRow{"breadth", breadth(ctx)} }()

	received := make(map[string]bool, 8)
	deadlineFired := false
	for len(received) < 8 && !deadlineFired {
		select {
		case r := <-results:
			switch r.kind {
			case "vix":
				res.VIXTermStructure = r.v.(rpc.RegimeVIXTerm)
			case "vol_of_vol":
				res.VolOfVol = r.v.(rpc.RegimeVolOfVol)
			case "hyg":
				res.HYGSPYDivergence = r.v.(rpc.RegimeHYGSPYDivergence)
			case "credit_spreads":
				res.CreditSpreads = r.v.(rpc.RegimeCreditSpreads)
			case "funding_stress":
				res.FundingStress = r.v.(rpc.RegimeFundingStress)
			case "usdjpy":
				res.USDJPY = r.v.(rpc.RegimeUSDJPY)
			case "gamma":
				res.GammaZero = r.v.(rpc.RegimeGammaZero)
			case "breadth":
				res.Breadth = r.v.(rpc.RegimeBreadth)
			}
			received[r.kind] = true
		case <-ctx.Done():
			deadlineFired = true
		}
	}
	if deadlineFired {
		// Fill any rows that didn't complete with an honest error
		// envelope so the wire payload is never half-filled. In
		// practice the laggard is usually one of the quote/history or
		// official daily-file rows — gamma and breadth mostly read
		// in-memory state — but we cover every row defensively.
		exceededMsg := contentionMsg()
		if !received["vix"] {
			res.VIXTermStructure = rpc.RegimeVIXTerm{Notes: vixTermNotes, Status: rpc.RegimeStatusError, ErrorMessage: exceededMsg}
		}
		if !received["vol_of_vol"] {
			res.VolOfVol = rpc.RegimeVolOfVol{Symbol: "VVIX", Notes: volOfVolNotes, Status: rpc.RegimeStatusError, ErrorMessage: exceededMsg}
		}
		if !received["hyg"] {
			res.HYGSPYDivergence = rpc.RegimeHYGSPYDivergence{Notes: hygSpyNotes, Status: rpc.RegimeStatusError, ErrorMessage: exceededMsg}
		}
		if !received["credit_spreads"] {
			res.CreditSpreads = rpc.RegimeCreditSpreads{Notes: creditSpreadsNotes, Status: rpc.RegimeStatusError, ErrorMessage: exceededMsg}
		}
		if !received["funding_stress"] {
			res.FundingStress = rpc.RegimeFundingStress{Notes: fundingStressNotes, Status: rpc.RegimeStatusError, ErrorMessage: exceededMsg}
		}
		if !received["usdjpy"] {
			res.USDJPY = rpc.RegimeUSDJPY{Symbol: "USD.JPY", Notes: usdJpyNotes, Status: rpc.RegimeStatusError, ErrorMessage: exceededMsg}
		}
		if !received["gamma"] {
			res.GammaZero = rpc.RegimeGammaZero{
				Notes:  gammaNotes,
				Status: rpc.RegimeStatusError,
				Envelope: rpc.GammaZeroSPXResult{
					Status: rpc.GammaZeroStatusError,
					Error:  exceededMsg,
				},
			}
		}
		if !received["breadth"] {
			res.Breadth = rpc.RegimeBreadth{Notes: breadthNotes, Status: rpc.RegimeStatusError}
		}
	}

	res.AsOf = time.Now()
	return regimeFanoutOutcome{Snapshot: res, Complete: !deadlineFired && len(received) == 8}
}

// regimeDeps is the dependency surface the three quote-and-history
// indicators (VIX, HYG/SPY, USD/JPY) share. It exists for two
// concrete reasons:
//
//  1. The three fetchers all call briefSnapshotPrice +
//     FetchHistoricalDailyBars + MarketDataSnapshot lookups, so a single
//     struct keeps the call sites uniform.
//  2. The unit tests need to drive each fetcher with canned data
//     without spinning up a real daemon or gateway connection.
//
// logWarnf is the operator-visible signal for partial failures:
// snapshot subscribe errors, history-bar fetch errors, insufficient-bar
// truncations, and history-close fallback use land here rather than getting
// silently swallowed. A null or degraded field in the returned envelope only
// tells the consumer *that* live data is missing; the daemon log tells them
// *why*. Tests inject a capture closure to assert the right diagnostic landed.
//
// snapshotWith52WHigh is the SPY-specific seam: the default snapshot
// path returns on the first price tick, too fast for IBKR's Misc
// Stats tick 165 (Week-range highs/lows) to arrive. The HYG/SPY
// indicator needs the 52w high to evaluate the spec's yellow-band
// trigger; without it the indicator drops to a 2-state signal.
//
// Indicators 4 (gamma) and 5 (breadth) already delegate to their own
// handlers (handleGammaZeroSPX, handleBreadthSPX); they don't need a
// deps struct because they already have a server-level seam.
type regimeDeps struct {
	// snapshot returns price + previous regular-session close (tick 9) +
	// gateway data-type. PrevClose is the same anchor tick 9 emits
	// alongside the price triple — surfacing it here lets the dashboard
	// header carry day-over-day change for SPY and VIX without a second
	// subscribe. PrevClose is 0 when the gateway didn't deliver tick 9 in
	// the budget; callers must distinguish "not arrived" from "zero".
	snapshot            func(ctx context.Context, sym string, timeout time.Duration) (price, prevClose float64, dataType string)
	snapshotWith52WHigh func(ctx context.Context, sym string, timeout time.Duration) (price, prevClose, week52High float64, dataType string)
	// history takes ctx instead of an explicit timeout so cancellation
	// from handleRegimeSnapshot's outer deadline propagates into the
	// HMDS fetch. The fetcher wraps each call in context.WithTimeout
	// for its own per-call budget; canceling either the parent ctx or
	// the per-call ctx unblocks the call. See v0.27.6 changelog for
	// the bug class this guards against.
	history        func(ctx context.Context, sym string, days int) ([]ibkrlib.HistoricalBar, error)
	officialSeries func(ctx context.Context, seriesID string) ([]regimeSeriesPoint, error)
	vvixSeries     func(ctx context.Context) ([]regimeSeriesPoint, error)
	logWarnf       func(format string, args ...any)
	// now overrides the fetchers' clock reads. Tests pin it so
	// calendar-keyed behavior (the closed-date day-change pin, the
	// off-hours banding branch) is deterministic regardless of when the
	// suite runs; nil means time.Now().
	now func() time.Time
}

// regimeNow is the fetchers' clock read; see regimeDeps.now.
func regimeNow(deps *regimeDeps) time.Time {
	if deps != nil && deps.now != nil {
		return deps.now()
	}
	return time.Now()
}

// productionRegimeDeps wires the deps struct to the live connector.
// Tests pass a hand-rolled regimeDeps with closures returning canned
// values instead.
func productionRegimeDeps(c *ibkrlib.Connector, logWarnf func(format string, args ...any), seriesCache *regimeSeriesCache, historyCache *regimeHistoryCache) *regimeDeps {
	officialSeries := fetchOfficialRegimeSeries
	if seriesCache != nil {
		officialSeries = func(ctx context.Context, seriesID string) ([]regimeSeriesPoint, error) {
			return seriesCache.fetch(ctx, seriesID, fetchOfficialRegimeSeries)
		}
	}
	return &regimeDeps{
		snapshot: func(ctx context.Context, sym string, timeout time.Duration) (float64, float64, string) {
			return briefSnapshotPriceWithClose(ctx, c, sym, timeout, logWarnf)
		},
		snapshotWith52WHigh: func(ctx context.Context, sym string, timeout time.Duration) (float64, float64, float64, string) {
			return briefSnapshotPriceWith52WHigh(ctx, c, sym, timeout, logWarnf)
		},
		history: func(ctx context.Context, sym string, days int) ([]ibkrlib.HistoricalBar, error) {
			fetch := func(ctx context.Context, sym string, days int) ([]ibkrlib.HistoricalBar, error) {
				return c.FetchHistoricalDailyBars(ctx, sym, days, 0)
			}
			if historyCache != nil {
				return historyCache.fetch(ctx, sym, days, fetch)
			}
			return fetch(ctx, sym, days)
		},
		officialSeries: officialSeries,
		vvixSeries:     fetchCBOEVVIXSeries,
		logWarnf:       logWarnf,
	}
}

// ----------------------------------------------------------------------------
// Per-indicator fetchers. Each one returns a fully-populated row even on
// failure — the regime envelope never carries nil sub-objects.

// boundedSnapshot bounds the wall time of deps.snapshot to ~budget+1s,
// regardless of whether deps.snapshot itself honours ctx all the way
// down. Kept as defense-in-depth after F-26 closed the structural gap
// that originally motivated it.
//
// History:
//
//   - v0.27.5 fixed a hard hang in SubscribeMarketData.
//   - v0.27.6 stopped a 45s envelope-level deadline from clobbering one-row
//     errors so a slow leg surfaced cleanly.
//   - v0.27.9 added this wrapper because the inner pkg/ibkr.acquireMarketDataSlot
//     used Connection.ctx, not the caller's ctx — a fetcher that hit slot
//     exhaustion would block past its 5s budget (the inner pollUntil never
//     ran because SubscribeMarketData never returned) and only bail at the
//     orchestrator's 45s handler ctx. The wrapper races deps.snapshot in a
//     goroutine and returns zeros after budget+1s regardless of inner ctx
//     honouring.
//   - F-26 (v0.27.11) threaded ctx through SubscribeMarketData →
//     RequestMarketDataWithContract → acquireMarketDataSlot so the budget
//     is enforced at the slot-acquire layer. The inner code now honours
//     ctx end-to-end and this wrapper is no longer load-bearing.
//
// We keep the wrapper anyway: it costs nothing in the happy path (the
// timer fires only after budget+1s, well past inner completion) and
// catches future regressions in either the slot path or any other
// inner code that might block past its declared budget.
//
// If the goroutine times out it leaks until it returns naturally;
// callers map zero values to a row-level "no spot tick" status.
var boundedSnapshotSlack = time.Second

func boundedSnapshot(ctx context.Context, deps *regimeDeps, sym string, budget time.Duration) (price, prevClose float64, dataType string) {
	type r struct {
		price, prevClose float64
		dt               string
	}
	resCh := make(chan r, 1)
	go func() {
		p, pc, d := deps.snapshot(ctx, sym, budget)
		resCh <- r{p, pc, d}
	}()
	// Slack over budget so deps.snapshot has a chance to
	// return its own deadline error before we bail. The slack matters
	// when the inner code DOES honour ctx — without it, we'd race the
	// inner deadline and lose, returning zeros instead of the inner
	// path's classified result.
	select {
	case got := <-resCh:
		return got.price, got.prevClose, got.dt
	case <-time.After(budget + boundedSnapshotSlack):
		return 0, 0, ""
	case <-ctx.Done():
		return 0, 0, ""
	}
}

// boundedSnapshotWith52WHigh is the boundedSnapshot wrapper for the
// snapshotWith52WHigh dep variant. Same rationale and structure.
func boundedSnapshotWith52WHigh(ctx context.Context, deps *regimeDeps, sym string, budget time.Duration) (price, prevClose, week52High float64, dataType string) {
	type r struct {
		price, prevClose, week52High float64
		dt                           string
	}
	resCh := make(chan r, 1)
	go func() {
		p, pc, w, d := deps.snapshotWith52WHigh(ctx, sym, budget)
		resCh <- r{p, pc, w, d}
	}()
	select {
	case got := <-resCh:
		return got.price, got.prevClose, got.week52High, got.dt
	case <-time.After(budget + boundedSnapshotSlack):
		return 0, 0, 0, ""
	case <-ctx.Done():
		return 0, 0, 0, ""
	}
}

// ----------------------------------------------------------------------------
// Closed-date day-change pinning.
//
// On official non-trading dates (weekends, holidays) the gateway keeps
// serving SPY/VIX snapshots, but the two inputs behind the dashboard's
// day-change fields — the last print and the tick-9 previous-close
// anchor — can each reset independently while the market is closed
// (nightly gateway restart, resubscribes, frozen-mode snapshot
// differences). The rendered pair is then a mixture no market ever
// printed: on Sunday 2026-07-19 the header read SPY +0.00% (the SPY
// anchor had rolled forward onto the last print) beside VIX +12.19%
// (the VIX anchor still held Thursday's close) while Friday's true
// close-to-close tape was SPY −0.99% / VIX +12.19%. On closed dates the
// change fields therefore pin to the official daily closes of the last
// two completed sessions, and the *_change_basis contract fields say
// which sessions the number spans. When those closes can't be resolved
// the fields stay nil — honest absence beats a drifted number. Trading
// dates (including pre/post hours) are untouched: extended-hours prints
// are live information and the tape row exists to catch them.
//
// The closed-date key is rpc.TapeSessionFor — the same official-calendar
// authority the Stress read's session-aware severity demotion uses — so value
// pinning and severity demotion always agree on what a closed date is.

// regimeTapePinLookbackDays is the calendar-day window for the
// closed-date pin's daily-bar fetch. Two completed sessions need ~4
// calendar days; 15 rides out holiday clusters, and the window gets its
// own regimeHistoryCache entry, so closed-date reads cost at most one
// HMDS call per symbol per cache freshness window.
const regimeTapePinLookbackDays = 15

// regimePrevSessionDates returns the last n completed US-equity trading
// session dates strictly before now's market-local date, newest first,
// as YYYY-MM-DD strings. Same day-by-day walkback as
// previousMarketCloseTime (handlers.go); 21 calendar days of slack
// covers holiday clusters. Nil when the calendar can't resolve n
// sessions — callers treat that as pin-unavailable.
func regimePrevSessionDates(now time.Time, n int) []string {
	cal := marketcal.New()
	sess, err := cal.SessionAt(marketcal.MarketUSEquity, now)
	if err != nil {
		return nil
	}
	loc, err := time.LoadLocation(sess.Timezone)
	if err != nil {
		return nil
	}
	local := now.In(loc)
	var out []string
	for i := 1; i <= 21 && len(out) < n; i++ {
		day := local.AddDate(0, 0, -i).Format("2006-01-02")
		res, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, Date: day, Days: 1})
		if err != nil {
			continue
		}
		if s := res.Session.State; s == marketcal.StateRegular || s == marketcal.StateEarlyClose {
			out = append(out, day)
		}
	}
	if len(out) < n {
		return nil
	}
	return out
}

// regimeTapePin carries the official closes behind a pinned day change.
type regimeTapePin struct {
	last, prev         float64
	lastDate, prevDate string
	basis              string
}

// regimeClosedDateTapePin resolves sym's official daily closes for the
// last two completed sessions, matching bars by exact session date so a
// stale series (or an early partial next-session bar) yields an honest
// miss instead of a wrong-session change. reason is the calendar's
// closed-date label ("weekend", a holiday name) for the basis string.
func regimeClosedDateTapePin(ctx context.Context, deps *regimeDeps, sym string, now time.Time, reason string) (regimeTapePin, bool) {
	if deps == nil || deps.history == nil {
		return regimeTapePin{}, false
	}
	dates := regimePrevSessionDates(now, 2)
	if dates == nil {
		warnDeps(deps, "regime: %s closed-date day change: calendar could not resolve the last two sessions", sym)
		return regimeTapePin{}, false
	}
	hctx, hcancel := context.WithTimeout(ctx, 15*time.Second)
	bars, err := deps.history(hctx, sym, regimeTapePinLookbackDays)
	hcancel()
	if err != nil {
		warnDeps(deps, "regime: %s closed-date day change: history fetch failed: %v", sym, err)
		return regimeTapePin{}, false
	}
	pin := regimeTapePin{lastDate: dates[0], prevDate: dates[1]}
	for _, bar := range bars {
		switch historyBarSessionDate(bar) {
		case pin.lastDate:
			pin.last = bar.Close
		case pin.prevDate:
			pin.prev = bar.Close
		}
	}
	if pin.last <= 0 || pin.prev <= 0 {
		warnDeps(deps, "regime: %s closed-date day change: official closes %s/%s not in %d-bar history; withholding day change", sym, pin.prevDate, pin.lastDate, len(bars))
		return regimeTapePin{}, false
	}
	if reason == "" {
		reason = "market closed"
	}
	pin.basis = fmt.Sprintf("official closes %s → %s (%s)", pin.prevDate, pin.lastDate, reason)
	return pin, true
}

const vixTermNotes = "VIX (30-day implied vol) divided by VIX3M (3-month implied vol). Spec thresholds: <0.92 green (healthy contango), 0.92-1.00 yellow (flattening), >1.00 red (backwardation — acute stress pricing). Signal requires sustained inversion over 2-3 sessions, not a single Fed-day spike. Confirmation gate: a red may confirm stress only after 2 consecutive NY trading sessions of inversion (or ratio >= 1.05 day one) on a fresh same-session tick; earlier or stale reds are provisional and warn only. On official non-trading dates the VIX day-change fields are pinned to the official daily closes of the last two completed sessions (vix_change_basis names them); frozen weekend prints and reset prev-close anchors never serve as day-change inputs."

// carryVIXTermFromLastGood repairs a VIX term row that lost only its VIX3M leg
// to a fetch timeout. Outside the publication window VIX3M cannot have changed,
// so the previous snapshot's print is still the newest value that exists, and
// blanking the row over one missed poll of a thin index costs the whole vol
// cluster. Bounded to the most recently completed window: a value older than
// that means a dead subscription rather than a slow one, and must stay
// unavailable. A miss while VIX3M is actually publishing is a real gap and is
// deliberately not carried here.
func carryVIXTermFromLastGood(res, prev *rpc.RegimeSnapshotResult, now time.Time) bool {
	if res == nil || prev == nil {
		return false
	}
	row := &res.VIXTermStructure
	if row.Status != rpc.RegimeStatusError || row.VIX == nil || row.VIX3M != nil {
		return false
	}
	if vix3mDisseminating(nyTime(now)) {
		return false
	}
	start, _, ok := vix3mLastDisseminationWindow(now)
	if !ok {
		return false
	}
	carried := prev.VIXTermStructure
	if carried.VIX3M == nil || *carried.VIX3M <= 0 || carried.VIX3MQuality == nil ||
		carried.VIX3MQuality.AsOf.Before(start) {
		return false
	}
	quality := *carried.VIX3MQuality
	quality.Source += " · carried, no tick within budget"
	ratio := *row.VIX / *carried.VIX3M
	row.VIX3M = carried.VIX3M
	row.VIX3MQuality = &quality
	row.Ratio = &ratio
	row.DataType = rpc.MarketDataFrozen
	row.Status = rpc.RegimeStatusStale
	row.ErrorMessage = ""
	return true
}

func fetchRegimeVIXTerm(ctx context.Context, deps *regimeDeps) rpc.RegimeVIXTerm {
	out := rpc.RegimeVIXTerm{Notes: vixTermNotes}
	now := regimeNow(deps)

	// VIX itself usually delivers a live mark (tick 37) even off-hours.
	// VIX3M is a thinner CBOE index whose calculation only updates with
	// active SPX option flow; pre-open it routinely emits no live ticks
	// at all and the snapshot helper falls back to the previous
	// regular-session close (tick 9) so the ratio still ranks. The
	// data-type field honestly reports "frozen" in that case so the
	// renderer dims the row instead of pretending it's live.
	vix, vixPrev, vixDT := boundedSnapshot(ctx, deps, "VIX", 5*time.Second)
	// VIX day-change anchor. Trading dates: populate as soon as tick 9
	// lands alongside a live print — independent of whether VIX3M
	// arrives, and ahead of the no-tick early return, so the dashboard
	// header stays useful when the ratio leg fails. Closed dates: pin to
	// official closes (see the closed-date pinning block above) — the
	// frozen print and the tick-9 anchor may each have reset
	// independently since the last session.
	if state, reason, _ := rpc.TapeSessionFor(now); state == rpc.TapeSessionClosedDate {
		if pin, ok := regimeClosedDateTapePin(ctx, deps, "VIX", now, reason); ok {
			out.VIXPrevClose = new(pin.prev)
			chg := (pin.last - pin.prev) / pin.prev * 100
			out.VIXChangePct = &chg
			out.VIXChangeBasis = pin.basis
		} else {
			out.FieldsMissing = append(out.FieldsMissing, "vix_day_change")
		}
	} else if vix > 0 && vixPrev > 0 {
		out.VIXPrevClose = new(vixPrev)
		chg := (vix - vixPrev) / vixPrev * 100
		out.VIXChangePct = &chg
	}
	if vix <= 0 {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "VIX: no spot tick"
		return out
	}
	// 8 s budget (vs 5 s for VIX) because VIX3M is a much thinner
	// CBOE index: off-hours the gateway sometimes takes longer than
	// the VIX leg to push the close tick, and 5 s reliably lost it on
	// cold-frozen-mode calls even with a warm contract cache. 8 s
	// matches the SPY 52w-high budget for the same reason.
	vix3m, _, vix3mDT := boundedSnapshot(ctx, deps, "VIX3M", 8*time.Second)
	if vix3m <= 0 {
		// One arm of the pair is enough to be informative, but the
		// ratio cannot be computed; surface VIX alone with an
		// error_message so the consumer knows the ratio is missing.
		out.VIX = new(vix)
		out.VIXQuality = firmTickQuality(now, vixDT, "VIX tick")
		out.DataType = vixDT
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "VIX3M: no tick within budget (thin CBOE index, common off-hours)"
		return out
	}

	out.VIX = new(vix)
	out.VIX3M = new(vix3m)
	out.VIXQuality = firmTickQuality(now, vixDT, "VIX tick")
	out.VIX3MQuality = vix3mTickQuality(now, vix3mDT)
	r := vix / vix3m
	out.Ratio = &r
	// The ratio is only as fresh as the staler leg. Both must be live
	// to call the whole row "live".
	out.DataType = vixDT
	if !rpc.IsLiveDataType(vix3mDT) {
		out.DataType = vix3mDT
	}
	if rpc.IsLiveDataType(out.DataType) {
		out.Status = rpc.RegimeStatusOK
	} else {
		out.Status = rpc.RegimeStatusStale
	}
	return out
}

const volOfVolNotes = "VVIX (Cboe VIX-of-VIX) from Cboe's official daily VVIX time series. Default heuristic bands: <90 green (vol-of-vol contained), 90-110 yellow (volatility demand rising), >110 red (vol-of-vol shock / convexity demand). This is an evidence-balance input, not a volatility forecast; use with VIX term structure because both live in the equity-vol cluster and can disagree. Confirmation gate: a red confirms after 2 sessions at >= 110 (or >= 120 day one) on a current official close."

func fetchRegimeVolOfVol(ctx context.Context, deps *regimeDeps) rpc.RegimeVolOfVol {
	out := rpc.RegimeVolOfVol{
		Symbol: "VVIX",
		Notes:  volOfVolNotes,
		Source: "Cboe official VVIX daily time series",
	}
	if deps == nil || deps.vvixSeries == nil {
		out.Status = rpc.RegimeStatusUnavailable
		out.ErrorMessage = "VVIX: no official series fetcher configured"
		return out
	}
	cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	points, err := deps.vvixSeries(cctx)
	cancel()
	if err != nil {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "VVIX: " + err.Error()
		return out
	}
	latest, ok := latestSeriesPoint(points)
	if !ok || latest.Value <= 0 {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "VVIX: no usable observation"
		return out
	}
	out.Last = new(latest.Value)
	out.AsOfDate = latest.Date.Format("2006-01-02")
	out.ValueQuality = officialDailyQuality(latest.Date, "Cboe VVIX daily close")
	if lagged, ok := laggedSeriesPoint(points, 20); ok && lagged.Value > 0 {
		chg := (latest.Value - lagged.Value) / lagged.Value * 100
		out.Change20D = &chg
	}
	out.Status = statusForOfficialDaily(latest.Date, time.Now())
	return out
}

const hygSpyNotes = "HYG (high-yield corporate bond ETF) vs SPY context. Spec thresholds: green when HYG is above its 50-day SMA; yellow when HYG breaks below its 50-day SMA; red when HYG is below its 50-day SMA while SPY remains within 3% of its 52-week high. Use the row's streak.sessions to distinguish an early one-session divergence from a sustained 5+ session credit downtrend. Observation window 2-4 weeks; single-day moves are noise. Confirmation gate: a red confirms only when HYG is at least 0.25% below the 50DMA for 2 sessions (or 1.0% below day one); outside regular hours the banding input is the latest official close, never a thin pre/post-market print. When the live HYG spot is unavailable, the latest official close serves as the banding input and the row is marked stale. On official non-trading dates the SPY day-change fields are pinned to the official daily closes of the last two completed sessions (spy_change_basis names them), so a closed-date header shows the last completed session's true change, never a drifted weekend anchor."

// HYGLookbackDays is the calendar-day window passed to the HMDS
// history fetch when computing HYG's 50-day SMA. 50 trading days ≈ 70
// calendar days when the window has zero holidays; the US market
// closes 9-10 days per year, so a 70-day window can come up short on
// the wrong side of Memorial Day / Labor Day / Thanksgiving. 90
// calendar days gives ~10 days of slack — the IBKR HMDS API only
// bills the call, not the bar count, so this is free. Widened from
// 70 to 90 in v0.23.0 (commit 02aba13).
const HYGLookbackDays = 90

func fetchRegimeHYGSPY(ctx context.Context, deps *regimeDeps) rpc.RegimeHYGSPYDivergence {
	out := rpc.RegimeHYGSPYDivergence{Notes: hygSpyNotes}
	now := regimeNow(deps)

	hyg, _, hygDT := boundedSnapshot(ctx, deps, "HYG", 5*time.Second)
	hygSpotMissing := hyg <= 0
	if !hygSpotMissing {
		out.HYGPrice = new(hyg)
		out.HYGDataType = hygDT
		out.HYGQuality = firmTickQuality(now, hygDT, "HYG tick (ARCA)")
	}

	// SPY: pull spot + 52-week high in one combined subscribe so tick
	// 165 (Misc Stats) has time to land. Either field may still come
	// back zero — the predicate inside snapshotWith52WHigh returns
	// partial results on timeout so a cold-start gateway still
	// surfaces what it had. 8s budget (vs 5s for plain snapshots)
	// because the Misc-Stats tick reliably arrives later than the
	// price triple in observed traces.
	spy, spyPrev, spy52, spyDT := boundedSnapshotWith52WHigh(ctx, deps, "SPY", 8*time.Second)
	if spy > 0 {
		out.SPYPrice = new(spy)
		out.SPYQuality = firmTickQuality(now, spyDT, "SPY tick")
	}
	// SPY day-change anchor. Trading dates: the same tick-9 close the
	// subscribe captures alongside the price triple, so the dashboard
	// header carries "SPY 530.42 +1.20 (+0.23%)" without a separate
	// quote call. Closed dates: pin to official closes (see the
	// closed-date pinning block above) — the 2026-07-19 header showed
	// SPY +0.00% beside VIX +12.19% after the SPY anchor rolled forward
	// alone.
	if state, reason, _ := rpc.TapeSessionFor(now); state == rpc.TapeSessionClosedDate {
		if pin, ok := regimeClosedDateTapePin(ctx, deps, "SPY", now, reason); ok {
			out.SPYPrevClose = new(pin.prev)
			diff := pin.last - pin.prev
			out.SPYChange = &diff
			pct := diff / pin.prev * 100
			out.SPYChangePct = &pct
			out.SPYChangeBasis = pin.basis
		} else {
			out.FieldsMissing = append(out.FieldsMissing, "spy_day_change")
		}
	} else if spy > 0 && spyPrev > 0 {
		out.SPYPrevClose = new(spyPrev)
		diff := spy - spyPrev
		out.SPYChange = &diff
		pct := diff / spyPrev * 100
		out.SPYChangePct = &pct
	}
	if spy52 > 0 {
		out.SPY52WHigh = new(spy52)
		out.SPY52WHighQuality = firmTickQuality(now, spyDT, "SPY tick 165 (Misc Stats)")
	} else {
		// Frozen-mode fallback: in MarketDataType=2 the gateway sends
		// the price triple as one static snapshot then goes silent —
		// tick 165 (Misc Stats) never arrives, no matter the budget.
		// Compute max(High) over ~1 trading year of daily bars instead,
		// so the indicator stays 3-state at all hours rather than
		// dropping to 2-state every time the market is closed. The
		// live tick is still primary above; this branch fires only
		// when the gateway didn't supply a value.
		//
		// 365 calendar days yields ~252 trading bars after weekends and
		// the 9-10 US holidays per year; FetchHistoricalDailyBars maps
		// >365 to "1 Y" anyway, so 365 is the exact knee.
		hctx, hcancel := context.WithTimeout(ctx, 20*time.Second)
		spyBars, err := deps.history(hctx, "SPY", 365)
		hcancel()
		switch {
		case err != nil:
			warnDeps(deps, "regime: SPY 52w high history fetch failed: %v", err)
		case len(spyBars) < 50:
			// 50 is a soft floor — any shorter window doesn't
			// meaningfully approximate a 52w high. Stay symmetric
			// with HYG 50DMA's diagnostic shape.
			warnDeps(deps, "regime: SPY 52w high insufficient bars: got %d, want ~252", len(spyBars))
		default:
			hi := maxHigh(spyBars, 252)
			if hi > 0 {
				out.SPY52WHigh = new(hi)
				out.SPY52WHighQuality = derivedQuality(now, "SPY 252d max(High) fallback")
			}
		}
	}

	// 50-day SMA on HYG. See HYGLookbackDays for the
	// calendar-day window's holiday-clipping rationale.
	hctx, hcancel := context.WithTimeout(ctx, 20*time.Second)
	bars, err := deps.history(hctx, "HYG", HYGLookbackDays)
	hcancel()
	switch {
	case err != nil:
		warnDeps(deps, "regime: HYG 50DMA history fetch failed: %v", err)
	case len(bars) < 50:
		warnDeps(deps, "regime: HYG 50DMA insufficient bars: got %d, need 50", len(bars))
	default:
		sma := averageClose(bars, 50)
		if sma > 0 {
			out.HYG50DMA = new(sma)
			out.HYG50DMAQuality = derivedQuality(historyBarAsOf(bars[len(bars)-1], now), "HYG 50-bar SMA")
		}
	}

	// Outside the regular US equity session the HYG banding input is the
	// latest official daily close, never a thin pre/post-market print —
	// the 2026-06-12 incident classified a credit red off a 7 bps
	// pre-open wobble. The settled close is the newest cadence-fresh
	// observation off-hours, so a row with a spot tick stays status ok.
	// The close also keeps the row bandable when the spot tick is missing
	// at any hour; that degraded path is marked stale below. SPY keeps its
	// tick: the 97%-of-52w-high condition carries a 3% buffer that a thin
	// print cannot meaningfully move, and on trading dates the SPY
	// day-change tape fields must keep reflecting the live tape (closed
	// dates pin those fields to official closes above).
	if (out.HYGPrice == nil || !usEquityRTHOpen(now)) && len(bars) > 0 {
		if c := bars[len(bars)-1].Close; c > 0 {
			out.HYGPrice = new(c)
			out.HYGDataType = "close"
			qualityLabel := "HYG latest official daily close (off-hours banding input)"
			if hygSpotMissing {
				qualityLabel = "HYG latest official daily close (spot-miss banding input)"
				out.FieldsMissing = append(out.FieldsMissing, "hyg_spot_tick")
				warnDeps(deps, "regime: HYG spot subscribe delivered no tick; latest official close is serving as banding input")
			}
			out.HYGQuality = derivedQuality(historyBarAsOf(bars[len(bars)-1], now), qualityLabel)
		}
	}

	if out.HYGPrice == nil || out.SPYPrice == nil {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "HYG or SPY spot missing"
		return out
	}
	out.Status = rpc.RegimeStatusOK
	if hygSpotMissing || (out.HYGDataType != "close" && !rpc.IsLiveDataType(hygDT)) {
		out.Status = rpc.RegimeStatusStale
	}
	// Advisory sub-field annotations — the row's primary measurements
	// landed, but a renderer may want to dim "52w-high" or "50DMA"
	// cells that didn't.
	if out.SPY52WHigh == nil {
		out.FieldsMissing = append(out.FieldsMissing, "spy_52w_high")
	}
	if out.HYG50DMA == nil {
		out.FieldsMissing = append(out.FieldsMissing, "hyg_50dma")
	}
	return out
}

const (
	fredSeriesHYOAS = "BAMLH0A0HYM2"
	fredSeriesIGOAS = "BAMLC0A0CM"
)

var regimeOfficialSeriesBudget = 12 * time.Second

const creditSpreadsNotes = "Cash credit spreads from official ICE BofA OAS series via FRED/St. Louis Fed: high-yield OAS (BAMLH0A0HYM2) and investment-grade corporate OAS (BAMLC0A0CM). Units are percentage points. Default heuristic bands use HY OAS: <4.0 green, 4.0-5.5 yellow, >5.5 red; a 20-observation HY OAS widening of >0.50 pp is mixed and >1.00 pp is stressed. This complements HYG/SPY: HYG is faster intraday, OAS is the official cash-credit close. Confirmation gate: the red levels are already deep, so a fresh red confirms after 1 session."

func fetchRegimeCreditSpreads(ctx context.Context, deps *regimeDeps) rpc.RegimeCreditSpreads {
	out := rpc.RegimeCreditSpreads{
		Notes:  creditSpreadsNotes,
		Source: "FRED/St. Louis Fed official ICE BofA OAS CSV",
	}
	if deps == nil || deps.officialSeries == nil {
		out.Status = rpc.RegimeStatusUnavailable
		out.ErrorMessage = "credit spreads: no official series fetcher configured"
		return out
	}
	hyPoints, hyErr, igPoints, igErr := fetchRegimeSeriesPair(ctx, deps, fredSeriesHYOAS, fredSeriesIGOAS, regimeOfficialSeriesBudget)
	if hyErr != nil || igErr != nil {
		out.Status = rpc.RegimeStatusError
		switch {
		case hyErr != nil && igErr != nil:
			out.ErrorMessage = "HY OAS: " + hyErr.Error() + "; IG OAS: " + igErr.Error()
		case hyErr != nil:
			out.ErrorMessage = "HY OAS: " + hyErr.Error()
		default:
			out.ErrorMessage = "IG OAS: " + igErr.Error()
		}
		return out
	}
	now := time.Now()
	hy, hyOK := latestSeriesPoint(hyPoints)
	ig, igOK := latestSeriesPoint(igPoints)
	if !hyOK {
		out.FieldsMissing = append(out.FieldsMissing, "hy_oas")
	}
	if !igOK {
		out.FieldsMissing = append(out.FieldsMissing, "ig_oas")
	}
	if !hyOK && !igOK {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "credit spreads: no usable FRED observations"
		return out
	}
	if hyOK {
		out.HYOAS = new(hy.Value)
		out.HYOASQuality = officialDailyQuality(hy.Date, "FRED "+fredSeriesHYOAS+" HY OAS")
		out.AsOfDate = hy.Date.Format("2006-01-02")
		if lagged, ok := laggedSeriesPoint(hyPoints, 20); ok {
			chg := hy.Value - lagged.Value
			out.HY20DChange = &chg
		}
	}
	if igOK {
		out.IGOAS = new(ig.Value)
		out.IGOASQuality = officialDailyQuality(ig.Date, "FRED "+fredSeriesIGOAS+" IG OAS")
		if out.AsOfDate == "" || ig.Date.Before(hy.Date) {
			out.AsOfDate = ig.Date.Format("2006-01-02")
		}
	}
	if hyOK && igOK {
		spread := hy.Value - ig.Value
		out.HYIGSpread = &spread
		out.SpreadQuality = officialDerivedQuality(minTime(hy.Date, ig.Date), "HY OAS minus IG OAS")
	}
	if hyOK && igOK && hy.Date.Format("2006-01-02") != ig.Date.Format("2006-01-02") {
		out.FieldsMissing = append(out.FieldsMissing, "series_date_mismatch")
	}
	if out.HYOAS == nil {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "credit spreads: HY OAS missing; cannot band"
		return out
	}
	out.Status = statusForOfficialDaily(hy.Date, now)
	return out
}

func fetchRegimeSeriesPair(ctx context.Context, deps *regimeDeps, leftID, rightID string, budget time.Duration) ([]regimeSeriesPoint, error, []regimeSeriesPoint, error) {
	type result struct {
		id     string
		points []regimeSeriesPoint
		err    error
	}
	ch := make(chan result, 2)
	fetchOne := func(seriesID string) {
		cctx, cancel := context.WithTimeout(ctx, budget)
		points, err := deps.officialSeries(cctx, seriesID)
		cancel()
		ch <- result{id: seriesID, points: points, err: err}
	}
	go fetchOne(leftID)
	go fetchOne(rightID)

	var leftPoints, rightPoints []regimeSeriesPoint
	var leftErr, rightErr error
	for range 2 {
		select {
		case got := <-ch:
			if got.id == leftID {
				leftPoints, leftErr = got.points, got.err
			} else {
				rightPoints, rightErr = got.points, got.err
			}
		case <-ctx.Done():
			if leftErr == nil && leftPoints == nil {
				leftErr = ctx.Err()
			}
			if rightErr == nil && rightPoints == nil {
				rightErr = ctx.Err()
			}
			return leftPoints, leftErr, rightPoints, rightErr
		}
	}
	return leftPoints, leftErr, rightPoints, rightErr
}

const (
	fredSeriesCP3M    = "RIFSPPFAAD90NB"
	fredSeriesTBill3M = "DTB3"
)

const fundingStressNotes = "Funding stress proxy from the OFR FSI source set: 90-day AA financial commercial paper rate minus 13-week Treasury bill bank-discount rate. The commercial-paper leg comes from the Federal Reserve Commercial Paper Data Download Program; the bill leg comes from U.S. Treasury Daily Treasury Bill Rates. Units are basis points. Default heuristic bands: <25 bp green, 25-75 bp yellow, >75 bp red. This is a slow daily funding/liquidity check, not an intraday funding-stress detector. Confirmation gate: the 75 bp red level is the depth gate; a fresh red confirms after 1 session."

func fetchRegimeFundingStress(ctx context.Context, deps *regimeDeps) rpc.RegimeFundingStress {
	out := rpc.RegimeFundingStress{
		Notes:  fundingStressNotes,
		Source: "Federal Reserve Commercial Paper DDP + U.S. Treasury Daily Treasury Bill Rates",
	}
	if deps == nil || deps.officialSeries == nil {
		out.Status = rpc.RegimeStatusUnavailable
		out.ErrorMessage = "funding stress: no official funding series fetcher configured"
		return out
	}
	cpPoints, cpErr, tbPoints, tbErr := fetchRegimeSeriesPair(ctx, deps, fredSeriesCP3M, fredSeriesTBill3M, regimeOfficialSeriesBudget)
	if cpErr != nil || tbErr != nil {
		out.Status = rpc.RegimeStatusError
		switch {
		case cpErr != nil && tbErr != nil:
			out.ErrorMessage = "CP 3M: " + cpErr.Error() + "; T-bill 3M: " + tbErr.Error()
		case cpErr != nil:
			out.ErrorMessage = "CP 3M: " + cpErr.Error()
		default:
			out.ErrorMessage = "T-bill 3M: " + tbErr.Error()
		}
		return out
	}
	now := time.Now()
	cp, cpOK := latestSeriesPoint(cpPoints)
	tb, tbOK := latestSeriesPoint(tbPoints)
	if !cpOK {
		out.FieldsMissing = append(out.FieldsMissing, "cp_3m_rate")
	}
	if !tbOK {
		out.FieldsMissing = append(out.FieldsMissing, "tbill_3m_rate")
	}
	if !cpOK || !tbOK {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "funding stress: CP or T-bill observation missing; cannot compute spread"
		return out
	}
	out.CP3M = new(cp.Value)
	out.TBill3M = new(tb.Value)
	spread := (cp.Value - tb.Value) * 100
	out.SpreadBps = &spread
	asOf := minTime(cp.Date, tb.Date)
	out.AsOfDate = asOf.Format("2006-01-02")
	out.CP3MQuality = officialDailyQuality(cp.Date, "Federal Reserve Commercial Paper DDP RIFSPPFAAD90_N.B")
	out.TBill3MQuality = officialDailyQuality(tb.Date, "U.S. Treasury Daily Treasury Bill Rates 13-week bank discount")
	out.SpreadQuality = officialDerivedQuality(asOf, "90-day AA financial CP minus 13-week Treasury bill")
	if cp.Date.Format("2006-01-02") != tb.Date.Format("2006-01-02") {
		out.FieldsMissing = append(out.FieldsMissing, "series_date_mismatch")
	}
	out.Status = statusForOfficialDaily(asOf, now)
	return out
}

const usdJpyNotes = "USD/JPY exchange rate. Spec thresholds: stable or <1% weekly move (green); 1-2% weekly yen strength i.e. USD/JPY falling (yellow); >2% in 3 days or >3% in a week (red). Speed of move matters more than absolute level; August 2024 carry unwind played out in 3 sessions. Daemon returns last + close 7 trading days ago so the consumer can compute weekly_change_pct themselves. Source: IBKR CASH/IDEALPRO FX (Symbol=USD, Currency=JPY, SecType=CASH) — routed via the dotted-pair classifier. If the gateway has no live/frozen FX tick, the row falls back to the latest HMDS MIDPOINT daily close and reports Status=stale; it is unavailable only when both the tick and midpoint history are unusable. Confirmation gate: speed is the depth — a fresh >= 2% weekly yen move confirms after 1 session."

// USDJPYLookbackDays is the calendar-day window passed to the HMDS
// history fetch when computing the 7-trading-day close for USD/JPY.
// FX trades 24/5 so 7 trading days = 7 weekday FX sessions. 14
// calendar days covers 7 FX sessions even when a Monday or Friday
// bank holiday interrupts the count (US: MLK Day, Memorial Day,
// Labor Day, Thanksgiving, etc. all fall on Mondays and clip one
// US-tradable FX day). Widened from 12 to 14 in v0.23.0
// (commit 02aba13).
const USDJPYLookbackDays = 14

func fetchRegimeUSDJPY(ctx context.Context, deps *regimeDeps) rpc.RegimeUSDJPY {
	out := rpc.RegimeUSDJPY{
		Symbol: "USD.JPY",
		Notes:  usdJpyNotes,
	}
	now := time.Now()

	// briefSnapshotPrice routes "USD.JPY" through pkg/ibkr.classifySymbol
	// to CASH/IDEALPRO/JPY (see commit 6ac583c). A 0 result here means
	// either the gateway has no FX entitlement for this account or there
	// is no frozen tick to fall back on. Do not fabricate a live value;
	// if HMDS can provide daily MIDPOINT history below, rank from that as
	// stale daily context instead.
	last, _, dt := boundedSnapshot(ctx, deps, "USD.JPY", 5*time.Second)

	// Latest and 7-trading-days-ago close. FX history uses MIDPOINT bars
	// (defaultHistoricalWhat for CASH). See USDJPYLookbackDays for
	// the calendar-day window's holiday-clipping rationale.
	hctx, hcancel := context.WithTimeout(ctx, 20*time.Second)
	bars, err := deps.history(hctx, "USD.JPY", USDJPYLookbackDays)
	hcancel()
	var latestHistoryClose float64
	var latestHistoryQuality *rpc.Quality
	switch {
	case err != nil:
		warnDeps(deps, "regime: USD.JPY history fetch failed: %v", err)
	case len(bars) < 8:
		warnDeps(deps, "regime: USD.JPY history insufficient bars: got %d, need 8", len(bars))
	default:
		latest := bars[len(bars)-1]
		if latest.Close > 0 {
			latestHistoryClose = latest.Close
			latestHistoryQuality = derivedQuality(historyBarAsOf(latest, now), "USD.JPY latest MIDPOINT daily close fallback")
		}
		// bars are oldest-first; pick the close from 7 trading days
		// before the most recent close.
		idx := len(bars) - 8
		if idx >= 0 {
			c7 := bars[idx].Close
			if c7 > 0 {
				out.Close7DAgo = new(c7)
				out.Close7DAgoQuality = derivedQuality(historyBarAsOf(bars[idx], now), "USD.JPY MIDPOINT bar t-7")
			}
		}
	}

	if last > 0 {
		out.Last = new(last)
		out.LastQuality = firmTickQuality(now, dt, "USD.JPY CASH tick (IDEALPRO)")
		out.DataType = dt
	} else if latestHistoryClose > 0 {
		out.Last = new(latestHistoryClose)
		out.LastQuality = latestHistoryQuality
		out.DataType = regimeFreshnessDailyClose
	} else {
		out.Status = rpc.RegimeStatusUnavailable
		out.ErrorMessage = "USD.JPY: gateway delivered no FX tick and HMDS midpoint fallback was unavailable"
		return out
	}

	if out.Close7DAgo != nil && out.Last != nil {
		chg := (*out.Last - *out.Close7DAgo) / *out.Close7DAgo * 100
		out.WeeklyChange = &chg
	}

	if last > 0 && rpc.IsLiveDataType(dt) {
		out.Status = rpc.RegimeStatusOK
	} else {
		out.Status = rpc.RegimeStatusStale
	}
	if out.Close7DAgo == nil {
		out.FieldsMissing = append(out.FieldsMissing, "close_7d_ago")
	}
	if out.WeeklyChange == nil {
		out.FieldsMissing = append(out.FieldsMissing, "weekly_change_pct")
	}
	return out
}

const gammaNotes = "Combined SPY+SPX dealer gamma context. SPY and SPX are reported as separate per-index γ-zero readings because their price scales differ; the combined top level intentionally has no spot, zero_gamma, gap_pct, or gamma_sign. Regime thresholds are applied to each per_index entry: spot >2% above γ-zero is green/stabilizing, within ±2% is yellow/transition, and below γ-zero is red/amplifying; when no crossing exists, a wholly long-γ sweep is green and a wholly short-γ sweep is red. The combined row uses per-index agreement and exposure weighting: both green => green, both red => red, a mixed book is red when the dominant/equal gamma exposure is red and yellow otherwise, no usable per-index profile => unranked. Methodology v3 (`bs-gamma-profile-v3-stickymoneyness-0dte-split`): BS gamma profile over 6 nearest non-0DTE-post-settlement expirations × the nearest 80 listed strikes per expiry inside the ±10% candidate window. The sweep reprices each leg's IV at the scenario-spot's moneyness via a per-expiry quadratic skew curve fitted at snapshot time — sticky-moneyness rather than sticky-IV. Curves that fail to fit fall back to sticky-IV for that expiry only and appear in `warning_details`. Each per-index envelope carries 0DTE, 1-7 DTE, and term γ-zero buckets because 0DTE flow can mask weekly/monthly positioning. Disclosure: the signed γ-zero applies the SqueezeMetrics-2017 \"dealers long calls, short puts\" convention, which the literature has materially deprecated since 2022 (SqueezeMetrics DDOI, SpotGamma TRACE, Glassnode taker-flow GEX). Treat the signed level as a regime hint; the dealer-hedging magnitude (`gamma_total_abs`, convention `sign-agnostic`) is a sign-convention agnostic gross gamma concentration read and is the more robust surface when customer-flow asymmetry is uncertain (e.g. covered-call ETF supply, autocallable hedging). When the gateway's model-computation engine is idle, the compute falls back to Black-Scholes Newton-Raphson on each option's bid/ask mid or prior-session close to back-solve IV; legs using the fallback are counted in derived_iv_legs. First regime call of an NY trading day auto-kicks the heavy compute; subsequent calls return the cached result. The envelope's summary, per_index, gamma_total_abs, and top_strikes are the primary agent/tooling surface. Confirmation gate: gamma confirms only from a compute made during the current NY trading date with spot at least 0.5% below gamma-zero (a wholly short-gamma profile is day-one eligible); a prior-date cache serves status=stale, stays visible, and warns only."

func fetchRegimeGamma(ctx context.Context, s *Server) rpc.RegimeGammaZero {
	out := rpc.RegimeGammaZero{Notes: gammaNotes}
	// Reuse the existing handler: daemon startup normally prewarms the cache,
	// and kickOrJoin owns any needed RTH refresh. WaitMs=0 returns the served
	// last-good immediately while a 15-minute soft-TTL refresh runs behind it;
	// without a last-good it returns the current cold/computing state.
	envelope, err := s.handleGammaZeroSPX(ctx, &rpc.Request{
		Method: rpc.MethodGammaZeroSPX,
		Params: json.RawMessage(`{}`),
	})
	if err != nil {
		out.Status = rpc.RegimeStatusError
		out.Envelope = rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusError, Error: err.Error()}
		return out
	}
	out.Envelope = *envelope
	switch envelope.Status {
	case rpc.GammaZeroStatusReady:
		out.Status = rpc.RegimeStatusOK
		// Cadence-relative staleness: gamma is intraday-capable during
		// option RTH and its inputs roll at the open (the cached profile
		// still contains the prior day's now-expired 0DTE exposure), so a
		// compute from a prior NY TRADING DATE is overdue — status stale,
		// band still visible for awareness, never confirmation-eligible.
		// A Friday-evening compute read on Saturday stays fresh: no newer
		// observation can exist until Monday. This closes the 2026-06-12
		// path where a 22:19-prior-evening cache confirmed
		// "contemporaneous" stress.
		if envelope.Result != nil && !envelope.Result.AsOf.IsZero() &&
			nyTradingSessionKey(nyTime(envelope.Result.AsOf)) != nyTradingSessionKey(nyDateNow()) {
			out.Status = rpc.RegimeStatusStale
		}
		if envelope.Result != nil {
			// Both scalars derive from the same compute, so AsOf is the
			// compute's completion timestamp. ZeroGamma is modelled (the
			// BS sweep's interpolation); GammaTotalAbs is the firmer
			// sign-agnostic notional aggregated from OI+IV observations
			// — still an estimate because per-leg coverage varies.
			//
			// When DerivedIVLegs == PricedLegCount, every IV in the compute came
			// from the BS-IV Newton-Raphson fallback against option quote
			// mids or prior-session closes. The Source string disclosure
			// surfaces that to the --explain reader without making the
			// fallback look like a live model tick.
			source := envelope.Result.Method
			if r := envelope.Result; r.DerivedIVLegs > 0 && r.DerivedIVLegs == r.PricedLegCount {
				source = r.Method + " · BS-IV from option quote/close fallback"
			}
			out.ZeroGammaQuality = modelledQuality(gammaInputAsOf(envelope.Result), source)
			out.GammaTotalAbsQuality = derivedQuality(envelope.Result.AsOf, "BS-sweep |Γ|·OI·spot²")
			if envelope.Result.Scope != rpc.GammaZeroScopeCombined {
				out.HorizonAgreement = classifyHorizonAgreement(envelope.Result)
			}
		}
	case rpc.GammaZeroStatusComputing:
		out.Status = rpc.RegimeStatusComputing
	case rpc.GammaZeroStatusCold:
		// Cold means no usable last-good exists and no compute is in flight.
		// Automatic refresh is deliberately not due off-hours, so map it to
		// Unavailable just as breadth maps its own cold state below.
		out.Status = rpc.RegimeStatusUnavailable
	case rpc.GammaZeroStatusError:
		out.Status = rpc.RegimeStatusError
	default:
		out.Status = rpc.RegimeStatusError
	}
	return out
}

const breadthNotes = "S&P 500 breadth — the daemon computes two SMA readings and the new-52-week-highs/lows count locally from the 500 constituent daily closes (IBKR doesn't redistribute the underlying S&P DJI / NYSE breadth indices on retail subscriptions). Refresh runs once per US trading day after the equity-session close plus a 35-minute settle pad (normally 16:35 ET). Method token: constituent-fanout-50/200dma+nh-v2. The 50-day reading (`pct_above_50dma`) keeps the spec's bands: >55 green / 40-55 yellow / <40 with SPX within 3% of 52-week high is the textbook late-cycle divergence (red). The 200-day reading (`pct_above_200dma`) uses 60/40 bands calibrated to the post-Mag-7 era: >60 green / 40-60 yellow / <40 red (the StockCharts 70/30 default fires red far too often in this regime). New-highs/lows surface as a sub-signal: when SPX is near highs and `net_new_highs_pct` is near zero or negative, that's the classic narrow-rally pattern — a small set of mega-caps carrying the index while the median name is rolling over. Confirmation gate: a red confirms at <= 38% above-50DMA for 2 sessions (or <= 30% day one)."

func fetchRegimeBreadth(ctx context.Context, s *Server) rpc.RegimeBreadth {
	out := rpc.RegimeBreadth{Notes: breadthNotes}
	envelope, err := s.handleBreadthSPX(ctx, &rpc.Request{
		Method: rpc.MethodBreadthSPX,
		Params: json.RawMessage(`{}`),
	})
	if err != nil {
		out.Status = rpc.RegimeStatusError
		return out
	}
	out.Envelope = *envelope

	// State on the envelope is the single source of truth — replaces
	// the pre-v0.27.3 side-channel that called s.breadth.IsRefreshing()
	// separately and tried to disambiguate (value==0 AND history==[])
	// heuristically. A warm refresh keeps the last good snapshot ranked
	// as ready and exposes progress through envelope.Refreshing.
	switch envelope.State {
	case rpc.BreadthStateComputing:
		out.Status = rpc.RegimeStatusComputing
		return out
	case rpc.BreadthStateCold, rpc.BreadthStateDegraded:
		out.Status = rpc.RegimeStatusUnavailable
		return out
	}
	// State == "ready" — fall through to the populated-envelope path.

	// The value is computed (not a live gateway tick). derivedQuality
	// is the right shelf — it tags FreshnessClass=derived,
	// Confidence=estimate so renderers don't mistake this for a
	// firm-tick reading.
	out.ValueQuality = derivedQuality(envelope.AsOf, envelope.Method)
	// Echo the four sub-fields onto the regime row so a consumer
	// doesn't have to dig into the nested envelope for the standard
	// breadth view that informs the band.
	out.PctAbove50DMA = envelope.PctAbove50DMA
	out.PctAbove200DMA = envelope.PctAbove200DMA
	out.NewHighsToday = envelope.NewHighsToday
	out.NewLowsToday = envelope.NewLowsToday
	out.NetNewHighsPct = envelope.NetNewHighsPct
	out.Status = rpc.RegimeStatusOK
	// Staleness is session-based. A Friday close remains current
	// through the weekend and until Monday's post-close refresh window;
	// the wall-clock AsOf is only a fallback for older envelopes that
	// predate SessionKey on the wire.
	if breadthEnvelopeStale(envelope, time.Now()) {
		out.Status = rpc.RegimeStatusStale
	}
	return out
}

func breadthEnvelopeStale(envelope *rpc.BreadthSPXResult, now time.Time) bool {
	if envelope == nil {
		return true
	}
	if envelope.SessionKey != "" {
		return envelope.SessionKey != spx.CompletedSessionKey(now)
	}
	return now.Sub(envelope.AsOf) > 30*time.Hour
}

// ----------------------------------------------------------------------------
// Helpers shared across the per-indicator fetchers.

func officialDailyQuality(date time.Time, source string) *rpc.Quality {
	return &rpc.Quality{
		AsOf:           date,
		FreshnessClass: rpc.FreshnessDerived,
		Confidence:     rpc.ConfidenceFirm,
		Source:         source,
	}
}

func officialDerivedQuality(date time.Time, source string) *rpc.Quality {
	return &rpc.Quality{
		AsOf:           date,
		FreshnessClass: rpc.FreshnessDerived,
		Confidence:     rpc.ConfidenceFirm,
		Source:         source,
	}
}

func statusForOfficialDaily(date time.Time, now time.Time) string {
	if date.IsZero() {
		return rpc.RegimeStatusError
	}
	if seriesObservationAge(date, now) > 7*24*time.Hour {
		return rpc.RegimeStatusStale
	}
	return rpc.RegimeStatusOK
}

func minTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}

// warnDeps is the per-deps log shim. Production deps wire logWarnf to
// the daemon logger; tests inject a capture closure; nil is a no-op
// for the rare caller that doesn't care.
func warnDeps(d *regimeDeps, format string, args ...any) {
	if d == nil || d.logWarnf == nil {
		return
	}
	d.logWarnf(format, args...)
}

// firmTickQuality builds a Quality for a value that came directly from
// a gateway tick. FreshnessClass tracks live vs frozen based on the
// data-type the gateway labelled the subscription with; Confidence is
// "firm" because the value is a direct gateway measurement (not
// computed from history or a model).
func firmTickQuality(at time.Time, dataType, source string) *rpc.Quality {
	cls := rpc.FreshnessLive
	if !rpc.IsLiveDataType(dataType) {
		cls = rpc.FreshnessFrozen
	}
	return &rpc.Quality{
		AsOf:           at,
		FreshnessClass: cls,
		Confidence:     rpc.ConfidenceFirm,
		Source:         source,
	}
}

// derivedQuality builds a Quality for a value computed from historical
// bars (e.g. a 50-day SMA or a 252-bar max). The freshness class is
// "derived" because the value reflects the most recent close anchoring
// the bar fetch, not a live tick; confidence is "estimate" — a fallback
// when a firm tick was unavailable or always-derived by methodology.
func derivedQuality(at time.Time, source string) *rpc.Quality {
	return &rpc.Quality{
		AsOf:           at,
		FreshnessClass: rpc.FreshnessDerived,
		Confidence:     rpc.ConfidenceEstimate,
		Source:         source,
	}
}

func historyBarAsOf(bar ibkrlib.HistoricalBar, fallback time.Time) time.Time {
	if !bar.Time.IsZero() {
		return bar.Time
	}
	raw := strings.TrimSpace(bar.Date)
	for _, layout := range []string{"2006-01-02", "20060102"} {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t
		}
	}
	return fallback
}

// historyBarSessionDate returns the bar's trading-session date label as
// YYYY-MM-DD. The HMDS Date string is authoritative (it is the session
// label as the gateway reported it); the date of bar.Time in its own
// location is the fallback. No zone conversion happens here — shifting
// a UTC-midnight stamp into market time would relabel the bar onto the
// prior NY date. "" means unlabelable.
func historyBarSessionDate(bar ibkrlib.HistoricalBar) string {
	raw := strings.TrimSpace(bar.Date)
	for _, layout := range []string{"2006-01-02", "20060102"} {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t.Format("2006-01-02")
		}
	}
	if !bar.Time.IsZero() {
		return bar.Time.Format("2006-01-02")
	}
	return ""
}

// modelledQuality builds a Quality for a value produced by a model
// (currently only the gamma compute's zero-flip estimate). The Source
// field carries the method token so consumers can deep-link to the
// methodology disclosure without re-reading the spec doc.
func modelledQuality(at time.Time, method string) *rpc.Quality {
	return &rpc.Quality{
		AsOf:           at,
		FreshnessClass: rpc.FreshnessModelled,
		Confidence:     rpc.ConfidenceProxy,
		Source:         method,
	}
}

// averageClose returns the simple average of the last N daily closes
// from a bars slice (oldest-first). Returns 0 if the slice has
// fewer than N rows so the caller can distinguish "computed" from
// "insufficient data."
func averageClose(bars []ibkrlib.HistoricalBar, n int) float64 {
	if len(bars) < n {
		return 0
	}
	sum := 0.0
	tail := bars[len(bars)-n:]
	for _, b := range tail {
		sum += b.Close
	}
	return sum / float64(n)
}

// maxHigh returns the largest High over the last N daily bars
// (oldest-first). If the slice has fewer than N rows the whole slice
// is scanned — partial data is still useful for the 52w-high fallback
// where the indicator needs a best-effort upper bound. Returns 0 only
// on an empty slice.
func maxHigh(bars []ibkrlib.HistoricalBar, n int) float64 {
	if len(bars) == 0 {
		return 0
	}
	tail := bars
	if len(bars) > n {
		tail = bars[len(bars)-n:]
	}
	hi := 0.0
	for _, b := range tail {
		if b.High > hi {
			hi = b.High
		}
	}
	return hi
}

// classifyHorizonAgreement compares the gamma compute's three
// horizon-bucketed regimes (0DTE, 1-7, term) and names how they relate.
// A bucket can get its regime from a zero-gamma gap or, when no crossing
// exists, from its one-sided GammaSign. Returns one of the documented
// HorizonAgreement strings — see rpc.RegimeGammaZero.HorizonAgreement
// for the meanings.
//
// v3 split semantics — three buckets instead of v2's two:
//
//	"all_long"             every bucket is long-gamma
//	"all_short"            every bucket is short-gamma
//	"all_transition"       every bucket is within ±2% of its γ-zero
//	"diverge:0dte_vs_term" 0DTE and term sit on opposite sides of spot
//	                       (highest-information case — short-fuse flow
//	                       disagrees with monthly positioning)
//	"diverge:partial"      some mixed pattern across the three buckets
//	                       that isn't the 0DTE-vs-term split — e.g.
//	                       1-7 alone disagrees, or only two buckets
//	                       have crossings and they disagree
//	"0dte_only"            only the 0DTE bucket has a usable regime
//	"1to7_only"            only the 1-7 bucket has a usable regime
//	"term_only"            only the term bucket has a usable regime
//	""                     no bucket has a usable regime
func classifyHorizonAgreement(c *rpc.GammaZeroComputed) string {
	if c == nil || c.SpotUnderlying <= 0 {
		return ""
	}
	buckets := []struct {
		name   string
		regime string
	}{
		{"0dte", rpc.GammaBucketRegime(c.SpotUnderlying, c.ZeroGamma0DTE, c.GammaSign0DTE)},
		{"1to7", rpc.GammaBucketRegime(c.SpotUnderlying, c.ZeroGamma1to7, c.GammaSign1to7)},
		{"term", rpc.GammaBucketRegime(c.SpotUnderlying, c.ZeroGammaTerm, c.GammaSignTerm)},
	}
	var usable []struct {
		name   string
		regime string
	}
	for _, b := range buckets {
		if b.regime != "" {
			usable = append(usable, b)
		}
	}
	switch len(usable) {
	case 0:
		return ""
	case 1:
		return usable[0].name + "_only"
	}
	first := usable[0].regime
	allSame := true
	for _, b := range usable[1:] {
		if b.regime != first {
			allSame = false
			break
		}
	}
	if allSame && len(usable) == 3 {
		return "all_" + strings.TrimSuffix(first, "_gamma")
	}
	if buckets[0].regime != "" && buckets[2].regime != "" && buckets[0].regime != buckets[2].regime {
		return "diverge:0dte_vs_term"
	}
	if !allSame {
		return "diverge:partial"
	}
	return strings.TrimSuffix(first, "_gamma") + "_only"
}
