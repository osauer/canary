package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const opportunityRefreshRetryBase = 30 * time.Second

// opportunityRefreshBackoffCap bounds sustained-failure retries independently
const opportunityRefreshBackoffCap = 15 * time.Minute

type opportunityEngine struct {
	server  *Server
	store   *opportunityStore
	cadence time.Duration
	now     func() time.Time

	mu       sync.Mutex
	snapshot rpc.OpportunitySnapshot
	ignored  map[string]struct{}

	refreshFailStreak int
	refreshFailSince  time.Time
	refreshFailCodes  []string

	kickOnce sync.Once
	kick     chan struct{}
}

// opportunityRefreshHealth is the engine's refresh-streak view for the
type opportunityRefreshHealth struct {
	Streak     int
	Since      time.Time
	Codes      []string
	ServedAsOf time.Time
}

type opportunityEvent struct {
	Version            int                               `json:"version"`
	At                 time.Time                         `json:"at"`
	Type               string                            `json:"type"`
	Key                string                            `json:"key,omitempty"`
	Revision           string                            `json:"revision,omitempty"`
	AccountID          string                            `json:"account_id,omitempty"`
	AccountMode        string                            `json:"account_mode,omitempty"`
	PolicyID           string                            `json:"policy_id,omitempty"`
	PolicyVersion      int                               `json:"policy_version,omitempty"`
	PolicyFingerprint  rpc.Fingerprint                   `json:"policy_fingerprint,omitzero"`
	PreviewTokenID     string                            `json:"preview_token_id,omitempty"`
	OrderRef           string                            `json:"order_ref,omitempty"`
	Message            string                            `json:"message,omitempty"`
	Reason             string                            `json:"reason,omitempty"`
	SourceFingerprints rpc.OpportunitySourceFingerprints `json:"source_fingerprints,omitzero"`
}

func (s *Server) installOpportunityEngine() {
	e := &opportunityEngine{
		server:  s,
		store:   &opportunityStore{},
		cadence: s.cfg.Opportunities.WithDefaults().RefreshCadenceDuration(),
		now:     s.now,
		ignored: map[string]struct{}{},
	}
	s.opportunities = e
}

func (e *opportunityEngine) Run(ctx context.Context) {
	if e == nil {
		return
	}
	e.Kick()
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	next := e.cadence
	if next <= 0 {
		next = config.Opportunities{}.WithDefaults().RefreshCadenceDuration()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.kickCh():
		case <-timer.C:
		}
		// Refresh records the outcome itself (noteRefreshOutcome); a second
		// call here would double-count the failure streak, halving the
		snap, err := e.Refresh(ctx, false)
		wait := next
		if err != nil || opportunityRefreshTransient(snap) {
			wait = refreshBackoff(next, opportunityRefreshRetryBase, opportunityRefreshBackoffCap, e.RefreshHealth().Streak)
		}
		timer.Reset(wait)
	}
}

func (e *opportunityEngine) Kick() {
	if e == nil {
		return
	}
	select {
	case e.kickCh() <- struct{}{}:
	default:
	}
}

func (e *opportunityEngine) kickCh() chan struct{} {
	e.kickOnce.Do(func() {
		e.kick = make(chan struct{}, 1)
	})
	return e.kick
}

func (e *opportunityEngine) Snapshot(show bool) rpc.OpportunitySnapshot {
	if e == nil {
		return emptyOpportunitySnapshot(time.Now().UTC())
	}
	e.mu.Lock()
	snap := cloneOpportunitySnapshot(e.snapshot)
	e.mu.Unlock()
	if snap.Kind == "" {
		snap = emptyOpportunitySnapshot(e.clock())
	}
	if show {
		e.appendShownEvents(snap)
	}
	return snap
}

func (e *opportunityEngine) Refresh(ctx context.Context, show bool) (rpc.OpportunitySnapshot, error) {
	if e == nil {
		return emptyOpportunitySnapshot(time.Now().UTC()), nil
	}
	snap, err := e.refresh(ctx, show)
	e.noteRefreshOutcome(snap, err)
	return snap, err
}

