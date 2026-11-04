package ibkr

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Instrument maps an IBKR contract onto the neutral identity. The ConID
// becomes the id as text; every other field the daemon reads off a contract
// is carried as it is. Market is not an IBKR contract field and stays empty;
// the daemon derives the route family from exchange and currency where it
// needs it. BondRules (a bond's size and price grid, an order-path input) are
// not part of the read identity.
func Instrument(c ibkrlib.Contract) broker.Instrument {
	in := broker.Instrument{
		Broker:       broker.KindIBKR,
		Symbol:       c.Symbol,
		SecType:      c.SecType,
		Venue:        c.Exchange,
		Primary:      c.PrimaryExch,
		Currency:     c.Currency,
		LocalSymbol:  c.LocalSymbol,
		TradingClass: c.TradingClass,
		Expiry:       c.Expiry,
		Strike:       c.Strike,
		Right:        c.Right,
		Multiplier:   c.Multiplier,
		SecIDType:    c.SecIDType,
		SecID:        c.SecID,
	}
	if c.ConID > 0 {
		in.ID = strconv.Itoa(c.ConID)
	}
	for _, leg := range c.ComboLegs {
		l := broker.Leg{Ratio: leg.Ratio, Action: leg.Action, Venue: leg.Exchange, OpenClose: leg.OpenClose}
		if leg.ConID > 0 {
			l.ID = strconv.Itoa(leg.ConID)
		}
		in.Legs = append(in.Legs, l)
	}
	return in
}

// Contract maps the neutral identity back onto an IBKR contract. An id that
// is not an integer (another broker's code) yields no ConID.
func Contract(in broker.Instrument) ibkrlib.Contract {
	c := ibkrlib.Contract{
		Symbol:       in.Symbol,
		SecType:      in.SecType,
		Exchange:     in.Venue,
		PrimaryExch:  in.Primary,
		Currency:     in.Currency,
		LocalSymbol:  in.LocalSymbol,
		TradingClass: in.TradingClass,
		Expiry:       in.Expiry,
		Strike:       in.Strike,
		Right:        in.Right,
		Multiplier:   in.Multiplier,
		SecIDType:    in.SecIDType,
		SecID:        in.SecID,
	}
	if id, ok := in.IDInt(); ok {
		c.ConID = id
	}
	for _, leg := range in.Legs {
		l := ibkrlib.ComboLeg{Ratio: leg.Ratio, Action: leg.Action, Exchange: leg.Venue, OpenClose: leg.OpenClose}
		if n, err := strconv.Atoi(strings.TrimSpace(leg.ID)); err == nil && n > 0 {
			l.ConID = n
		}
		c.ComboLegs = append(c.ComboLegs, l)
	}
	return c
}

// Resolve asks the connector for the exact contract behind in within the
// session b names, as the setup and chart paths do today.
func (a *Adapter) Resolve(ctx context.Context, b broker.SessionBinding, in broker.Instrument, timeout time.Duration) (broker.Instrument, error) {
	cb, ok := a.concreteFor(b)
	if !ok || !a.conn.SessionCurrent(cb) {
		return broker.Instrument{}, errSessionChanged
	}
	resolved, err := a.conn.ResolveOrderContractForSession(ctx, cb, Contract(in), timeout)
	if err != nil {
		return broker.Instrument{}, err
	}
	return Instrument(resolved.Contract), nil
}
