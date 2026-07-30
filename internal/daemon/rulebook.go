package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Trading rulebook assembly (internal-docs/design/trading-rulebook.md). The pure
// evaluator lives in internal/risk; this file maps daemon state into
// risk.RuleInputs, owns the cached evaluation preview causes read, and the
// rules-decisions journal. Advisory-only end to end: nothing here touches
// submit eligibility or broker-write authorization.

const (
	// rulesPreviewTTL spans the established one-minute Stress cadence with a
	// small scheduling margin. All cache reuse remains scope- and connector-
	// generation-bound; after this budget a preview gets a bounded canonical
	// evaluation or an explicit unavailable advisory.
	rulesPreviewTTL = 75 * time.Second
	// rulebookDaemonRefreshEvery moves the app's established complete Rulebook
	// workload into the daemon so alerts remain operational with the app shut.
	rulebookDaemonRefreshEvery = time.Minute
	rulebookRefreshRetryEvery  = 5 * time.Second
	// rulesSPYQuoteTimeout bounds the one best-effort SPY snapshot quote
	// per evaluation during regular hours (rules 9/10 tape context).
	rulesSPYQuoteTimeout = 2500 * time.Millisecond
)

type rulebookCacheBinding struct {
	scope          brokerStateScope
	connector      *ibkrlib.Connector
	connectorEpoch uint64
	broker         ibkrlib.BrokerEvidenceBinding
	brokerCaptured bool
}

func (s *Server) currentRulebookBinding() rulebookCacheBinding {
	if s == nil {
		return rulebookCacheBinding{}
	}
	scope := s.currentBrokerStateScope()
	s.mu.Lock()
	connector, epoch := s.connector, s.connectorEpoch
	s.mu.Unlock()
	binding := rulebookCacheBinding{scope: scope, connector: connector, connectorEpoch: epoch}
	if connector != nil {
		binding.broker, binding.brokerCaptured = connector.CaptureBrokerEvidence()
	}
	return binding
}

func sameRulebookBinding(a, b rulebookCacheBinding) bool {
	if !sameBrokerScope(a.scope, b.scope) || a.connector != b.connector || a.connectorEpoch != b.connectorEpoch {
		return false
	}
	if a.connector == nil {
		return true
	}
	return a.brokerCaptured && b.brokerCaptured && a.broker == b.broker
}

func (s *Server) cachedRulebookResult(binding rulebookCacheBinding, maxAge time.Duration, now time.Time) (*rpc.RulesResult, bool) {
	if s == nil || maxAge <= 0 || !brokerScopeConcrete(binding.scope) {
		return nil, false
	}
	s.rulesMu.Lock()
	cached, at := s.lastRules, s.lastRulesAt
	cachedBinding := rulebookCacheBinding{
		scope: s.lastRulesScope, connector: s.lastRulesConnector, connectorEpoch: s.lastRulesConnectorEpoch,
		broker: s.lastRulesBroker, brokerCaptured: s.lastRulesBrokerCaptured,
	}
	s.rulesMu.Unlock()
	now = now.UTC()
	if cached == nil || at.IsZero() || at.After(now) || now.Sub(at.UTC()) > maxAge || !sameRulebookBinding(binding, cachedBinding) {
		return nil, false
	}
	return cloneRulesResult(cached), true
}

func (s *Server) cacheRulebookResult(result *rpc.RulesResult, binding rulebookCacheBinding, cachedAt time.Time) bool {
	if s == nil || result == nil || !brokerScopeConcrete(binding.scope) {
		return false
	}
	if binding.connector != nil && !sameRulebookBinding(binding, s.currentRulebookBinding()) {
		return false
	}
	return s.cacheRulebookResultStable(result, binding, cachedAt)
}

// cacheRulebookResultStable publishes after the caller has either validated
// an unbound/unavailable result or entered the exact Connector evidence
// barrier. It must not call currentRulebookBinding while that write barrier is
// held because doing so would recursively acquire its read side.
func (s *Server) cacheRulebookResultStable(result *rpc.RulesResult, binding rulebookCacheBinding, cachedAt time.Time) bool {
	copyResult := cloneRulesResult(result)
	if copyResult == nil {
		return false
	}
	s.rulesMu.Lock()
	s.lastRules = copyResult
	s.lastRulesAt = cachedAt.UTC()
	s.lastRulesScope = binding.scope
	s.lastRulesConnector = binding.connector
	s.lastRulesConnectorEpoch = binding.connectorEpoch
	s.lastRulesBroker = binding.broker
	s.lastRulesBrokerCaptured = binding.brokerCaptured
	wake := s.rulesRefreshWake
	s.rulesMu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	return true
}

func (s *Server) rulebookRefreshWakeChannel() <-chan struct{} {
	s.rulesMu.Lock()
	if s.rulesRefreshWake == nil {
		s.rulesRefreshWake = make(chan struct{}, 1)
	}
	wake := s.rulesRefreshWake
	s.rulesMu.Unlock()
	return wake
}

func (s *Server) rulebookNextRefreshDue(binding rulebookCacheBinding, now time.Time) time.Time {
	s.rulesMu.Lock()
	at := s.lastRulesAt
	cachedBinding := rulebookCacheBinding{
		scope: s.lastRulesScope, connector: s.lastRulesConnector, connectorEpoch: s.lastRulesConnectorEpoch,
		broker: s.lastRulesBroker, brokerCaptured: s.lastRulesBrokerCaptured,
	}
	current := s.lastRules != nil && !at.IsZero() && sameRulebookBinding(binding, cachedBinding)
	s.rulesMu.Unlock()
	if !current || at.After(now.UTC()) {
		return now.UTC()
	}
	return at.UTC().Add(rulebookDaemonRefreshEvery)
}

func (s *Server) publishCanonicalRulebookResult(ctx context.Context, result *rpc.RulesResult, binding rulebookCacheBinding) bool {
	if s == nil || result == nil || ctx == nil || ctx.Err() != nil || !sameRulebookBinding(binding, s.currentRulebookBinding()) {
		return false
	}
	commit := func(shadowResult *rpc.RulesResult) error {
		if err := s.commitRulebookAlertShadow(ctx, shadowResult, binding.scope); err != nil {
			return err
		}
		s.journalRuleTransitionsForBinding(result, binding)
		if !s.cacheRulebookResultStable(result, binding, s.orderNow().UTC()) {
			return fmt.Errorf("cache canonical rulebook result")
		}
		return nil
	}
	if binding.connector == nil {
		// A real unbound evaluation is necessarily degraded because neither
		// account nor portfolio authority is available. Preserve that useful
		// diagnostic and the test seam, but make the candidate copy explicitly
		// uncovered so an injected/current-looking result can never recover.
		shadowResult := cloneRulesResult(result)
		if shadowResult.Status == "ok" {
			shadowResult.Status = "degraded"
		}
		return commit(shadowResult) == nil
	}
	if !binding.brokerCaptured {
		return false
	}
	committed, err := s.withStableBrokerEvidence(daemonBrokerEvidenceBinding{
		scope: binding.scope, connector: binding.connector, connectorEpoch: binding.connectorEpoch, broker: binding.broker,
	}, func() error { return commit(result) })
	return committed && err == nil
}

func cloneRulesResult(in *rpc.RulesResult) *rpc.RulesResult {
	if in == nil {
		return nil
	}
	out := *in
	out.Rules = append([]risk.RuleRow(nil), in.Rules...)
	for i := range out.Rules {
		out.Rules[i].Offenders = append([]risk.RuleOffender(nil), in.Rules[i].Offenders...)
		out.Rules[i].Exempt = append([]risk.RuleOffender(nil), in.Rules[i].Exempt...)
		out.Rules[i].Notes = append([]string(nil), in.Rules[i].Notes...)
		if in.Rules[i].Observed != nil {
			value := *in.Rules[i].Observed
			out.Rules[i].Observed = &value
		}
		if in.Rules[i].Threshold != nil {
			value := *in.Rules[i].Threshold
			out.Rules[i].Threshold = &value
		}
	}
	out.Ranked = append([]int(nil), in.Ranked...)
	if in.BreachCounts != nil {
		out.BreachCounts = make(map[string]int, len(in.BreachCounts))
		maps.Copy(out.BreachCounts, in.BreachCounts)
	}
	out.InputHealth = append([]rpc.SourceHealth(nil), in.InputHealth...)
	for i := range out.InputHealth {
		out.InputHealth[i].Notes = append([]string(nil), in.InputHealth[i].Notes...)
		if in.InputHealth[i].Fingerprint != nil {
			value := *in.InputHealth[i].Fingerprint
			out.InputHealth[i].Fingerprint = &value
		}
		if in.InputHealth[i].NextAttempt != nil {
			value := *in.InputHealth[i].NextAttempt
			out.InputHealth[i].NextAttempt = &value
		}
		if in.InputHealth[i].LastFailure != nil {
			value := *in.InputHealth[i].LastFailure
			out.InputHealth[i].LastFailure = &value
		}
	}
	out.Earnings = append([]rpc.EarningsInfo(nil), in.Earnings...)
	for i := range out.Earnings {
		out.Earnings[i].Providers = append([]rpc.EarningsProviderInfo(nil), in.Earnings[i].Providers...)
		for j := range out.Earnings[i].Providers {
			provider := &out.Earnings[i].Providers[j]
			if in.Earnings[i].Providers[j].NextAttempt != nil {
				value := *in.Earnings[i].Providers[j].NextAttempt
				provider.NextAttempt = &value
			}
			if in.Earnings[i].Providers[j].LastFailure != nil {
				value := *in.Earnings[i].Providers[j].LastFailure
				provider.LastFailure = &value
			}
		}
		if in.Earnings[i].Terminal != nil {
			terminal := *in.Earnings[i].Terminal
			terminal.Evidence = append([]rpc.EarningsEvidenceReference(nil), in.Earnings[i].Terminal.Evidence...)
			out.Earnings[i].Terminal = &terminal
		}
		if in.Earnings[i].Identity != nil {
			identity := *in.Earnings[i].Identity
			if in.Earnings[i].Identity.NextAttempt != nil {
				value := *in.Earnings[i].Identity.NextAttempt
				identity.NextAttempt = &value
			}
			if in.Earnings[i].Identity.LastFailure != nil {
				value := *in.Earnings[i].Identity.LastFailure
				identity.LastFailure = &value
			}
			out.Earnings[i].Identity = &identity
		}
	}
	if in.PolicyFingerprint != nil {
		value := *in.PolicyFingerprint
		out.PolicyFingerprint = &value
	}
	return &out
}

