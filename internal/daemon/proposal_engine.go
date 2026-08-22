package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	proposalEventFileVersion = 1
	proposalOrderSource      = "trade_proposals"
)

type proposalEngine struct {
	mu      sync.Mutex
	server  *Server
	store   *proposalStore
	cadence time.Duration
	now     func() time.Time
	// scope resolves the connected broker session identity. Test seam;
	// nil falls back to server.currentBrokerStateScope.
	scope    func() brokerStateScope
	snapshot rpc.TradeProposalSnapshot
	// ignored is keyed by scopedIgnoreKey (account|mode|proposal key):
	ignored map[string]struct{}
	// refreshFailStreak counts consecutive refreshes that ended on a
	// streak start time and the latest failure's blocker codes alongside.
	// Observability only — Run's backoff keeps its own counter — but
	// without it a preserved-snapshot outage is invisible: the failures
	// return err == nil and the served as_of silently freezes (observed
	refreshFailStreak int
	refreshFailSince  time.Time
	refreshFailCodes  []string
	trailVolCache     map[string]cachedStockTrailVolatility
	// kick wakes Run for an immediate refresh (gateway reconnect). Lazily
	// need no extra setup. Buffered: senders never block.
	kick chan struct{}
}

type cachedStockTrailVolatility struct {
	value     stockTrailVolatility
	fetchedAt time.Time
}

type stockTrailVolatility struct {
	ATR14          *float64
	ATRPct         *float64
	AsOf           time.Time
	MissingReasons []string
}

const (
	stockTrailSizingMethod       = "atr-spread-policy"
	stockTrailSizingVersion      = "stock-trail-sizing-v1"
	stockTrailATRMultiplier      = 1.2
	stockTrailSpreadMultiplier   = 3.0
	stockTrailVolatilityDays     = 45
	stockTrailVolatilityTimeout  = 4 * time.Second
	stockTrailVolatilityCacheTTL = 4 * time.Hour
	optionExitQuoteTimeout       = 4 * time.Second
)

type proposalEvent struct {
	Version            int                                 `json:"version"`
	At                 time.Time                           `json:"at"`
	Type               string                              `json:"type"`
	Key                string                              `json:"key,omitempty"`
	Revision           string                              `json:"revision,omitempty"`
	Bucket             string                              `json:"bucket,omitempty"`
	AccountID          string                              `json:"account_id,omitempty"`
	AccountMode        string                              `json:"account_mode,omitempty"`
	PolicyID           string                              `json:"policy_id,omitempty"`
	PolicyVersion      int                                 `json:"policy_version,omitempty"`
	PolicyFingerprint  rpc.Fingerprint                     `json:"policy_fingerprint,omitzero"`
	PreviewTokenID     string                              `json:"preview_token_id,omitempty"`
	OrderRef           string                              `json:"order_ref,omitempty"`
	Message            string                              `json:"message,omitempty"`
	Reason             string                              `json:"reason,omitempty"`
	SourceFingerprints rpc.TradeProposalSourceFingerprints `json:"source_fingerprints,omitzero"`
}

func (s *Server) installProposalEngine() {
	e := &proposalEngine{
		server:  s,
		store:   &proposalStore{},
		cadence: s.cfg.AutoTrade.WithDefaults().ProposalCadenceDuration(),
		now:     s.now,
		ignored: map[string]struct{}{},
	}
	s.tradeProposals = e
}

// proposalRefreshRetryBase is the first quick-retry delay after a refresh
// that failed on a transient session condition (gateway still connecting,
// account/positions fetch failure, no concrete account identity yet). It
// doubles per consecutive transient failure and caps at the sustained-outage
// ceiling. Without it the startup refresh races the gateway connect and the
// cached "IBKR connection unavailable" blocker is served for a full cadence
const proposalRefreshRetryBase = 30 * time.Second

// proposalRefreshBackoffCap bounds sustained-failure retries independently of
// the healthy cadence. With a 30s clean cadence, capping failure waits at the
// cadence would mean a blocked attempt (and its warn line) twice a minute for
// the whole length of a gateway outage; capping at 15m keeps outage logs quiet.
// Post-outage recovery latency does not ride on this cap — the gateway
// reconnect kicks the loop directly (see Kick).
const proposalRefreshBackoffCap = 15 * time.Minute

func (e *proposalEngine) Run(ctx context.Context) {
	if e == nil {
		return
	}
	failures := 0
	for {
		snap, err := e.Refresh(ctx, false)
		if err != nil || proposalRefreshTransient(snap) {
			failures++
		} else {
			failures = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-e.kickCh():
			// A fresh gateway handshake invalidates the escalated wait:
			// restart the quick-retry ladder so a transient failure on the
			// immediate post-reconnect refresh waits 30s, not the
			// accumulated outage backoff. The logging streak in
			// noteRefreshOutcome is deliberately untouched so the
			// "recovered after N blocked attempts" line still closes the
			// outage trail.
			failures = 0
		case <-time.After(proposalRefreshWait(e.cadence, failures)):
		}
	}
}

// Kick wakes Run for an immediate refresh, dropping the wake when one is
// already pending. Called from postConnectSetup after RequestAccountUpdates
func (e *proposalEngine) Kick() {
	if e == nil {
		return
	}
	select {
	case e.kickCh() <- struct{}{}:
	default:
	}
}

func (e *proposalEngine) kickCh() chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.kick == nil {
		e.kick = make(chan struct{}, 1)
	}
	return e.kick
}

// refreshBackoff paces a broker-state engine's automatic refresh retries
// clean refresh (failures == 0), then an escalating base<<(failures-1) backoff
// while refreshes keep failing, capped at max(cadence, backoffCap) so a
// slow-cadence override never retries faster on failure than on success. The
// proposal and opportunity engines share it so both broker-state feeds throttle
func refreshBackoff(cadence, base, backoffCap time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return cadence
	}
	if cadence <= 0 {
		cadence = base
	}
	ceil := max(cadence, backoffCap)
	wait := base << (failures - 1)
	if wait <= 0 || wait > ceil {
		return ceil
	}
	return wait
}

// proposalRefreshWait returns the pause before the next automatic refresh:
// the full cadence after a clean refresh, an escalating 30s/1m/2m/… backoff
// while refreshes keep failing on transient session conditions, capped at
// proposalRefreshBackoffCap (or the cadence when that is longer). See
// refreshBackoff, which the opportunity engine shares.
func proposalRefreshWait(cadence time.Duration, failures int) time.Duration {
	return refreshBackoff(cadence, proposalRefreshRetryBase, proposalRefreshBackoffCap, failures)
}

// proposalPositionsUnprimed reports whether an empty positions list can be
// cache (no error) until the account-updates portfolio burst lands, so
// only the stream's completed account-scoped receipt proves flatness —
// the account summary's gross position value cannot carry that burden,
// because an absent wire field flattens to numeric zero and would bless
// tripwire against a receipt that raced a stale projection. Generating
// "no proposals" from an untrusted empty list would replace a last-good
func proposalPositionsUnprimed(pos *rpc.PositionsResult, acct *rpc.AccountResult, health ibkrlib.PortfolioStreamHealth) bool {
	if pos == nil || len(pos.Stocks) != 0 || len(pos.Options) != 0 {
		return false
	}
	if health.InitialCompletedAt.IsZero() {
		return true
	}
	return acct != nil && acct.GrossPositionValue > 0
}

// proposalRefreshTransient reports whether the installed snapshot is
// blocked on a condition the next broker heartbeat can clear (connection
// settling). Refresh failures that preserve a last-good snapshot return
// un-pinned data-only gateway stays account_identity_unscoped forever)
// no-broker-call pass.
func proposalRefreshTransient(snap rpc.TradeProposalSnapshot) bool {
	for _, b := range snap.Blockers {
		switch b.Code {
		case "account_identity_unscoped", "account_unavailable", "positions_unavailable", "positions_pending", "proposal_scope_mismatch":
			return true
		}
	}
	return false
}

func (e *proposalEngine) Snapshot(show bool) rpc.TradeProposalSnapshot {
	if e == nil {
		return emptyProposalSnapshot(time.Now().UTC())
	}
	e.mu.Lock()
	snap := cloneProposalSnapshot(e.snapshot)
	e.mu.Unlock()
	if snap.Kind == "" {
		snap = emptyProposalSnapshot(e.clock())
	}
	// Serve guard: proposals are generated from one account/mode session
	// and must never surface under another (paper proposals shown on a
	// live session was the originating incident). Proposal-free shells
	// carry session-independent blockers and pass through unchanged.
	if len(snap.Proposals) > 0 {
		scope := e.currentScope()
		if blockers := proposalScopeBlockers(snap.AccountID, snap.AccountMode, scope); len(blockers) > 0 {
			shell := emptyProposalSnapshot(e.clock())
			if brokerScopeConcrete(scope) {
				shell.AccountID = scope.Account
				shell.AccountMode = scope.Mode
			}
			shell.Blockers = blockers
			return shell
		}
	}
	if show {
		e.appendShownEvents(snap)
	}
	return snap
}

// proposalRefreshWarnStreak is how many consecutive transient-failed
// refreshes after a daemon start routinely race the gateway connect and
// failure on keeps boot logs clean while a real outage surfaces within
const proposalRefreshWarnStreak = 3

func (e *proposalEngine) Refresh(ctx context.Context, show bool) (rpc.TradeProposalSnapshot, error) {
	snap, err := e.refresh(ctx, show)
	e.noteRefreshOutcome(snap, err)
	return snap, err
}

// noteRefreshOutcome advances the transient-failure streak after every
// refresh, regardless of caller, and emits the throttled log trail.
// Transient failures preserve the last-good snapshot and return err == nil
// — the blocker codes are the only signal — so this is where a stalled
// panel becomes diagnosable. Quiet below proposalRefreshWarnStreak, then
// one warn per failed attempt: Run's backoff (refreshBackoff) paces those at
// 30s/1m/2m/… doubling up to proposalRefreshBackoffCap, so a persistent outage
// logs once per escalation and then once per cap (15m), not once per poll. One
// info line closes the streak when a refresh finally lands.
func (e *proposalEngine) noteRefreshOutcome(snap rpc.TradeProposalSnapshot, err error) {
	failed := err != nil || proposalRefreshTransient(snap)
	now := e.clock()
	e.mu.Lock()
	if !failed {
		streak, since := e.refreshFailStreak, e.refreshFailSince
		e.refreshFailStreak, e.refreshFailSince, e.refreshFailCodes = 0, time.Time{}, nil
		e.mu.Unlock()
		if streak >= proposalRefreshWarnStreak && e.server != nil {
			e.server.infof("trade proposals: refresh recovered after %d blocked attempts over %s", streak, now.Sub(since).Round(time.Second))
		}
		return
	}
	e.refreshFailStreak++
	if e.refreshFailStreak == 1 {
		e.refreshFailSince = now
	}
	e.refreshFailCodes = proposalBlockerCodes(snap, err)
	streak, since, codes := e.refreshFailStreak, e.refreshFailSince, e.refreshFailCodes
	e.mu.Unlock()
	if streak < proposalRefreshWarnStreak || e.server == nil {
		return
	}
	e.server.warnf("trade proposals: refresh blocked %d consecutive times over %s (codes: %s); serving snapshot as_of %s (%s old)",
		streak, now.Sub(since).Round(time.Second), strings.Join(codes, ","),
		snap.AsOf.Format(time.RFC3339), now.Sub(snap.AsOf).Round(time.Second))
}

// proposalBlockerCodes flattens the installed snapshot's blocker codes for
// the refresh-streak trail; the raw fetch error stands in when a failure
func proposalBlockerCodes(snap rpc.TradeProposalSnapshot, err error) []string {
	var out []string
	for _, b := range snap.Blockers {
		if b.Code != "" && !slices.Contains(out, b.Code) {
			out = append(out, b.Code)
		}
	}
	if len(out) == 0 && err != nil {
		out = append(out, err.Error())
	}
	return out
}

// proposalRefreshHealth is the engine's refresh-streak view for the
// status.health proposals subsystem row.
type proposalRefreshHealth struct {
	Streak     int
	Since      time.Time
	Codes      []string
	ServedAsOf time.Time
}

// RefreshHealth reports the current transient-failure streak and the as_of
func (e *proposalEngine) RefreshHealth() proposalRefreshHealth {
	if e == nil {
		return proposalRefreshHealth{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return proposalRefreshHealth{
		Streak:     e.refreshFailStreak,
		Since:      e.refreshFailSince,
		Codes:      append([]string(nil), e.refreshFailCodes...),
		ServedAsOf: e.snapshot.AsOf,
	}
}

func (e *proposalEngine) refresh(ctx context.Context, show bool) (rpc.TradeProposalSnapshot, error) {
	now := e.clock()
	cfg := e.server.cfg.AutoTrade.WithDefaults()
	autoStatus := e.server.autoTradeStatus()
	if !cfg.ProposalsEnabledResolved() {
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = autoStatus.Policy
		snap.Blockers = []rpc.TradingBlocker{{Code: "proposals_disabled", Message: "manual protection proposals are disabled by config"}}
		if err := e.installSnapshot(snap, show); err != nil {
			return e.Snapshot(false), err
		}
		return snap, nil
	}
	policy, policyStatus := e.server.protectionPolicies.Active()
	if policyStatus.Status == rpc.ProtectionPolicyStatusDrift || policyStatus.Status == rpc.ProtectionPolicyStatusError {
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.Blockers = append([]rpc.TradingBlocker(nil), policyStatus.Blockers...)
		if err := e.installSnapshot(snap, show); err != nil {
			return e.Snapshot(false), err
		}
		if err := e.appendEvent(proposalEvent{At: now, Type: "policy-" + policyStatus.Status, PolicyID: policyStatus.PolicyID, PolicyVersion: policyStatus.PolicyVersion, PolicyFingerprint: policyStatus.Fingerprint, Message: policyStatus.Message}); err != nil {
			return snap, err
		}
		return snap, nil
	}
	// Bind the refresh to the connected session identity before touching
	// any account data. The aggregate "All" (or an empty / multi-account
	// managedAccounts list, or an unknown paper/live mode) is not an
	// account identity — proposals scoped to it would survive paper/live
	// session switches, which is exactly the leak this gate prevents.
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) {
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.Blockers = []rpc.TradingBlocker{proposalScopeUnscopedBlocker(scope)}
		if err := e.installSnapshot(snap, show); err != nil {
			return e.Snapshot(false), err
		}
		return snap, nil
	}
	acct, err := e.server.handleAccountSummary(ctx)
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "account_unavailable", Message: err.Error()}}
		if snap, ok := e.preserveSnapshotOnRefreshFailure(scope, autoStatus, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.AccountID = scope.Account
		snap.AccountMode = scope.Mode
		snap.Blockers = blockers
		if installErr := e.installSnapshot(snap, show); installErr != nil {
			return e.Snapshot(false), installErr
		}
		return snap, err
	}
	var portfolioHealth ibkrlib.PortfolioStreamHealth
	pos, err := e.server.handlePositionsListCaptured(ctx, &rpc.Request{}, &portfolioHealth)
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "positions_unavailable", Message: err.Error()}}
		if snap, ok := e.preserveSnapshotOnRefreshFailure(scope, autoStatus, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.AccountID = scope.Account
		snap.AccountMode = scope.Mode
		snap.Blockers = blockers
		if installErr := e.installSnapshot(snap, show); installErr != nil {
			return e.Snapshot(false), installErr
		}
		return snap, err
	}
	pos = e.server.analysisPositions(pos, now)
	if proposalPositionsUnprimed(pos, acct, portfolioHealth) {
		blockers := []rpc.TradingBlocker{{Code: "positions_pending", Message: "portfolio stream not yet primed; an empty position list needs a completed account-scoped receipt and no contradicting account summary"}}
		if snap, ok := e.preserveSnapshotOnRefreshFailure(scope, autoStatus, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyProposalSnapshot(now)
		snap.AutoTrade = autoStatus
		snap.PolicyStatus = policyStatus
		snap.AccountID = scope.Account
		snap.AccountMode = scope.Mode
		snap.Blockers = blockers
		if err := e.installSnapshot(snap, show); err != nil {
			return e.Snapshot(false), err
		}
		return snap, nil
	}
	accountFP := rpc.BuildAccountFingerprint(acct)
	positionsFP := rpc.BuildPositionsFingerprint(pos, acct.NetLiquidation)
	rulebookPolicy := risk.DefaultRulebookPolicy()
	rulebookFP := rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: rulebookPolicy.FingerprintKey()}
	sources := rpc.TradeProposalSourceFingerprints{Account: &accountFP, Positions: &positionsFP, Rulebook: &rulebookFP}
	if fp, ok := e.regimeFingerprint(ctx); ok {
		sources.Regime = &fp
	}
	marketEvents := e.marketEventsSnapshot(ctx, pos)
	if marketEvents != nil {
		fp := marketEvents.Fingerprint
		if fp.Key == "" {
			fp = rpc.BuildMarketEventsFingerprint(marketEvents)
		}
		sources.MarketEvents = &fp
	}
	proposals, thetaSuppressions := e.generate(ctx, policy, policyStatus, acct, pos, sources, marketEvents, scope, now)
	slices.SortStableFunc(proposals, func(a, b rpc.TradeProposal) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	revision := proposalRevision(policyStatus.Fingerprint, sources, scope, proposals)
	for i := range proposals {
		proposals[i].Rank = i + 1
		proposals[i].Revision = revision
	}
	snap := rpc.TradeProposalSnapshot{
		Kind:               rpc.TradeProposalSnapshotKind,
		SchemaVersion:      rpc.TradeProposalSnapshotSchemaVersion,
		AsOf:               now,
		Revision:           revision,
		AccountID:          scope.Account,
		AccountMode:        scope.Mode,
		PolicyID:           policy.PolicyID,
		PolicyVersion:      policy.PolicyVersion,
		PolicyFingerprint:  policyStatus.Fingerprint,
		PolicyStatus:       policyStatus,
		AutoTrade:          autoStatus,
		Trading:            autoStatus.Trading,
		SourceFingerprints: sources,
		MarketEvents:       marketEvents,
		Proposals:          proposals,
		Counts:             proposalCounts(proposals, protectionCoverageBaseCurrency(pos)),
	}
	return e.installScoped(snap, scope, show, thetaSuppressions)
}

