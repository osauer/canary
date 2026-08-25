package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
)

const (
	briefMoverLimit = 3
)

func (s *Server) handleBriefSnapshot(ctx context.Context, req *rpc.Request) (*rpc.BriefResult, error) {
	if len(req.Params) > 0 {
		var p rpc.BriefSnapshotParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
	}
	res, _ := s.composeBrief(ctx)
	return res, nil
}

func (s *Server) composeBrief(ctx context.Context) (*rpc.BriefResult, *rpc.RulesResult) {
	acct, acctErr := s.buildAccountSummary(ctx, false)
	pos, posErr := s.handlePositionsList(ctx, &rpc.Request{})
	pos = s.analysisPositions(pos, s.briefNow())
	regime, regimeErr := s.briefRegimeSnapshotContext(ctx)
	breadth, breadthErr := s.buildBreadthSPX(&rpc.Request{}, false)
	gamma := s.briefGammaSnapshot()

	var marketEvents *rpc.MarketEventsResult
	var marketEventsErr error
	if pos != nil {
		symbols := marketEventSymbolsFromPositions(pos)
		marketEvents, marketEventsErr = s.handleMarketEventsSnapshot(ctx, &rpc.Request{Params: briefJSON(rpc.MarketEventsParams{Symbols: symbols})})
	} else {
		marketEventsErr = posErr
	}

	rules := s.evaluateRulesMode(ctx, false, false)
	// The brief boundary is captured after its input reads. In particular,
	// and causes the alert producer to fail closed with source_time_invalid.
	now := s.briefNow()
	res := &rpc.BriefResult{AsOf: now}
	cal, calErr := s.handleMarketCalendar(&rpc.Request{Params: briefJSON(rpc.MarketCalendarParams{Market: "us", At: now, Days: 1})})
	renderAuthority := s.currentNudgeAuthority(now)
	policy := s.briefPolicyResultForAuthority(acct, acctErr, renderAuthority, now)
	constitution := renderAuthority.policy
	recon := s.buildReconReport()

	// A closed official session downgrades expected coldness (paused event
	sessionOpen := calErr != nil || cal == nil || cal.Session.IsOpen

	market, can := composeBriefMarket(now, acct, pos, regime, breadth, gamma, marketEvents,
		acctErr, posErr, regimeErr, breadthErr, marketEventsErr, sessionOpen)
	// Brief-hook stress evidence: the same computed result the brief row
	s.journalStressDecision(&can)
	calendar := composeBriefCalendar(cal, marketEvents, rules, calErr, marketEventsErr, sessionOpen, briefBorrowFeeRelevant(pos, posErr))
	portfolio := s.composeBriefPortfolio(acct, pos, acctErr, posErr, sessionOpen)
	riskLimits := composeBriefRisk(policy, now)
	if recon != nil {
		riskLimits.Latch.ReportCoverageTo = recon.CoverageTo
		riskLimits.Latch.ReportCheckedAt = recon.Fetch.LastAttempt
		if riskLimits.Latch.Latched && !recon.CoverageTo.IsZero() && recon.CoverageTo.Before(riskLimits.Latch.At) {
			riskLimits.Latch.Detail = fmt.Sprintf("drawdown review needed; latest daily broker report covers through %s", recon.CoverageTo.In(time.Local).Format(time.DateOnly))
		}
	}
	process := s.composeBriefProcessForAuthority(policy, constitution, recon, rules, renderAuthority, now)

	// The five domain sections above are composition intermediates: the two
	res.Review = s.composeBriefReview(portfolio, riskLimits, process, s.briefEdgeRow(ctx), now)
	res.Ready = composeBriefReady(market, calendar, riskLimits, portfolio, process, s.briefReadyProposals())
	res.BriefFingerprint = briefContentFingerprint(res)
	// The narrative is a deterministic projection of the two movements above
	// only, so revised prose can never invalidate the brief identity.
	res.Narrative = composeBriefNarrative(res)
	// Bind v4 brief identity to the current constitution even when a policy-only
	if constitution != nil && constitution.PolicyVersion >= 4 {
		res.BriefFingerprint = opaqueIdentity("v4-brief", res.BriefFingerprint, renderAuthority.policyIdentity)
	}
	return res, rules
}

func (s *Server) briefNow() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func briefJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (s *Server) briefGammaSnapshot() *rpc.GammaZeroSPXResult {
	if s == nil || s.zeroGamma == nil {
		return nil
	}
	env := s.zeroGamma.snapshotCurrent(rpc.GammaZeroScopeCombined, s.briefNow)
	return &env
}

func (s *Server) briefRegimeSnapshotContext(ctx context.Context) (*rpc.RegimeSnapshotResult, error) {
	if s == nil {
		return nil, fmt.Errorf("regime snapshot unavailable")
	}
	return s.currentDecisionReadyRegimeSnapshot(ctx)
}

func (s *Server) briefPolicyResultForAuthority(acct *rpc.AccountResult, acctErr error, authority nudgeAuthorityState, now time.Time) *rpc.RiskPolicyResult {
	value := authority.report
	res := &value
	res.AsOf = now
	res.Unapproved = append([]string(nil), authority.report.Unapproved...)
	res.Inventory = append([]rpc.PolicyPinStatus(nil), authority.report.Inventory...)
	if authority.policy == nil {
		res.Unapproved = (&risk.Constitution{}).UnapprovedKeys()
	}
	var obs *risk.CapitalObservation
	if acctErr == nil && acct != nil && acct.NetLiquidation > 0 &&
		brokerScopeConcrete(authority.scope) && strings.EqualFold(strings.TrimSpace(acct.AccountID), strings.TrimSpace(authority.scope.Account)) &&
		(authority.policy == nil || authority.policy.Capital.BaseCurrency == "" || acct.BaseCurrency == "" || strings.EqualFold(authority.policy.Capital.BaseCurrency, acct.BaseCurrency)) {
		obs = &risk.CapitalObservation{EquityBase: acct.NetLiquidation, AsOf: acct.AsOf}
	}
	scope := authority.scope
	res.Capital = s.riskCapital.Report(authority.policy, obs, scope)
	res.Limits = risk.ConstitutionLimits(authority.policy)
	res.Overrides = s.riskCapital.OverridesSnapshotForScope(scope)
	return res
}

// composeBriefReview assembles the post-trade Review movement from the existing
func (s *Server) composeBriefReview(portfolio rpc.BriefPortfolioSection, riskLimits rpc.BriefRiskSection, process rpc.BriefProcessSection, edge rpc.BriefEdgeRow, now time.Time) rpc.BriefReviewSection {
	out := rpc.BriefReviewSection{
		SessionPnL:    portfolio.Account,
		LastSession:   s.composeBriefLastSession(now),
		Edge:          edge,
		Attribution:   portfolio.Movers,
		Rules:         process.Rules,
		Proposals:     s.briefProposals(now),
		Overrides:     riskLimits.Overrides,
		CapitalEvents: briefCapitalEvents(riskLimits.Capital, riskLimits.Latch),
		Reconcile:     process.Reconcile,
		AutoExtend:    process.AutoExtend,
		WorkingOrders: portfolio.WorkingOrders,
	}
	// Edge is a retrospective discovery row, not current desk authority. Its
	// setup/backfill state stays visible without degrading today's brief.
	out.BriefRowState = briefSectionState("review",
		out.SessionPnL.BriefRowState, out.LastSession.BriefRowState, out.Attribution.BriefRowState, out.Rules.BriefRowState,
		out.Proposals.BriefRowState, out.Overrides.BriefRowState, out.CapitalEvents.BriefRowState,
		out.Reconcile.BriefRowState, out.AutoExtend.BriefRowState,
		out.WorkingOrders.BriefRowState)
	return out
}