func (s *Server) rulebookUnavailableResult(reason string) *rpc.RulesResult {
	now := time.Now().UTC()
	if s != nil {
		now = s.orderNow().UTC()
	}
	pol := risk.DefaultRulebookPolicy()
	fingerprint := rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: pol.FingerprintKey()}
	if strings.TrimSpace(reason) == "" {
		reason = "canonical_cache_unavailable"
	}
	health := make([]rpc.SourceHealth, 0, 5)
	for _, source := range []string{"account", "positions", "earnings", "regime_stage", "tape"} {
		health = append(health, rpc.SourceHealth{
			Source: source, Status: "unavailable", Notes: []string{reason},
		})
	}
	return &rpc.RulesResult{
		AsOf: now, Enabled: true, Status: "degraded", InputHealth: health,
		PolicyID: pol.ID, PolicyVersion: pol.Version, PolicyFingerprint: &fingerprint,
	}
}

// rulebookEnabled reads features.rulebook.enabled (runtime, default true).
func (s *Server) rulebookEnabled() bool {
	if s.platformSettings == nil {
		return true
	}
	data := s.platformSettings.snapshot()
	if data.Features.Rulebook.Enabled != nil {
		return *data.Features.Rulebook.Enabled
	}
	return true
}

// rulebookEarningsOverrides returns the operator's manual per-symbol
// earnings dates ("YYYY-MM-DD" or "YYYY-MM-DDTamc"/"Tbmo"), uppercased keys.
func (s *Server) rulebookEarningsOverrides() map[string]string {
	if s.platformSettings == nil {
		return nil
	}
	data := s.platformSettings.snapshot()
	if len(data.Features.Rulebook.EarningsOverrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(data.Features.Rulebook.EarningsOverrides))
	for sym, v := range data.Features.Rulebook.EarningsOverrides {
		out[strings.ToUpper(strings.TrimSpace(sym))] = strings.TrimSpace(v)
	}
	return out
}

func (s *Server) handleRulesSnapshot(ctx context.Context, req *rpc.Request) (*rpc.RulesResult, error) {
	var params rpc.RulesSnapshotParams
	if len(req.Params) > 0 {
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, err
		}
	}
	binding := s.currentRulebookBinding()
	if cached, ok := s.cachedRulebookResult(binding, rulesPreviewTTL, s.orderNow().UTC()); ok {
		if params.Symbol != "" {
			filterRuleOffenders(cached, strings.ToUpper(strings.TrimSpace(params.Symbol)))
		}
		return cached, nil
	}
	res := s.canonicalRulebookResult(ctx, binding)
	if params.Symbol != "" {
		filterRuleOffenders(res, strings.ToUpper(strings.TrimSpace(params.Symbol)))
	}
	return res, nil
}

// rulesForPreview returns a scope-bound canonical evaluation. If neither a
// fresh cache nor a bounded canonical read is available, the returned
// degraded result produces a typed rulebook_unavailable advisory; contention
// can never silently erase warnings.
func (s *Server) rulesForPreview(ctx context.Context) *rpc.RulesResult {
	if !s.rulebookEnabled() {
		return nil
	}
	binding := s.currentRulebookBinding()
	if cached, ok := s.cachedRulebookResult(binding, rulesPreviewTTL, s.orderNow().UTC()); ok {
		return cached
	}
	return s.canonicalRulebookResult(ctx, binding)
}

// canonicalRulebookResult is the full Rulebook single-flight used by user,
// app, and preview reads. It rechecks the cache after acquiring the evaluation
// lock so a caller queued behind the daemon refresh reuses that result instead
// of issuing a second account/positions/quote fanout.
func (s *Server) canonicalRulebookResult(ctx context.Context, binding rulebookCacheBinding) *rpc.RulesResult {
	if ctx == nil {
		return s.rulebookUnavailableResult("canonical_read_context_missing")
	}
	if cached, ok := s.cachedRulebookResult(binding, rulesPreviewTTL, s.orderNow().UTC()); ok {
		return cached
	}
	if !s.lockRulesEvaluation(ctx) {
		return s.rulebookUnavailableResult("canonical_read_contention")
	}
	defer s.rulesEvaluationMu.Unlock()
	if cached, ok := s.cachedRulebookResult(binding, rulesPreviewTTL, s.orderNow().UTC()); ok {
		return cached
	}
	result := s.evaluateRulesModeLocked(ctx, true, true)
	if ctx.Err() != nil || !sameRulebookBinding(binding, s.currentRulebookBinding()) {
		return s.rulebookUnavailableResult("canonical_read_interrupted")
	}
	// Publish while the single-flight remains held. A queued caller's post-lock
	// recheck therefore cannot slip between evaluation completion and caching.
	if !s.publishCanonicalRulebookResult(ctx, result, binding) {
		return s.rulebookUnavailableResult("canonical_publish_interrupted")
	}
	return cloneRulesResult(result)
}

func (s *Server) lockRulesEvaluation(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	for !s.rulesEvaluationMu.TryLock() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
	return true
}

// evaluateRulesMode lets read-only daemon composition reuse the exact rule
// mapper/evaluator without turning its account fetch into a capital-state
// observation. Transition journaling remains exclusively in
// handleRulesSnapshot. All canonical callers serialize here so two complete
// account/positions/tape assemblies cannot overlap.
func (s *Server) evaluateRulesMode(ctx context.Context, includeTape, allowMaintenance bool) *rpc.RulesResult {
	if ctx == nil {
		return s.rulebookUnavailableResult("canonical_read_context_missing")
	}
	binding := s.currentRulebookBinding()
	if cached, ok := s.cachedRulebookResult(binding, rulesPreviewTTL, s.orderNow().UTC()); ok {
		return cached
	}
	if !s.lockRulesEvaluation(ctx) {
		return s.rulebookUnavailableResult("canonical_read_contention")
	}
	defer s.rulesEvaluationMu.Unlock()
	if cached, ok := s.cachedRulebookResult(binding, rulesPreviewTTL, s.orderNow().UTC()); ok {
		return cached
	}
	return s.evaluateRulesModeLocked(ctx, includeTape, allowMaintenance)
}