// installScoped re-resolves the broker scope immediately before publishing a
// generated snapshot. The un-pinned gateway can reconnect to a different TWS
// session while Refresh fetches account/position data; installing that data
// with one session's identity but built from another's positions. Fail
func (e *proposalEngine) installScoped(snap rpc.TradeProposalSnapshot, scope brokerStateScope, show bool, thetaSuppressions []thetaSuppression) (rpc.TradeProposalSnapshot, error) {
	if current := e.currentScope(); !sameBrokerScope(current, scope) {
		shell := emptyProposalSnapshot(snap.AsOf)
		shell.AutoTrade = snap.AutoTrade
		shell.PolicyStatus = snap.PolicyStatus
		shell.Blockers = proposalScopeBlockers(scope.Account, scope.Mode, current)
		if err := e.installSnapshot(shell, show); err != nil {
			return e.Snapshot(false), err
		}
		return shell, nil
	}
	if err := e.installSnapshot(snap, show, e.thetaSuppressionEvents(snap, thetaSuppressions)...); err != nil {
		return e.Snapshot(false), err
	}
	return snap, nil
}

// thetaSuppressionEvents records near-expiry options that were deliberately
func (e *proposalEngine) thetaSuppressionEvents(snap rpc.TradeProposalSnapshot, suppressions []thetaSuppression) []proposalEvent {
	if len(suppressions) == 0 {
		return nil
	}
	events := make([]proposalEvent, 0, len(suppressions))
	for _, s := range suppressions {
		events = append(events, proposalEvent{
			At:                 snap.AsOf,
			Type:               "theta-suppressed",
			Key:                s.Key,
			Bucket:             rpc.TradeProposalBucketThetaHygiene,
			AccountID:          snap.AccountID,
			AccountMode:        snap.AccountMode,
			PolicyID:           snap.PolicyID,
			PolicyVersion:      snap.PolicyVersion,
			PolicyFingerprint:  snap.PolicyFingerprint,
			Reason:             s.Reason,
			Message:            s.Message,
			SourceFingerprints: snap.SourceFingerprints,
		})
	}
	return events
}

func (e *proposalEngine) generate(ctx context.Context, policy protectionPolicy, status rpc.ProtectionPolicyStatus, acct *rpc.AccountResult, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, marketEvents *rpc.MarketEventsResult, scope brokerStateScope, now time.Time) ([]rpc.TradeProposal, []thetaSuppression) {
	var out []rpc.TradeProposal
	var suppressions []thetaSuppression
	baseCcy := protectionCoverageBaseCurrency(pos)
	if policy.Buckets.ThetaHygiene.Enabled {
		for _, row := range pos.Options {
			p, ok, supp := thetaProposal(policy, status, row, sources, now)
			if ok {
				if rate, rateOK := positionBaseRate(row, baseCcy); rateOK && p.ThetaPerDay > 0 {
					base := p.ThetaPerDay * rate
					p.ThetaPerDayBase = &base
				}
				enrichProposalPositionContext(&p, row, acct)
				applyMarketEventFlagsToProposal(&p, marketEvents)
				if !e.isIgnored(scope, p.Key) {
					out = append(out, p)
				}
			} else if supp != nil {
				suppressions = append(suppressions, *supp)
			}
		}
	}
	if policy.Buckets.RiskReduction.Enabled {
		for _, group := range pos.ByUnderlying {
			if p, ok := riskReductionProposal(policy, status, group, sources, now); ok {
				enrichRiskReductionContext(&p, group, acct)
				applyMarketEventFlagsToProposal(&p, marketEvents)
				if !e.isIgnored(scope, p.Key) {
					out = append(out, p)
				}
			}
		}
	}
	if policy.Buckets.TrailingStop.Enabled {
		stockEnabled := true
		if e != nil && e.server != nil {
			stockEnabled = e.server.stockProtectionEnabled()
		}
		if policy.Buckets.TrailingStop.StockETF.Enabled {
			for _, row := range pos.Stocks {
				trailSizing := e.stockTrailSizing(ctx, policy.Buckets.TrailingStop.StockETF, row, now)
				if p, ok := trailingStopStockProposal(policy, status, row, sources, now, stockEnabled, e.resolveRowMinTick(row), trailSizing); ok {
					enrichProtectiveStopProposal(&p, row, acct)
					enrichProposalPositionContext(&p, row, acct)
					applyMarketEventFlagsToProposal(&p, marketEvents)
					for _, b := range e.duplicateProtectiveBlockers(ctx, p, pos) {
						proposalBlock(&p, b.Code, b.Message)
					}
					if !e.isIgnored(scope, p.Key) {
						out = append(out, p)
					}
				}
			}
		}
		if policy.Buckets.TrailingStop.Options.Enabled {
			intents := directionalOptionIntents(policy.Buckets.TrailingStop.Options)
			strategyLegs, ambiguousStrategies := optionExitStrategyScope(pos)
			rulebookPolicy := risk.DefaultRulebookPolicy()
			for _, row := range pos.Options {
				intent, declared := intents[row.ConID]
				if !declared {
					continue
				}
				exactRow := e.optionExitExactQuote(ctx, row)
				standalone := !strategyLegs[row.ConID] && !ambiguousStrategies[strings.ToUpper(strings.TrimSpace(row.Symbol))]
				roleAllowed, economicRole := optionExitEconomicRole(row, rulebookPolicy)
				intentCurrent := !now.Before(intent.ApprovedAt) && now.Before(intent.ExpiresAt)
				decision := evaluateOptionExit(policy.Buckets.TrailingStop.Options, exactRow, now, intentCurrent, standalone, roleAllowed, rulebookPolicy.ExitActLossPct)
				if decision.Action == "" && len(decision.Blockers) == 0 {
					continue
				}
				minTick := 0.0
				if decision.Action == risk.OptionExitActionProfitTrail {
					minTick = e.resolveRowMinTick(exactRow)
				}
				if p, ok := optionExitProposal(policy, status, exactRow, sources, now, decision, economicRole, minTick, rulebookPolicy.ExitActLossPct); ok {
					if p.Trail != nil {
						p.ExecutionSemantics = buildProposalExecutionSemantics(p, "bid", decision.ReferencePrice, exactRow.PriceAt)
					}
					enrichProposalPositionContext(&p, exactRow, acct)
					applyMarketEventFlagsToProposal(&p, marketEvents)
					if decision.Action != "" {
						for _, b := range e.duplicateProtectiveBlockers(ctx, p, pos) {
							proposalBlock(&p, b.Code, b.Message)
						}
					}
					if !e.isIgnored(scope, p.Key) {
						out = append(out, p)
					}
				}
			}
		}
	}
	return out, suppressions
}

func (e *proposalEngine) marketEventsSnapshot(ctx context.Context, pos *rpc.PositionsResult) *rpc.MarketEventsResult {
	if e == nil || e.server == nil {
		return nil
	}
	symbols := marketEventSymbolsFromPositions(pos)
	if len(symbols) == 0 {
		return nil
	}
	eventsCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	res := e.server.marketEventsForSymbols(eventsCtx, symbols)
	return &res
}

func (e *proposalEngine) stockTrailSizing(ctx context.Context, cfg protectionTrailAssetPolicy, row rpc.PositionView, now time.Time) *rpc.TradeProposalTrailSizing {
	reference, source, refAt := trailingStopReference(row, trailActionForPosition(row.Quantity))
	vol := e.stockTrailVolatility(ctx, row, now)
	return buildStockTrailSizing(cfg, row, reference, source, refAt, vol, now)
}

func trailActionForPosition(qty float64) string {
	if qty < 0 {
		return rpc.OrderActionBuy
	}
	return rpc.OrderActionSell
}

func (e *proposalEngine) stockTrailVolatility(ctx context.Context, row rpc.PositionView, now time.Time) stockTrailVolatility {
	key := stockTrailVolatilityKey(row)
	if key == "" {
		return stockTrailVolatility{MissingReasons: []string{"volatility_symbol_missing"}}
	}
	e.mu.Lock()
	if e.trailVolCache != nil {
		if cached, ok := e.trailVolCache[key]; ok && now.Sub(cached.fetchedAt) < stockTrailVolatilityCacheTTL {
			e.mu.Unlock()
			return cached.value
		}
	}
	e.mu.Unlock()

	value := e.fetchStockTrailVolatility(ctx, row, now)
	e.mu.Lock()
	if e.trailVolCache == nil {
		e.trailVolCache = map[string]cachedStockTrailVolatility{}
	}
	e.trailVolCache[key] = cachedStockTrailVolatility{value: value, fetchedAt: now}
	e.mu.Unlock()
	return value
}

func (e *proposalEngine) fetchStockTrailVolatility(ctx context.Context, row rpc.PositionView, now time.Time) stockTrailVolatility {
	if e == nil || e.server == nil {
		return stockTrailVolatility{MissingReasons: []string{"volatility_daemon_unavailable"}}
	}
	c := e.server.gatewayConnector()
	if c == nil {
		return stockTrailVolatility{MissingReasons: []string{"volatility_gateway_unavailable"}}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, stockTrailVolatilityTimeout)
	defer cancel()
	contractParams := proposalContractFromPosition(row, positionWireSecType(row.SecType))
	contract, _, _, err := normaliseStockQuoteContract(contractParams)
	var bars []ibkrlib.HistoricalBar
	if err == nil {
		bars, err = c.FetchHistoricalDailyBarsWithContract(fetchCtx, contract, stockTrailVolatilityDays, 0)
	}
	if err != nil {
		bars, err = c.FetchHistoricalDailyBars(fetchCtx, row.Symbol, stockTrailVolatilityDays, 0)
	}
	if err != nil {
		return stockTrailVolatility{MissingReasons: []string{"atr_14_unavailable", "history_unavailable"}}
	}
	atr := technicalATR(bars, 14)
	latest, ok := latestTechnicalBar(bars)
	if atr <= 0 || !ok || latest.Close <= 0 {
		return stockTrailVolatility{MissingReasons: []string{"atr_14_unavailable"}}
	}
	atrPct := atr / latest.Close * 100
	asOf := now
	if !latest.Time.IsZero() {
		asOf = latest.Time
	}
	return stockTrailVolatility{ATR14: &atr, ATRPct: &atrPct, AsOf: asOf}
}

func stockTrailVolatilityKey(row rpc.PositionView) string {
	if row.ConID != 0 {
		return "conid:" + strconv.Itoa(row.ConID)
	}
	symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
	if symbol == "" {
		return ""
	}
	return strings.Join([]string{
		"symbol",
		symbol,
		strings.ToUpper(strings.TrimSpace(row.Currency)),
		strings.ToUpper(strings.TrimSpace(row.Exchange)),
		strings.ToUpper(strings.TrimSpace(row.LocalSymbol)),
		strings.ToUpper(strings.TrimSpace(row.TradingClass)),
	}, ":")
}

func buildStockTrailSizing(cfg protectionTrailAssetPolicy, row rpc.PositionView, reference float64, source string, refAt time.Time, vol stockTrailVolatility, now time.Time) *rpc.TradeProposalTrailSizing {
	fallbackPct := cfg.FallbackPct
	if fallbackPct <= 0 {
		fallbackPct = 10
	}
	sizing := &rpc.TradeProposalTrailSizing{
		Method:            stockTrailSizingMethod,
		Version:           stockTrailSizingVersion,
		ReferenceSource:   source,
		ReferenceAsOf:     refAt,
		PolicyMinPct:      cfg.MinPct,
		PolicyDefaultPct:  cfg.DefaultPct,
		PolicyFallbackPct: fallbackPct,
		PolicyMaxPct:      cfg.MaxPct,
		ATRMultiplier:     new(stockTrailATRMultiplier),
		SpreadMultiplier:  new(stockTrailSpreadMultiplier),
		SpreadPct:         cloneFloat64Ptr(row.SpreadPct),
		MissingReasons:    append([]string(nil), vol.MissingReasons...),
		AsOf:              now,
	}
	if reference > 0 {
		sizing.ReferencePrice = new(reference)
	}
	if vol.ATR14 != nil && vol.ATRPct != nil && *vol.ATRPct > 0 {
		sizing.ATR14 = cloneFloat64Ptr(vol.ATR14)
		sizing.ATRPct = cloneFloat64Ptr(vol.ATRPct)
		sizing.AsOf = nonZeroTime(vol.AsOf, now)
		atrCandidate := *vol.ATRPct * stockTrailATRMultiplier
		sizing.ATRCandidatePct = new(atrCandidate)
	}
	if row.SpreadPct != nil && *row.SpreadPct > 0 {
		spreadFloor := *row.SpreadPct * stockTrailSpreadMultiplier
		sizing.SpreadFloorPct = new(spreadFloor)
	}
	chosen, selected := fallbackPct, "fallback"
	if sizing.ATRCandidatePct != nil {
		chosen, selected = cfg.DefaultPct, "policy_default"
		if *sizing.ATRCandidatePct > chosen {
			chosen, selected = *sizing.ATRCandidatePct, "atr"
		}
	}
	if sizing.SpreadFloorPct != nil && *sizing.SpreadFloorPct > chosen {
		chosen, selected = *sizing.SpreadFloorPct, "spread_floor"
	}
	if chosen < cfg.MinPct {
		chosen = cfg.MinPct
		selected = "policy_min"
	}
	if chosen > cfg.MaxPct {
		chosen = cfg.MaxPct
		sizing.Capped = true
	}
	sizing.ChosenPct = chosen
	sizing.SelectedBy = selected
	sizing.Fallback = selected == "fallback"
	if sizing.Fallback {
		sizing.DataQuality = "fallback"
		if len(sizing.MissingReasons) == 0 {
			sizing.MissingReasons = append(sizing.MissingReasons, "atr_14_unavailable")
		}
	} else if len(sizing.MissingReasons) > 0 {
		sizing.DataQuality = "partial"
	} else {
		sizing.DataQuality = "ok"
	}
	return sizing
}

func nonZeroTime(v, fallback time.Time) time.Time {
	if v.IsZero() {
		return fallback
	}
	return v
}

func (e *proposalEngine) regimeFingerprint(ctx context.Context) (rpc.Fingerprint, bool) {
	if e == nil || e.server == nil {
		return rpc.Fingerprint{}, false
	}
	regimeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	regime, err := e.server.handleRegimeSnapshot(regimeCtx, &rpc.Request{})
	if err != nil || regime == nil {
		return rpc.Fingerprint{}, false
	}
	fp := regime.Fingerprint
	if fp.Key == "" {
		fp = rpc.BuildRegimeFingerprint(regime)
	}
	return fp, fp.Key != ""
}

// thetaSuppression records a near-expiry option that cleared the DTE and dust
type thetaSuppression struct {
	Key     string
	Reason  string
	Message string
}