func (s *Server) briefEdgeRow(ctx context.Context) rpc.BriefEdgeRow {
	row := rpc.BriefEdgeRow{State: rpc.EdgeStateUnavailable, HorizonSessions: 20}
	result, err := s.handleEdgeSnapshot(ctx, &rpc.Request{Params: briefJSON(rpc.EdgeSnapshotParams{Window: "90d", HorizonSessions: 20, Limit: 1})})
	if err != nil || result == nil {
		row.BriefRowState = briefUnavailable("Canary Edge snapshot unavailable")
		return row
	}
	row.State, row.Headline, row.Fingerprint = result.State, result.Headline, result.Fingerprint
	switch result.State {
	case rpc.EdgeStateCurrent:
		row.BriefRowState = briefOK("broker-truth decision review current")
	case rpc.EdgeStateDegraded, rpc.EdgeStateBackfilling, rpc.EdgeStateInsufficient:
		row.BriefRowState = briefDegraded("broker-truth decision review " + result.State)
	default:
		row.BriefRowState = briefUnavailable("broker-truth decision review " + result.State)
	}
	if len(result.Findings) > 0 {
		finding := result.Findings[0]
		value := finding.DecisionImpactBase
		row.ChangeID, row.Symbol, row.Action = finding.ChangeID, finding.Symbol, finding.Action
		row.HorizonSessions, row.DecisionImpactBase = finding.HorizonSessions, &value
	}
	if result.Account != nil {
		row.BaseCurrency = result.Account.BaseCurrency
	}
	return row
}

// composeBriefLastSession serves the daemon's close capture as the last
// completed session's Daily P&L. It never substitutes: a retained capture for
// any other session date, a non-concrete broker scope, or an unresolvable
// calendar all read as not captured, because everything the broker serves off
func (s *Server) composeBriefLastSession(now time.Time) rpc.BriefLastSessionRow {
	row := rpc.BriefLastSessionRow{}
	date, ok := lastCompletedUSEquitySessionDate(now)
	if !ok {
		row.BriefRowState = briefUnavailable("the served calendar cannot resolve the last completed session")
		return row
	}
	row.SessionDate = date
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		row.BriefRowState = briefUnavailable("close captures bind to one concrete account and mode; the current broker scope names neither")
		return row
	}
	capture, ok := s.dailyPnLCloseCaptures.captureFor(dailyPnLScopeSource(scope))
	if !ok || capture.SessionKey != date {
		row.BriefRowState = briefUnavailable(fmt.Sprintf("not captured for %s — the daemon records this figure only while running and connected at the official close", date))
		return row
	}
	row.BriefRowState = briefOK("account Daily P&L pinned at the official close; unlike the live row it does not move on off-session marks")
	row.DailyPnLBase = new(capture.DailyPnL)
	row.BaseCurrency = capture.BaseCurrency
	row.SessionClose = capture.SessionClose
	row.CapturedAt = capture.CapturedAt
	return row
}

// composeBriefReady assembles the pre-trade Ready movement from the existing
func composeBriefReady(market rpc.BriefMarketSection, calendar rpc.BriefCalendarSection,
	riskLimits rpc.BriefRiskSection, portfolio rpc.BriefPortfolioSection, process rpc.BriefProcessSection,
	proposals rpc.BriefReadyProposalsRow) rpc.BriefReadySection {
	out := rpc.BriefReadySection{
		Regime:        market.Regime,
		Breadth:       market.Breadth,
		Gamma:         market.Gamma,
		Stress:        market.Stress,
		Session:       calendar.Session,
		MarketEvents:  calendar.MarketEvents,
		Capital:       riskLimits.Capital,
		Latch:         riskLimits.Latch,
		PremiumAtRisk: portfolio.PremiumAtRisk,
		HedgeCost:     portfolio.HedgeCost,
		Proposals:     proposals,
		PolicyDrift:   riskLimits.PolicyDrift,
		MonthlyPulse:  process.MonthlyPulse,
	}
	out.BriefRowState = briefReadySectionState(out)
	return out
}

func briefReadySectionState(ready rpc.BriefReadySection) rpc.BriefRowState {
	rows := []rpc.BriefRowState{
		ready.Regime.BriefRowState, ready.Breadth.BriefRowState, ready.Gamma.BriefRowState,
		ready.Stress.BriefRowState, ready.Session.BriefRowState,
	}
	for _, ev := range ready.MarketEvents {
		rows = append(rows, ev.BriefRowState)
	}
	rows = append(rows,
		ready.Capital.BriefRowState, ready.Latch.BriefRowState,
		ready.PremiumAtRisk.BriefRowState, ready.HedgeCost.BriefRowState,
		ready.Proposals.BriefRowState, ready.PolicyDrift.BriefRowState)
	if ready.MonthlyPulse != nil {
		rows = append(rows, briefMonthlyPulseRollupState(ready.MonthlyPulse.Status))
	}
	return briefSectionState("ready", rows...)
}

// briefCapitalEvents frames the current latch and adjusted-peak provenance as
// the Review movement's capital-events row. It regroups existing facts only —
// latch or an absent constitution never reads as a clean "no events" line.
func briefCapitalEvents(capital rpc.BriefCapitalRow, latch rpc.BriefLatchRow) rpc.BriefCapitalEventsRow {
	row := rpc.BriefCapitalEventsRow{
		BriefRowState:      briefOK("no capital events this session; adjusted-peak provenance shown"),
		Latched:            latch.Latched,
		LatchedAt:          latch.At,
		LatchProvisional:   latch.Provisional,
		LatchAgeDays:       latch.AgeDays,
		ConsumedPctAtLatch: latch.ConsumedPctAtLatch,
		AdjustedPeakBase:   capital.AdjustedPeakBase,
		PeakAsOf:           capital.PeakAsOf,
		BaseCurrency:       capital.BaseCurrency,
		ReportCoverageTo:   latch.ReportCoverageTo,
		ReportCheckedAt:    latch.ReportCheckedAt,
	}
	switch {
	case capital.Status == rpc.BriefStatusUnavailable:
		row.BriefRowState = briefUnavailable("risk constitution absent; capital events cannot be evaluated")
	case latch.Latched && latch.Provisional:
		row.BriefRowState = briefAttention("drawdown latch engaged provisionally; awaiting the broker statement that covers the latch day")
	case latch.Latched:
		row.BriefRowState = briefAttention("drawdown latch engaged this episode and remains open until a human reset")
	}
	return row
}

