package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	backtestEODNote     = "statement series is EOD; the runtime observes intraday — small peak and crossing divergence is expected"
	backtestGenesisNote = "replay starts at runtime genesis; earlier statement history informs the flow review only"
)

// buildReconBacktest builds the read-only, full-window flow review and
// capital-ladder replay from retained statement truth. It changes no matching,
// sign-off, or enforcement state.
func (s *Server) buildReconBacktest() *rpc.ReconBacktestResult {
	res := &rpc.ReconBacktestResult{
		AsOf:       time.Now(),
		FlowCounts: make(map[string]int),
	}
	var health []rpc.SourceHealth
	pol := s.riskPolicies.snapshot().policy
	rc := reconPolicyOf(pol)
	if rc == nil {
		res.Status = rpc.ReconStatusUnapproved
		res.Message = "recon.* policy keys are unapproved; write them in the risk policy before the backtest can classify anything"
		res.InputHealth = append(health, rpc.SourceHealth{Source: "risk_policy", Status: "unapproved"})
		return res
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "reconciliation needs one selected broker account"
		res.InputHealth = append(health, rpc.SourceHealth{Source: "account_scope", Status: "unavailable", Notes: []string{rpc.ReconReportReasonAuthorityUnavailable}})
		return res
	}

	statements, problems, err := s.loadActiveRetainedFlexStatementsContext(context.Background(), nil)
	switch {
	case err != nil:
		s.warnf("reconciliation backtest: retained Flex statement read failed: %v", err)
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "retained statements are temporarily unavailable"
		res.InputHealth = append(health, rpc.SourceHealth{Source: "statements", Status: "unavailable", Notes: []string{rpc.ReconReportReasonStorageFailed}})
		return res
	case len(statements) == 0:
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "no retained Flex statements yet; enable [flex] and wait for the daily fetch, or check fetch.last_error"
		res.InputHealth = append(health, rpc.SourceHealth{Source: "statements", Status: "unavailable"})
		return res
	}
	statements, skipped := retainedStatementsForScope(statements, scope)
	res.FlowCounts[reconSkippedSiblingStatementsCount] = skipped
	if len(statements) == 0 {
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "no retained Flex statements belong to the selected account"
		res.InputHealth = append(health, rpc.SourceHealth{Source: "statements", Status: "unavailable", Notes: []string{
			rpc.ReconReportReasonReportInvalid,
			fmt.Sprintf("%s=%d", reconSkippedSiblingStatementsCount, skipped),
		}})
		return res
	}
	res.Status = rpc.ReconStatusActive
	if len(problems) > 0 {
		for _, problem := range problems {
			s.warnf("reconciliation backtest: retained Flex statement rejected: %s", problem)
		}
		res.Status = rpc.ReconStatusDegraded
		health = append(health, rpc.SourceHealth{Source: "statements", Status: "degraded", Notes: []string{
			rpc.ReconReportReasonResponseInvalid,
			fmt.Sprintf("invalid_retained_statements=%d", len(problems)),
			fmt.Sprintf("%s=%d", reconSkippedSiblingStatementsCount, skipped),
		}})
	} else {
		health = append(health, rpc.SourceHealth{Source: "statements", Status: "ok", Notes: []string{
			fmt.Sprintf("%s=%d", reconSkippedSiblingStatementsCount, skipped),
		}})
	}

	merged := mergeRetainedStatements(statements)
	res.StatementAsOf = merged.statementAsOf
	res.CoverageFrom = merged.coverageFrom
	res.CoverageTo = merged.coverageTo
	res.ClassifiedCounts = merged.classifiedCounts
	res.EquityDays = len(merged.equityByDay)
	res.PolicyFingerprint = &rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: pol.FingerprintKey()}

	ctx, err := s.riskCapital.ReplayContextForScope(scope)
	if err != nil {
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "capital state is unavailable for the selected account"
		res.InputHealth = append(health, rpc.SourceHealth{Source: "capital_state", Status: "unavailable", Notes: []string{rpc.ReconReportReasonAuthorityUnavailable}})
		return res
	}
	res.GenesisAt = ctx.GenesisAt
	matchableFlows, baseline := partitionReconBaselineFlows(merged.flows, ctx)
	baselineByLine := make(map[string]bool, len(baseline))
	for _, row := range baseline {
		baselineByLine[row.LineID] = true
	}
	events, err := s.riskCapital.CapitalFlowEventsContextForScope(context.Background(), nil, scope)
	if err != nil {
		s.warnf("reconciliation backtest: capital event authority unavailable: %v", err)
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "capital events are temporarily unavailable"
		res.InputHealth = append(health, rpc.SourceHealth{Source: "capital_events", Status: "unavailable", Notes: []string{rpc.ReconReportReasonAuthorityUnavailable}})
		return res
	}
	if pol.PolicyVersion >= 3 {
		events, _, _ = splitV3ReconEvents(events, ctx, res.CoverageTo)
	}
	matchedExceptions, matched := matchReconFlows(matchableFlows, events, rc)
	if pol.PolicyVersion >= 3 {
		for i := range matchedExceptions {
			if matchedExceptions[i].Category == rpc.ReconMissingFromLedger {
				matchedExceptions[i].Category = rpc.ReconConfirmed
			}
		}
	}
	exceptions := append(merged.exceptions, matchedExceptions...)
	if err := s.applyReconDismissalsContext(context.Background(), exceptions, nil, scope); err != nil {
		s.warnf("reconciliation backtest: risk governance authority unavailable: %v", err)
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "reconciliation decisions are temporarily unavailable"
		res.InputHealth = append(health, rpc.SourceHealth{Source: "risk_governance", Status: "unavailable", Notes: []string{rpc.ReconReportReasonAuthorityUnavailable}})
		return res
	}
	exceptionByLine := make(map[string]rpc.ReconException, len(exceptions))
	for _, ex := range exceptions {
		exceptionByLine[ex.LineID] = ex
		if ex.Category == rpc.ReconUncategorized {
			res.UncategorizedCount++
		}
	}

	flows := append([]reconFlow(nil), merged.flows...)
	sort.Slice(flows, func(i, j int) bool {
		if flows[i].valueDate.Equal(flows[j].valueDate) {
			return flows[i].id < flows[j].id
		}
		return flows[i].valueDate.Before(flows[j].valueDate)
	})
	for _, flow := range flows {
		amount := flow.amountBase
		row := rpc.ReconBacktestFlow{
			LineID: flow.id, Type: flow.typ, Description: flow.desc,
			ValueDate: flow.valueDate, AmountBase: &amount,
		}
		if baselineByLine[flow.id] {
			row.Status = rpc.ReconBaseline
		} else if matched[flow.id] {
			row.Status = "matched"
		} else if ex, ok := exceptionByLine[flow.id]; ok {
			row.Status = ex.Category
			if ex.Category != rpc.ReconConfirmed {
				row.Dismissed = ex.Dismissed
			}
		}
		row.PreGenesis = baselineByLine[flow.id]
		res.Flows = append(res.Flows, row)
		res.FlowCounts[row.Status]++
		if row.Dismissed {
			res.FlowCounts["dismissed"]++
		}
	}

	missing := replayPrerequisites(ctx, pol)
	if len(missing) > 0 {
		res.Message = "equity replay unavailable: missing " + strings.Join(missing, ", ")
	} else {
		res.Replay = buildCapitalReplay(merged, ctx, *pol.Drawdown.WarnConsumedPct, *pol.Drawdown.BlockConsumedPct, *pol.Capital.DeclaredRiskCapital, s.earliestCapitalWarnAt(scope))
	}
	res.ReportID = reconBacktestReportID(res, pol.FingerprintKey(), reconScopeIdentity(scope))
	res.InputHealth = health
	return res
}