// thetaProposal evaluates one held option for theta hygiene. It returns at most
// blocker) or a suppression record to journal. Theta only erodes EXTRINSIC
func thetaProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, row rpc.PositionView, sources rpc.TradeProposalSourceFingerprints, now time.Time) (rpc.TradeProposal, bool, *thetaSuppression) {
	if !strings.EqualFold(row.SecType, "OPTION") && !strings.EqualFold(row.SecType, "OPT") || row.Quantity == 0 || row.Theta == nil {
		return rpc.TradeProposal{}, false, nil
	}
	dte, ok := optionDTE(row.Expiry, now)
	if !ok || dte > policy.Buckets.ThetaHygiene.MaxDTE {
		return rpc.TradeProposal{}, false, nil
	}
	mult := float64(max(row.Multiplier, 1))
	qtyAbs := math.Abs(row.Quantity)
	thetaPerDay := math.Abs(*row.Theta * row.Quantity * mult)
	// Dust floor: cheaply skip trivially small positions before the extrinsic
	if thetaPerDay < policy.Buckets.ThetaHygiene.MinAbsThetaPerDay {
		return rpc.TradeProposal{}, false, nil
	}

	qty := int(math.Ceil(qtyAbs))
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}

	// Extrinsic decomposition from fields already on the row. If the
	// underlying spot or a usable mark is missing or the row is stale, we
	// cannot separate intrinsic from time value and therefore cannot assert
	// the close is non-destructive. Surface a blocked row with remediation
	// rather than silently dropping what was previously a visible proposal.
	mark := row.Mark
	if mark <= 0 {
		mark = row.ValuationMark
	}
	if row.Underlying == nil || mark <= 0 || row.Stale {
		p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketThetaHygiene, row, action, qty, rpc.OrderPositionEffectClose, fmt.Sprintf("option expires in %d DTE; time-value at risk is unknown without a fresh quote", dte))
		p.ThetaPerDay = thetaPerDay
		p.Score = thetaPerDay
		p.Details = []string{fmt.Sprintf("dte=%d", dte)}
		p.State = rpc.TradeProposalStateBlocked
		p.Blockers = []rpc.TradingBlocker{{Code: "extrinsic_uncomputable", Message: "underlying spot or option mark is unavailable or stale; refresh during 09:30-16:00 ET so the Greeks and underlying tick are present before assessing theta hygiene"}}
		return p, true, nil
	}

	intrinsicPerShare := optionIntrinsicPerShare(row.Right, *row.Underlying, row.Strike)
	extrinsicPerShare := mark - intrinsicPerShare
	extrinsicPctOfMark := extrinsicPerShare / mark * 100
	if extrinsicPerShare <= 0 || extrinsicPctOfMark < policy.Buckets.ThetaHygiene.MinExtrinsicPctOfMark {
		// Intrinsic-dominated (or a stale mark sitting below intrinsic).
		key := proposalKey(rpc.TradeProposalBucketThetaHygiene, proposalContractFromPosition(row, positionWireSecType(row.SecType)), action)
		msg := fmt.Sprintf("%s suppressed reason=intrinsic_dominated extrinsic_pct=%.1f dte=%d theta_per_day=%.0f qty=%.0f", strings.ToUpper(strings.TrimSpace(row.Symbol)), extrinsicPctOfMark, dte, thetaPerDay, row.Quantity)
		return rpc.TradeProposal{}, false, &thetaSuppression{Key: key, Reason: "intrinsic_dominated", Message: msg}
	}

	// Genuine time-value bleed. Rank by the forfeitable extrinsic dollars over
	// kept only as the rendered headline (counts/CLI/SPA), never as the rank.
	extrinsicTotal := extrinsicPerShare * qtyAbs * mult
	extrinsicAtRisk := math.Min(extrinsicTotal, thetaPerDay*float64(max(dte, 1)))
	thetaPctOfExtrinsic := math.Abs(*row.Theta) / extrinsicPerShare * 100

	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketThetaHygiene, row, action, qty, rpc.OrderPositionEffectClose, fmt.Sprintf("%.0f%% of premium is time value decaying into %d DTE; ~$%.0f extrinsic at risk (intrinsic value and delta are unaffected)", extrinsicPctOfMark, dte, extrinsicAtRisk))
	p.ThetaPerDay = thetaPerDay
	p.Score = extrinsicAtRisk
	p.Details = []string{
		fmt.Sprintf("dte=%d", dte),
		fmt.Sprintf("extrinsic_pct=%.0f", extrinsicPctOfMark),
		fmt.Sprintf("extrinsic_at_risk=%.0f", extrinsicAtRisk),
		fmt.Sprintf("theta_pct_extrinsic=%.0f", thetaPctOfExtrinsic),
	}
	// Transaction-cost guard. row.SpreadPct is never populated for option legs
	if spreadPct, ok := optionSpreadPct(row); ok && spreadPct > policy.Buckets.ThetaHygiene.MaxSpreadPctOfMid {
		p.State = rpc.TradeProposalStateBlocked
		p.Blockers = []rpc.TradingBlocker{{Code: "wide_spread", Message: fmt.Sprintf("option spread %.1f%% exceeds policy max %.1f%% of mid; the round-trip exit cost likely exceeds the extrinsic this would save", spreadPct, policy.Buckets.ThetaHygiene.MaxSpreadPctOfMid)}}
	}
	return p, true, nil
}

// optionIntrinsicPerShare is the per-share in-the-money amount; 0 for an
// out-of-the-money option or an unrecognized right.
func optionIntrinsicPerShare(right string, underlying, strike float64) float64 {
	switch strings.ToUpper(strings.TrimSpace(right)) {
	case "C", "CALL":
		return math.Max(0, underlying-strike)
	case "P", "PUT":
		return math.Max(0, strike-underlying)
	default:
		return 0
	}
}

// optionSpreadPct is the bid/ask spread as a percentage of mid, computed from
// the option leg's own quote. Returns ok=false when the quote is missing or
// crossed/locked.
func optionSpreadPct(row rpc.PositionView) (float64, bool) {
	if row.OptionBid == nil || row.OptionAsk == nil {
		return 0, false
	}
	bid, ask := *row.OptionBid, *row.OptionAsk
	mid := (bid + ask) / 2
	if mid <= 0 || ask < bid {
		return 0, false
	}
	return (ask - bid) / mid * 100, true
}

func riskReductionProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, group rpc.PositionGroup, sources rpc.TradeProposalSourceFingerprints, now time.Time) (rpc.TradeProposal, bool) {
	if group.GroupMarketValuePctNLV == nil || math.Abs(*group.GroupMarketValuePctNLV) <= policy.Buckets.RiskReduction.SingleNameTargetPctNLV {
		return rpc.TradeProposal{}, false
	}
	var row rpc.PositionView
	if group.Stock != nil && group.Stock.Quantity != 0 {
		row = *group.Stock
	} else {
		for _, opt := range group.Options {
			if opt.Quantity != 0 {
				row = opt
				break
			}
		}
	}
	if row.Symbol == "" || row.Quantity == 0 {
		return rpc.TradeProposal{}, false
	}
	if !proposalSupportedSecType(row.SecType) {
		return rpc.TradeProposal{}, false
	}
	pct := math.Abs(*group.GroupMarketValuePctNLV)
	excessPct := pct - policy.Buckets.RiskReduction.SingleNameTargetPctNLV
	excessNotional := math.Abs(groupMarketValueOrderValue(group)) * (excessPct / pct)
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	maxQty := int(math.Ceil(math.Abs(row.Quantity)))
	qty := maxQty
	mark := math.Abs(row.Mark)
	if mark <= 0 {
		mark = math.Abs(row.ValuationMark)
	}
	if mark > 0 {
		mult := float64(max(row.Multiplier, 1))
		qty = int(math.Ceil(excessNotional / (mark * mult)))
		maxByNotional := int(math.Max(1, math.Floor(policy.Buckets.RiskReduction.MaxOrderNotional/(mark*mult))))
		qty = min(qty, maxByNotional)
	}
	qty = max(1, min(qty, maxQty))
	effect := rpc.OrderPositionEffectReduce
	if qty == maxQty {
		effect = rpc.OrderPositionEffectClose
	}
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketRiskReduction, row, action, qty, effect, fmt.Sprintf("%s is %.1f%% of NLV, above %.1f%% target", group.Underlying, pct, policy.Buckets.RiskReduction.SingleNameTargetPctNLV))
	p.MarketValuePctNLV = cloneFloat64Ptr(group.GroupMarketValuePctNLV)
	p.RiskExcessNotional = excessNotional
	p.RiskExcessCurrency = p.Contract.Currency
	if group.GroupMarketValueBase != nil {
		base := math.Abs(*group.GroupMarketValueBase) * (excessPct / pct)
		p.RiskExcessNotionalBase = &base
	}
	p.Score = pct
	return p, true
}

func trailingStopStockProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, row rpc.PositionView, sources rpc.TradeProposalSourceFingerprints, now time.Time, stockProtectionEnabled bool, minTick float64, sizingInput ...*rpc.TradeProposalTrailSizing) (rpc.TradeProposal, bool) {
	secType := strings.ToUpper(strings.TrimSpace(row.SecType))
	if secType != rpc.SecTypeStock && secType != "STK" && secType != "ETF" || row.Quantity == 0 {
		return rpc.TradeProposal{}, false
	}
	cfg := policy.Buckets.TrailingStop.StockETF
	qty, fractionalRemainder := closeReduceQuantity(row.Quantity)
	if qty == 0 {
		return rpc.TradeProposal{}, false
	}
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	reference, refSource, refAt := trailingStopReference(row, action)
	if reference <= 0 && stockPositionLooksInactive(row) {
		return rpc.TradeProposal{}, false
	}
	sizing := firstStockTrailSizing(sizingInput)
	if sizing == nil {
		sizing = buildStockTrailSizing(cfg, row, reference, refSource, refAt, stockTrailVolatility{MissingReasons: []string{"atr_14_unavailable"}}, now)
	}
	trailPct := cfg.DefaultPct
	if sizing.ChosenPct > 0 {
		trailPct = sizing.ChosenPct
	}
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketTrailingStop, row, action, qty, rpc.OrderPositionEffectClose, fmt.Sprintf("broker-side trailing stop at %.1f%% below/above the instrument price", trailPct))
	p.Contract.MinTick = minTick
	p.TIF = policy.Buckets.TrailingStop.effectiveTIF()
	if fractionalRemainder > 0 {
		p.Details = append(p.Details, fmt.Sprintf("fractional %.4g shares stay unprotected under the integer order path", fractionalRemainder))
	}
	applyTrailToProposal(&p, cfg.OrderType, trailPct, reference, action, cfg.LimitOffsetAbs)
	completeTrailSizingFromProposal(sizing, p.Trail)
	p.TrailSizing = sizing
	p.TriggerMethod = rpc.OrderTriggerMethodLast
	p.Score = math.Abs(row.MarketValue)
	p.Details = append(p.Details, trailingStopTrailDetail(trailPct, p.Trail, p.Contract.Currency))
	if detail := trailingStopSizingDetail(p.TrailSizing); detail != "" {
		p.Details = append(p.Details, detail)
	}
	p.Details = append(p.Details, trailingStopTIFDetail(p.TIF, false))
	enrichProtectiveStopProposal(&p, row, nil)
	if !stockProtectionEnabled {
		proposalBlock(&p, "stock_protection_disabled", "stock/ETF protection is disabled in platform settings")
	}
	if reference <= 0 {
		proposalBlock(&p, "missing_reference_price", "stock/ETF trailing stop requires bid/ask or a positive portfolio mark")
	}
	if row.SpreadPct != nil && *row.SpreadPct > cfg.MaxSpreadPctOfMid {
		proposalBlock(&p, "wide_spread", fmt.Sprintf("stock/ETF spread %.1f%% exceeds policy max %.1f%% of mid", *row.SpreadPct, cfg.MaxSpreadPctOfMid))
	}
	return p, true
}

func firstStockTrailSizing(in []*rpc.TradeProposalTrailSizing) *rpc.TradeProposalTrailSizing {
	if len(in) == 0 {
		return nil
	}
	return cloneTrailSizing(in[0])
}

func optionExitProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, row rpc.PositionView, sources rpc.TradeProposalSourceFingerprints, now time.Time, decision risk.OptionExitDecision, economicRole string, minTick, lossExitPct float64) (rpc.TradeProposal, bool) {
	if !strings.EqualFold(row.SecType, "OPTION") && !strings.EqualFold(row.SecType, "OPT") || row.Quantity == 0 {
		return rpc.TradeProposal{}, false
	}
	cfg := policy.Buckets.TrailingStop.Options
	qty, remainder := 0, 0.0
	if !math.IsNaN(row.Quantity) && !math.IsInf(row.Quantity, 0) {
		qty, remainder = closeReduceQuantity(row.Quantity)
	}
	bucket, reason := rpc.TradeProposalBucketOptionExitReview,
		"directional option exit cannot be measured from current exact-contract evidence"
	switch decision.Action {
	case risk.OptionExitActionLoss:
		bucket = rpc.TradeProposalBucketOptionLossExit
		reason = fmt.Sprintf("fresh executable bid is %.1f%% below cost; Rulebook exits the full directional option at %.1f%% loss", math.Abs(decision.ReturnPct), lossExitPct)
	case risk.OptionExitActionProfitTrail:
		bucket = rpc.TradeProposalBucketTrailingStop
		reason = fmt.Sprintf("directional option gained %.1f%% versus cost; profit trail armed at %.1f%%", decision.ReturnPct, cfg.ProfitArmGainPct)
	}
	p := baseProposal(policy, status, sources, now, bucket, row, rpc.OrderActionSell, qty, rpc.OrderPositionEffectClose, reason)
	p.Contract.MinTick = minTick
	p.TIF = cfg.effectiveTIF()
	p.LimitPrice = nil
	p.Score = math.Abs(row.MarketValue)
	p.OptionExit = &rpc.TradeProposalOptionExit{
		Kind: nonEmptyString(decision.Action, "review"), Intent: "directional", EconomicRole: economicRole,
		DTE: optionExitDTE(row, now), MinDTE: cfg.MinDTE, LossExitPct: lossExitPct, ProfitArmGainPct: cfg.ProfitArmGainPct,
		LockedGainPct: cfg.LockedGainPct, ProfitTrailPct: cfg.DefaultPct, MinTrailPct: cfg.MinPct,
		MaxTrailPct: cfg.MaxPct, MaxSpreadPctOfMid: cfg.MaxSpreadPctOfMid, MinTrailAbs: cfg.MinTrailAbs,
		SpreadMultiple: cfg.SpreadMultiple, Method: "fresh_bid_vs_multiplier_adjusted_cost",
	}
	if decision.CostPremium > 0 && !math.IsNaN(decision.CostPremium) && !math.IsInf(decision.CostPremium, 0) {
		p.OptionExit.CostBasisPremium = cloneFloat64Ptr(&decision.CostPremium)
	}
	if decision.ReferencePrice > 0 && !math.IsNaN(decision.ReferencePrice) && !math.IsInf(decision.ReferencePrice, 0) {
		p.OptionExit.ReferencePrice = cloneFloat64Ptr(&decision.ReferencePrice)
		p.OptionExit.ReturnPct = cloneFloat64Ptr(&decision.ReturnPct)
	}
	if remainder > 0 || qty <= 0 {
		proposalBlock(&p, "whole_contract_quantity_required", "option exits require a positive whole-contract position quantity")
	}
	for _, code := range decision.Blockers {
		proposalBlock(&p, code, optionExitBlockerMessage(code, cfg))
	}
	if decision.Action == "" {
		proposalBlock(&p, "option_exit_measurement_unavailable", "exact-contract option exit evidence is incomplete; no threshold or order may be inferred")
		p.LimitPrice = nil
		return p, true
	}
	if decision.Action == risk.OptionExitActionLoss {
		p.Details = append(p.Details,
			fmt.Sprintf("rulebook_loss_exit=%.1f%% from multiplier-adjusted cost on fresh bid", lossExitPct),
			"order=DAY patient midpoint limit; may remain unfilled; no resting loss stop and no overnight loss guarantee")
		return p, true
	}

	trailAmount := ceilPriceToTick(decision.TrailAmount, trailMinimumTick(p.Contract, decision.ReferencePrice))
	chosenPct := 0.0
	if decision.ReferencePrice > 0 {
		chosenPct = trailAmount / decision.ReferencePrice * 100
	}
	applyNativeTrailPercentToProposal(&p, cfg.OrderType, chosenPct, trailAmount, decision.ReferencePrice, rpc.OrderActionSell, cfg.LimitOffsetAbs)
	initialLockPct := 0.0
	if decision.CostPremium > 0 && p.Trail != nil {
		initialLockPct = (p.Trail.InitialStopPrice/decision.CostPremium - 1) * 100
		p.OptionExit.InitialLockedGainPct = cloneFloat64Ptr(&initialLockPct)
	}
	if p.Trail == nil || !risk.OptionExitLockedGainMet(decision.CostPremium, p.Trail.InitialStopPrice, cfg.LockedGainPct) {
		proposalBlock(&p, "option_trail_locked_gain_not_met", fmt.Sprintf("rounded initial stop must retain at least %.1f%% over cost; wider spread/tick floors cannot weaken that invariant", cfg.LockedGainPct))
	}
	if !risk.OptionExitTrailPctWithinBounds(decision.ReferencePrice, trailAmount, cfg.MinPct, cfg.MaxPct) {
		proposalBlock(&p, "option_trail_outside_policy_bounds", fmt.Sprintf("rounded premium trail must stay within the %.1f%% to %.1f%% approved range", cfg.MinPct, cfg.MaxPct))
	}
	p.TrailSizing = &rpc.TradeProposalTrailSizing{
		Method: "option-profit-lock-v1", Version: "option-profit-lock-v1", SelectedBy: optionTrailSelectedBy(cfg, decision),
		ReferencePrice: cloneFloat64Ptr(&decision.ReferencePrice), ReferenceSource: "bid", ReferenceAsOf: row.PriceAt,
		PolicyMinPct: cfg.MinPct, PolicyDefaultPct: cfg.DefaultPct, PolicyMaxPct: cfg.MaxPct,
		ChosenPct: chosenPct, ChosenAmount: cloneFloat64Ptr(&trailAmount), InitialStopPrice: cloneFloat64Ptr(&p.Trail.InitialStopPrice),
		SpreadPct: cloneFloat64Ptr(&decision.SpreadPctOfMid), SpreadMultiplier: cloneFloat64Ptr(&cfg.SpreadMultiple), AsOf: now,
	}
	p.Details = append(p.Details,
		trailingStopPremiumTrailDetail(chosenPct, p.Trail, p.Contract.Currency),
		fmt.Sprintf("profit_arm=+%.1f%% locked_gain>=+%.1f%% initial=+%.1f%%", cfg.ProfitArmGainPct, cfg.LockedGainPct, initialLockPct),
		trailingStopTIFDetail(p.TIF, true))
	return p, true
}