// RefreshHealth reports the current transient-failure streak and the as_of
func (e *opportunityEngine) RefreshHealth() opportunityRefreshHealth {
	if e == nil {
		return opportunityRefreshHealth{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return opportunityRefreshHealth{
		Streak:     e.refreshFailStreak,
		Since:      e.refreshFailSince,
		Codes:      append([]string(nil), e.refreshFailCodes...),
		ServedAsOf: e.snapshot.AsOf,
	}
}

// noteRefreshOutcome advances the transient-failure streak after every
// Transient failures preserve the last-good snapshot and return err == nil
// — the blocker codes are the only signal — so this is where a stalled
// one warn per failed attempt: Run's backoff (refreshBackoff) paces those at
// refreshBackoff with the proposals engine so both broker-state feeds diagnose
func (e *opportunityEngine) noteRefreshOutcome(snap rpc.OpportunitySnapshot, err error) {
	if e == nil {
		return
	}
	failed := err != nil || opportunityRefreshTransient(snap)
	now := e.clock()
	e.mu.Lock()
	if !failed {
		streak, since := e.refreshFailStreak, e.refreshFailSince
		e.refreshFailStreak, e.refreshFailSince, e.refreshFailCodes = 0, time.Time{}, nil
		e.mu.Unlock()
		if streak >= proposalRefreshWarnStreak && e.server != nil {
			e.server.infof("opportunities: refresh recovered after %d blocked attempts over %s", streak, now.Sub(since).Round(time.Second))
		}
		return
	}
	e.refreshFailStreak++
	if e.refreshFailStreak == 1 {
		e.refreshFailSince = now
	}
	e.refreshFailCodes = opportunityBlockerCodes(snap, err)
	streak, since, codes := e.refreshFailStreak, e.refreshFailSince, e.refreshFailCodes
	e.mu.Unlock()
	if streak < proposalRefreshWarnStreak || e.server == nil {
		return
	}
	e.server.warnf("opportunities: refresh blocked %d consecutive times over %s (codes: %s); serving snapshot as_of %s (%s old)",
		streak, now.Sub(since).Round(time.Second), strings.Join(codes, ","),
		snap.AsOf.Format(time.RFC3339), now.Sub(snap.AsOf).Round(time.Second))
}

// opportunityRefreshTransient reports whether the installed snapshot is
// blocked on a condition the next broker heartbeat can clear (connection
// settling). Refresh failures that preserve a last-good snapshot return
// operator-owned states, not outages, and must not count as refresh
// failures.
func opportunityRefreshTransient(snap rpc.OpportunitySnapshot) bool {
	for _, b := range snap.Blockers {
		switch b.Code {
		case "opportunity_scope_unavailable", "opportunity_scope_mismatch", "account_unavailable", "positions_unavailable", "positions_pending":
			return true
		}
	}
	return false
}

// opportunityBlockerCodes flattens the installed snapshot's blocker codes
// failure path produced no blockers.
func opportunityBlockerCodes(snap rpc.OpportunitySnapshot, err error) []string {
	var out []string
	for _, blocker := range snap.Blockers {
		if blocker.Code != "" && !slices.Contains(out, blocker.Code) {
			out = append(out, blocker.Code)
		}
	}
	if len(out) == 0 && err != nil {
		out = append(out, err.Error())
	}
	return out
}

func (e *opportunityEngine) refresh(ctx context.Context, show bool) (rpc.OpportunitySnapshot, error) {
	now := e.clock()
	cfg := e.server.cfg.Opportunities.WithDefaults()
	status := e.server.opportunityStatus()
	if !cfg.EnabledResolved() {
		snap := emptyOpportunitySnapshot(now)
		snap.Status = status
		snap.PolicyStatus = status.Policy
		snap.Trading = status.Trading
		snap.Blockers = []rpc.TradingBlocker{{Code: "opportunities_disabled", Message: "opportunities are disabled by config"}}
		if err := e.installSnapshot(snap, show); err != nil {
			return e.Snapshot(false), err
		}
		return snap, nil
	}
	policy, policyStatus := e.server.opportunityPolicies.Active()
	if policyStatus.Status == rpc.OpportunityPolicyStatusDrift || policyStatus.Status == rpc.OpportunityPolicyStatusError {
		snap := emptyOpportunitySnapshot(now)
		snap.Status = status
		snap.PolicyStatus = policyStatus
		snap.Trading = status.Trading
		snap.Blockers = append([]rpc.TradingBlocker(nil), policyStatus.Blockers...)
		if err := e.installSnapshot(snap, show); err != nil {
			return e.Snapshot(false), err
		}
		if err := e.appendEvent(opportunityEvent{At: now, Type: "policy-" + policyStatus.Status, PolicyID: policyStatus.PolicyID, PolicyVersion: policyStatus.PolicyVersion, PolicyFingerprint: policyStatus.Fingerprint, Message: policyStatus.Message}); err != nil {
			return snap, err
		}
		return snap, nil
	}
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) {
		snap := emptyOpportunitySnapshot(now)
		snap.Status = status
		snap.PolicyStatus = policyStatus
		snap.Trading = status.Trading
		snap.Blockers = []rpc.TradingBlocker{opportunityScopeUnscopedBlocker(scope)}
		if err := e.installSnapshot(snap, show); err != nil {
			return e.Snapshot(false), err
		}
		return snap, nil
	}
	acct, err := e.server.handleAccountSummary(ctx)
	if err != nil {
		blockers := []rpc.TradingBlocker{{Code: "account_unavailable", Message: err.Error()}}
		if snap, ok := e.preserveSnapshotOnRefreshFailure(scope, status, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyOpportunitySnapshot(now)
		snap.Status = status
		snap.PolicyStatus = policyStatus
		snap.Trading = status.Trading
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
		if snap, ok := e.preserveSnapshotOnRefreshFailure(scope, status, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyOpportunitySnapshot(now)
		snap.Status = status
		snap.PolicyStatus = policyStatus
		snap.Trading = status.Trading
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
		if snap, ok := e.preserveSnapshotOnRefreshFailure(scope, status, policyStatus, blockers, show); ok {
			return snap, nil
		}
		snap := emptyOpportunitySnapshot(now)
		snap.Status = status
		snap.PolicyStatus = policyStatus
		snap.Trading = status.Trading
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
	sources := rpc.OpportunitySourceFingerprints{Account: &accountFP, Positions: &positionsFP}
	opportunities := e.generate(policy, policyStatus, pos, sources, scope, now)
	for i := range opportunities {
		opportunities[i].PortfolioGeneration = portfolioHealth.ProjectionGeneration
		opportunities[i].PortfolioAccount = portfolioHealth.Account
		applyExerciseFundingPreflight(&opportunities[i], acct)
	}
	slices.SortStableFunc(opportunities, func(a, b rpc.Opportunity) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	revision := opportunityRevision(policyStatus.Fingerprint, sources, scope, opportunities)
	for i := range opportunities {
		opportunities[i].Rank = i + 1
		opportunities[i].Revision = revision
	}
	snap := rpc.OpportunitySnapshot{
		Kind:               rpc.OpportunitySnapshotKind,
		SchemaVersion:      rpc.OpportunitySnapshotSchemaVersion,
		AsOf:               now,
		Revision:           revision,
		AccountID:          scope.Account,
		AccountMode:        scope.Mode,
		PolicyID:           policy.PolicyID,
		PolicyVersion:      policy.PolicyVersion,
		PolicyFingerprint:  policyStatus.Fingerprint,
		PolicyStatus:       policyStatus,
		Status:             status,
		Trading:            status.Trading,
		SourceFingerprints: sources,
		Opportunities:      opportunities,
		Counts:             opportunityCounts(opportunities),
	}
	return e.installScoped(snap, scope, show)
}

func (e *opportunityEngine) installScoped(snap rpc.OpportunitySnapshot, scope brokerStateScope, show bool) (rpc.OpportunitySnapshot, error) {
	if current := e.currentScope(); !sameBrokerScope(current, scope) {
		shell := emptyOpportunitySnapshot(snap.AsOf)
		shell.Status = snap.Status
		shell.PolicyStatus = snap.PolicyStatus
		shell.Trading = snap.Trading
		shell.Blockers = opportunityScopeBlockers(scope.Account, scope.Mode, current)
		if err := e.installSnapshot(shell, show); err != nil {
			return e.Snapshot(false), err
		}
		return shell, nil
	}
	if err := e.installSnapshot(snap, show); err != nil {
		return e.Snapshot(false), err
	}
	return snap, nil
}

func (e *opportunityEngine) generate(policy opportunityPolicy, status rpc.OpportunityPolicyStatus, pos *rpc.PositionsResult, sources rpc.OpportunitySourceFingerprints, scope brokerStateScope, now time.Time) []rpc.Opportunity {
	if pos == nil || !policy.Buckets.OptionExercise.Enabled {
		return nil
	}
	stocks := map[string]rpc.PositionView{}
	for _, row := range pos.Stocks {
		if !positionCanAnchorUnderlyingGroup(row) {
			continue
		}
		stocks[strings.ToUpper(strings.TrimSpace(row.Symbol))] = row
	}
	coverage := opportunityCoverageByUnderlying(pos.ProtectionCoverage)
	var out []rpc.Opportunity
	for _, row := range pos.Options {
		symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
		opp, ok := optionExerciseOpportunity(policy, status, row, stocks[symbol], sources, now, coverage[symbol])
		if !ok {
			continue
		}
		if e.isIgnored(scope, opp.Key) {
			continue
		}
		out = append(out, opp)
	}
	return out
}

func opportunityCoverageByUnderlying(summary *rpc.ProtectionCoverageSummary) map[string]rpc.ProtectionCoverageRow {
	out := map[string]rpc.ProtectionCoverageRow{}
	if summary == nil {
		return out
	}
	for _, row := range summary.ByUnderlying {
		symbol := strings.ToUpper(strings.TrimSpace(row.Underlying))
		if symbol == "" {
			continue
		}
		out[symbol] = row
	}
	return out
}

func optionExerciseOpportunity(policy opportunityPolicy, status rpc.OpportunityPolicyStatus, row, stock rpc.PositionView, sources rpc.OpportunitySourceFingerprints, now time.Time, coverageRows ...rpc.ProtectionCoverageRow) (rpc.Opportunity, bool) {
	if !strings.EqualFold(positionWireSecType(row.SecType), "OPT") || row.Quantity <= 0 {
		return rpc.Opportunity{}, false
	}
	var coverage rpc.ProtectionCoverageRow
	if len(coverageRows) > 0 {
		coverage = coverageRows[0]
	}
	qty := int(math.Floor(row.Quantity))
	if qty <= 0 {
		return rpc.Opportunity{}, false
	}
	right := strings.ToUpper(strings.TrimSpace(row.Right))
	if right != "C" && right != "P" {
		return rpc.Opportunity{}, false
	}
	policyBucket := policy.Buckets.OptionExercise
	contract := proposalContractFromPosition(row, "OPT")
	underlying := opportunityUnderlyingContract(row, stock)
	multiplier := max(row.Multiplier, 1)
	shareChange := float64(qty * multiplier)
	if right == "P" {
		shareChange = -shareChange
	}
	before := stock.Quantity
	after := before + shareChange
	effect := classifyExercisePositionEffect(before, after)

	opp := rpc.Opportunity{
		Key:                      opportunityKey(rpc.OpportunityBucketOptionExercise, contract, rpc.OpportunityActionExercise),
		State:                    rpc.OpportunityStateGenerated,
		Bucket:                   rpc.OpportunityBucketOptionExercise,
		Symbol:                   contract.Symbol,
		SecType:                  "OPT",
		Action:                   rpc.OpportunityActionExercise,
		ExerciseAction:           rpc.ExerciseActionExercise,
		Quantity:                 qty,
		MaxQuantity:              qty,
		PositionQuantity:         row.Quantity,
		PositionEffect:           effect,
		UnderlyingQuantityBefore: before,
		UnderlyingQuantityAfter:  after,
		UnderlyingShareChange:    shareChange,
		Contract:                 contract,
		UnderlyingContract:       underlying,
		ExpectedGainCurrency:     nonEmptyString(row.Currency, stock.Currency),
		OptionBid:                row.OptionBid,
		UnderlyingBid:            stock.Bid,
		UnderlyingAsk:            stock.Ask,
		Reason:                   "option exercise candidate",
		PolicyID:                 policy.PolicyID,
		PolicyVersion:            policy.PolicyVersion,
		PolicyFingerprint:        status.Fingerprint,
		SourceFingerprints:       sources,
		CreatedAt:                now,
	}
	opp.PostExerciseRisk = opportunityPostExerciseRisk(opp, coverage)
	switch effect {
	case rpc.ExercisePositionEffectClose, rpc.ExercisePositionEffectReduce:
	default:
		addBlocker := func(code, message, action string) {
			opp.Blockers = appendTradingBlockerOnce(opp.Blockers, rpc.TradingBlocker{Code: code, Message: message, Action: action})
		}
		addBlocker("exercise_not_reduce_only", "option exercise would "+effect+" the underlying exposure; Canary only submits exercises that close or reduce an existing position", "Use an ordinary reviewed order if you deliberately want to open, increase, or flip exposure.")
	}
	if opp.ExpectedGainCurrency == "" {
		opp.ExpectedGainCurrency = "USD"
	}
	addBlocker := func(code, message, action string) {
		opp.Blockers = appendTradingBlockerOnce(opp.Blockers, rpc.TradingBlocker{Code: code, Message: message, Action: action})
	}
	if policyBucket.RequireRTH && !optionSessionOpen(now) {
		addBlocker("options_rth_required", "option exercise opportunities require regular U.S. options trading hours in this policy", "Refresh opportunities during regular U.S. options trading hours (09:30-16:15 ET on trading days).")
	}
	if policyBucket.RequireAmericanStyle && !opportunityLooksAmericanEquityOption(row, stock) {
		addBlocker("exercise_style_unknown_or_unsupported", "option exercise style is not confirmed as a U.S. equity or ETF style contract", "Use TWS Option Exercise manually for non-U.S., index, cash-settled, or unknown-style options.")
	}
	maxQuoteAge, _ := policyBucket.maxQuoteAgeDuration()
	if row.Stale || row.PriceAt.IsZero() || now.Sub(row.PriceAt) > maxQuoteAge {
		addBlocker("option_quote_stale", "option quote context is stale or unavailable", "Refresh opportunities after live option bid/ask data updates.")
	}
	if !stock.PriceAt.IsZero() && now.Sub(stock.PriceAt) > maxQuoteAge {
		addBlocker("underlying_quote_stale", "underlying quote context is stale", "Refresh opportunities after live underlying bid/ask data updates.")
	}

	underlyingPrice, underlyingOK := opportunityUnderlyingExercisePrice(right, row, stock)
	if !underlyingOK {
		return rpc.Opportunity{}, false
	}
	intrinsicPerShare := 0.0
	switch right {
	case "C":
		intrinsicPerShare = max(underlyingPrice-row.Strike, 0)
	case "P":
		intrinsicPerShare = max(row.Strike-underlyingPrice, 0)
	}
	if intrinsicPerShare <= 0 {
		return rpc.Opportunity{}, false
	}
	if row.OptionBid == nil || *row.OptionBid < 0 {
		return rpc.Opportunity{}, false
	}
	closePerShare := max(*row.OptionBid, 0)
	opp.IntrinsicValue = intrinsicPerShare * float64(qty) * float64(multiplier)
	opp.CloseValue = closePerShare * float64(qty) * float64(multiplier)
	opp.ExpectedGain = opp.IntrinsicValue - opp.CloseValue
	opp.Score = opp.ExpectedGain
	gainPct := opp.ExpectedGain / opp.IntrinsicValue * 100
	if opp.ExpectedGain < policyBucket.MinTotalGain || gainPct < policyBucket.MinGainPctIntrinsic {
		return rpc.Opportunity{}, false
	}
	opp.Details = append(opp.Details,
		fmt.Sprintf("intrinsic %.2f %s", opp.IntrinsicValue, opp.ExpectedGainCurrency),
		fmt.Sprintf("sell-at-bid value %.2f %s", opp.CloseValue, opp.ExpectedGainCurrency),
		fmt.Sprintf("expected gain %.2f %s", opp.ExpectedGain, opp.ExpectedGainCurrency),
	)
	if len(opp.Blockers) == 0 {
		opp.Reason = "exercise value exceeds executable option close value"
	} else {
		opp.Reason = "blocked exercise value exceeds executable option close value"
	}
	if len(opp.Blockers) > 0 {
		opp.State = rpc.OpportunityStateBlocked
	}
	return opp, true
}

// applyExerciseFundingPreflight adds current account funding evidence to call
// account-scoped preflight; IBKR remains authoritative for final acceptance.
func applyExerciseFundingPreflight(opp *rpc.Opportunity, account *rpc.AccountResult) {
	if opp == nil || !strings.EqualFold(opp.Contract.Right, "C") ||
		(opp.PositionEffect != rpc.ExercisePositionEffectClose && opp.PositionEffect != rpc.ExercisePositionEffectReduce) {
		return
	}
	add := func(code, message, action string) {
		opp.Blockers = appendTradingBlockerOnce(opp.Blockers, rpc.TradingBlocker{Code: code, Message: message, Action: action})
		opp.State = rpc.OpportunityStateBlocked
	}
	multiplier := max(opp.Contract.Multiplier, 1)
	required := opp.Contract.Strike * float64(opp.Quantity) * float64(multiplier)
	opp.RequiredCash = required
	opp.RequiredCashCurrency = nonEmptyString(opp.Contract.Currency, "USD")
	if account == nil || account.Authority == nil || account.Authority.Availability != rpc.AccountDataAvailable ||
		account.Authority.Freshness != rpc.AccountDataFreshnessCurrent || account.Authority.Fields == nil ||
		!account.Authority.Fields.AvailableFunds || !account.Authority.Fields.BaseCurrency {
		add("exercise_funding_preflight_unavailable", "fresh account funding evidence is unavailable for this call exercise", "Refresh the account and opportunity snapshot before previewing exercise.")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(account.BaseCurrency), strings.TrimSpace(opp.RequiredCashCurrency)) {
		add("exercise_funding_currency_unresolved", "exercise cash requirement is not in the account base currency", "Use TWS to review currency funding or wait for a contract-equivalent FX funding preflight.")
		return
	}
	if account.AvailableFunds+1e-9 < required {
		add("exercise_funding_insufficient", fmt.Sprintf("exercise requires about %.2f %s but current available funds are %.2f %s", required, opp.RequiredCashCurrency, account.AvailableFunds, account.BaseCurrency), "Reduce the exercise quantity or add settled funds, then refresh the preview.")
		return
	}
	opp.Details = append(opp.Details, fmt.Sprintf("funding preflight %.2f %s required; %.2f %s available", required, opp.RequiredCashCurrency, account.AvailableFunds, account.BaseCurrency))
}

func opportunityUnderlyingContract(row, stock rpc.PositionView) rpc.ContractParams {
	symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
	if stock.Symbol != "" {
		return proposalContractFromPosition(stock, positionWireSecType(stock.SecType))
	}
	return rpc.ContractParams{Symbol: symbol, SecType: "STK", Exchange: "SMART", Currency: nonEmptyString(row.Currency, "USD"), Multiplier: 1}
}

func opportunityUnderlyingExercisePrice(right string, row, stock rpc.PositionView) (float64, bool) {
	switch right {
	case "C":
		if stock.Bid != nil && *stock.Bid > 0 {
			return *stock.Bid, true
		}
	case "P":
		if stock.Ask != nil && *stock.Ask > 0 {
			return *stock.Ask, true
		}
	}
	if row.Underlying != nil && *row.Underlying > 0 {
		return *row.Underlying, false
	}
	return 0, false
}

func opportunityLooksAmericanEquityOption(row, stock rpc.PositionView) bool {
	if !strings.EqualFold(nonEmptyString(row.Currency, stock.Currency), "USD") {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(stock.SecType)) {
	case "STOCK", "STK", "ETF":
		return true
	default:
		return false
	}
}

func classifyExercisePositionEffect(before, after float64) string {
	const eps = 1e-9
	if math.Abs(before) <= eps {
		if math.Abs(after) <= eps {
			return rpc.ExercisePositionEffectUnknown
		}
		return rpc.ExercisePositionEffectOpen
	}
	if math.Abs(after) <= eps {
		return rpc.ExercisePositionEffectClose
	}
	if before > 0 && after > 0 || before < 0 && after < 0 {
		if math.Abs(after) < math.Abs(before) {
			return rpc.ExercisePositionEffectReduce
		}
		if math.Abs(after) > math.Abs(before) {
			return rpc.ExercisePositionEffectIncrease
		}
		return rpc.ExercisePositionEffectUnknown
	}
	return rpc.ExercisePositionEffectFlip
}

func opportunityPostExerciseRisk(opp rpc.Opportunity, coverage rpc.ProtectionCoverageRow) *rpc.OpportunityPostExerciseRisk {
	riskChange := exerciseRiskChange(opp.PositionEffect)
	ctx := &rpc.OpportunityPostExerciseRisk{
		Underlying:     strings.ToUpper(strings.TrimSpace(nonEmptyString(opp.UnderlyingContract.Symbol, opp.Symbol))),
		BeforeQuantity: opp.UnderlyingQuantityBefore,
		AfterQuantity:  opp.UnderlyingQuantityAfter,
		ShareChange:    opp.UnderlyingShareChange,
		PositionEffect: opp.PositionEffect,
		RiskChange:     riskChange,
		RiskOpened:     riskChange == rpc.ExerciseRiskChangeOpened,
		RiskIncreased:  riskChange == rpc.ExerciseRiskChangeIncreased,
		RiskFlipped:    riskChange == rpc.ExerciseRiskChangeFlipped,
	}
	if coverage.State != "" {
		ctx.ProtectionCoverageState = coverage.State
		ctx.CurrentProtectedQuantity = coverage.ProtectedQuantity
		ctx.CurrentUnprotectedQuantity = coverage.UnprotectedQuantity
		ctx.CurrentUnprotectedNotionalBase = coverage.UnprotectedNotionalBase
		ctx.UnprotectedNotionalBaseCurrency = coverage.UnprotectedNotionalBaseCurrency
	} else if math.Abs(opp.UnderlyingQuantityAfter) > 1e-9 {
		ctx.ProtectionCoverageState = rpc.ProtectionCoverageStateUnknown
	}
	review, reason, warnings := opportunityProtectionReview(opp, coverage)
	ctx.ProtectionReviewNeeded = review
	ctx.ProtectionReviewReason = reason
	ctx.WarningCodes = append(ctx.WarningCodes, warnings...)
	return ctx
}

func exerciseRiskChange(effect string) string {
	switch strings.ToLower(strings.TrimSpace(effect)) {
	case rpc.ExercisePositionEffectClose:
		return rpc.ExerciseRiskChangeClosed
	case rpc.ExercisePositionEffectReduce:
		return rpc.ExerciseRiskChangeReduced
	case rpc.ExercisePositionEffectOpen:
		return rpc.ExerciseRiskChangeOpened
	case rpc.ExercisePositionEffectIncrease:
		return rpc.ExerciseRiskChangeIncreased
	case rpc.ExercisePositionEffectFlip:
		return rpc.ExerciseRiskChangeFlipped
	default:
		return rpc.ExerciseRiskChangeUnknown
	}
}

func opportunityProtectionReview(opp rpc.Opportunity, coverage rpc.ProtectionCoverageRow) (bool, string, []string) {
	state := strings.ToLower(strings.TrimSpace(coverage.State))
	currentHasProtectiveOrder := coverage.ProtectedQuantity > 1e-9 || len(coverage.Orders) > 0
	switch state {
	case rpc.ProtectionCoverageStateOrphanedOrder, rpc.ProtectionCoverageStateReconcileRequired:
		return true, "current protective order already needs reconciliation before relying on post-exercise coverage", []string{"stale_protective_order"}
	}
	if math.Abs(opp.UnderlyingQuantityAfter) <= 1e-9 {
		if currentHasProtectiveOrder {
			return true, "exercise would flatten the underlying; reconcile or cancel remaining protective stops after exercise", []string{"exercise_flattens_protected_underlying"}
		}
		return false, "", nil
	}
	switch exerciseRiskChange(opp.PositionEffect) {
	case rpc.ExerciseRiskChangeOpened, rpc.ExerciseRiskChangeIncreased, rpc.ExerciseRiskChangeFlipped:
		return true, "exercise opens, increases, or flips underlying exposure; review protective stops after exercise", []string{"exercise_increases_underlying_risk"}
	case rpc.ExerciseRiskChangeReduced:
		return true, "exercise reduces but leaves underlying exposure; reconcile protective stop quantity after exercise", []string{"exercise_changes_remaining_exposure"}
	case rpc.ExerciseRiskChangeUnknown:
		return true, "post-exercise exposure effect is unknown; review protection before relying on the new position", []string{"exercise_effect_unknown"}
	}
	switch state {
	case "", rpc.ProtectionCoverageStateUnknown:
		return true, "post-exercise protection coverage cannot be confirmed from the current snapshot", []string{"protection_coverage_unavailable"}
	case rpc.ProtectionCoverageStateCovered:
		return false, "", nil
	default:
		return true, "remaining underlying exposure is not fully covered by current protective stops", []string{"remaining_exposure_not_fully_covered"}
	}
}

func (e *opportunityEngine) Preview(ctx context.Context, p rpc.OpportunityExercisePreviewParams) (rpc.OpportunityExercisePreviewResult, error) {
	now := e.clock()
	opp, blockers, err := e.revalidatedOpportunity(ctx, p.Key, p.Revision)
	if err != nil {
		return rpc.OpportunityExercisePreviewResult{Opportunity: opp, Blockers: blockers, AsOf: now}, err
	}
	return e.previewRevalidatedOpportunity(p, opp, blockers, now), nil
}

func (e *opportunityEngine) previewRevalidatedOpportunity(p rpc.OpportunityExercisePreviewParams, opp rpc.Opportunity, blockers []rpc.TradingBlocker, now time.Time) rpc.OpportunityExercisePreviewResult {
	qty := p.Quantity
	if qty <= 0 {
		qty = opp.Quantity
	}
	if qty <= 0 || qty != opp.Quantity {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{Code: "invalid_quantity", Message: "exercise quantity must match the freshly preflighted opportunity quantity"})
	}
	if opp.UnderlyingContract.ConID <= 0 {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{Code: "exercise_underlying_identity_unavailable", Message: "exercise requires an exact broker identity for the underlying position"})
	}
	if opp.PortfolioGeneration == 0 || strings.TrimSpace(opp.PortfolioAccount) == "" {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{Code: "exercise_portfolio_authority_unavailable", Message: "exercise requires a current account-scoped portfolio generation"})
	}
	auth := e.exerciseAuthorization(p.Origin)
	if !auth.Allowed {
		blockers = mergeTradingBlockers(blockers, auth.Blockers)
	}
	submitEligible := len(blockers) == 0
	res := rpc.OpportunityExercisePreviewResult{
		Accepted:       submitEligible,
		Opportunity:    opp,
		SubmitEligible: submitEligible,
		TokenMinted:    submitEligible,
		Blockers:       blockers,
		AsOf:           now,
	}
	if submitEligible {
		if e.server == nil || e.server.orderTokens == nil {
			res.Accepted, res.SubmitEligible = false, false
			res.TokenMinted = false
			res.Blockers = appendTradingBlockerOnce(res.Blockers, rpc.TradingBlocker{Code: "exercise_preview_signer_unavailable", Message: "exercise preview signer is unavailable"})
			return res
		}
		draft := exerciseOrderDraft(opp, qty, now)
		token, tokenID, expiresAt, err := e.server.orderTokens.mint(orderPreviewTokenPayload{
			Scope:               rpc.OrderTokenScopeExercise,
			Mode:                auth.Status.Mode,
			Account:             auth.Status.Account,
			Endpoint:            auth.Status.Endpoint,
			ClientID:            auth.Status.ClientID,
			Draft:               draft,
			Position:            rpc.OrderPositionImpact{Before: opp.UnderlyingQuantityBefore, After: opp.UnderlyingQuantityAfter, Effect: opp.PositionEffect},
			PortfolioGeneration: opp.PortfolioGeneration,
			PortfolioAccount:    opp.PortfolioAccount,
			Notional:            opp.RequiredCash,
			ExerciseKey:         opp.Key,
			ExerciseRevision:    opp.Revision,
			ExerciseUnderlying:  opp.UnderlyingContract,
			ExpiresAt:           now.Add(5 * time.Minute),
		})
		if err != nil {
			res.Accepted, res.SubmitEligible = false, false
			res.TokenMinted = false
			res.Blockers = appendTradingBlockerOnce(res.Blockers, rpc.TradingBlocker{Code: "exercise_preview_token_failed", Message: err.Error()})
			return res
		}
		res.PreviewToken, res.PreviewTokenID = token, tokenID
		res.PreviewTokenExpiresAt = expiresAt
		res.TokenMinted = true
	}
	return res
}

func (e *opportunityEngine) Submit(ctx context.Context, p rpc.OpportunityExerciseSubmitParams) (rpc.OpportunityExerciseSubmitResult, error) {
	now := e.clock()
	opp, blockers, err := e.revalidatedOpportunity(ctx, p.Key, p.Revision)
	preview := rpc.OpportunityExercisePreviewResult{Opportunity: opp, Blockers: blockers, AsOf: now}
	if err != nil {
		return rpc.OpportunityExerciseSubmitResult{Preview: &preview, Opportunity: opp, Blockers: blockers, AsOf: now}, err
	}
	origin := normalizedWriteOrigin(p.Origin)
	qty := p.Quantity
	if qty <= 0 {
		qty = opp.Quantity
	}
	if qty <= 0 || qty != opp.Quantity {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{Code: "invalid_quantity", Message: "exercise quantity must match the freshly preflighted opportunity quantity"})
	}
	payload, tokenErr := e.verifyExercisePreviewToken(p.PreviewToken, opp, qty)
	if tokenErr != nil {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{Code: "exercise_preview_token_invalid", Message: tokenErr.Error(), Action: "Run a fresh exercise preview, then confirm that exact token."})
	}
	preview.PreviewTokenID = payload.TokenID
	preview.TokenMinted = payload.TokenID != ""
	preview.SubmitEligible = len(blockers) == 0
	preview.Accepted = preview.SubmitEligible
	preview.Blockers = blockers
	if !preview.SubmitEligible {
		e.appendEvent(opportunityEvent{At: now, Type: "submit-blocked", Key: opp.Key, Revision: opp.Revision, PreviewTokenID: payload.TokenID, AccountID: e.currentScope().Account, PolicyID: opp.PolicyID, PolicyVersion: opp.PolicyVersion, PolicyFingerprint: opp.PolicyFingerprint, Message: firstTradingBlockerMessage(blockers), SourceFingerprints: opp.SourceFingerprints})
		return rpc.OpportunityExerciseSubmitResult{Preview: &preview, Opportunity: opp, PreviewTokenID: payload.TokenID, Blockers: blockers, Message: "exercise submit blocked", AsOf: now}, nil
	}
	orderRef := payload.Draft.OrderRef
	if err := e.server.submitOptionExercise(ctx, payload, opp, qty, origin); err != nil {
		blockers := []rpc.TradingBlocker{{Code: "exercise_submit_failed", Message: err.Error(), Action: "Reconcile in TWS before trying again."}}
		e.appendEvent(opportunityEvent{At: now, Type: "submit-error", Key: opp.Key, Revision: opp.Revision, PreviewTokenID: payload.TokenID, OrderRef: orderRef, AccountID: e.currentScope().Account, PolicyID: opp.PolicyID, PolicyVersion: opp.PolicyVersion, PolicyFingerprint: opp.PolicyFingerprint, Message: err.Error(), SourceFingerprints: opp.SourceFingerprints})
		return rpc.OpportunityExerciseSubmitResult{Preview: &preview, Opportunity: opp, PreviewTokenID: payload.TokenID, OrderRef: orderRef, Blockers: blockers, Message: "exercise submit failed; broker receipt may be uncertain, reconcile before retrying", AsOf: now}, nil
	}
	e.appendEvent(opportunityEvent{At: now, Type: "submitted", Key: opp.Key, Revision: opp.Revision, PreviewTokenID: payload.TokenID, OrderRef: orderRef, AccountID: e.currentScope().Account, PolicyID: opp.PolicyID, PolicyVersion: opp.PolicyVersion, PolicyFingerprint: opp.PolicyFingerprint, Message: "option exercise instruction sent; reconcile status in TWS", SourceFingerprints: opp.SourceFingerprints})
	return rpc.OpportunityExerciseSubmitResult{Accepted: true, Preview: &preview, Opportunity: opp, PreviewTokenID: payload.TokenID, OrderRef: orderRef, Message: "option exercise instruction sent; verify broker status in TWS", AsOf: now}, nil
}

