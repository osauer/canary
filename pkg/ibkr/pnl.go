package ibkr

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

// AccountDailyPnL is the most recent account-level frame from an IBKR reqPnL
// subscription. Monetary values are expressed in the account's base currency,
// distinguish an observed zero from a missing, unavailable, or IBKR sentinel
type AccountDailyPnL struct {
	DailyPnL           *float64
	UnrealizedTotalPnL *float64
	RealizedTotalPnL   *float64
	AsOf               time.Time
	DailyPnLStatus     DailyPnLFrameStatus
}

// DailyPnLFrameStatus distinguishes a usable Daily P&L value from a gateway
// placeholder and from malformed wire data. Callers must not infer those
// states from a nil value alone.
type DailyPnLFrameStatus string

// Daily P&L frame statuses reported by the connector.
const (
	DailyPnLFrameAvailable   DailyPnLFrameStatus = "available"
	DailyPnLFrameUnavailable DailyPnLFrameStatus = "unavailable"
	DailyPnLFrameMalformed   DailyPnLFrameStatus = "malformed"
)

// PositionDailyPnL is the most recent per-contract frame from an IBKR
// reqPnLSingle subscription. Monetary values are expressed in the account's
// base currency, and AsOf is the UTC receive time. Pointer fields use the same
// missing-versus-zero semantics as [AccountDailyPnL]. UnrealizedTotalPnL and
// RealizedTotalPnL are lifetime totals, not components of DailyPnL.
type PositionDailyPnL struct {
	DailyPnL           *float64
	UnrealizedTotalPnL *float64
	RealizedTotalPnL   *float64
	AsOf               time.Time
}

// pnlCache holds account and per-position subscription identities and the
// immutable snapshots published by their handlers.
type pnlCache struct {
	mu      sync.RWMutex
	session ConnectorSessionBinding

	accountReqID int // 0 means no active subscription
	accountAcct  string
	// accountStartedAt distinguishes a healthy subscription still awaiting
	accountStartedAt time.Time
	account          AccountDailyPnL

	// positionReqIDs maps conId -> reqID. positionByReqID is the
	// reverse map so the inbound handler (which sees reqID, not
	// conId) can find the cache entry to update.
	positionReqIDs   map[int]int
	positionByReqID  map[int]int
	positionSnapshot map[int]PositionDailyPnL
}

func newPnLCache() *pnlCache {
	return &pnlCache{
		positionReqIDs:   make(map[int]int),
		positionByReqID:  make(map[int]int),
		positionSnapshot: make(map[int]PositionDailyPnL),
	}
}

// dblMaxNotSent is IBKR's "not yet computed" sentinel for double
// fields. The wire emits the exact DBL_MAX value (1.7976931348623157e+308);
const dblMaxNotSent = 1e300

// parsePnLFloat converts a wire field to a pointer-or-nil. Empty
// We don't surface parse errors — IBKR's gateway is the source of truth;
// a malformed field is something to be silent about, not a hard fail.
func parsePnLFloat(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	if math.IsNaN(v) || math.Abs(v) >= dblMaxNotSent {
		return nil
	}
	return &v
}

func parseDailyPnLFloat(s string) (*float64, DailyPnLFrameStatus) {
	if s == "" {
		return nil, DailyPnLFrameUnavailable
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, DailyPnLFrameMalformed
	}
	if math.Abs(v) >= dblMaxNotSent {
		return nil, DailyPnLFrameUnavailable
	}
	return &v, DailyPnLFrameAvailable
}

// RequestPnL starts a reqPnL stream for account using reqID. modelCode is empty
// for accounts without a Financial Advisor model. The caller owns reqID,
// response handling, and cancellation; [Connector.SubscribeAccountPnL]
func (c *Connection) RequestPnL(reqID int, account, modelCode string) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	if account == "" {
		return fmt.Errorf("account is required for reqPnL")
	}
	if err := ensureASCII("account", account); err != nil {
		return err
	}
	if err := ensureASCII("modelCode", modelCode); err != nil {
		return err
	}
	if err := c.claimRequestID(reqID); err != nil {
		return err
	}
	msg := c.encodeMsg(reqPnL, reqID, account, modelCode)
	return c.sendMessage(msg)
}