// applyNativeTrailPercentToProposal sends the option profit-lock distance as
// IBKR's broker-managed percentage trail. initialAmount is the exact-tick
// activation distance used to seed and verify the initial stop; it is not sent
// as a second trail field because IBKR requires amount xor percentage.
func applyNativeTrailPercentToProposal(p *rpc.TradeProposal, orderType string, pct, initialAmount, reference float64, action string, limitOffset float64) {
	if p == nil {
		return
	}
	p.OrderType = strings.ToUpper(strings.TrimSpace(orderType))
	p.Trail = &rpc.OrderTrailSpec{
		Basis:            rpc.OrderTrailBasisInstrumentPrice,
		OffsetType:       rpc.OrderTrailOffsetPercent,
		TrailingPercent:  cloneFloat64Ptr(&pct),
		InitialStopPrice: trailingStopInitialPriceForContract(action, reference, initialAmount, p.Contract),
	}
	if strings.EqualFold(p.OrderType, rpc.OrderTypeTRAILLIMIT) && limitOffset > 0 {
		p.Trail.LimitOffset = cloneFloat64Ptr(&limitOffset)
	}
	p.LimitPrice = nil
}

func applyTrailToProposal(p *rpc.TradeProposal, orderType string, pct, reference float64, action string, limitOffset float64) {
	if p == nil {
		return
	}
	p.OrderType = strings.ToUpper(strings.TrimSpace(orderType))
	if p.OrderType == "" {
		p.OrderType = rpc.OrderTypeTRAIL
	}
	trail := &rpc.OrderTrailSpec{
		Basis:      rpc.OrderTrailBasisInstrumentPrice,
		OffsetType: rpc.OrderTrailOffsetPercent,
	}
	if reference > 0 {
		amount := ceilPriceToTick(reference*pct/100, trailMinimumTick(p.Contract, reference))
		trail.OffsetType = rpc.OrderTrailOffsetAmount
		trail.TrailingAmount = cloneFloat64Ptr(&amount)
		trail.InitialStopPrice = trailingStopInitialPriceForContract(action, reference, amount, p.Contract)
	} else {
		trail.TrailingPercent = cloneFloat64Ptr(&pct)
	}
	if strings.EqualFold(p.OrderType, rpc.OrderTypeTRAILLIMIT) && limitOffset > 0 {
		trail.LimitOffset = cloneFloat64Ptr(&limitOffset)
	}
	p.Trail = trail
	if isTrailOrderType(p.OrderType) {
		p.LimitPrice = nil
	}
}

func trailingStopTrailDetail(pct float64, trail *rpc.OrderTrailSpec, currency string) string {
	return trailingStopTrailDetailWithLabel("trail", pct, trail, currency)
}

func trailingStopPremiumTrailDetail(pct float64, trail *rpc.OrderTrailSpec, currency string) string {
	return trailingStopTrailDetailWithLabel("premium trail", pct, trail, currency)
}

func trailingStopTrailDetailWithLabel(label string, pct float64, trail *rpc.OrderTrailSpec, currency string) string {
	if trail != nil && trail.TrailingAmount != nil {
		unit := strings.ToUpper(strings.TrimSpace(currency))
		if unit == "" {
			unit = "currency"
		}
		return fmt.Sprintf("%s=%.1f%% initial -> fixed %.2f %s broker trail", label, pct, *trail.TrailingAmount, unit)
	}
	return fmt.Sprintf("%s=%.1f%%", label, pct)
}

func trailingStopReference(row rpc.PositionView, action string) (float64, string, time.Time) {
	at := row.QuotePriceAt
	if at.IsZero() {
		at = row.PriceAt
	}
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		if row.Ask != nil && *row.Ask > 0 {
			return *row.Ask, "ask", at
		}
	} else if row.Bid != nil && *row.Bid > 0 {
		return *row.Bid, "bid", at
	}
	if row.QuotePrice != nil && *row.QuotePrice > 0 {
		source := strings.TrimSpace(row.QuotePriceSource)
		if source == "" {
			source = "quote_price"
		}
		return *row.QuotePrice, source, row.QuotePriceAt
	}
	if row.Mark > 0 {
		return row.Mark, "mark", row.PriceAt
	}
	if row.ValuationMark > 0 {
		return row.ValuationMark, "valuation_mark", row.PriceAt
	}
	return 0, "", time.Time{}
}

func completeTrailSizingFromProposal(sizing *rpc.TradeProposalTrailSizing, trail *rpc.OrderTrailSpec) {
	if sizing == nil || trail == nil {
		return
	}
	sizing.ChosenAmount = cloneFloat64Ptr(trail.TrailingAmount)
	if trail.InitialStopPrice > 0 {
		sizing.InitialStopPrice = new(trail.InitialStopPrice)
	}
}

func trailingStopSizingDetail(sizing *rpc.TradeProposalTrailSizing) string {
	if sizing == nil {
		return ""
	}
	if sizing.Fallback {
		fallbackPct := sizing.PolicyFallbackPct
		if fallbackPct <= 0 {
			fallbackPct = sizing.ChosenPct
		}
		return fmt.Sprintf("trail_sizing=fallback %.1f%%: ATR unavailable, %.1f%% policy fallback used", sizing.ChosenPct, fallbackPct)
	}
	if sizing.Capped {
		return fmt.Sprintf("trail_sizing=%s %.1f%% capped at policy max %.1f%%", sizing.SelectedBy, sizing.ChosenPct, sizing.PolicyMaxPct)
	}
	if sizing.SelectedBy != "" {
		return fmt.Sprintf("trail_sizing=%s %.1f%%", sizing.SelectedBy, sizing.ChosenPct)
	}
	return ""
}

func stockPositionLooksInactive(row rpc.PositionView) bool {
	return row.Mark <= 0 &&
		row.ValuationMark <= 0 &&
		row.MarketValue == 0 &&
		(row.QuotePrice == nil || *row.QuotePrice <= 0) &&
		(row.Bid == nil || *row.Bid <= 0) &&
		(row.Ask == nil || *row.Ask <= 0)
}

func optionTrailReference(row rpc.PositionView, action string) (reference float64, spreadAbs float64, ok bool) {
	if row.OptionBid == nil || row.OptionAsk == nil || *row.OptionBid <= 0 || *row.OptionAsk <= 0 || *row.OptionAsk < *row.OptionBid {
		return 0, 0, false
	}
	spreadAbs = *row.OptionAsk - *row.OptionBid
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		return *row.OptionAsk, spreadAbs, true
	}
	return *row.OptionBid, spreadAbs, true
}

func directionalOptionIntents(cfg protectionTrailOptionPolicy) map[int]protectionOptionDirectionalIntent {
	out := make(map[int]protectionOptionDirectionalIntent, len(cfg.DirectionalIntents))
	for _, intent := range cfg.DirectionalIntents {
		if intent.ConID > 0 {
			out[intent.ConID] = intent
		}
	}
	return out
}

// optionExitStrategyScope treats every reconstructed strategy leg as
// non-standalone. An unresolved grouping issue blocks every option under that
// underlying because choosing one leg could dismantle an economic strategy.
func optionExitStrategyScope(pos *rpc.PositionsResult) (map[int]bool, map[string]bool) {
	legs := make(map[int]bool)
	ambiguous := make(map[string]bool)
	if pos == nil {
		return legs, ambiguous
	}
	for _, strategy := range pos.Strategies {
		for _, leg := range strategy.Legs {
			if leg.Contract.ConID > 0 {
				legs[leg.Contract.ConID] = true
			}
		}
	}
	for _, issue := range pos.StrategyIssues {
		if underlying := strings.ToUpper(strings.TrimSpace(issue.Underlying)); underlying != "" {
			ambiguous[underlying] = true
		}
	}
	return legs, ambiguous
}

// optionExitEconomicRole refuses to infer intent from a hedge-listed put's
// product shape. Such a contract must be economically directional under the
// current Rulebook as well as explicitly declared directional by the operator.
func optionExitEconomicRole(row rpc.PositionView, pol risk.RulebookPolicy) (bool, string) {
	if !pol.IsHedgeSymbol(row.Symbol) || !strings.EqualFold(strings.TrimSpace(row.Right), "P") {
		return true, risk.IndexPutRoleDirectional
	}
	// The general positions Greeks cache is keyed by underlying/expiry/right/
	// rounded strike and cannot prove SPX versus SPXW (or another exact class).
	// Until option Greeks carry a positive-ConID receipt, every hedge-listed
	// put remains unclassified here; a symbol-shape or shared-cache role must
	// never authorize selling a possible hedge.
	return false, risk.IndexPutRoleUnclassified
}

// optionExitExactQuote discards any symbol/Greeks-cache quote fields and reads
// a new non-sharing subscription keyed by the held contract's positive ConID.
// This keeps SPX/SPXW and other trading-class distinctions exact and carries
// the broker tick receipt time into the decision row.
func (e *proposalEngine) optionExitExactQuote(ctx context.Context, row rpc.PositionView) rpc.PositionView {
	row.OptionBid = nil
	row.OptionAsk = nil
	row.DataType = ""
	row.PriceAt = time.Time{}
	row.PriceAsOf = ""
	row.Stale = true
	row.StaleReason = "exact option quote unavailable"
	row.SessionContext = nil
	if e == nil || e.server == nil || row.ConID <= 0 {
		return row
	}
	authority, err := e.server.captureOrderPreviewBrokerAuthority()
	if err != nil || authority == nil {
		return row
	}
	contract := proposalContractFromPosition(row, positionWireSecType(row.SecType))
	quote, err := e.server.previewExactSessionContractQuoteWithReady(ctx, authority, contract, optionExitQuoteTimeout, func(q *rpc.Quote) bool {
		return q != nil && q.Bid != nil && q.Ask != nil
	})
	if err != nil {
		return row
	}
	row.OptionBid = cloneFloat64Ptr(quote.Bid)
	row.OptionAsk = cloneFloat64Ptr(quote.Ask)
	row.DataType = quote.DataType
	row.PriceAt = quote.PriceAt
	row.PriceAsOf = quote.PriceAsOf
	row.Stale = quote.Stale
	row.StaleReason = quote.StaleReason
	row.SessionContext = quote.SessionContext
	return row
}

func evaluateOptionExit(cfg protectionTrailOptionPolicy, row rpc.PositionView, now time.Time, directionalIntent, standalone, roleAllowed bool, lossExitPct float64) risk.OptionExitDecision {
	dte := optionExitDTE(row, now)
	sessionOpen := optionSessionOpen(now)
	if row.SessionContext != nil {
		sessionOpen = row.SessionContext.IsOpen
	}
	in := risk.OptionExitInput{
		ConID: row.ConID, Quantity: row.Quantity, Multiplier: row.Multiplier, AvgCost: row.AvgCost,
		DTE: dte, DirectionalIntent: directionalIntent, Standalone: standalone, EconomicRoleAllowed: roleAllowed,
		QuoteLive: rpc.IsLiveDataType(row.DataType), QuoteFresh: !row.Stale && !row.PriceAt.IsZero(),
		SessionOpen: sessionOpen,
	}
	if row.OptionBid != nil {
		in.Bid = *row.OptionBid
	}
	if row.OptionAsk != nil {
		in.Ask = *row.OptionAsk
	}
	return risk.EvaluateOptionExit(in, risk.OptionExitPolicy{
		MinDTE: cfg.MinDTE, LossExitPct: lossExitPct, ProfitArmGainPct: cfg.ProfitArmGainPct,
		ProfitTrailPct: cfg.DefaultPct, LockedGainPct: cfg.LockedGainPct, MinTrailPct: cfg.MinPct,
		MaxTrailPct: cfg.MaxPct, MaxSpreadPctOfMid: cfg.MaxSpreadPctOfMid,
		MinTrailAbs: cfg.MinTrailAbs, SpreadMultiple: cfg.SpreadMultiple,
	})
}

func optionExitDTE(row rpc.PositionView, now time.Time) int {
	if dte, ok := optionDTE(row.Expiry, now); ok {
		return dte
	}
	return -1
}

func optionExitBlockerMessage(code string, cfg protectionTrailOptionPolicy) string {
	switch code {
	case "exact_contract_required":
		return "option exits require a positive exact broker contract id"
	case "directional_intent_required":
		return "option exit requires explicit exact-contract directional intent"
	case "standalone_option_required":
		return "option belongs to or may belong to a multi-leg strategy; close it through the strategy workflow"
	case "directional_role_not_confirmed":
		return "current Rulebook role is protection or unknown; option exit cannot sell a possible hedge"
	case "long_option_required":
		return "option exit V1 supports long option positions only"
	case "whole_contract_quantity_required":
		return "option exits require a positive whole-contract position quantity"
	case "option_exit_min_dte":
		return fmt.Sprintf("option exit requires at least %d calendar DTE", cfg.MinDTE)
	case "option_cost_basis_unavailable":
		return "option exit requires positive multiplier-adjusted broker cost basis"
	case "live_option_quote_required":
		return "option exit requires live option market data"
	case "fresh_option_quote_required":
		return "option exit requires a fresh timestamped option quote"
	case "option_rth_closed":
		return "option exit proposals require the regular listed-options session to be open"
	case "two_sided_option_quote_required":
		return "option exit requires a positive two-sided option bid/ask"
	case "option_spread_too_wide":
		return fmt.Sprintf("option spread exceeds the %.1f%% of mid outer data-quality limit", cfg.MaxSpreadPctOfMid)
	case "option_trail_outside_policy_bounds":
		return fmt.Sprintf("computed premium trail falls outside the %.1f%% to %.1f%% policy bounds", cfg.MinPct, cfg.MaxPct)
	case "option_trail_locked_gain_not_met":
		return fmt.Sprintf("initial trail stop must retain at least %.1f%% over cost after spread and tick floors", cfg.LockedGainPct)
	case "option_numeric_input_invalid":
		return "option exit received a non-finite position, cost, or quote value and is blocked"
	case "option_exit_policy_invalid":
		return "option exit policy contains a non-finite numeric value and is blocked"
	default:
		return "option exit policy requirement is not satisfied"
	}
}

func optionTrailSelectedBy(cfg protectionTrailOptionPolicy, decision risk.OptionExitDecision) string {
	const eps = 1e-9
	defaultAmount := decision.ReferencePrice * cfg.DefaultPct / 100
	spreadFloor := cfg.SpreadMultiple * decision.SpreadAbs
	if spreadFloor > defaultAmount+eps && spreadFloor >= cfg.MinTrailAbs-eps {
		return "spread_floor"
	}
	if cfg.MinTrailAbs > defaultAmount+eps && cfg.MinTrailAbs >= spreadFloor-eps {
		return "absolute_floor"
	}
	return "policy_default"
}

func proposalBlock(p *rpc.TradeProposal, code, message string) {
	if p == nil {
		return
	}
	p.State = rpc.TradeProposalStateBlocked
	p.Blockers = appendTradingBlockerOnce(p.Blockers, rpc.TradingBlocker{Code: code, Message: message})
}

// positionWireSecType maps PositionView.SecType — the canonical AssetType
// the forward mapping) — to the IBKR wire security type for broker contract
// fields. The enum forms are not valid on the wire: TWS rejects secType
// "STOCK" with error 321 "Please enter a valid security type".
func positionWireSecType(raw string) string {
	switch {
	case strings.EqualFold(raw, "OPTION") || strings.EqualFold(raw, "OPT"):
		return "OPT"
	case strings.EqualFold(raw, "ETF"):
		return "ETF"
	default:
		return "STK"
	}
}

func baseProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, bucket string, row rpc.PositionView, action string, qty int, effect string, reason string) rpc.TradeProposal {
	secType := positionWireSecType(row.SecType)
	contract := proposalContractFromPosition(row, secType)
	maxQuantity := 0
	if !math.IsNaN(row.Quantity) && !math.IsInf(row.Quantity, 0) {
		maxQuantity = int(math.Ceil(math.Abs(row.Quantity)))
	}
	p := rpc.TradeProposal{Key: proposalKey(bucket, contract, action), State: rpc.TradeProposalStateGenerated, Bucket: bucket, Symbol: contract.Symbol, SecType: secType, Action: action, Quantity: qty, MaxQuantity: maxQuantity, PositionQuantity: row.Quantity, PositionEffect: effect, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Contract: contract, Reason: reason, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint, SourceFingerprints: sources, CreatedAt: now}
	if row.Mark > 0 {
		v := row.Mark
		p.LimitPrice = &v
		p.Notional = math.Abs(row.Mark) * float64(qty) * float64(max(row.Multiplier, 1))
	}
	return p
}

