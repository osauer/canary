package ibkr

import (
	"github.com/osauer/canary/v2/internal/broker"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Position maps one cached portfolio row onto the neutral position; its
// contract becomes the instrument.
func Position(p *ibkrlib.RawPosition) broker.Position {
	if p == nil {
		return broker.Position{}
	}
	return broker.Position{
		ValuationAt:   p.ValuationAt,
		Account:       p.Account,
		Instrument:    Instrument(p.Contract),
		Quantity:      p.Position,
		MarketPrice:   p.MarketPrice,
		MarketValue:   p.MarketValue,
		AverageCost:   p.AverageCost,
		UnrealizedPnL: p.UnrealizedPNL,
		RealizedPnL:   p.RealizedPNL,
	}
}

// Positions maps the cached rows in their order, skipping nil entries.
func Positions(in []*ibkrlib.RawPosition) []broker.Position {
	out := make([]broker.Position, 0, len(in))
	for _, p := range in {
		if p != nil {
			out = append(out, Position(p))
		}
	}
	return out
}

// PositionsHealth maps the portfolio stream's receipt. The two structs are
// field for field the same, so the value converts directly: every timestamp
// and the projection generation are carried as they are.
func PositionsHealth(h ibkrlib.PortfolioStreamHealth) broker.PositionsHealth {
	return broker.PositionsHealth(h)
}

// StreamHealth is the inverse, for the daemon's IBKR-typed classification
// of a receipt at the handler boundary.
func StreamHealth(h broker.PositionsHealth) ibkrlib.PortfolioStreamHealth {
	return ibkrlib.PortfolioStreamHealth(h)
}

// Positions reads the held book from the client's portfolio cache with the
// stream's own receipt; it issues no request of its own.
func (a *Adapter) Positions() ([]broker.Position, broker.PositionsHealth, error) {
	if a == nil || a.conn == nil {
		return nil, broker.PositionsHealth{}, ibkrlib.ErrIBKRUnavailable
	}
	rows, health, err := a.conn.CachedPositionsWithHealth()
	if err != nil {
		return nil, broker.PositionsHealth{}, err
	}
	return Positions(rows), PositionsHealth(health), nil
}
