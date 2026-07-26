package cli

import (
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	opportunityResearchSignalSource = "research_opportunity_features"
	opportunityResearchRankMetric   = "tuning_fired_vs_candidate_avg_lift_pct"
)

type opportunityResearchOptions struct {
	Plan     string
	MaxSlots int
	Ledger   *opportunityPriceBarLedger
}

type opportunitySignalPlan struct {
	ID          string
	Family      string
	Description string
	Hypothesis  string
	Evaluate    func(OpportunityPointInTimeFeatures) OpportunityBacktestSignal
}

// OpportunityResearchPlan describes one named signal hypothesis evaluated by
// the offline opportunity research workflow.
type OpportunityResearchPlan struct {
	ID          string `json:"id"`
	Family      string `json:"family,omitempty"`
	Description string `json:"description,omitempty"`
	Hypothesis  string `json:"hypothesis,omitempty"`
}

// OpportunityResearchResult ranks evaluated plans and retains their metrics,
// evidence status, findings, and simulation outputs.
type OpportunityResearchResult struct {
	RunAt          time.Time                       `json:"run_at"`
	Rows           int                             `json:"rows"`
	PlansEvaluated int                             `json:"plans_evaluated"`
	RankedBy       string                          `json:"ranked_by"`
	PlanMode       string                          `json:"plan_mode"`
	Plans          []OpportunityResearchPlanResult `json:"plans"`
	Findings       []string                        `json:"findings,omitempty"`
	NotAdvice      string                          `json:"not_advice"`
}

// OpportunityResearchPlanResult contains tuning and holdout results for one
// ranked research plan.
type OpportunityResearchPlanResult struct {
	Rank           int                           `json:"rank"`
	Plan           OpportunityResearchPlan       `json:"plan"`
	RankValuePct   *float64                      `json:"rank_value_pct,omitempty"`
	Metrics        OpportunityBacktestMetrics    `json:"metrics"`
	TuningMetrics  OpportunityBacktestMetrics    `json:"tuning_metrics"`
	HoldoutMetrics OpportunityBacktestMetrics    `json:"holdout_metrics"`
	Simulation     OpportunityBacktestSimulation `json:"simulation"`
	Evidence       OpportunityBacktestEvidence   `json:"evidence"`
	Findings       []string                      `json:"findings,omitempty"`
}

// OpportunityBacktestDiagnostics groups missed or blocked observations by
// reason and feature.
type OpportunityBacktestDiagnostics struct {
	Reasons  []OpportunityBacktestDiagnosticBucket `json:"reasons,omitempty"`
	Features []OpportunityBacktestDiagnosticBucket `json:"features,omitempty"`
}

// IsZero reports whether the diagnostic contains no reason or feature buckets.
func (d OpportunityBacktestDiagnostics) IsZero() bool {
	return len(d.Reasons) == 0 && len(d.Features) == 0
}

// OpportunityBacktestDiagnosticBucket aggregates metrics for one diagnostic
// class and optional research plan.
type OpportunityBacktestDiagnosticBucket struct {
	Name    string                     `json:"name"`
	Class   string                     `json:"class,omitempty"`
	PlanID  string                     `json:"plan_id,omitempty"`
	Metrics OpportunityBacktestMetrics `json:"metrics"`
}

