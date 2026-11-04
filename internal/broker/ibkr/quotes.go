package ibkr

import (
	"context"
	"errors"

	"github.com/osauer/canary/v2/internal/broker"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// errSessionChanged is the adapter's refusal for a binding that no longer
// names the connector's session; it wraps the client's unavailable error so
// callers that classify on it keep working.
var errSessionChanged = errors.Join(ibkrlib.ErrIBKRUnavailable, errors.New("broker session changed"))

// Key is the subscription key the connector files a routed line under.
func (a *Adapter) Key(in broker.Instrument) string {
	return ibkrlib.MarketDataKeyForContract(Contract(in))
}

// SubscribeSymbol opens a line by bare symbol; the client resolves its
// default route. The binding is the session captured before the request so
// the caller can tell whether the line belongs to the session it still has.
func (a *Adapter) SubscribeSymbol(ctx context.Context, symbol string, fields []string) (string, broker.SessionBinding, error) {
	if a == nil || a.conn == nil {
		return "", broker.SessionBinding{}, ibkrlib.ErrIBKRUnavailable
	}
	b, ok := a.Capture()
	if !ok {
		return "", broker.SessionBinding{}, ibkrlib.ErrIBKRUnavailable
	}
	return symbol, b, a.conn.SubscribeMarketData(ctx, symbol, fields)
}

// Subscribe opens a line for an exact instrument.
func (a *Adapter) Subscribe(ctx context.Context, in broker.Instrument, fields []string) (string, broker.SessionBinding, error) {
	if a == nil || a.conn == nil {
		return "", broker.SessionBinding{}, ibkrlib.ErrIBKRUnavailable
	}
	b, ok := a.Capture()
	if !ok {
		return "", broker.SessionBinding{}, ibkrlib.ErrIBKRUnavailable
	}
	key, err := a.conn.SubscribeMarketDataWithContract(ctx, Contract(in), fields)
	return key, b, err
}

// Unsubscribe releases a line regardless of session.
func (a *Adapter) Unsubscribe(ctx context.Context, key string) error {
	if a == nil || a.conn == nil {
		return ibkrlib.ErrIBKRUnavailable
	}
	return a.conn.UnsubscribeMarketDataContext(ctx, key)
}

// UnsubscribeForSession releases a shared line only while b still names
// the session that opened it, as the subscription manager does today.
func (a *Adapter) UnsubscribeForSession(ctx context.Context, b broker.SessionBinding, key string) error {
	cb, ok := a.concreteFor(b)
	if !ok {
		return nil
	}
	return a.conn.UnsubscribeSharedMarketDataForSession(ctx, cb, key)
}

// Snapshot is the connector's detached market-data cache, mapped.
func (a *Adapter) Snapshot() map[string]*broker.Quote {
	if a == nil || a.conn == nil {
		return nil
	}
	raw := a.conn.MarketDataSnapshot()
	out := make(map[string]*broker.Quote, len(raw))
	for key, md := range raw {
		if md == nil {
			continue
		}
		q := Quote(md)
		out[key] = &q
	}
	return out
}

// Load is not reported by the IBKR client in this phase; the daemon keeps
// its own line accounting.
func (a *Adapter) Load() (broker.Load, bool) { return broker.Load{}, false }

// Quote maps one cached market-data line onto the neutral quote.
func Quote(md *ibkrlib.MarketData) broker.Quote {
	if md == nil {
		return broker.Quote{}
	}
	return broker.Quote{
		Symbol:         md.Symbol,
		FeedType:       md.FeedType,
		Timestamp:      md.Timestamp,
		LastAt:         md.LastAt,
		BidAt:          md.BidAt,
		AskAt:          md.AskAt,
		MarkAt:         md.MarkAt,
		CloseAt:        md.CloseAt,
		Last:           md.Last,
		Bid:            md.Bid,
		Ask:            md.Ask,
		Mid:            md.Mid,
		MarkPrice:      md.MarkPrice,
		Open:           md.Open,
		High:           md.High,
		Low:            md.Low,
		Close:          md.Close,
		VWAP:           md.VWAP,
		Volume:         md.Volume,
		VolumeObserved: md.VolumeObserved,
		VolumeAt:       md.VolumeAt,
		AvgVolume:      md.AvgVolume,
		BidSize:        md.BidSize,
		AskSize:        md.AskSize,
		LastTickAt:     md.LastTickAt,
		LastPriceTick:  md.LastPriceTickAt,
		LastTradeTime:  md.LastTradeTime,
	}
}
