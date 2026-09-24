package daemon

import (
	"fmt"
	"math"

	"github.com/osauer/canary/v2/internal/rpc"
)

// describeMarketTape uses only observations through the selected session. Its
// labels describe displayed changes, not fitted signals or execution policy.
func describeMarketTape(rows []rpc.MarketTapeSession) *rpc.MarketTapeReading {
	r := &rpc.MarketTapeReading{
		Headline: "Price comparison unavailable",
		Summary:  "A consecutive-session SPX close is needed to describe the day's move.",
		Evidence: []rpc.MarketTapeEvidence{},
		WatchFor: []string{"In the next completed session, compare price direction with the change in 50-day participation.", "Check daily advancers and directional volume separately; a higher index alone does not show how many stocks rose."},
		Limits:   []string{"Descriptive daily closes, not a forecast. Historical first-availability is unknown; no same-day warning is demonstrated."},
	}
	if len(rows) == 0 {
		return r
	}
	s := rows[len(rows)-1]
	add := func(key, label, value, meaning string) {
		r.Evidence = append(r.Evidence, rpc.MarketTapeEvidence{Key: key, Label: label, Value: value, Meaning: meaning})
	}
	if s.SPX != nil && s.SPX.ChangePct != nil {
		price := tapeDirection(*s.SPX.ChangePct, 2)
		r.Headline = map[int]string{-1: "Price fell", 0: "Price was nearly unchanged", 1: "Price rose"}[price]
		r.Summary = "A price move alone does not establish the breadth or durability of the move."
		if s.Breadth != nil && s.Breadth.Change50PP != nil {
			breadth := tapeDirection(*s.Breadth.Change50PP, 2)
			switch {
			case price > 0 && breadth > 0:
				r.Headline, r.Summary = "Price rose; trend participation improved", "The index and the share of measured stocks above their 50-day average rose together. This supports the day's move; it does not establish follow-through."
			case price > 0 && breadth < 0:
				r.Headline, r.Summary = "Price rose; trend participation narrowed", "The index rose while fewer measured stocks stayed above their 50-day average. The rally was not confirmed by this trend measure."
			case price < 0 && breadth < 0:
				r.Headline, r.Summary = "Price fell; trend participation weakened", "The index and 50-day participation fell together. Weakness extended beyond the index price, but this does not predict the next session."
			case price < 0 && breadth > 0:
				r.Headline, r.Summary = "Price fell; trend participation improved", "The index fell while a larger share of measured stocks stood above their 50-day average. The two measures diverged."
			case price == 0 && breadth < 0:
				r.Headline, r.Summary = "Price held; trend participation narrowed", "The index was nearly unchanged at the displayed precision while 50-day participation fell. The flat index concealed deterioration in this measure."
			case price == 0 && breadth > 0:
				r.Headline, r.Summary = "Price held; trend participation improved", "The index was nearly unchanged at the displayed precision while 50-day participation improved."
			default:
				r.Summary = "50-day participation was nearly unchanged at the displayed precision. Compare the daily advancers and volume before attributing the price move to broad participation."
			}
		} else {
			r.Limits = append(r.Limits, "A comparable prior-session breadth measurement is missing; trend participation cannot confirm this price move.")
		}
		add("price", "SPX daily move", fmt.Sprintf("%+.2f%%", *s.SPX.ChangePct), "Close-to-close index change. This does not measure intraday persistence.")
		if price <= 0 {
			r.Rally = recentTapeRally(rows)
			if rally := r.Rally; rally != nil {
				meaning := "Retracement of that single up day's point gain, measured from its close. Values over 100% mean the whole gain was lost; negative values mean the current close is still higher."
				add("giveback", "Recent up day", fmt.Sprintf("%s: %+.2f%%; %.0f%% given back", rally.Session, rally.GainPct, rally.GivebackPct), meaning)
			}
		}
	}
	if s.QQQ != nil && s.QQQ.ChangePct != nil {
		add("qqq", "QQQ daily move", fmt.Sprintf("%+.2f%%", *s.QQQ.ChangePct), "ETF close-to-close change; compare with SPX to see whether the two moved together.")
	}
	if b := s.Breadth; b != nil {
		if b.PctAbove50DMA != nil {
			value := fmt.Sprintf("%.1f%% · %d/%d names", *b.PctAbove50DMA, b.Coverage50, b.MemberCount)
			if b.Change50PP != nil {
				value += fmt.Sprintf(" · %+.2f pp on prior session", *b.Change50PP)
			}
			add("breadth_50", "Above 50-day average", value, "Trend participation is a level, not the percentage of stocks that rose today. Percentage-point changes require equal coverage counts and, when recorded, equal membership.")
		}
		if b.PctAbove200DMA != nil {
			add("breadth_200", "Above 200-day average", fmt.Sprintf("%.1f%% · %d/%d names", *b.PctAbove200DMA, b.Coverage200, b.MemberCount), "Longer-term trend participation; it can remain weak even during a strong up day.")
		}
		if b.NewHighs != nil && b.NewLows != nil {
			add("highs_lows", "New closing highs / lows", fmt.Sprintf("%d / %d · %d/%d names", *b.NewHighs, *b.NewLows, b.CoverageHighsLows, b.MemberCount), "Closes beyond the preceding 252-bar range. More lows than highs describes remaining weakness, not a timing signal.")
		}
		p := b.Participation
		if p != nil && p.PctAbove20DMA != nil {
			add("breadth_20", "Above 20-day average", fmt.Sprintf("%.1f%% · %d/%d names", *p.PctAbove20DMA, p.Coverage20, b.MemberCount), "Shorter-term trend participation. Compare it with the 50- and 200-day measures to distinguish a bounce from broader trend repair.")
		}
		if p != nil && p.CoverageAD > 0 {
			value := fmt.Sprintf("%d up / %d down / %d unchanged · %d/%d names", p.Advancing, p.Declining, p.Unchanged, p.CoverageAD, b.MemberCount)
			if p.AdvancePct != nil {
				value += fmt.Sprintf(" · %.1f%% advancers", *p.AdvancePct)
			}
			add("advance_decline", "Daily participation", value, "Direction versus each stock's previous official session close. The advancer share excludes unchanged stocks; 50% means equal advancing and declining counts.")
		} else {
			add("advance_decline", "Daily participation", "Not collected for this session", "Older trend breadth does not reconstruct daily advancers. Dated close pairs will arrive through scheduled collection.")
		}
		if p != nil && p.CoverageVolume > 0 {
			value := fmt.Sprintf("%d/%d names", p.CoverageVolume, b.MemberCount)
			if p.UpVolumePct != nil {
				value = fmt.Sprintf("%.1f%% up-volume share · %s", *p.UpVolumePct, value)
			} else {
				value = "No directional volume · " + value
			}
			add("constituent_volume", "Directional share volume", value, "Reported daily share volume grouped by whether the stock closed up or down. Unchanged-stock volume is excluded from the share; this is not buyer-initiated flow or dollar turnover.")
		} else {
			add("constituent_volume", "Directional share volume", "Not collected for this session", "Missing constituent volume is not zero selling or zero buying.")
		}
		if p == nil {
			r.Limits = append(r.Limits, "Historical membership was not recorded for this legacy row; equal coverage counts do not prove an identical covered set.")
		} else {
			r.Limits = append(r.Limits, "Membership records the universe used at collection, not a reconstructed historical index. Equal coverage counts do not guarantee the same covered names.")
			if p.CoverageAD < b.MemberCount || p.CoverageVolume < b.MemberCount {
				r.Limits = append(r.Limits, "Daily participation and volume have separate coverage; missing names can change the proportions.")
			}
		}
	}
	if s.QQQ != nil && s.QQQ.RelativeVolume20 != nil {
		add("relative_volume", "QQQ relative volume", fmt.Sprintf("%.2f× prior 20-session mean", *s.QQQ.RelativeVolume20), "Above 1× means more ETF activity than the prior baseline. Volume alone gives neither direction nor a probability of reversal.")
	} else {
		add("relative_volume", "QQQ relative volume", "Unavailable", "Requires volume for this session and all 20 preceding official sessions.")
	}
	return r
}

func tapeDirection(v float64, digits int) int {
	v = math.Round(v * math.Pow10(digits))
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}

func recentTapeRally(rows []rpc.MarketTapeSession) *rpc.MarketTapeRally {
	last := len(rows) - 1
	if last < 1 || rows[last].SPX == nil {
		return nil
	}
	for i := last - 1; i >= max(0, last-5); i-- {
		p := rows[i].SPX
		if p == nil || p.ChangePct == nil {
			return nil
		} // Never bridge a missing session.
		if tapeDirection(*p.ChangePct, 2) <= 0 {
			continue
		}
		base := p.Close / (1 + *p.ChangePct/100)
		gain := p.Close - base
		if gain <= 0 {
			return nil
		}
		giveback := 100 * (p.Close - rows[last].SPX.Close) / gain
		if math.IsNaN(giveback) || math.IsInf(giveback, 0) {
			return nil
		}
		return &rpc.MarketTapeRally{Session: rows[i].Date, GainPct: *p.ChangePct, GivebackPct: giveback}
	}
	return nil
}