// CancelPnL requests cancellation of the reqPnL stream identified by reqID.
func (c *Connection) CancelPnL(reqID int) error {
	if reqID <= 0 || reqID > maxProtoInt32 {
		return fmt.Errorf("reqPnL request ID must be a positive signed 32-bit integer")
	}
	if !c.IsConnected() {
		return nil
	}
	msg := c.encodeMsg(cancelPnL, reqID)
	return c.sendMessage(msg)
}

// RequestPnLSingle starts a reqPnLSingle stream for conID on account using
// reqID. modelCode is empty for accounts without a Financial Advisor model.
func (c *Connection) RequestPnLSingle(reqID int, account, modelCode string, conID int) error {
	return c.requestPnLSingleContext(context.Background(), reqID, account, modelCode, conID)
}
func (c *Connection) requestPnLSingleContext(ctx context.Context, reqID int, account, modelCode string, conID int) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	if account == "" {
		return fmt.Errorf("account is required for reqPnLSingle")
	}
	if conID <= 0 {
		return fmt.Errorf("conId is required for reqPnLSingle")
	}
	if err := ensureASCII("account", account); err != nil {
		return err
	}
	if err := ensureASCII("modelCode", modelCode); err != nil {
		return err
	}
	if err := c.claimRequestID(reqID); err != nil {
		return err
	}
	msg := c.encodeMsg(reqPnLSingle, reqID, account, modelCode, conID)
	return c.sendMessageWithTypeContext(ctx, msg, RequestTypeGeneral)
}

// CancelPnLSingle requests cancellation of the reqPnLSingle stream identified
func (c *Connection) CancelPnLSingle(reqID int) error {
	if reqID <= 0 || reqID > maxProtoInt32 {
		return fmt.Errorf("reqPnLSingle request ID must be a positive signed 32-bit integer")
	}
	if !c.IsConnected() {
		return nil
	}
	msg := c.encodeMsg(cancelPnLSingle, reqID)
	return c.sendMessage(msg)
}

// parseAccountPnLFields parses an inbound msgPnL frame. The wire layout
// after the leading msgID is:
//
//	[reqId] [dailyPnL] [unrealizedPnL] [realizedPnL]
//
// unrealizedPnL / realizedPnL (fields 3 & 4) are the account's TOTAL
// unrealized / realized P&L (inception to now), not a decomposition of
// dailyPnL — see AccountDailyPnL. Older gateways (server < 150) emit only
// the first two fields after reqId; we accept the short form and leave the
// remaining pointers nil. Returns reqID and a populated snapshot; ok=false
// on a malformed frame.
func parseAccountPnLFields(fields []string) (reqID int, snap AccountDailyPnL, ok bool) {
	// fields[0] = msgID; fields[1] = reqId; fields[2+] = payload.
	if len(fields) < 3 {
		return 0, AccountDailyPnL{}, false
	}
	rid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, AccountDailyPnL{}, false
	}
	snap.DailyPnL, snap.DailyPnLStatus = parseDailyPnLFloat(fields[2])
	if len(fields) > 3 {
		snap.UnrealizedTotalPnL = parsePnLFloat(fields[3])
	}
	if len(fields) > 4 {
		snap.RealizedTotalPnL = parsePnLFloat(fields[4])
	}
	snap.AsOf = time.Now().UTC()
	return rid, snap, true
}

// parsePositionPnLFields parses an inbound msgPnLSingle frame. Wire
// position size (we already have it from reqAccountUpdates), and value
func parsePositionPnLFields(fields []string) (reqID int, snap PositionDailyPnL, ok bool) {
	// fields[0] = msgID; fields[1] = reqId; fields[2] = pos;
	if len(fields) < 4 {
		return 0, PositionDailyPnL{}, false
	}
	rid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, PositionDailyPnL{}, false
	}
	snap.DailyPnL = parsePnLFloat(fields[3])
	if len(fields) > 4 {
		snap.UnrealizedTotalPnL = parsePnLFloat(fields[4])
	}
	if len(fields) > 5 {
		snap.RealizedTotalPnL = parsePnLFloat(fields[5])
	}
	snap.AsOf = time.Now().UTC()
	return rid, snap, true
}

