package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The mandate verdict (internal-docs/design/mandate-risk-budget.md): the
// daemon gathers the capital state, one current positions read with its
// premiums and armed stops (mandate_loss.go) and its decision-grade series,
// runs mandate.Evaluate and keeps the result. The headline is the loss
// measure against the loss room (spec TM-04.12); the volatility sizes the
// sleeve allocations. The additions gate reads the kept verdict (never a
// broker read on the order path) and refuses a risk-adding order while it
// is not within, is older than mandateVerdictMaxAge, or the order itself
// would take the loss measure over the room, a gap over its cap or its
// sleeve over its allocation.

const (
	// mandateVerdictMaxAge is how old a verdict the gate still trusts.
	mandateVerdictMaxAge = 5 * time.Minute
	// mandateVerdictRefreshEvery is the background cadence; a read serves a
	// verdict younger than mandateVerdictServeWithin as it is.
	mandateVerdictRefreshEvery = 2 * time.Minute
	mandateVerdictServeWithin  = 30 * time.Second
	mandateBudgetCode          = rpc.MandateBudgetBlockerCode
	// mandateStreakSpacing is how far apart two scheduled verdicts must be to
	// count as consecutive for the forced reduction; mandateStreakMaxGap is
	// how long a reading may wait for the next before the streak starts
	// again. A read's own evaluation never counts: only the background
	// cadence, so polling cannot build the streak in seconds.
	mandateStreakSpacing = 5 * time.Minute
	mandateStreakMaxGap  = 15 * time.Minute
)

type mandateVerdictCache struct {
	mu          sync.Mutex
	result      *rpc.MandateRiskResult
	input       *mandate.Input
	evaluatedAt time.Time
	fingerprint string
	binding     daemonBrokerEvidenceBinding
	// overStreak counts consecutive scheduled evaluations reading over or
	// reduce, each at least mandateStreakSpacing and at most
	// mandateStreakMaxGap after the last one counted (streakAt), under one
	// policy (streakFP); the forced reduction starts at two, or at one on a
	// gap-cap breach (mandate_reduction.go). Any reading, scheduled or not,
	// that is neither over nor reduce resets it. scheduledAt is the last
	// scheduled evaluation, which keeps the cadence however often reads
	// evaluate.
	overStreak  int
	streakAt    time.Time
	streakFP    string
	scheduledAt time.Time
}

// mandateVerdictKept is one read of the cache.
type mandateVerdictKept struct {
	result      *rpc.MandateRiskResult
	input       *mandate.Input
	evaluatedAt time.Time
	fingerprint string
	binding     daemonBrokerEvidenceBinding
	overStreak  int
}

func (c *mandateVerdictCache) current() (*rpc.MandateRiskResult, *mandate.Input, time.Time, string) {
	k := c.kept()
	return k.result, k.input, k.evaluatedAt, k.fingerprint
}

func (c *mandateVerdictCache) kept() mandateVerdictKept {
	c.mu.Lock()
	defer c.mu.Unlock()
	return mandateVerdictKept{result: c.result, input: c.input, evaluatedAt: c.evaluatedAt, fingerprint: c.fingerprint, binding: c.binding, overStreak: c.overStreak}
}

// set keeps one evaluation; scheduled says it came from the background
// cadence rather than a read, and only those count toward the streak.
func (c *mandateVerdictCache) set(result *rpc.MandateRiskResult, input *mandate.Input, at time.Time, fingerprint string, scheduled bool, binding daemonBrokerEvidenceBinding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A breach reading: the book over or reduce, or the owner sleeve's stop
	// engaged with risk still to cut (mandate_sleeve_stop.go).
	breach := result != nil && result.Verdict != nil && (result.Verdict.Status == mandate.StatusOver || result.Verdict.Status == mandate.StatusReduce || mandateStopOver(*result.Verdict))
	switch {
	case !breach || !sameMandateBookSession(c.binding, binding) || (c.overStreak > 0 && fingerprint != c.streakFP):
		c.overStreak, c.streakAt, c.streakFP = 0, time.Time{}, ""
	}
	if breach && scheduled {
		gap := at.Sub(c.streakAt)
		switch {
		case c.overStreak > 0 && gap < mandateStreakSpacing:
			// Too soon after the last counted reading: it neither counts nor
			// resets.
		case c.overStreak > 0 && gap <= mandateStreakMaxGap:
			c.overStreak, c.streakAt = c.overStreak+1, at
		default:
			c.overStreak, c.streakAt, c.streakFP = 1, at, fingerprint
		}
	}
	if scheduled {
		c.scheduledAt = at
	}
	c.result, c.input, c.evaluatedAt, c.fingerprint = result, input, at, fingerprint
	c.binding = binding
}

// captureMandateBookBinding pins the book's account, connector publication,
// socket and structural portfolio generation before evaluation. Order inventory
// refreshes are reads and can advance the order callback frontier themselves;
// the mandate binds the position generation, not that refresh frontier.
func (s *Server) captureMandateBookBinding() daemonBrokerEvidenceBinding {
	b := s.currentProtectionOrderSnapshotBinding()
	out := daemonBrokerEvidenceBinding{scope: b.scope, connector: b.connector, connectorEpoch: b.connectorEpoch}
	if b.connector != nil {
		// Consumers also run inside the first-byte guard, which already
		// owns the broker evidence barrier. These accessors do not reenter it.
		out.broker.Session = b.session
		out.broker.PortfolioProjectionGeneration = b.connector.PortfolioProjectionGeneration()
	}
	return out
}

func sameMandateBookSession(a, b daemonBrokerEvidenceBinding) bool {
	return sameBrokerScope(a.scope, b.scope) && a.connector == b.connector &&
		a.connectorEpoch == b.connectorEpoch && a.broker.Session == b.broker.Session
}

func (s *Server) mandateBookBindingCurrent(binding daemonBrokerEvidenceBinding) bool {
	current := s.captureMandateBookBinding()
	return sameMandateBookSession(binding, current) && binding.broker == current.broker &&
		(binding.connector == nil || binding.connector.SessionCurrent(binding.broker.Session))
}

// keepMandateVerdict publishes only while the captured book still owns the
// broker frontier. A slow old evaluation cannot displace a successor's cache.
func (s *Server) keepMandateVerdict(binding daemonBrokerEvidenceBinding, result *rpc.MandateRiskResult, input *mandate.Input, at time.Time, fingerprint string, scheduled bool) bool {
	keep := func() error {
		s.mandateVerdicts.set(result, input, at, fingerprint, scheduled, binding)
		return nil
	}
	if binding.connector != nil {
		latest, ok := binding.connector.CaptureBrokerEvidence()
		if !ok || latest.Session != binding.broker.Session || latest.PortfolioProjectionGeneration != binding.broker.PortfolioProjectionGeneration {
			return false
		}
		// The existing publication barrier also pins the current order frontier
		// during the commit; only the position frontier belongs to the cache.
		commitBinding := binding
		commitBinding.broker = latest
		kept, _ := s.withStableBrokerEvidence(commitBinding, keep)
		return kept
	}
	// An unconnected evaluation (including pure fixtures) has no broker
	// frontier. Keep its exact daemon identity under the publication lock.
	s.mu.Lock()
	defer s.mu.Unlock()
	configuredAccount, port := "", s.endpoint.Port
	if s.cfg != nil {
		configuredAccount = s.cfg.Gateway.Account
		if port == 0 && s.cfg.Gateway.Port != nil {
			port = *s.cfg.Gateway.Port
		}
	}
	scope := brokerStateScopeFromSnapshot(configuredAccount, s.endpoint.Account, port, "")
	if s.connector != nil || s.connectorEpoch != binding.connectorEpoch || !sameBrokerScope(scope, binding.scope) {
		return false
	}
	_ = keep()
	return true
}

// keptMandateVerdict validates the authority on every consumer read. The zero
// identity used by pure fixtures matches only that exact unconnected identity;
// it never exempts a changed account, connector epoch or socket from validation.
func (s *Server) keptMandateVerdict() mandateVerdictKept {
	kept := s.mandateVerdicts.kept()
	if !s.mandateBookBindingCurrent(kept.binding) {
		return mandateVerdictKept{}
	}
	return kept
}

func (s *Server) currentMandateVerdict() (*rpc.MandateRiskResult, *mandate.Input, time.Time, string) {
	k := s.keptMandateVerdict()
	return k.result, k.input, k.evaluatedAt, k.fingerprint
}

// lastScheduled is the time of the last scheduled evaluation.
func (c *mandateVerdictCache) lastScheduled() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scheduledAt
}