// briefProposals derives protection-proposal offered-vs-acted counts read-only
// restructure adds; only counts and the covered day reach the wire.
func (s *Server) briefProposals(_ time.Time) rpc.BriefProposalsRow {
	if s == nil || s.proposalOutcomes == nil {
		return rpc.BriefProposalsRow{BriefRowState: briefUnavailable("proposal outcome journal is unavailable")}
	}
	offered, acted, day, ok, err := s.proposalOutcomes.SessionSummary()
	if err != nil {
		return rpc.BriefProposalsRow{BriefRowState: briefUnavailable("proposal outcome journal could not be read")}
	}
	if !ok {
		return rpc.BriefProposalsRow{BriefRowState: briefOK("no protection proposals recorded yet")}
	}
	return rpc.BriefProposalsRow{
		BriefRowState: briefOK(fmt.Sprintf("%d offered, %d acted in the last recorded session (%s)", offered, acted, day)),
		Day:           day, Offered: offered, Acted: acted,
	}
}

// briefReadyProposals projects the CURRENT protection-proposal snapshot into
// attention row — never an authorization: submission keeps every gate it has.
func (s *Server) briefReadyProposals() rpc.BriefReadyProposalsRow {
	if s == nil || s.tradeProposals == nil {
		return rpc.BriefReadyProposalsRow{BriefRowState: briefUnavailable("protection proposal snapshot is unavailable")}
	}
	snap := s.tradeProposals.Snapshot(false)
	total, actionable := snap.Counts.Total, snap.Counts.Actionable
	blocked := max(total-actionable, 0)
	row := rpc.BriefReadyProposalsRow{Actionable: actionable, Blocked: blocked, Total: total}
	switch {
	case actionable > 0:
		row.BriefRowState = briefAttention(fmt.Sprintf("%d protection proposal(s) ready to act, %d blocked", actionable, blocked))
	case blocked > 0:
		row.BriefRowState = briefOK(fmt.Sprintf("no protection proposal is ready to act; %d blocked", blocked))
	default:
		row.BriefRowState = briefOK("no protection proposals are staged")
	}
	return row
}

// composeBriefMarket stays pure: it also returns the computed stress
func composeBriefMarket(now time.Time, acct *rpc.AccountResult, pos *rpc.PositionsResult,
	regime *rpc.RegimeSnapshotResult, breadth *rpc.BreadthSPXResult, gamma *rpc.GammaZeroSPXResult,
	events *rpc.MarketEventsResult, acctErr, posErr, regimeErr, breadthErr, eventsErr error, sessionOpen bool) (rpc.BriefMarketSection, rpc.StressResult) {
	out := rpc.BriefMarketSection{}
	if regimeErr != nil || regime == nil {
		out.Regime.BriefRowState = briefUnavailable("regime snapshot unavailable: " + errText(regimeErr))
	} else {
		out.Regime.BriefRowState = briefOK("daemon regime lifecycle and composite verdict")
		out.Regime.Stage = regime.Posture.Stage
		if out.Regime.Stage == "" {
			out.Regime.Stage = regime.Lifecycle.Stage
		}
		out.Regime.Verdict = regime.Composite.Verdict
		if len(regime.WarningDetails) > 0 || out.Regime.Stage == "" || out.Regime.Verdict == "" {
			out.Regime.BriefRowState = briefDegraded("regime returned partial or unclassified evidence")
		}
		if health := regime.AuthorityHealth; health != nil {
			switch health.Status {
			case rpc.RegimeAuthorityUnavailable:
				out.Regime.BriefRowState = briefUnavailable("daemon Regime last-good authority is unavailable")
			case rpc.RegimeAuthorityStale:
				out.Regime.BriefRowState = briefDegraded("daemon Regime verdict is retained stale last-good context")
			case rpc.RegimeAuthorityFresh:
				if health.FailureCode != rpc.RegimeAuthorityFailureNone {
					out.Regime.BriefRowState = briefDegraded("daemon Regime last-good is fresh but its latest authority operation failed")
				}
			default:
				out.Regime.BriefRowState = briefUnavailable("daemon Regime authority health is invalid")
			}
		}
	}
	if breadthErr != nil || breadth == nil {
		out.Breadth.BriefRowState = briefUnavailable("breadth snapshot unavailable: " + errText(breadthErr))
	} else if breadth.State != rpc.BreadthStateReady {
		out.Breadth.BriefRowState = briefDegraded("breadth source is " + string(breadth.State))
	} else {
		// Name the session on every row. A reading is one trading day's close
		detail := "S&P 500 constituent breadth · " + breadth.SessionKey + " session"
		switch {
		case !breadth.Stale:
			out.Breadth.BriefRowState = briefOK(detail)
		case spx.PublicationPending(breadth.SessionKey, breadth.Refreshing, now):
			// The ordinary post-close window: the newer session's fan-out is
			// running and still inside its bounded deadline. Degrading here
			// would light the row for ~90 min after every close.
			out.Breadth.BriefRowState = briefOK(detail + "; the newer session is still computing")
		default:
			out.Breadth.BriefRowState = briefDegraded(detail + "; a newer session is overdue")
		}
		out.Breadth.PctAbove50DMA = new(breadth.PctAbove50DMA)
		out.Breadth.PctAbove200DMA = new(breadth.PctAbove200DMA)
		out.Breadth.NetNewHighsPct = new(breadth.NetNewHighsPct)
	}
	if breadth != nil {
		out.Breadth.AsOf, out.Breadth.DataType = breadth.AsOf, breadth.DataType
	}
	out.Gamma = composeBriefGamma(gamma, sessionOpen, now)
	stressInput := rpc.StressInput{Now: now}
	if acct != nil {
		stressInput.Account = *acct
	}
	if pos != nil {
		stressInput.Positions = *pos
	}
	if regime != nil {
		stressInput.Regime = *regime
	}
	if events != nil {
		stressInput.MarketEvents = *events
	}
	can := stress.ComputeStress(stressInput)
	out.Stress = rpc.BriefStressRow{
		BriefRowState: briefOK("pure stress composition over daemon snapshots"),
		Action:        can.Action, Severity: string(can.Severity), Summary: can.Summary,
	}
	if acctErr != nil || posErr != nil || regimeErr != nil || eventsErr != nil || can.InputHealth != "ok" {
		out.Stress.BriefRowState = briefDegraded("stress inputs are partial; unavailable sources remain explicit")
	}
	out.BriefRowState = briefSectionState("market", out.Regime.BriefRowState, out.Breadth.BriefRowState, out.Gamma.BriefRowState, out.Stress.BriefRowState)
	return out, can
}

