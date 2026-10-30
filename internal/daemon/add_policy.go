package daemon

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// opportunityAddPolicy is the Add picker's horizons and delta bands, one list
// per side, in the opportunity policy's [add] table. Owner, 2026-10-30 06:56
// CET: "move the bands for both put and call into the policy file … write
// useful default values at startup (the current ones) and use them, if the
// operator does not override." The bands pick which listed contract Add
// offers; they grant no order authority, so they stay outside the exercise
// policy's identity and take effect at the next reload.
type opportunityAddPolicy struct {
	// Calls are the call horizons Add offers (Canary default 14, 30, 60 and 120 days). Each picks the expiry nearest `days` within `dte` and the listed call whose live delta lies in `delta`, nearest `target`.
	Calls []addHorizonPolicy `toml:"calls" json:"calls"`
	// Puts are the protective put horizons Add offers (Canary default 30, 60, 90 and 120 days), on the same terms; put deltas are written without their sign.
	Puts []addHorizonPolicy `toml:"puts" json:"puts"`
}

type addHorizonPolicy struct {
	Days   int       `toml:"days" json:"days"`
	DTE    []int     `toml:"dte" json:"dte"`
	Delta  []float64 `toml:"delta" json:"delta"`
	Target float64   `toml:"target" json:"target"`
}

// addHorizonLimit is the most horizons a side may list: discovery reads them
// all at once (addHorizonSlots) and a refresh carries at most this many.
const addHorizonLimit = 4

// defaultOpportunityAddPolicy holds the bands Add used before they moved to
// policy. Calls: owner, 2026-10-22 20:34 CEST, "max 0.5 for 30 days, 0.4 for
// 60 and 120 days"; 14 days keeps 0.50–0.70; floors 0.15 under each cap;
// target the cap, at the money for 14 days. Puts: the protective put
// recommendation of 2026-10-30, nearer the money for the shorter horizons.
func defaultOpportunityAddPolicy() opportunityAddPolicy {
	h := func(days, minDTE, maxDTE int, lo, hi, target float64) addHorizonPolicy {
		return addHorizonPolicy{Days: days, DTE: []int{minDTE, maxDTE}, Delta: []float64{lo, hi}, Target: target}
	}
	return opportunityAddPolicy{
		Calls: []addHorizonPolicy{h(14, 8, 21, .5, .7, .5), h(30, 22, 45, .35, .5, .5), h(60, 46, 90, .25, .4, .4), h(120, 91, 150, .25, .4, .4)},
		Puts:  []addHorizonPolicy{h(30, 22, 45, .3, .4, .35), h(60, 46, 75, .25, .35, .3), h(90, 76, 105, .2, .3, .25), h(120, 106, 150, .2, .3, .25)},
	}
}

func validateOpportunityAddPolicy(p opportunityAddPolicy) error {
	for _, side := range []struct {
		name     string
		horizons []addHorizonPolicy
	}{{"calls", p.Calls}, {"puts", p.Puts}} {
		if len(side.horizons) == 0 || len(side.horizons) > addHorizonLimit {
			return fmt.Errorf("add.%s must list one to %d horizons", side.name, addHorizonLimit)
		}
		lastMax := 0
		for i, h := range side.horizons {
			at := fmt.Sprintf("add.%s[%d]", side.name, i)
			if len(h.DTE) != 2 || len(h.Delta) != 2 {
				return fmt.Errorf("%s needs dte = [min, max] and delta = [min, max]", at)
			}
			// Add never offers an expiry inside a week (addCallMarket).
			if h.DTE[0] < 8 || h.DTE[0] > h.DTE[1] || h.DTE[1] > 400 || h.DTE[0] <= lastMax {
				return fmt.Errorf("%s dte must lie within 8 to 400 days, ascending, after the previous horizon's", at)
			}
			if h.Days < h.DTE[0] || h.Days > h.DTE[1] {
				return fmt.Errorf("%s days must lie within its dte", at)
			}
			lo, hi := h.Delta[0], h.Delta[1]
			if !(lo > 0) || !(hi < 1) || lo > hi || h.Target < lo || h.Target > hi || math.IsNaN(h.Target) {
				return fmt.Errorf("%s needs 0 < delta min <= target <= delta max < 1", at)
			}
			lastMax = h.DTE[1]
		}
	}
	return nil
}