// mandateVerdictFingerprint names one verdict for the rows sized on it: the
// evaluation time, the status, the risk and budget figures, the policy and
// the book's leg quantities. Two evaluations of the same book at the same
// instant share it; any other reading has its own.
func mandateVerdictFingerprint(res *rpc.MandateRiskResult) string {
	if res == nil || res.Verdict == nil {
		return ""
	}
	v := res.Verdict
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|", v.AsOf.UTC().Format(time.RFC3339Nano), v.Status)
	if v.Risk != nil {
		fmt.Fprintf(&b, "%.6f|", v.Risk.SDAnnualBase)
	}
	fmt.Fprintf(&b, "%.6f|", v.Capital.BudgetAnnualSD)
	if res.PolicyFingerprint != nil {
		b.WriteString(res.PolicyFingerprint.Key)
	}
	for _, l := range v.Legs {
		fmt.Fprintf(&b, "|%s=%g", l.ID, l.Quantity)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:6])
}

// mandateInactiveResult is the payload while the constitution carries no
// [mandate] table: nothing is measured and the gate is inactive.
func mandateInactiveResult(now time.Time, mgr riskPolicySnapshot) *rpc.MandateRiskResult {
	msg := "the risk constitution carries no [mandate] table; the mandate budget is inactive"
	if mgr.policy == nil {
		msg = "no risk constitution is in force; the mandate budget is inactive"
	}
	return &rpc.MandateRiskResult{AsOf: now, Active: false, Message: msg, Gate: &rpc.MandateGateView{}}
}

// refreshMandateVerdict re-evaluates on the background cadence; it never
// reads the account, which the ordinary account reads keep observed.
func (s *Server) refreshMandateVerdict(ctx context.Context) {
	if s.riskPolicies == nil {
		return
	}
	c := s.riskPolicies.snapshot().policy
	if !c.MandateActive() {
		s.mandateVerdicts.set(nil, nil, time.Time{}, "", true, daemonBrokerEvidenceBinding{})
		return
	}
	// A revised policy is judged at once; otherwise the cadence decides. A
	// read's evaluation does not move it: the forced reduction counts only
	// scheduled verdicts.
	if _, _, _, fp := s.currentMandateVerdict(); fp == c.FingerprintKey() {
		if last := s.mandateVerdicts.lastScheduled(); !last.IsZero() && s.nowUTC().Sub(last) < mandateVerdictRefreshEvery {
			return
		}
	}
	s.evaluateMandate(ctx, false)
}

// handleMandateRisk serves the daemon-owned verdict: a verdict younger than
// mandateVerdictServeWithin as it is, else a fresh evaluation after an
// account read, so the capital state is observed before it is judged.
func (s *Server) handleMandateRisk(ctx context.Context, req *rpc.Request) (*rpc.MandateRiskResult, error) {
	var p rpc.MandateRiskParams
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
	}
	now := s.nowUTC()
	mgr := s.riskPolicies.snapshot()
	if !mgr.policy.MandateActive() {
		return mandateInactiveResult(now, mgr), nil
	}
	if result, _, at, fp := s.currentMandateVerdict(); !p.Refresh && result != nil && fp == mgr.policy.FingerprintKey() && now.Sub(at) < mandateVerdictServeWithin {
		out := *result
		out.Gate = s.mandateGateView(&out, now)
		out.Reduction = s.mandateReductionView()
		return &out, nil
	}
	res := s.evaluateMandate(ctx, true)
	res.Reduction = s.mandateReductionView()
	return res, nil
}

// evaluateMandate runs one evaluation and keeps it. With refreshAccount the
// account is read first (which observes equity into the capital state): a
// read's evaluation. Without it, the evaluation is the scheduled one, the
// only kind the forced reduction's streak counts.
func (s *Server) evaluateMandate(ctx context.Context, refreshAccount bool) *rpc.MandateRiskResult {
	mgr := s.riskPolicies.snapshot()
	c := mgr.policy
	if !c.MandateActive() {
		res := mandateInactiveResult(s.nowUTC(), mgr)
		s.mandateVerdicts.set(nil, nil, time.Time{}, "", !refreshAccount, daemonBrokerEvidenceBinding{})
		return res
	}
	if refreshAccount {
		acctCtx, cancel := requestCtx(ctx, rpc.MethodAccountSummary)
		_, _ = s.handleAccountSummary(acctCtx)
		cancel()
	}
	binding := s.captureMandateBookBinding()
	scope := binding.scope
	base := normCcy(c.Capital.BaseCurrency)
	// The verdict's clock is read once the capital state is in hand, so it is
	// never older than the equity observation it is judged on.
	capital, _ := s.riskCapital.MandateCapitalForScope(c, scope, s.nowUTC())
	now := s.nowUTC()
	in := mandate.Input{Now: now, BaseCurrency: base, Policy: mandatePolicyNumbers(c), Capital: capital,
		Series: map[string]mandate.Series{}, FX: map[string]mandate.Series{}, FXNow: map[string]float64{}}
	if unapproved := c.Mandate.UnapprovedKeys(); len(unapproved) > 0 {
		in.Reasons = append(in.Reasons, "mandate keys are unwritten: "+strings.Join(unapproved, ", "))
	}
	if base == "" {
		in.Reasons = append(in.Reasons, "capital.base_currency is unapproved")
	}
	evidence := &rpc.MandateEvidence{EvaluatedAt: now, EquityAsOf: capital.EquityAsOf}
	pos, err := s.mandatePositions(ctx)
	switch {
	case err != nil:
		in.Reasons = append(in.Reasons, "positions could not be read: "+errText(err))
	case pos == nil || !currentPortfolioAuthority(pos.Authority):
		in.Reasons = append(in.Reasons, "the positions read is not current for the selected account")
	case brokerScopeConcrete(scope) && !strings.EqualFold(strings.TrimSpace(pos.Authority.Scope.AccountID), strings.TrimSpace(scope.Account)):
		in.Reasons = append(in.Reasons, "the positions read belongs to another account than the selected one")
	case pos.Portfolio != nil && pos.Portfolio.BaseCurrency != "" && base != "" && normCcy(pos.Portfolio.BaseCurrency) != base:
		in.Reasons = append(in.Reasons, fmt.Sprintf("the account base currency %s differs from capital.base_currency %s", pos.Portfolio.BaseCurrency, base))
	}
	if pos != nil {
		evidence.PositionsAsOf = pos.AsOf
		var reasons []string
		in.Legs, in.NotModelled, in.FXNow, reasons = mandateLegsFromPositions(pos, base, now)
		// The share lines' stops, for the loss measure (mandate_loss.go).
		stops, stopsWhy := s.mandateArmedStops(ctx, pos, scope)
		var stopNote string
		in.Legs, stopNote = mandateApplyStops(in.Legs, pos, base, stops, stopsWhy)
		if stopNote != "" {
			in.NotModelled = append(in.NotModelled, stopNote)
		}
		in.LineISINs = s.registerFundLines(ctx)
		in.Reasons = append(in.Reasons, reasons...)
		for _, need := range mandateHistoryNeeds(pos, base, in.LineISINs) {
			if need.Ambiguous {
				in.Reasons = append(in.Reasons, "daily history for "+need.Contract.Symbol+" is ambiguous across distinct contracts")
			}
			series := s.mandateHistoryForNeed(ctx, need)
			row := rpc.MandateSeriesEvidence{Key: need.Key, Missing: series == nil}
			if series != nil {
				row.Closes, row.LastClose, row.FetchedAt, row.Source = len(series.Closes), series.Dates[len(series.Dates)-1], series.FetchedAt, series.Source
				row.Stale = mandateSeriesStale(series, now)
				conv := mandateSeriesInput(series, now)
				if ccy, ok := strings.CutPrefix(need.Key, "fx:"); ok {
					in.FX[ccy] = conv
				} else {
					in.Series[strings.TrimPrefix(need.Key, "u:")] = conv
				}
			}
			evidence.Series = append(evidence.Series, row)
		}
	}
	// The allocation plan follows the resolved strategy register; it reads
	// the legs, since a live strategy on hold keeps its reservation only
	// while its sleeve holds positions.
	resolved := s.resolvedRegister(c)
	// A register entry's young line is measured on its signal series' returns
	// until its own closes fill the history (strategy_signal.go).
	s.withStrategyRiskSeries(ctx, resolved, &in, evidence, now)
	placement := s.mandateStrategyPlacement(ctx, c, resolved, in.Legs)
	in.Legs = placement.Legs
	in.Allocations = mandateAllocationPlanPlaced(c, resolved, placement.Placed, in.Legs)
	if !s.mandateBookBindingCurrent(binding) {
		in.Reasons = append(in.Reasons, "the broker account, session or positions changed during the mandate measurement")
	}
	verdict := mandate.Evaluate(in)
	// The owner sleeve's drawdown stop reads this verdict's legs and use, and
	// the kept input carries it, so every evaluation sized or judged on this
	// book (the forced reduction, the gate) reads the same stop.
	if stopPct, ok := c.Mandate.OwnerStopPct(); ok {
		stop := s.riskCapital.ObserveOwnerSleeveStop(c, scope, stopPct, capital, &verdict, in.Allocations, now)
		in.OwnerStop = &stop
		mandate.ApplySleeveStop(&verdict, in.OwnerStop)
	}
	fp := c.FingerprintKey()
	res := &rpc.MandateRiskResult{AsOf: now, Active: true, Verdict: &verdict, Evidence: evidence,
		PolicyFingerprint: &rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: fp},
		Policy:            mandatePolicyView(c.Mandate)}
	if pos != nil && currentPortfolioAuthority(pos.Authority) {
		res.Exposure = s.mandateExposure(pos, &verdict, base, now)
	}
	// Recheck after all derived reads: never publish an old book as a
	// measurable result under the successor session. The stored binding
	// remains the one captured before the read, so later consumers reject it.
	if !s.mandateBookBindingCurrent(binding) {
		if verdict.Status != mandate.StatusUnknown {
			in.Reasons = append(in.Reasons, "the broker account, session or positions changed during the mandate measurement")
			verdict = mandate.Evaluate(in)
		}
		res.Exposure = nil
	}
	res.Gate = s.mandateGateView(res, now)
	res.Register = mandateRegisterView(resolved)
	res.StatusSince = s.mandateStatusSince(ctx, verdict.Status, now)
	if !s.keepMandateVerdict(binding, res, &in, now, fp, !refreshAccount) {
		if verdict.Status != mandate.StatusUnknown {
			in.Reasons = append(in.Reasons, "the broker account, session or positions changed during the mandate measurement")
			verdict = mandate.Evaluate(in)
		}
		res.Exposure = nil
		res.Gate = s.mandateGateView(res, now)
	}
	return res
}

