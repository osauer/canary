package ibkr

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Delayed requests support only a subset of the normal generic tick list.
const delayedQuoteGenericTicks = "101,106,165,221,236"

func (c *Connector) sharedQuoteMode(key string) (int, error) {
	absent := c.marketDataAbsenceFor(key)
	if absent == nil {
		return 0, nil
	}
	if absent.Code != 354 {
		return 0, absent
	}
	c.absenceMu.Lock()
	state := c.mktDataAbsent[key]
	allowed := state.delayedAvailable || state.delayedAttemptAt.IsZero() || c.absenceClock().Sub(state.delayedAttemptAt) >= marketDataAbsenceRetry
	c.absenceMu.Unlock()
	if !allowed {
		return 0, absent
	}
	return 4, nil
}

func (c *Connector) recordDelayedQuoteAttempt(key string) {
	c.absenceMu.Lock()
	defer c.absenceMu.Unlock()
	state, ok := c.mktDataAbsent[key]
	if ok {
		state.delayedAttemptAt = c.absenceClock()
		c.mktDataAbsent[key] = state
	}
}

func (c *Connector) recordDelayedQuoteAvailable(key string) {
	c.absenceMu.Lock()
	defer c.absenceMu.Unlock()
	state, ok := c.mktDataAbsent[key]
	if ok {
		state.delayedAvailable = true
		c.mktDataAbsent[key] = state
	}
}

func (c *Connector) subscribeSharedQuote(ctx context.Context, key string, fields []string, spec mdReplaySpec) error {
	c.subMu.RLock()
	existing := c.subscriptions[key] != nil
	c.subMu.RUnlock()
	if existing {
		return nil
	}
	mode, err := c.sharedQuoteMode(key)
	if err != nil {
		return err
	}
	contract, ticks, err := marketDataReplayRequest(spec)
	if err != nil {
		return err
	}
	sub := &Subscription{Symbol: key, Fields: fields, LastTime: time.Now(), RejectCh: make(chan SubscriptionRejection, 1), replaySpec: &spec, delayedFallback: mode == 4}
	c.subMu.Lock()
	if c.subscriptions[key] != nil {
		c.subMu.Unlock()
		return nil
	}
	c.subscriptions[key] = sub
	c.subMu.Unlock()
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return nil
	}
	origin := ConnectorSessionBinding{connector: c, connection: conn, epoch: conn.BrokerSessionEpoch()}
	if mode == 4 {
		ticks = delayedQuoteGenericTicks
		c.recordDelayedQuoteAttempt(key)
	}
	adopted := false
	id, err := conn.requestMarketDataWithContractForEpochMode(ctx, contract, ticks, false, false, origin.epoch, false, mode, func(id int) func() {
		c.subMu.Lock()
		if c.subscriptions[key] == sub {
			sub.ReqID = id
			c.reqIDMap[id] = key
			adopted = true
		}
		c.subMu.Unlock()
		return func() { c.removeSharedQuoteRequest(key, sub, id) }
	})
	if err != nil {
		c.removeSharedQuoteRequest(key, sub, id)
		return err
	}
	if !adopted {
		_ = conn.cancelMarketDataForEpoch(ctx, id, origin.epoch)
		return fmt.Errorf("quote subscription removed during request")
	}
	if mode == 4 {
		c.scheduleQuoteLiveRetry(origin, key, sub)
	}
	return nil
}

func (c *Connector) removeSharedQuoteRequest(key string, sub *Subscription, id int) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if c.subscriptions[key] == sub && (id == 0 || sub.ReqID == id) {
		delete(c.reqIDMap, sub.ReqID)
		if sub.liveRetryTimer != nil {
			sub.liveRetryTimer.Stop()
		}
		delete(c.subscriptions, key)
	}
}

// prepareDelayedQuoteRecovery runs under the incoming notice's session lease.
// Claim the one attempt before returning outbound work to the post-barrier path.
// Exact-session and derivative subscriptions retain their existing owners.
// The line keeps the refusal message so the delayed service stays disclosed.
func (c *Connector) prepareDelayedQuoteRecovery(origin ConnectorSessionBinding, id int, message string) func() {
	c.subMu.Lock()
	key := c.reqIDMap[id]
	sub := c.subscriptions[key]
	if sub == nil || sub.ReqID != id || sub.SessionEpoch != 0 || sub.replaySpec == nil {
		c.subMu.Unlock()
		return nil
	}
	contract, _, err := marketDataReplayRequest(*sub.replaySpec)
	switch strings.ToUpper(contract.SecType) {
	case "OPT", "FOP", "WAR", "BAG":
		c.subMu.Unlock()
		return nil
	}
	if err != nil || sub.delayedFallback {
		c.subMu.Unlock()
		c.absenceMu.Lock()
		state, ok := c.mktDataAbsent[key]
		if ok {
			state.delayedAvailable = false
			c.mktDataAbsent[key] = state
		}
		c.absenceMu.Unlock()
		return nil
	}
	sub.delayedFallback = true
	sub.fallbackRefusal = &marketDataAbsence{code: 354, message: message, at: c.absenceClock()}
	c.subMu.Unlock()
	c.recordDelayedQuoteAttempt(key)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.replaceSharedQuote(ctx, origin, key, sub, id, 4); err != nil {
			c.pushSubscriptionRejection(id, 354, "delayed quote recovery unavailable")
			c.logWarn("Delayed quote recovery failed for %s: %v", key, err)
		}
		c.scheduleQuoteLiveRetry(origin, key, sub)
	}
}

