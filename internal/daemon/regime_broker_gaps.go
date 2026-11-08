package daemon

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
)

// regimeGaps collects, during one fan-out, the regime inputs the broker
// refused outright, with its refusal (brokerRefusal).
type regimeGaps struct {
	mu      sync.Mutex
	refused map[string]error
}

// add records sym's refusal; a nil refusal records nothing.
func (g *regimeGaps) add(symbol string, refusal error) {
	if g == nil || refusal == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refused == nil {
		g.refused = map[string]error{}
	}
	sym := strings.TrimSpace(symbol)
	if _, seen := g.refused[sym]; !seen {
		g.refused[sym] = refusal
	}
}

// list is the refused inputs, sorted.
func (g *regimeGaps) list() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Sorted(maps.Keys(g.refused))
}

func (g *regimeGaps) refusal(sym string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refused[sym]
}

// regimeGapRows says, for each input a broker may refuse, which row it feeds,
// what kind of data it is and the row's name for a reader.
//
// DRAFT wording, 2026-11-08: the words regimeGapWarnings builds reach every
// regime reader and are the owner's to approve before this ships.
var regimeGapRows = map[string]struct{ scope, kind, row string }{
	"VIX":            {"vix_term_structure", "index quotes", "VIX term structure"},
	"VIX3M":          {"vix_term_structure", "index quotes", "VIX term structure"},
	"USD.JPY":        {"usd_jpy", "currency quotes", "USD/JPY"},
	"HYG":            {"hyg_spy_divergence", "quotes", "HYG/SPY divergence"},
	"SPY":            {"hyg_spy_divergence", "quotes", "HYG/SPY divergence"},
	regimeGammaInput: {"gamma_zero", "option chains", "gamma"},
}

// regimeGammaInput is the gamma row's broker input: the SPX option chain.
const regimeGammaInput = "SPX options"

// regimeGapWarnings names, one warning per row and reason, the inputs the
// broker refused, so a regime that publishes without them says so instead of
// reading as complete.
func regimeGapWarnings(kind broker.Kind, gaps *regimeGaps) []rpc.RegimeWarning {
	type rowGap struct {
		scope, kind, row, market string
		noRight                  bool
		inputs                   []string
	}
	var groups []*rowGap
	find := func(scope string, noRight bool, market string) *rowGap {
		for _, g := range groups {
			if g.scope == scope && g.noRight == noRight && g.market == market {
				return g
			}
		}
		return nil
	}
	for _, sym := range gaps.list() {
		r, ok := regimeGapRows[sym]
		if !ok {
			continue
		}
		var market string
		right, noRight := errors.AsType[*broker.NoQuoteRightError](gaps.refusal(sym))
		if noRight {
			market = right.Market
		}
		g := find(r.scope, noRight, market)
		if g == nil {
			g = &rowGap{scope: r.scope, kind: r.kind, row: r.row, market: market, noRight: noRight}
			groups = append(groups, g)
		}
		g.inputs = append(g.inputs, sym)
	}
	slices.SortFunc(groups, func(a, b *rowGap) int { return strings.Compare(a.scope, b.scope) })
	name := brokerDisplayName(kind)
	out := make([]rpc.RegimeWarning, 0, len(groups))
	for _, g := range groups {
		inputs := strings.Join(g.inputs, " and ")
		w := rpc.RegimeWarning{Scope: g.scope, Severity: "warning"}
		if g.noRight {
			w.Code = "broker_quote_right_missing"
			w.Message = fmt.Sprintf("This %s login has no %s quote right, so %s are missing from this regime.", name, g.market, inputs)
			w.Impact = fmt.Sprintf("The %s row is left out of the verdict, which rests on the other rows.", g.row)
			w.Action = fmt.Sprintf("Add the %s quote right to the %s login; the row returns on the next refresh.", g.market, name)
		} else {
			w.Code = "broker_input_not_supplied"
			w.Message = fmt.Sprintf("%s has no %s, so %s %s missing from this regime.", name, g.kind, inputs, isAre(len(g.inputs)))
			w.Impact = fmt.Sprintf("The %s row is left out of the verdict, which rests on the other rows.", g.row)
			w.Action = fmt.Sprintf("Nothing to fix: %s does not carry this data. On an IBKR account the row is read.", name)
		}
		out = append(out, w)
	}
	return out
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// brokerDisplayName is the broker's name as a reader knows it.
func brokerDisplayName(kind broker.Kind) string {
	switch kind.Normalized() {
	case broker.KindOpenD:
		return "OpenD"
	case broker.KindIBKR:
		return "IBKR"
	}
	return string(kind)
}