func proposalContractFromPosition(row rpc.PositionView, secType string) rpc.ContractParams {
	contract := rpc.ContractParams{
		ConID:        row.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(row.Symbol)),
		SecType:      secType,
		Exchange:     nonEmptyString(row.Exchange, "SMART"),
		Currency:     nonEmptyString(row.Currency, "USD"),
		LocalSymbol:  row.LocalSymbol,
		TradingClass: row.TradingClass,
		Expiry:       row.Expiry,
		Strike:       row.Strike,
		Right:        row.Right,
		Multiplier:   row.Multiplier,
	}
	if secType == "STK" || secType == "ETF" {
		// msgPortfolioValue stores the *primary* exchange under row.Exchange
		// (documented wire quirk); routing a protective order directly to it
		// forfeits SMART routing. Route SMART and keep the venue as
		// PrimaryExch — ConID anchors contract identity either way.
		primary := strings.ToUpper(strings.TrimSpace(row.Exchange))
		if primary != "" && primary != "SMART" {
			contract.PrimaryExch = primary
		}
		contract.Exchange = "SMART"
		if primary == "IBIS" {
			contract.Market = "de"
			contract.Currency = nonEmptyString(row.Currency, "EUR")
		}
	}
	return contract
}

func applyMarketEventFlagsToProposal(prop *rpc.TradeProposal, events *rpc.MarketEventsResult) {
	if prop == nil || events == nil {
		return
	}
	flags := proposalMarketEventFlags(*prop, events)
	if len(flags) == 0 {
		return
	}
	prop.MarketFlags = flags
	for _, flag := range flags {
		switch {
		case flag.ID == rpc.MarketEventHaltRegulatoryOrNews && flag.Status == rpc.MarketEventStatusActive:
			marketEventBlockProposal(prop, flag, "active halt")
		case flag.ID == rpc.MarketEventLULDRecent && flag.Status == rpc.MarketEventStatusActive:
			marketEventBlockProposal(prop, flag, "active LULD pause")
		}
	}
}

func proposalMarketEventFlags(prop rpc.TradeProposal, events *rpc.MarketEventsResult) []rpc.MarketEventFlag {
	if events == nil || events.BySymbol == nil {
		return nil
	}
	symbol := strings.ToUpper(strings.TrimSpace(prop.Symbol))
	if symbol == "" {
		return nil
	}
	out := []rpc.MarketEventFlag{}
	for _, flag := range events.BySymbol[symbol] {
		if !proposalMarketEventFlagApplies(prop, flag) {
			continue
		}
		out = append(out, flag)
	}
	slices.SortFunc(out, func(a, b rpc.MarketEventFlag) int {
		if c := cmpMarketEventSeverity(a.Severity, b.Severity); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func proposalMarketEventFlagApplies(prop rpc.TradeProposal, flag rpc.MarketEventFlag) bool {
	switch flag.ID {
	case rpc.MarketEventHaltRegulatoryOrNews, rpc.MarketEventLULDRecent:
		return flag.Status == rpc.MarketEventStatusActive || flag.Status == rpc.MarketEventStatusRecent
	case rpc.MarketEventRegSHOThreshold:
		return proposalCloseReduceEffect(prop.PositionEffect)
	case rpc.MarketEventBorrowInventoryTight, rpc.MarketEventBorrowFeeExtreme:
		return prop.PositionQuantity < 0 &&
			strings.EqualFold(prop.Action, rpc.OrderActionBuy) &&
			proposalCloseReduceEffect(prop.PositionEffect)
	default:
		return flag.Status == rpc.MarketEventStatusActive || flag.Status == rpc.MarketEventStatusRecent
	}
}

func marketEventBlockProposal(prop *rpc.TradeProposal, flag rpc.MarketEventFlag, reason string) {
	prop.State = rpc.TradeProposalStateBlocked
	code := "market_event_" + flag.ID
	message := fmt.Sprintf("%s is %s for %s", flag.Label, reason, flag.Symbol)
	if flag.Source != "" {
		message += " (" + flag.Source + ")"
	}
	prop.Blockers = appendTradingBlockerOnce(prop.Blockers, rpc.TradingBlocker{
		Code:    code,
		Message: message + "; refresh proposals after the market event clears.",
		Action:  "Wait for fresh tradability context before previewing or submitting this protection proposal.",
	})
}

func (e *proposalEngine) Preview(ctx context.Context, p rpc.TradeProposalPreviewParams) (rpc.TradeProposalPreviewResult, error) {
	prop, blockers, err := e.previewProposal(ctx, p)
	now := e.clock()
	if len(blockers) > 0 || err != nil {
		e.appendBlocked(prop, p.Key, p.Revision, blockers, err)
		return rpc.TradeProposalPreviewResult{Proposal: prop, Blockers: blockers, AsOf: now}, err
	}
	preview, err := e.server.previewOrder(ctx, proposalOrderPreviewParams(prop, selectedProposalQty(prop, p.Quantity), p.TimeoutMs))
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "preview_failed", Message: err.Error()}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, err)
		return rpc.TradeProposalPreviewResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	e.appendEvent(proposalEventForProposal("previewed", prop, now, preview.PreviewTokenID, preview.Draft.OrderRef, "proposal previewed"))
	if blockers := proposalPreviewSafetyBlockers(prop, preview); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalPreviewResult{Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, Preview: sanitizeProposalPreviewForProposal(preview, prop), Blockers: blockers, AsOf: now}, nil
	}
	if blockers := e.duplicateProtectiveBlockers(ctx, prop); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalPreviewResult{Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, Preview: sanitizeProposalPreviewForProposal(preview, prop), Blockers: blockers, AsOf: now}, nil
	}
	if !preview.SubmitEligible {
		blockers := previewNotSubmitEligibleBlockers(preview)
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalPreviewResult{Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, SubmitEligible: false, Preview: sanitizeProposalPreviewForProposal(preview, prop), Blockers: blockers, AsOf: now}, nil
	}
	return rpc.TradeProposalPreviewResult{Accepted: true, Proposal: prop, PreviewTokenID: preview.PreviewTokenID, PreviewTokenExpiresAt: preview.PreviewTokenExpiresAt, SubmitEligible: preview.SubmitEligible, Preview: sanitizeProposalPreviewForProposal(preview, prop), AsOf: now}, nil
}

func (e *proposalEngine) previewProposal(ctx context.Context, p rpc.TradeProposalPreviewParams) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
	if p.FastPath {
		if prop, blockers, ok := e.fastPathPreviewProposal(p.Key, p.Revision); ok {
			return prop, blockers, nil
		}
	}
	return e.revalidatedProposal(ctx, p.Key, p.Revision)
}

func (e *proposalEngine) submitProposal(ctx context.Context, p rpc.TradeProposalSubmitParams, fastPathEnabled bool) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
	if p.FastPath && fastPathEnabled {
		if prop, blockers, ok := e.fastPathSubmitProposal(p.Key, p.Revision); ok {
			return prop, blockers, nil
		}
	}
	return e.revalidatedProposal(ctx, p.Key, p.Revision)
}

func (e *proposalEngine) fastPathPreviewProposal(key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, bool) {
	return e.fastPathCachedProposal(key, revision)
}

func (e *proposalEngine) fastPathSubmitProposal(key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, bool) {
	return e.fastPathCachedProposal(key, revision)
}

func (e *proposalEngine) fastPathCachedProposal(key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, bool) {
	key, revision = strings.TrimSpace(key), strings.TrimSpace(revision)
	if key == "" || revision == "" {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "bad_request", Message: "proposal key and revision are required"}}, true
	}
	e.mu.Lock()
	snap := cloneProposalSnapshot(e.snapshot)
	e.mu.Unlock()
	if snap.Kind == "" || snap.Revision == "" {
		return rpc.TradeProposal{}, nil, false
	}
	// The fast path serves the cached snapshot; cap its age so a daemon
	// restart (LoadedFromState) or a stalled refresh can never preview
	maxAge := 2 * e.cadence
	if maxAge <= 0 {
		maxAge = 30 * time.Minute
	}
	if snap.LoadedFromState || e.clock().Sub(snap.AsOf) > maxAge {
		return rpc.TradeProposal{}, nil, false
	}
	// The fast path may only act on a cached snapshot generated under the
	// currently-connected account/mode session. Mismatch or an unscoped
	// session fails closed; proposal-free shells carry session-independent
	if len(snap.Proposals) > 0 {
		if blockers := proposalScopeBlockers(snap.AccountID, snap.AccountMode, e.currentScope()); len(blockers) > 0 {
			return rpc.TradeProposal{}, blockers, true
		}
	}
	if len(snap.Blockers) > 0 && len(snap.Proposals) == 0 {
		return rpc.TradeProposal{}, snap.Blockers, true
	}
	if snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusDrift || snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusError {
		return rpc.TradeProposal{}, snap.PolicyStatus.Blockers, true
	}
	if len(snap.AutoTrade.Blockers) > 0 {
		return rpc.TradeProposal{}, snap.AutoTrade.Blockers, true
	}
	if snap.Revision != revision {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "proposal revision is stale; refresh proposals before preview or submit"}}, true
	}
	for _, prop := range snap.Proposals {
		if prop.Key != key {
			continue
		}
		// Directional-option exits depend on a current executable bid versus
		// cost and must always re-evaluate the arm/loss/locked-gain invariants.
		// The cached fast path remains stock/ETF protective-stop only.
		if prop.Bucket != rpc.TradeProposalBucketTrailingStop || prop.OptionExit != nil {
			return rpc.TradeProposal{}, nil, false
		}
		if len(snap.Blockers) > 0 {
			return prop, mergeTradingBlockers(snap.Blockers, prop.Blockers), true
		}
		return prop, prop.Blockers, true
	}
	return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "proposal key is not present in the current snapshot"}}, true
}

func (e *proposalEngine) Submit(ctx context.Context, p rpc.TradeProposalSubmitParams) (rpc.TradeProposalSubmitResult, error) {
	now := e.clock()
	cfg := e.server.cfg.AutoTrade.WithDefaults()
	prop, blockers, err := e.submitProposal(ctx, p, cfg.FastPathEnabledResolved())
	if len(blockers) > 0 || err != nil {
		e.appendBlocked(prop, p.Key, p.Revision, blockers, err)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, err
	}
	if !cfg.FastPathEnabledResolved() || !p.FastPath {
		blockers := []rpc.TradingBlocker{{Code: "fast_path_disabled", Message: "proposal submit requires fast_path=true and [auto_trade].fast_path_enabled=true"}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	if blockers := e.server.proposalSubmitWriteBlockers(p.Origin); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	preview, err := e.server.previewOrder(ctx, proposalOrderPreviewParams(prop, selectedProposalQty(prop, p.Quantity), p.TimeoutMs))
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "preview_failed", Message: err.Error()}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, err)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Blockers: blockers, AsOf: now}, nil
	}
	e.appendEvent(proposalEventForProposal("previewed", prop, now, preview.PreviewTokenID, preview.Draft.OrderRef, "proposal fast-path previewed"))
	if blockers := proposalPreviewSafetyBlockers(prop, preview); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreviewForProposal(preview, prop), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	if blockers := e.duplicateProtectiveBlockers(ctx, prop); len(blockers) > 0 {
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreviewForProposal(preview, prop), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	if !preview.SubmitEligible {
		blockers := previewNotSubmitEligibleBlockers(preview)
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, nil)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreviewForProposal(preview, prop), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	place, err := e.server.proposalPlaceOrder(ctx, rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken, TimeoutMs: p.TimeoutMs, Origin: p.Origin})
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "submit_failed", Message: err.Error()}}
		e.appendBlocked(prop, prop.Key, prop.Revision, blockers, err)
		return rpc.TradeProposalSubmitResult{Proposal: prop, Preview: sanitizeProposalPreviewForProposal(preview, prop), PreviewTokenID: preview.PreviewTokenID, Blockers: blockers, AsOf: now}, nil
	}
	e.appendEvent(proposalEventForProposal("submitted", prop, now, preview.PreviewTokenID, place.OrderRef, "proposal submitted through preview-backed fast path"))
	if e.server.proposalOutcomes != nil {
		if err := e.server.proposalOutcomes.AppendMark(proposalOutcomeSubmitted(prop, preview, place, now)); err != nil {
			e.server.warnf("trade proposal outcomes: append submitted mark: %v", err)
		}
	}
	return rpc.TradeProposalSubmitResult{Accepted: place.Accepted, Proposal: prop, Preview: sanitizeProposalPreviewForProposal(preview, prop), Place: place, PreviewTokenID: preview.PreviewTokenID, OrderRef: place.OrderRef, Message: place.Message, AsOf: e.clock()}, nil
}

// resolveRowMinTick returns the broker-reported minimum increment for a held
// lifetime. Generation and preview must round trail prices on the same grid:
// the proposal-vs-preview drift gate compares them exactly — so the fetch
// row.SecType mapped to its wire code. Passing the row's enum form
// ("STOCK") verbatim made TWS reject the contract-details request with
// error 321 on every refresh: the failure is never cached, so each held
func (e *proposalEngine) resolveRowMinTick(row rpc.PositionView) float64 {
	if e == nil || e.server == nil {
		return 0
	}
	contract := proposalContractFromPosition(row, positionWireSecType(row.SecType))
	return e.server.resolveContractMinTick(context.Background(), contract, previewMinTickTimeout)
}

// closeReduceQuantity sizes a close/reduce order for a possibly fractional
// never mention fractions. The remainder is surfaced in proposal details.
func closeReduceQuantity(position float64) (int, float64) {
	abs := math.Abs(position)
	qty := int(math.Floor(abs + 1e-9))
	remainder := abs - float64(qty)
	if remainder < 1e-9 {
		remainder = 0
	}
	return qty, remainder
}