func (s *Server) evaluateRulesModeLocked(ctx context.Context, includeTape, allowMaintenance bool) *rpc.RulesResult {
	now := time.Now()
	brokerScope := s.currentBrokerStateScope()
	pol := risk.DefaultRulebookPolicy()
	fp := rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: pol.FingerprintKey()}
	res := &rpc.RulesResult{
		AsOf:              now,
		Enabled:           s.rulebookEnabled(),
		Status:            "ok",
		PolicyID:          pol.ID,
		PolicyVersion:     pol.Version,
		PolicyFingerprint: &fp,
	}
	if !res.Enabled {
		res.Status = "disabled"
		return res
	}

	in := risk.RuleInputs{AsOf: now}
	var health []rpc.SourceHealth
	cal := marketcal.New()
	dailyPnLDue := true
	if session, err := cal.SessionAt(marketcal.MarketUSEquity, now); err == nil && session.State != marketcal.StateUnknown {
		in.SessionOpen = session.IsOpen
		dailyPnLDue = session.IsOpen
	}

	acct, accountAuthority, acctErr := s.buildAccountSummaryWithAuthority(ctx, allowMaintenance)
	accountCompletedAt := time.Now().UTC()
	if acctErr != nil || acct == nil {
		in.Account = risk.SourceState{Healthy: false, Reason: "account_unavailable"}
		health = append(health, rpc.SourceHealth{Source: "account", Status: "unavailable", Notes: []string{errText(acctErr)}})
	} else {
		accountState, accountHealth := rulebookAccountSourceHealth(brokerScope, acct, accountAuthority, dailyPnLDue, accountCompletedAt)
		in.Account = accountState
		if accountAuthority.NetLiquidationAvailable {
			in.NLVBase = new(acct.NetLiquidation)
		}
		if accountAuthority.TotalCashAvailable {
			in.CashBase = new(acct.TotalCash)
		}
		in.DailyPnLBase = acct.DailyPnL
		if accountAuthority.BaseCurrencyAvailable {
			if baseCurrency, ok := rulebookBaseCurrency(acct.BaseCurrency); ok {
				in.BaseCurrency = baseCurrency
				res.BaseCurrency = baseCurrency
			}
		}
		health = append(health, accountHealth)
	}

	var portfolioHealth ibkrlib.PortfolioStreamHealth
	pos, posErr := s.handlePositionsListCapturedForScope(ctx, &rpc.Request{}, &portfolioHealth, brokerScope)
	positionsCompletedAt := time.Now().UTC()
	switch {
	case posErr != nil || pos == nil:
		in.Positions = risk.SourceState{Healthy: false, Reason: "positions_unavailable"}
		health = append(health, rpc.SourceHealth{Source: "positions", Status: "unavailable", Notes: []string{errText(posErr)}})
	case proposalPositionsUnprimed(pos, acct, portfolioHealth):
		in.Positions = risk.SourceState{Healthy: false, Reason: "positions_pending"}
		health = append(health, rpc.SourceHealth{Source: "positions", Status: "pending",
			Notes: []string{"portfolio stream not yet primed; an empty position list needs a completed account-scoped receipt and no contradicting account summary"}})
	default:
		positionsState, positionsHealth := rulebookPortfolioSourceHealth(brokerScope, portfolioHealth, positionsCompletedAt)
		in.Positions = positionsState
		health = append(health, positionsHealth)
	}

	// Rule 14 inputs: base-normalized non-base NLV, corroborated against the
	// positions snapshot so an empty currency report on a book with non-base
	// legs degrades to unknown instead of passing (never-false-pass). An
	// unhealthy or unprimed snapshot cannot corroborate anything — pass nil
	// so the empty-report case stays unknown during boot races.
	if acct != nil {
		corr := pos
		if !in.Positions.Healthy {
			corr = nil
		}
		in.NonBaseNLVBase, in.NonBaseCurrencies = nonBaseExposure(acct, corr)
	}

	// Rules 3/4/12 regime-conditional thresholds: serve the latched stage,
	// mark it carried past the policy max age, and self-heal a cold or stale
	// latch with one async refresh (single-flight; never from previews).
	if st := s.rulesRegimeStageSnapshot(); st.Bucket != "" {
		in.RegimeStage = st.Bucket
		in.RegimeStageAsOf = st.AsOf
		maxAge := time.Duration(pol.RegimeStageMaxAgeMinutes) * time.Minute
		in.RegimeStageCarried = time.Since(st.AsOf) > maxAge
		stageStatus := "ok"
		if in.RegimeStageCarried {
			stageStatus = "stale"
		}
		health = append(health, rpc.SourceHealth{Source: "regime_stage", Status: stageStatus, AsOf: st.AsOf,
			Notes: []string{fmt.Sprintf("stage %s (%s bucket)", st.Stage, st.Bucket)}})
	} else {
		health = append(health, rpc.SourceHealth{Source: "regime_stage", Status: "unavailable",
			Notes: []string{"no regime stage observed yet; calm thresholds apply until a regime snapshot lands"}})
	}
	if includeTape && (in.RegimeStage == "" || in.RegimeStageCarried) {
		s.kickRulesRegimeStageRefresh(ctx)
	}

	earningsDegraded := false
	if in.Positions.Healthy && pos != nil {
		in.Names = mapRuleNames(pos, pol, in.BaseCurrency)
		earnings, infos := s.assembleEarnings(ctx, in.Names, pol, cal, now, allowMaintenance)
		in.Earnings = earnings
		res.Earnings = infos
		earningsHealth, degraded := rulesEarningsSourceHealth(infos, now)
		earningsDegraded = degraded
		health = append(health, earningsHealth)
	} else {
		earningsDegraded = true
		health = append(health, rpc.SourceHealth{
			Source: "earnings", Status: "unavailable",
			Notes: []string{"earnings cannot be projected without a current scoped portfolio"},
		})
	}

	if includeTape && in.SessionOpen && in.Positions.Healthy {
		in.SPYDayChangePct = s.spyDayChangePct(ctx)
	}
	health = append(health, rulebookTapeSourceHealth(includeTape, in.SessionOpen, in.Positions.Healthy, in.SPYDayChangePct, now))

	ev := risk.EvaluateRulebook(in, pol)
	res.Rules = ev.Rows
	res.Ranked = ev.Ranked
	res.InputHealth = health
	counts := map[string]int{}
	for _, r := range ev.Rows {
		counts[r.Status]++
	}
	res.BreachCounts = counts
	if !in.Positions.Healthy || !in.Account.Healthy || earningsDegraded || rulebookInputHealthDegraded(health) {
		res.Status = "degraded"
	}
	// The envelope is the completion boundary for the assembled inputs. Some
	// components (notably the positions read model) stamp their own receipt
	// after evaluation starts; retaining the start time here makes valid
	// same-call evidence appear to come from the future to strict consumers.
	res.AsOf = time.Now().UTC()
	return res
}

func rulebookInputHealthDegraded(health []rpc.SourceHealth) bool {
	for _, source := range health {
		if source.Status != rpc.SourceStatusOK {
			return true
		}
	}
	return false
}

func rulebookTapeSourceHealth(includeTape, sessionOpen, positionsHealthy bool, dayChange *float64, now time.Time) rpc.SourceHealth {
	switch {
	case !includeTape:
		return rpc.SourceHealth{Source: "tape", Status: "unavailable", Notes: []string{"canonical tape read was not requested"}}
	case !sessionOpen:
		return rpc.SourceHealth{
			Source: "tape", Status: rpc.SourceStatusOK, AsOf: now.UTC(), RefreshState: rpc.SourceRefreshNotDue,
			Notes: []string{"US equity tape rules are not due outside the live session"},
		}
	case !positionsHealthy:
		return rpc.SourceHealth{Source: "tape", Status: "unavailable", Notes: []string{"tape rules require a current scoped portfolio"}}
	case dayChange == nil:
		return rpc.SourceHealth{Source: "tape", Status: "unavailable", AsOf: now.UTC(), Notes: []string{"current SPY tape quote is unavailable"}}
	default:
		return rpc.SourceHealth{Source: "tape", Status: rpc.SourceStatusOK, AsOf: now.UTC()}
	}
}

func rulebookAccountSourceHealth(scope brokerStateScope, account *rpc.AccountResult, authority accountSummaryAuthority, dailyPnLDue bool, completedAt time.Time) (risk.SourceState, rpc.SourceHealth) {
	health := rpc.SourceHealth{Source: "account"}
	if account == nil || !brokerScopeConcrete(scope) || !brokerScopeAccountConcrete(account.AccountID) ||
		!strings.EqualFold(strings.TrimSpace(account.AccountID), strings.TrimSpace(scope.Account)) {
		health.Status = "unavailable"
		health.Notes = []string{"account snapshot is missing or belongs to another broker account"}
		return risk.SourceState{Reason: "account_unavailable"}, health
	}
	if authority.Provenance != ibkrlib.AccountSummaryProvenanceRequest {
		health.Status = rpc.SourceStatusDegraded
		health.Notes = []string{"one-shot account snapshot was unavailable; unstamped cache is context only"}
		return risk.SourceState{Reason: "account_cached_fallback"}, health
	}
	health.AsOf = authority.AsOf.UTC()
	if health.AsOf.IsZero() || health.AsOf.After(completedAt.UTC()) {
		health.Status = "unavailable"
		health.Notes = []string{"account snapshot receipt time is missing or future-dated"}
		return risk.SourceState{Reason: "account_unavailable"}, health
	}
	missing := make([]string, 0, 4)
	if !authority.NetLiquidationAvailable || account.NetLiquidation <= 0 || math.IsNaN(account.NetLiquidation) || math.IsInf(account.NetLiquidation, 0) {
		missing = append(missing, "positive net liquidation")
	}
	if _, ok := rulebookBaseCurrency(account.BaseCurrency); !authority.BaseCurrencyAvailable || !ok {
		missing = append(missing, "base currency")
	}
	if !authority.TotalCashAvailable || math.IsNaN(account.TotalCash) || math.IsInf(account.TotalCash, 0) {
		missing = append(missing, "total cash")
	}
	pnlFailed, pnlNotDue := rulebookDailyPnLState(account, dailyPnLDue)
	if pnlFailed {
		missing = append(missing, "daily P&L")
	}
	if len(missing) > 0 {
		health.Status = rpc.SourceStatusDegraded
		health.Notes = []string{"fresh account snapshot is incomplete: " + strings.Join(missing, ", ")}
		return risk.SourceState{Reason: "account_incomplete"}, health
	}
	health.Status = rpc.SourceStatusOK
	if pnlNotDue {
		health.RefreshState = rpc.SourceRefreshNotDue
		health.Notes = []string{"daily P&L is not due outside the US equity regular session"}
	}
	return risk.SourceState{Healthy: true}, health
}