// addHorizonsText writes one side's list as the policy file carries it.
func addHorizonsText(key string, horizons []addHorizonPolicy) []string {
	lines := []string{key + " = ["}
	for _, h := range horizons {
		lines = append(lines, fmt.Sprintf("  { days = %d, dte = [%d, %d], delta = [%s, %s], target = %s },",
			h.Days, h.DTE[0], h.DTE[1], addDeltaText(h.Delta[0]), addDeltaText(h.Delta[1]), addDeltaText(h.Target)))
	}
	return append(lines, "]")
}

func addDeltaText(v float64) string { return fmt.Sprintf("%.2f", v) }

// addPolicyTableLines is the [add] table a policy file without one receives.
func addPolicyTableLines(release string) []string {
	p := defaultOpportunityAddPolicy()
	lines := []string{
		"[add]",
		"# The contracts Add offers, written by Canary " + release + " with its defaults.",
		"# For each horizon, Add takes the listed expiry nearest `days` within `dte`",
		"# and the contract whose live delta lies within `delta`, nearest `target`.",
		"# Put deltas are written without their sign. These pick suggestions only;",
		"# every order still passes the gates. Edits apply at the next reload.",
	}
	lines = append(lines, addHorizonsText("calls", p.Calls)...)
	return append(lines, addHorizonsText("puts", p.Puts)...)
}

// addSide is one side of the picker as the policy in force sets it.
type addSide struct {
	right    string // C or P
	horizons []addHorizonPolicy
}

func (s *Server) addSide(right string) addSide {
	p := defaultOpportunityAddPolicy()
	if s != nil && s.opportunityPolicies != nil {
		if active, _ := s.opportunityPolicies.Active(); validateOpportunityAddPolicy(active.Add) == nil {
			p = active.Add
		}
	}
	if right == "P" {
		return addSide{right: "P", horizons: p.Puts}
	}
	return addSide{right: "C", horizons: p.Calls}
}

func defaultAddSide(right string) addSide { return (*Server)(nil).addSide(right) }

func (a addSide) noun() string {
	if a.right == "P" {
		return "put"
	}
	return "call"
}

// Noun is the capitalised noun the picker's messages start with.
func (a addSide) Noun() string { return strings.ToUpper(a.noun()[:1]) + a.noun()[1:] }

func (a addSide) horizonDays() []int {
	out := make([]int, len(a.horizons))
	for i, h := range a.horizons {
		out[i] = h.Days
	}
	return out
}

// bandForDTE is the band of the horizon whose dte holds the days; below the
// first horizon the first, past or between horizons the nearest earlier one.
func (a addSide) bandForDTE(dte int) addDeltaBand {
	if len(a.horizons) == 0 {
		return addDeltaBand{}
	}
	pick := a.horizons[0]
	for _, h := range a.horizons {
		if dte >= h.DTE[0] {
			pick = h
		}
	}
	return addDeltaBand{pick.Delta[0], pick.Delta[1], pick.Target}
}

// bandFor reads a contract's band from its expiry. A contract without a
// readable expiry keeps the first horizon's band.
func (a addSide) bandFor(c *rpc.ContractParams) addDeltaBand {
	if c == nil {
		return a.bandForDTE(0)
	}
	_, dte := addHorizonExpiry([]rpc.SetupOptionExpiry{{Date: c.Expiry}}, 0, 0, 10000, time.Now())
	return a.bandForDTE(dte)
}

// size is the delta the bands judge: a put's without its sign.
func (a addSide) size(delta float64) float64 {
	if a.right == "P" {
		return math.Abs(delta)
	}
	return delta
}

func (a addSide) holds(c *rpc.ContractParams, delta float64) bool {
	if a.right == "P" && delta > 0 {
		return false
	}
	return a.bandFor(c).holds(a.size(delta))
}