func replayPrerequisites(ctx capitalReplayContext, pol *risk.Constitution) []string {
	var missing []string
	if !ctx.Seeded {
		missing = append(missing, "seeded runtime capital state")
	}
	// Keep the policy checks explicit: these values are operator-owned and
	// the replay must never invent a threshold.
	if pol == nil || pol.Drawdown.WarnConsumedPct == nil {
		missing = append(missing, "drawdown.warn_consumed_pct")
	}
	if pol == nil || pol.Drawdown.BlockConsumedPct == nil {
		missing = append(missing, "drawdown.block_consumed_pct")
	}
	if pol == nil || pol.Capital.DeclaredRiskCapital == nil {
		missing = append(missing, "capital.declared_risk_capital")
	}
	return missing
}

func buildCapitalReplay(merged retainedStatementMerge, ctx capitalReplayContext, warnPct, blockPct, declared float64, warnRuntimeAt time.Time) *rpc.ReconBacktestReplay {
	replay := &rpc.ReconBacktestReplay{
		Notes: []string{backtestEODNote, backtestGenesisNote},
	}
	var days []flexstmt.EquityRow
	for _, row := range merged.equityByDay {
		if ctx.GenesisAt.IsZero() || !utcDateBefore(row.ReportDate, ctx.GenesisAt) {
			days = append(days, row)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].ReportDate.Before(days[j].ReportDate) })
	replay.Days = len(days)
	if len(days) > 0 {
		replay.FirstDay = days[0].ReportDate
		replay.LastDay = days[len(days)-1].ReportDate
	}

	var eraFlows []reconFlow
	for _, flow := range merged.flows {
		if ctx.GenesisAt.IsZero() || !utcDateBefore(flow.valueDate, ctx.GenesisAt) {
			eraFlows = append(eraFlows, flow)
		}
	}
	sort.Slice(eraFlows, func(i, j int) bool {
		if eraFlows[i].valueDate.Equal(eraFlows[j].valueDate) {
			return eraFlows[i].id < eraFlows[j].id
		}
		return eraFlows[i].valueDate.Before(eraFlows[j].valueDate)
	})

	flowIndex := 0
	var cumulative float64
	var havePeak, sawWarn, sawBlock bool
	for _, row := range days {
		day := row.ReportDate.UTC().Format("2006-01-02")
		for flowIndex < len(eraFlows) && eraFlows[flowIndex].valueDate.UTC().Format("2006-01-02") <= day {
			cumulative += eraFlows[flowIndex].amountBase
			flowIndex++
		}
		adjusted := row.TotalBase - cumulative
		if !havePeak || adjusted > replay.ReplayedPeakBase {
			havePeak = true
			replay.ReplayedPeakBase = adjusted
			replay.ReplayedPeakAt = row.ReportDate
		}
		consumed := (replay.ReplayedPeakBase - adjusted) / declared * 100
		if !sawWarn && consumed >= warnPct {
			replay.Crossings = append(replay.Crossings, rpc.ReconBacktestCrossing{
				Tier: "warn", ReplayedAt: row.ReportDate, ReplayedConsumedPct: consumed, RuntimeAt: warnRuntimeAt,
			})
			sawWarn = true
		}
		if !sawBlock && consumed >= blockPct {
			replay.Crossings = append(replay.Crossings, rpc.ReconBacktestCrossing{
				Tier: "block", ReplayedAt: row.ReportDate, ReplayedConsumedPct: consumed, RuntimeAt: ctx.LatchedAt,
			})
			sawBlock = true
		}
		if sample, ok := ctx.DailyEquity[day]; ok && row.TotalBase != 0 {
			div := (sample - row.TotalBase) / math.Abs(row.TotalBase) * 100
			replay.SameDayComparisons++
			if replay.MaxSameDayDivergencePct == nil || math.Abs(div) > math.Abs(*replay.MaxSameDayDivergencePct) {
				replay.MaxSameDayDivergencePct = &div
			}
		}
	}

	runtimePeak := ctx.AdjustedPeakBase
	replay.RuntimePeakBase = &runtimePeak
	replay.RuntimePeakAt = ctx.PeakAsOf
	if ctx.AdjustedPeakBase != 0 {
		div := (replay.ReplayedPeakBase - ctx.AdjustedPeakBase) / ctx.AdjustedPeakBase * 100
		replay.PeakDivergencePct = &div
	}
	return replay
}