func runOpportunityResearch(rows []OpportunityPointInTimeRow, runAt time.Time, opts opportunityResearchOptions) (OpportunityResearchResult, error) {
	if runAt.IsZero() {
		runAt = time.Now()
	}
	plans, err := selectOpportunityResearchPlans(opts.Plan)
	if err != nil {
		return OpportunityResearchResult{}, err
	}
	out := OpportunityResearchResult{
		RunAt:     runAt,
		Rows:      len(rows),
		RankedBy:  opportunityResearchRankMetric,
		PlanMode:  opportunityResearchPlanMode(opts.Plan),
		NotAdvice: "Research diagnostic only; not investment advice, not alpha proof, and not a trade recommendation.",
	}
	for _, plan := range plans {
		observations := buildOpportunityResearchObservations(rows, plan)
		res := runOpportunityBacktestWithSlots(observations, runAt, opts.MaxSlots)
		if opts.Ledger != nil {
			if err := applyOpportunityMarkToMarketSimulation(&res, *opts.Ledger); err != nil {
				return OpportunityResearchResult{}, fmt.Errorf("%s mark-to-market: %w", plan.ID, err)
			}
		}
		tuning := opportunityBacktestMetricsForRows(opportunityBacktestRowsByResearchSplit(res.Observations, "tuning"))
		holdout := opportunityBacktestMetricsForRows(opportunityBacktestRowsByResearchSplit(res.Observations, "holdout"))
		rankValue, ok := opportunityResearchPlanRankValue(tuning)
		var rankPtr *float64
		if ok {
			rankPtr = &rankValue
		}
		out.Plans = append(out.Plans, OpportunityResearchPlanResult{
			Plan:           opportunityResearchPlanSummary(plan),
			RankValuePct:   rankPtr,
			Metrics:        res.Metrics,
			TuningMetrics:  tuning,
			HoldoutMetrics: holdout,
			Simulation:     res.Simulation,
			Evidence:       res.Evidence,
			Findings:       res.Findings,
		})
	}
	sort.SliceStable(out.Plans, func(i, j int) bool {
		vi, iok := opportunityResearchPlanRankValue(out.Plans[i].TuningMetrics)
		vj, jok := opportunityResearchPlanRankValue(out.Plans[j].TuningMetrics)
		switch {
		case iok && jok && vi != vj:
			return vi > vj
		case iok != jok:
			return iok
		}
		ai := opportunityResearchPlanAvgNet(out.Plans[i].TuningMetrics)
		aj := opportunityResearchPlanAvgNet(out.Plans[j].TuningMetrics)
		if ai != aj {
			return ai > aj
		}
		return out.Plans[i].Plan.ID < out.Plans[j].Plan.ID
	})
	for i := range out.Plans {
		out.Plans[i].Rank = i + 1
	}
	out.PlansEvaluated = len(out.Plans)
	out.Findings = opportunityResearchFindings(out)
	return out, nil
}

func renderOpportunityResearchText(env *Env, out io.Writer, r *OpportunityResearchResult) int {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Opportunity Research  ·  %d scored rows  ·  %d plans  ·  ranked by %s\n", r.Rows, r.PlansEvaluated, r.RankedBy)
	fmt.Fprintln(out)
	if len(r.Plans) > 0 {
		header := fmt.Sprintf("  %-3s %-30s %-18s %5s %8s %8s %5s %8s %8s %-22s",
			"#", "PLAN", "FAMILY", "TFIRE", "TLIFT", "TAVG", "HFIRE", "HLIFT", "HAVG", "EVIDENCE")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim(strings.Repeat("-", visibleLen(header))))
		for _, plan := range r.Plans {
			fmt.Fprintf(out, "  %-3d %-30s %-18s %5d %8s %8s %5d %8s %8s %-22s\n",
				plan.Rank,
				truncateVisible(plan.Plan.ID, 30),
				truncateVisible(plan.Plan.Family, 18),
				plan.TuningMetrics.SignalFired,
				formatBacktestPercent(plan.TuningMetrics.FiredVsCandidateAvgLiftPct),
				formatBacktestPercent(plan.TuningMetrics.AvgNetExcessReturnPct),
				plan.HoldoutMetrics.SignalFired,
				formatBacktestPercent(plan.HoldoutMetrics.FiredVsCandidateAvgLiftPct),
				formatBacktestPercent(plan.HoldoutMetrics.AvgNetExcessReturnPct),
				truncateVisible(plan.Evidence.Status, 22),
			)
		}
		fmt.Fprintln(out)
		for _, plan := range r.Plans {
			if len(plan.Evidence.Reasons) == 0 {
				continue
			}
			fmt.Fprintf(out, "  %-12s %-30s %s\n",
				"Gate",
				truncateVisible(plan.Plan.ID, 30),
				truncateVisible(plan.Evidence.Reasons[0], 96),
			)
		}
		if len(r.Plans) > 0 {
			fmt.Fprintln(out)
		}
	}
	if len(r.Findings) > 0 {
		fmt.Fprintln(out, "Findings")
		for _, finding := range r.Findings {
			fmt.Fprintf(out, "  - %s\n", finding)
		}
		fmt.Fprintln(out)
	}
	if r.NotAdvice != "" {
		fmt.Fprintln(out, env.dim("  "+r.NotAdvice))
		fmt.Fprintln(out)
	}
	return 0
}

