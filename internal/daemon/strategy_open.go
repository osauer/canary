package daemon

import (
	"strings"

	"github.com/osauer/canary/v3/internal/rpc"
	positionstrategy "github.com/osauer/canary/v3/internal/strategy"
)

// strategyOpenBlockerCode marks a preview refused because the order is not a
// debit vertical Canary opens (strategy_add.go).
const strategyOpenBlockerCode = "strategy_open_shape"

// strategyOpenGroup is the group a debit vertical opens from flat: its two
// contracts by ID, the bought one ratio 1 and the sold one ratio -1, under
// the ID the daemon's grouping gives them once held, so the fill lands on the
// strategy this order records. The preview resolves and validates both legs.
//
// Other options of the name may be held, the bought contract too: the order
// record makes the spread a group of its own (reconcileJournaledStrategyGroups),
// and what the book holds beyond it stays a line of its own. The sold contract
// must not be held, nor the bought one held short.
func strategyOpenGroup(positions *rpc.PositionsResult, p rpc.StrategyPreviewParams) (rpc.PositionStrategy, error) {
	open := p.Open
	if open == nil {
		return rpc.PositionStrategy{}, errBadRequest("open needs the bought and the sold option: open.long and open.short, as strategy spreads returns them")
	}
	if strings.TrimSpace(p.StrategyID) != "" || p.ExpectedRevision != 0 {
		return rpc.PositionStrategy{}, errBadRequest("open takes no strategy id or revision: it opens a new strategy")
	}
	long, short := open.Long, open.Short
	symbol := normSym(long.Symbol)
	for _, c := range []rpc.ContractParams{long, short} {
		if symbol == "" || normSym(c.Symbol) != symbol || !strings.EqualFold(c.SecType, "OPT") || c.ConID <= 0 || strings.TrimSpace(c.Expiry) == "" || !(c.Strike > 0) || (c.Right != "C" && c.Right != "P") {
			return rpc.PositionStrategy{}, errBadRequest("open needs two exact options on one underlying: con_id, symbol, sec_type OPT, expiry, strike and right C or P")
		}
	}
	if long.ConID == short.ConID {
		return rpc.PositionStrategy{}, errBadRequest("a spread buys and sells two different options")
	}
	if positions == nil || !currentPortfolioAuthority(positions.Authority) {
		return rpc.PositionStrategy{}, errBadRequest("the positions read is not current; a spread is opened only on a book Canary can read")
	}
	for _, row := range positions.Options {
		switch {
		case row.Quantity == 0:
		case row.ConID == short.ConID:
			return rpc.PositionStrategy{}, errBadRequest("the book holds the option this spread sells already; a spread sells a contract the book does not hold")
		case row.ConID == long.ConID && row.Quantity < 0:
			return rpc.PositionStrategy{}, errBadRequest("the book is short the option this spread buys; buying it back is a close, not a spread")
		}
	}
	leg := func(c rpc.ContractParams, ratio int) rpc.PositionStrategyLeg {
		c.Symbol, c.SecType, c.Exchange, c.Currency = symbol, "OPT", "SMART", "USD"
		return rpc.PositionStrategyLeg{Contract: c, Ratio: ratio}
	}
	return rpc.PositionStrategy{
		ID: positionstrategy.StrategyID(symbol, long.ConID, short.ConID), Underlying: symbol,
		Status: rpc.PositionStrategyStatusCurrent, Actionable: true,
		Legs: []rpc.PositionStrategyLeg{leg(long, 1), leg(short, -1)},
	}, nil
}