// SubscribeAccountPnL starts and caches a streaming reqPnL subscription for
// account. account must be non-empty. Repeated calls for the same account are
// idempotent; changing the account cancels the previous stream, clears its
// stream. Use [Connector.AccountDailyPnL] for non-blocking cache reads. Callers
// must serialize attempts to switch one connector between different accounts.
func (c *Connector) SubscribeAccountPnL(account string) error {
	origin, ok := c.CaptureSession()
	if !ok {
		return ErrIBKRUnavailable
	}
	return c.subscribeAccountPnLForSession(origin, account)
}

func (c *Connector) subscribeAccountPnLForSession(origin ConnectorSessionBinding, account string) error {
	if account == "" {
		return fmt.Errorf("account is required")
	}
	if err := ensureASCII("account", account); err != nil {
		return err
	}
	conn := origin.connection
	if !c.SessionCurrent(origin) {
		return ErrIBKRUnavailable
	}

	c.pnl.mu.Lock()
	if c.pnl.session == origin && c.pnl.accountReqID != 0 && c.pnl.accountAcct == account {
		c.pnl.mu.Unlock()
		return nil
	}
	// Account changed (rare): tear down the old subscription before
	// claiming the new reqID slot.
	oldReqID := 0
	if c.pnl.session == origin {
		oldReqID = c.pnl.accountReqID
	}
	c.pnl.mu.Unlock()
	if oldReqID != 0 {
		if err := c.sendPnLForSession(context.Background(), origin, conn.encodeMsg(cancelPnL, oldReqID), nil); err != nil {
			connectorLogger.Debugf("CancelPnL(reqID=%d) failed during account change: %v", oldReqID, err)
		}
	}

	reqID, _, err := conn.reserveNextRequestIDForEpoch(origin.epoch)
	if err != nil {
		return err
	}
	defer discardPnLReservation(origin, reqID)
	adopted := false
	if !c.mutatePnLForSession(origin, func() {
		c.resetPnLSessionLocked(origin)
		if c.pnl.accountReqID != 0 && c.pnl.accountAcct == account {
			return
		}
		c.pnl.accountReqID, c.pnl.accountAcct = reqID, account
		c.pnl.accountStartedAt, c.pnl.account = c.pnlResubClock().UTC(), AccountDailyPnL{}
		adopted = true
	}) {
		return ErrIBKRUnavailable
	}
	if !adopted {
		return nil
	}
	if _, err = conn.claimRequestIDForEpoch(reqID, origin.epoch); err == nil {
		err = c.sendPnLForSession(context.Background(), origin, conn.encodeMsg(reqPnL, reqID, account, ""), func() bool { return c.pnl.session == origin && c.pnl.accountReqID == reqID })
	}
	if err != nil {
		c.pnl.mu.Lock()
		if c.pnl.session == origin && c.pnl.accountReqID == reqID {
			c.pnl.accountReqID = 0
			c.pnl.accountAcct = ""
			c.pnl.accountStartedAt = time.Time{}
			c.pnl.account = AccountDailyPnL{}
		}
		c.pnl.mu.Unlock()
		return fmt.Errorf("request PnL: %w", err)
	}
	return nil
}

// AccountDailyPnL returns the most recently received account Daily P&L
// snapshot. ok is false until [Connector.SubscribeAccountPnL] is active and a
// frame has been received. The method neither blocks nor issues wire traffic.
func (c *Connector) AccountDailyPnL() (AccountDailyPnL, bool) {
	c.pnl.mu.RLock()
	defer c.pnl.mu.RUnlock()
	if c.pnl.accountReqID == 0 || c.pnl.account.AsOf.IsZero() || !c.SessionCurrent(c.pnl.session) {
		return AccountDailyPnL{}, false
	}
	// Defensive copy; the snapshot holds pointers but they aren't
	return c.pnl.account, true
}

