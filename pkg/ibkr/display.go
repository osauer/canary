package ibkr

import (
	"maps"
	"sync"
	"time"
)

// displayChanges broadcasts cache invalidation without blocking the wire reader.
type displayChanges struct {
	mu      sync.Mutex
	changed chan struct{}
}

func (d *displayChanges) watch() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.changed == nil {
		d.changed = make(chan struct{})
	}
	return d.changed
}
func (d *displayChanges) notify() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.changed != nil {
		close(d.changed)
	}
	d.changed = make(chan struct{})
}

// DisplayChanges returns a broadcast invalidation channel. Capture it before
// DisplaySnapshot to avoid missing a change between reading and waiting. Closing
// the channel means reread, not that every intermediate tick has been retained.
func (c *Connector) DisplayChanges() <-chan struct{} { return c.display.watch() }

// DisplaySnapshot is a detached, cache-only display projection. Session and
// portfolio health gate use of its contents. Receive clocks do not prove exchange
// freshness. It performs no broker requests and grants no execution authority.
type DisplaySnapshot struct {
	Session          ConnectorSessionBinding
	Account          *RawAccountSummary
	NetLiquidationAt time.Time
	Positions        []*RawPosition
	Health           PortfolioStreamHealth
	Quotes           map[string]*MarketData
	DataTypes        map[string]int
	AccountPnL       AccountDailyPnL
	PnLAccount       string
	PositionPnL      map[int]PositionDailyPnL
}

// CaptureDisplay snapshots caches within one inbound generation. False means
// the socket session is unavailable. Callers must also check portfolio scope,
// backend readiness, individual field clocks and current session before use.
func (c *Connector) CaptureDisplay() (DisplaySnapshot, bool) {
	binding, ok := c.CaptureSession()
	if !ok {
		return DisplaySnapshot{}, false
	}
	out, ok := c.captureDisplayAccount(binding)
	if !ok {
		return DisplaySnapshot{}, false
	}

	out.Quotes = c.MarketDataSnapshot()
	for key := range out.Quotes {
		out.DataTypes[key] = out.Quotes[key].FeedType
	}
	if !c.SessionCurrent(binding) {
		return DisplaySnapshot{}, false
	}
	return out, true
}

// The account/portfolio boundary must be released before taking subMu: some
// subscription paths hold subMu while waiting for a broker response.
func (c *Connector) captureDisplayAccount(binding ConnectorSessionBinding) (DisplaySnapshot, bool) {
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	binding.connection.inboundEpochMu.Lock()
	defer binding.connection.inboundEpochMu.Unlock()
	c.evidenceBarrier.Lock()
	defer c.evidenceBarrier.Unlock()
	if !c.SessionCurrent(binding) {
		return DisplaySnapshot{}, false
	}
	conn := binding.connection
	out := DisplaySnapshot{Session: binding, DataTypes: map[string]int{}}
	positions, health := conn.GetPositionsWithPortfolioHealth()
	out.Health = health
	for _, p := range positions {
		copy := *p
		copy.Contract.ComboLegs = append([]ComboLeg(nil), p.Contract.ComboLegs...)
		out.Positions = append(out.Positions, &copy)
	}
	conn.accountMu.RLock()
	if conn.displayAccount == out.Health.Account && accountCodeConcrete(conn.displayAccount) {
		out.Account = parseAccountSummary(maps.Clone(conn.displayAccountValues), conn.displayAccount)
	}
	conn.accountMu.RUnlock()
	if out.Account != nil {
		// Legacy summary AsOf is a read time. Display never exposes that clock.
		out.Account.AsOf = time.Time{}
		_, currency, found := lookupAccountValue(out.Account.Raw, "NetLiquidation", out.Account.BaseCurrency)
		if found {
			key := "NetLiquidation"
			if currency != "" && currency != "BASE" {
				key += "_" + currency
			}
			conn.accountMu.RLock()
			out.NetLiquidationAt = conn.accountValueTimes[key]
			conn.accountMu.RUnlock()
		}
	}
	c.pnl.mu.RLock()
	out.AccountPnL = c.pnl.account
	out.PnLAccount = c.pnl.accountAcct
	out.PositionPnL = maps.Clone(c.pnl.positionSnapshot)
	c.pnl.mu.RUnlock()
	return out, true
}

func stampDisplayPrice(sub *Subscription, tick int, at time.Time) {
	switch tick {
	case 1, 66:
		sub.BidAt = at
	case 2, 67:
		sub.AskAt = at
	case 4, 68:
		sub.LastAt = at
	case 37:
		sub.MarkAt = at
	case 9, 75:
		sub.CloseAt = at
	}
}
