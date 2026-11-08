package daemon

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
)

// regimeGaps collects, during one fan-out, the regime inputs the broker
// refused because it does not supply them at all.
type regimeGaps struct {
	mu      sync.Mutex
	symbols []string
}

func (g *regimeGaps) add(symbol string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if sym := strings.TrimSpace(symbol); !slices.Contains(g.symbols, sym) {
		g.symbols = append(g.symbols, sym)
	}
}

func (g *regimeGaps) list() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Sorted(slices.Values(g.symbols))
}

// regimeGapRows says, for each input a broker may not supply, which row it
// feeds, what kind of quote it is and the row's name for a reader.
//
// DRAFT wording, 2026-11-08: the words below reach every regime reader and are
// the owner's to approve before this ships.
var regimeGapRows = map[string]struct{ scope, kind, row string }{
	"VIX":            {"vix_term_structure", "index quotes", "VIX term structure"},
	"VIX3M":          {"vix_term_structure", "index quotes", "VIX term structure"},
	"USD.JPY":        {"usd_jpy", "currency quotes", "USD/JPY"},
	regimeGammaInput: {"gamma_zero", "option chains", "gamma"},
}

// regimeGammaInput is the gamma row's broker input: the SPX option chain.
const regimeGammaInput = "SPX options"

// regimeGapWarnings names, one warning per row, the inputs the broker does not
// supply, so a regime that publishes without them says so instead of reading
// as complete.
func regimeGapWarnings(kind broker.Kind, symbols []string) []rpc.RegimeWarning {
	type rowGap struct {
		kind, row string
		inputs    []string
	}
	byScope := map[string]*rowGap{}
	var scopes []string
	for _, sym := range symbols {
		r, ok := regimeGapRows[sym]
		if !ok {
			continue
		}
		g := byScope[r.scope]
		if g == nil {
			g = &rowGap{kind: r.kind, row: r.row}
			byScope[r.scope] = g
			scopes = append(scopes, r.scope)
		}
		g.inputs = append(g.inputs, sym)
	}
	slices.Sort(scopes)
	name := brokerDisplayName(kind)
	out := make([]rpc.RegimeWarning, 0, len(scopes))
	for _, scope := range scopes {
		g := byScope[scope]
		out = append(out, rpc.RegimeWarning{
			Code:     "broker_input_not_supplied",
			Scope:    scope,
			Severity: "warning",
			Message:  fmt.Sprintf("%s gives no %s, so this regime has no %s reading.", name, g.kind, strings.Join(g.inputs, " or ")),
			Impact:   fmt.Sprintf("The %s row cannot rank on %s; the verdict rests on the rows that can.", g.row, name),
			Action:   fmt.Sprintf("Nothing to fix on %s: it does not carry this data. A daemon on IBKR reads it.", name),
		})
	}
	return out
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
