package ibkr

import (
	"context"
	"time"
)

// Cache publication holds the session barriers only for a small memory update,
// never while waiting for pacing or a socket write.
func (c *Connector) mutatePnLForSession(origin ConnectorSessionBinding, change func()) bool {
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	c.evidenceBarrier.Lock()
	defer c.evidenceBarrier.Unlock()
	if !c.SessionCurrent(origin) {
		return false
	}
	c.pnl.mu.Lock()
	defer c.pnl.mu.Unlock()
	change()
	return true
}

func (c *Connector) resetPnLSessionLocked(origin ConnectorSessionBinding) {
	if c.pnl.session == origin {
		return
	}
	c.pnl.session = origin
	c.pnl.accountReqID, c.pnl.accountAcct = 0, ""
	c.pnl.accountStartedAt, c.pnl.account = time.Time{}, AccountDailyPnL{}
	clear(c.pnl.positionReqIDs)
	clear(c.pnl.positionByReqID)
	clear(c.pnl.positionSnapshot)
}

func (c *Connector) sendPnLForSession(ctx context.Context, origin ConnectorSessionBinding, msg []byte, owns func() bool) error {
	return origin.connection.sendMessageWithTypeContextForEpochGuarded(ctx, msg, RequestTypeGeneral, origin.epoch, true, func() error {
		if !c.SessionCurrent(origin) {
			return ErrIBKRUnavailable
		}
		if owns != nil {
			c.pnl.mu.RLock()
			valid := owns()
			c.pnl.mu.RUnlock()
			if !valid {
				return ErrIBKRUnavailable
			}
		}
		return nil
	})
}

// receivePnLForSession runs under dispatch's publication, inbound and evidence
// leases, in that order. Reacquiring publication here would invert that order.
func (c *Connector) receivePnLForSession(origin ConnectorSessionBinding, receive func()) {
	if !c.SessionReceiptCurrent(origin) {
		return
	}
	c.pnl.mu.RLock()
	owns := c.pnl.session == origin
	c.pnl.mu.RUnlock()
	if owns {
		receive()
	}
}

// An old repair must not discard a reservation from a successor namespace that
// happens to reuse the same numeric request ID.
func discardPnLReservation(origin ConnectorSessionBinding, id int) {
	conn := origin.connection
	conn.reqIDMu.Lock()
	defer conn.reqIDMu.Unlock()
	if conn.brokerSessionEpoch.Load() == origin.epoch {
		delete(conn.reservedRequestIDs, id)
	}
}

func (c *Connector) resubscribeAccountUpdatesForSession(origin ConnectorSessionBinding) error {
	if !c.SessionCurrent(origin) {
		return ErrIBKRUnavailable
	}
	c.acctUpdatesMu.Lock()
	account := c.acctUpdatesAccount
	c.acctUpdatesMu.Unlock()
	conn := origin.connection
	msg := conn.encodeMsg(reqAcctData, "2", "1", account)
	return conn.sendMessageWithTypeContextForEpochGuarded(context.Background(), msg, RequestTypeGeneral, origin.epoch, true, func() error {
		if !c.SessionCurrent(origin) {
			return ErrIBKRUnavailable
		}
		now := c.acctUpdatesClock()
		c.acctUpdatesMu.Lock()
		c.acctUpdatesLastAt = now
		c.acctUpdatesMu.Unlock()
		conn.resetPortfolioStreamHealthUnderEvidence(account, now.UTC())
		return nil
	})
}
