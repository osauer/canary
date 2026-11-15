package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/scorecard"
)

func runScorecard(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "scorecard")
	sleeve := fs.String("sleeve", "", "one sleeve, for example owner (default: every sleeve)")
	horizon := fs.Int("horizon", scorecard.PrimaryHorizon, "sessions after entry to read: 5, 10 or 21")
	limit := fs.Int("limit", 10, "decisions to list, newest first (1-2000)")
	jsonOut := fs.Bool("json", false, "emit the full typed scorecard")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	if *horizon != 5 && *horizon != scorecard.PrimaryHorizon && *horizon != scorecard.MaxHorizon {
		return fail(env, "scorecard: --horizon must be 5, 10 or 21")
	}
	res, err := scorecard.Fetch(ctx, env.Conn, rpc.ScorecardReadParams{Sleeve: *sleeve, Limit: *limit})
	if err != nil {
		return fail(env, "scorecard: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderScorecard(env, res, *horizon)
	return 0
}

// renderScorecard says in plain words what the daemon's evidence means and
// how large the sample is. A sleeve is never called skilled before the
// established gate; accruing reads as "not enough evidence yet".
func renderScorecard(env *Env, res rpc.ScorecardResult, horizon int) {
	out := env.Stdout
	state := res.State
	if state == "" {
		state = "unavailable"
	}
	tone := ""
	if state != rpc.ScorecardStateCurrent {
		tone = "yellow"
	}
	riskDisplayLine(env, env.bold("Decision scorecard")+"  ", strings.ToUpper(state), func(s string) string { return riskTone(env, tone, env.bold(s)) })
	scored := "unavailable"
	if !res.AsOf.IsZero() {
		scored = res.AsOf.Local().Format("2 Jan 15:04 MST")
	}
	evidence := ""
	if !res.EvidenceTo.IsZero() {
		evidence = "statements to " + res.EvidenceTo.Format(time.DateOnly)
	}
	riskReadLine(env, "Scored", scored, evidence, strings.ReplaceAll(res.Reason, "_", " "))
	if res.State != rpc.ScorecardStateCurrent && len(res.Sleeves) == 0 {
		riskReadLine(env, "", scorecardStateWords(res))
		riskReadLine(env, "", "Read-only review of past fills; it places no orders.")
		return
	}
	riskReadLine(env, "Horizon", fmt.Sprintf("%d sessions after entry", horizon), fmt.Sprintf("primary %d", res.PrimaryHorizon), "others "+scorecardOtherHorizons(res, horizon))

	shown := 0
	for _, sleeve := range res.Sleeves {
		// An index stream appears once the sleeve has an index decision.
		if sleeve.Horizon != horizon || (scorecardIndexStream(sleeve.Stream) && sleeve.Decisions == 0) {
			continue
		}
		shown++
		fmt.Fprintln(out)
		renderScorecardSleeve(env, sleeve, res.BaseCurrency)
	}
	if shown == 0 {
		fmt.Fprintln(out)
		riskReadLine(env, "Sleeves", "none scored yet")
	}
	renderScorecardDecisions(env, res, horizon)
	fmt.Fprintln(out)
	riskReadLine(env, "Coverage", fmt.Sprintf("%d decisions in the ledger", res.Coverage.Ledger), fmt.Sprintf("%d stock", res.Coverage.Stocks), fmt.Sprintf("%d option", res.Coverage.Options),
		fmt.Sprintf("%d entries", res.Coverage.Entries), fmt.Sprintf("%d exits", res.Coverage.Exits),
		fmt.Sprintf("%d index entries", res.Coverage.IndexEntries), fmt.Sprintf("%d index exits", res.Coverage.IndexExits), scorecardCounts("rule-originated, in no owner stream", res.Coverage.RuleOriginated),
		scorecardCounts("skipped", res.Coverage.Skipped), scorecardCounts("unknown", res.Coverage.UnknownByReason),
		fmt.Sprintf("%d with a fill quote", res.Coverage.SlippageQuoted), scorecardCounts("without", res.Coverage.SlippageByReason))
	history := fmt.Sprintf("%d names", res.History.Names)
	if res.History.Missing > 0 || res.History.Pending > 0 {
		history = fmt.Sprintf("%d names, %d without history, %d still to fetch", res.History.Names, res.History.Missing, res.History.Pending)
	}
	sectors := ""
	if n := len(res.History.SectorBars); n > 0 {
		sectors = fmt.Sprintf("%d sector %s", n, pluralWord(n, "fund", "funds"))
	}
	riskReadLine(env, "History", history, fmt.Sprintf("%s %d sessions", res.History.MarketSymbol, res.History.MarketBars), sectors)
	renderScorecardHistoryFailures(env, res.History.Failures)
	fmt.Fprintln(out)
	riskReadLine(env, "Reading", "z is the underlying's move from the close of the fill's session, net of its exposure to the market and, where the name has one, to its sector fund, in units of its own daily noise; positive means the move went the decision's way. On an index (SPY, SPX, QQQ and the like, or a name that moves with SPY at R² 0.90 or more) z is the index's whole move in units of its whole daily noise. Entries (opens and adds) carry the gates; exits (trims and exits) and the index streams are reported, never gated. One name in one entry week is one unit of evidence; units whose horizons overlap count less, so the effective sample is smaller than the unit count. The directional score is the mean z per unit; it is not a correlation. The e-value is the weight of evidence for information in the decisions (20 is the bar); the posterior gives the chance the true score exceeds 0, 0.05 and 0.10; the futility e-value weighs no information against a score of 0.05, the smallest worth a risk allocation.")
	riskReadLine(env, "", "A sleeve is established only when every gate on its entries passes, the sign-shuffle placebo included; until then nothing here says whether the decisions carry information. Slippage is the half-spread paid against the quote retained at the fill; fills before 18 Oct 2026 have none. Fills made by Canary's rules or the broker's lifecycle are the rule sleeve and count for no one. Read-only review of past fills; it places no orders.")
}

// renderScorecardHistoryFailures names each name whose last history read
// failed, with the broker's answer, when, and when the daemon asks again.
func renderScorecardHistoryFailures(env *Env, failures []rpc.ScorecardHistoryFailure) {
	if len(failures) == 0 {
		return
	}
	riskReadLine(env, "History failures", fmt.Sprintf("%d %s; the closes of the last successful read stay behind the scores", len(failures), pluralWord(len(failures), "name", "names")))
	for _, f := range failures {
		kept := "no history"
		if f.Bars > 0 {
			kept = fmt.Sprintf("%d sessions kept", f.Bars)
		}
		riskReadLine(env, "  "+f.Symbol, f.Failure, "failed "+f.FailedAt.Local().Format("2 Jan 15:04"), "next try "+f.RetryAt.Local().Format("2 Jan 15:04"), kept)
	}
}

func scorecardStateWords(res rpc.ScorecardResult) string {
	switch res.Reason {
	case "flex_configuration_required":
		return "Flex statement setup is required before decisions can be scored."
	case "snapshot_pending", "statement_account_pending":
		return "The first pass over the broker statements has not completed yet."
	case "history_backfill_pending":
		return "Price history is still being fetched within the broker's budget; scores appear as names arrive."
	case "account_scope_unavailable", "account_scope_changed":
		return "The broker account is not confirmed, so no scorecard is served."
	default:
		return "The scorecard is not available: " + strings.ReplaceAll(res.Reason, "_", " ") + "."
	}
}

func scorecardOtherHorizons(res rpc.ScorecardResult, horizon int) string {
	var others []string
	for _, h := range res.Horizons {
		if h != horizon {
			others = append(others, fmt.Sprintf("%d", h))
		}
	}
	if len(others) == 0 {
		return "none"
	}
	return strings.Join(others, ", ")
}

// scorecardReportedStreams names each ungated stream and says in one line
// what it asks.
var scorecardReportedStreams = map[string]struct{ title, meaning string }{
	rpc.ScorecardStreamExits:        {"exits", "Trims and exits: did the name move against the position afterwards? Exit timing is a different skill from entry selection and never gates the sleeve."},
	rpc.ScorecardStreamIndexEntries: {"index entries", "Market timing and hedges: did the index move the way the decision positioned for? A different skill from single-name selection; never gates the sleeve."},
	rpc.ScorecardStreamIndexExits:   {"index exits", "Index trims and exits: did the index move against the position afterwards? Never gates the sleeve."},
}

func renderScorecardSleeve(env *Env, s rpc.ScorecardSleeve, base string) {
	if !s.Gated {
		stream := scorecardReportedStreams[s.Stream]
		riskDisplayLine(env, env.bold("Sleeve "+s.Sleeve+" · "+firstNonEmptyCLI(stream.title, strings.ReplaceAll(s.Stream, "_", " ")))+"  ", "REPORTED, NOT GATED", func(t string) string { return env.dim(env.bold(t)) })
		if stream.meaning != "" {
			riskDisplayLine(env, "  ", stream.meaning, nil)
		}
	} else {
		gate := strings.ToUpper(s.Gate)
		tone := ""
		switch s.Gate {
		case rpc.ScorecardGateEstablished:
			tone = "green"
		case rpc.ScorecardGateFutile:
			tone = "red"
		}
		riskDisplayLine(env, env.bold("Sleeve "+s.Sleeve+" · entries")+"  ", gate, func(t string) string { return riskTone(env, tone, env.bold(t)) })
		riskDisplayLine(env, "  ", scorecardGateWords(s), nil)
	}
	adjusted := ""
	if s.Scored > 0 && !scorecardIndexStream(s.Stream) {
		adjusted = fmt.Sprintf("%d of %d net of the sector fund too", s.SectorAdjusted, s.Scored)
	}
	riskReadLine(env, "  Sample", fmt.Sprintf("%d decisions", s.Decisions), fmt.Sprintf("%d scored", s.Scored), fmt.Sprintf("%d name-weeks (each name counts once a week)", s.Units), fmt.Sprintf("%.1f effective after overlap", s.EffectiveUnits), fmt.Sprintf("%d %s", s.Weeks, pluralWord(s.Weeks, "week", "weeks")), adjusted)
	if s.Units == 0 {
		renderScorecardSlippage(env, s.Slippage, base)
		return
	}
	riskReadLine(env, "  Directional score", scorecardSigned(s.DirectionalScore, 2)+" per name-week", fmt.Sprintf("running sum %+.1f", s.SumZ), fmt.Sprintf("posterior %s ± %.2f", scorecardSigned(s.PosteriorMean, 3), deref(s.PosteriorSD)))
	eValue := "0 (against)"
	if !s.EValueAgainst {
		eValue = fmt.Sprintf("%.1f of %.0f", s.EValue, scorecard.EstablishedEValue)
	}
	riskReadLine(env, "  Evidence", "e-value "+eValue, "P(score > 0) "+scorecardProb(s.PScoreAbove0), "P(score > 0.05) "+scorecardProb(s.PScoreAbove005), "P(score > 0.10) "+scorecardProb(s.PScoreAbove010), fmt.Sprintf("futility e-value %.1f of %.0f", s.FutilityEValue, scorecard.FutileEValue))
	if s.Gated && s.UnitsToEstablish != nil {
		pace := ""
		if s.WeeksToEstablish != nil {
			pace = fmt.Sprintf(", about %d weeks at this pace", *s.WeeksToEstablish)
		}
		riskReadLine(env, "  Distance", fmt.Sprintf("at this score the e-value gate needs about %d effective name-weeks%s; the sample gate needs %.0f over %d weeks", *s.UnitsToEstablish, pace, scorecard.EstablishedUnits, scorecard.EstablishedWeeks))
	}
	halves := "halves " + scorecardSigned(s.Gates.FirstHalfMeanZ, 2) + " / " + scorecardSigned(s.Gates.SecondHalfMeanZ, 2)
	markets := fmt.Sprintf("up markets %s (%d) / down %s (%d)", scorecardSigned(s.Gates.UpMarketMeanZ, 2), s.Gates.UpUnits, scorecardSigned(s.Gates.DownMarketMeanZ, 2), s.Gates.DownUnits)
	placebo := ""
	if p := s.Placebo; p != nil && p.Replicates > 0 {
		placebo = fmt.Sprintf("placebo: %d of %d sign-shuffles reach the bar (at most %.0f%% allowed), shuffled spread %.2f of the noise the scores assume (at most %.1f)", p.Established, p.Replicates, 100*scorecard.PlaceboMaxRate, p.StandardisedSD, scorecard.PlaceboMaxStandardisedSD)
	}
	riskReadLine(env, "  Consistency", halves, markets, placebo)
	renderScorecardSlippage(env, s.Slippage, base)
	if s.Wrapper != nil || s.RealisedVsImplied != nil || s.LongPutRealisedVsImplied != nil {
		values := []string{}
		if w := s.Wrapper; w != nil {
			values = append(values, fmt.Sprintf("wrapper mean %s (95%% %s to %s) over %d", formatMoneyCcyForPnL(w.MeanBase, base), formatMoneyCcyForPnL(w.CILow, base), formatMoneyCcyForPnL(w.CIHigh, base), w.Count))
		}
		if r := s.RealisedVsImplied; r != nil {
			values = append(values, fmt.Sprintf("realised/implied median %.2f over %d", r.Median, r.Count))
		}
		if r := s.LongPutRealisedVsImplied; r != nil {
			values = append(values, fmt.Sprintf("bought puts realised/implied median %.2f over %d", r.Median, r.Count))
		}
		riskReadLine(env, "  Options", values...)
	}
	if s.HolmAdjustedP != nil {
		riskReadLine(env, "  Across sleeves", fmt.Sprintf("Holm-adjusted p %.3f", *s.HolmAdjustedP))
	}
	if unknown := scorecardCounts("", s.Unknown); unknown != "" {
		riskReadLine(env, "  Unknown", unknown)
	}
}

// renderScorecardSlippage sums the half-spread the stream's decisions paid
// from the quotes retained at their fills, and says why the rest have none.
func renderScorecardSlippage(env *Env, s *rpc.ScorecardSlippageSummary, base string) {
	if s == nil || s.Decisions == 0 {
		return
	}
	values := []string{}
	if s.Quoted > 0 {
		values = append(values, fmt.Sprintf("half-spread paid %s over %d quoted of %d", formatMoneyCcyForPnL(s.HalfSpreadBase, base), s.Quoted, s.Decisions))
		if s.MeanBp != nil {
			values = append(values, fmt.Sprintf("mean %.1f bp of the mid", *s.MeanBp))
		}
	} else {
		values = append(values, fmt.Sprintf("no quoted fill among %d", s.Decisions))
	}
	if unquoted := scorecardCounts("without", s.Unquoted); unquoted != "" {
		values = append(values, unquoted)
	}
	riskReadLine(env, "  Slippage", values...)
}

func scorecardIndexStream(stream string) bool {
	return stream == rpc.ScorecardStreamIndexEntries || stream == rpc.ScorecardStreamIndexExits
}

func scorecardGateWords(s rpc.ScorecardSleeve) string {
	switch s.Gate {
	case rpc.ScorecardGateEstablished:
		return "Opens and adds: every gate passes, the evidence says these entries carry information at this horizon."
	case rpc.ScorecardGateFutile:
		return "Opens and adds: the evidence says these entries carry no information worth a risk allocation at this horizon; no information is at least twenty times as likely as a directional score of 0.05."
	default:
		return "Opens and adds: not enough evidence yet to say either way."
	}
}

func scorecardSigned(v *float64, decimals int) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%+.*f", decimals, *v)
}