func mandatePolicyView(m *risk.ConstitutionMandate) *rpc.MandatePolicyView {
	if m == nil {
		return nil
	}
	v := &rpc.MandatePolicyView{MaxLossPct: policyCheckDeref(m.MaxLossPct), VolTargetPct: policyCheckDeref(m.VolTargetPct), TaxReserveBase: policyCheckDeref(m.TaxReserveBase),
		StartedAt: m.StartedAt, GapShockPct: policyCheckDeref(m.GapShockPct), IndexGapShockPct: m.IndexGapShockPct, GapRoomPct: policyCheckDeref(m.GapRoomPct), Unapproved: m.UnapprovedKeys()}
	if m.Sleeves != nil {
		v.Sleeves = &rpc.MandateSleevesView{OwnerVolPct: m.Sleeves.OwnerVolPct, CoreVolPct: m.Sleeves.CoreVolPct, OverlayVolPct: m.Sleeves.OverlayVolPct, OwnerStopPct: m.Sleeves.OwnerStopPct}
	}
	return v
}

// mandateLegsFromPositions maps one positions read to the evaluator's legs.
// Bills, bonds and conversions are listed as not modelled; a stale row, a
// row without a price, spot or FX rate, and any other security type are
// reasons for an unknown verdict, named per line.
func mandateLegsFromPositions(pos *rpc.PositionsResult, base string, now time.Time) (legs []mandate.Leg, notModelled []string, fxNow map[string]float64, reasons []string) {
	fxNow = map[string]float64{}
	noteFX := func(row rpc.PositionView) {
		ccy := normCcy(row.Currency)
		if ccy == "" || ccy == base {
			return
		}
		if rate, ok := positionBaseRate(row, base); ok {
			fxNow[ccy] = rate
		}
	}
	legID := mandateLegID
	for _, row := range pos.Stocks {
		secType := strings.ToUpper(strings.TrimSpace(row.SecType))
		switch {
		case secType == "BILL" || secType == "BOND":
			notModelled = append(notModelled, fmt.Sprintf("%s (%s, no equity risk modelled)", row.Symbol, strings.ToLower(secType)))
			continue
		case secType == "CASH":
			notModelled = append(notModelled, fmt.Sprintf("%s (currency conversion)", row.Symbol))
			continue
		case positionWarningHasCode(row.WarningDetails, zeroValueStockPositionCode):
			notModelled = append(notModelled, fmt.Sprintf("%s (zero-value row, likely inactive)", row.Symbol))
			continue
		}
		if positionIsIndexFuture(row) {
			// A held Micro S&P future is the hedge's own line (owner
			// decisions 2026-10-28 13:17 and 20:07 CET): a linear leg at its
			// multiplier under the MES FUT key, moved by the index.
			if row.Stale {
				reasons = append(reasons, fmt.Sprintf("%s has a stale quote (%s)", mandateFutureSymbol(row), nonEmptyString(row.StaleReason, "no reason given")))
			}
			noteFX(row)
			legs = append(legs, mandate.Leg{ID: legID(row), Symbol: mandateFutureSymbol(row), Underlying: mandate.MicroSP500Future, Kind: mandate.KindStock,
				Quantity: row.Quantity, Multiplier: float64(row.Multiplier), Currency: normCcy(row.Currency), Spot: row.Mark})
			continue
		}
		if !positionQuotesAsStock(row) {
			reasons = append(reasons, fmt.Sprintf("%s carries security type %s, which the mandate does not model", row.Symbol, secType))
			continue
		}
		if row.Stale {
			reasons = append(reasons, fmt.Sprintf("%s has a stale quote (%s)", row.Symbol, nonEmptyString(row.StaleReason, "no reason given")))
		}
		noteFX(row)
		legs = append(legs, mandate.Leg{ID: legID(row), Symbol: normSym(row.Symbol), Underlying: normSym(row.Symbol), Kind: mandate.KindStock,
			Quantity: row.Quantity, Multiplier: 1, Currency: normCcy(row.Currency), Spot: row.Mark})
	}
	for _, row := range pos.Options {
		if row.Stale {
			reasons = append(reasons, fmt.Sprintf("%s %s %g %s has a stale quote (%s)", row.Symbol, row.Expiry, row.Strike, row.Right, nonEmptyString(row.StaleReason, "no reason given")))
		}
		noteFX(row)
		spot := 0.0
		if row.Underlying != nil && *row.Underlying > 0 {
			spot = *row.Underlying
		} else if row.PrevClose != nil && *row.PrevClose > 0 {
			spot = *row.PrevClose
		}
		leg := mandate.Leg{ID: legID(row), Symbol: fmt.Sprintf("%s %s %g %s", normSym(row.Symbol), row.Expiry, row.Strike, strings.ToUpper(row.Right)),
			Underlying: normSym(row.Symbol), Kind: mandate.KindOption, Quantity: row.Quantity, Multiplier: float64(optionMultiplier(row)),
			Currency: normCcy(row.Currency), Spot: spot, Right: strings.ToUpper(strings.TrimSpace(row.Right)), Strike: row.Strike,
			TimeToExpiryYears: mandateYearsToExpiry(row.Expiry, now), IV: row.IV}
		mandateOptionLoss(&leg, row, base)
		mandateFallbackIV(&leg, row)
		legs = append(legs, leg)
	}
	return legs, notModelled, fxNow, reasons
}

// positionIsFuture reports a held futures row, as the positions spell it.
func positionIsFuture(row rpc.PositionView) bool {
	switch strings.ToUpper(strings.TrimSpace(row.SecType)) {
	case rpc.SecTypeFuture, "FUT":
		return true
	}
	return false
}

// positionIsIndexFuture reports a held Micro E-mini S&P 500 future: the
// hedge's contract root in USD. Any other future stays unmodelled.
func positionIsIndexFuture(row rpc.PositionView) bool {
	return positionIsFuture(row) && normSym(row.Symbol) == hedgeFutureSymbol && normCcy(row.Currency) == "USD"
}

// mandateFutureSymbol names a future's line as the owner knows it: its
// local symbol (MESZ6), else root and month.
func mandateFutureSymbol(row rpc.PositionView) string {
	if sym := strings.ToUpper(strings.TrimSpace(row.LocalSymbol)); sym != "" {
		return sym
	}
	return strings.TrimSpace(normSym(row.Symbol) + " " + row.Expiry)
}

// mandateFallbackIV prices an option the position row carries no volatility
// for at the volatility its own mark implies, tagged as such with the mark's
// time (owner instruction 2026-10-28 08:51 CET). Before a session's first
// model tick, after a daemon restart or in another market's hours the row has
// none, and one such leg used to leave the whole book unknown. The evaluator
// counts the tag only under the owner's [mandate.iv_fallback]; a mark it
// cannot invert (none, or at or below intrinsic) leaves the leg as it was.
func mandateFallbackIV(leg *mandate.Leg, row rpc.PositionView) {
	if leg.IV != nil || leg.Spot <= 0 || leg.Strike <= 0 || leg.TimeToExpiryYears <= 0 || (leg.Right != "C" && leg.Right != "P") {
		return
	}
	iv, ok := mandate.ImpliedVolatility(leg.Right == "C", row.Mark, leg.Spot, leg.Strike, leg.TimeToExpiryYears)
	if !ok {
		return
	}
	leg.IV, leg.IVSource = &iv, mandate.IVSourceMark
	leg.IVAsOf = row.PriceAt
	if leg.IVAsOf.IsZero() {
		leg.IVAsOf = row.QuotePriceAt
	}
}

