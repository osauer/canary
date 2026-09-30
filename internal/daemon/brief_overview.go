package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// composeBriefOverview changes reading order, never the underlying verdict.
// Coverage groups scopes and statuses, not guessed causes parsed from errors.
func composeBriefOverview(res *rpc.BriefResult, topics []briefTopic) *rpc.BriefOverview {
	o := &rpc.BriefOverview{}
	coverage := append([]briefTopic(nil), topics...)
	coverage = append(coverage,
		briefTopic{label: "last session close", state: res.Review.LastSession.BriefRowState},
		briefTopic{label: "Edge", state: res.Review.Edge.BriefRowState})
	// Attention can coexist with missing measurements in these rows.
	if res.Review.Rules.Unknown > 0 {
		for i := range coverage {
			if coverage[i].label == "policy adherence" {
				coverage[i].state.Status = rpc.BriefStatusDegraded
			}
		}
	}
	for i := range coverage {
		if coverage[i].state.Status == "" {
			coverage[i].state.Status = rpc.BriefStatusUnavailable
		}
	}
	for _, status := range []string{rpc.BriefStatusUnavailable, rpc.BriefStatusDegraded} {
		for _, scope := range []string{"Portfolio", "Market", "Held-name events", "Desk", "History"} {
			var labels []string
			for _, topic := range coverage {
				if topic.state.Status == status && briefCoverageScope(topic.label) == scope {
					labels = append(labels, strings.TrimPrefix(topic.label, "held-name "))
				}
			}
			if len(labels) > 0 {
				o.Coverage = append(o.Coverage, rpc.BriefParagraph{Runs: []rpc.BriefRun{{Text: scope + " · " + status + ": " + strings.Join(labels, ", ")}}})
			}
		}
	}
	p := &briefProse{}
	if len(o.Coverage) > 0 {
		p.text("Assessment incomplete.")
	} else {
		p.text("Assessment available.")
	}
	p.sentence()
	briefStressSentence(p, res.Ready.Stress)
	o.Assessment = briefMergeRuns(p.runs)
	for _, topic := range briefFlaggedTopics(topics) {
		p := &briefProse{}
		p.tintedTopic(topic.role, briefUpperFirst(topic.label), topic.slug())
		p.text(" · ")
		switch topic.label {
		case "capital":
			p.text(res.Ready.Capital.Tier + " tier")
			if res.Ready.Capital.Enforcement != "" {
				p.text(" · " + res.Ready.Capital.Enforcement + " enforcement")
			}
			// PeakAsOf dates the peak, not the capital assessment.
			p.text(" · observation time unavailable")
		case "protection proposals":
			counts := fmt.Sprintf("%d ready", res.Ready.Proposals.Actionable)
			if res.Ready.Proposals.Blocked > 0 {
				counts += fmt.Sprintf(" · %d blocked", res.Ready.Proposals.Blocked)
			}
			p.text(counts + " · observation time unavailable")
		default:
			// Details may contain account-derived values or source free text.
			// Keep privacy binding in the app even for an unfamiliar row.
			p.pushRun(rpc.BriefRun{Text: topic.state.Detail, AccountSensitive: true})
		}
		o.Attention = append(o.Attention, rpc.BriefParagraph{Runs: briefMergeRuns(p.runs)})
	}
	ready := res.Ready
	for _, event := range ready.MarketEvents {
		if event.Kind == "earnings" && event.Status == rpc.BriefStatusAttention {
			o.Context = append(o.Context, briefOverviewLine("Held-name earnings", fmt.Sprintf("%d names with event context; event details in full brief", event.Count)))
		}
	}
	if ready.Regime.Status == rpc.BriefStatusOK {
		o.Context = append(o.Context, briefOverviewLine("Market regime", briefRegimeReading(ready.Regime)))
	}
	if ready.Breadth.Status == rpc.BriefStatusOK {
		b := ready.Breadth
		values := []string{}
		if b.PctAbove50DMA != nil {
			values = append(values, briefPercent(*b.PctAbove50DMA, false)+" above 50-day average")
		}
		if b.PctAbove200DMA != nil {
			values = append(values, briefPercent(*b.PctAbove200DMA, false)+" above 200-day average")
		}
		values = append(values, briefObservedAt(b.AsOf))
		o.Context = append(o.Context, briefOverviewLine("Breadth", strings.Join(values, " · ")))
	}
	if ready.Gamma.Status == rpc.BriefStatusOK {
		o.Context = append(o.Context, briefOverviewLine("Dealer gamma", strings.ReplaceAll(ready.Gamma.Regime, "_", " ")+" · "+briefObservedAt(ready.Gamma.AsOf)))
	}
	if ready.Session.Status == rpc.BriefStatusOK {
		market := ready.Session.Market
		if market == "us_equity" {
			market = "US equities"
		}
		o.Context = append(o.Context, briefOverviewLine(market, strings.ReplaceAll(ready.Session.State, "_", " ")))
	}
	return o
}

func briefOverviewLine(label, value string) rpc.BriefParagraph {
	return rpc.BriefParagraph{Runs: []rpc.BriefRun{{Text: label + " · " + value}}}
}

func briefObservedAt(at time.Time) string {
	if at.IsZero() {
		return "observation time unavailable"
	}
	return "observed " + at.Local().Format("2 Jan 15:04 MST")
}

func briefCoverageScope(label string) string {
	if strings.HasPrefix(label, "held-name ") {
		return "Held-name events"
	}
	switch label {
	case "session P/L", "attribution", "premium at risk", "index-put theta", "stress":
		return "Portfolio"
	case "regime", "breadth", "dealer gamma", "session":
		return "Market"
	case "last session close", "Edge":
		return "History"
	default:
		return "Desk"
	}
}