// replaceSharedQuote keeps the prior observation visible, with its original
// clock and feed type, until the replacement supplies an actual positive price.
func (c *Connector) replaceSharedQuote(ctx context.Context, origin ConnectorSessionBinding, key string, sub *Subscription, oldID, mode int) error {
	if !c.SessionCurrent(origin) {
		return fmt.Errorf("quote recovery session changed")
	}
	c.subMu.Lock()
	if c.subscriptions[key] != sub || sub.ReqID != oldID {
		c.subMu.Unlock()
		return fmt.Errorf("quote recovery subscription changed")
	}
	spec := *sub.replaySpec
	cancelOld := wireCancelNeeded(sub)
	c.subMu.Unlock()
	contract, ticks, err := marketDataReplayRequest(spec)
	if err != nil {
		return err
	}
	if mode == 4 {
		ticks = delayedQuoteGenericTicks
	}
	if cancelOld {
		if err := origin.connection.cancelMarketDataForEpoch(ctx, oldID, origin.epoch); err != nil {
			return err
		}
	} else {
		origin.connection.releaseMarketDataSlotAtEpoch(oldID, origin.epoch)
	}
	adopted := false
	id, err := origin.connection.requestMarketDataWithContractForEpochMode(ctx, contract, ticks, false, false, origin.epoch, false, mode, func(id int) func() {
		c.subMu.Lock()
		if c.subscriptions[key] == sub && sub.ReqID == oldID {
			if !sub.LastPriceTickAt.IsZero() {
				previous := *sub
				previous.previousQuote, previous.liveRetryTimer = nil, nil
				sub.previousQuote, sub.previousDataType = &previous, c.subscriptionDataType(sub)
			}
			resetSubscriptionObservations(sub)
			sub.delayedFallback = mode == 4
			sub.ReqID, sub.LastTime = id, time.Now()
			delete(c.reqIDMap, oldID)
			c.reqIDMap[id] = key
			select {
			case <-sub.RejectCh:
			default:
			}
			adopted = true
		}
		c.subMu.Unlock()
		return func() {
			c.subMu.Lock()
			defer c.subMu.Unlock()
			if adopted && c.subscriptions[key] == sub && sub.ReqID == id {
				delete(c.reqIDMap, id)
				c.reqIDMap[oldID] = key
				sub.ReqID, sub.rejectedReqID = oldID, oldID
			}
		}
	})
	if err != nil {
		return err
	}
	if !adopted {
		_ = origin.connection.cancelMarketDataForEpoch(ctx, id, origin.epoch)
		return fmt.Errorf("quote recovery owner retired")
	}
	c.display.notify()
	return nil
}

func (c *Connector) scheduleQuoteLiveRetry(origin ConnectorSessionBinding, key string, sub *Subscription) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if c.subscriptions[key] != sub {
		return
	}
	if !sub.delayedFallback && sub.previousQuote == nil && !sub.LastPriceTickAt.IsZero() {
		return
	}
	if sub.liveRetryTimer != nil {
		sub.liveRetryTimer.Stop()
	}
	sub.liveRetryTimer = time.AfterFunc(marketDataAbsenceRetry, func() { c.retrySharedQuoteLive(origin, key, sub) })
}

func (c *Connector) retrySharedQuoteLive(origin ConnectorSessionBinding, key string, sub *Subscription) {
	if !c.SessionCurrent(origin) {
		return
	}
	c.subMu.Lock()
	if c.subscriptions[key] != sub {
		c.subMu.Unlock()
		return
	}
	id := sub.ReqID
	c.subMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Type 2 retains the ordinary live/frozen behavior. A new 354 rearms one
	// fallback; a silent probe retains the cached delayed value and is bounded.
	if err := c.replaceSharedQuote(ctx, origin, key, sub, id, 2); err != nil {
		c.logWarn("Live quote retry failed for %s: %v", key, err)
	} else {
		// A silent live probe must not leave the delayed stream cancelled until
		// the next half-hour retry. Restore best-available acquisition promptly.
		for ctx.Err() == nil {
			c.subMu.RLock()
			resolved := c.subscriptions[key] != sub || sub.delayedFallback || !sub.LastPriceTickAt.IsZero()
			c.subMu.RUnlock()
			if resolved {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(25 * time.Millisecond):
			}
		}
		c.subMu.RLock()
		pending := c.subscriptions[key] == sub && !sub.delayedFallback && sub.LastPriceTickAt.IsZero()
		probeID := sub.ReqID
		c.subMu.RUnlock()
		if pending {
			fallbackCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			if err := c.replaceSharedQuote(fallbackCtx, origin, key, sub, probeID, 4); err != nil {
				c.logWarn("Quote fallback after silent live probe failed for %s: %v", key, err)
			}
			stop()
		}
	}
	c.scheduleQuoteLiveRetry(origin, key, sub)
}