// duplicateProtectiveBlockers prevents two broker-working exits from
// competing for the same position. Stock/ETF trails retain their stop-like
// duplicate rule; an option loss exit or profit trail conflicts with any open
// same-side exact-contract close order.
func (e *proposalEngine) duplicateProtectiveBlockers(ctx context.Context, p rpc.TradeProposal, currentPositions ...*rpc.PositionsResult) []rpc.TradingBlocker {
	if e == nil || e.server == nil {
		return nil
	}
	optionExit := proposalIsOptionExit(p)
	if !optionExit && (p.Bucket != "" && p.Bucket != rpc.TradeProposalBucketTrailingStop || !isTrailOrderType(p.OrderType)) {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if optionExit {
		return e.optionExitBrokerOrderBlockers(ctx, p, len(currentPositions) == 0)
	}
	var views []rpc.OrderView
	var err error
	if len(currentPositions) > 0 && currentPositions[0] != nil {
		views, _, err = e.server.loadOrderViews()
	} else {
		views, _, err = e.server.loadOrderViewsReconciled(ctx)
	}
	if err != nil {
		return []rpc.TradingBlocker{{
			Code: "protective_order_evidence_unavailable", Message: "current protective-order evidence is unavailable",
			Action: "Refresh and retry after order evidence recovers.",
		}}
	}
	if len(currentPositions) > 0 && currentPositions[0] != nil {
		reconcileFlatPositionProtectiveOrders(views, currentPositions[0], e.clock())
	}
	for _, v := range views {
		if optionExit {
			if !proposalDuplicateOrderIsOptionExit(v, p) {
				continue
			}
			return []rpc.TradingBlocker{{
				Code:    "existing_option_exit_order",
				Message: fmt.Sprintf("open order %s already works an exit for this exact option contract", v.OrderRef),
				Action:  fmt.Sprintf("Keep the standing exit, or cancel it first with `canary order cancel %s` before submitting a replacement.", v.OrderRef),
			}}
		}
		if !proposalDuplicateOrderIsProtective(v, p) {
			continue
		}
		return []rpc.TradingBlocker{{
			Code:    "existing_protective_order",
			Message: fmt.Sprintf("open order %s already works %s %s (%s)", v.OrderRef, p.Action, p.Symbol, nonEmptyString(v.OrderType, "order")),
			Action:  fmt.Sprintf("Keep the standing protection, or cancel it first with `canary order cancel %s` before submitting a replacement.", v.OrderRef),
		}}
	}
	return nil
}

func (e *proposalEngine) optionExitBrokerOrderBlockers(ctx context.Context, p rpc.TradeProposal, forceCurrent bool) []rpc.TradingBlocker {
	block := func(code, message string) []rpc.TradingBlocker {
		return []rpc.TradingBlocker{{Code: code, Message: message, Action: "Refresh and retry only after current broker open-order evidence is complete."}}
	}
	if e == nil || e.server == nil || p.Contract.ConID <= 0 {
		return block("option_exit_order_evidence_unavailable", "exact-contract broker open-order evidence is unavailable")
	}
	binding := e.server.currentProtectionOrderSnapshotBinding()
	if binding.connector == nil || !brokerScopeConcrete(binding.scope) {
		return block("option_exit_order_evidence_unavailable", "broker open-order authority is not bound to a concrete account session")
	}
	var snapshot ibkrlib.OpenOrderSnapshot
	var err error
	if forceCurrent {
		snapshot, err = e.server.snapshotOpenOrdersFrom(ctx, binding.connector)
	} else {
		snapshot, err = e.server.protectionSnapshotOpenOrders(ctx, binding)
	}
	now := e.clock().UTC()
	if err != nil || !snapshot.Complete || snapshot.AsOf.IsZero() || snapshot.AsOf.After(now) || now.Sub(snapshot.AsOf.UTC()) > protectionOrderSnapshotMaxAge {
		return block("option_exit_order_snapshot_unavailable", "complete current all-client API open-order inventory is unavailable")
	}
	receipt := binding
	receipt.session = snapshot.Session
	receipt.generation = snapshot.Generation
	if e.server.orderSnapshotFn != nil && receipt.session == (ibkrlib.ConnectorSessionBinding{}) {
		receipt.session = binding.session
	}
	if !e.server.protectionOrderSnapshotBindingCurrent(receipt) {
		return block("option_exit_order_snapshot_changed", "broker order session changed during the open-order inventory read")
	}
	for _, order := range snapshot.Orders {
		if order.Type != ibkrlib.OrderLifecycleEventOpenOrder || order.WhatIf || !strings.EqualFold(order.Action, p.Action) ||
			!strings.EqualFold(order.SecType, "OPT") || optionExitSnapshotRemaining(order) <= 0 {
			continue
		}
		if order.ConID == p.Contract.ConID {
			return block("existing_option_exit_order", "a broker-working close order already exists for this exact option contract")
		}
		if order.ConID <= 0 && optionExitSnapshotContractCouldMatch(order, p.Contract) {
			return block("option_exit_order_identity_unknown", "a broker-working option close may match this contract but lacks exact positive contract identity")
		}
	}
	return nil
}

func optionExitSnapshotContractCouldMatch(order ibkrlib.OrderLifecycleEvent, contract rpc.ContractParams) bool {
	conflicts := func(left, right string) bool {
		left, right = strings.ToUpper(strings.TrimSpace(left)), strings.ToUpper(strings.TrimSpace(right))
		return left != "" && right != "" && left != right
	}
	if conflicts(order.Symbol, contract.Symbol) || conflicts(order.Currency, contract.Currency) ||
		conflicts(order.Expiry, contract.Expiry) || conflicts(order.Right, contract.Right) ||
		conflicts(order.LocalSymbol, contract.LocalSymbol) || conflicts(order.TradingClass, contract.TradingClass) {
		return false
	}
	if order.Strike > 0 && contract.Strike > 0 && math.Abs(order.Strike-contract.Strike) > 1e-9 {
		return false
	}
	// With no contradictory field, even a partially parsed same-symbol order
	// is plausible competition for the full close and must block.
	return strings.TrimSpace(order.Symbol) == "" || strings.EqualFold(strings.TrimSpace(order.Symbol), strings.TrimSpace(contract.Symbol))
}

func optionExitSnapshotRemaining(order ibkrlib.OrderLifecycleEvent) float64 {
	if order.Remaining > 0 {
		return order.Remaining
	}
	if remaining := order.TotalQuantity - order.Filled; remaining > 0 {
		return remaining
	}
	return 0
}

func proposalIsOptionExit(p rpc.TradeProposal) bool {
	option := strings.EqualFold(p.SecType, "OPT") || strings.EqualFold(p.SecType, "OPTION") ||
		strings.EqualFold(p.Contract.SecType, "OPT") || strings.EqualFold(p.Contract.SecType, "OPTION")
	return option && (p.Bucket == rpc.TradeProposalBucketOptionLossExit || p.Bucket == rpc.TradeProposalBucketOptionExitReview || p.Bucket == rpc.TradeProposalBucketTrailingStop && p.OptionExit != nil)
}

func proposalDuplicateOrderIsOptionExit(v rpc.OrderView, p rpc.TradeProposal) bool {
	if !v.Open || !strings.EqualFold(v.Action, p.Action) || !orderViewMatchesProposalContract(v, p) {
		return false
	}
	return orderViewRemainingQuantity(v) > 0
}

func proposalDuplicateOrderIsProtective(v rpc.OrderView, p rpc.TradeProposal) bool {
	if !v.Open || !strings.EqualFold(v.Action, p.Action) {
		return false
	}
	if !protectionCoverageOrderIsStopLike(v) || protectionCoverageOrderIsProblem(v) {
		return false
	}
	if !strings.EqualFold(v.OpenClose, "C") && !strings.EqualFold(v.Source, proposalOrderSource) {
		return false
	}
	if !orderViewMatchesProposalContract(v, p) {
		return false
	}
	return orderViewActionCanCloseQuantity(v, p.PositionQuantity, orderViewRemainingQuantity(v))
}

func orderViewMatchesProposalContract(v rpc.OrderView, p rpc.TradeProposal) bool {
	if v.ConID != 0 && p.Contract.ConID != 0 {
		return v.ConID == p.Contract.ConID
	}
	if strings.EqualFold(v.SecType, "OPT") || strings.EqualFold(v.SecType, "OPTION") ||
		strings.EqualFold(p.SecType, "OPT") || strings.EqualFold(p.SecType, "OPTION") ||
		strings.EqualFold(p.Contract.SecType, "OPT") || strings.EqualFold(p.Contract.SecType, "OPTION") {
		if v.LocalSymbol != "" && p.Contract.LocalSymbol != "" {
			return strings.EqualFold(v.LocalSymbol, p.Contract.LocalSymbol)
		}
		return strings.EqualFold(v.Symbol, p.Symbol) &&
			equivalentStockSecType(v.SecType, p.SecType) &&
			strings.EqualFold(v.Expiry, p.Contract.Expiry) &&
			strings.EqualFold(v.Right, p.Contract.Right) &&
			math.Abs(v.Strike-p.Contract.Strike) < 1e-9
	}
	return strings.EqualFold(v.Symbol, p.Symbol) && equivalentStockSecType(v.SecType, p.SecType)
}

func equivalentStockSecType(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToUpper(strings.TrimSpace(s))
		// Position rows carry the wire label "STOCK"; order views carry
		// "STK". Both must normalize together or conid-less orders (manual
		if s == "ETF" || s == "STOCK" {
			return "STK"
		}
		return s
	}
	return norm(a) == norm(b)
}

// previewNotSubmitEligibleBlockers keeps the stable blocker code but carries
// the broker's own WhatIf verdict as the message when one exists — "error
func previewNotSubmitEligibleBlockers(preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	blocker := rpc.TradingBlocker{
		Code:    "preview_not_submit_eligible",
		Message: "broker WhatIf did not make this proposal submit-eligible",
		Action:  "Resolve broker WhatIf availability and preview again before submitting a broker-managed stop.",
	}
	if preview != nil {
		if cause := strings.TrimSpace(preview.WhatIf.Message); cause != "" {
			blocker.Message = truncateBlockerCause(cause)
		}
		if action := strings.TrimSpace(preview.WhatIf.Action); action != "" {
			blocker.Action = action
		}
	}
	return []rpc.TradingBlocker{blocker}
}

// truncateBlockerCause bounds broker-originated text so a verbose reject
func truncateBlockerCause(s string) string {
	const maxRunes = 200
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "…"
}

func (e *proposalEngine) Ignore(p rpc.TradeProposalIgnoreParams) rpc.TradeProposalIgnoreResult {
	now := e.clock()
	key := strings.TrimSpace(p.Key)
	if key == "" {
		return rpc.TradeProposalIgnoreResult{Accepted: false, Message: "proposal key is required", AsOf: now}
	}
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) {
		return rpc.TradeProposalIgnoreResult{Accepted: false, Key: key, Revision: strings.TrimSpace(p.Revision), Message: "proposal ignore requires a concrete account and paper/live mode", AsOf: now}
	}
	ev := proposalEvent{At: now, Type: "ignored", Key: key, Revision: strings.TrimSpace(p.Revision), Reason: strings.TrimSpace(p.Reason), Message: "proposal ignored",
		AccountID:   scope.Account,
		AccountMode: scope.Mode}
	if err := e.appendEvent(ev); err != nil {
		return rpc.TradeProposalIgnoreResult{Accepted: false, Key: key, Revision: strings.TrimSpace(p.Revision), Message: "proposal ignore was not persisted", AsOf: now}
	}
	e.mu.Lock()
	if e.ignored == nil {
		e.ignored = map[string]struct{}{}
	}
	e.ignored[scopedIgnoreKey(scope, key)] = struct{}{}
	e.mu.Unlock()
	return rpc.TradeProposalIgnoreResult{Accepted: true, Key: key, Revision: strings.TrimSpace(p.Revision), Message: "proposal ignored", AsOf: now}
}

func (e *proposalEngine) revalidatedProposal(ctx context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
	key, revision = strings.TrimSpace(key), strings.TrimSpace(revision)
	if key == "" || revision == "" {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "bad_request", Message: "proposal key and revision are required"}}, nil
	}
	snap, err := e.Refresh(ctx, false)
	if err != nil && len(snap.Proposals) == 0 {
		return rpc.TradeProposal{}, snap.Blockers, err
	}
	if len(snap.Blockers) > 0 && len(snap.Proposals) == 0 {
		return rpc.TradeProposal{}, snap.Blockers, nil
	}
	if snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusDrift || snap.PolicyStatus.Status == rpc.ProtectionPolicyStatusError {
		return rpc.TradeProposal{}, snap.PolicyStatus.Blockers, nil
	}
	if len(snap.AutoTrade.Blockers) > 0 {
		return rpc.TradeProposal{}, snap.AutoTrade.Blockers, nil
	}
	if snap.Revision != revision {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "proposal revision is stale; refresh proposals before preview or submit"}}, nil
	}
	for _, prop := range snap.Proposals {
		if prop.Key == key {
			if len(snap.Blockers) > 0 {
				return prop, mergeTradingBlockers(snap.Blockers, prop.Blockers), nil
			}
			return prop, prop.Blockers, nil
		}
	}
	return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "proposal key is not present in the current snapshot"}}, nil
}

func proposalOrderPreviewParams(prop rpc.TradeProposal, qty, timeoutMs int) rpc.OrderPreviewParams {
	orderType := strings.ToUpper(strings.TrimSpace(prop.OrderType))
	if orderType == "" {
		orderType = rpc.OrderTypeLMT
	}
	strategy := rpc.OrderStrategyPatientLimit
	if orderType == rpc.OrderTypeTRAIL || orderType == rpc.OrderTypeTRAILLIMIT {
		strategy = rpc.OrderStrategyBrokerTrail
	}
	trail := cloneTrailSpec(prop.Trail)
	return rpc.OrderPreviewParams{Action: prop.Action, Contract: prop.Contract, Quantity: qty, OrderType: orderType, Trail: trail, TriggerMethod: proposalTriggerMethod(prop), Strategy: strategy, TIF: proposalTIF(prop), OutsideRTH: prop.OutsideRTH, TimeoutMs: timeoutMs, Source: proposalOrderSource}
}

// proposalTIF normalizes a proposal's TIF for preview params and the
// drift gate; proposals persisted before the field existed mean DAY.
func proposalTIF(prop rpc.TradeProposal) string {
	tif := strings.ToUpper(strings.TrimSpace(prop.TIF))
	if tif == "" {
		return rpc.OrderTIFDay
	}
	return tif
}

func proposalTriggerMethod(prop rpc.TradeProposal) int {
	if prop.TriggerMethod != rpc.OrderTriggerMethodDefault {
		return prop.TriggerMethod
	}
	if isTrailOrderType(prop.OrderType) && trailTriggerDefaultsToLast(prop.Contract) {
		return rpc.OrderTriggerMethodLast
	}
	return rpc.OrderTriggerMethodDefault
}

// trailingStopTIFDetail spells out the lifetime consequence of the bucket
func trailingStopTIFDetail(tif string, optionPremiumTrail bool) string {
	if strings.EqualFold(tif, rpc.OrderTIFGTC) {
		if optionPremiumTrail {
			return "tif=GTC: stop persists until filled or cancelled; theta decay alone walks the premium into the stop eventually"
		}
		return "tif=GTC: stop persists across sessions until filled or cancelled"
	}
	if optionPremiumTrail {
		return "tif=DAY: option trail expires at the session close and provides no overnight order; GTC is unsupported in option-exit V1"
	}
	return "tif=DAY: stop expires at the session close and does not cover overnight gaps; set tif = \"GTC\" in [buckets.trailing_stop] to persist"
}

func selectedProposalQty(prop rpc.TradeProposal, requested int) int {
	if requested <= 0 {
		return prop.Quantity
	}
	return max(1, min(requested, prop.MaxQuantity))
}

func proposalPreviewSafetyBlockers(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	var blockers []rpc.TradingBlocker
	add := func(code, message, action string) {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{Code: code, Message: message, Action: action})
	}
	if preview == nil {
		add("proposal_preview_missing", "proposal preview result is unavailable", "Refresh and preview the proposal again before submit.")
		return blockers
	}
	if !proposalCloseReduceEffect(prop.PositionEffect) {
		add("proposal_effect_not_close_reduce", fmt.Sprintf("proposal effect %q is not close/reduce", prop.PositionEffect), "Refresh proposals so the daemon can rebuild a close/reduce-only recommendation.")
	}
	if !proposalCloseReduceEffect(preview.Position.Effect) {
		add("preview_effect_not_close_reduce", fmt.Sprintf("preview effect %q is not close/reduce", preview.Position.Effect), "Refresh positions and preview again; proposal submit cannot open, increase, or flip exposure.")
	}
	if !proposalSupportedSecType(prop.SecType) || !proposalSupportedSecType(preview.Draft.Contract.SecType) {
		add("unsupported_security_type", "protection proposals support single-leg STK/ETF/OPT orders only", "Use a manual workflow for unsupported instruments.")
	}
	if !proposalSupportedOrderType(preview.Draft.OrderType) {
		add("unsupported_order_type", fmt.Sprintf("proposal order type %q is not supported", preview.Draft.OrderType), "Refresh proposals and preview a supported close/reduce order.")
	}
	previewTIF := strings.ToUpper(strings.TrimSpace(preview.Draft.TIF))
	switch {
	case previewTIF != rpc.OrderTIFDay && previewTIF != rpc.OrderTIFGTC:
		add("unsupported_tif", fmt.Sprintf("proposal time-in-force %q is not DAY or GTC", preview.Draft.TIF), "Refresh proposals and preview a supported time-in-force.")
	case previewTIF != proposalTIF(prop):
		add("tif_drift", fmt.Sprintf("preview time-in-force %q does not match proposal time-in-force %q", preview.Draft.TIF, proposalTIF(prop)), "Refresh proposals and preview again.")
	}
	if strings.EqualFold(preview.Draft.Contract.SecType, "OPT") && preview.Draft.OutsideRTH {
		add("option_outside_rth", "option protection proposals must not request outside_rth", "Refresh proposals and preview during the supported option session.")
	}
	if preview.Draft.Quantity <= 0 || preview.Draft.Quantity > prop.MaxQuantity {
		add("quantity_outside_position", fmt.Sprintf("proposal preview quantity %d exceeds close/reduce cap %d", preview.Draft.Quantity, prop.MaxQuantity), "Refresh positions and preview a quantity within the current position.")
	}
	if prop.OptionExit != nil && preview.Draft.Quantity != prop.MaxQuantity {
		add("option_exit_full_quantity_required", fmt.Sprintf("option exit preview quantity %d does not equal the full exact-contract quantity %d", preview.Draft.Quantity, prop.MaxQuantity), "Refresh positions and preview the full exact-contract option exit.")
	}
	if prop.OptionExit != nil {
		before := preview.Position.Before
		full := !math.IsNaN(before) && !math.IsInf(before, 0) && before > 0 &&
			math.Abs(before-math.Round(before)) <= 1e-9 && int(math.Round(before)) == preview.Draft.Quantity &&
			preview.Draft.Quantity == prop.MaxQuantity && preview.Position.Effect == rpc.OrderPositionEffectClose
		if !full {
			add("option_exit_fresh_full_close_required", "fresh preview evidence does not prove a full close of the current long exact-contract position", "Refresh positions and preview the full exact-contract close again.")
		}
		if prop.Contract.ConID <= 0 || preview.Draft.Contract.ConID != prop.Contract.ConID {
			add("option_exit_contract_drift", "preview exact contract does not match the option-exit proposal", "Refresh proposals and preview the exact contract again.")
		}
		for _, blocker := range optionExitPreviewDecisionBlockers(prop, preview) {
			add(blocker.Code, blocker.Message, blocker.Action)
		}
	}
	if !strings.EqualFold(preview.Draft.Action, prop.Action) {
		add("action_drift", fmt.Sprintf("preview action %q does not match proposal action %q", preview.Draft.Action, prop.Action), "Refresh proposals and preview again.")
	}
	expectedTriggerMethod := proposalTriggerMethod(prop)
	if preview.Draft.TriggerMethod != expectedTriggerMethod {
		add("trigger_method_drift", fmt.Sprintf("preview trigger_method %d does not match proposal trigger_method %d", preview.Draft.TriggerMethod, expectedTriggerMethod), "Refresh proposals and preview again.")
	}
	propOrderType := strings.ToUpper(strings.TrimSpace(prop.OrderType))
	if propOrderType == "" {
		propOrderType = rpc.OrderTypeLMT
	}
	if strings.ToUpper(strings.TrimSpace(preview.Draft.OrderType)) != propOrderType {
		add("order_type_drift", fmt.Sprintf("preview order type %q does not match proposal order type %q", preview.Draft.OrderType, prop.OrderType), "Refresh proposals and preview again.")
	}
	if isTrailOrderType(preview.Draft.OrderType) {
		switch {
		case prop.Trail == nil:
			add("proposal_trail_missing", "proposal is missing broker-side trail fields", "Refresh proposals and preview again.")
		case preview.Draft.Trail == nil:
			add("trail_missing", "proposal preview is missing broker-side trail fields", "Refresh proposals and preview again.")
		default:
			for _, blocker := range proposalTrailDriftBlockers(prop.Trail, preview.Draft.Trail) {
				add(blocker.Code, blocker.Message, blocker.Action)
			}
		}
	}
	if strings.TrimSpace(preview.Draft.Source) != proposalOrderSource {
		add("source_drift", "proposal preview source does not match the protection proposal engine", "Refresh proposals and preview again.")
	}
	return blockers
}