// mandateLegID identifies a position row as the evaluator's leg: the exact
// contract id, or the identity fields when the row carries none.
func mandateLegID(row rpc.PositionView) string {
	if row.ConID > 0 {
		return fmt.Sprintf("con:%d", row.ConID)
	}
	return "sym:" + normSym(row.Symbol) + ":" + normCcy(row.Currency) + ":" + row.Expiry + ":" + row.Right + ":" + fmt.Sprint(row.Strike)
}

// mandateYearsToExpiry measures to the close of the expiry day in New York;
// an unreadable expiry reads zero, so the leg prices at intrinsic.
func mandateYearsToExpiry(expiryYMD string, now time.Time) float64 {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.UTC
	}
	day, err := time.ParseInLocation("20060102", strings.TrimSpace(expiryYMD), ny)
	if err != nil {
		return 0
	}
	close := time.Date(day.Year(), day.Month(), day.Day(), 16, 0, 0, 0, ny)
	return math.Max(0, close.Sub(now).Hours()/24/365)
}

// mandateGateView says how the gate reads a result right now.
func (s *Server) mandateGateView(res *rpc.MandateRiskResult, now time.Time) *rpc.MandateGateView {
	if res == nil || !res.Active || res.Verdict == nil {
		return &rpc.MandateGateView{}
	}
	if age := now.Sub(res.AsOf); age > mandateVerdictMaxAge {
		reason := fmt.Sprintf("the verdict is %s old; the gate trusts one up to %s", formatQuoteAge(age), mandateVerdictMaxAge)
		return &rpc.MandateGateView{RefusesAdditions: true, Reason: reason, Options: &rpc.MandateOptionsGateView{RefusesAdditions: true, Reason: reason}}
	}
	ccy := res.Verdict.BaseCurrency
	view := mandateVolatilityGateView(res.Verdict, ccy)
	// Option additions read the premium limits; the volatility verdict is
	// their warning (owner decision 2026-10-23 18:22 CEST).
	view.Options = mandateOptionsGateView(res, view)
	// The owner's override lifts the volatility gate and the premium limits;
	// the holds that bound a loss stay (mandate_options_gate.go).
	if override := s.mandateAdditionsOverride(now); override != nil {
		view.Override = override
		if hard := mandateHardHolds(res.Verdict, mandate.SleeveOwner, ccy, ""); hard != nil {
			view.RefusesAdditions, view.Reason = true, strings.TrimPrefix(hard.Message, "Mandate budget: ")
		} else {
			view.RefusesAdditions, view.Reason = false, mandateOverrideSentence(override, res.Verdict, ccy)
		}
		// The override lifts the premium limits too; option buys share the
		// holds that bound a loss.
		view.Options = &rpc.MandateOptionsGateView{RefusesAdditions: view.RefusesAdditions, Reason: view.Reason}
	}
	return view
}

// mandateVolatilityGateView reads the volatility budget's gate for the
// owner's stock and ETF additions.
func mandateVolatilityGateView(v *mandate.Verdict, ccy string) *rpc.MandateGateView {
	switch v.Status {
	case mandate.StatusWithin:
	case mandate.StatusOver, mandate.StatusReduce:
		// Over budget, your sleeve keeps adding while it is inside its own
		// allocation (owner, 2026-10-18 15:31 CEST).
		if why := mandateOverBudgetHolds(v, mandate.SleeveOwner, ccy); why != "" {
			return &rpc.MandateGateView{RefusesAdditions: true, Reason: mandateStatusSentence(v, ccy) + "; " + why}
		}
		if why := mandateOwnerStopHolds(v, mandate.SleeveOwner, ccy); why != "" {
			return &rpc.MandateGateView{RefusesAdditions: true, Reason: mandateStatusSentence(v, ccy) + "; " + why}
		}
		return &rpc.MandateGateView{Reason: mandateStatusSentence(v, ccy) + ", but your sleeve is inside its allocation: an addition that keeps it there passes"}
	default:
		return &rpc.MandateGateView{RefusesAdditions: true, Reason: mandateStatusSentence(v, ccy)}
	}
	// Your sleeve's own drawdown stop holds your additions while engaged.
	if why := mandateOwnerStopHolds(v, mandate.SleeveOwner, ccy); why != "" {
		return &rpc.MandateGateView{RefusesAdditions: true, Reason: why}
	}
	// Within the budget, your additions still stop at your allocation.
	if allocation, ok := v.Allocation(mandate.SleeveOwner); ok {
		if over, alone := mandateSleeveOver(allocation); over {
			return &rpc.MandateGateView{RefusesAdditions: true, Reason: mandateAllocationOverSentence(mandate.SleeveOwner, allocation, mandateMachineAllocations(*v), "", mandateSleeveUse(allocation, alone), alone, v.BaseCurrency)}
		}
	}
	return &rpc.MandateGateView{}
}

// mandateStatusSentence states a verdict in one sentence for a refusal.
func mandateStatusSentence(v *mandate.Verdict, ccy string) string {
	switch v.Status {
	case mandate.StatusUnknown:
		why := "no reason given"
		if len(v.Reasons) > 0 {
			why = v.Reasons[0]
		}
		return "the mandate risk cannot be measured: " + why
	case mandate.StatusOver, mandate.StatusReduce:
		var parts []string
		if l := v.Loss; l != nil && (l.TotalBase > l.RoomBase || v.Capital.Known && !(v.Capital.Room > 0)) {
			parts = append(parts, mandateLossSentence(l, ccy))
		} else if l == nil && v.Risk != nil {
			// A verdict kept by an older daemon: the volatility was its headline.
			parts = append(parts, fmt.Sprintf("the mandate risk %s a year is over the budget %s", mandateMoney(v.Risk.SDAnnualBase, ccy), mandateMoney(v.Capital.BudgetAnnualSD, ccy)))
		}
		if v.Stresses != nil && len(v.Stresses.GapBreaches) > 0 {
			parts = append(parts, "a gap in "+strings.Join(v.Stresses.GapBreaches, ", ")+" would lose more than the gap cap "+mandateMoney(v.Stresses.GapCapBase, ccy))
		}
		if len(parts) == 0 {
			return "the mandate reads " + v.Status
		}
		return strings.Join(parts, "; ")
	}
	return "the loss measure is within the mandate's maximum loss"
}

// mandateLossSentence states the loss measure against the loss room: "the
// loss measure 140,000 EUR (premium at risk 120,000 EUR, stop loss 20,000
// EUR) is over the mandate's loss room 69,000 EUR (203%)".
func mandateLossSentence(l *mandate.LossMeasure, ccy string) string {
	sentence := fmt.Sprintf("the loss measure %s (premium at risk %s, stop loss %s) is over the mandate's loss room %s", mandateMoney(l.TotalBase, ccy), mandateMoney(l.PremiumBase, ccy), mandateMoney(l.StopLossBase, ccy), mandateMoney(l.RoomBase, ccy))
	if l.RoomBase > 0 && !math.IsInf(l.RatioToRoom, 0) {
		sentence += fmt.Sprintf(" (%.0f%%)", l.RatioToRoom*100)
	} else if !(l.RoomBase > 0) {
		sentence += " (the loss has reached the mandate's maximum, so no loss room is left)"
	}
	return sentence
}

func mandateMoney(v float64, ccy string) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "unmeasurable"
	}
	return policyCheckMoney(math.Round(v), ccy)
}

// riskAddingOrder is the one classification the drawdown brake and the
// mandate gate share: a close or reduce never adds risk (a BUY that closes
// or shrinks a held index hedge included), a BUY of a put on a Rulebook
// hedge symbol or one that protects shares held (protectivePut) is a hedge
// entry that stays open, and a SELL of the index future is a reduction when
// indexHedgeReduces says so; everything else adds risk. Cancels never reach
// either gate. The mandate gate alone also passes a bill purchase
// (mandateBillBuy).
func (s *Server) riskAddingOrder(draft rpc.OrderDraft, position rpc.OrderPositionImpact) bool {
	switch position.Effect {
	case rpc.OrderPositionEffectClose, rpc.OrderPositionEffectReduce:
		return false
	}
	if strings.EqualFold(draft.Action, "BUY") && strings.EqualFold(draft.Contract.SecType, "OPT") && strings.EqualFold(draft.Contract.Right, "P") &&
		(s.rulebookPolicy().IsHedgeSymbol(draft.Contract.Symbol) || s.protectivePut(draft)) {
		return false
	}
	if strings.EqualFold(draft.Action, rpc.OrderActionSell) && isIndexFutureContract(draft.Contract) {
		return !s.indexHedgeReduces(draft)
	}
	return true
}

// protectivePut reports a put BUY that protects shares the book holds
// (owner, 2026-10-29 14:28 CET: protective puts must stay open whatever the
// premium limits read). On the kept mandate book, the long puts on the name
// with this order cover no more shares than the book holds long; beyond
// them a put is a bet on a fall. Without a current measurement of the book
// it is not protection, so the gates judge it as an addition. Its premium
// still counts in the loss measure and the premium figures once held.
func (s *Server) protectivePut(draft rpc.OrderDraft) bool {
	if !(draft.Quantity > 0) {
		return false
	}
	shares, covered, ok := s.heldPutCover(draft.Contract.Symbol)
	mult := float64(draft.Contract.Multiplier)
	if mult <= 0 {
		mult = 100
	}
	return ok && shares > 0 && covered+float64(draft.Quantity)*mult <= shares+1e-9
}