func exerciseOrderDraft(opp rpc.Opportunity, qty int, now time.Time) rpc.OrderDraft {
	return rpc.OrderDraft{
		Action:    rpc.OpportunityActionExercise,
		Contract:  opp.Contract,
		Quantity:  qty,
		OrderRef:  "option-exercise-" + shortStableHash(opp.Key+"|"+opp.Revision+"|"+strconv.Itoa(qty)+"|"+now.UTC().Format(time.RFC3339Nano)),
		OpenClose: opp.PositionEffect,
		Source:    "option_exercise",
	}
}

func (e *opportunityEngine) verifyExercisePreviewToken(token string, opp rpc.Opportunity, qty int) (orderPreviewTokenPayload, error) {
	if e == nil || e.server == nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("exercise authority is unavailable")
	}
	payload, err := e.server.verifyPreviewTokenForCurrentGate(token)
	if err != nil {
		return orderPreviewTokenPayload{}, err
	}
	if payload.Scope != rpc.OrderTokenScopeExercise {
		return orderPreviewTokenPayload{}, fmt.Errorf("preview token scope %q cannot authorize option exercise", payload.Scope)
	}
	if payload.ExerciseKey != opp.Key || payload.ExerciseRevision != opp.Revision || payload.Draft.Quantity != qty ||
		payload.Draft.Contract != opp.Contract || payload.ExerciseUnderlying != opp.UnderlyingContract ||
		payload.PortfolioGeneration == 0 || payload.PortfolioGeneration != opp.PortfolioGeneration ||
		!strings.EqualFold(strings.TrimSpace(payload.PortfolioAccount), strings.TrimSpace(opp.PortfolioAccount)) ||
		payload.Position.Effect != opp.PositionEffect ||
		payload.Position.Before != opp.UnderlyingQuantityBefore || payload.Position.After != opp.UnderlyingQuantityAfter {
		return orderPreviewTokenPayload{}, fmt.Errorf("exercise facts changed after preview; preview again")
	}
	if opp.PositionEffect != rpc.ExercisePositionEffectClose && opp.PositionEffect != rpc.ExercisePositionEffectReduce {
		return orderPreviewTokenPayload{}, fmt.Errorf("exercise is not close/reduce-only")
	}
	return payload, nil
}