func earliestCapitalWarnAt() time.Time {
	path, err := defaultTradingStatePath(riskPolicyJournalFile)
	if err != nil {
		return time.Time{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	var earliest time.Time
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			At   time.Time `json:"at"`
			Kind string    `json:"kind"`
			To   string    `json:"to"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Kind != "capital_tier" || entry.To != "warn" || entry.At.IsZero() {
			continue
		}
		if earliest.IsZero() || entry.At.Before(earliest) {
			earliest = entry.At
		}
	}
	return earliest
}

func (s *Server) earliestCapitalWarnAt(scope brokerStateScope) time.Time {
	if s == nil || s.riskCapital == nil || s.riskCapital.core == nil {
		// Legacy unit/import helper only; Start binds core before runtime.
		return earliestCapitalWarnAt()
	}
	payloads, err := s.riskCapital.GovernanceEventPayloadsForScope(context.Background(), scope)
	if err != nil {
		return time.Time{}
	}
	var earliest time.Time
	for _, payload := range payloads {
		var entry struct {
			At   time.Time `json:"at"`
			Kind string    `json:"kind"`
			To   string    `json:"to"`
		}
		if json.Unmarshal(payload, &entry) != nil || entry.Kind != "capital_tier" || entry.To != "warn" || entry.At.IsZero() {
			continue
		}
		if earliest.IsZero() || entry.At.Before(earliest) {
			earliest = entry.At
		}
	}
	return earliest
}

func reconBacktestReportID(res *rpc.ReconBacktestResult, fingerprintKey, scopeIdentity string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|", scopeIdentity, res.CoverageFrom.UTC().Format(time.RFC3339), res.CoverageTo.UTC().Format(time.RFC3339),
		res.StatementAsOf.UTC().Format(time.RFC3339), fingerprintKey)
	for _, flow := range res.Flows {
		fmt.Fprintf(h, "%s|%s|%t\n", flow.LineID, flow.Status, flow.Dismissed)
	}
	if res.Replay != nil {
		for _, crossing := range res.Replay.Crossings {
			fmt.Fprintf(h, "%s|%s\n", crossing.Tier, crossing.ReplayedAt.UTC().Format(time.RFC3339))
		}
	}
	return "backtest-" + hex.EncodeToString(h.Sum(nil))[:16]
}
