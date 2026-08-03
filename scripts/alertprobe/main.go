//go:build ignore

// Command alertprobe is a read-only diagnostic for github.com/osauer/canary issue #19 layer 2.
// Dials the running daemon and prints the rulebook alert source's coverage
// verdict beside the exact rulebook conditions alertShadowMapRulebook checks.
// Redacted by construction: no account id, no symbol, no order reference.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

var canonicalHealth = []string{"account", "positions", "earnings", "regime_stage", "tape"}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conn, err := dial.Connect(dial.DefaultSocketPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close()

	var status rpc.AlertStatusResult
	if err := conn.Call(ctx, rpc.MethodAlertStatus, rpc.AlertStatusParams{}, &status); err != nil {
		fmt.Fprintln(os.Stderr, "alerts.status:", err)
		os.Exit(1)
	}
	var rules rpc.RulesResult
	if err := conn.Call(ctx, "rules.snapshot", map[string]any{}, &rules); err != nil {
		fmt.Fprintln(os.Stderr, "rules.snapshot:", err)
		os.Exit(1)
	}

	fmt.Printf("probe_at=%s\n", time.Now().UTC().Format(time.RFC3339))
	for _, src := range status.Sources {
		if src.Source != rpc.AlertSourceRulebook {
			continue
		}
		m := src.Measurements
		fmt.Printf("rulebook status=%s reason=%s covered=%v evals=%d covered_evals=%d coverage_failures=%d\n",
			src.Status, src.Reason, src.Covered, m.Evaluations, m.CoveredEvaluations, m.CoverageFailures)
		if len(src.UncoveredRules) > 0 {
			fmt.Printf("  uncovered_rules=%s\n", strings.Join(src.UncoveredRules, ","))
		}
	}
	var candidates rpc.AlertCandidateSnapshot
	if err := conn.Call(ctx, rpc.MethodAlertCandidates, rpc.AlertCandidatesParams{}, &candidates); err != nil {
		fmt.Fprintln(os.Stderr, "alerts.candidates:", err)
	} else {
		active := 0
		for _, candidate := range candidates.Candidates {
			if candidate.State != rpc.AlertEpisodeRecovered {
				active++
			}
		}
		fmt.Printf("coverage state=%s freshness=%s covered_sources=%d/%d active_candidates=%d\n",
			candidates.Coverage.State, candidates.Coverage.Freshness,
			len(candidates.Coverage.CoveredSources), len(candidates.Coverage.ExpectedSources), active)
	}

	fmt.Printf("rules status=%q enabled=%v as_of=%s\n", rules.Status, rules.Enabled, rules.AsOf.UTC().Format(time.RFC3339))
	if rules.Status != "ok" {
		fmt.Printf("  BLOCKER precondition: covered requires rules.status==\"ok\"\n")
	}

	// Condition 1: canonical input health, present / ok / non-zero AsOf / unexpired.
	seen := map[string]rpc.SourceHealth{}
	for _, h := range rules.InputHealth {
		seen[h.Source] = h
	}
	fmt.Println("input_health:")
	for _, name := range canonicalHealth {
		h, ok := seen[name]
		if !ok {
			fmt.Printf("  %-13s MISSING -> blocks coverage\n", name)
			continue
		}
		note := "ok"
		switch {
		case h.Status != rpc.SourceStatusOK:
			note = "BLOCKS (status != ok)"
		case h.AsOf.IsZero():
			note = "BLOCKS (zero as_of -> source_time_invalid)"
		case h.AsOf.After(rules.AsOf):
			note = "BLOCKS (as_of after result as_of -> source_time_invalid)"
		case h.MaxAgeSeconds > 0 && time.Now().UTC().After(h.AsOf.UTC().Add(time.Duration(h.MaxAgeSeconds)*time.Second)):
			note = "BLOCKS (expired -> source_health_stale)"
		}
		fmt.Printf("  %-13s status=%-11s refresh=%-9s age=%ds max_age=%ds -> %s\n",
			name, h.Status, orDash(h.RefreshState), h.AgeSeconds, h.MaxAgeSeconds, note)
	}

	// Condition 2: all 14 canonical rows, no unknown, every not_evaluated safe.
	fmt.Println("rules:")
	byStatus := map[string]int{}
	for _, r := range rules.Rules {
		byStatus[r.Status]++
		verdict := "ok"
		switch r.Status {
		case risk.RuleStatusUnknown:
			verdict = "BLOCKS (unknown -> source_health_incomplete)"
		case risk.RuleStatusNotEvaluated:
			verdict = safeNotEvaluated(r, rules, seen["account"].Status)
		}
		if verdict == "ok" {
			continue
		}
		fmt.Printf("  %-2d %-21s %-14s reason=%-22s %s\n", r.Number, r.ID, r.Status, orDash(r.Reason), verdict)
	}
	keys := make([]string, 0, len(byStatus))
	for k := range byStatus {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, byStatus[k]))
	}
	fmt.Printf("  rows=%d  %s\n", len(rules.Rules), strings.Join(parts, " "))

	// Earnings resolution: the RTH-side producer input, counted not named.
	resolved, unresolved, stale := 0, 0, 0
	reasons := map[string]int{}
	for _, e := range rules.Earnings {
		switch {
		case e.Stale:
			stale++
		case e.Status == rpc.EarningsStatusDate || e.Status == rpc.EarningsStatusTerminalNonReporting ||
			e.Status == rpc.EarningsStatusNotApplicable:
			resolved++
		default:
			unresolved++
		}
		reasons[e.Status+"/"+orDash(e.Reason)]++
	}
	fmt.Printf("earnings: names=%d resolved=%d unresolved=%d stale=%d\n", len(rules.Earnings), resolved, unresolved, stale)
	rkeys := make([]string, 0, len(reasons))
	for k := range reasons {
		rkeys = append(rkeys, k)
	}
	sort.Strings(rkeys)
	for _, k := range rkeys {
		fmt.Printf("  %-45s n=%d\n", k, reasons[k])
	}
	if raw, err := json.Marshal(struct{}{}); err == nil {
		_ = raw
	}
}