func opportunityResearchFindings(r OpportunityResearchResult) []string {
	var findings []string
	findings = append(findings, "Plans are ranked on tuning rows only; holdout metrics are audit evidence, not a selection input.")
	if len(r.Plans) == 0 {
		return append(findings, "No research plans were evaluated.")
	}
	top := r.Plans[0]
	findings = append(findings, fmt.Sprintf("Top tuning plan is %s with tuning lift %s and holdout lift %s.",
		top.Plan.ID,
		formatBacktestPercent(top.TuningMetrics.FiredVsCandidateAvgLiftPct),
		formatBacktestPercent(top.HoldoutMetrics.FiredVsCandidateAvgLiftPct),
	))
	if top.HoldoutMetrics.SignalFired == 0 {
		findings = append(findings, "Top tuning plan has no holdout fires; it is not deployable evidence.")
	}
	if top.HoldoutMetrics.FiredVsCandidateAvgLiftPct == nil || *top.HoldoutMetrics.FiredVsCandidateAvgLiftPct <= 0 {
		findings = append(findings, "Top tuning plan does not show positive holdout lift versus all candidates.")
	}
	return findings
}

func buildOpportunityResearchObservations(rows []OpportunityPointInTimeRow, plan opportunitySignalPlan) []OpportunityBacktestObservation {
	out := make([]OpportunityBacktestObservation, 0, len(rows))
	for _, row := range rows {
		obs := buildOpportunityBacktestObservation(row)
		obs.Signal = plan.Evaluate(row.Features)
		out = append(out, obs)
	}
	return out
}

func opportunityBacktestDiagnostics(observations []OpportunityBacktestObservation, rows []OpportunityBacktestRowResult) OpportunityBacktestDiagnostics {
	return OpportunityBacktestDiagnostics{
		Reasons:  opportunityReasonDiagnostics(rows),
		Features: opportunityFeatureDiagnostics(observations),
	}
}

func opportunityReasonDiagnostics(rows []OpportunityBacktestRowResult) []OpportunityBacktestDiagnosticBucket {
	accs := map[string]*opportunityBacktestAccumulator{}
	for _, row := range rows {
		for _, reason := range compactOpportunityReasons(row.SignalReasons) {
			if accs[reason] == nil {
				accs[reason] = &opportunityBacktestAccumulator{}
			}
			accs[reason].add(row)
		}
	}
	names := make([]string, 0, len(accs))
	for name := range accs {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		mi := accs[names[i]].metrics.Observations
		mj := accs[names[j]].metrics.Observations
		if mi != mj {
			return mi > mj
		}
		return names[i] < names[j]
	})
	out := make([]OpportunityBacktestDiagnosticBucket, 0, len(names))
	for _, name := range names {
		out = append(out, OpportunityBacktestDiagnosticBucket{
			Name:    name,
			Class:   opportunityReasonClass(name),
			Metrics: accs[name].result(),
		})
	}
	return out
}

func opportunityFeatureDiagnostics(observations []OpportunityBacktestObservation) []OpportunityBacktestDiagnosticBucket {
	if len(observations) == 0 {
		return nil
	}
	plans := opportunitySignalPlans()
	out := make([]OpportunityBacktestDiagnosticBucket, 0, len(plans))
	for _, plan := range plans {
		acc := &opportunityBacktestAccumulator{}
		for _, obs := range observations {
			researchObs := obs
			researchObs.Signal = plan.Evaluate(obs.Features)
			acc.add(runOpportunityBacktestObservation(researchObs))
		}
		out = append(out, OpportunityBacktestDiagnosticBucket{
			Name:    plan.ID,
			Class:   plan.Family,
			PlanID:  plan.ID,
			Metrics: acc.result(),
		})
	}
	return out
}

func opportunityBacktestMetricsForRows(rows []OpportunityBacktestRowResult) OpportunityBacktestMetrics {
	acc := &opportunityBacktestAccumulator{}
	for _, row := range rows {
		acc.add(row)
	}
	return acc.result()
}

func opportunityBacktestRowsByResearchSplit(rows []OpportunityBacktestRowResult, split string) []OpportunityBacktestRowResult {
	split = cleanOpportunityBacktestSplit(split)
	out := make([]OpportunityBacktestRowResult, 0, len(rows))
	for _, row := range rows {
		switch split {
		case "holdout":
			if row.Holdout {
				out = append(out, row)
			}
		case "tuning":
			if !row.Holdout && !opportunityBacktestSplitUnknown(row.Split) {
				out = append(out, row)
			}
		default:
			out = append(out, row)
		}
	}
	return out
}

