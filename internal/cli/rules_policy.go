package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// runRulesPolicy shows the Rulebook limits in force (from the daemon) or,
// with set/reset, edits the owner's policy file. The daemon validates and
// adopts an edited file on its next reload; the edit validates it first with
// the same parser, so an invalid value is refused before anything is written.
func runRulesPolicy(ctx context.Context, env *Env, args []string) int {
	if idx := firstPositionalIndex(args); idx >= 0 && (args[idx] == "set" || args[idx] == "reset") {
		return runRulesPolicyEdit(env, args[idx], append(append([]string{}, args[:idx]...), args[idx+1:]...))
	}
	fs := flagSet(env, "rules policy")
	jsonOut := fs.Bool("json", false, "emit the policy in force and its status as JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "rules policy: usage is `canary rules policy [--json]`, `canary rules policy set KEY=VALUE…` or `canary rules policy reset KEY…|--all`")
	}
	var res rpc.RulesResult
	if err := env.Conn.Call(ctx, rpc.MethodRulesSnapshot, rpc.RulesSnapshotParams{}, &res); err != nil {
		return fail(env, "rules policy: %v", err)
	}
	if res.Policy == nil || res.PolicyStatus == nil {
		return fail(env, "rules policy: the daemon did not report its Rulebook policy; restart it after upgrading (canary restart)")
	}
	if *jsonOut {
		return printJSON(env, struct {
			Status           *rpc.RulebookPolicyStatus   `json:"status"`
			Policy           *risk.RulebookPolicy        `json:"policy"`
			TerminalEvidence *rpc.TerminalEvidenceStatus `json:"terminal_evidence,omitempty"`
		}{res.PolicyStatus, res.Policy, res.TerminalEvidence})
	}
	renderRulesPolicy(env, res.PolicyStatus, *res.Policy, res.TerminalEvidence)
	return 0
}