// heldPutCover counts, on the kept mandate book, the shares of a name held
// long and the shares its long puts cover; ok is false when the book cannot
// be read. The protective put gate and Add's put cover read it alike.
func (s *Server) heldPutCover(symbol string) (shares, covered float64, ok bool) {
	input, _, _ := s.mandateCurrentInput()
	if input == nil {
		return 0, 0, false
	}
	sym := normSym(symbol)
	for _, l := range input.Legs {
		if normSym(l.Underlying) != sym || !(l.Quantity > 0) {
			continue
		}
		switch {
		case l.Kind == mandate.KindStock:
			shares += l.Quantity
		case l.Kind == mandate.KindOption && strings.EqualFold(l.Right, "P"):
			covered += l.Quantity * l.Multiplier
		}
	}
	return shares, covered, true
}

// addPutCover is what Add's put picker may still offer on the name: the
// contracts of 100 shares the protective put gate would still accept.
func (s *Server) addPutCover(symbol string) *rpc.AddPutCover {
	shares, covered, ok := s.heldPutCover(symbol)
	if !ok {
		return nil
	}
	return &rpc.AddPutCover{HeldShares: shares, CoveredShares: covered, RoomContracts: int(max(0, math.Floor((shares-covered+1e-9)/100)))}
}

// indexHedgeReduces classifies a SELL of the index future (owner decision
// 2026-10-28 20:07 CET; design note index-hedge.md, "gate
// classification"). On the kept mandate book with the order added it is a
// reduction only when the 10% index fall's loss is lower than without it
// and not below zero (the hedge never turns the book into a bet on a
// fall), the loss measure does not rise and stays within the loss room,
// and it adds or deepens no gap-cap breach: the index gap nets the hedge
// against the funds it hedges, so a breach it lessens does not count
// against it (owner decision "1. A", 2026-10-30 09:41 CET). Otherwise, or
// when the book cannot be measured now, the normal gate applies.
//
// The hedge's loss in a 10% rise spends loss room (TM-04.12). A sale
// increasing that measure remains an addition and must pass the normal
// drawdown, mandate and sleeve gates even when its loss fits the room.
func (s *Server) indexHedgeReduces(draft rpc.OrderDraft) bool {
	input, _, _ := s.mandateCurrentInput()
	if input == nil {
		return false
	}
	leg, ok := mandateOrderLeg(draft, input)
	if !ok {
		return false
	}
	before, after := mandate.Evaluate(*input), mandate.Evaluate(mandateInputWithLegs(*input, []mandate.Leg{leg}))
	if before.Stresses == nil || after.Stresses == nil || before.Loss == nil || after.Loss == nil {
		return false
	}
	fall := after.Stresses.IndexLossBase
	return fall < before.Stresses.IndexLossBase-1e-9 && fall >= 0 &&
		after.Loss.TotalBase <= before.Loss.TotalBase+1e-9 &&
		after.Loss.TotalBase <= after.Loss.RoomBase+1e-9 && !gapBreachAdded(&before, after)
}

// gapBreachAdded is mandateGapBreachAdded's yes or no.
func gapBreachAdded(before *mandate.Verdict, with mandate.Verdict) bool {
	_, added := mandateGapBreachAdded(before, with)
	return added
}

// mandateBillBuy reports a BUY of a government bill (security type BILL).
// The mandate measures a held bill as carrying no equity or volatility
// risk, so buying one moves cash into a cash-like line and adds none the
// budget counts (owner, 2026-10-17 06:47 CEST). A bond stays risk-adding:
// its interest-rate risk is not measured, so a long bond could carry real
// risk counted as zero. The drawdown brake does not read this.
func mandateBillBuy(draft rpc.OrderDraft) bool {
	return strings.EqualFold(strings.TrimSpace(draft.Action), rpc.OrderActionBuy) &&
		strings.EqualFold(strings.TrimSpace(draft.Contract.SecType), ibkrlib.SecTypeBill)
}

// mandateBudgetGate is the additions gate at every enforcement point the
// drawdown brake has. While the constitution carries no [mandate] table it
// refuses nothing. Otherwise a risk-adding order other than a bill purchase
// is refused unless a kept verdict no older than mandateVerdictMaxAge reads
// within, and the order itself, re-evaluated on the same book, keeps the
// risk inside the budget and its own sleeve inside its allocation (mandate_allocations.go).
// An option buy is judged by the premium limits instead and reads the
// volatility verdict as a warning (owner decision 2026-10-23 18:22 CEST,
// mandate_options_gate.go); an active override of mandate.vol_target_pct
// lifts the volatility gate and the premium limits for the owner's
// additions. Cheap by
// construction: the kept verdict and a simulation of milliseconds, never a
// broker read.
func (s *Server) mandateBudgetGate(draft rpc.OrderDraft, position rpc.OrderPositionImpact) *rpc.TradingBlocker {
	blocker, _ := s.mandateBudgetJudgement(draft, position)
	return blocker
}

// mandateBudgetJudgement is the gate with the warning an order passes with:
// the volatility verdict an option buy adds risk against, or the override
// that lifts it. The preview carries the warning; admission and the wire
// guard read the blocker alone.
func (s *Server) mandateBudgetJudgement(draft rpc.OrderDraft, position rpc.OrderPositionImpact) (*rpc.TradingBlocker, *rpc.DataWarning) {
	if s == nil || s.riskPolicies == nil || s.riskCapital == nil {
		return nil, nil
	}
	const open = " Reductions, closes, cancels and hedges stay open."
	const action = "Reduce or close instead, or wait for the verdict to read within; `canary mandate` has the measurement."
	c := s.riskPolicies.snapshot().policy
	// A machine sleeve's own order is a stock order of its own line, before
	// any other test: neither the hedge pass (a put on a Rulebook hedge
	// symbol) nor the unmeasured pass of a combination is ever a machine
	// sleeve's (mandate-risk-budget.md, "Placement": the two gate gaps).
	if draft.RegisterEntry != "" {
		if why := s.mandateEntryOrderRefusal(c, draft); why != "" {
			return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: "Mandate budget: " + why + "." + open, Action: "Refresh proposals; a strategy sends only its own row."}, nil
		}
	}
	if !s.riskAddingOrder(draft, position) || mandateBillBuy(draft) {
		return nil, nil
	}
	if !c.MandateActive() {
		return nil, nil
	}
	now := s.nowUTC()
	result, input, at, fp := s.currentMandateVerdict()
	if result == nil || result.Verdict == nil || input == nil || fp != c.FingerprintKey() {
		return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: "Mandate budget: no verdict has been measured for the policy in force yet; this order adds risk." + open, Action: action}, nil
	}
	if age := now.Sub(at); age > mandateVerdictMaxAge {
		return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: fmt.Sprintf("Mandate budget: the verdict is %s old and cannot be shown current; this order adds risk.", formatQuoteAge(age)) + open, Action: action}, nil
	}
	ccy := result.Verdict.BaseCurrency
	// A live entry's line is the entry's to add to: a hand buy of it would
	// mix the owner's units into the line it trades, so it is refused while
	// the entry is live; sales stay open (strategy_placement.go).
	if draft.RegisterEntry == "" {
		if why := s.mandateEntryLineRefusal(c, draft); why != "" {
			return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: "Mandate budget: " + why + "." + open,
				Action: "Let the strategy trade its line, or write its stage = \"suspended\" with a higher policy_version to trade it by hand."}, nil
		}
	}
	// The order is placed by its origin, as the verdict places fills
	// (strategy_placement.go): the core's contract is the core's whoever
	// sends it, an entry's own row is the entry's, anything else the owner's.
	sleeve, legID := mandateOrderPlacement(input.Allocations, draft, s.registerSleeveOf(c, draft.RegisterEntry))
	// An option buy adds risk within the premium limits, never within the
	// volatility budget (owner decision 2026-10-23 18:22 CEST): the premium
	// gate refuses it, the holds that bound a loss (the maximum loss, the
	// owner sleeve's stop) refuse it, and the volatility verdict is its
	// warning.
	// The owner's one-shot override lifts the volatility gate and, since
	// 2026-10-29 14:28 CET (owner: "I always need to be able to stay in
	// control"), the premium limits for the owner's own additions
	// (mandate_options_gate.go: what still binds). A machine sleeve's order
	// never reads it.
	override := s.mandateAdditionsOverride(now)
	if override != nil && sleeve != mandate.SleeveOwner {
		override = nil
	}
	option := mandateOptionAddition(draft)
	if option && override == nil {
		if blocker := s.mandatePremiumGate(draft, result, input, ccy, open); blocker != nil {
			return blocker, nil
		}
	}
	if option || override != nil {
		if blocker := mandateHardHolds(result.Verdict, sleeve, ccy, open); blocker != nil {
			return blocker, nil
		}
		if override != nil {
			return nil, mandateOverrideWarning(override, result.Verdict, ccy)
		}
	}
	vol := s.mandateVolatilityGate(draft, position, result, input, sleeve, legID, now, ccy, open, action)
	if vol == nil {
		return nil, nil
	}
	if option {
		return nil, mandateOptionWarning(vol)
	}
	return vol, nil
}