func (e *opportunityEngine) Ignore(p rpc.OpportunityIgnoreParams) rpc.OpportunityIgnoreResult {
	now := e.clock()
	key := strings.TrimSpace(p.Key)
	if key == "" {
		return rpc.OpportunityIgnoreResult{Accepted: false, Message: "opportunity key is required", AsOf: now}
	}
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) {
		return rpc.OpportunityIgnoreResult{Accepted: false, Key: key, Revision: strings.TrimSpace(p.Revision), Message: "opportunity ignore requires a concrete account and paper/live mode", AsOf: now}
	}
	ev := opportunityEvent{At: now, Type: "ignored", Key: key, Revision: strings.TrimSpace(p.Revision), Reason: strings.TrimSpace(p.Reason), Message: "opportunity ignored"}
	ev.AccountID = scope.Account
	ev.AccountMode = scope.Mode
	if err := e.appendEvent(ev); err != nil {
		return rpc.OpportunityIgnoreResult{Accepted: false, Key: key, Revision: strings.TrimSpace(p.Revision), Message: "opportunity ignore was not persisted", AsOf: now}
	}
	e.mu.Lock()
	if e.ignored == nil {
		e.ignored = map[string]struct{}{}
	}
	e.ignored[opportunityIgnoreKey(scope, key)] = struct{}{}
	e.mu.Unlock()
	return rpc.OpportunityIgnoreResult{Accepted: true, Key: key, Revision: strings.TrimSpace(p.Revision), Message: "opportunity ignored", AsOf: now}
}