func optionExitPreviewDecisionBlockers(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	if prop.OptionExit == nil || preview == nil {
		return nil
	}
	var blockers []rpc.TradingBlocker
	add := func(code, message string) {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{
			Code: code, Message: message,
			Action: "Refresh proposals and preview again from current exact-contract evidence.",
		})
	}
	exit := prop.OptionExit
	quote := preview.Quote
	quantity := preview.Position.Before
	multiplier := preview.Position.Multiplier
	costPremium := 0.0
	if multiplier > 0 && preview.Position.AverageCost > 0 &&
		!math.IsNaN(preview.Position.AverageCost) && !math.IsInf(preview.Position.AverageCost, 0) {
		costPremium = preview.Position.AverageCost / float64(multiplier)
	}
	sessionOpen := quote.SessionContext != nil && quote.SessionContext.IsOpen
	in := risk.OptionExitInput{
		ConID: preview.Draft.Contract.ConID, Quantity: quantity, Multiplier: multiplier,
		AvgCost: preview.Position.AverageCost, DTE: exit.DTE,
		DirectionalIntent: true, Standalone: true, EconomicRoleAllowed: true,
		QuoteLive: rpc.IsLiveDataType(quote.DataType), QuoteFresh: !quote.Stale && !quote.PriceAt.IsZero(),
		SessionOpen: sessionOpen,
	}
	if quote.Bid != nil {
		in.Bid = *quote.Bid
	}
	if quote.Ask != nil {
		in.Ask = *quote.Ask
	}
	decision := risk.EvaluateOptionExit(in, risk.OptionExitPolicy{
		MinDTE: exit.MinDTE, LossExitPct: exit.LossExitPct, ProfitArmGainPct: exit.ProfitArmGainPct,
		ProfitTrailPct: exit.ProfitTrailPct, LockedGainPct: exit.LockedGainPct,
		MinTrailPct: exit.MinTrailPct, MaxTrailPct: exit.MaxTrailPct,
		MaxSpreadPctOfMid: exit.MaxSpreadPctOfMid, MinTrailAbs: exit.MinTrailAbs,
		SpreadMultiple: exit.SpreadMultiple,
	})
	if exit.CostBasisPremium == nil || !floatEqual(*exit.CostBasisPremium, costPremium) {
		add("option_exit_cost_basis_changed", "fresh exact-contract average cost no longer matches the proposal")
		return blockers
	}
	if len(decision.Blockers) > 0 {
		add("option_exit_preview_policy_blocked", "fresh preview quote or position no longer satisfies option-exit policy")
		return blockers
	}
	if decision.Action != exit.Kind {
		add("option_exit_threshold_changed", "fresh preview evidence no longer selects the proposal's option-exit action")
		return blockers
	}
	if decision.Action != risk.OptionExitActionProfitTrail {
		return blockers
	}
	if preview.Draft.Trail == nil || preview.Draft.Trail.TrailingPercent == nil || preview.Draft.Trail.TrailingAmount != nil {
		add("option_exit_preview_trail_missing", "fresh preview is missing the approved native percentage premium trail")
		return blockers
	}
	tick := trailMinimumTick(preview.Draft.Contract, decision.ReferencePrice)
	expectedAmount := ceilPriceToTick(decision.TrailAmount, tick)
	expectedPct := expectedAmount / decision.ReferencePrice * 100
	expectedStop := trailingStopInitialPriceForContract(prop.Action, decision.ReferencePrice, expectedAmount, preview.Draft.Contract)
	if !risk.OptionExitTrailPctWithinBounds(decision.ReferencePrice, expectedAmount, exit.MinTrailPct, exit.MaxTrailPct) {
		add("option_trail_outside_policy_bounds", "fresh rounded broker trail is outside the approved percentage range")
	}
	if !risk.OptionExitLockedGainMet(decision.CostPremium, expectedStop, exit.LockedGainPct) {
		add("option_trail_locked_gain_not_met", "fresh rounded broker trail no longer preserves the approved locked gain")
	}
	if !floatEqual(*preview.Draft.Trail.TrailingPercent, expectedPct) ||
		!floatEqual(preview.Draft.Trail.InitialStopPrice, expectedStop) {
		add("option_exit_quote_drift", "fresh exact-contract quote changes the approved trail percentage or initial stop")
	}
	return blockers
}

func proposalTrailDriftBlockers(proposal, preview *rpc.OrderTrailSpec) []rpc.TradingBlocker {
	var blockers []rpc.TradingBlocker
	add := func(code, message string) {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{
			Code:    code,
			Message: message,
			Action:  "Refresh proposals and preview again before submitting a broker-managed stop.",
		})
	}
	if !strings.EqualFold(strings.TrimSpace(proposal.OffsetType), strings.TrimSpace(preview.OffsetType)) {
		add("trail_offset_type_drift", fmt.Sprintf("preview trail offset type %q does not match proposal offset type %q", preview.OffsetType, proposal.OffsetType))
	}
	if !floatPtrEqual(proposal.TrailingPercent, preview.TrailingPercent) {
		add("trail_percent_drift", "preview trailing_percent does not match proposal trailing_percent")
	}
	if !floatPtrEqual(proposal.TrailingAmount, preview.TrailingAmount) {
		add("trail_amount_drift", "preview trailing_amount does not match proposal trailing_amount")
	}
	if !floatPtrEqual(proposal.LimitOffset, preview.LimitOffset) {
		add("trail_limit_offset_drift", "preview limit_offset does not match proposal limit_offset")
	}
	if !floatEqual(proposal.InitialStopPrice, preview.InitialStopPrice) {
		add("trail_initial_stop_drift", "preview initial_stop_price does not match proposal initial_stop_price")
	}
	return blockers
}

func floatPtrEqual(a, b *float64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return math.Abs(*a-*b) < 1e-9
	}
}

func floatEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func proposalSupportedOrderType(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeLMT, rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return true
	default:
		return false
	}
}

func isTrailOrderType(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return true
	default:
		return false
	}
}

func cloneTrailSpec(in *rpc.OrderTrailSpec) *rpc.OrderTrailSpec {
	if in == nil {
		return nil
	}
	out := *in
	out.TrailingPercent = cloneFloat64Ptr(in.TrailingPercent)
	out.TrailingAmount = cloneFloat64Ptr(in.TrailingAmount)
	out.LimitOffset = cloneFloat64Ptr(in.LimitOffset)
	return &out
}

func cloneTrailSizing(in *rpc.TradeProposalTrailSizing) *rpc.TradeProposalTrailSizing {
	if in == nil {
		return nil
	}
	out := *in
	out.ReferencePrice = cloneFloat64Ptr(in.ReferencePrice)
	out.ChosenAmount = cloneFloat64Ptr(in.ChosenAmount)
	out.InitialStopPrice = cloneFloat64Ptr(in.InitialStopPrice)
	out.ATR14 = cloneFloat64Ptr(in.ATR14)
	out.ATRPct = cloneFloat64Ptr(in.ATRPct)
	out.ATRMultiplier = cloneFloat64Ptr(in.ATRMultiplier)
	out.ATRCandidatePct = cloneFloat64Ptr(in.ATRCandidatePct)
	out.SpreadPct = cloneFloat64Ptr(in.SpreadPct)
	out.SpreadMultiplier = cloneFloat64Ptr(in.SpreadMultiplier)
	out.SpreadFloorPct = cloneFloat64Ptr(in.SpreadFloorPct)
	out.MissingReasons = append([]string(nil), in.MissingReasons...)
	return &out
}

func cloneOptionExit(in *rpc.TradeProposalOptionExit) *rpc.TradeProposalOptionExit {
	if in == nil {
		return nil
	}
	out := *in
	out.CostBasisPremium = cloneFloat64Ptr(in.CostBasisPremium)
	out.ReferencePrice = cloneFloat64Ptr(in.ReferencePrice)
	out.ReturnPct = cloneFloat64Ptr(in.ReturnPct)
	out.InitialLockedGainPct = cloneFloat64Ptr(in.InitialLockedGainPct)
	return &out
}

func cloneExecutionSemantics(in *rpc.TradeProposalExecutionSemantics) *rpc.TradeProposalExecutionSemantics {
	if in == nil {
		return nil
	}
	out := *in
	out.ReferencePrice = cloneFloat64Ptr(in.ReferencePrice)
	return &out
}

func cloneStopRisk(in *rpc.TradeProposalStopRisk) *rpc.TradeProposalStopRisk {
	if in == nil {
		return nil
	}
	out := *in
	out.ReferencePrice = cloneFloat64Ptr(in.ReferencePrice)
	out.StopPrice = cloneFloat64Ptr(in.StopPrice)
	out.Distance = cloneFloat64Ptr(in.Distance)
	out.DistancePct = cloneFloat64Ptr(in.DistancePct)
	out.EstimatedLoss = cloneFloat64Ptr(in.EstimatedLoss)
	out.EstimatedLossBase = cloneFloat64Ptr(in.EstimatedLossBase)
	out.EstimatedLossPctNLV = cloneFloat64Ptr(in.EstimatedLossPctNLV)
	out.WarningCodes = append([]string(nil), in.WarningCodes...)
	if in.GapScenario != nil {
		gap := *in.GapScenario
		gap.AssumedExecutionPrice = cloneFloat64Ptr(in.GapScenario.AssumedExecutionPrice)
		gap.EstimatedLoss = cloneFloat64Ptr(in.GapScenario.EstimatedLoss)
		gap.EstimatedLossBase = cloneFloat64Ptr(in.GapScenario.EstimatedLossBase)
		gap.EstimatedLossPctNLV = cloneFloat64Ptr(in.GapScenario.EstimatedLossPctNLV)
		out.GapScenario = &gap
	}
	return &out
}

func cloneStopLadder(in []rpc.TradeProposalStopLadderStep) []rpc.TradeProposalStopLadderStep {
	out := append([]rpc.TradeProposalStopLadderStep(nil), in...)
	for i := range out {
		out[i].Percent = cloneFloat64Ptr(in[i].Percent)
		out[i].StopPrice = cloneFloat64Ptr(in[i].StopPrice)
		out[i].EstimatedLoss = cloneFloat64Ptr(in[i].EstimatedLoss)
		out[i].EstimatedLossBase = cloneFloat64Ptr(in[i].EstimatedLossBase)
		out[i].EstimatedLossPctNLV = cloneFloat64Ptr(in[i].EstimatedLossPctNLV)
		out[i].ReferencePrice = cloneFloat64Ptr(in[i].ReferencePrice)
	}
	return out
}

func mergeTradingBlockers(first, second []rpc.TradingBlocker) []rpc.TradingBlocker {
	out := append([]rpc.TradingBlocker(nil), first...)
	for _, blocker := range second {
		out = appendTradingBlockerOnce(out, blocker)
	}
	return out
}

func proposalCloseReduceEffect(effect string) bool {
	switch effect {
	case rpc.OrderPositionEffectClose, rpc.OrderPositionEffectReduce:
		return true
	default:
		return false
	}
}

func sanitizeProposalPreviewForProposal(in *rpc.OrderPreviewResult, prop rpc.TradeProposal) *rpc.TradeProposalOrderPreview {
	if in == nil {
		return nil
	}
	return &rpc.TradeProposalOrderPreview{PreviewTokenID: in.PreviewTokenID, PreviewTokenScope: in.PreviewTokenScope, PreviewTokenExpiresAt: in.PreviewTokenExpiresAt, TokenMinted: in.TokenMinted, SubmitEligible: in.SubmitEligible, Mode: in.Mode, Account: in.Account, Endpoint: in.Endpoint, ClientID: in.ClientID, Draft: in.Draft, Quote: in.Quote, Position: in.Position, ExecutionSemantics: cloneExecutionSemantics(prop.ExecutionSemantics), StopRisk: cloneStopRisk(prop.StopRisk), Notional: in.Notional, MaxNotional: in.MaxNotional, WhatIf: in.WhatIf, Warnings: append([]rpc.DataWarning(nil), in.Warnings...), AsOf: in.AsOf}
}

func (e *proposalEngine) installSnapshot(snap rpc.TradeProposalSnapshot, show bool, extraEvents ...proposalEvent) error {
	e.mu.Lock()
	prevRevision := e.snapshot.Revision
	prevMarkDate := e.snapshot.AsOf.Format(time.DateOnly)
	e.mu.Unlock()
	// "generated" events and daily outcome marks record new generation
	newRevision := snap.Revision != prevRevision
	newMarkDate := snap.AsOf.Format(time.DateOnly) != prevMarkDate
	var generated []proposalEvent
	if newRevision {
		generated = append(generated, extraEvents...)
		for _, prop := range snap.Proposals {
			ev := proposalEventForProposal("generated", prop, snap.AsOf, "", "", "proposal generated")
			ev.AccountID = snap.AccountID
			ev.AccountMode = snap.AccountMode
			generated = append(generated, ev)
		}
	}
	// Persist the authoritative current document and its generation events in
	// one SQLite transaction before changing the served cache. A failed CAS,
	// closed database, or latched integrity error therefore leaves both the
	// previous current row and the in-memory view unchanged.
	if proposalSnapshotPersistable(snap) {
		if e.store == nil {
			return errors.New("proposal store is not attached")
		}
		if err := e.store.SaveCurrentWithEvents(context.Background(), snap, generated); err != nil {
			return fmt.Errorf("persist proposal snapshot: %w", err)
		}
	}
	e.replaceSnapshot(snap)
	for _, prop := range snap.Proposals {
		if (newRevision || newMarkDate) && e.server != nil && e.server.proposalOutcomes != nil {
			if err := e.server.proposalOutcomes.AppendMark(proposalOutcomeMarked(prop, snap.AsOf)); err != nil {
				e.server.warnf("trade proposal outcomes: append daily mark: %v", err)
			}
		}
	}
	if show {
		e.appendShownEvents(snap)
	}
	return nil
}

func (e *proposalEngine) installPreservedSnapshot(snap rpc.TradeProposalSnapshot, show bool) {
	e.replaceSnapshot(snap)
	if show {
		e.appendShownEvents(snap)
	}
}

func (e *proposalEngine) replaceSnapshot(snap rpc.TradeProposalSnapshot) {
	e.mu.Lock()
	e.snapshot = cloneProposalSnapshot(snap)
	e.mu.Unlock()
}

func (e *proposalEngine) preserveSnapshotOnRefreshFailure(scope brokerStateScope, autoStatus rpc.AutoTradeStatus, policyStatus rpc.ProtectionPolicyStatus, blockers []rpc.TradingBlocker, show bool) (rpc.TradeProposalSnapshot, bool) {
	e.mu.Lock()
	snap := cloneProposalSnapshot(e.snapshot)
	e.mu.Unlock()
	if !proposalSnapshotUsable(snap) || !sameProposalPolicy(snap, policyStatus) {
		return rpc.TradeProposalSnapshot{}, false
	}
	// Preserving last-good proposals through a transient fetch failure is
	// only safe when they were generated for the same session: a paper
	if !sameBrokerScope(brokerStateScope{Account: snap.AccountID, Mode: snap.AccountMode}, scope) {
		if e.server != nil {
			e.server.warnf("trade proposals: dropping preserved snapshot on refresh failure: snapshot scope %q/%q does not match connected session %q/%q", snap.AccountID, snap.AccountMode, scope.Account, scope.Mode)
		}
		return rpc.TradeProposalSnapshot{}, false
	}
	snap.AutoTrade = autoStatus
	snap.PolicyStatus = policyStatus
	snap.Trading = autoStatus.Trading
	merged := append([]rpc.TradingBlocker(nil), blockers...)
	for _, blocker := range snap.Blockers {
		merged = appendTradingBlockerOnce(merged, blocker)
	}
	snap.Blockers = merged
	e.installPreservedSnapshot(snap, show)
	return snap, true
}

func proposalSnapshotUsable(snap rpc.TradeProposalSnapshot) bool {
	return snap.Kind == rpc.TradeProposalSnapshotKind && snap.Revision != "" && snap.Revision != "empty" && len(snap.Proposals) > 0
}

// proposalSnapshotPersistable reports whether snap is a generated,
// concretely scoped snapshot (including a legitimate zero-proposal
// generation) rather than a transient error/unscoped shell. Only these
// are written to disk; see replaceSnapshot.
func proposalSnapshotPersistable(snap rpc.TradeProposalSnapshot) bool {
	return snap.Revision != "" && snap.Revision != "empty" &&
		brokerScopeConcrete(brokerStateScope{Account: snap.AccountID, Mode: snap.AccountMode})
}