func composeBriefGamma(env *rpc.GammaZeroSPXResult, sessionOpen bool, now time.Time) rpc.BriefGammaRow {
	row := rpc.BriefGammaRow{BriefRowState: briefUnavailable("dealer gamma cache is unavailable")}
	if env == nil {
		return row
	}
	if env.Status != rpc.GammaZeroStatusReady || env.Result == nil {
		cadence := gammaOperationalCadence(env, now)
		row.BriefRowState = briefDegraded("dealer gamma source is " + env.Status + " (" + cadence + ")")
		return row
	}
	computed := env.Result
	if spx := computed.PerIndex["SPX"]; spx != nil {
		computed = spx
	}
	row.BriefRowState = briefOK("SPX dealer zero-gamma versus spot")
	if computed.SpotUnderlying > 0 {
		row.Spot = new(computed.SpotUnderlying)
	}
	row.ZeroGamma, row.GapPct, row.GammaSign, row.AsOf = computed.ZeroGamma, computed.GapPct, computed.GammaSign, computed.AsOf
	cadence := gammaOperationalCadence(env, now)
	if !sessionOpen && cadence == rpc.DataCadenceNotDue {
		row.BriefRowState = briefOK("dealer gamma is last-completed-session context; no newer regular-session compute is due")
	} else if cadence == rpc.DataCadenceMissedSession || cadence == rpc.DataCadenceUnknown {
		row.BriefRowState = briefDegraded("dealer gamma process health is " + cadence)
	}
	if row.Spot == nil || (row.ZeroGamma == nil && row.GammaSign == "") {
		row.BriefRowState = briefDegraded("gamma result lacks a complete spot/zero-crossing classification")
	}
	if computed.Quality != nil && computed.Quality.Rankability != rpc.GammaRankabilityRankable &&
		!(cadence == rpc.DataCadenceNotDue && !sessionOpen && gammaRankabilityCadenceOnly(computed.Quality)) {
		row.BriefRowState = briefDegraded("gamma is context-only: " + computed.Quality.RankabilityReason)
	}
	return row
}

func gammaRankabilityCadenceOnly(quality *rpc.GammaSignalQuality) bool {
	if quality == nil {
		return false
	}
	reason := strings.ToLower(strings.TrimSpace(quality.RankabilityReason))
	return strings.Contains(reason, "market is closed") || strings.Contains(reason, "prior session")
}

func composeBriefCalendar(cal *rpc.MarketCalendarResult, events *rpc.MarketEventsResult, rules *rpc.RulesResult, calErr, eventsErr error, sessionOpen bool, borrowRelevant ...*bool) rpc.BriefCalendarSection {
	out := rpc.BriefCalendarSection{}
	if calErr != nil || cal == nil {
		out.Session.BriefRowState = briefUnavailable("market calendar unavailable: " + errText(calErr))
	} else {
		s := cal.Session
		out.Session = rpc.BriefSessionRow{BriefRowState: briefOK(nonEmptyString(s.Reason, "official session calendar")),
			Market: s.Market, State: s.State, IsOpen: s.IsOpen, Open: s.Open, Close: s.Close}
		if s.NextOpen != nil {
			out.Session.NextOpen = *s.NextOpen
		}
	}
	out.MarketEvents = briefMarketEventRows(events, rules, eventsErr, sessionOpen, borrowRelevant...)
	states := []rpc.BriefRowState{out.Session.BriefRowState}
	for _, row := range out.MarketEvents {
		states = append(states, row.BriefRowState)
	}
	out.BriefRowState = briefSectionState("calendar", states...)
	return out
}

func briefMarketEventRows(events *rpc.MarketEventsResult, rules *rpc.RulesResult, sourceErr error, sessionOpen bool, borrowRelevant ...*bool) []rpc.BriefMarketEventRow {
	kinds := []string{"earnings", "halt", "ssr", "borrow"}
	sets := map[string]map[string]struct{}{}
	for _, kind := range kinds {
		sets[kind] = map[string]struct{}{}
	}
	if rules != nil {
		for _, e := range rules.Earnings {
			// A terminal non-reporting issuer has no earnings to await; the
			if e.Status == rpc.EarningsStatusTerminalNonReporting {
				continue
			}
			if strings.TrimSpace(e.Symbol) != "" {
				sets["earnings"][strings.ToUpper(e.Symbol)] = struct{}{}
			}
		}
	}
	if events != nil {
		for _, flag := range events.Flags {
			id := strings.ToLower(flag.ID + " " + flag.Label)
			kind := ""
			switch {
			case strings.Contains(id, "halt") || strings.Contains(id, "luld"):
				kind = "halt"
			case strings.Contains(id, "reg_sho") || strings.Contains(id, "ssr"):
				kind = "ssr"
			case strings.Contains(id, "borrow"):
				kind = "borrow"
			}
			// Active and recent flags both count: a halt that lifted
			// minutes ago is still desk-relevant on a held name.
			if kind != "" {
				sets[kind][strings.ToUpper(flag.Symbol)] = struct{}{}
			}
		}
	}
	rows := make([]rpc.BriefMarketEventRow, 0, len(kinds))
	hardErr := sourceErr != nil || events == nil
	for _, kind := range kinds {
		syms := mapKeysSorted(sets[kind])
		flagged := fmt.Sprintf("%d held %s flagged", len(syms), pluralNoun(len(syms), "symbol"))
		if kind == "earnings" {
			flagged = fmt.Sprintf("%d held %s with earnings context", len(syms), pluralNoun(len(syms), "symbol"))
		}
		state := briefOK(flagged)
		if kind == "borrow" && len(borrowRelevant) > 0 && borrowRelevant[0] != nil && !*borrowRelevant[0] {
			state = briefOK(flagged + "; borrow-fee coverage is not required because there is no short-stock exposure")
			rows = append(rows, rpc.BriefMarketEventRow{BriefRowState: state, Kind: kind, Count: len(syms), Symbols: syms})
			continue
		}
		worst, refreshState, lastChecked := briefEventKindHealth(events, kind)
		switch {
		case hardErr || (kind == "earnings" && rules == nil):
			state = briefDegraded(fmt.Sprintf("%d known; one or more event sources are degraded", len(syms)))
		case worst == "" || worst == "ok":
			// healthy source: flagged copy stands as-is
		case !sessionOpen && (worst == rpc.SourceStatusStale || worst == rpc.SourceStatusUnknown) &&
			(kind != "borrow" || (refreshState == rpc.SourceRefreshNotDue && worst == rpc.SourceStatusUnknown)):
			// Only stale/unknown are quiet-eligible while closed: no fresh
			// update is expected, and the copy claims only what the code
			// check, so a zero is never asserted as current fact.
			inLast := fmt.Sprintf("%d held %s flagged in the last good data", len(syms), pluralNoun(len(syms), "symbol"))
			if len(syms) == 0 {
				inLast = "no flags in the last good data"
			}
			state = briefOK(inLast + "; no fresh update expected while the market is closed (source health " + worst + briefLastChecked(lastChecked) + ")")
		default:
			// Everything else — degraded, partial, any status outside the
			// known vocabulary, or any non-ok state during an open session —
			// keeps its weight: a source that misbehaved is not idle.
			state = briefDegraded(flagged + "; source health is " + worst + briefLastChecked(lastChecked))
		}
		if kind == "earnings" && len(syms) > 0 {
			unresolved := briefUnresolvedEarnings(rules)
			if len(unresolved) > 0 {
				state = briefDegraded(fmt.Sprintf("%d held earnings context unresolved (%s)", len(unresolved), strings.Join(unresolved, ", ")))
			}
			if unknown := briefUnknownEarningsRules(rules); len(unknown) > 0 {
				verb := "report"
				if len(unknown) == 1 {
					verb = "reports"
				}
				if len(unresolved) > 0 {
					state = briefAttention(fmt.Sprintf("%d held earnings context unresolved (%s) while the %s %s %s unknown; the rulebook cannot confirm the held-name earnings controls",
						len(unresolved), strings.Join(unresolved, ", "), strings.Join(unknown, " and "), pluralNoun(len(unknown), "rule"), verb))
				} else {
					state = briefAttention(fmt.Sprintf("%d held earnings upcoming while the %s %s %s unknown; the rulebook cannot confirm the held-name earnings controls",
						len(syms), strings.Join(unknown, " and "), pluralNoun(len(unknown), "rule"), verb))
				}
			}
			if applicability := briefEarningsApplicabilitySummary(rules); applicability != "" {
				state.Detail += "; " + applicability
			}
			if evidenceInfo := briefEarningsEvidenceInfo(rules); evidenceInfo != "" {
				state.Detail += "; " + evidenceInfo
			}
			if notice := briefWSHEntitlementNotice(rules); notice != "" {
				state.Detail += "; " + notice
			}
			if hint := briefEarningsOverrideHint(rules); hint != "" {
				state.Detail += "; " + hint
			}
		}
		rows = append(rows, rpc.BriefMarketEventRow{BriefRowState: state, Kind: kind, Count: len(syms), Symbols: syms})
	}
	return rows
}