func rulebookDailyPnLState(account *rpc.AccountResult, dailyPnLDue bool) (failed, notDue bool) {
	if account == nil {
		return true, false
	}
	if observation := account.DailyPnLObservation; observation != nil {
		switch observation.Status {
		case rpc.DailyPnLObservationNotDue:
			return false, true
		case rpc.DailyPnLObservationMissing, rpc.DailyPnLObservationInvalid, rpc.DailyPnLObservationStale:
			return true, false
		case rpc.DailyPnLObservationOK:
			return account.DailyPnL == nil || math.IsNaN(*account.DailyPnL) || math.IsInf(*account.DailyPnL, 0), false
		default:
			return true, false
		}
	}
	if account.DailyPnL == nil {
		return dailyPnLDue, !dailyPnLDue
	}
	return math.IsNaN(*account.DailyPnL) || math.IsInf(*account.DailyPnL, 0), false
}

func rulebookBaseCurrency(raw string) (string, bool) {
	currency := normCcy(raw)
	if len(currency) != 3 || currency == "BASE" {
		return "", false
	}
	for i := range len(currency) {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return "", false
		}
	}
	return currency, true
}

func rulebookPortfolioSourceHealth(scope brokerStateScope, receipt ibkrlib.PortfolioStreamHealth, now time.Time) (risk.SourceState, rpc.SourceHealth) {
	evidenceAt := portfolioStreamEvidenceAsOf(receipt)
	health := rpc.SourceHealth{
		Source: "positions", AsOf: evidenceAt,
		MaxAgeSeconds: int64(portfolioStreamReceiptMaxAge / time.Second),
	}
	if !evidenceAt.IsZero() && !evidenceAt.After(now.UTC()) {
		health.AgeSeconds = int64(now.UTC().Sub(evidenceAt) / time.Second)
	}
	switch classifyPortfolioStreamHealth(scope, receipt, now) {
	case orderIntegrityHealthCurrent:
		health.Status = rpc.SourceStatusOK
		return risk.SourceState{Healthy: true}, health
	case orderIntegrityHealthStale:
		health.Status = rpc.SourceStatusStale
		health.Notes = []string{"portfolio stream receipt exceeded its freshness budget"}
		return risk.SourceState{Reason: "positions_stale"}, health
	default:
		if receipt.InitialCompletedAt.IsZero() && !receipt.RequestedAt.IsZero() && !receipt.RequestedAt.After(now.UTC()) {
			health.Status = "pending"
			health.AsOf = receipt.RequestedAt.UTC()
			health.AgeSeconds = int64(now.UTC().Sub(receipt.RequestedAt.UTC()) / time.Second)
			health.Notes = []string{"portfolio stream has not completed its initial snapshot"}
			return risk.SourceState{Reason: "positions_pending"}, health
		}
		health.Status = "unavailable"
		health.Notes = []string{"portfolio stream receipt is missing, future-dated, or belongs to another broker account"}
		return risk.SourceState{Reason: "positions_unavailable"}, health
	}
}

func rulesEarningsSourceHealth(infos []rpc.EarningsInfo, now time.Time) (rpc.SourceHealth, bool) {
	status := rpc.SourceStatusOK
	degraded := false
	var lastFailure *rpc.SourceFailure
	var nextAttempt *time.Time
	var notes []string
	informational := map[string]struct{}{}
	for _, info := range infos {
		resolved := info.Status == rpc.EarningsStatusDate || info.Status == rpc.EarningsStatusTerminalNonReporting || info.Status == rpc.EarningsStatusNotApplicable
		wshEntitlementOnly := earningsWSHNotEntitledOnly(info)
		if info.Source == "unknown" || info.Stale || (!resolved && !((info.Status == rpc.EarningsStatusNoDatePublished || info.Status == rpc.EarningsStatusUnsupportedSecurity) && wshEntitlementOnly)) {
			status = rpc.SourceStatusDegraded
			degraded = true
			notes = append(notes, info.Symbol+": "+nonEmptyString(info.Reason, nonEmptyString(info.Status, "not_observed")))
		} else if info.Reason == earningsReasonSingleSource && !wshEntitlementOnly && status == rpc.SourceStatusOK {
			status = rpc.SourceStatusPartial
		}
		for _, provider := range info.Providers {
			failure := provider.LastFailure
			if failure != nil && (lastFailure == nil || failure.FailedAt.After(lastFailure.FailedAt)) {
				copyFailure := *failure
				lastFailure = &copyFailure
				nextAttempt = cloneTimePointer(provider.NextAttempt)
			}
			if resolved && failure != nil {
				note := fmt.Sprintf("retained provider issue: source=%s code=%s stage=%s retry=scheduled", provider.Provider, failure.Code, failure.Stage)
				informational[note] = struct{}{}
			}
		}
		if failure := func() *rpc.SourceFailure {
			if info.Identity == nil {
				return nil
			}
			return info.Identity.LastFailure
		}(); failure != nil && (lastFailure == nil || failure.FailedAt.After(lastFailure.FailedAt)) {
			copyFailure := *failure
			lastFailure = &copyFailure
			nextAttempt = cloneTimePointer(info.Identity.NextAttempt)
		}
		if resolved && info.Identity != nil && info.Identity.NotApplicable && info.Identity.LastFailure != nil {
			failure := info.Identity.LastFailure
			note := fmt.Sprintf("retained broker identity issue: code=%s stage=%s retry=scheduled", failure.Code, failure.Stage)
			informational[note] = struct{}{}
		}
	}
	infoNotes := make([]string, 0, len(informational))
	for note := range informational {
		infoNotes = append(infoNotes, note)
	}
	sort.Strings(infoNotes)
	notes = append(notes, infoNotes...)
	return rpc.SourceHealth{Source: "earnings", Status: status, AsOf: now, NextAttempt: nextAttempt, LastFailure: lastFailure, Notes: notes}, degraded
}

func earningsWSHNotEntitledOnly(info rpc.EarningsInfo) bool {
	if info.Status != rpc.EarningsStatusDate && info.Status != rpc.EarningsStatusNoDatePublished && info.Status != rpc.EarningsStatusUnsupportedSecurity {
		return false
	}
	seenNasdaq, seenWSH := false, false
	for _, provider := range info.Providers {
		switch provider.Provider {
		case earningsNasdaqProvider:
			if provider.Status != rpc.EarningsStatusDate && provider.Status != rpc.EarningsStatusNoDatePublished && provider.Status != rpc.EarningsStatusUnsupportedSecurity {
				return false
			}
			seenNasdaq = true
		case earningsWSHProvider:
			failure := provider.LastFailure
			if provider.Status != rpc.EarningsStatusTransportFailure || failure == nil || failure.Code != rpc.SourceFailureNotEntitled || failure.Retryable ||
				(failure.Stage != rpc.SourceFailureStageWSHMetadata && failure.Stage != rpc.SourceFailureStageWSHEvent) {
				return false
			}
			seenWSH = true
		default:
			return false
		}
	}
	return seenNasdaq && seenWSH
}