// safeNotEvaluated mirrors alertShadowRulebookSafeNotEvaluated's arms so the
// probe reports which specific arm rejected the row.
func safeNotEvaluated(row risk.RuleRow, res rpc.RulesResult, accountHealth string) string {
	switch row.ID {
	case risk.RuleCatalystCoverage, risk.RuleOverwriteEarnings, risk.RuleEarningsSizeFreeze:
		if len(row.Exempt) == 0 {
			return "BLOCKS (earnings arm: no exempt rows -> candidate_invalid)"
		}
		return fmt.Sprintf("earnings arm: exempt=%d offenders=%d reason=%s (see authority re-derivation)",
			len(row.Exempt), len(row.Offenders), row.Reason)
	case risk.RuleRedOnGreen, risk.RuleWinnerTrim:
		if row.Reason == risk.RuleReasonOffSession {
			return "ok"
		}
		return "BLOCKS (tape arm: reason != off_session -> candidate_invalid)"
	case risk.RuleGreenDayAction:
		if row.Reason == risk.RuleReasonPnLUnavailable && res.Status == "degraded" && accountHealth == rpc.SourceStatusDegraded {
			return "safe, but result.status==degraded already forecloses coverage"
		}
		return fmt.Sprintf("BLOCKS (green-day arm needs result.status==degraded AND account health==degraded; "+
			"saw status=%q account=%q -> candidate_invalid)", res.Status, accountHealth)
	case risk.RuleHedgeIntegrity:
		if row.Reason == risk.RuleReasonNoLongBook {
			return "ok"
		}
		return "BLOCKS (hedge arm: reason != no_long_book -> candidate_invalid)"
	default:
		return "BLOCKS (no safe arm for this rule -> candidate_invalid)"
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