func briefEarningsApplicabilitySummary(rules *rpc.RulesResult) string {
	if rules == nil {
		return ""
	}
	broker, terminal := 0, 0
	for _, info := range rules.Earnings {
		switch {
		case info.Source == "broker_identity" && info.Status == rpc.EarningsStatusNotApplicable:
			broker++
		case info.Source == "verified_terminal" && info.Status == rpc.EarningsStatusTerminalNonReporting:
			terminal++
		}
	}
	parts := make([]string, 0, 2)
	if broker > 0 {
		parts = append(parts, fmt.Sprintf("%d broker-proven nonissuer", broker))
	}
	if terminal > 0 {
		parts = append(parts, fmt.Sprintf("%d terminal/non-reporting", terminal))
	}
	if len(parts) == 0 {
		return ""
	}
	return "issuer earnings not applicable: " + strings.Join(parts, ", ")
}

func briefEarningsEvidenceInfo(rules *rpc.RulesResult) string {
	if rules == nil {
		return ""
	}
	for _, health := range rules.InputHealth {
		if health.Source == "earnings" && health.Status == rpc.SourceStatusOK && len(health.Notes) > 0 {
			return "earnings evidence informational issue: " + strings.Join(health.Notes, "; ")
		}
	}
	return ""
}

// briefEarningsOverrideHint names the one operator action that resolves a
// vendor-side date gap: no provider can supply a date the issuer has not
// announced, so the row says what to do when it lands instead of reading as
// an unactionable fault.
func briefEarningsOverrideHint(rules *rpc.RulesResult) string {
	if rules == nil {
		return ""
	}
	var symbols []string
	for _, info := range rules.Earnings {
		if info.Status == rpc.EarningsStatusNoDatePublished {
			symbols = append(symbols, info.Symbol)
		}
	}
	if len(symbols) == 0 {
		return ""
	}
	return fmt.Sprintf("no vendor has published a date for %s; when the issuer announces one, record it with the settings key features.rulebook.earnings_overrides", strings.Join(symbols, ", "))
}

func briefWSHEntitlementNotice(rules *rpc.RulesResult) string {
	if rules == nil {
		return ""
	}
	for _, info := range rules.Earnings {
		for _, provider := range info.Providers {
			failure := provider.LastFailure
			if provider.Provider != "ibkr_wsh" || failure == nil ||
				failure.Code != rpc.SourceFailureNotEntitled || failure.Retryable ||
				(failure.Stage != rpc.SourceFailureStageWSHMetadata && failure.Stage != rpc.SourceFailureStageWSHEvent) {
				continue
			}
			return "optional Wall Street Horizon earnings feed unavailable because this account lacks the WSH research subscription; Nasdaq remains active; names without a usable date stay unknown, never pass"
		}
	}
	return ""
}

// briefBorrowFeeRelevant returns nil when positions are unavailable, false for
// a known all-long book, and true only for actual short-stock exposure. Option
func briefBorrowFeeRelevant(pos *rpc.PositionsResult, posErr error) *bool {
	if posErr != nil || pos == nil {
		return nil
	}
	relevant := false
	for _, stock := range pos.Stocks {
		if briefPositionIsStock(stock) && stock.Quantity < 0 {
			relevant = true
			break
		}
	}
	if !relevant {
		for _, group := range pos.ByUnderlying {
			if group.Stock != nil && briefPositionIsStock(*group.Stock) && group.Stock.Quantity < 0 {
				relevant = true
				break
			}
		}
	}
	return &relevant
}

func briefPositionIsStock(position rpc.PositionView) bool {
	secType := strings.ToUpper(strings.TrimSpace(position.SecType))
	// Empty is a legacy stock projection. Explicit non-stock security types
	return secType == "" || secType == rpc.SecTypeStock || secType == "STK" || secType == "ETF"
}

// briefEventKindHealth maps one brief event kind to its own source-health rows
func briefEventKindHealth(events *rpc.MarketEventsResult, kind string) (string, string, time.Time) {
	if events == nil {
		return "", "", time.Time{}
	}
	match := func(source string) bool {
		source = strings.ToLower(source)
		switch kind {
		case "halt":
			return strings.Contains(source, "halt") || strings.Contains(source, "luld")
		case "ssr":
			return strings.Contains(source, "reg_sho") || strings.Contains(source, "ssr")
		case "borrow":
			return strings.Contains(source, "borrow")
		default:
			return false
		}
	}
	rank := map[string]int{rpc.SourceStatusOK: 0, rpc.SourceStatusStale: 1, rpc.SourceStatusUnknown: 2, rpc.SourceStatusPartial: 3, rpc.SourceStatusDegraded: 4}
	worst, worstRefresh, worstRank := "", "", -1
	var lastChecked time.Time
	for _, row := range events.SourceHealth {
		if !match(row.Source) {
			continue
		}
		if r, known := rank[row.Status]; known && r > worstRank {
			worst, worstRefresh, worstRank = row.Status, row.RefreshState, r
		} else if !known && row.Status != "" && worstRank < len(rank) {
			worst, worstRefresh, worstRank = row.Status, row.RefreshState, len(rank)
		}
		if row.AsOf.After(lastChecked) {
			lastChecked = row.AsOf
		}
	}
	return worst, worstRefresh, lastChecked
}

func briefLastChecked(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return "; last checked " + at.In(time.Local).Format("2006-01-02 15:04")
}

func pluralNoun(count int, noun string) string {
	if count == 1 {
		return noun
	}
	return noun + "s"
}

// briefUnknownEarningsRules cross-links the earnings event row to the rules
// that govern earnings behavior. This is disclosure only — it names which
// governing rules cannot currently be evaluated; it gates nothing.
func briefUnknownEarningsRules(rules *rpc.RulesResult) []string {
	if rules == nil {
		return nil
	}
	governing := map[string]bool{"catalyst_coverage": true, "earnings_size_freeze": true, "overwrite_earnings": true}
	var unknown []string
	for _, row := range rules.Rules {
		if governing[row.ID] && row.Status == risk.RuleStatusUnknown {
			unknown = append(unknown, strings.ReplaceAll(row.ID, "_", " "))
		}
	}
	slices.Sort(unknown)
	return unknown
}