// mapRuleNames converts the positions snapshot into pure rule inputs. The
// exposure figure is the same DollarDeltaBase the Stress read uses — one
// aggregation, several bars (design: "Which verdict wins when").
func mapRuleNames(pos *rpc.PositionsResult, pol risk.RulebookPolicy, baseCcy string) []risk.NameInput {
	loc, _ := time.LoadLocation("America/New_York")
	today := time.Now().In(loc)
	exactStocks, stocksAuthoritative := rulebookExactStocksBySymbol(pos)
	names := make([]risk.NameInput, 0, len(pos.ByUnderlying))
	for _, g := range pos.ByUnderlying {
		n := risk.NameInput{Symbol: g.Underlying}
		if stocksAuthoritative {
			identity, ok := exactStocks[strings.ToUpper(strings.TrimSpace(g.Underlying))]
			if ok && !identity.ambiguous && g.Stock != nil && g.Stock.ConID == identity.conID &&
				sameRulebookStockSecurityType(g.Stock.SecType, identity.secType) {
				n.StockConID = identity.conID
				n.StockSecType = identity.secType
			}
		} else if g.Stock != nil {
			n.StockConID = g.Stock.ConID
			n.StockSecType = g.Stock.SecType
		}
		// ExposureBaseComplete guards rule 1's lower bound: the group sum
		// excludes legs the aggregator couldn't price (delta without spot,
		// markless stock, missing FX) — a bound seeded from a partial sum
		// could overstate, and "proven ≥" must never overstate.
		n.ExposureBaseComplete = g.GroupDollarDeltaBase != nil
		if g.GroupDollarDeltaBase != nil {
			n.ExposureBase = *g.GroupDollarDeltaBase
		}
		if g.GroupMarketValueBase != nil {
			n.MarketValueBase = *g.GroupMarketValueBase
		}
		var stockSpot *float64
		if g.Stock != nil && g.Stock.Quantity != 0 {
			n.HasStockLeg = true
			n.StockDayChangePct = g.Stock.DayChangePct
			// The account mark that values the book is good enough to assess
			// an option leg's OTM-ness/extrinsic when the leg's own greeks
			// tick carried no spot (routine off-session). Stale marks are
			// not: nil stays nil rather than valuing against a dead price.
			// Indicative is deliberately NOT gated: it describes the quote
			// enrichment layer, not the account mark, and pre-market — where
			// every stock row is indicative — is exactly when this join
			// earns its keep.
			if g.Stock.Mark > 0 && !g.Stock.Stale {
				stockSpot = new(g.Stock.Mark)
			}
			if g.Stock.Mark <= 0 {
				n.ExposureBaseComplete = false
			}
		}
		for _, o := range g.Options {
			if o.Quantity == 0 {
				continue
			}
			leg := risk.LegInput{
				Desc:        legDesc(o),
				Right:       o.Right,
				Strike:      o.Strike,
				Quantity:    o.Quantity,
				Multiplier:  float64(max(o.Multiplier, 1)),
				Mark:        o.Mark,
				Underlying:  o.Underlying,
				Delta:       o.Delta,
				HedgeListed: pol.IsHedgeSymbol(g.Underlying),
			}
			if o.Delta != nil && o.Underlying == nil {
				// The aggregator can't pair this delta with a spot, so the
				// group sum excluded the leg — no lower bound may build on it.
				n.ExposureBaseComplete = false
			}
			if leg.Underlying == nil && stockSpot != nil {
				leg.Underlying = stockSpot
				leg.UnderlyingSource = risk.UnderlyingSourceStockLegMark
			}
			if o.MarketValueBase != nil {
				leg.MarketValueBase = *o.MarketValueBase
			} else {
				leg.MarketValueBase = o.MarketValue
			}
			// FX path: same-currency is implicitly 1 and non-base uses the
			// gateway rate (positionBaseRate) — the MV ratio is only a
			// fallback and is undefined on a zero-marked leg, which is
			// exactly the −100% line rule 13 exists to catch.
			if rate, ok := positionBaseRate(o, baseCcy); ok {
				leg.FXToBase = &rate
			} else if o.MarketValueBase != nil && o.MarketValue != 0 {
				leg.FXToBase = new(*o.MarketValueBase / o.MarketValue)
			}
			// IBKR AvgCost is multiplier-inclusive on options: cost basis is
			// AvgCost × |contracts| × fx, with NO extra multiplier.
			if leg.FXToBase != nil && o.AvgCost > 0 {
				leg.CostBasisBase = new(o.AvgCost * math.Abs(o.Quantity) * *leg.FXToBase)
			}
			if exp, err := time.ParseInLocation("20060102", o.Expiry, loc); err == nil {
				leg.Expiry = exp
				leg.DTE = int(exp.Sub(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)).Hours() / 24)
			}
			if extPerShare, ok := risk.OptionExtrinsicPerShare(o.Right, leg.Underlying, o.Strike, o.Mark); ok && o.Quantity > 0 {
				ext := extPerShare * o.Quantity * leg.Multiplier
				if o.MarketValueBase != nil && o.MarketValue != 0 {
					ext *= *o.MarketValueBase / o.MarketValue
				}
				leg.ExtrinsicBase = &ext
			}
			if o.Delta == nil {
				gap := leg.MarketValueBase
				if gap < 0 {
					gap = -gap
				}
				n.GreeksGapNotionalBase += gap
			}
			n.Legs = append(n.Legs, leg)
		}
		if n.HasStockLeg || len(n.Legs) > 0 {
			names = append(names, n)
		}
	}
	return names
}

type rulebookExactStockIdentity struct {
	conID     int
	secType   string
	ambiguous bool
}

// rulebookExactStocksBySymbol uses the ungrouped position rows as identity
// authority. PositionGroup is intentionally symbol-aggregated and has room
// for only one stock pointer, so it cannot prove exact identity when two
// listings share a ticker. A non-nil Stocks slice means the full projection
// was supplied; any missing, malformed, or conflicting identity then stays
// unclassified instead of falling back to the group's overwritten pointer.
func rulebookExactStocksBySymbol(pos *rpc.PositionsResult) (map[string]rulebookExactStockIdentity, bool) {
	if pos == nil || pos.Stocks == nil {
		return nil, false
	}
	identities := make(map[string]rulebookExactStockIdentity, len(pos.Stocks))
	for _, stock := range pos.Stocks {
		symbol := strings.ToUpper(strings.TrimSpace(stock.Symbol))
		secType := strings.ToUpper(strings.TrimSpace(stock.SecType))
		if symbol == "" || stock.ConID <= 0 || !isRulebookStockSecurityType(secType) {
			if symbol != "" {
				identity := identities[symbol]
				identity.ambiguous = true
				identities[symbol] = identity
			}
			continue
		}
		secType = "STK"
		identity, exists := identities[symbol]
		if exists && (identity.ambiguous || identity.conID != stock.ConID || identity.secType != secType) {
			identity.ambiguous = true
			identities[symbol] = identity
			continue
		}
		identities[symbol] = rulebookExactStockIdentity{conID: stock.ConID, secType: secType}
	}
	return identities, true
}

func isRulebookStockSecurityType(secType string) bool {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK", "ETF":
		return true
	default:
		return false
	}
}

func sameRulebookStockSecurityType(got, want string) bool {
	return isRulebookStockSecurityType(got) && isRulebookStockSecurityType(want)
}

func legDesc(o rpc.PositionView) string {
	return fmt.Sprintf("%s %s %s %s", o.Symbol, o.Expiry, o.Right, trimFloat(o.Strike))
}

// nonBaseExposure sums base-normalized NLV held outside the account's base
// currency (rule 14). nil = unavailable: an empty currency report is only
// trusted as "base-only" when the positions snapshot corroborates it —
// $LEDGER flakes must degrade to unknown, never pass a 90%-USD book.
func nonBaseExposure(acct *rpc.AccountResult, pos *rpc.PositionsResult) (*float64, []string) {
	base := strings.ToUpper(strings.TrimSpace(acct.BaseCurrency))
	if base == "" {
		return nil, nil
	}
	if len(acct.CurrencyExposure) == 0 {
		if positionsCarryNonBase(pos, base) {
			return nil, nil // report absent but the book visibly holds non-base legs
		}
		return new(0.0), nil // documented same-currency shape, corroborated
	}
	total := 0.0
	var ccys []string
	for _, row := range acct.CurrencyExposure {
		ccy := strings.ToUpper(strings.TrimSpace(row.Currency))
		if ccy == "" || ccy == base || ccy == "BASE" {
			continue
		}
		if row.ExchangeRate == 0 || (row.NetLiquidationBase == 0 && row.NetLiquidationCcy != 0) {
			return nil, nil // FX conversion unavailable for this slice of NLV
		}
		total += row.NetLiquidationBase
		ccys = append(ccys, ccy)
	}
	return &total, ccys
}

