package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"

	brokerIBKR "github.com/osauer/canary/v3/internal/broker/ibkr"
	"github.com/osauer/canary/v3/internal/rpc"
	positionstrategy "github.com/osauer/canary/v3/internal/strategy"
)

// A single-option line can share its contract with a strategy group: a call
// spread opened over calls the book already held (the order record groups
// one call with the sold one; the rest stay a line of their own,
// strategy_orders.go). The line is what the book holds outside every group
// that sells an option against the contract: a seller of single contracts
// (an exit plan, Reduce) sells at most that, re-read when it acts, and
// never the strategy's bought leg, which leaves only with its strategy (a
// group close or reduction) or after the group goes to review because its
// ratio broke. A name the grouping cannot read whole, with a sold option on
// it, has no line at all: its sold options may lean on any long.

// strategyCommittedLongs is the contracts of conID the book's strategy
// groups hold as a bought leg against a sold one: a group under review
// commits everything its leg holds.
func strategyCommittedLongs(groups []rpc.PositionStrategy, conID int) float64 {
	committed := 0.0
	for _, g := range groups {
		sold, mine := false, 0.0
		for _, leg := range g.Legs {
			sold = sold || leg.Ratio < 0
			if leg.Contract.ConID == conID && leg.Quantity > 0 {
				mine += leg.Quantity
			}
		}
		if sold {
			committed += mine
		}
	}
	return committed
}

// strategyFreeLine is row's line: what it holds outside the strategies that
// sell against it, and the blocker that refuses a single-contract sale when
// nothing is left outside them (or the name cannot be grouped and holds a
// sold option). Any other row is all line.
func strategyFreeLine(pos *rpc.PositionsResult, row rpc.PositionView) (float64, *rpc.TradingBlocker) {
	if pos == nil || row.ConID <= 0 || row.Quantity <= 0 || sameContractSecFamily(row.SecType) != "OPT" {
		return row.Quantity, nil
	}
	symbol := normSym(row.Symbol)
	// A sold option no strategy accounts for, on a name the grouping
	// cannot read whole, may lean on any long of it.
	grouped := map[int]float64{}
	for _, g := range pos.Strategies {
		for _, leg := range g.Legs {
			grouped[leg.Contract.ConID] += leg.Quantity
		}
	}
	for _, issue := range pos.StrategyIssues {
		if normSym(issue.Underlying) != symbol {
			continue
		}
		for _, other := range pos.Options {
			if other.Quantity-grouped[other.ConID] < -1e-9 && normSym(other.Symbol) == symbol {
				b := strategyLegBlocker(pos, row.ConID)
				return 0, &b
			}
		}
	}
	committed := strategyCommittedLongs(pos.Strategies, row.ConID)
	line := row.Quantity - committed
	if committed > 0 && line < 1-1e-9 {
		b := strategyLegBlocker(pos, row.ConID)
		return 0, &b
	}
	return math.Max(line, 0), nil
}

// optionLineFromRows is the line of conID, held contracts on the book,
// grouped from raw option rows as the positions read groups them
// (InferStrategies, then Canary's order records).
func optionLineFromRows(s *Server, rows []rpc.PositionView, conID int, held float64) float64 {
	groups, issues := positionstrategy.InferStrategies(nil, rows)
	groups, issues = s.reconcileJournaledStrategyGroups(nil, rows, groups, issues)
	row := rpc.PositionView{ConID: conID, SecType: "OPT", Quantity: held}
	for _, r := range rows {
		if r.ConID == conID {
			row = r
			row.Quantity = held
		}
	}
	line, _ := strategyFreeLine(&rpc.PositionsResult{Options: rows, Strategies: groups, StrategyIssues: issues}, row)
	return line
}

// optionLineQuantity is what an exit plan on contract may sell now, held
// contracts being held: its line, re-read from the broker's portfolio
// projection through the read seam at every call, so a spread closed since
// frees its calls and a line sold by hand reads as gone. Without the
// projection it fails closed, never reading the whole contract as the line.
func (s *Server) optionLineQuantity(_ context.Context, contract rpc.ContractParams, held float64) (float64, error) {
	if held <= 0 || !strings.EqualFold(strings.TrimSpace(contract.SecType), "OPT") {
		return held, nil
	}
	rows, err := s.heldOptionRows(contract.Symbol)
	if err != nil {
		return 0, err
	}
	if rows == nil {
		return held, nil
	}
	return optionLineFromRows(s, rows, contract.ConID, held), nil
}

// heldOptionRows is the book's option rows on the underlying, from the
// portfolio projection on the read seam's IBKR connector. nil, nil only in
// a test whose position seam stands in for the broker.
func (s *Server) heldOptionRows(symbol string) ([]rpc.PositionView, error) {
	if s.heldOptionRowsForTest != nil {
		return s.heldOptionRowsForTest(symbol)
	}
	if s.orderRiskAuthorityForTest != nil || s.orderPreviewPositionImpact != nil {
		return nil, nil
	}
	a, ok := s.brokerReads().(*brokerIBKR.Adapter)
	if !ok || a.Connector() == nil {
		return nil, fmt.Errorf("%w: the option book cannot be read to tell a line from a strategy's legs", ErrTradingDisabled)
	}
	c := a.Connector()
	session, ok := c.CaptureSession()
	if !ok {
		return nil, fmt.Errorf("%w: broker session unavailable", ErrTradingDisabled)
	}
	projection, ok := c.CapturePortfolioProjectionForSession(session)
	if !ok {
		return nil, fmt.Errorf("%w: the portfolio projection is unavailable", ErrTradingDisabled)
	}
	sym := normSym(symbol)
	rows := []rpc.PositionView{}
	for _, p := range projection.Positions {
		if p == nil || p.Position == 0 || !strings.EqualFold(p.Contract.SecType, "OPT") || normSym(p.Contract.Symbol) != sym {
			continue
		}
		rows = append(rows, rpc.PositionView{Symbol: sym, SecType: "OPT", ConID: p.Contract.ConID, Currency: p.Contract.Currency, Exchange: p.Contract.Exchange,
			Quantity: p.Position, Multiplier: p.Contract.Multiplier, Expiry: p.Contract.Expiry, Strike: p.Contract.Strike,
			Right: strings.ToUpper(strings.TrimSpace(p.Contract.Right)), LocalSymbol: p.Contract.LocalSymbol, TradingClass: p.Contract.TradingClass})
	}
	return rows, nil
}

// captureOptionLinePosition is the position authority an exit plan acts on:
// the broker's, its Before the contract's line rather than everything held
// of it, so the plan never plans, fires on or sells a strategy's bought leg.
func (s *Server) captureOptionLinePosition(ctx context.Context, contract rpc.ContractParams, action string, qty int) (orderPositionAuthority, error) {
	position, err := s.capturePreviewOrderPositionAuthority(ctx, s.currentTradingStatus(), contract, action, qty)
	if err != nil {
		return position, err
	}
	line, err := s.optionLineQuantity(ctx, contract, position.Impact.Before)
	if err != nil {
		return orderPositionAuthority{}, err
	}
	if line != position.Impact.Before {
		change := position.Impact.After - position.Impact.Before
		position.Impact.Before, position.Impact.After = line, line+change
	}
	return position, nil
}