func briefUnresolvedEarnings(rules *rpc.RulesResult) []string {
	if rules == nil {
		return nil
	}
	var unresolved []string
	for _, earnings := range rules.Earnings {
		if earnings.Status == rpc.EarningsStatusNotApplicable || earnings.Status == rpc.EarningsStatusTerminalNonReporting {
			continue
		}
		if earnings.Date != "" && earnings.Source != "unknown" && (earnings.Status == "" || earnings.Status == rpc.EarningsStatusDate) {
			continue
		}
		reason := nonEmptyString(earnings.Reason, nonEmptyString(earnings.Status, "not_observed"))
		unresolved = append(unresolved, strings.ToUpper(earnings.Symbol)+" ("+strings.ReplaceAll(reason, "_", " ")+")")
	}
	slices.Sort(unresolved)
	return unresolved
}

func briefAccountDataCurrent(authority *rpc.AccountDataAuthority) bool {
	return authority != nil && authority.Availability == rpc.AccountDataAvailable && authority.Freshness == rpc.AccountDataFreshnessCurrent
}

func (s *Server) composeBriefPortfolio(acct *rpc.AccountResult, pos *rpc.PositionsResult, acctErr, posErr error, sessionOpen bool) rpc.BriefPortfolioSection {
	out := rpc.BriefPortfolioSection{}
	if acctErr != nil || acct == nil || !briefAccountDataCurrent(acct.Authority) {
		detail := "account summary unavailable"
		if acctErr != nil {
			detail += ": " + errText(acctErr)
		} else if acct != nil {
			detail += ": current account data is not proven"
		}
		out.Account.BriefRowState = briefUnavailable(detail)
	} else {
		detail := "account summary in base currency"
		if !sessionOpen {
			detail = "account summary in base currency; market closed — daily P&L is the broker's running since-close value at off-session marks, not a completed-session result"
		}
		out.Account = rpc.BriefAccountRow{BriefRowState: briefOK(detail),
			DailyPnLBase: acct.DailyPnL, BaseCurrency: acct.BaseCurrency, AsOf: acct.AsOf}
		if acct.NetLiquidation > 0 {
			out.Account.EquityBase = new(acct.NetLiquidation)
		}
		if out.Account.EquityBase == nil || acct.DailyPnL == nil {
			out.Account.BriefRowState = briefDegraded("daily P&L is unavailable; equity remains present")
			if out.Account.EquityBase == nil {
				out.Account.Detail = "account equity is unavailable; zero was not substituted"
			}
		}
	}
	if posErr != nil || pos == nil || !briefAccountDataCurrent(pos.Authority) {
		detail := "positions unavailable"
		if posErr != nil {
			detail += ": " + errText(posErr)
		} else if pos != nil {
			detail += ": current position data is not proven"
		}
		out.Movers.BriefRowState = briefUnavailable(detail)
		out.PremiumAtRisk.BriefRowState = briefUnavailable("positions unavailable")
		out.HedgeCost.BriefRowState = briefUnavailable("positions unavailable")
	} else {
		out.Movers = briefMovers(pos, sessionOpen)
		out.PremiumAtRisk = briefPremiumAtRisk(pos, out.Account.BaseCurrency)
		out.HedgeCost = briefHedgeCost(pos, out.Account.BaseCurrency)
		// The premium-at-risk headline includes every long option leg. When a
		// that premium is unknown, so the row's confidence must say so even
		if out.HedgeCost.ExcludedLegs > 0 && out.PremiumAtRisk.Status == rpc.BriefStatusOK {
			out.PremiumAtRisk.BriefRowState = briefDegraded(fmt.Sprintf(
				"long-option market value in base currency; %d hedge-candidate %s cannot be classified, so the protective share of this premium is unknown",
				out.HedgeCost.ExcludedLegs, pluralNoun(out.HedgeCost.ExcludedLegs, "leg")))
		}
	}
	out.WorkingOrders = s.briefWorkingOrders()
	out.BriefRowState = briefSectionState("portfolio", out.Account.BriefRowState, out.Movers.BriefRowState,
		out.PremiumAtRisk.BriefRowState, out.HedgeCost.BriefRowState, out.WorkingOrders.BriefRowState)
	return out
}

// briefMovers aggregates daily P&L by underlying — the same basis as the
// top rows is disclosed so the row's implied total matches the account-level
func briefMovers(pos *rpc.PositionsResult, sessionOpen bool) rpc.BriefMoversRow {
	detail := "daily P&L by underlying, largest absolute first; position-level sums can differ from the account row by fees and FX"
	if !sessionOpen {
		detail += " (market closed — broker running values at off-session marks)"
	}
	row := rpc.BriefMoversRow{BriefRowState: briefOK(detail)}
	for _, group := range pos.ByUnderlying {
		if group.GroupDailyPnLBase != nil {
			row.Rows = append(row.Rows, rpc.BriefMover{Symbol: strings.ToUpper(group.Underlying), DailyPnLBase: *group.GroupDailyPnLBase})
		}
	}
	sort.SliceStable(row.Rows, func(i, j int) bool {
		return math.Abs(row.Rows[i].DailyPnLBase) > math.Abs(row.Rows[j].DailyPnLBase)
	})
	if len(row.Rows) > briefMoverLimit {
		var rest float64
		for _, mover := range row.Rows[briefMoverLimit:] {
			rest += mover.DailyPnLBase
		}
		row.OtherPnLBase = &rest
		row.OtherCount = len(row.Rows) - briefMoverLimit
		row.Rows = row.Rows[:briefMoverLimit]
	}
	if len(row.Rows) == 0 {
		row.BriefRowState = briefDegraded("no per-underlying daily P&L values are available")
	}
	return row
}

func briefPremiumAtRisk(pos *rpc.PositionsResult, base string) rpc.BriefMoneyCoverageRow {
	row := rpc.BriefMoneyCoverageRow{BriefRowState: briefOK("long-option market value in base currency"), BaseCurrency: base}
	var sum float64
	for _, p := range pos.Options {
		if p.Quantity <= 0 {
			continue
		}
		if p.MarketValueBase == nil {
			row.ExcludedLegs++
			continue
		}
		sum += *p.MarketValueBase
		row.IncludedLegs++
	}
	if row.IncludedLegs > 0 {
		row.AmountBase = new(sum)
	}
	if row.ExcludedLegs > 0 {
		row.BriefRowState = briefDegraded(fmt.Sprintf("%d long option %s excluded because base market value is unavailable", row.ExcludedLegs, pluralNoun(row.ExcludedLegs, "leg")))
	} else if row.IncludedLegs == 0 {
		row.BriefRowState = briefOK("no long option positions")
		zero := 0.0
		row.AmountBase = &zero
	}
	return row
}

func briefHedgeCost(pos *rpc.PositionsResult, base string) rpc.BriefMoneyCoverageRow {
	row := rpc.BriefMoneyCoverageRow{BriefRowState: briefOK("daily theta of long index puts"), BaseCurrency: base}
	pol := risk.DefaultRulebookPolicy()
	var sum float64
	for _, p := range pos.Options {
		candidate := p.Quantity > 0 && strings.EqualFold(p.Right, "P") && pol.IsHedgeSymbol(p.Symbol)
		if !candidate {
			continue
		}
		if p.Delta == nil || p.Underlying == nil || p.Theta == nil {
			row.ExcludedLegs++
			continue
		}
		value := *p.Theta * p.Quantity * float64(max(p.Multiplier, 1))
		if rate, ok := positionBaseRate(p, base); ok {
			value *= rate
		} else {
			row.ExcludedLegs++
			continue
		}
		sum += value
		row.IncludedLegs++
	}
	if row.IncludedLegs > 0 {
		row.AmountBase = new(sum)
	}
	if row.ExcludedLegs > 0 {
		row.BriefRowState = briefDegraded(fmt.Sprintf("%d long index-put %s excluded because Greeks, theta, or FX are unavailable", row.ExcludedLegs, pluralNoun(row.ExcludedLegs, "leg")))
	} else if row.IncludedLegs == 0 {
		row.BriefRowState = briefOK("no long index puts")
		zero := 0.0
		row.AmountBase = &zero
	}
	return row
}