// SubscribePositionDailyPnL starts and caches a reqPnLSingle stream for conID
// on account. account must be non-empty and conID must be positive. Streams are
// returns [ErrIBKRUnavailable] when the connector is disconnected. One
// connector must not reuse the same conID for different accounts.
func (c *Connector) SubscribePositionDailyPnL(account string, conID int) error {
	return c.SubscribePositionDailyPnLContext(context.Background(), account, conID)
}

// SubscribePositionDailyPnLContext starts the shared position P&L subscription
// with caller-owned cancellation. The connector retains at most 50 contracts;
// an existing contract is idempotent and remains daemon-owned after this returns.
func (c *Connector) SubscribePositionDailyPnLContext(ctx context.Context, account string, conID int) error {
	origin, ok := c.CaptureSession()
	if !ok {
		return ErrIBKRUnavailable
	}
	return c.subscribePositionPnLForSession(ctx, origin, account, conID)
}

func (c *Connector) subscribePositionPnLForSession(ctx context.Context, origin ConnectorSessionBinding, account string, conID int) error {
	if account == "" || conID <= 0 {
		return fmt.Errorf("account and positive conId are required")
	}
	if err := ensureASCII("account", account); err != nil {
		return err
	}
	if !c.SessionCurrent(origin) {
		return ErrIBKRUnavailable
	}
	conn := origin.connection

	c.pnl.mu.Lock()
	if _, ok := c.pnl.positionReqIDs[conID]; ok && c.pnl.session == origin {
		c.pnl.mu.Unlock()
		return nil
	}
	c.pnl.mu.Unlock()

	reqID, _, err := conn.reserveNextRequestIDForEpoch(origin.epoch)
	if err != nil {
		return err
	}
	defer discardPnLReservation(origin, reqID)
	adopted := false
	if !c.mutatePnLForSession(origin, func() {
		c.resetPnLSessionLocked(origin)
		if _, ok := c.pnl.positionReqIDs[conID]; ok {
			return
		}
		if len(c.pnl.positionReqIDs) >= 50 {
			err = fmt.Errorf("position PnL subscription limit reached")
			return
		}
		c.pnl.positionReqIDs[conID], c.pnl.positionByReqID[reqID] = reqID, conID
		c.pnl.positionSnapshot[conID] = PositionDailyPnL{}
		adopted = true
	}) {
		return ErrIBKRUnavailable
	}
	if err != nil || !adopted {
		return err
	}
	if _, err = conn.claimRequestIDForEpoch(reqID, origin.epoch); err == nil {
		err = c.sendPnLForSession(ctx, origin, conn.encodeMsg(reqPnLSingle, reqID, account, "", conID), func() bool { return c.pnl.session == origin && c.pnl.positionReqIDs[conID] == reqID })
	}
	if err != nil {
		c.pnl.mu.Lock()
		if current, ok := c.pnl.positionReqIDs[conID]; ok && c.pnl.session == origin && current == reqID {
			delete(c.pnl.positionReqIDs, conID)
			delete(c.pnl.positionByReqID, reqID)
			delete(c.pnl.positionSnapshot, conID)
		}
		c.pnl.mu.Unlock()
		return fmt.Errorf("request PnL single: %w", err)
	}
	return nil
}

// PositionDailyPnL returns the cached per-contract Daily P&L snapshot for
// usable values. The method neither blocks nor issues wire traffic. Handlers
func (c *Connector) PositionDailyPnL(conID int) (PositionDailyPnL, bool) {
	c.pnl.mu.RLock()
	defer c.pnl.mu.RUnlock()
	snap, ok := c.pnl.positionSnapshot[conID]
	return snap, ok && c.SessionCurrent(c.pnl.session)
}

// ActiveDailyPnLSubscriptions reports the number of tracked per-contract
// reqPnLSingle streams. It does not include the account-level reqPnL stream.
func (c *Connector) ActiveDailyPnLSubscriptions() int {
	c.pnl.mu.RLock()
	defer c.pnl.mu.RUnlock()
	return len(c.pnl.positionReqIDs)
}

// AccountID returns the account code most recently received from IBKR's
// managedAccounts frame. It returns an empty string before that frame is
// observed or when the connector has no connection.
func (c *Connector) AccountID() string {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return ""
	}
	return conn.GetAccountCode()
}