// mandateVolatilityGate judges a risk-adding order against the volatility
// budget: the standing verdict, the book re-evaluated with the order, the
// gap caps and the order's sleeve allocation.
func (s *Server) mandateVolatilityGate(draft rpc.OrderDraft, position rpc.OrderPositionImpact, result *rpc.MandateRiskResult, input *mandate.Input, sleeve, legID string, now time.Time, ccy, open, action string) *rpc.TradingBlocker {
	// A book over budget adds nothing, except in a sleeve inside its own risk
	// allocation (owner, 2026-10-18 15:31 CEST: "yes, it should. unless you
	// advise differently"): that sleeve keeps adding inside its allocation
	// while the forced reduction cuts the sleeve furthest over its own. An
	// unknown verdict adds nothing, ever. The same reading sizes a strategy
	// row's buy (strategy_target.go), so the two never disagree.
	why, overBudget := mandateVerdictHolds(result.Verdict, sleeve, ccy)
	if why != "" {
		return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: "Mandate budget: " + why + "; this order adds risk." + open, Action: action}
	}
	// The owner sleeve's drawdown stop: while engaged the sleeve is
	// reduce-only, inside the budget or over it (mandate_sleeve_stop.go).
	if why := mandateOwnerStopHolds(result.Verdict, sleeve, ccy); why != "" {
		return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: "Mandate budget: " + why + "; this order adds risk." + open,
			Action: fmt.Sprintf("Reduce or close instead; the stop releases when the sleeve's drawdown is back under %g%%. `canary mandate` has the sleeve's drawdown and high-water mark.", result.Verdict.OwnerStop.ReleasePct)}
	}
	// A name or currency the book does not hold yet has no decision-grade
	// history in the verdict: the order is measured on the series the store
	// keeps, and refused, its series requested on the background lane, while
	// none is fresh. Never measured blind, never passed unmeasured.
	book, missing := s.mandateOrderBook(draft, input, now)
	if len(missing) > 0 {
		var why []string
		var needs []mandateHistoryNeed
		for _, m := range missing {
			why = append(why, m.reason)
			if m.need.Key != "" {
				needs = append(needs, m.need)
			}
		}
		if len(needs) > 0 {
			s.requestMandateHistory(needs...)
		}
		return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: "Mandate budget: this order cannot be measured: " + strings.Join(why, "; ") + "." + open,
			Action: "Retry once the history has arrived (the background lane reads it within minutes); `canary mandate` has the measurement."}
	}
	leg, ok := mandateOrderLeg(draft, &book)
	if !ok {
		// Fail closed: an order the evaluator cannot price is never passed
		// unmeasured (core sleeve review, 2026-10-14).
		return mandateUnpricedOrderGate(draft, result.Verdict, book, sleeve, overBudget, ccy, open, action)
	}
	// The order's units open in its sleeve: the allocations are judged with it
	// placed there, as a leg of its own where the line's placed units belong
	// to another sleeve.
	if legID != "" {
		leg.ID = legID
	}
	book.Allocations = book.Allocations.WithPlaced(leg.ID, sleeve)
	with := mandate.Evaluate(mandate.WithLeg(book, leg))
	if with.Status == mandate.StatusUnknown || with.Risk == nil {
		why := "no reason given"
		if len(with.Reasons) > 0 {
			why = with.Reasons[0]
		}
		return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: "Mandate budget: with this order the book cannot be measured: " + why + "." + open, Action: action}
	}
	// The whole book with the order must stay inside the loss room (spec
	// TM-04.12: the loss measure is the headline the gate reads), unless the
	// book is over it already and the order's sleeve adds inside its own
	// allocation (checked below). The volatility with the order is measured
	// for the allocation check, never judged against the volatility budget.
	if l := with.Loss; !overBudget && l != nil && l.TotalBase > l.RoomBase+mandateReductionEpsilon {
		now := 0.0
		if result.Verdict.Loss != nil {
			now = result.Verdict.Loss.TotalBase
		}
		return &rpc.TradingBlocker{Code: mandateBudgetCode,
			Message: fmt.Sprintf("Mandate budget: with this order the loss measure would be %s, over the mandate's loss room %s (now %s).", mandateMoney(l.TotalBase, ccy), mandateMoney(l.RoomBase, ccy), mandateMoney(now, ccy)) + open,
			Action:  "Size it smaller, or reduce elsewhere first; `canary mandate` has the measurement."}
	}
	// The gap caps bind an addition as the budget does: an order that
	// would create a gap-cap breach, or deepen one, is refused, or the
	// forced reduction would sell it back on the next readings
	// (core-sleeve.md, review B1).
	if u, ok := mandateGapBreachAdded(result.Verdict, with); ok {
		return &rpc.TradingBlocker{Code: mandateBudgetCode,
			Message: fmt.Sprintf("Mandate budget: with this order a %s gap in %s would lose %s, over the gap cap %s.", mandateGapPct(u.GapShockPct), u.Underlying, mandateMoney(u.GapLossBase, ccy), mandateMoney(u.GapCapBase, ccy)) + open,
			Action:  "Size it smaller; `canary mandate` has each name's gap against its cap."}
	}
	// Allocations sit inside the whole-book limits: an addition also stays inside
	// its own sleeve's allocation (2026-10-18 08:06 CEST).
	return mandateAllocationRefusal(sleeve, result.Verdict, with, ccy, open)
}

// mandateUnpricedOrderGate judges a risk-adding order the evaluator cannot
// price. A stock or ETF without a price is refused. An option sale is
// refused: a short option's loss has no bound without the volatility its
// limit did not give. An option buy is judged by a volatility-free bound
// (mandateLongOptionBound) and refused without a limit, which gives no
// premium to bound it; its sleeve's own risk plus that bound must fit its
// allocation (a sleeve's use is at most its own risk, which the leg can raise by
// at most the bound). Any other security type is left as it was: this check
// judges only what the evaluator models.
func mandateUnpricedOrderGate(draft rpc.OrderDraft, standing *mandate.Verdict, book mandate.Input, sleeve string, overBudget bool, ccy, open, action string) *rpc.TradingBlocker {
	sym := normSym(draft.Contract.Symbol)
	refuse := func(msg, act string) *rpc.TradingBlocker {
		return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: "Mandate budget: " + msg + open, Action: act}
	}
	switch strings.ToUpper(strings.TrimSpace(draft.Contract.SecType)) {
	case "STK", "ETF":
		return refuse("this order cannot be measured: no price for "+sym+".", action)
	case "OPT":
	default:
		return nil
	}
	if strings.EqualFold(draft.Action, rpc.OrderActionSell) {
		return refuse("this option sale cannot be measured: its limit gives no volatility on "+sym+"'s price, and a short option's loss has no bound without one.",
			"Sell at a limit the volatility can be solved from (above intrinsic value, before expiry), or reduce elsewhere first; `canary mandate` has the measurement.")
	}
	if !(draft.LimitPrice > 0) {
		return refuse("this option buy cannot be measured: without a limit there is no premium to bound its loss.", "Buy at a limit; `canary mandate` has the measurement.")
	}
	b, why := mandateLongOptionBound(draft, book, standing)
	if why != "" {
		return refuse("this option buy cannot be measured: "+why+".", action)
	}
	budget := standing.Capital.BudgetAnnualSD
	if !overBudget && b.riskAnnual > budget {
		return refuse(fmt.Sprintf("with this order the risk could reach %s a year, over the budget %s: the option's limit gives no volatility, so it is bounded by %s's full daily moves.",
			mandateMoney(b.riskAnnual, ccy), mandateMoney(budget, ccy), sym), "Size it smaller, or buy at a limit the volatility can be solved from; `canary mandate` has the measurement.")
	}
	if b.gapLoss > b.gapCap || b.breached {
		return refuse(fmt.Sprintf("with this order a gap in %s could lose %s (its premium at most on top of the book's %s), over the gap cap %s.", sym, mandateMoney(b.gapLoss, ccy), mandateMoney(b.gapLoss-b.premium, ccy), mandateMoney(b.gapCap, ccy)),
			"Size it smaller; `canary mandate` has each name's gap against its cap.")
	}
	if allocation, ok := standing.Allocation(sleeve); ok {
		if worst := allocation.StandaloneBase + (b.riskAnnual - standing.Risk.SDAnnualBase); worst > allocation.AllocationBase+mandateReductionEpsilon {
			return refuse("with this order "+mandateAllocationOverSentence(sleeve, allocation, mandateMachineAllocations(*standing), "could", worst, false, ccy)+
				": the option's limit gives no volatility, so the allocation is bounded by its own risk plus "+sym+"'s full daily moves.",
				"Size it smaller, or buy at a limit the volatility can be solved from; `canary mandate` has each allocation.")
		}
	}
	return nil
}