func (s *Server) briefWorkingOrders() rpc.BriefCountRow {
	views, _, err := s.loadOrderViews()
	if err != nil {
		return rpc.BriefCountRow{BriefRowState: briefUnavailable("open-orders journal unavailable: " + err.Error())}
	}
	scope := s.currentBrokerStateScope()
	count := 0
	for _, view := range views {
		if view.Open && orderViewMatchesBrokerScope(view, scope) {
			count++
		}
	}
	return rpc.BriefCountRow{BriefRowState: briefOK("daemon open-orders journal view"), Count: &count}
}

func composeBriefRisk(policy *rpc.RiskPolicyResult, now time.Time) rpc.BriefRiskSection {
	out := rpc.BriefRiskSection{}
	if policy == nil || policy.Status == rpc.RiskPolicyStatusAbsent {
		state := briefUnavailable("risk constitution absent; capital controls are unapproved")
		out.Capital.BriefRowState, out.Latch.BriefRowState, out.Overrides.BriefRowState, out.PolicyDrift.BriefRowState = state, state, state, state
		out.BriefRowState = state
		return out
	}
	c := policy.Capital
	out.Capital = rpc.BriefCapitalRow{BriefRowState: briefOK("constitution capital state"), Tier: c.Tier,
		Enforcement: c.Enforcement, ConsumedPct: c.ConsumedPct, DrawdownBase: c.DrawdownBase,
		AdjustedPeakBase: c.AdjustedPeakBase, PeakAsOf: c.PeakAsOf, BaseCurrency: c.BaseCurrency}
	// The capital status derives from the values it shows: a breached tier or
	// a fully consumed budget can never render ok, whatever produced it. In
	blockDetail := "drawdown block tier is breached; risk-increasing orders are the enforcement target"
	if strings.EqualFold(c.Enforcement, "shadow") {
		blockDetail = "drawdown block tier is breached; shadow enforcement journals what would block — nothing is blocked yet, and reductions and closes stay available"
	}
	switch {
	case c.Tier == risk.CapitalTierBlock || c.BlockLatched || (c.ConsumedPct != nil && *c.ConsumedPct >= 100):
		out.Capital.BriefRowState = briefAttention(blockDetail)
	case c.Tier == risk.CapitalTierWarn:
		out.Capital.BriefRowState = briefAttention("advisory drawdown tier is breached; consumed risk capital needs eyes")
	case len(policy.Unapproved) > 0 || c.Tier == risk.CapitalTierUnapproved || c.ConsumedPct == nil:
		out.Capital.BriefRowState = briefDegraded("one or more capital inputs or policy decisions are unapproved")
	case c.Tier == risk.CapitalTierUnknown:
		out.Capital.BriefRowState = briefDegraded("capital state cannot be evaluated from current inputs")
	}
	out.Latch = rpc.BriefLatchRow{BriefRowState: briefOK("drawdown latch is not engaged"), Latched: c.BlockLatched, At: c.LatchedAt,
		Provisional: c.LatchProvisional, ConsumedPctAtLatch: c.LatchConsumedPct}
	if c.BlockLatched {
		age := max(int(now.Sub(c.LatchedAt).Hours()/24), 0)
		out.Latch.AgeDays = &age
		// An engaged latch is an active risk state, not a healthy steady
		if c.LatchProvisional {
			out.Latch.BriefRowState = briefAttention("drawdown latch is engaged provisionally; the broker statement covering the latch day will confirm it or dissolve it")
		} else {
			out.Latch.BriefRowState = briefAttention("drawdown latch is engaged and remains so until a human reset")
		}
	}
	out.Overrides.BriefRowState = briefOK("no active overrides")
	for _, o := range policy.Overrides {
		if o.Active && !now.After(o.ExpiresAt) {
			out.Overrides.Rows = append(out.Overrides.Rows, rpc.BriefOverride{Control: o.Control, ExpiresAt: o.ExpiresAt})
		}
	}
	if len(out.Overrides.Rows) > 0 {
		verb := "widen"
		if len(out.Overrides.Rows) == 1 {
			verb = "widens"
		}
		out.Overrides.BriefRowState = briefAttention(fmt.Sprintf("%d active %s temporarily %s policy controls",
			len(out.Overrides.Rows), pluralNoun(len(out.Overrides.Rows), "override"), verb))
	}
	out.PolicyDrift.SignoffRequired = policy.SignoffRequired
	out.PolicyDrift.BriefRowState = briefOK("all approval pins match")
	unavailable := 0
	for _, pin := range policy.Inventory {
		if pin.Status != "match" {
			out.PolicyDrift.Rows = append(out.PolicyDrift.Rows, pin)
		}
		if pin.Status == "unavailable" {
			unavailable++
		}
	}
	switch {
	case unavailable > 0:
		out.PolicyDrift.BriefRowState = briefDegraded(fmt.Sprintf("%d sibling-policy pin(s) cannot read a live identity", unavailable))
	case len(out.PolicyDrift.Rows) == 0:
	case policy.SignoffRequired:
		out.PolicyDrift.BriefRowState = briefDegraded(fmt.Sprintf("%d sibling-policy approval pin(s) do not match", len(out.PolicyDrift.Rows)))
	default:
		out.PolicyDrift.BriefRowState = briefOK(fmt.Sprintf("%d sibling-policy pin(s) differ from live; sign-off is not required", len(out.PolicyDrift.Rows)))
	}
	out.BriefRowState = briefSectionState("risk and limits", out.Capital.BriefRowState, out.Latch.BriefRowState,
		out.Overrides.BriefRowState, out.PolicyDrift.BriefRowState)
	return out
}