func renderRulesPolicy(env *Env, st *rpc.RulebookPolicyStatus, p risk.RulebookPolicy, terminal *rpc.TerminalEvidenceStatus) {
	out := env.Stdout
	source := "compiled baseline"
	if st.Source == "file" {
		source = "policy file"
	}
	status := st.Status
	if st.Review == rpc.PolicyReviewUnreviewed {
		status = "default, unreviewed"
	}
	fmt.Fprintf(out, "Rulebook policy — %s %s v%d (%s)\n", source, p.ID, p.Version, status)
	if st.Path != "" {
		fmt.Fprintf(out, "  file         %s\n", st.Path)
	}
	if st.Review == rpc.PolicyReviewUnreviewed {
		fmt.Fprintln(out, "  review       Canary's defaults, not yet reviewed: read the file, then delete its \"# Canary defaults, not yet reviewed.\" line")
	}
	if len(st.Missing) > 0 {
		fmt.Fprintf(out, "  not in file  %s\n", strings.Join(st.Missing, ", "))
		fmt.Fprintln(out, "               these follow Canary's defaults until written")
	}
	if st.Message != "" {
		fmt.Fprintf(out, "  note         %s\n", st.Message)
	}
	fmt.Fprintf(out, "  fingerprint  %s\n\n", shortFingerprint(st.Fingerprint.Key))
	pct := func(v float64) string {
		return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", v), "0"), ".0") + "%"
	}
	band := func(t risk.RegimeThresholds, lo, hi func(risk.RegimeThresholds) float64) string {
		return pct(lo(t)) + "/" + pct(hi(t))
	}
	extW := func(t risk.RegimeThresholds) float64 { return t.ExtrinsicWatchPct }
	extA := func(t risk.RegimeThresholds) float64 { return t.ExtrinsicActPct }
	hMin := func(t risk.RegimeThresholds) float64 { return t.HedgeBandMinPct }
	hMax := func(t risk.RegimeThresholds) float64 { return t.HedgeBandMaxPct }
	limits := map[string]string{
		risk.RuleSingleNameExposure: fmt.Sprintf("worst-case loss per issuer: watch at %s, act at %s of NLV, trim back to %s; illiquid (over %s days to exit at %s of 20-day volume) %s/%s; hedges count from %d days out and past earnings; unbounded legs sized at a %s rise",
			pct(p.SingleNameWatchPct), pct(p.SingleNameActPct), pct(p.SingleNameWatchPct), strings.TrimSuffix(pct(p.IlliquidDaysToExit), "%"), pct(p.ExitParticipationPct), pct(p.IlliquidWatchPct), pct(p.IlliquidActPct), p.HedgeMinDays, pct(p.TakeoverGapPct)),
		risk.RuleOptionLinePremium:  fmt.Sprintf("watch at %s, act at %s of NLV per position (higher of price paid and value); protection %s/%s", pct(p.OptionLineWatchPct), pct(p.OptionLineActPct), pct(p.HedgeLineWatchPct), pct(p.HedgeLineActPct)),
		risk.RuleCashSellOnly:       fmt.Sprintf("available funds at least %s of NLV", pct(p.CashReserveMinPct)),
		risk.RuleExtrinsicBudget:    fmt.Sprintf("time value of NLV, watch/act at: calm %s, early warning %s, confirmed %s", band(p.RegimeCalm, extW, extA), band(p.RegimeEarlyWarning, extW, extA), band(p.RegimeConfirmed, extW, extA)),
		risk.RuleExpiryRunway:       fmt.Sprintf("watch at %d days or fewer, act at %d days or fewer to expiry; in the money from delta %.2f", p.RunwayWatchDTE, p.RunwayActDTE, p.RunwayITMDeltaFloor),
		risk.RuleCatalystCoverage:   "earnings inside an option's life (no threshold)",
		risk.RuleOverwriteEarnings:  fmt.Sprintf("short puts through earnings: act at %s per position, %s per name of NLV", pct(p.ShortPutActLinePctNLV), pct(p.ShortPutActNamePctNLV)),
		risk.RuleEarningsSizeFreeze: fmt.Sprintf("%d sessions before earnings, for an issuer at rule 1's watch level", p.EarningsFreezeSessions),
		risk.RuleRedOnGreen:         fmt.Sprintf("holding %s while SPY is up %s", pct(p.RedOnGreenNameDropPct), pct(p.RedOnGreenSPYUpPct)),
		risk.RuleWinnerTrim:         fmt.Sprintf("up %s today on at least %s of NLV", pct(p.WinnerTrimDayUpPct), pct(p.WinnerTrimMinExpoPct)),
		risk.RuleGreenDayAction:     "a green day while an act-level rule is open",
		risk.RuleHedgeIntegrity:     fmt.Sprintf("index protection band of long exposure: calm %s, early warning %s, confirmed %s; act above %g× the band's top", band(p.RegimeCalm, hMin, hMax), band(p.RegimeEarlyWarning, hMin, hMax), band(p.RegimeConfirmed, hMin, hMax), p.OverhedgeMultiple),
		risk.RuleExitDiscipline:     fmt.Sprintf("watch at −%s, act at −%s of premium paid", pct(p.ExitWatchLossPct), pct(p.ExitActLossPct)),
		risk.RuleFXExposure:         fmt.Sprintf("watch at %s of NLV in other currencies", pct(p.FXExposureWatchPct)),
		risk.RuleNetExposure:        fmt.Sprintf("watch at %s, act at %s of NLV, whole book with hedges", pct(p.NetExposureWatchPct), pct(p.NetExposureActPct)),
		risk.RuleDeltaSwing:         fmt.Sprintf("watch at %s of NLV in one issuer's dollar delta; never acts", pct(p.DeltaSwingWatchPct)),
		risk.RuleClusterStress:      fmt.Sprintf("watch when a declared cluster falling %s together loses %s of NLV; never acts", pct(p.ClusterDropPct), pct(p.ClusterWatchPct)),
		risk.RuleLossBudget:         fmt.Sprintf("watch when one issuer's worst-case loss reaches %s of effective risk capital; never acts", pct(p.BudgetWatchPct)),
	}
	for i, id := range risk.RuleIDs() {
		fmt.Fprintf(out, "  %2d %-22s %-5s  %s\n", i+1, id, p.ModeFor(id), limits[id])
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Issuer groups:    %s\n", symbolGroupsText(p.IssuerGroups, "none (every symbol is its own issuer)"))
	fmt.Fprintf(out, "Clusters:         %s\n", symbolGroupsText(p.Clusters, "none (rule 17 is not evaluated)"))
	for _, line := range terminalEvidenceLines(terminal) {
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Change a limit:   canary rules policy set cash_reserve_min_pct=70")
	fmt.Fprintln(out, "Set a rule's mode: canary rules policy set modes.winner_trim=off   (off | track | alert)")
	fmt.Fprintln(out, "Back to baseline: canary rules policy reset KEY…  or  --all")
	fmt.Fprintln(out, "Every key:        canary policy default rulebook")
}

// terminalEvidenceLines renders the terminal-evidence authority rules 6-8
// read and, when the configured startup import was not applied, why and what
// stays in force. A daemon that reports none renders nothing.
func terminalEvidenceLines(te *rpc.TerminalEvidenceStatus) []string {
	if te == nil {
		return nil
	}
	inForce := "none committed"
	if te.AuthorityRevision > 0 {
		inForce = fmt.Sprintf("revision %d, %d contract(s)", te.AuthorityRevision, te.Contracts)
		if !te.ReviewedAt.IsZero() {
			inForce += ", reviewed " + te.ReviewedAt.UTC().Format(time.DateOnly)
		}
	}
	if te.ImportConfigured {
		inForce += "; import " + te.ImportPath
	}
	lines := []string{"Terminal evidence: " + inForce}
	if te.Status == rpc.TerminalEvidenceStatusImportError {
		lines = append(lines, "  import failed:  "+te.ImportError, "  in force:       "+te.Message)
	}
	return lines
}

// symbolGroupsText renders issuer groups or clusters as "NAME: A, B; …".
func symbolGroupsText(groups map[string][]string, none string) string {
	if len(groups) == 0 {
		return none
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+": "+strings.Join(groups[name], ", "))
	}
	return strings.Join(parts, "; ")
}

func runRulesPolicyEdit(env *Env, verb string, args []string) int {
	fs := flagSet(env, "rules policy "+verb)
	file := fs.String("file", "", "policy file (default: [rulebook] policy_file, else ~/.config/ibkr/policies/rulebook-policy.toml)")
	all := fs.Bool("all", false, "reset: rewrite the file from Canary's current template (the old file is kept as a backup)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var assignments, resets []string
	switch {
	case verb == "set" && fs.NArg() > 0 && !*all:
		assignments = fs.Args()
	case verb == "reset" && (fs.NArg() > 0) != *all:
		resets = fs.Args()
	default:
		return fail(env, "rules policy: usage is `canary rules policy set KEY=VALUE…` or `canary rules policy reset KEY…|--all`")
	}
	daemon.SetRulebookEditRelease(env.Version)
	edit, err := daemon.EditRulebookPolicy(*file, assignments, resets, *all)
	if err != nil {
		return fail(env, "rules policy %s: %v; nothing was written", verb, err)
	}
	out := env.Stdout
	if len(edit.Changes) == 0 {
		fmt.Fprintf(out, "No limit changed; %s is now v%d.\n", edit.Path, edit.Version)
	} else {
		fmt.Fprintf(out, "Wrote %s (policy v%d):\n", edit.Path, edit.Version)
		for _, c := range edit.Changes {
			fmt.Fprintf(out, "  %-40s %s → %s\n", c.Key, orBaseline(c.From), orBaseline(c.To))
		}
	}
	if edit.Review == rpc.PolicyReviewUnreviewed {
		fmt.Fprintln(out, "The file still opens with \"# Canary defaults, not yet reviewed.\"; delete that line once you have reviewed the rest.")
	}
	fmt.Fprintln(out, "The daemon applies it within 30 seconds; `canary rules policy` shows what is in force.")
	return 0
}

func orBaseline(v string) string {
	if v == "" {
		return "baseline"
	}
	return v
}