func (e *opportunityEngine) revalidatedOpportunity(ctx context.Context, key, revision string) (rpc.Opportunity, []rpc.TradingBlocker, error) {
	key, revision = strings.TrimSpace(key), strings.TrimSpace(revision)
	if key == "" || revision == "" {
		return rpc.Opportunity{}, []rpc.TradingBlocker{{Code: "bad_request", Message: "opportunity key and revision are required"}}, nil
	}
	snap, err := e.Refresh(ctx, false)
	if err != nil && len(snap.Opportunities) == 0 {
		return rpc.Opportunity{}, snap.Blockers, err
	}
	if len(snap.Blockers) > 0 && len(snap.Opportunities) == 0 {
		return rpc.Opportunity{}, snap.Blockers, nil
	}
	if snap.PolicyStatus.Status == rpc.OpportunityPolicyStatusDrift || snap.PolicyStatus.Status == rpc.OpportunityPolicyStatusError {
		return rpc.Opportunity{}, snap.PolicyStatus.Blockers, nil
	}
	if len(snap.Status.Blockers) > 0 {
		return rpc.Opportunity{}, snap.Status.Blockers, nil
	}
	if snap.Revision != revision {
		return rpc.Opportunity{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "opportunity revision is stale; refresh opportunities before preview or exercise"}}, nil
	}
	for _, opp := range snap.Opportunities {
		if opp.Key == key {
			if len(snap.Blockers) > 0 {
				return opp, mergeTradingBlockers(snap.Blockers, opp.Blockers), nil
			}
			return opp, opp.Blockers, nil
		}
	}
	return rpc.Opportunity{}, []rpc.TradingBlocker{{Code: "opportunity_not_found", Message: "opportunity key is not present in the current snapshot"}}, nil
}