func positionsCarryNonBase(pos *rpc.PositionsResult, base string) bool {
	if pos == nil {
		return true // can't corroborate — stay unknown
	}
	for _, g := range pos.ByUnderlying {
		if g.Stock != nil && g.Stock.Currency != "" && !strings.EqualFold(g.Stock.Currency, base) {
			return true
		}
		for _, o := range g.Options {
			if o.Currency != "" && !strings.EqualFold(o.Currency, base) {
				return true
			}
		}
	}
	if pos.Portfolio != nil && pos.Portfolio.FXSensitivityPerPct != nil {
		return true
	}
	return false
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// assembleEarnings merges manual overrides with the cache, applies reviewed
// exact-contract terminal evidence, kicks the async refresher for gaps, and
// computes ET session distances. Overrides remain authoritative over ordinary
// provider dates, but they cannot silently overrule a terminal classification:
// a date on the same exact cancelled contract is an explicit conflict.
func (s *Server) assembleEarnings(ctx context.Context, names []risk.NameInput, pol risk.RulebookPolicy, cal *marketcal.Calendar, now time.Time, allowRefresh bool) (map[string]risk.EarningsInput, []rpc.EarningsInfo) {
	loc, _ := time.LoadLocation("America/New_York")
	overrides := s.rulebookEarningsOverrides()
	earnings := make(map[string]risk.EarningsInput, len(names))
	infos := make([]rpc.EarningsInfo, 0, len(names))
	var toFetch []earningsRefreshTarget
	for _, n := range names {
		sym := strings.ToUpper(n.Symbol)
		info := rpc.EarningsInfo{Symbol: sym, Source: "unknown", Reason: "not_observed"}
		override, hasOverride := risk.EarningsInput{}, false
		if raw, ok := overrides[sym]; ok {
			override, hasOverride = parseEarningsOverride(raw, loc)
		}
		view, observed := earningsResolutionView{}, false
		if s.earnings != nil {
			if len(n.Legs) == 0 {
				view, observed = s.earnings.resolutionForIdentity(sym, n.StockConID, n.StockSecType)
			} else {
				// Position groups do not carry each option leg's exact underlying
				// ConID. A stock identity proof may therefore exempt only a
				// stock-only name; mixed groups use ordinary provider evidence.
				view, observed = s.earnings.resolution(sym)
			}
		}
		if observed {
			info.Status = view.Status
			info.Reason = view.Reason
			info.Providers = view.Providers
			info.Identity = view.Identity
		}

		if terminal, found := s.earningsTerminal.terminalEarningsFor(n, now); found {
			terminalInfo := terminal.Info
			info.Terminal = &terminalInfo
			switch {
			case terminal.Status != rpc.EarningsStatusTerminalNonReporting:
				info.Status = terminal.Status
				info.Reason = terminal.Reason
			case hasOverride || (observed && (view.Status == rpc.EarningsStatusDate || view.Status == rpc.EarningsStatusConflictingSources)):
				info.Status = rpc.EarningsStatusConflictingSources
				info.Reason = earningsTerminalReasonSourceConflict
			default:
				info.Source = "verified_terminal"
				info.Status = rpc.EarningsStatusTerminalNonReporting
				info.Reason = terminal.Reason
				earnings[sym] = risk.EarningsInput{
					TerminalNonReporting: true,
					Source:               "verified_terminal",
					Reason:               terminal.Reason,
				}
				infos = append(infos, info)
				continue
			}
			earnings[sym] = risk.EarningsInput{Source: "verified_terminal", Reason: info.Reason}
			toFetch = append(toFetch, earningsRefreshTarget{Symbol: sym, ConID: n.StockConID, SecType: n.StockSecType})
			infos = append(infos, info)
			continue
		}

		if hasOverride && observed && view.Identity != nil && view.Identity.NotApplicable {
			info.Source = "broker_identity"
			info.Status = rpc.EarningsStatusConflictingSources
			info.Reason = earningsReasonConflicting
			earnings[sym] = risk.EarningsInput{Source: "broker_identity", Reason: earningsReasonConflicting}
			toFetch = append(toFetch, earningsRefreshTarget{Symbol: sym, ConID: n.StockConID, SecType: n.StockSecType})
			infos = append(infos, info)
			continue
		}

		if hasOverride {
			override.Source = "override"
			override.Reason = "override"
			override.SessionsUntil = sessionsUntil(cal, now.In(loc), override.Date)
			earnings[sym] = override
			info.Source = "override"
			info.Status = rpc.EarningsStatusDate
			info.Reason = "override"
			info.Date = override.Date.Format("2006-01-02")
			info.TimeOfDay = override.TimeOfDay
			infos = append(infos, info)
			continue
		}

		if observed {
			if view.Status == rpc.EarningsStatusNotApplicable {
				earnings[sym] = risk.EarningsInput{NotApplicable: true, Source: "broker_identity", Reason: earningsReasonBrokerNonIssuer}
				info.Source = "broker_identity"
			} else if view.Status == rpc.EarningsStatusDate {
				entry := view.Entry
				if d, err := time.ParseInLocation("2006-01-02", entry.Date, loc); err == nil {
					e := risk.EarningsInput{Known: true, Date: d, TimeOfDay: entry.TimeOfDay,
						Estimated: entry.Estimated, Stale: view.Stale, Source: "fetched", Reason: view.Reason}
					e.SessionsUntil = sessionsUntil(cal, now.In(loc), d)
					earnings[sym] = e
					info.Source = "fetched"
					info.Date = entry.Date
					info.TimeOfDay = entry.TimeOfDay
					info.Estimated = entry.Estimated
					info.ObservedAt = entry.ObservedAt
					info.Stale = view.Stale
				}
			} else {
				earnings[sym] = risk.EarningsInput{Known: false, Source: "fetched", Reason: nonEmptyString(view.Reason, view.Status)}
				if view.Status == rpc.EarningsStatusNoDatePublished || view.Status == rpc.EarningsStatusUnsupportedSecurity {
					info.Source = "fetched"
				}
			}
		}
		if _, ok := earnings[sym]; !ok {
			earnings[sym] = risk.EarningsInput{Known: false, Source: "unknown", Reason: info.Reason}
		}
		// Aggregate freshness and provider retry readiness are different
		// clocks. Always hand non-override names to the cache; kickRefresh
		// cheaply filters them by each provider's durable NextAttempt.
		toFetch = append(toFetch, earningsRefreshTarget{Symbol: sym, ConID: n.StockConID, SecType: n.StockSecType})
		infos = append(infos, info)
	}
	// Async, bounded, off the snapshot path — this call returns immediately.
	if allowRefresh && s.earnings != nil {
		s.earnings.kickRefreshTargets(context.WithoutCancel(ctx), toFetch)
	}
	return earnings, infos
}

// parseEarningsOverride accepts "YYYY-MM-DD" or "YYYY-MM-DDTamc"/"Tbmo".
func parseEarningsOverride(raw string, loc *time.Location) (risk.EarningsInput, bool) {
	tod := ""
	if date, half, found := strings.Cut(raw, "T"); found {
		half = strings.ToLower(strings.TrimSpace(half))
		if half == "amc" || half == "bmo" {
			tod = half
		}
		raw = date
	}
	d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(raw), loc)
	if err != nil {
		return risk.EarningsInput{}, false
	}
	return risk.EarningsInput{Known: true, Date: d, TimeOfDay: tod}, true
}

// sessionsUntil counts US equity sessions from today to target inclusive,
// in ET; nil when the target is behind us or unreasonably far.
func sessionsUntil(cal *marketcal.Calendar, today time.Time, target time.Time) *int {
	start := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	end := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, today.Location())
	if end.Before(start) {
		return nil
	}
	count := 0
	for d := start; !d.After(end) && count <= 40; d = d.AddDate(0, 0, 1) {
		session, err := cal.SessionAt(marketcal.MarketUSEquity, d.Add(16*time.Hour+30*time.Minute))
		if err != nil {
			return nil
		}
		if session.State == marketcal.StateRegular || session.State == marketcal.StateEarlyClose {
			count++
		}
	}
	return &count
}

// spyDayChangePct fetches one best-effort SPY snapshot quote for the tape
// rules. Snapshot quotes are transient (no standing subscription); failure
// degrades rule 9 to unknown rather than failing the evaluation.
func (s *Server) spyDayChangePct(ctx context.Context) *float64 {
	ctx, cancel := context.WithTimeout(ctx, rulesSPYQuoteTimeout)
	defer cancel()
	params, err := json.Marshal(rpc.QuoteSnapshotParams{
		Contract:  rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"},
		TimeoutMs: int(rulesSPYQuoteTimeout / time.Millisecond),
	})
	if err != nil {
		return nil
	}
	q, err := s.handleQuoteSnapshot(ctx, &rpc.Request{Params: params})
	if err != nil || q == nil {
		return nil
	}
	if q.QuoteChangePct != nil {
		return q.QuoteChangePct
	}
	return q.ChangePct
}

// filterRuleOffenders narrows offender/exempt lists to one symbol without
// changing verdicts — scoping is presentation, not policy.
func filterRuleOffenders(res *rpc.RulesResult, sym string) {
	for i := range res.Rules {
		res.Rules[i].Offenders = filterOffenders(res.Rules[i].Offenders, sym)
		res.Rules[i].Exempt = filterOffenders(res.Rules[i].Exempt, sym)
	}
}

func filterOffenders(list []risk.RuleOffender, sym string) []risk.RuleOffender {
	if len(list) == 0 {
		return list
	}
	out := list[:0:0]
	for _, o := range list {
		if strings.EqualFold(o.Symbol, sym) {
			out = append(out, o)
		}
	}
	return out
}

// ruleTransitionTerminalAuthority is the minimum exact-contract linkage a
// rule transition needs to name the accepted terminal/non-reporting authority
// it consumed. Human-facing issuer text and source-document references remain
// on the live RulesResult; immutable transition evidence carries only typed
// identities, revisions, timestamps, and fingerprints.
type ruleTransitionTerminalAuthority struct {
	ContractConID        int       `json:"contract_con_id"`
	AuthorityRevision    int64     `json:"authority_revision"`
	AuthorityFingerprint string    `json:"authority_fingerprint"`
	AuthorityBinding     string    `json:"authority_binding"`
	AuthorityReviewedAt  time.Time `json:"authority_reviewed_at"`
	VerifiedAt           time.Time `json:"verified_at"`
	RevalidateAfter      time.Time `json:"revalidate_after"`
	Classification       string    `json:"classification"`
}

// ruleTransitionIdentityAuthority links an accepted broker-nonissuer proof to
// its exact append-only observation and state revision without exposing the
// held contract identifier.
type ruleTransitionIdentityAuthority struct {
	AuthorityRevision    int64     `json:"authority_revision"`
	AuthorityFingerprint string    `json:"authority_fingerprint"`
	ObservationID        string    `json:"observation_id"`
	AuthorityBinding     string    `json:"authority_binding"`
	ObservedAt           time.Time `json:"observed_at"`
	Outcome              string    `json:"outcome"`
}

