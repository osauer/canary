package ibkr

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

// AccountCurrencySettlementProbe records an explicit currency-only Multi
// subscription. No amounts, account identities or cash authority are returned.
// API1050 EClient/EDecoder verify opcodes76/77 and callbacks73/74. Account
// protobuf starts at207; this experiment refuses that unimplemented protocol.
type AccountCurrencySettlementProbe struct {
	Status, CancelStatus, ClockSource                string
	BrokerErrorCode                                  int
	SocketEpoch                                      uint64
	RequestedAt, CompletedAt, LastCallbackAt, ReadAt time.Time
	Callbacks                                        int
	Rows                                             []AccountStreamRow
	RowsTruncated                                    bool
}
type currencyProbeSnapshot struct {
	account                                  string
	epoch                                    uint64
	done                                     chan struct{}
	terminal, conflict, malformed            bool
	brokerErrorCode                          int
	requestedAt, completedAt, lastCallbackAt time.Time
	callbacks                                int
	rows                                     map[string]AccountStreamRow
	truncated                                bool
}

func (c *Connection) currencyProbeRawCaptureEnabled() bool {
	c.packetLoggerMu.RLock()
	packet := c.packetLogger != nil
	c.packetLoggerMu.RUnlock()
	return packet || c.logWireHex || (c.config != nil && c.config.PacketLogPath != "") || (c.wireTap != nil && c.wireTap.Enabled())
}

// RequestCurrencySettlementProbe is opt-in only. It uses the existing ready
// connector with exact account, empty model and ledgerAndNLV=true. Ordinary
// account reads retain priority through the existing diagnostic scheduling lane.
func (c *Connector) RequestCurrencySettlementProbe(ctx context.Context, timeout time.Duration) (out AccountCurrencySettlementProbe) {
	out.Status, out.CancelStatus, out.ClockSource = "unavailable", "not_started", "local_receive"
	if ctx == nil || ctx.Err() != nil {
		out.Status = "cancelled"
		return
	}
	origin, ok := c.CaptureSession()
	if !ok {
		return
	}
	conn := origin.connection
	if version := conn.ServerVersion(); version < 103 || version >= 207 {
		out.Status = "unsupported_protocol"
		return
	}
	if conn.currencyProbeRawCaptureEnabled() {
		out.Status = "skipped_raw_capture"
		return
	}
	expected := accountSummaryExpectedAccount(conn)
	if !expected.valid() {
		out.Status = "scope_conflict"
		return
	}
	conn.accountMu.RLock()
	quarantineFull := len(conn.retiredCurrencyProbes) >= 256
	conn.accountMu.RUnlock()
	if quarantineFull {
		out.Status = "skipped_quarantine_limit"
		return
	}
	if timeout <= 0 || timeout > 2*time.Second {
		timeout = 2 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	flight := conn.summarySchedule.beginProbe(cancel)
	if flight == nil {
		out.Status = "skipped_busy"
		return
	}
	defer func() {
		if conn.summarySchedule.finishProbe(flight) && out.Status != "session_changed" && out.Status != "scope_conflict" {
			out.Status = "yielded"
			out.Rows = nil
		}
	}()
	reqID, err := conn.nextRequestIDForForwarding()
	if err != nil {
		return
	}
	defer conn.discardRequestIDReservation(reqID)
	if err = conn.claimRequestID(reqID); err != nil {
		return
	}
	snap := &currencyProbeSnapshot{account: string(expected), epoch: origin.epoch, done: make(chan struct{}), requestedAt: time.Now().UTC(), rows: make(map[string]AccountStreamRow)}
	conn.accountMu.Lock()
	if conn.currencyProbes == nil {
		conn.currencyProbes = make(map[int]*currencyProbeSnapshot)
	}
	conn.currencyProbes[reqID] = snap
	conn.accountMu.Unlock()
	sentPossible := false
	out.SocketEpoch = origin.epoch
	defer func() {
		// Quarantine first, then cancel on exactly the original session, then let
		// the scheduling defer release ordinary readers. End is not cancellation.
		conn.accountMu.Lock()
		if conn.currencyProbes[reqID] == snap {
			delete(conn.currencyProbes, reqID)
		}
		if conn.BrokerSessionEpoch() == origin.epoch {
			if conn.retiredCurrencyProbes == nil {
				conn.retiredCurrencyProbes = make(map[int]uint64)
			}
			conn.retiredCurrencyProbes[reqID] = origin.epoch
		}
		conn.accountMu.Unlock()
		if !c.SessionCurrent(origin) {
			out.Status, out.CancelStatus = "session_changed", "session_changed"
			out.Rows = nil
			return
		}
		if !expected.equal(accountSummaryExpectedAccount(conn)) {
			out.Status = "scope_conflict"
			out.Rows = nil
		}
		if !sentPossible {
			out.CancelStatus = "not_needed"
			return
		}
		cancelCtx, cancelSend := context.WithTimeout(context.Background(), time.Second)
		defer cancelSend()
		err := conn.sendMessageWithTypeContextForEpochGuarded(cancelCtx, conn.encodeMsg(cancelAccountUpdatesMulti, 1, reqID), RequestTypeGeneral, origin.epoch, true, func() error {
			if !c.SessionCurrent(origin) {
				return ErrIBKRUnavailable
			}
			return nil
		})
		if !c.SessionCurrent(origin) {
			out.Status, out.CancelStatus = "session_changed", "session_changed"
			out.Rows = nil
			return
		}
		if err != nil {
			out.CancelStatus, out.Rows = "failed", nil
			conn.summarySchedule.mu.Lock()
			if conn.BrokerSessionEpoch() == origin.epoch {
				conn.summarySchedule.uncertainCurrencyProbe = true
			}
			conn.summarySchedule.mu.Unlock()
		} else {
			out.CancelStatus = "sent"
		}
		if !expected.equal(accountSummaryExpectedAccount(conn)) {
			out.Status = "scope_conflict"
			out.Rows = nil
		}
		out.ReadAt = time.Now().UTC()
	}()
	guard := func() error {
		if !c.SessionCurrent(origin) {
			return ErrIBKRUnavailable
		}
		if !expected.equal(accountSummaryExpectedAccount(conn)) {
			return ErrAccountSummaryScopeConflict
		}
		if conn.currencyProbeRawCaptureEnabled() {
			return errors.New("currency diagnostic raw capture enabled")
		}
		return nil
	}
	err = conn.sendMessageWithTypeContextForEpochGuarded(probeCtx, conn.encodeMsg(reqAccountUpdatesMulti, 1, reqID, string(expected), "", true), RequestTypeGeneral, origin.epoch, true, guard)
	sentPossible = err == nil || SendDispositionOf(err) != SendDispositionDefinitelyUnsent
	if err != nil {
		out.Status = "send_error"
		if errors.Is(err, context.DeadlineExceeded) {
			out.Status = "timeout"
		}
		if errors.Is(err, context.Canceled) {
			out.Status = "cancelled"
		}
		return
	}
	select {
	case <-snap.done:
		if err := guard(); err != nil {
			out.Status = "session_changed"
			if errors.Is(err, ErrAccountSummaryScopeConflict) {
				out.Status = "scope_conflict"
			}
			return
		}
		conn.accountMu.RLock()
		switch {
		case snap.conflict:
			out.Status = "scope_conflict"
		case snap.malformed:
			out.Status = "invalid_callback"
		case snap.brokerErrorCode != 0:
			out.Status, out.BrokerErrorCode = "broker_error", snap.brokerErrorCode
		default:
			out.Status = "completed"
			if snap.callbacks == 0 {
				out.Status = "completed_empty"
			}
			out.RequestedAt, out.CompletedAt, out.LastCallbackAt = snap.requestedAt, snap.completedAt, snap.lastCallbackAt
			out.Callbacks, out.RowsTruncated = snap.callbacks, snap.truncated
			out.Rows = make([]AccountStreamRow, 0, len(snap.rows))
			for _, row := range snap.rows {
				out.Rows = append(out.Rows, row)
			}
			slices.SortFunc(out.Rows, func(a, b AccountStreamRow) int {
				return strings.Compare(a.Key+"\x00"+a.Currency+"\x00"+a.Source, b.Key+"\x00"+b.Currency+"\x00"+b.Source)
			})
		}
		conn.accountMu.RUnlock()
	case <-probeCtx.Done():
		out.Status = "cancelled"
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			out.Status = "timeout"
		}
	}
	return
}