func (e *opportunityEngine) exerciseAuthorization(origin string) brokerWriteAuthorization {
	return e.server.brokerWriteAuthorizationForRequest(origin)
}

func (e *opportunityEngine) installSnapshot(snap rpc.OpportunitySnapshot, show bool) error {
	if opportunitySnapshotPersistable(snap) {
		if e.store == nil {
			return errors.New("opportunity store is not attached")
		}
		if err := e.store.SaveCurrent(snap); err != nil {
			return fmt.Errorf("persist opportunity snapshot: %w", err)
		}
	}
	e.replaceSnapshot(snap)
	if show {
		e.appendShownEvents(snap)
	}
	return nil
}

// preserveSnapshotOnRefreshFailure keeps serving the last-good snapshot
// through a transient fetch failure instead of clobbering it with an empty
// re-adopted from disk because the startup kick races the gateway connect.
func (e *opportunityEngine) preserveSnapshotOnRefreshFailure(scope brokerStateScope, status rpc.OpportunityStatus, policyStatus rpc.OpportunityPolicyStatus, blockers []rpc.TradingBlocker, show bool) (rpc.OpportunitySnapshot, bool) {
	e.mu.Lock()
	snap := cloneOpportunitySnapshot(e.snapshot)
	e.mu.Unlock()
	if !opportunitySnapshotUsable(snap) || !sameOpportunityPolicy(snap, policyStatus) {
		return rpc.OpportunitySnapshot{}, false
	}
	// Preserving last-good opportunities through a transient fetch failure
	// is only safe when they were generated for the same session: a paper
	if !sameBrokerScope(brokerStateScope{Account: snap.AccountID, Mode: snap.AccountMode}, scope) {
		if e.server != nil {
			e.server.warnf("opportunities: dropping preserved snapshot on refresh failure: snapshot scope %q/%q does not match connected session %q/%q", snap.AccountID, snap.AccountMode, scope.Account, scope.Mode)
		}
		return rpc.OpportunitySnapshot{}, false
	}
	snap.Status = status
	snap.PolicyStatus = policyStatus
	snap.Trading = status.Trading
	merged := append([]rpc.TradingBlocker(nil), blockers...)
	for _, blocker := range snap.Blockers {
		merged = appendTradingBlockerOnce(merged, blocker)
	}
	snap.Blockers = merged
	e.installPreservedSnapshot(snap, show)
	return snap, true
}

