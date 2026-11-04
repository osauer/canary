package ibkr

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultExecutionsTimeout bounds one reqExecutions round trip. TWS answers
// a day's executions in well under a second; the generous bound covers the
// background pacing lane queueing behind interactive reads.
const defaultExecutionsTimeout = 15 * time.Second

// ExecutionsSnapshot is one answer to reqExecutions: the current day's
// executions TWS reports to this API client for the filtered account, as
// execDetails callbacks parsed into OrderLifecycleEvent (Type execDetails).
//
// Complete is true only when the broker's execDetailsEnd for this exact
// request ID arrived; a false Complete with rows means the deadline ended the
// collection and proves nothing about rows not seen. Which clients' executions
// appear is decided by TWS, not by this request: a non-master API client is
// documented to receive its own orders' executions, and the master client ID
// (TWS Global Configuration → API → Settings) receives every client's,
// including orders entered in TWS by hand. AsOf is the local UTC completion
// time, never a broker timestamp.
type ExecutionsSnapshot struct {
	Complete   bool
	Executions []OrderLifecycleEvent
	AsOf       time.Time
	RequestID  int
}

// executionsFlight collects the execDetails that answer one request ID until
// its end marker or the caller's deadline.
type executionsFlight struct {
	mu   sync.Mutex
	rows []OrderLifecycleEvent
	end  chan struct{}
	done bool
}

func (f *executionsFlight) add(ev OrderLifecycleEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return
	}
	f.rows = append(f.rows, ev)
}

func (f *executionsFlight) finish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return
	}
	f.done = true
	close(f.end)
}

func (f *executionsFlight) snapshot() []OrderLifecycleEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done = true
	return append([]OrderLifecycleEvent(nil), f.rows...)
}

// ExecutionsFilter narrows reqExecutions beyond the account. LastNDays is
// the filter's lastNDays field, sent from server version 200 (TWS API 10.30)
// and ignored by older servers; zero leaves it unset, which the broker reads
// as the current day. What a given server returns for a set value is the
// server's decision; the snapshot's dates say what came back.
type ExecutionsFilter struct {
	LastNDays int
}

// FetchExecutions sends reqExecutions with an account-only filter and waits
// for the same-request execDetailsEnd. ctx carries the pacing priority; a
// timeout of zero or less uses defaultExecutionsTimeout. The answer rows are
// routed to this flight by request ID and are never dispatched to the order
// lifecycle handlers, so a requested inventory cannot be mistaken for a live
// fill. A late answer after the deadline is dropped. This is a read request:
// nothing here places, changes or cancels an order.
func (c *Connector) FetchExecutions(ctx context.Context, account string, timeout time.Duration) (ExecutionsSnapshot, error) {
	return c.FetchExecutionsFiltered(ctx, account, ExecutionsFilter{}, timeout)
}