// cancelAllPnL is called from Connector.Stop to tear down every
// outstanding PnL subscription. Best-effort: the gateway drops
// subscriptions on socket close anyway, so cancel-time errors are
// logged at debug and otherwise ignored.
func (c *Connector) cancelAllPnL() {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return
	}
	c.pnl.mu.Lock()
	acctReq := c.pnl.accountReqID
	posReqs := make([]int, 0, len(c.pnl.positionReqIDs))
	for _, r := range c.pnl.positionReqIDs {
		posReqs = append(posReqs, r)
	}
	c.pnl.accountReqID = 0
	c.pnl.accountAcct = ""
	c.pnl.accountStartedAt = time.Time{}
	c.pnl.positionReqIDs = make(map[int]int)
	c.pnl.positionByReqID = make(map[int]int)
	c.pnl.positionSnapshot = make(map[int]PositionDailyPnL)
	c.pnl.mu.Unlock()

	if acctReq != 0 {
		if err := conn.CancelPnL(acctReq); err != nil {
			connectorLogger.Debugf("CancelPnL(reqID=%d) on shutdown: %v", acctReq, err)
		}
	}
	for _, r := range posReqs {
		if err := conn.CancelPnLSingle(r); err != nil {
			connectorLogger.Debugf("CancelPnLSingle(reqID=%d) on shutdown: %v", r, err)
		}
	}
}

// dailyPnLStaleResubscribe bounds how long the account daily-P&L stream may go
const dailyPnLStaleResubscribe = 90 * time.Second

func (c *Connector) pnlResubClock() time.Time {
	if c.pnlResubNow != nil {
		return c.pnlResubNow()
	}
	return time.Now()
}

// MaybeResubscribeStaleDailyPnL rebuilds all Daily P&L streams when marketOpen
// is true and the account stream has not produced its first frame or its last
func (c *Connector) MaybeResubscribeStaleDailyPnL(marketOpen bool) bool {
	if !marketOpen || !c.isConnected() || c.BackendLink().Down {
		return false
	}
	c.pnl.mu.RLock()
	origin := c.pnl.session
	reqID := c.pnl.accountReqID
	asOf := c.pnl.account.AsOf
	startedAt := c.pnl.accountStartedAt
	c.pnl.mu.RUnlock()
	if reqID == 0 {
		// The startup/lazy-kick path in SubscribeAccountPnL owns a stream that
		// has never been requested.
		return false
	}
	now := c.pnlResubClock()
	referenceAt := asOf
	if referenceAt.IsZero() {
		referenceAt = startedAt
	}
	if referenceAt.IsZero() || now.Sub(referenceAt) < dailyPnLStaleResubscribe {
		return false
	}
	c.pnlResubMu.Lock()
	if !c.pnlResubLastAt.IsZero() && now.Sub(c.pnlResubLastAt) < dailyPnLStaleResubscribe {
		c.pnlResubMu.Unlock()
		return false
	}
	c.pnlResubLastAt = now
	c.pnlResubMu.Unlock()

	warn, attempts, age := c.pnlSilenceLog.Observe(now)
	if warn {
		connectorLogger.Warnf("account daily P&L stream silent during market hours; rebuilding reqPnL subscriptions (attempts=%d, incident_duration=%s, last_frame_age=%s)", attempts, age.Round(time.Second), now.Sub(referenceAt).Round(time.Second))
	}
	return c.rebuildPnLForSession(origin, reqID)
}

// forceResubscribeDailyPnL tears down and re-issues the account and per-position
// daily-P&L subscriptions, preserving the same account/conId set. Unlike the
// idempotent SubscribeAccountPnL / SubscribePositionDailyPnL, it revives a
// the idempotency guards re-arm, then the old wire subscriptions are cancelled
// by handlePnL (reqID mismatch), so no stale frame races the rebuild.
func (c *Connector) forceResubscribeDailyPnL() {
	origin, ok := c.CaptureSession()
	if ok {
		c.forceResubscribeDailyPnLForSession(origin)
	}
}