// opportunitySnapshotUsable reports whether snap carries generated
func opportunitySnapshotUsable(snap rpc.OpportunitySnapshot) bool {
	return snap.Kind == rpc.OpportunitySnapshotKind && snap.Revision != "" && snap.Revision != "empty" && len(snap.Opportunities) > 0
}

func sameOpportunityPolicy(snap rpc.OpportunitySnapshot, status rpc.OpportunityPolicyStatus) bool {
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

// installPreservedSnapshot swaps the served snapshot without persisting:
// the preserved copy carries transient blockers that must not survive a
// restart, and the on-disk last-good copy is exactly what preservation is
func (e *opportunityEngine) installPreservedSnapshot(snap rpc.OpportunitySnapshot, show bool) {
	e.replaceSnapshot(snap)
	if show {
		e.appendShownEvents(snap)
	}
}

func (e *opportunityEngine) replaceSnapshot(snap rpc.OpportunitySnapshot) {
	e.mu.Lock()
	e.snapshot = cloneOpportunitySnapshot(snap)
	e.mu.Unlock()
}

func opportunitySnapshotPersistable(snap rpc.OpportunitySnapshot) bool {
	return snap.Kind == rpc.OpportunitySnapshotKind && snap.Revision != "" && snap.Revision != "empty" && brokerScopeConcrete(brokerStateScope{Account: snap.AccountID, Mode: snap.AccountMode})
}

func (e *opportunityEngine) appendShownEvents(snap rpc.OpportunitySnapshot) {
	for _, opp := range snap.Opportunities {
		e.appendEvent(opportunityEvent{At: e.clock(), Type: "shown", Key: opp.Key, Revision: opp.Revision, AccountID: snap.AccountID, PolicyID: opp.PolicyID, PolicyVersion: opp.PolicyVersion, PolicyFingerprint: opp.PolicyFingerprint, SourceFingerprints: opp.SourceFingerprints})
	}
}

func (e *opportunityEngine) appendEvent(ev opportunityEvent) error {
	if e == nil || e.store == nil {
		return errors.New("opportunity store is not attached")
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
			e.server.warnf("opportunities: append event: %v", err)
		}
		return err
	}
	return nil
}

func (e *opportunityEngine) isIgnored(scope brokerStateScope, key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.ignored[opportunityIgnoreKey(scope, key)]
	return ok
}

func (e *opportunityEngine) currentScope() brokerStateScope {
	if e == nil || e.server == nil {
		return brokerStateScope{}
	}
	return e.server.currentBrokerStateScope()
}

func (e *opportunityEngine) clock() time.Time {
	if e.now != nil {
		return e.now().UTC()
	}
	return time.Now().UTC()
}

func emptyOpportunitySnapshot(now time.Time) rpc.OpportunitySnapshot {
	return rpc.OpportunitySnapshot{Kind: rpc.OpportunitySnapshotKind, SchemaVersion: rpc.OpportunitySnapshotSchemaVersion, AsOf: now, Revision: "empty", Opportunities: []rpc.Opportunity{}}
}

func opportunityCounts(rows []rpc.Opportunity) rpc.OpportunityCounts {
	var out rpc.OpportunityCounts
	out.Total = len(rows)
	for _, row := range rows {
		if len(row.Blockers) == 0 {
			out.Actionable++
			out.ExpectedGain += row.ExpectedGain
			out.ExpectedGainCurrency = mergedCurrency(out.ExpectedGainCurrency, row.ExpectedGainCurrency)
		} else {
			out.Blocked++
		}
		if row.Bucket == rpc.OpportunityBucketOptionExercise {
			out.OptionExercise++
		}
	}
	if out.ExpectedGainCurrency == "MIX" {
		out.ExpectedGain = 0
		out.ExpectedGainCurrency = ""
	}
	return out
}