func scorecardProb(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", *v)
}

func scorecardCounts(label string, counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", strings.ReplaceAll(key, "_", " "), counts[key]))
	}
	if label == "" {
		return strings.Join(parts, ", ")
	}
	return label + ": " + strings.Join(parts, ", ")
}

func renderScorecardDecisions(env *Env, res rpc.ScorecardResult, horizon int) {
	out := env.Stdout
	fmt.Fprintln(out)
	title := fmt.Sprintf("Latest decisions · %d of %d", len(res.Decisions), res.DecisionsTotal)
	fmt.Fprintln(out, env.bold(title))
	if len(res.Decisions) == 0 {
		riskReadLine(env, "  ", "none in the ledger")
		return
	}
	fmt.Fprintf(out, "  %-10s %-8s %-5s %-4s %8s %6s  %s\n", "DATE", "NAME", "ACT", "SIDE", "QTY", "z", "NOTE")
	for _, d := range res.Decisions {
		var score *rpc.ScorecardHorizonScore
		for i := range d.Scores {
			if d.Scores[i].Sessions == horizon {
				score = &d.Scores[i]
			}
		}
		z, note := "—", ""
		if score != nil {
			if score.Z != nil {
				z = fmt.Sprintf("%+.2f", *score.Z)
			}
			note = scorecardDecisionNote(d, score, res.BaseCurrency)
		}
		name := d.Underlying
		if d.Instrument == scorecard.InstrumentOption && d.Option != nil {
			name = fmt.Sprintf("%s %s", d.Underlying, strings.ToUpper(d.Option.Right[:1]))
		}
		line := fmt.Sprintf("  %-10s %-8s %-5s %-4s %8s %6s", d.ExecutedAt.Format(time.DateOnly), sanitizeRunText(name), d.Action, d.Side, formatQuantity(d.Quantity), z)
		if note != "" {
			line += "  " + env.dim(sanitizeRunText(note))
		}
		fmt.Fprintln(out, line)
	}
}