func acceptedRuleTransitionIdentityAuthorities(res *rpc.RulesResult) []ruleTransitionIdentityAuthority {
	out := make([]ruleTransitionIdentityAuthority, 0)
	if res == nil || res.AsOf.IsZero() {
		return out
	}
	type candidate struct {
		symbol    string
		authority ruleTransitionIdentityAuthority
	}
	candidates := make([]candidate, 0, len(res.Earnings))
	receipts := make(map[string]candidate)
	conflictedReceipts := make(map[string]bool)
	for _, info := range res.Earnings {
		if !validRulebookBrokerEarningsAuthority(info, res.AsOf.UTC()) {
			continue
		}
		identity := info.Identity
		authority := ruleTransitionIdentityAuthority{
			AuthorityRevision: identity.AuthorityRevision, AuthorityFingerprint: identity.AuthorityFingerprint,
			ObservationID: identity.ObservationID, AuthorityBinding: identity.AuthorityBinding,
			ObservedAt: identity.ProofObservedAt.UTC(), Outcome: identity.ProofOutcome,
		}
		item := candidate{symbol: info.Symbol, authority: authority}
		if previous, exists := receipts[authority.ObservationID]; exists && previous != item {
			conflictedReceipts[authority.ObservationID] = true
		}
		receipts[authority.ObservationID] = item
		candidates = append(candidates, item)
	}

	byRevision := make(map[int64]ruleTransitionIdentityAuthority)
	conflicted := make(map[int64]bool)
	for _, item := range candidates {
		authority := item.authority
		if conflictedReceipts[authority.ObservationID] || conflicted[authority.AuthorityRevision] {
			continue
		}
		if previous, exists := byRevision[authority.AuthorityRevision]; exists && previous != authority {
			delete(byRevision, authority.AuthorityRevision)
			conflicted[authority.AuthorityRevision] = true
			continue
		}
		byRevision[authority.AuthorityRevision] = authority
	}
	for _, authority := range byRevision {
		out = append(out, authority)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AuthorityRevision != out[j].AuthorityRevision {
			return out[i].AuthorityRevision < out[j].AuthorityRevision
		}
		return out[i].AuthorityFingerprint < out[j].AuthorityFingerprint
	})
	return out
}

// validRulebookBrokerEarningsAuthority recognizes only the exact public
// projection emitted by earningsCache. In particular, a retained proof remains
// usable only for the cache's five-minute exact-contract retry contract.
func validRulebookBrokerEarningsAuthority(info rpc.EarningsInfo, asOf time.Time) bool {
	identity := info.Identity
	if strings.TrimSpace(info.Symbol) == "" || strings.TrimSpace(info.Symbol) != info.Symbol || info.Stale ||
		info.Source != "broker_identity" || info.Status != rpc.EarningsStatusNotApplicable || identity == nil ||
		!identity.NotApplicable || identity.ProofOutcome != rpc.EarningsStatusNotApplicable ||
		identity.AuthorityRevision <= 0 || !validAlertRegistryFingerprint(identity.AuthorityFingerprint) ||
		!validOpaqueEarningsIdentityObservationID(identity.ObservationID) ||
		!validAlertRegistryFingerprint(identity.AuthorityBinding) ||
		identity.AuthorityBinding != rpc.BuildEarningsIdentityAuthorityBinding(info.Symbol, *identity) || asOf.IsZero() {
		return false
	}
	attemptedAt := identity.AttemptedAt.UTC()
	proofObservedAt := identity.ProofObservedAt.UTC()
	if attemptedAt.IsZero() || proofObservedAt.IsZero() || attemptedAt.After(asOf) || proofObservedAt.After(asOf) || identity.NextAttempt == nil {
		return false
	}
	nextAttempt := identity.NextAttempt.UTC()
	switch identity.Outcome {
	case earningsIdentityNotApplicable:
		return identity.LastFailure == nil && !proofObservedAt.Before(attemptedAt) &&
			nextAttempt.Equal(proofObservedAt.Add(earningsFreshWindow))
	case earningsIdentityUnknown:
		failure := identity.LastFailure
		if failure == nil || !failure.Retryable || failure.Stage != rpc.SourceFailureStageWSHContractResolve ||
			!validEarningsSourceFailure(*failure) {
			return false
		}
		switch failure.Code {
		case rpc.SourceFailureContractUnavailable, rpc.SourceFailureTimeout, rpc.SourceFailureGatewayUnavailable:
		default:
			return false
		}
		failedAt := failure.FailedAt.UTC()
		return !failedAt.IsZero() && !failedAt.After(asOf) && !failedAt.Before(attemptedAt) &&
			!proofObservedAt.After(attemptedAt) && nextAttempt.Equal(failedAt.Add(earningsContractResolutionRetry))
	default:
		return false
	}
}

func acceptedRuleTransitionTerminalAuthorities(res *rpc.RulesResult) []ruleTransitionTerminalAuthority {
	// A non-nil empty slice deliberately serializes as [] on every transition:
	// omission must not be mistaken for a writer-version or ingest failure.
	out := make([]ruleTransitionTerminalAuthority, 0)
	if res == nil || res.AsOf.IsZero() {
		return out
	}
	byConID := make(map[int]ruleTransitionTerminalAuthority)
	conflicted := make(map[int]bool)
	for _, info := range res.Earnings {
		if !validRulebookTerminalEarningsAuthority(info, res.AsOf.UTC()) {
			continue
		}
		terminal := info.Terminal
		authority := ruleTransitionTerminalAuthority{
			ContractConID:        terminal.ContractConID,
			AuthorityRevision:    terminal.AuthorityRevision,
			AuthorityFingerprint: terminal.AuthorityFingerprint,
			AuthorityBinding:     terminal.AuthorityBinding,
			AuthorityReviewedAt:  terminal.AuthorityReviewedAt.UTC(),
			VerifiedAt:           terminal.VerifiedAt.UTC(),
			RevalidateAfter:      terminal.RevalidateAfter.UTC(),
			Classification:       terminal.Classification,
		}
		if conflicted[authority.ContractConID] {
			continue
		}
		if previous, exists := byConID[authority.ContractConID]; exists && previous != authority {
			delete(byConID, authority.ContractConID)
			conflicted[authority.ContractConID] = true
			continue
		}
		byConID[authority.ContractConID] = authority
	}
	for _, authority := range byConID {
		out = append(out, authority)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ContractConID != out[j].ContractConID {
			return out[i].ContractConID < out[j].ContractConID
		}
		return out[i].AuthorityFingerprint < out[j].AuthorityFingerprint
	})
	return out
}

// validRulebookTerminalEarningsAuthority recognizes only a current terminal
// projection whose safe digest binds the exact public symbol to the contract,
// authority revision, fingerprint, timestamps, and closed classification.
func validRulebookTerminalEarningsAuthority(info rpc.EarningsInfo, asOf time.Time) bool {
	terminal := info.Terminal
	if strings.TrimSpace(info.Symbol) == "" || strings.TrimSpace(info.Symbol) != info.Symbol || info.Stale || asOf.IsZero() ||
		info.Source != "verified_terminal" || info.Status != rpc.EarningsStatusTerminalNonReporting || terminal == nil ||
		terminal.ContractConID <= 0 || terminal.AuthorityRevision <= 0 ||
		!validAlertRegistryFingerprint(terminal.AuthorityFingerprint) ||
		!validAlertRegistryFingerprint(terminal.AuthorityBinding) ||
		terminal.AuthorityBinding != rpc.BuildEarningsTerminalAuthorityBinding(info.Symbol, *terminal) {
		return false
	}
	if terminal.Classification != earningsTerminalClassEquityCancelled && terminal.Classification != earningsTerminalClassIssuerDissolved {
		return false
	}
	effective, err := time.Parse(time.DateOnly, terminal.EffectiveDate)
	if err != nil {
		return false
	}
	verifiedAt := terminal.VerifiedAt.UTC()
	reviewedAt := terminal.AuthorityReviewedAt.UTC()
	revalidateAfter := terminal.RevalidateAfter.UTC()
	return !verifiedAt.IsZero() && !reviewedAt.IsZero() && !revalidateAfter.IsZero() &&
		!effective.After(verifiedAt) && !verifiedAt.After(reviewedAt) && !reviewedAt.After(asOf) &&
		revalidateAfter.After(asOf) && revalidateAfter.After(verifiedAt) &&
		revalidateAfter.Sub(verifiedAt) <= 366*24*time.Hour
}

// journalRuleTransitions appends status changes as typed SQLite events so
// threshold calibration has a direct authoritative history. The JSONL branch
// remains only for legacy unit/import oracles.
func (s *Server) journalRuleTransitions(res *rpc.RulesResult) {
	s.journalRuleTransitionsBound(res, rulebookCacheBinding{}, false)
}