func opportunityResearchPlanRankValue(metrics OpportunityBacktestMetrics) (float64, bool) {
	if metrics.SignalFired == 0 || metrics.FiredVsCandidateAvgLiftPct == nil || math.IsNaN(*metrics.FiredVsCandidateAvgLiftPct) {
		return 0, false
	}
	return *metrics.FiredVsCandidateAvgLiftPct, true
}

func opportunityResearchPlanAvgNet(metrics OpportunityBacktestMetrics) float64 {
	if metrics.AvgNetExcessReturnPct == nil || math.IsNaN(*metrics.AvgNetExcessReturnPct) {
		return math.Inf(-1)
	}
	return *metrics.AvgNetExcessReturnPct
}

func opportunityResearchPlanMode(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "all"
	}
	return raw
}

func selectOpportunityResearchPlans(raw string) ([]opportunitySignalPlan, error) {
	plans := opportunitySignalPlans()
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "all") {
		return plans, nil
	}
	byID := map[string]opportunitySignalPlan{}
	for _, plan := range plans {
		byID[plan.ID] = plan
	}
	var out []opportunitySignalPlan
	seen := map[string]struct{}{}
	for id := range strings.SplitSeq(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		plan, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown research plan %q; use --list-plans", id)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, plan)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--plan must name at least one research plan or all")
	}
	return out, nil
}

func opportunityResearchPlanSummaries() []OpportunityResearchPlan {
	plans := opportunitySignalPlans()
	out := make([]OpportunityResearchPlan, 0, len(plans))
	for _, plan := range plans {
		out = append(out, opportunityResearchPlanSummary(plan))
	}
	return out
}

func opportunityResearchPlanSummary(plan opportunitySignalPlan) OpportunityResearchPlan {
	return OpportunityResearchPlan{
		ID:          plan.ID,
		Family:      plan.Family,
		Description: plan.Description,
		Hypothesis:  plan.Hypothesis,
	}
}