func (c *Connector) forceResubscribeDailyPnLForSession(origin ConnectorSessionBinding) {
	c.rebuildPnLForSession(origin, 0)
}

func (c *Connector) rebuildPnLForSession(origin ConnectorSessionBinding, expectedAccountRequest int) bool {
	c.pnlRepairMu.Lock()
	defer c.pnlRepairMu.Unlock()
	defer c.display.notify()
	conn := origin.connection
	var acct string
	var acctReq int
	var posConIDs, posReqs []int
	if !c.mutatePnLForSession(origin, func() {
		if c.pnl.session != origin || (expectedAccountRequest != 0 && c.pnl.accountReqID != expectedAccountRequest) {
			return
		}

		acct = c.pnl.accountAcct
		acctReq = c.pnl.accountReqID
		posConIDs = make([]int, 0, len(c.pnl.positionReqIDs))
		posReqs = make([]int, 0, len(c.pnl.positionReqIDs))
		for conID, r := range c.pnl.positionReqIDs {
			posConIDs = append(posConIDs, conID)
			posReqs = append(posReqs, r)
		}
		c.pnl.accountReqID = 0
		c.pnl.accountAcct = ""
		c.pnl.accountStartedAt = time.Time{}
		c.pnl.account = AccountDailyPnL{}
		c.pnl.positionReqIDs = make(map[int]int)
		c.pnl.positionByReqID = make(map[int]int)
		c.pnl.positionSnapshot = make(map[int]PositionDailyPnL)
	}) {
		return false
	}

	if acctReq != 0 {
		if err := c.sendPnLForSession(context.Background(), origin, conn.encodeMsg(cancelPnL, acctReq), nil); err != nil {
			connectorLogger.Debugf("CancelPnL(reqID=%d) during resubscribe: %v", acctReq, err)
		}
	}
	for _, r := range posReqs {
		if err := c.sendPnLForSession(context.Background(), origin, conn.encodeMsg(cancelPnLSingle, r), nil); err != nil {
			connectorLogger.Debugf("CancelPnLSingle(reqID=%d) during resubscribe: %v", r, err)
		}
	}

	if acct == "" {
		return false
	}
	accountSent := c.subscribeAccountPnLForSession(origin, acct) == nil
	positionFailures := 0
	for _, conID := range posConIDs {
		if err := c.subscribePositionPnLForSession(context.Background(), origin, acct, conID); err != nil {
			positionFailures++
		}
	}
	if c.SessionCurrent(origin) && (!accountSent || positionFailures > 0) {
		connectorLogger.Warnf("P&L rebuild requests incomplete: account_request_sent=%t position_request_failures=%d; account upkeep and position demand can retry", accountSent, positionFailures)
	}
	return true
}

// handlePnL is the connector-side msgPnL handler. Decodes the frame
// and updates the cache so AccountDailyPnL reads see fresh values.
func (c *Connector) handlePnL(fields []string) {
	reqID, snap, ok := parseAccountPnLFields(fields)
	if !ok {
		return
	}
	c.pnl.mu.Lock()
	if c.pnl.accountReqID == 0 || c.pnl.accountReqID != reqID {
		// Stale frame from a previous subscription (or we never
		// reconnect can race with the last few frames of the prior
		c.pnl.mu.Unlock()
		return
	}
	c.pnl.account = snap
	c.pnl.mu.Unlock()
	if attempts, age := c.pnlSilenceLog.Recover(c.pnlResubClock()); attempts > 0 {
		connectorLogger.Warnf("account daily P&L stream resumed after %s (%d rebuild attempts); frame quality remains in account health", age.Round(time.Second), attempts)
	}
}

// handlePnLSingle is the connector-side msgPnLSingle handler.
func (c *Connector) handlePnLSingle(fields []string) {
	reqID, snap, ok := parsePositionPnLFields(fields)
	if !ok {
		return
	}
	c.pnl.mu.Lock()
	conID, known := c.pnl.positionByReqID[reqID]
	if !known {
		c.pnl.mu.Unlock()
		return
	}
	c.pnl.positionSnapshot[conID] = snap
	c.pnl.mu.Unlock()
}