func (s *Server) composeBriefProcessForAuthority(policy *rpc.RiskPolicyResult, constitution *risk.Constitution, recon *rpc.ReconResult, rules *rpc.RulesResult, authority nudgeAuthorityState, now time.Time) rpc.BriefProcessSection {
	out := rpc.BriefProcessSection{}
	if policy == nil {
		out.Reconcile.BriefRowState = briefUnavailable("risk policy unavailable")
	} else {
		capital := policy.Capital
		out.Reconcile = rpc.BriefReconcileRow{BriefRowState: briefOK("reconcile evidence and shared constitution clock"),
			LastReconciledAt: capital.LastReconciledAt, Source: capital.LastReconcileSource}
		clock := s.riskCapital.UnreconciledClockForScope(constitution, now, authority.scope)
		if !clock.Approved {
			out.Reconcile.BriefRowState = briefDegraded("capital.max_unreconciled_days is unapproved")
		} else if capital.LastReconciledAt.IsZero() {
			out.Reconcile.BriefRowState = briefDegraded("no reconcile evidence has been recorded")
		} else {
			out.Reconcile.Deadline, out.Reconcile.DaysRemaining = clock.Deadline, clock.DaysRemaining
			if clock.Stale {
				out.Reconcile.BriefRowState = briefDegraded("reconcile evidence is past its declared horizon")
			}
		}
	}
	if recon == nil {
		out.AutoExtend.BriefRowState = briefUnavailable("reconciliation report unavailable")
	} else {
		out.AutoExtend = rpc.BriefAutoExtendRow{BriefRowState: briefOK("no automatic extension recorded"),
			ReportID: recon.LastAutoExtendReportID, At: recon.LastAutoExtendedAt}
		if recon.LastAutoExtendReportID != "" {
			out.AutoExtend.Detail = "latest clean-report automatic extension"
		}
	}
	out.Rules = briefRulesStatus(rules)
	if constitution != nil && constitution.PolicyVersion >= 4 {
		evaluation := s.governanceMonthlyPulseForAuthority(authority, constitution, recon, now)
		out.MonthlyPulse = &rpc.BriefMonthlyPulseRow{
			Status: evaluation.Status, Month: evaluation.Month, DueAt: evaluation.DueAt,
		}
	}
	out.BriefRowState = briefProcessSectionState(out)
	return out
}

func briefProcessSectionState(process rpc.BriefProcessSection) rpc.BriefRowState {
	rows := []rpc.BriefRowState{
		process.Reconcile.BriefRowState, process.AutoExtend.BriefRowState,
		process.Rules.BriefRowState,
	}
	if process.MonthlyPulse != nil {
		rows = append(rows, briefMonthlyPulseRollupState(process.MonthlyPulse.Status))
	}
	return briefSectionState("process", rows...)
}

// briefMonthlyPulseRollupState maps the monthly-pulse status vocabulary onto a
// section-rollup row state. Shared so the Ready movement and the legacy process
func briefMonthlyPulseRollupState(status string) rpc.BriefRowState {
	switch status {
	case rpc.BriefMonthlyPulseNotDue, rpc.BriefMonthlyPulseCompleted:
		return briefOK("monthly pulse is current")
	default:
		return briefDegraded("monthly pulse is blocked by policy evidence")
	}
}

func policyPinsReady(inventory []rpc.PolicyPinStatus) bool {
	return policyPinsReadable(inventory, true)
}

func policyPinsReadable(inventory []rpc.PolicyPinStatus, requireMatch bool) bool {
	if len(inventory) != 3 {
		return false
	}
	want := map[string]bool{"rulebook": false, "protection": false, "stress": false}
	for _, pin := range inventory {
		statusReadable := pin.Status == "match" || (!requireMatch && pin.Status == "drift")
		if _, known := want[pin.Policy]; !known || want[pin.Policy] || !statusReadable || pin.PinnedID == "" || pin.PinnedVersion == "" || pin.LiveID == "" || pin.LiveVersion == "" {
			return false
		}
		want[pin.Policy] = true
	}
	return true
}

func briefRulesStatus(current *rpc.RulesResult) rpc.BriefRulesRow {
	row := rpc.BriefRulesRow{BriefRowState: briefOK("all due current rulebook checks pass")}
	if current == nil {
		row.BriefRowState = briefUnavailable("rulebook snapshot unavailable")
		return row
	}
	for _, r := range current.Rules {
		mode := r.Mode
		if mode == "" {
			mode = risk.RuleModeAlert
		}
		if mode == risk.RuleModeOff {
			row.NotEvaluated++
			continue
		}
		if mode == risk.RuleModeTrack {
			if r.Status != risk.RuleStatusPass && r.Status != risk.RuleStatusNotEvaluated {
				row.Track++
			}
			if r.Status == risk.RuleStatusPass {
				row.Pass++
			}
			continue
		}
		switch r.Status {
		case risk.RuleStatusPass:
			row.Pass++
		case risk.RuleStatusInfo:
			row.Info++
		case risk.RuleStatusWatch:
			row.Watch++
		case risk.RuleStatusAct:
			row.Act++
		case risk.RuleStatusUnknown:
			row.Unknown++
		case risk.RuleStatusNotEvaluated:
			row.NotEvaluated++
		default:
			// An unrecognized future status remains fail-closed until every
			// brief consumer learns its semantics.
			row.Unknown++
		}
	}
	switch {
	case row.Act > 0:
		row.BriefRowState = briefAttention(fmt.Sprintf("%d current %s require action", row.Act, pluralNoun(row.Act, "rule")))
	case row.Watch > 0 || row.Unknown > 0 || current.Status == "degraded":
		row.BriefRowState = briefDegraded(fmt.Sprintf("current rulebook: %d watch, %d unknown, %d not evaluated", row.Watch, row.Unknown, row.NotEvaluated))
	case row.Track > 0:
		row.Detail = fmt.Sprintf("no alerting rule needs attention; %d tracked finding(s), %d not evaluated", row.Track, row.NotEvaluated)
	case row.NotEvaluated > 0:
		row.Detail = fmt.Sprintf("all due current rulebook checks pass; %d not evaluated", row.NotEvaluated)
	}
	return row
}

func briefContentFingerprint(res *rpc.BriefResult) string {
	projection := struct {
		Review rpc.BriefReviewSection
		Ready  rpc.BriefReadySection
	}{res.Review, res.Ready}
	raw, _ := json.Marshal(projection)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func briefOK(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusOK, Detail: nonEmptyString(detail, "available")}
}
func briefAttention(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusAttention, Detail: nonEmptyString(detail, "needs attention")}
}
func briefDegraded(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusDegraded, Detail: nonEmptyString(detail, "degraded")}
}
func briefUnavailable(detail string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: rpc.BriefStatusUnavailable, Detail: nonEmptyString(detail, "unavailable")}
}

// briefSectionState rolls a section up to its worst child — attention
// so an all-green header can never sit above a row that needs eyes.
func briefSectionState(name string, rows ...rpc.BriefRowState) rpc.BriefRowState {
	ok, attention, unavailable := 0, 0, 0
	for _, row := range rows {
		switch row.Status {
		case rpc.BriefStatusOK:
			ok++
		case rpc.BriefStatusAttention:
			attention++
		case rpc.BriefStatusUnavailable:
			unavailable++
		}
	}
	degraded := len(rows) - ok - attention - unavailable
	if len(rows) > 0 && unavailable == len(rows) {
		return briefUnavailable(name + " section unavailable")
	}
	if attention > 0 {
		verb := "need"
		if attention == 1 {
			verb = "needs"
		}
		detail := fmt.Sprintf("%s: %d of %d %s %s attention", name, attention, len(rows), pluralNoun(len(rows), "row"), verb)
		if degraded+unavailable > 0 {
			detail += fmt.Sprintf("; %d degraded or unavailable", degraded+unavailable)
		}
		return briefAttention(detail)
	}
	if ok != len(rows) {
		return briefDegraded(fmt.Sprintf("%s: %d of %d %s degraded or unavailable", name, degraded+unavailable, len(rows), pluralNoun(len(rows), "row")))
	}
	return briefOK(name + " section complete")
}

func mapKeysSorted(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		if key != "" {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}