// mandateLongBound is a volatility-free bound on a long option the
// evaluator cannot price.
type mandateLongBound struct {
	premium    float64
	riskAnnual float64
	gapLoss    float64
	gapCap     float64
	breached   bool
}

// mandateLongOptionBound bounds a long option the evaluator cannot price
// (a limit below intrinsic value, an expiry reached, an implied volatility
// that does not solve) without a volatility, in the evaluator's own terms.
//
// Risk: the leg's one-day P&L in scenario k is V(S_k)·fx_k − V(S)·fx, and
// its spread over the scenarios is the spread of V(S_k)·fx_k: the decay is
// the same shift in every scenario. A vanilla option's price moves at most
// as much as its underlying (1-Lipschitz in spot) and is worth at most the
// spot (a call) or the strike (a put), so the spread is at most
// units × the largest rate × the root mean square of the underlying's
// daily moves, plus that largest value × the root mean square of the
// rate's daily relative moves. By the triangle inequality the book's
// annualised risk grows by at most √252 times that.
//
// Gap: an option bought at a limit loses at most what is paid for it, its
// premium at the limit, so the underlying's gap loss grows by at most that
// premium.
//
// Coarse, and only ever above the truth.
func mandateLongOptionBound(draft rpc.OrderDraft, book mandate.Input, standing *mandate.Verdict) (mandateLongBound, string) {
	var b mandateLongBound
	if standing == nil || standing.Risk == nil || standing.Stresses == nil {
		return b, "the standing verdict carries no risk to add the bound to"
	}
	sym := normSym(draft.Contract.Symbol)
	ccy := normCcy(draft.Contract.Currency)
	spot := 0.0
	for _, l := range book.Legs {
		if l.Underlying == sym && l.Spot > 0 {
			spot = l.Spot
			if ccy == "" {
				ccy = normCcy(l.Currency)
			}
		}
	}
	returnsKey := mandate.ReturnsSeries(sym, ccy)
	series, ok := book.Series[returnsKey]
	if !ok || len(series.Closes) < mandate.Closes {
		return b, "no decision-grade daily history for " + sym
	}
	if spot <= 0 && returnsKey == sym {
		spot = series.Closes[len(series.Closes)-1]
	}
	if spot <= 0 {
		return b, "no price for " + sym
	}
	// rms of the relative daily moves over the last mandate.Scenarios
	// returns, and the largest factor a move reached.
	moves := func(closes []float64) (rms, maxFactor float64) {
		closes = closes[len(closes)-mandate.Closes:]
		maxFactor, ss := 1.0, 0.0
		for i := 1; i < len(closes); i++ {
			r := closes[i]/closes[i-1] - 1
			ss += r * r
			maxFactor = math.Max(maxFactor, 1+r)
		}
		return math.Sqrt(ss / float64(len(closes)-1)), maxFactor
	}
	priceRMS, _ := moves(series.Closes)
	fx, fxRMS, fxMax := 1.0, 0.0, 1.0
	if ccy != "" && ccy != normCcy(book.BaseCurrency) {
		rate, ok := book.FXNow[ccy]
		fxSeries, okSeries := book.FX[ccy]
		if !ok || !(rate > 0) || !okSeries || len(fxSeries.Closes) < mandate.Closes {
			return b, "no " + book.BaseCurrency + "/" + ccy + " exchange rate with its history"
		}
		fx = rate
		fxRMS, fxMax = moves(fxSeries.Closes)
	}
	mult := float64(draft.Contract.Multiplier)
	if mult <= 0 {
		mult = 100
	}
	units := float64(draft.Quantity) * mult
	worth := spot // a call is worth at most the spot
	if strings.EqualFold(strings.TrimSpace(draft.Contract.Right), "P") {
		worth = draft.Contract.Strike // a put at most the strike
	}
	if !(worth > 0) {
		return b, "incomplete option terms"
	}
	spread1D := units*fx*fxMax*spot*priceRMS + units*worth*fx*fxMax*fxRMS
	b.premium = units * draft.LimitPrice * fx
	b.riskAnnual = standing.Risk.SDAnnualBase + math.Sqrt(mandate.Scenarios)*spread1D
	b.gapCap = standing.Stresses.GapCapBase
	for _, u := range standing.Underlyings {
		if u.Underlying == sym {
			b.gapLoss, b.gapCap, b.breached = u.GapLossBase, u.GapCapBase, u.GapBreach
		}
	}
	b.gapLoss += b.premium
	return b, ""
}

// mandateGapBreachAdded finds the first underlying whose gap loss breaches
// its cap with the order and did not before it, or breaches by more than it
// did: an addition that creates or deepens a gap-cap breach. A broad-index
// line is judged by the index gap group it moves with (owner decision
// "1. A", 2026-10-30 09:41 CET): a hedge that leaves the group's standing
// breach smaller adds none, though its own line is new.
func mandateGapBreachAdded(before *mandate.Verdict, with mandate.Verdict) (mandate.UnderlyingReport, bool) {
	prior := map[string]mandate.UnderlyingReport{}
	var index *mandate.IndexGapReport
	if before != nil {
		for _, u := range before.Underlyings {
			prior[u.Underlying] = u
		}
		if before.Stresses != nil {
			index = before.Stresses.IndexGap
		}
	}
	for _, u := range with.Underlyings {
		if !u.GapBreach {
			continue
		}
		if u.BroadIndex {
			if index == nil || !index.Breach || u.GapLossBase > index.LossBase+1e-6 {
				return u, true
			}
			continue
		}
		was, held := prior[u.Underlying]
		if !held || !was.GapBreach || u.GapLossBase > was.GapLossBase+1e-6 {
			return u, true
		}
	}
	return mandate.UnderlyingReport{}, false
}

// mandateGapPct names a gap shock in a refusal: "−25%".
func mandateGapPct(v float64) string {
	return "−" + strconv.FormatFloat(v, 'f', -1, 64) + "%"
}

// mandateBudgetGateError is the gate as the admission and wire-guard checks
// report it: a trading-disabled error carrying the blocker's words.
func (s *Server) mandateBudgetGateError(draft rpc.OrderDraft, position rpc.OrderPositionImpact) error {
	blocker := s.mandateBudgetGate(draft, position)
	if blocker == nil {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrTradingDisabled, blocker.Message)
}

// mandateMissing is one series an order needs that the store cannot give
// fresh, and why; need is empty when nothing can be fetched for it.
type mandateMissing struct {
	need   mandateHistoryNeed
	reason string
}