func opportunitySignalPlans() []opportunitySignalPlan {
	return []opportunitySignalPlan{
		{
			ID:          opportunityBuilderSignalKind,
			Family:      "baseline",
			Description: "Existing constructive-breakout rule.",
			Hypothesis:  "A clean breakout with positive relative strength and liquidity should select better candidates.",
			Evaluate:    opportunityConstructiveBreakoutSignal,
		},
		{
			ID:          "rs63_positive_v1",
			Family:      "relative_strength",
			Description: "Context/liquidity gates plus RS63 > 0 versus benchmark.",
			Hypothesis:  "Shorter relative-strength continuation beats the broad candidate baseline.",
			Evaluate: func(f OpportunityPointInTimeFeatures) OpportunityBacktestSignal {
				reasons := opportunityResearchContextReasons(f)
				if f.RS63D == nil || *f.RS63D <= 0 {
					reasons = append(reasons, "rs_63d_not_positive")
				}
				return opportunityResearchSignal("rs63_positive_v1", reasons)
			},
		},
		{
			ID:          "rs126_positive_v1",
			Family:      "relative_strength",
			Description: "Context/liquidity gates plus RS126 > 0 versus benchmark.",
			Hypothesis:  "Intermediate relative-strength continuation beats the broad candidate baseline.",
			Evaluate: func(f OpportunityPointInTimeFeatures) OpportunityBacktestSignal {
				reasons := opportunityResearchContextReasons(f)
				if f.RS126D == nil || *f.RS126D <= 0 {
					reasons = append(reasons, "rs_126d_not_positive")
				}
				return opportunityResearchSignal("rs126_positive_v1", reasons)
			},
		},
		{
			ID:          "rs63_126_positive_v1",
			Family:      "relative_strength",
			Description: "Context/liquidity gates plus both RS63 and RS126 positive.",
			Hypothesis:  "Requiring short and intermediate relative strength reduces false continuation signals.",
			Evaluate: func(f OpportunityPointInTimeFeatures) OpportunityBacktestSignal {
				reasons := opportunityResearchContextReasons(f)
				if f.RS63D == nil || *f.RS63D <= 0 {
					reasons = append(reasons, "rs_63d_not_positive")
				}
				if f.RS126D == nil || *f.RS126D <= 0 {
					reasons = append(reasons, "rs_126d_not_positive")
				}
				return opportunityResearchSignal("rs63_126_positive_v1", reasons)
			},
		},
		{
			ID:          "pullback_uptrend_rs63_v1",
			Family:      "pullback_trend",
			Description: "Above 200dma, no more than 5% above 50dma, RS63 > 0.",
			Hypothesis:  "Pullbacks inside an uptrend outperform raw breakout chasing.",
			Evaluate: func(f OpportunityPointInTimeFeatures) OpportunityBacktestSignal {
				reasons := opportunityResearchPullbackUptrendRS63Reasons(f)
				return opportunityResearchSignal("pullback_uptrend_rs63_v1", reasons)
			},
		},
		{
			ID:          "pullback_uptrend_rs63_macro_veto_v1",
			Family:      "pullback_trend",
			Description: "Pullback-uptrend RS63 rule, vetoed by stress/risk-off regime context.",
			Hypothesis:  "Relative-strength pullbacks work best when broad-market stress is not confirmed.",
			Evaluate: func(f OpportunityPointInTimeFeatures) OpportunityBacktestSignal {
				reasons := opportunityResearchPullbackUptrendRS63Reasons(f)
				reasons = append(reasons, opportunityResearchMacroVetoReasons(f)...)
				return opportunityResearchSignal("pullback_uptrend_rs63_macro_veto_v1", reasons)
			},
		},
		{
			ID:          "avoid_extended_rs63_v1",
			Family:      "extension_filter",
			Description: "RS63 > 0 while avoiding large 50/200dma extension and event-gap/chase risk.",
			Hypothesis:  "The breakout baseline is hurt by extended entries; extension filters improve selection.",
			Evaluate: func(f OpportunityPointInTimeFeatures) OpportunityBacktestSignal {
				reasons := opportunityResearchContextReasons(f)
				pct50 := opportunityFeaturePctAbove50(f)
				pct200 := opportunityFeaturePctAbove200(f)
				if f.RS63D == nil || *f.RS63D <= 0 {
					reasons = append(reasons, "rs_63d_not_positive")
				}
				if pct50 == nil {
					reasons = append(reasons, "pct_above_50dma_missing")
				} else if *pct50 > 0.10 {
					reasons = append(reasons, "extended_from_50dma")
				}
				if pct200 == nil {
					reasons = append(reasons, "pct_above_200dma_missing")
				} else if *pct200 > 0.40 {
					reasons = append(reasons, "extended_from_200dma")
				}
				if f.EventGapPct != nil && *f.EventGapPct > opportunityBuilderMaxGapPct {
					reasons = append(reasons, "event_gap_too_large")
				}
				if f.ExtendedChaseRisk {
					reasons = append(reasons, "extended_chase_risk")
				}
				return opportunityResearchSignal("avoid_extended_rs63_v1", reasons)
			},
		},
	}
}

func opportunityResearchPullbackUptrendRS63Reasons(f OpportunityPointInTimeFeatures) []string {
	reasons := opportunityResearchContextReasons(f)
	pct50 := opportunityFeaturePctAbove50(f)
	pct200 := opportunityFeaturePctAbove200(f)
	if pct200 == nil {
		reasons = append(reasons, "pct_above_200dma_missing")
	} else if *pct200 <= 0 {
		reasons = append(reasons, "below_200dma")
	}
	if pct50 == nil {
		reasons = append(reasons, "pct_above_50dma_missing")
	} else if *pct50 > 0.05 {
		reasons = append(reasons, "too_far_above_50dma")
	}
	if f.RS63D == nil || *f.RS63D <= 0 {
		reasons = append(reasons, "rs_63d_not_positive")
	}
	return reasons
}

func opportunityResearchMacroVetoReasons(f OpportunityPointInTimeFeatures) []string {
	if f.Macro == nil {
		return []string{"macro_context_missing"}
	}
	if strings.TrimSpace(f.Macro.Error) != "" {
		return []string{"macro_context_error"}
	}
	tone := strings.ToLower(strings.TrimSpace(f.Macro.Tone))
	switch tone {
	case rpc.RegimeToneNormal, rpc.RegimeToneWatch:
		return nil
	case rpc.RegimeToneStress:
		return []string{"macro_stress_veto"}
	case rpc.RegimeToneRiskOff:
		return []string{"macro_risk_off_veto"}
	case rpc.RegimeToneDataQuality:
		return []string{"macro_data_quality_veto"}
	case "":
		return []string{"macro_tone_missing"}
	default:
		return []string{"macro_tone_unknown"}
	}
}