func scorecardDecisionNote(d rpc.ScorecardDecision, score *rpc.ScorecardHorizonScore, base string) string {
	parts := []string{}
	if d.Sleeve == rpc.ScorecardSleeveRule {
		parts = append(parts, "rule "+strings.ReplaceAll(strings.ReplaceAll(d.Origin, "proposal:", ""), "_", " ")+", counts for no one")
	}
	if scorecardIndexStream(d.Stream) {
		parts = append(parts, "index")
	}
	if score.Z == nil {
		if score.Reason != "" {
			parts = append(parts, strings.ReplaceAll(score.Reason, "_", " "))
		} else {
			parts = append(parts, "unknown")
		}
		return strings.Join(parts, ", ")
	}
	if d.Instrument == scorecard.InstrumentOption {
		if score.WrapperBase != nil {
			parts = append(parts, "wrapper "+formatMoneyCcyForPnL(*score.WrapperBase, base))
		} else if score.WrapperReason != "" {
			parts = append(parts, "wrapper "+strings.ReplaceAll(score.WrapperReason, "_", " "))
		}
		if score.RealisedVsImplied != nil {
			parts = append(parts, fmt.Sprintf("moved %.1fx implied", *score.RealisedVsImplied))
		}
	}
	if sl := d.Slippage; sl != nil && sl.Status == "measured" {
		if sl.Bp != nil {
			parts = append(parts, fmt.Sprintf("paid %.1f bp", *sl.Bp))
		} else if sl.HalfSpreadBase != nil {
			parts = append(parts, "paid "+formatMoneyCcyForPnL(*sl.HalfSpreadBase, base))
		}
	}
	return strings.Join(parts, ", ")
}

func formatQuantity(q float64) string {
	if q == float64(int64(q)) {
		return fmt.Sprintf("%d", int64(q))
	}
	return fmt.Sprintf("%.2f", q)
}