func (s *Server) journalRuleTransitionsForBinding(res *rpc.RulesResult, binding rulebookCacheBinding) {
	s.journalRuleTransitionsBound(res, binding, true)
}

func (s *Server) journalRuleTransitionsBound(res *rpc.RulesResult, binding rulebookCacheBinding, enforceBinding bool) {
	s.rulesMu.Lock()
	prev := s.lastRules
	if enforceBinding {
		cachedBinding := rulebookCacheBinding{
			scope: s.lastRulesScope, connector: s.lastRulesConnector, connectorEpoch: s.lastRulesConnectorEpoch,
			broker: s.lastRulesBroker, brokerCaptured: s.lastRulesBrokerCaptured,
		}
		if !sameRulebookBinding(binding, cachedBinding) {
			prev = nil
		}
	}
	s.rulesMu.Unlock()
	if res == nil || len(res.Rules) == 0 {
		return
	}
	policyFingerprint := ""
	if res.PolicyFingerprint != nil {
		policyFingerprint = res.PolicyFingerprint.Key
	}
	terminalAuthorities := acceptedRuleTransitionTerminalAuthorities(res)
	identityAuthorities := acceptedRuleTransitionIdentityAuthorities(res)
	prevStatus := map[string]string{}
	if prev != nil {
		for _, r := range prev.Rules {
			prevStatus[r.ID] = r.Status
		}
	}
	// All transition lines are buffered and land in ONE write syscall: to
	// the history-index ingester a torn multi-syscall append would be
	// indistinguishable from a crash mid-write. Line shape is unchanged
	// (json.Encoder emits each map with a trailing newline).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	var coreEvents []corestore.EventInput
	coreEventOrdinal := 0
	if s.coreStore != nil {
		head, err := s.coreStore.AuthorityHead(context.Background())
		if err != nil {
			s.warnf("rules transitions: SQLite authority head unavailable: %v", err)
			return
		}
		coreEventOrdinal = int(head.LastEventSeq) + 1
	}
	for _, r := range res.Rules {
		if was, seen := prevStatus[r.ID]; seen && was == r.Status {
			continue
		}
		entry := map[string]any{
			"version": 1, "at": res.AsOf, "rule": r.ID, "status": r.Status,
			"was": prevStatus[r.ID], "evidence": r.Evidence,
			"policy_id": res.PolicyID, "policy_version": res.PolicyVersion,
			"policy_fingerprint":   policyFingerprint,
			"terminal_authorities": terminalAuthorities,
			"identity_authorities": identityAuthorities,
		}
		if enforceBinding {
			entry["account"] = binding.scope.Account
			entry["mode"] = binding.scope.Mode
			entry["connector_epoch"] = binding.connectorEpoch
		}
		_ = enc.Encode(entry)
		if s.coreStore != nil {
			raw, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			version := int64(res.PolicyVersion)
			key := coreEventKey(coreEventRuleTransition, res.AsOf, raw, coreEventOrdinal+len(coreEvents))
			coreEvents = append(coreEvents, corestore.EventInput{
				ScopeKey: daemonStateScope, EventKey: key, Type: coreEventRuleTransition,
				Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
				OccurredAt: res.AsOf, PayloadJSON: raw,
				Projection: corestore.EventProjection{RuleTransition: &corestore.RuleTransitionProjection{
					RuleID: r.ID, Status: r.Status, PreviousStatus: prevStatus[r.ID],
					PolicyID: res.PolicyID, PolicyVersion: &version, PolicyFingerprint: policyFingerprint,
				}},
			})
		}
	}
	if s.coreStore != nil {
		if len(coreEvents) > 0 {
			if _, err := s.coreStore.AppendEvents(context.Background(), coreEvents); err != nil {
				s.warnf("rules transitions: SQLite append failed: %v", err)
			}
		}
		return
	}
	// rulesJournalMu is the writer-quiescence lock journal rotation
	// excludes: held across path resolve, open, write, and close so a
	// live-file swap can never interleave with an append. Line shape,
	// dedupe, and the 0o644 mode are untouched.
	s.rulesJournalMu.Lock()
	path, err := defaultTradingStatePath("rules-decisions.jsonl")
	if err != nil {
		s.rulesJournalMu.Unlock()
		return
	}
	if err := ensurePrivateStateDir(path); err != nil {
		s.rulesJournalMu.Unlock()
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.rulesJournalMu.Unlock()
		return
	}
	if buf.Len() > 0 {
		_, _ = f.Write(buf.Bytes())
	}
	_ = f.Close()
	s.rulesJournalMu.Unlock()
}

func errText(err error) string {
	if err == nil {
		return "unavailable"
	}
	return err.Error()
}

// rulebookPreviewWarnings maps currently breached rules to advisory
// DataWarnings on an order preview — only when the draft would WORSEN the
// breached metric. Reduce/close intents never warn; submit eligibility is
// never touched (advisory-only, internal-docs/design/trading-rulebook.md).
func rulebookPreviewWarnings(res *rpc.RulesResult, draft rpc.OrderDraft, position rpc.OrderPositionImpact) []rpc.DataWarning {
	if res == nil || !res.Enabled {
		return nil
	}
	var out []rpc.DataWarning
	if res.Status != "ok" {
		out = append(out, rpc.DataWarning{
			Code:     "rulebook_unavailable",
			Scope:    "rulebook",
			Severity: "warning",
			Message:  "The canonical Rulebook evaluation is unavailable or incomplete for this broker scope.",
			Impact:   "Rulebook causes may be missing from this advisory preview; submit eligibility is unaffected.",
			Action:   "Resolve the reported account, portfolio, earnings, regime, or tape input gap and preview again.",
		})
	}
	if len(res.Rules) == 0 {
		return out
	}
	switch position.Effect {
	case "close", "reduce":
		return out
	}
	sym := strings.ToUpper(draft.Contract.Symbol)
	isBuy := strings.EqualFold(draft.Action, "BUY")
	isOption := strings.EqualFold(draft.Contract.SecType, "OPT")
	rows := map[string]risk.RuleRow{}
	for _, r := range res.Rules {
		rows[r.ID] = r
	}
	breached := func(id string) (risk.RuleRow, bool) {
		r, found := rows[id]
		return r, found && (r.Status == risk.RuleStatusWatch || r.Status == risk.RuleStatusAct)
	}
	offends := func(r risk.RuleRow) bool {
		for _, o := range r.Offenders {
			if strings.EqualFold(o.Symbol, sym) {
				return true
			}
		}
		return false
	}
	warn := func(r risk.RuleRow, msg string) rpc.DataWarning {
		return rpc.DataWarning{
			Code:     "rule_" + r.ID,
			Scope:    "rulebook",
			Severity: r.Status,
			Message:  msg,
			Impact:   fmt.Sprintf("Advisory rulebook cause (rule %d, as of %s); submit eligibility is unaffected.", r.Number, res.AsOf.Format(time.RFC3339)),
			Action:   "Run `canary rules` for the full checklist.",
		}
	}
	if r, ok := breached(risk.RuleSingleNameExposure); ok && offends(r) {
		out = append(out, warn(r, fmt.Sprintf("%s already breaches the per-name exposure cap; this order increases it.", sym)))
	}
	if r, ok := breached(risk.RuleOptionLinePremium); ok && isBuy && isOption && offends(r) {
		out = append(out, warn(r, fmt.Sprintf("%s already holds an option line over the premium cap; this adds premium.", sym)))
	}
	if r, ok := breached(risk.RuleCashSellOnly); ok && isBuy {
		out = append(out, warn(r, "Cash ratio is below the sell-only floor; a buy deepens the margin debit."))
	}
	if r, ok := breached(risk.RuleExtrinsicBudget); ok && isBuy && isOption {
		out = append(out, warn(r, "Portfolio extrinsic already exceeds its budget; buying options adds nightly decay."))
	}
	if r, ok := breached(risk.RuleEarningsSizeFreeze); ok && offends(r) {
		out = append(out, warn(r, fmt.Sprintf("%s is oversized inside its pre-earnings freeze window; adding is the opposite of at-size.", sym)))
	}
	// Averaging down is the one behavior that resets rule 13's loss fence —
	// warn when the drafted option leg matches a line already past it. Rule
	// 14 (fx_exposure) deliberately has no preview cause: at structurally
	// high non-base exposure it would fire on every ordinary order, and a
	// warning with a 100% base rate trains the operator to ignore rule_*
	// causes entirely.
	if r, ok := breached(risk.RuleExitDiscipline); ok && isBuy && isOption {
		draftDesc := fmt.Sprintf("%s %s %s %s", sym,
			strings.ReplaceAll(draft.Contract.Expiry, "-", ""), strings.ToUpper(draft.Contract.Right), trimFloat(draft.Contract.Strike))
		for _, o := range r.Offenders {
			if strings.EqualFold(o.Leg, draftDesc) {
				out = append(out, warn(r, fmt.Sprintf("%s is already past the exit-discipline loss fence; averaging down resets the basis and hides the loss.", draftDesc)))
				break
			}
		}
	}
	return out
}