func opportunityRevision(policy rpc.Fingerprint, sources rpc.OpportunitySourceFingerprints, scope brokerStateScope, rows []rpc.Opportunity) string {
	projection := struct {
		Policy      rpc.Fingerprint                   `json:"policy"`
		Account     string                            `json:"account"`
		Mode        string                            `json:"mode"`
		Sources     rpc.OpportunitySourceFingerprints `json:"sources"`
		Opportunity []string                          `json:"opportunity"`
	}{Policy: policy, Account: strings.ToUpper(strings.TrimSpace(scope.Account)), Mode: strings.ToLower(strings.TrimSpace(scope.Mode)), Sources: sources}
	for _, row := range rows {
		risk := ""
		if row.PostExerciseRisk != nil {
			risk = ":" + row.PostExerciseRisk.RiskChange +
				":" + row.PostExerciseRisk.ProtectionCoverageState +
				":" + strconv.FormatBool(row.PostExerciseRisk.ProtectionReviewNeeded) +
				":" + fmt.Sprintf("%.4f", row.PostExerciseRisk.CurrentProtectedQuantity) +
				":" + fmt.Sprintf("%.4f", row.PostExerciseRisk.CurrentUnprotectedQuantity)
		}
		projection.Opportunity = append(projection.Opportunity, row.Key+":"+strconv.Itoa(row.Quantity)+":"+row.PositionEffect+":"+fmt.Sprintf("%.2f", row.ExpectedGain)+risk)
	}
	raw, _ := json.Marshal(projection)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func opportunityKey(bucket string, contract rpc.ContractParams, action string) string {
	raw := strings.Join([]string{bucket, strings.ToUpper(contract.Symbol), strings.ToUpper(contract.SecType), strings.ToUpper(contract.LocalSymbol), contract.Expiry, strings.ToUpper(contract.Right), fmt.Sprintf("%.4f", contract.Strike), strings.ToUpper(action)}, "|")
	sum := sha256.Sum256([]byte(raw))
	return bucket + ":" + hex.EncodeToString(sum[:8])
}

func opportunityIgnoreKey(scope brokerStateScope, key string) string {
	return strings.ToUpper(strings.TrimSpace(scope.Account)) + "|" + strings.ToLower(strings.TrimSpace(scope.Mode)) + "|" + strings.TrimSpace(key)
}

func opportunityScopeBlockers(snapAccount, snapMode string, scope brokerStateScope) []rpc.TradingBlocker {
	if !brokerScopeConcrete(scope) {
		return []rpc.TradingBlocker{opportunityScopeUnscopedBlocker(scope)}
	}
	if !sameBrokerScope(brokerStateScope{Account: snapAccount, Mode: snapMode}, scope) {
		return []rpc.TradingBlocker{{
			Code:    "opportunity_scope_mismatch",
			Message: fmt.Sprintf("opportunity snapshot was generated for account %q mode %q but the connected session is account %q mode %q", snapAccount, snapMode, scope.Account, scope.Mode),
			Action:  "Refresh opportunities to regenerate them for the connected session.",
		}}
	}
	return nil
}

func opportunityScopeUnscopedBlocker(scope brokerStateScope) rpc.TradingBlocker {
	return rpc.TradingBlocker{
		Code:    "opportunity_scope_unavailable",
		Message: fmt.Sprintf("connected session has no concrete single-account identity (observed account %q mode %q); opportunities are scoped per account and paper/live mode", scope.Account, scope.Mode),
		Action:  "Reconnect TWS/Gateway with a single concrete account, then refresh opportunities.",
	}
}

func cloneOpportunitySnapshot(in rpc.OpportunitySnapshot) rpc.OpportunitySnapshot {
	out := in
	out.Opportunities = append([]rpc.Opportunity(nil), in.Opportunities...)
	out.Blockers = append([]rpc.TradingBlocker(nil), in.Blockers...)
	for i := range out.Opportunities {
		out.Opportunities[i].Details = append([]string(nil), in.Opportunities[i].Details...)
		out.Opportunities[i].Blockers = append([]rpc.TradingBlocker(nil), in.Opportunities[i].Blockers...)
	}
	return out
}

func shortStableHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

func (s *Server) handleOpportunitiesStatus() *rpc.OpportunityStatus {
	st := s.opportunityStatus()
	return &st
}

func (s *Server) handleOpportunitiesSnapshot(req *rpc.Request) *rpc.OpportunitySnapshot {
	var p rpc.OpportunitySnapshotParams
	_ = decodeParams(req.Params, &p)
	if s.opportunities == nil {
		snap := emptyOpportunitySnapshot(s.orderNow())
		return &snap
	}
	snap := s.opportunities.Snapshot(p.Show)
	return &snap
}

func (s *Server) handleOpportunitiesRefresh(ctx context.Context, req *rpc.Request) (*rpc.OpportunitySnapshot, error) {
	var p rpc.OpportunityRefreshParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.opportunities == nil {
		snap := emptyOpportunitySnapshot(s.orderNow())
		return &snap, nil
	}
	snap, err := s.opportunities.Refresh(ctx, p.Show)
	return &snap, err
}

func (s *Server) handleOpportunitiesPreviewExercise(ctx context.Context, req *rpc.Request) (*rpc.OpportunityExercisePreviewResult, error) {
	var p rpc.OpportunityExercisePreviewParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.opportunities == nil {
		return &rpc.OpportunityExercisePreviewResult{Accepted: false, AsOf: s.orderNow(), Blockers: []rpc.TradingBlocker{{Code: "opportunity_engine_unavailable", Message: "opportunity engine is unavailable"}}}, nil
	}
	res, err := s.opportunities.Preview(ctx, p)
	return &res, err
}

func (s *Server) handleOpportunitiesSubmitExercise(ctx context.Context, req *rpc.Request) (*rpc.OpportunityExerciseSubmitResult, error) {
	var p rpc.OpportunityExerciseSubmitParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.opportunities == nil {
		return &rpc.OpportunityExerciseSubmitResult{Accepted: false, AsOf: s.orderNow(), Blockers: []rpc.TradingBlocker{{Code: "opportunity_engine_unavailable", Message: "opportunity engine is unavailable"}}}, nil
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	res, err := s.opportunities.Submit(ctx, p)
	return &res, err
}

func (s *Server) handleOpportunitiesIgnore(req *rpc.Request) *rpc.OpportunityIgnoreResult {
	var p rpc.OpportunityIgnoreParams
	_ = decodeParams(req.Params, &p)
	if s.opportunities == nil {
		return &rpc.OpportunityIgnoreResult{Accepted: false, Key: p.Key, Revision: p.Revision, Message: "opportunity engine is unavailable", AsOf: s.orderNow()}
	}
	res := s.opportunities.Ignore(p)
	return &res
}

func (s *Server) opportunityStatus() rpc.OpportunityStatus {
	now := s.orderNow()
	cfg := s.cfg.Opportunities.WithDefaults()
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	trading := s.tradingStatus(ep)
	policy := rpc.OpportunityPolicyStatus{Status: rpc.OpportunityPolicyStatusDisabled}
	if s.opportunityPolicies != nil {
		policy = s.opportunityPolicies.Status()
	}
	out := rpc.OpportunityStatus{
		Kind:           rpc.OpportunityStatusKind,
		AsOf:           now,
		Enabled:        cfg.EnabledResolved(),
		HotReload:      cfg.HotReloadEnabled(),
		ReloadInterval: cfg.ReloadIntervalDuration().String(),
		RefreshCadence: cfg.RefreshCadenceDuration().String(),
		Policy:         policy,
		Trading:        trading,
	}
	if !out.Enabled {
		out.Blockers = append(out.Blockers, rpc.TradingBlocker{Code: "opportunities_disabled", Message: "opportunities are disabled by config"})
	}
	if policy.Status == rpc.OpportunityPolicyStatusDrift || policy.Status == rpc.OpportunityPolicyStatusError {
		out.Blockers = append(out.Blockers, policy.Blockers...)
	}
	out.Blocked = len(out.Blockers) > 0
	return out
}
