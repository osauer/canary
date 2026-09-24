package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func marketEventInventoryReceiptCurrent(md *ibkr.MarketData, now time.Time) bool {
	return md != nil && md.ShortableObserved && md.ShortableShares >= 0 && !md.ShortableTickAt.IsZero() && !md.ShortableTickAt.After(now) && now.Sub(md.ShortableTickAt) < marketEventsInventoryMaxAge
}

// readBorrowInventory owns receipt reuse and absence backoff. The transport
// callbacks borrow a subscription only for the bounded probe; releasing it
// does not erase a still-valid receipt. Nothing is restored as current on boot.
// Names in unquoted expect no market data: they are neither probed nor counted
// as missing, and the notes report them as not expected.
func (c *marketEventCache) readBorrowInventory(ctx context.Context, symbols, unquoted []string, binding ibkr.ConnectorSessionBinding, res *rpc.MarketEventsResult, current func() bool, peek func(string) *ibkr.MarketData, probe func(context.Context, string) (*ibkr.MarketData, error)) rpc.SourceHealth {
	notExpected := 0
	symbols = slices.DeleteFunc(slices.Clone(symbols), func(symbol string) bool {
		if slices.Contains(unquoted, symbol) {
			notExpected++
			return true
		}
		return false
	})
	unknown := func(note string) rpc.SourceHealth {
		return marketEventSourceHealth("borrow_inventory", rpc.SourceStatusUnknown, time.Time{}, c.now().UTC(), marketEventsInventoryMaxAge, "low", []string{note})
	}
	c.mu.Lock()
	if c.borrowInventoryGate == nil {
		c.borrowInventoryGate = make(chan struct{}, 1)
	}
	gate := c.borrowInventoryGate
	c.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return unknown("shortable-share observation canceled")
	}
	defer func() { <-gate }()
	if ctx.Err() != nil || !current() {
		return unknown("shortable-share session is no longer current")
	}
	now := c.now().UTC()
	c.mu.Lock()
	if c.shortableBinding != binding {
		c.shortableBinding = binding
		c.shortableAbsent, c.shortableReceipts = nil, nil
	}
	for symbol, at := range c.shortableAbsent {
		if at.After(now) || now.Sub(at) >= marketEventsShortableAbsentRetry {
			delete(c.shortableAbsent, symbol)
		}
	}
	for symbol, rec := range c.shortableReceipts {
		if rec.AsOf.After(now) || now.Sub(rec.AsOf) >= marketEventsInventoryMaxAge {
			delete(c.shortableReceipts, symbol)
		}
	}
	c.mu.Unlock()
	probes := make([]*ibkr.MarketData, len(symbols))
	jobs := make([]int, 0, min(len(symbols), marketEventsInventoryCacheLimit))
	skipped := 0
	for i, sym := range symbols {
		// A newly arrived tick outranks a previous negative probe, including
		// one received by another active subscription during its backoff.
		if md := peek(sym); marketEventInventoryReceiptCurrent(md, now) {
			probes[i] = md
			continue
		}
		c.mu.Lock()
		rec, ok := c.shortableReceipts[sym]
		c.mu.Unlock()
		if ok {
			probes[i] = inventoryRecordData(rec)
			continue
		}
		if c.shortableAbsentRecently(sym, now) {
			skipped++
			continue
		}
		if len(jobs) < marketEventsInventoryCacheLimit {
			jobs = append(jobs, i)
		}
	}
	runBounded(jobs, marketEventsBorrowPollWorkers, func(i int) {
		if ctx.Err() != nil || !current() {
			return
		}
		probeCtx, cancel := context.WithTimeout(ctx, marketEventsBorrowPollBudget)
		defer cancel()
		md, err := probe(probeCtx, symbols[i])
		if ctx.Err() != nil || !current() {
			return
		}
		at := c.now().UTC()
		if marketEventInventoryReceiptCurrent(md, at) {
			probes[i] = md
			return
		}
		// Cancellation, a subscription setup failure or a reconnect did not
		// complete a missing-tick observation and cannot suppress later work.
		if errors.Is(err, context.DeadlineExceeded) || IsSubscriptionRejected(err) {
			c.rememberShortableAbsent(symbols[i], at, binding)
		}
	})
	if ctx.Err() != nil || !current() {
		return unknown("shortable-share session changed or observation canceled")
	}
	now = c.now().UTC()
	observations := make(map[string]marketEventBorrowInventoryRecord)
	var flags []rpc.MarketEventFlag
	oldest := time.Time{}
	for i, md := range probes {
		if !marketEventInventoryReceiptCurrent(md, now) {
			continue
		}
		sym := symbols[i]
		observations[sym] = marketEventBorrowInventoryRecord{Symbol: sym, ShortableShares: md.ShortableShares, AsOf: md.ShortableTickAt, DataType: md.DataType, Delayed: md.IsDelayed}
		if oldest.IsZero() || md.ShortableTickAt.Before(oldest) {
			oldest = md.ShortableTickAt
		}
		if flag, ok := marketEventBorrowInventoryFlag(sym, *md, now); ok {
			flags = append(flags, flag)
		}
	}
	if err := c.persistBorrowInventory(ctx, now, observations); err != nil {
		return unknown("shortable-share observations could not be retained")
	}
	if ctx.Err() != nil || !current() {
		return unknown("shortable-share session changed before publication")
	}
	c.mu.Lock()
	if c.shortableBinding == binding {
		if c.shortableReceipts == nil {
			c.shortableReceipts = make(map[string]marketEventBorrowInventoryRecord)
		}
		for sym, rec := range observations {
			delete(c.shortableAbsent, sym)
			if _, exists := c.shortableReceipts[sym]; exists || len(c.shortableReceipts) < marketEventsInventoryCacheLimit {
				c.shortableReceipts[sym] = rec
			}
		}
	}
	c.mu.Unlock()
	res.Flags = append(res.Flags, flags...)
	status, confidence := rpc.SourceStatusUnknown, "low"
	if len(observations) > 0 {
		status, confidence = rpc.SourceStatusPartial, "medium-low"
	}
	if len(symbols) > 0 && len(observations) == len(symbols) {
		status, confidence = rpc.SourceStatusOK, "medium"
	}
	notes := []string{fmt.Sprintf("current shortable-share receipts cover %d/%d expected symbols", len(observations), len(symbols))}
	if notExpected > 0 {
		notes = append(notes, fmt.Sprintf("%d held symbols expect no market data and are not expected to report shortable shares", notExpected))
	}
	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d missing-tick probes are in bounded backoff", skipped))
	}
	health := marketEventSourceHealth("borrow_inventory", status, oldest, now, marketEventsInventoryMaxAge, confidence, notes)
	c.mu.Lock()
	for _, sym := range symbols {
		if _, observed := observations[sym]; observed {
			continue
		}
		if at, absent := c.shortableAbsent[sym]; absent && !at.After(now) {
			next := at.Add(marketEventsShortableAbsentRetry)
			if now.Before(next) && (health.NextAttempt == nil || next.Before(*health.NextAttempt)) {
				health.NextAttempt = &next
			}
		}
	}
	c.mu.Unlock()
	return health
}

func inventoryRecordData(rec marketEventBorrowInventoryRecord) *ibkr.MarketData {
	return &ibkr.MarketData{ShortableObserved: true, ShortableShares: rec.ShortableShares, ShortableTickAt: rec.AsOf, DataType: rec.DataType, IsDelayed: rec.Delayed}
}
