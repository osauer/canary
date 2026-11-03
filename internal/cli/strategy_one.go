package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// runStrategyOne prints strategy one's screen (Desk spec TM-03.01): the
// watch list with the leading set marked, the names that left it, the entry
// plans and the open names' adds and exits. Reads only.
func runStrategyOne(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "strategy-one")
	jsonOut := fs.Bool("json", false, "emit the typed screen")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	res, err := rpc.FetchStrategyOne(ctx, env.Conn)
	if err != nil {
		return fail(env, "strategy-one: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderStrategyOne(env, res)
	return 0
}

func renderStrategyOne(env *Env, r rpc.StrategyOneResult) {
	fmt.Fprintln(env.Stdout, env.bold("Strategy one")+"  strong names on short dips · "+r.Rule+" · spec "+r.Spec)
	if r.Status != "ready" {
		riskDisplayLine(env, "Holds · ", r.Reason, env.yellow)
		return
	}
	riskDisplayLine(env, "", fmt.Sprintf("Session %s · %d constituents read, %d eligible · regime gate %s at %s", r.Session, r.Universe, r.Eligible, r.GateState, mandatePct(r.GateScale)), nil)
	for _, n := range r.Notes {
		riskDisplayLine(env, "  ", n, nil)
	}
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Watch list")+fmt.Sprintf("  the top %d by %d-session return over SPY, in a rising trend; * the leading %d", r.Numbers.WatchSize, r.Numbers.StrengthSessions, r.Numbers.LeadingSize))
	if len(r.Watch) == 0 {
		riskDisplayLine(env, "  ", "none", nil)
	}
	for _, w := range r.Watch {
		mark := " "
		if w.Leading {
			mark = "*"
		}
		rank := "unranked"
		if w.Rank > 0 {
			rank = fmt.Sprintf("#%d", w.Rank)
		}
		parts := []string{fmt.Sprintf("%s %-6s %s", mark, w.Symbol, rank), fmt.Sprintf("%+.1f%% over SPY", w.Strength), fmt.Sprintf("%.2f, %.1f%% under its 10-session high", w.Close, w.DipPct), "on the list since " + w.Since}
		if w.Outside > 0 {
			parts = append(parts, fmt.Sprintf("outside the top %d for %d sessions", r.Numbers.WatchSize, w.Outside))
		}
		riskDisplayLine(env, "  ", strings.Join(parts, " · "), nil)
	}
	if len(r.Departures) > 0 {
		fmt.Fprintln(env.Stdout, "\n"+env.bold("Left the list"))
		for _, d := range r.Departures {
			riskDisplayLine(env, "  ", d.Symbol+" on "+d.On+" · "+d.Why, nil)
		}
	}
	fmt.Fprintln(env.Stdout, "\n"+env.bold("Entry plans")+fmt.Sprintf("  a leader %g%% to %g%% under its %d-session high; bought the next session at the mid, judged after %d sessions", r.Numbers.DipMinPct, r.Numbers.DipMaxPct, r.Numbers.HighSessions, r.Numbers.JudgeSessions))
	if len(r.Entries) == 0 {
		riskDisplayLine(env, "  ", "none today", nil)
	}
	for _, e := range r.Entries {
		parts := []string{fmt.Sprintf("%s at %.2f, %.1f%% under its high", e.Symbol, e.Ref, e.DipPct)}
		if e.ATR > 0 {
			parts = append(parts, fmt.Sprintf("stop %.2f (%g × ATR %.2f)", e.Stop, r.Numbers.ATRMultiple, e.ATR))
		}
		parts = append(parts, fmt.Sprintf("rule size %d shares, buys %d", e.RuleUnits, e.Units), e.Why)
		riskDisplayLine(env, "  ", strings.Join(parts, " · "), nil)
	}
	if len(r.Held) > 0 {
		fmt.Fprintln(env.Stdout, "\n"+env.bold("Open names"))
		for _, h := range r.Held {
			parts := []string{fmt.Sprintf("%s · %d lots", h.Symbol, h.Lots)}
			switch {
			case h.Exit != "":
				parts = append(parts, h.Exit)
			case h.Add:
				parts = append(parts, "add: "+h.Why)
			case h.Why != "":
				parts = append(parts, h.Why)
			}
			riskDisplayLine(env, "  ", strings.Join(parts, " · "), nil)
		}
	}
}