// mandateOrderBook is the kept book with the decision-grade series an order
// needs and the verdict does not carry (a name or currency the book does
// not hold), read from the history store, or what is missing: the
// underlying's series, the base currency's pair against the order's
// currency, or the currency itself. A store read, never a broker read.
func (s *Server) mandateOrderBook(draft rpc.OrderDraft, in *mandate.Input, now time.Time) (mandate.Input, []mandateMissing) {
	book := *in
	book.Series, book.FX, book.FXNow = maps.Clone(in.Series), maps.Clone(in.FX), maps.Clone(in.FXNow)
	if book.Series == nil {
		book.Series = map[string]mandate.Series{}
	}
	if book.FX == nil {
		book.FX = map[string]mandate.Series{}
	}
	if book.FXNow == nil {
		book.FXNow = map[string]float64{}
	}
	sym := mandateOrderUnderlying(draft)
	ccy := normCcy(draft.Contract.Currency)
	for _, l := range in.Legs {
		if l.Underlying == sym && ccy == "" {
			ccy = normCcy(l.Currency)
		}
	}
	if sym == "" {
		return book, []mandateMissing{{reason: "the order names no instrument"}}
	}
	if ccy == "" {
		return book, []mandateMissing{{reason: "the order names no currency for " + sym}}
	}
	fresh := func(need mandateHistoryNeed) (*mandateHistorySeries, bool) {
		series := s.mandateHistoryForNeed(context.Background(), need)
		return series, series != nil && !mandateSeriesStale(series, now) && len(series.Closes) >= mandate.Closes
	}
	var missing []mandateMissing
	if returns := mandate.ReturnsSeriesFor(sym, ccy, in.LineISINs[mandateConLegID(draft.Contract.ConID)]); returns != sym {
		// A line moved by its proxy's returns (mandate.ReturnsProxies): the
		// proxy is the index, which every verdict reads; its price is the
		// held line's or the order's limit, never the proxy's close.
		proxy := mandateIndexHistoryNeed()
		if series, ok := fresh(proxy); ok && returns == proxy.Contract.Symbol {
			book.Series[returns] = mandateSeriesInput(series, now)
		} else {
			missing = append(missing, mandateMissing{reason: "no daily history for " + returns + ", whose returns " + sym + " is measured by"})
		}
		held := false
		for _, l := range in.Legs {
			held = held || l.Underlying == sym && l.Spot > 0
		}
		if !held && !(draft.LimitPrice > 0) {
			missing = append(missing, mandateMissing{reason: "no price for " + sym + ": the book does not hold it and the order carries no limit"})
		}
	} else {
		contract := rpc.ContractParams{Symbol: sym, SecType: "STK", Currency: ccy, Exchange: "SMART"}
		if strings.EqualFold(draft.Contract.SecType, "STK") || strings.EqualFold(draft.Contract.SecType, "ETF") {
			contract.ConID, contract.PrimaryExch = draft.Contract.ConID, strings.ToUpper(strings.TrimSpace(draft.Contract.PrimaryExch))
		} else {
			// An option reads the series the book reads for its underlying.
			contract = mandateOptionUnderlyingContract(sym, ccy)
		}
		// Options use the held stock's underlying identity when available,
		// never the option contract ID. Multiple same-symbol stocks conflict.
		if contract.ConID == 0 {
			for _, leg := range in.Legs {
				if leg.Underlying == sym && leg.Kind == mandate.KindStock {
					id := mandateLegConID(leg.ID)
					if id <= 0 || (contract.ConID > 0 && contract.ConID != id) {
						return book, []mandateMissing{{reason: "daily history for " + sym + " is ambiguous across distinct contracts"}}
					}
					contract.ConID = id
				}
			}
		}
		for _, leg := range in.Legs {
			if leg.Underlying != sym {
				continue
			}
			if normCcy(leg.Currency) != ccy || leg.Kind == mandate.KindStock && contract.ConID > 0 && mandateLegConID(leg.ID) != contract.ConID {
				return book, []mandateMissing{{reason: "daily history for " + sym + " is ambiguous across distinct contracts"}}
			}
		}
		if series, ok := fresh(mandateHistoryNeed{Key: "u:" + sym, Contract: contract}); ok {
			book.Series[sym] = mandateSeriesInput(series, now)
		} else if series := s.strategyLineSeries(draft.Contract.ConID, now); series != nil {
			// A registered strategy's line the book does not hold yet is
			// measured on the strategy's own closes, as its verdict's
			// headroom was, so its first row is never refused unmeasured.
			book.Series[sym] = mandateSeriesInput(series, now)
		} else {
			missing = append(missing, mandateMissing{need: mandateHistoryNeed{Key: "u:" + sym, Contract: contract}, reason: "no daily history for " + sym + " yet; the desk is fetching it"})
		}
	}
	if base := normCcy(in.BaseCurrency); ccy != base {
		if series, ok := fresh(mandateHistoryNeed{Key: "fx:" + ccy, Contract: rpc.ContractParams{Symbol: base, SecType: "CASH", Currency: ccy, Exchange: "IDEALPRO"}}); ok {
			book.FX[ccy] = mandateSeriesInput(series, now)
		} else {
			missing = append(missing, mandateMissing{need: mandateHistoryNeed{Key: "fx:" + ccy, Contract: rpc.ContractParams{Symbol: base, SecType: "CASH", Currency: ccy, Exchange: "IDEALPRO"}},
				reason: "no daily history for the " + base + "/" + ccy + " exchange rate yet; the desk is fetching it"})
		}
		if _, ok := book.FXNow[ccy]; !ok {
			if fx, ok := book.FX[ccy]; ok && len(fx.Closes) > 0 {
				book.FXNow[ccy] = fx.Closes[len(fx.Closes)-1]
			}
		}
	}
	return book, missing
}

// mandateOrderLeg prices an order as a leg of the kept book: a stock at the
// held line's spot (else, for a name not held, its last decision-grade
// close, else the order's limit), an option at the held contract's terms
// and IV, or, for a contract not held, at the volatility its limit price
// implies on the underlying's spot (held, else its last decision-grade
// close). False when the addition cannot be priced without a guess.
func mandateOrderLeg(draft rpc.OrderDraft, in *mandate.Input) (mandate.Leg, bool) {
	qty := float64(draft.Quantity)
	if strings.EqualFold(draft.Action, rpc.OrderActionSell) {
		qty = -qty
	}
	sym := mandateOrderUnderlying(draft)
	ccy := normCcy(draft.Contract.Currency)
	id := ""
	if draft.Contract.ConID > 0 {
		id = fmt.Sprintf("con:%d", draft.Contract.ConID)
	}
	var held *mandate.Leg
	var underlyingSpot float64
	for i := range in.Legs {
		l := &in.Legs[i]
		if id != "" && l.ID == id {
			held = l
		}
		if l.Underlying == sym && l.Spot > 0 {
			underlyingSpot = l.Spot
			if ccy == "" {
				ccy = l.Currency
			}
		}
	}
	if ccy != "" && ccy != strings.ToUpper(in.BaseCurrency) {
		if _, ok := in.FXNow[ccy]; !ok {
			return mandate.Leg{}, false
		}
	}
	if underlyingSpot <= 0 {
		// A name the book does not hold: its last decision-grade close.
		if series, ok := in.Series[sym]; ok && len(series.Closes) > 0 {
			underlyingSpot = series.Closes[len(series.Closes)-1]
		}
	}
	switch strings.ToUpper(draft.Contract.SecType) {
	case "STK", "ETF":
		spot := underlyingSpot
		if held != nil && held.Kind == mandate.KindStock {
			spot = held.Spot
		}
		if spot <= 0 {
			spot = draft.LimitPrice
		}
		if spot <= 0 || qty == 0 {
			return mandate.Leg{}, false
		}
		if id == "" {
			id = "order:" + sym
		}
		if held != nil {
			id = held.ID
		}
		return mandate.Leg{ID: id, Symbol: sym, Underlying: sym, Kind: mandate.KindStock, Quantity: qty, Multiplier: 1, Currency: ccy, Spot: spot}, true
	case "OPT":
		// A buy's premium at its limit is the loss it adds (contracts ×
		// multiplier × limit, in base currency); a sale adds none.
		premium := 0.0
		if qty > 0 && draft.LimitPrice > 0 {
			mult := float64(draft.Contract.Multiplier)
			if mult <= 0 {
				mult = 100
			}
			fx := 1.0
			if ccy != "" && ccy != strings.ToUpper(in.BaseCurrency) {
				fx = in.FXNow[ccy]
			}
			premium = draft.LimitPrice * mult * fx
		}
		if held != nil && held.Kind == mandate.KindOption && held.IV != nil {
			leg := *held
			leg.Quantity, leg.LossPerUnitBase, leg.LossUnknown = qty, premium, ""
			return leg, true
		}
		right := strings.ToUpper(strings.TrimSpace(draft.Contract.Right))
		mult := float64(draft.Contract.Multiplier)
		if mult <= 0 {
			mult = 100
		}
		t := mandateYearsToExpiry(draft.Contract.Expiry, in.Now)
		if underlyingSpot <= 0 || draft.Contract.Strike <= 0 || (right != "C" && right != "P") || draft.LimitPrice <= 0 || t <= 0 || qty == 0 {
			return mandate.Leg{}, false
		}
		iv, ok := mandate.ImpliedVolatility(right == "C", draft.LimitPrice, underlyingSpot, draft.Contract.Strike, t)
		if !ok {
			return mandate.Leg{}, false
		}
		if id == "" {
			id = fmt.Sprintf("order:%s:%s:%g:%s", sym, draft.Contract.Expiry, draft.Contract.Strike, right)
		}
		return mandate.Leg{ID: id, Symbol: fmt.Sprintf("%s %s %g %s", sym, draft.Contract.Expiry, draft.Contract.Strike, right), Underlying: sym, Kind: mandate.KindOption,
			Quantity: qty, Multiplier: mult, Currency: ccy, Spot: underlyingSpot, Right: right, Strike: draft.Contract.Strike, TimeToExpiryYears: t, IV: &iv, LossPerUnitBase: premium}, true
	case "FUT":
		// The index future is the mandate's own linear index line (MES FUT),
		// at its multiplier and the held contract's mark, else the order's
		// limit; no other future is priced.
		mult := float64(draft.Contract.Multiplier)
		spot := draft.LimitPrice
		if held != nil && mandate.IsIndexFuture(*held) && held.Spot > 0 {
			spot = held.Spot
		}
		if !isIndexFutureContract(draft.Contract) || mult <= 0 || spot <= 0 || qty == 0 {
			return mandate.Leg{}, false
		}
		if id == "" {
			id = "order:" + mandate.MicroSP500Future
		}
		if held != nil {
			id = held.ID
		}
		return mandate.Leg{ID: id, Symbol: nonEmptyString(draft.Contract.LocalSymbol, "MES "+draft.Contract.Expiry), Underlying: mandate.MicroSP500Future, Kind: mandate.KindStock,
			Quantity: qty, Multiplier: mult, Currency: ccy, Spot: spot}, true
	}
	return mandate.Leg{}, false
}

// mandateOrderUnderlying is the mandate's underlying key of an order's
// contract: the index future's own key (moved by SPY's returns), else its
// symbol.
func mandateOrderUnderlying(draft rpc.OrderDraft) string {
	if isIndexFutureContract(draft.Contract) {
		return mandate.MicroSP500Future
	}
	return normSym(draft.Contract.Symbol)
}
