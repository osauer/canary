package cli

import (
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// riskTone only styles a served classification; it never classifies a value.
func riskTone(env *Env, tone, text string) string {
	switch tone {
	case "red", "act", "urgent", rpc.RegimeToneStress, rpc.RegimeToneRiskOff:
		return env.red(text)
	case "yellow", "watch", rpc.RegimeToneDataQuality:
		return env.yellow(text)
	case "green", rpc.RegimeToneNormal:
		return env.green(text)
	default:
		return env.dim(text)
	}
}

// riskDisplayLine sanitizes external text before styling, and wraps beneath
// the value column. Prefixes contain only locally constructed labels/styles.
func riskDisplayLine(env *Env, prefix, text string, style func(string) string) {
	indent := strings.Repeat(" ", visibleLen(prefix))
	for i, line := range wrapVisibleText(sanitizeRunText(text), max(1, briefProseWidth(env.Stdout)-visibleLen(prefix))) {
		if style != nil {
			line = style(line)
		}
		if i == 0 {
			fmt.Fprintln(env.Stdout, prefix+line)
		} else {
			fmt.Fprintln(env.Stdout, indent+line)
		}
	}
}

type regimeDisplayValue struct {
	reading string
	reason  string
}

// regimeDisplayValues selects the useful scalars, with units, from the same
// typed snapshot. Reasons and all color bands remain producer-authored.
func regimeDisplayValues(r rpc.RegimeSnapshotResult) map[string]regimeDisplayValue {
	value := func(p *float64, format string) string {
		if p == nil {
			return "unavailable"
		}
		return fmt.Sprintf(format, *p)
	}
	breadth := "unavailable above 50-day average"
	if r.Breadth.Envelope.Coverage50 > 0 && (r.Breadth.Status == rpc.RegimeStatusOK || r.Breadth.Status == rpc.RegimeStatusStale) {
		breadth = fmt.Sprintf("%.1f%% above 50-day average", r.Breadth.PctAbove50DMA)
	}
	gamma := "unavailable"
	if c := r.GammaZero.Envelope.Result; c != nil && c.Summary != nil {
		var parts []string
		for _, index := range []string{"SPX", "SPY"} {
			if row, ok := c.Summary.PerIndex[index]; ok {
				reading := index + " zero " + value(row.ZeroGamma, "%.2f")
				if row.ZeroGamma != nil && *row.ZeroGamma != 0 {
					reading = index + " zero " + formatMoneyBare(*row.ZeroGamma)
				}
				if row.ZeroGamma == nil {
					reading = index + " " + nonEmpty(strings.ReplaceAll(row.Regime, "_", " "), "unavailable")
				}
				parts = append(parts, reading)
			}
		}
		gamma = nonEmpty(strings.Join(parts, " · "), c.Summary.PrimaryStatement)
	}
	return map[string]regimeDisplayValue{
		"VIX/VIX3M":      {value(r.VIXTermStructure.Ratio, "%.2f") + "  30-day / 3-month volatility", r.VIXTermStructure.BandReason},
		"VVIX":           {value(r.VolOfVol.Last, "%.1f") + "  volatility of VIX · " + value(r.VolOfVol.Change5D, "%+.1f%% / 5 sessions"), r.VolOfVol.BandReason},
		"HYG/SPY":        {"HYG " + value(r.HYGSPYDivergence.HYGPrice, "%.2f") + " · 50-day avg " + value(r.HYGSPYDivergence.HYG50DMA, "%.2f"), r.HYGSPYDivergence.BandReason},
		"Credit spreads": {"HY " + value(r.CreditSpreads.HYOAS, "%.2f%%") + " · " + value(r.CreditSpreads.HY20DChange, "%+.2f pp / 20d"), r.CreditSpreads.BandReason},
		"Funding":        {value(r.FundingStress.SpreadBps, "%.0f bp") + "  CP minus T-bills · " + value(r.FundingStress.Change5Bps, "%+.0f bp / 5 obs"), r.FundingStress.BandReason},
		"USD/JPY":        {value(r.USDJPY.Last, "%.2f") + " · " + value(r.USDJPY.WeeklyChange, "%+.2f%% / week"), r.USDJPY.BandReason},
		"Gamma":          {gamma, r.GammaZero.BandReason},
		"Breadth":        {breadth, r.Breadth.BandReason},
	}
}

func renderRegimeIndicator(env *Env, row rpc.RegimeMonitorIndicator, display regimeDisplayValue, current, explain bool) {
	band := row.Band
	reading := nonEmpty(display.reading, nonEmpty(row.Reading, "unavailable"))
	usable := current && row.Status == rpc.RegimeStatusOK
	label := "—"
	switch band {
	case "green", "red":
		label = strings.ToUpper(band)
	case "yellow":
		label = "AMBER"
	}
	if !usable {
		band, label = "", "—"
		reading = "recorded: " + reading
	} else if label == "—" {
		band, label = "", "—"
	}
	prefix := "  " + riskTone(env, band, fmt.Sprintf("%-5s", label)) + "  " + fmt.Sprintf("%-14s", sanitizeRunText(row.Name)) + "  "
	riskDisplayLine(env, prefix, reading, nil)
	notes := []string{}
	if usable && display.reason != "" {
		notes = append(notes, display.reason)
	}
	if !usable {
		notes = append(notes, nonEmpty(row.Status, "unavailable"), "not a current rating")
	}
	if row.FreshnessClass != "" && row.FreshnessClass != rpc.RegimeFreshnessFresh {
		notes = append(notes, strings.ReplaceAll(row.FreshnessClass, "_", " "))
	}
	if row.AsOf != nil {
		if row.AsOf.Date != "" {
			notes = append(notes, row.AsOf.Date)
		} else if row.AsOf.Label != "" && row.AsOf.Label != "live" {
			notes = append(notes, row.AsOf.Label)
		}
	}
	if len(notes) > 0 {
		riskDisplayLine(env, "         ", strings.Join(notes, " · "), env.dim)
	}
	if explain {
		riskReadLine(env, "    Full reading", row.Reading)
	}
}