func opportunityResearchSignal(id string, reasons []string) OpportunityBacktestSignal {
	reasons = compactOpportunityReasons(reasons)
	fired := len(reasons) == 0
	confidence := "low"
	if fired {
		confidence = "medium"
		reasons = []string{"passed_" + id}
	}
	return OpportunityBacktestSignal{
		Fired:      fired,
		Kind:       id,
		Confidence: confidence,
		Source:     opportunityResearchSignalSource,
		Reasons:    reasons,
	}
}

func opportunityResearchContextReasons(f OpportunityPointInTimeFeatures) []string {
	var reasons []string
	dataQuality := strings.ToLower(strings.TrimSpace(f.DataQuality))
	switch dataQuality {
	case "ok":
	case "":
		reasons = append(reasons, "data_quality_missing")
	default:
		reasons = append(reasons, "data_quality_not_ok")
	}
	dataType := strings.ToLower(strings.TrimSpace(f.DataType))
	switch dataType {
	case rpc.MarketDataLive, opportunityHistoricalBarDataType:
	case "":
		reasons = append(reasons, "data_type_missing")
	default:
		reasons = append(reasons, "data_type_not_live")
	}
	quoteQuality := strings.ToLower(strings.TrimSpace(f.QuoteQuality))
	switch quoteQuality {
	case "firm", opportunityHistoricalBarQuoteQuality:
	case "":
		reasons = append(reasons, "quote_quality_missing")
	case "missing", "prev_close", "stale":
		reasons = append(reasons, "quote_quality_"+quoteQuality)
	default:
		reasons = append(reasons, "quote_quality_not_firm")
	}
	if f.Stale {
		reasons = append(reasons, "quote_stale")
	}
	if f.Indicative {
		reasons = append(reasons, "quote_indicative")
	}
	if strings.TrimSpace(f.QuoteError) != "" {
		reasons = append(reasons, "quote_error")
	}
	if strings.TrimSpace(f.TechnicalError) != "" {
		reasons = append(reasons, "technical_error")
	}
	if f.Price == nil || *f.Price <= 0 {
		reasons = append(reasons, "price_missing")
	}
	if f.SessionContext == nil {
		reasons = append(reasons, "session_context_missing")
	} else {
		state := strings.ToLower(strings.TrimSpace(f.SessionContext.State))
		if !f.SessionContext.IsOpen || (state != "regular" && state != "early_close") {
			if state == "" {
				state = "unknown"
			}
			reasons = append(reasons, "session_"+state)
		}
	}
	if f.AvgDollarVolume20D == nil || *f.AvgDollarVolume20D < opportunityBuilderMinDollarVolume {
		reasons = append(reasons, "liquidity_below_min")
	}
	return reasons
}

func opportunityFeaturePctAbove50(f OpportunityPointInTimeFeatures) *float64 {
	if f.PctAbove50DMA != nil {
		return f.PctAbove50DMA
	}
	if f.Price == nil || f.SMA50 == nil || *f.SMA50 <= 0 {
		return nil
	}
	v := (*f.Price - *f.SMA50) / *f.SMA50
	return &v
}

func opportunityFeaturePctAbove200(f OpportunityPointInTimeFeatures) *float64 {
	if f.PctAbove200DMA != nil {
		return f.PctAbove200DMA
	}
	if f.Price == nil || f.SMA200 == nil || *f.SMA200 <= 0 {
		return nil
	}
	v := (*f.Price - *f.SMA200) / *f.SMA200
	return &v
}

func compactOpportunityReasons(reasons []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			continue
		}
		if _, ok := seen[reason]; ok {
			continue
		}
		seen[reason] = struct{}{}
		out = append(out, reason)
	}
	return out
}

func opportunityReasonClass(reason string) string {
	reason = strings.TrimSpace(reason)
	switch {
	case strings.HasPrefix(reason, "passed_"):
		return "pass"
	case opportunitySignalContextBlocked([]string{reason}):
		return "context_blocker"
	default:
		return "feature_filter"
	}
}

func topOpportunityDiagnosticBuckets(buckets []OpportunityBacktestDiagnosticBucket, limit int) []OpportunityBacktestDiagnosticBucket {
	out := slices.Clone(buckets)
	sort.SliceStable(out, func(i, j int) bool {
		mi := out[i].Metrics.SignalFired
		mj := out[j].Metrics.SignalFired
		if mi != mj {
			return mi > mj
		}
		oi := out[i].Metrics.Observations
		oj := out[j].Metrics.Observations
		if oi != oj {
			return oi > oj
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		return out[:limit]
	}
	return out
}