// The dispatcher holds an inbound epoch lease. Even unknown/retired Multi
// IDs are consumed without entering financial parsers or generic raw logs.
func (c *Connection) handleCurrencyProbeMessage(msgID int, fields []string, epoch uint64) {
	if len(fields) < 3 {
		return
	}
	reqID, err := strconv.Atoi(fields[2])
	if err != nil {
		return
	}
	c.accountMu.Lock()
	defer c.accountMu.Unlock()
	snap := c.currencyProbes[reqID]
	if snap == nil || snap.epoch != epoch || snap.terminal {
		return
	}
	terminal := func() { snap.terminal = true; close(snap.done) }
	if fields[1] != "1" {
		snap.malformed = true
		terminal()
		return
	}
	if msgID == msgAccountUpdateMultiEnd {
		if len(fields) != 4 || fields[3] != "" {
			snap.malformed = true
		} else {
			snap.completedAt = time.Now().UTC()
		}
		terminal()
		return
	}
	if len(fields) != 9 || fields[8] != "" {
		snap.malformed = true
		terminal()
		return
	}
	if !strings.EqualFold(strings.TrimSpace(fields[3]), snap.account) || fields[4] != "" {
		snap.conflict = true
		terminal()
		return
	}
	at := time.Now().UTC()
	snap.lastCallbackAt = at
	if snap.callbacks < 1_000_000 {
		snap.callbacks++
	}
	key, value, currency := fields[5], fields[6], fields[7]
	field, _ := splitGatewayLedgerTag(key)
	if !streamCashDiagnosticField(field) {
		return
	}
	source := "currency_only_multi"
	switch {
	case currency == "BASE":
		source = "base_currency_total"
	case currency == "":
		source = "unlabelled"
	case !concreteAccountSummaryLedgerCurrency(currency):
		currency, source = "", "invalid_currency"
	}
	status := streamCashValueStatus(field, value)
	id := key + "\x00" + currency + "\x00" + source
	row, exists := snap.rows[id]
	if !exists && len(snap.rows) >= 128 {
		snap.truncated = true
		return
	}
	if !exists {
		row = AccountStreamRow{Key: key, Currency: currency, Source: source, FirstReceivedAt: at}
	}
	row.ValueStatus, row.LastReceivedAt = status, at
	if row.Callbacks < 1_000_000 {
		row.Callbacks++
	}
	snap.rows[id] = row
}
func (c *Connection) consumeCurrencyProbeError(reqText string, code int, epoch uint64) bool {
	reqID, err := strconv.Atoi(reqText)
	if err != nil {
		return false
	}
	c.accountMu.Lock()
	defer c.accountMu.Unlock()
	if retired, ok := c.retiredCurrencyProbes[reqID]; ok && retired == epoch {
		return true
	}
	snap := c.currencyProbes[reqID]
	if snap == nil || snap.epoch != epoch {
		return false
	}
	if !snap.terminal && code > 0 && code < 2000 {
		snap.brokerErrorCode, snap.terminal = code, true
		close(snap.done)
	}
	return true
}