// FetchExecutionsFiltered is FetchExecutions with an explicit filter.
func (c *Connector) FetchExecutionsFiltered(ctx context.Context, account string, filter ExecutionsFilter, timeout time.Duration) (ExecutionsSnapshot, error) {
	now := time.Now().UTC()
	if c == nil {
		return ExecutionsSnapshot{AsOf: now}, fmt.Errorf("connector is nil")
	}
	if ctx == nil {
		return ExecutionsSnapshot{AsOf: now}, fmt.Errorf("context is nil")
	}
	if !c.IsReady() {
		return ExecutionsSnapshot{AsOf: now}, ErrIBKRUnavailable
	}
	if timeout <= 0 {
		timeout = defaultExecutionsTimeout
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return ExecutionsSnapshot{AsOf: now}, ErrIBKRUnavailable
	}
	reqID, err := conn.nextRequestID()
	if err != nil {
		return ExecutionsSnapshot{AsOf: now}, fmt.Errorf("request executions: %w", err)
	}
	flight := &executionsFlight{end: make(chan struct{})}
	c.executionsMu.Lock()
	if c.executions == nil {
		c.executions = make(map[int]*executionsFlight)
	}
	c.executions[reqID] = flight
	c.executionsMu.Unlock()
	endHandlerID := conn.RegisterHandler(msgExecDetailsEnd, func(fields []string) {
		if execDetailsEndRequestID(fields) == reqID {
			flight.finish()
		}
	})
	cleanupExpiring := func(expired bool) {
		conn.UnregisterHandler(msgExecDetailsEnd, endHandlerID)
		now := time.Now()
		c.executionsMu.Lock()
		delete(c.executions, reqID)
		if expired {
			c.expireExecutionsRequestLocked(reqID, now)
		}
		c.executionsMu.Unlock()
	}
	cleanup := func() { cleanupExpiring(false) }

	// REQ_EXECUTIONS version 3: request ID, then the ExecutionFilter in wire
	// order (clientId, acctCode, time, symbol, secType, exchange, side). A zero
	// client ID and empty fields leave those dimensions unfiltered; only the
	// account is pinned. The broker still decides which clients' executions
	// this API client may see.
	fields := []any{reqExecutions, 3, reqID, 0, strings.TrimSpace(account), "", "", "", "", ""}
	if conn.serverVersion >= minServerVerParametrizedDaysOfExecutions {
		// From server version 200 the filter ends with lastNDays (unset: an
		// empty field, as the broker's own clients send it) and the count of
		// specificDates (none). The gateway reads these two fields whatever
		// the frame's length: a frame that stopped at side made it read the
		// next request's bytes as them, so once a minute on the poll it
		// answered error 320 ("Unable to parse field: 'Client Req Id' for
		// input string: 'MART'") and the request after the poll was lost
		// (2026-10-18, every build since the poll shipped).
		var lastNDays any = ""
		if filter.LastNDays > 0 {
			lastNDays = filter.LastNDays
		}
		fields = append(fields, lastNDays, 0)
	}
	msg := conn.encodeMsg(fields...)
	if err := conn.sendMessageWithTypeContext(ctx, msg, RequestTypeGeneral); err != nil {
		cleanup()
		return ExecutionsSnapshot{AsOf: time.Now().UTC(), RequestID: reqID}, fmt.Errorf("request executions: %w", err)
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	complete := false
	var waitErr error
	select {
	case <-flight.end:
		complete = true
	case <-deadline.C:
	case <-ctx.Done():
		waitErr = ctx.Err()
	}
	// A deadline, not a caller's cancellation, leaves an end the broker may
	// still send: remember it so a late one is named.
	expired := !complete && (waitErr == nil || errors.Is(waitErr, context.DeadlineExceeded))
	cleanupExpiring(expired)
	return ExecutionsSnapshot{Complete: complete, Executions: flight.snapshot(), AsOf: time.Now().UTC(), RequestID: reqID}, waitErr
}

// executionsExpiredKeep bounds how long an expired request ID is remembered:
// a stalled Gateway has answered within minutes, and an end later than that
// is reported as unknown.
const executionsExpiredKeep = 10 * time.Minute

// expireExecutionsRequestLocked remembers a request whose flight reached its
// deadline without its execDetailsEnd and forgets those older than
// executionsExpiredKeep. The caller holds executionsMu.
func (c *Connector) expireExecutionsRequestLocked(reqID int, at time.Time) {
	if c.executionsExpired == nil {
		c.executionsExpired = make(map[int]time.Time)
	}
	for id, ended := range c.executionsExpired {
		if at.Sub(ended) > executionsExpiredKeep {
			delete(c.executionsExpired, id)
		}
	}
	c.executionsExpired[reqID] = at
}

// noteLateExecDetailsEnd reports an execDetailsEnd that arrived after its
// flight's deadline. The rows were dropped and the next poll reads the day's
// executions again, so the line is evidence of a slow answer, not a lost
// fill. It warns while the logging schedule wants attention and is INFO
// otherwise; an end later than executionsExpiredKeep, or one no request
// waited for, stays in the debug log. On 2026-10-20 such an end, during
// IBKR's nightly reset, logged "Unhandled message ID 55" at WARN as if the
// message were unknown.
func (c *Connector) noteLateExecDetailsEnd(fields []string, now time.Time) {
	reqID := execDetailsEndRequestID(fields)
	c.executionsMu.Lock()
	_, live := c.executions[reqID]
	ended, late := c.executionsExpired[reqID]
	if late {
		delete(c.executionsExpired, reqID)
		late = now.Sub(ended) <= executionsExpiredKeep
	}
	c.executionsMu.Unlock()
	switch {
	case live:
	case late:
		log := connectorLogger.Infof
		if c.logRequiredAt(now) {
			log = connectorLogger.Warnf
		}
		log("%s: IBKR answered executions request %d %s after its deadline; its rows were dropped and the next poll reads the day again", c.name, reqID, now.Sub(ended).Round(time.Millisecond))
	default:
		connectorLogger.Debugf("%s: execDetailsEnd for request %d, which no request is waiting for", c.name, reqID)
	}
}

// deliverRequestedExecution hands an execDetails that answers reqExecutions
// to the flight that asked. A row for an unknown request ID is a late answer
// to a flight that already ended and is dropped.
func (c *Connector) deliverRequestedExecution(ev OrderLifecycleEvent) {
	c.executionsMu.Lock()
	flight := c.executions[ev.RequestID]
	c.executionsMu.Unlock()
	if flight == nil {
		connectorLogger.Debugf("executions: dropping late execDetails for request %d (exec %s)", ev.RequestID, ev.ExecID)
		return
	}
	flight.add(ev)
}

// execDetailsEndRequestID reads the request ID from an execDetailsEnd frame in
// either encoding: legacy [55, version, reqId] or the protobuf summary with a
// reqId= field. Zero means absent or unparsable.
func execDetailsEndRequestID(fields []string) int {
	if len(fields) > 1 && fields[1] == "protobuf" {
		return orderEventInt(summaryFieldValue(fields, "reqId="))
	}
	if len(fields) < 3 {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(fields[2]))
	if err != nil {
		return 0
	}
	return v
}