func sameProposalPolicy(snap rpc.TradeProposalSnapshot, status rpc.ProtectionPolicyStatus) bool {
	if snap.PolicyID != "" && status.PolicyID != "" && snap.PolicyID != status.PolicyID {
		return false
	}
	if snap.PolicyVersion != 0 && status.PolicyVersion != 0 && snap.PolicyVersion != status.PolicyVersion {
		return false
	}
	if snap.PolicyFingerprint.Key != "" && status.Fingerprint.Key != "" && snap.PolicyFingerprint.Key != status.Fingerprint.Key {
		return false
	}
	return true
}

func (e *proposalEngine) appendShownEvents(snap rpc.TradeProposalSnapshot) {
	for _, prop := range snap.Proposals {
		e.appendEvent(proposalEventForProposal("shown", prop, e.clock(), "", "", "proposal shown"))
	}
}

func (e *proposalEngine) appendBlocked(prop rpc.TradeProposal, key, revision string, blockers []rpc.TradingBlocker, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	} else if len(blockers) > 0 {
		msg = blockers[0].Message
	}
	ev := proposalEventForProposal("blocked", prop, e.clock(), "", "", msg)
	if ev.Key == "" {
		ev.Key = strings.TrimSpace(key)
	}
	if ev.Revision == "" {
		ev.Revision = strings.TrimSpace(revision)
	}
	e.appendEvent(ev)
}

func proposalEventForProposal(eventType string, prop rpc.TradeProposal, at time.Time, tokenID, orderRef, msg string) proposalEvent {
	return proposalEvent{At: at, Type: eventType, Key: prop.Key, Revision: prop.Revision, Bucket: prop.Bucket, PolicyID: prop.PolicyID, PolicyVersion: prop.PolicyVersion, PolicyFingerprint: prop.PolicyFingerprint, PreviewTokenID: tokenID, OrderRef: orderRef, Message: msg, SourceFingerprints: prop.SourceFingerprints}
}

func (e *proposalEngine) appendEvent(ev proposalEvent) error {
	if e == nil || e.store == nil {
		return errors.New("proposal store is not attached")
	}
	if ev.AccountID == "" || ev.AccountMode == "" {
		e.mu.Lock()
		if ev.AccountID == "" {
			ev.AccountID = e.snapshot.AccountID
		}
		if ev.AccountMode == "" {
			ev.AccountMode = e.snapshot.AccountMode
		}
		e.mu.Unlock()
	}
	if err := e.store.AppendEvent(ev); err != nil {
		if e.server != nil {
			e.server.warnf("trade proposals: append event: %v", err)
		}
		return err
	}
	return nil
}

func (e *proposalEngine) isIgnored(scope brokerStateScope, key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.ignored[scopedIgnoreKey(scope, key)]
	return ok
}

func scopedIgnoreKey(scope brokerStateScope, key string) string {
	return strings.ToUpper(strings.TrimSpace(scope.Account)) + "|" + strings.ToLower(strings.TrimSpace(scope.Mode)) + "|" + key
}

// currentScope resolves the connected broker session identity (account +
func (e *proposalEngine) currentScope() brokerStateScope {
	if e == nil {
		return brokerStateScope{}
	}
	if e.scope != nil {
		return e.scope()
	}
	return e.server.currentBrokerStateScope()
}

// proposalScopeBlockers reports why a snapshot bound to snapAccount/snapMode
// must not be served or acted on under the current broker scope; nil means
func proposalScopeBlockers(snapAccount, snapMode string, scope brokerStateScope) []rpc.TradingBlocker {
	if !brokerScopeConcrete(scope) {
		return []rpc.TradingBlocker{proposalScopeUnscopedBlocker(scope)}
	}
	if !sameBrokerScope(brokerStateScope{Account: snapAccount, Mode: snapMode}, scope) {
		return []rpc.TradingBlocker{{
			Code:    "proposal_scope_mismatch",
			Message: fmt.Sprintf("proposal snapshot was generated for account %q mode %q but the connected session is account %q mode %q", snapAccount, snapMode, scope.Account, scope.Mode),
			Action:  "Refresh proposals to regenerate them for the connected session.",
		}}
	}
	return nil
}

func proposalScopeUnscopedBlocker(scope brokerStateScope) rpc.TradingBlocker {
	return rpc.TradingBlocker{
		Code:    "account_identity_unscoped",
		Message: fmt.Sprintf("connected session has no concrete single-account identity (observed account %q mode %q); protection proposals are scoped per account and paper/live mode", scope.Account, scope.Mode),
		Action:  "Reconnect TWS/Gateway with a single concrete account, then refresh proposals.",
	}
}

func (e *proposalEngine) clock() time.Time {
	if e.now != nil {
		return e.now().UTC()
	}
	return time.Now().UTC()
}

func emptyProposalSnapshot(now time.Time) rpc.TradeProposalSnapshot {
	return rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, AsOf: now, Revision: "empty", Proposals: []rpc.TradeProposal{}}
}

func proposalCounts(proposals []rpc.TradeProposal, baseCurrency string) rpc.TradeProposalCounts {
	var out rpc.TradeProposalCounts
	out.Total = len(proposals)
	var thetaBase, riskBase float64
	thetaBaseOK, riskBaseOK := true, true
	for _, p := range proposals {
		if len(p.Blockers) == 0 {
			out.Actionable++
		}
		out.MarketFlags += len(p.MarketFlags)
		switch p.Bucket {
		case rpc.TradeProposalBucketThetaHygiene:
			out.ThetaHygiene++
			out.ThetaPerDay += p.ThetaPerDay
			out.ThetaPerDayCurrency = mergedCurrency(out.ThetaPerDayCurrency, p.Contract.Currency)
			if p.ThetaPerDayBase != nil {
				thetaBase += *p.ThetaPerDayBase
			} else {
				thetaBaseOK = false
			}
		case rpc.TradeProposalBucketRiskReduction:
			out.RiskReduction++
			out.RiskReductionExcessNotional += p.RiskExcessNotional
			out.RiskReductionExcessCurrency = mergedCurrency(out.RiskReductionExcessCurrency, p.RiskExcessCurrency)
			if p.RiskExcessNotionalBase != nil {
				riskBase += *p.RiskExcessNotionalBase
			} else {
				riskBaseOK = false
			}
		case rpc.TradeProposalBucketTrailingStop:
			out.TrailingStop++
		case rpc.TradeProposalBucketOptionLossExit:
			out.OptionLossExit++
		case rpc.TradeProposalBucketOptionExitReview:
			out.OptionExitReview++
		}
	}
	// A raw sum across different local currencies is meaningless. Rather
	if out.RiskReductionExcessCurrency == "MIX" {
		out.RiskReductionExcessNotional = 0
		out.RiskReductionExcessCurrency = ""
	}
	// ThetaPerDay has no omitempty and legacy renderers print it raw, so a
	// mixed-currency sum keeps its value and only loses the label — zeroing
	if out.ThetaPerDayCurrency == "MIX" {
		out.ThetaPerDayCurrency = ""
	}
	// Base twins: served only when every contributing proposal converted
	// (nil means unavailable, not zero) and the account base is known.
	if baseCurrency = normCcy(baseCurrency); baseCurrency != "" {
		if out.ThetaHygiene > 0 && thetaBaseOK {
			out.ThetaPerDayBase = &thetaBase
		}
		if out.RiskReduction > 0 && riskBaseOK {
			out.RiskReductionExcessNotionalBase = &riskBase
		}
		if out.ThetaPerDayBase != nil || out.RiskReductionExcessNotionalBase != nil {
			out.BaseCurrency = baseCurrency
		}
	}
	return out
}

func proposalRevision(policy rpc.Fingerprint, sources rpc.TradeProposalSourceFingerprints, scope brokerStateScope, proposals []rpc.TradeProposal) string {
	stableSources := sources
	// Regime and market-event evidence are informative for ranking and blockers,
	// but their source-health fields can advance between list and preview. Keep
	// revision anchored to policy/account/positions so the one-confirm path does
	// not false-stale while refreshed proposals still carry live blockers.
	stableSources.Regime = nil
	stableSources.MarketEvents = nil
	// Account/mode enter the revision directly: the account and positions
	projection := struct {
		Policy   rpc.Fingerprint                     `json:"policy"`
		Account  string                              `json:"account"`
		Mode     string                              `json:"mode"`
		Sources  rpc.TradeProposalSourceFingerprints `json:"sources"`
		Proposal []string                            `json:"proposal"`
	}{Policy: policy, Account: strings.ToUpper(strings.TrimSpace(scope.Account)), Mode: strings.ToLower(strings.TrimSpace(scope.Mode)), Sources: stableSources}
	for _, p := range proposals {
		projection.Proposal = append(projection.Proposal, p.Key+":"+strconv.Itoa(p.Quantity)+":"+p.PositionEffect)
	}
	raw, _ := json.Marshal(projection)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func proposalKey(bucket string, contract rpc.ContractParams, action string) string {
	exactIdentity := ""
	if contract.ConID > 0 && (strings.EqualFold(contract.SecType, "OPT") || strings.EqualFold(contract.SecType, "OPTION")) {
		exactIdentity = "CONID:" + strconv.Itoa(contract.ConID)
	}
	raw := strings.Join([]string{bucket, exactIdentity, strings.ToUpper(contract.Symbol), strings.ToUpper(contract.SecType), strings.ToUpper(contract.LocalSymbol), strings.ToUpper(contract.TradingClass), strings.ToUpper(contract.Exchange), contract.Expiry, strings.ToUpper(contract.Right), fmt.Sprintf("%.4f", contract.Strike), strings.ToUpper(action)}, "|")
	sum := sha256.Sum256([]byte(raw))
	return bucket + ":" + hex.EncodeToString(sum[:8])
}

func optionDTE(expiry string, now time.Time) (int, bool) {
	expiry = strings.TrimSpace(expiry)
	var t time.Time
	var err error
	switch len(expiry) {
	case len("20060102"):
		t, err = time.ParseInLocation("20060102", expiry, now.Location())
	case len("2006-01-02"):
		t, err = time.ParseInLocation("2006-01-02", expiry, now.Location())
	default:
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	expiryDay := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return int(expiryDay.Sub(today) / (24 * time.Hour)), true
}

func groupMarketValueOrderValue(g rpc.PositionGroup) float64 {
	if g.GroupMarketValue != 0 {
		return g.GroupMarketValue
	}
	if g.GroupMarketValueBase != nil {
		return *g.GroupMarketValueBase
	}
	return 0
}

func mergedCurrency(existing, next string) string {
	next = strings.ToUpper(strings.TrimSpace(next))
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	if existing == next {
		return existing
	}
	return "MIX"
}

func proposalSupportedSecType(secType string) bool {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK", "ETF", "OPT", "OPTION":
		return true
	default:
		return false
	}
}

func cloneProposalSnapshot(in rpc.TradeProposalSnapshot) rpc.TradeProposalSnapshot {
	out := in
	out.Proposals = append([]rpc.TradeProposal(nil), in.Proposals...)
	for i := range out.Proposals {
		out.Proposals[i].Trail = cloneTrailSpec(in.Proposals[i].Trail)
		out.Proposals[i].TrailSizing = cloneTrailSizing(in.Proposals[i].TrailSizing)
		out.Proposals[i].OptionExit = cloneOptionExit(in.Proposals[i].OptionExit)
		out.Proposals[i].ExecutionSemantics = cloneExecutionSemantics(in.Proposals[i].ExecutionSemantics)
		out.Proposals[i].StopRisk = cloneStopRisk(in.Proposals[i].StopRisk)
		out.Proposals[i].StopLadder = cloneStopLadder(in.Proposals[i].StopLadder)
		out.Proposals[i].Details = append([]string(nil), in.Proposals[i].Details...)
		out.Proposals[i].MarketFlags = append([]rpc.MarketEventFlag(nil), in.Proposals[i].MarketFlags...)
		out.Proposals[i].Blockers = append([]rpc.TradingBlocker(nil), in.Proposals[i].Blockers...)
	}
	out.Blockers = append([]rpc.TradingBlocker(nil), in.Blockers...)
	if in.MarketEvents != nil {
		events := *in.MarketEvents
		events.Flags = append([]rpc.MarketEventFlag(nil), in.MarketEvents.Flags...)
		events.SourceHealth = append([]rpc.SourceHealth(nil), in.MarketEvents.SourceHealth...)
		events.BorrowFeeCoverage = append([]rpc.MarketEventBorrowFeeCoverage(nil), in.MarketEvents.BorrowFeeCoverage...)
		for i := range events.BorrowFeeCoverage {
			if feeRate := in.MarketEvents.BorrowFeeCoverage[i].FeeRate; feeRate != nil {
				value := *feeRate
				events.BorrowFeeCoverage[i].FeeRate = &value
			}
			if failure := in.MarketEvents.BorrowFeeCoverage[i].LastFailure; failure != nil {
				copy := *failure
				events.BorrowFeeCoverage[i].LastFailure = &copy
			}
		}
		events.WarningDetails = append([]rpc.DataWarning(nil), in.MarketEvents.WarningDetails...)
		if in.MarketEvents.BySymbol != nil {
			events.BySymbol = make(map[string][]rpc.MarketEventFlag, len(in.MarketEvents.BySymbol))
			for sym, flags := range in.MarketEvents.BySymbol {
				events.BySymbol[sym] = append([]rpc.MarketEventFlag(nil), flags...)
			}
		}
		out.MarketEvents = &events
	}
	return out
}

func (s *Server) handleAutoTradeStatus() *rpc.AutoTradeStatus {
	st := s.autoTradeStatus()
	return &st
}

func (s *Server) handleTradeProposalsSnapshot(req *rpc.Request) *rpc.TradeProposalSnapshot {
	var p rpc.TradeProposalSnapshotParams
	_ = decodeParams(req.Params, &p)
	if s.tradeProposals == nil {
		snap := emptyProposalSnapshot(s.orderNow())
		return &snap
	}
	snap := s.tradeProposals.Snapshot(p.Show)
	return &snap
}

func (s *Server) handleTradeProposalsRefresh(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalSnapshot, error) {
	var p rpc.TradeProposalRefreshParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		snap := emptyProposalSnapshot(s.orderNow())
		return &snap, nil
	}
	snap, err := s.tradeProposals.Refresh(ctx, p.Show)
	return &snap, err
}

func (s *Server) handleTradeProposalsPreview(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalPreviewResult, error) {
	var p rpc.TradeProposalPreviewParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalPreviewResult{Accepted: false, AsOf: s.orderNow(), Blockers: []rpc.TradingBlocker{{Code: "proposal_engine_unavailable", Message: "proposal engine is unavailable"}}}, nil
	}
	res, err := s.tradeProposals.Preview(ctx, p)
	return &res, err
}

func (s *Server) handleTradeProposalsSubmit(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalSubmitResult, error) {
	var p rpc.TradeProposalSubmitParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalSubmitResult{Accepted: false, AsOf: s.orderNow(), Blockers: []rpc.TradingBlocker{{Code: "proposal_engine_unavailable", Message: "proposal engine is unavailable"}}}, nil
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	res, err := s.tradeProposals.Submit(ctx, p)
	return &res, err
}

func (s *Server) handleTradeProposalsIgnore(req *rpc.Request) *rpc.TradeProposalIgnoreResult {
	var p rpc.TradeProposalIgnoreParams
	_ = decodeParams(req.Params, &p)
	if s.tradeProposals == nil {
		return &rpc.TradeProposalIgnoreResult{Accepted: false, Key: p.Key, Revision: p.Revision, Message: "proposal engine is unavailable", AsOf: s.orderNow()}
	}
	res := s.tradeProposals.Ignore(p)
	return &res
}

func (s *Server) autoTradeStatus() rpc.AutoTradeStatus {
	now := s.orderNow()
	cfg := s.cfg.AutoTrade.WithDefaults()
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	trading := s.tradingStatus(ep)
	policy := rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDisabled}
	if s.protectionPolicies != nil {
		policy = s.protectionPolicies.Status()
	}
	out := rpc.AutoTradeStatus{
		Kind:             "ibkr.auto_trade_status",
		AsOf:             now,
		Trading:          trading,
		ProposalsEnabled: cfg.ProposalsEnabledResolved(),
		FastPathEnabled:  cfg.FastPathEnabledResolved(),
		HotReload:        cfg.HotReloadEnabled(),
		ReloadInterval:   cfg.ReloadIntervalDuration().String(),
		ProposalCadence:  cfg.ProposalCadenceDuration().String(),
		Policy:           policy,
	}
	if !out.ProposalsEnabled {
		out.Blockers = append(out.Blockers, rpc.TradingBlocker{Code: "proposals_disabled", Message: "manual proposals are disabled by config"})
	}
	if policy.Status == rpc.ProtectionPolicyStatusDrift || policy.Status == rpc.ProtectionPolicyStatusError {
		out.Blockers = append(out.Blockers, policy.Blockers...)
	}
	out.Blocked = len(out.Blockers) > 0
	return out
}
