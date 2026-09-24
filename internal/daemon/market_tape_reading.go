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
		Summary:  "We need this day's and the previous day's S&P 500 closing prices.",
		Evidence: []rpc.MarketTapeEvidence{},
		WatchFor: []string{"Next close: if the S&P 500 rises, do more stocks finish above their own 50-day average?", "Check how many stocks rose and fell that day once those counts are available."},
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
		r.Headline = map[int]string{-1: "S&P 500 fell", 0: "S&P 500 was nearly unchanged", 1: "S&P 500 rose"}[price]
		r.Summary = "Price alone does not show how many stocks share the strength or whether it will last."
		if s.Breadth != nil && s.Breadth.Change50PP != nil {
			breadth := tapeDirection(*s.Breadth.Change50PP, 2)
			switch {
			case price > 0 && breadth > 0:
				r.Headline, r.Summary = "S&P 500 rose; more stocks above average", "The index rose, and a larger share of measured stocks finished above their own average price over 50 trading days. This does not predict tomorrow."
			case price > 0 && breadth < 0:
				r.Headline, r.Summary = "S&P 500 rose; fewer stocks above average", "The index rose, but a smaller share of measured stocks finished above their own average price over 50 trading days. The price rise hides that weakness."
			case price < 0 && breadth < 0:
				r.Headline, r.Summary = "S&P 500 fell; fewer stocks above average", "The index fell, and a smaller share of measured stocks finished above their own average price over 50 trading days. This does not predict tomorrow."
			case price < 0 && breadth > 0:
				r.Headline, r.Summary = "S&P 500 fell; more stocks above average", "The index fell, but a larger share of measured stocks finished above their own average price over 50 trading days. The two measures moved in opposite directions."
			case price == 0 && breadth < 0:
				r.Headline, r.Summary = "S&P 500 held; fewer stocks above average", "The index barely moved, but a smaller share of measured stocks finished above their own average price over 50 trading days."
			case price == 0 && breadth > 0:
				r.Headline, r.Summary = "S&P 500 held; more stocks above average", "The index barely moved, but a larger share of measured stocks finished above their own average price over 50 trading days."
			default:
				r.Summary = "The share of stocks above their own 50-day average barely changed. This does not tell us how many stocks rose today."
			}
		} else {
			r.Summary += " A matching stock measurement from the previous day is missing."
			r.Limits = append(r.Limits, "We cannot compare the share above average with the previous day because comparable data is missing.")
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
			add("breadth_50", "Above 50-day average", value, "Each measured stock counts once. We count those at or above their own average closing price over 50 trading days. This is not the percentage that rose today; day-to-day comparisons require comparable stock coverage.")
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
			add("advance_decline", "Stocks rising / falling", value, "Each stock is compared with its previous closing price. The percentage rising leaves out unchanged stocks; 50% means equal rising and falling counts.")
		} else {
			add("advance_decline", "Stocks rising / falling", "Not collected for this session", "The above-average measure cannot tell us how many stocks rose that day. These counts need separate daily collection.")
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
