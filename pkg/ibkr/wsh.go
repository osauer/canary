package ibkr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	minServerVerWSHCalendar         = 161
	minServerVerWSHEventFilters     = 171
	minServerVerWSHEventFilterDates = 173
	wshRequestLimit                 = 10
	wshMetadataReadyTTL             = 24 * time.Hour
)

// WSHErrorKind is a stable, sanitized classification for a failed Wall Street
// branch on Kind and Code without persisting an untrusted broker message.
type WSHErrorKind string

const (
	// WSHErrorCanceled means the caller canceled the queued or active request.
	WSHErrorCanceled WSHErrorKind = "canceled"
	// WSHErrorTimeout means the caller's deadline expired.
	WSHErrorTimeout WSHErrorKind = "timeout"
	// WSHErrorTransport means no usable broker transport was available.
	WSHErrorTransport WSHErrorKind = "transport_failure"
	// WSHErrorUnsupportedProtocol means TWS or Gateway is too old for the request.
	WSHErrorUnsupportedProtocol WSHErrorKind = "unsupported_protocol"
	// WSHErrorConnectorInactive means the current connector temporarily marked
	// the symbol inactive after repeated definition failures. The mark expires
	// after a bounded interval and is cleared on reconnect, so callers must not
	// persist it as a provider verdict about the security.
	WSHErrorConnectorInactive WSHErrorKind = "connector_inactive"
	// WSHErrorUnsupportedSecurity means WSH cannot be queried for the instrument.
	WSHErrorUnsupportedSecurity WSHErrorKind = "unsupported_security"
	// WSHErrorContractResolution means the stock conId could not be resolved.
	WSHErrorContractResolution WSHErrorKind = "contract_resolution_failure"
	// WSHErrorEntitlementRequired means the account lacks the WSH subscription.
	WSHErrorEntitlementRequired WSHErrorKind = "entitlement_required"
	// WSHErrorDuplicateRequest means TWS rejected a concurrent WSH request.
	WSHErrorDuplicateRequest WSHErrorKind = "duplicate_request"
	// WSHErrorMetadataRequired means event data was requested without current metadata.
	WSHErrorMetadataRequired WSHErrorKind = "metadata_required"
	// WSHErrorProviderFailure means WSH rejected or failed the request.
	WSHErrorProviderFailure WSHErrorKind = "provider_failure"
	// WSHErrorMalformedResponse means WSH returned empty or invalid JSON.
	WSHErrorMalformedResponse WSHErrorKind = "malformed_response"
	// WSHErrorEventTypeUnavailable means metadata did not advertise earnings dates.
	WSHErrorEventTypeUnavailable WSHErrorKind = "event_type_unavailable"
)

// WSHError describes a failed read-only Wall Street Horizon request. Operation
// and Code are allowlisted protocol facts; broker and transport prose is not
// retained. Context cancellation is the only wrapped cause.
type WSHError struct {
	Kind      WSHErrorKind
	Operation string
	Code      int
	cause     error
}

// Error returns a sanitized classification without broker response prose.
func (e *WSHError) Error() string {
	if e == nil {
		return "ibkr WSH request failed"
	}
	if e.Code != 0 {
		return fmt.Sprintf("ibkr WSH %s: %s (code %d)", e.Operation, e.Kind, e.Code)
	}
	return fmt.Sprintf("ibkr WSH %s: %s", e.Operation, e.Kind)
}

// Unwrap exposes only a caller context cancellation or deadline cause.
func (e *WSHError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// WSHEarningsResult pairs WSH event JSON with an optional exact broker stock
// row matched the requested ConID, stock security type, and symbol.
type WSHEarningsResult struct {
	EventJSON     string
	StockIdentity *ContractDetailsLite
}

// FetchWSHEarnings returns the raw WSH earnings-event JSON for a stock symbol.
// It preserves the legacy symbol-resolution and temporary-inactive-cache
// positive held contract ID and needs fresh broker identity evidence.
func (c *Connector) FetchWSHEarnings(ctx context.Context, symbol string) (string, error) {
	result, err := c.fetchWSHEarnings(ctx, symbol, 0, false)
	return result.EventJSON, err
}

// FetchWSHEarningsWithIdentity reads WSH earnings events using a caller-proven
// positive held contract ID and independently attempts an exact contract-
// suppress this exact lookup. If exact details are unavailable or contradict
// callers must not infer issuer classification from the supplied ID or cache.
func (c *Connector) FetchWSHEarningsWithIdentity(ctx context.Context, symbol string, conID int) (WSHEarningsResult, error) {
	if conID <= 0 {
		return WSHEarningsResult{}, &WSHError{Kind: WSHErrorUnsupportedSecurity, Operation: "resolve_contract"}
	}
	return c.fetchWSHEarnings(ctx, symbol, conID, true)
}

// ResolveWSHStockIdentity independently reads exact broker contract details
// event request. All failures are returned as sanitized WSH errors.
func (c *Connector) ResolveWSHStockIdentity(ctx context.Context, symbol string, conID int) (*ContractDetailsLite, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.resolveWSHStockIdentity(ctx, symbol, conID, boundedWSHResolveTimeout(ctx, 5*time.Second))
}

// fetchWSHEarnings performs no broker write: it establishes daily WSH metadata
// readiness for the current broker session, then issues a serialized event-
// calendar read filtered to the broker-resolved event tag. IBKR permits only
// one WSH request of each kind for a client; the gate covers the complete
// contract-details -> metadata -> event sequence.
func (c *Connector) fetchWSHEarnings(ctx context.Context, symbol string, exactConID int, exactIdentity bool) (WSHEarningsResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		return WSHEarningsResult{}, &WSHError{Kind: WSHErrorTransport, Operation: "acquire"}
	}
	if err := ctx.Err(); err != nil {
		return WSHEarningsResult{}, newWSHContextError("acquire", err)
	}

	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return WSHEarningsResult{}, &WSHError{Kind: WSHErrorUnsupportedSecurity, Operation: "resolve_contract"}
	}
	secType, _, _, _ := classifySymbol(symbol)
	if !strings.EqualFold(secType, "STK") {
		return WSHEarningsResult{}, &WSHError{Kind: WSHErrorUnsupportedSecurity, Operation: "resolve_contract"}
	}
	// A positive contract cache entry can outlive a later broker-confirmed
	// contract resolution or the event-calendar wire path.
	if !exactIdentity && c.IsSymbolInactive(symbol) {
		return WSHEarningsResult{}, &WSHError{Kind: WSHErrorConnectorInactive, Operation: "resolve_contract"}
	}

	release, err := c.acquireWSH(ctx)
	if err != nil {
		return WSHEarningsResult{}, err
	}
	defer release()

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return WSHEarningsResult{}, &WSHError{Kind: WSHErrorTransport, Operation: "metadata"}
	}
	serverVersion := conn.ServerVersion()
	if serverVersion < minServerVerWSHEventFilters || serverVersion > maxClientVersion {
		return WSHEarningsResult{}, &WSHError{Kind: WSHErrorUnsupportedProtocol, Operation: "event_data"}
	}

	resolveTimeout := boundedWSHResolveTimeout(ctx, 5*time.Second)
	result := WSHEarningsResult{}
	eventConID := exactConID
	if exactIdentity {
		detail, resolveErr := c.resolveWSHStockIdentity(ctx, symbol, exactConID, resolveTimeout)
		if errors.Is(resolveErr, context.Canceled) || errors.Is(resolveErr, context.DeadlineExceeded) {
			return WSHEarningsResult{}, resolveErr
		}
		if resolveErr == nil {
			copy := *detail
			result.StockIdentity = &copy
		}
	} else {
		resolver := c.resolveWSHContract
		if resolver == nil {
			resolver = c.resolveWSHStockContract
		}
		detail, resolveErr := resolver(ctx, symbol, resolveTimeout)
		if resolveErr != nil {
			if errors.Is(resolveErr, context.Canceled) || errors.Is(resolveErr, context.DeadlineExceeded) {
				return WSHEarningsResult{}, newWSHContextError("resolve_contract", resolveErr)
			}
			if errors.Is(resolveErr, ErrSymbolInactive) || c.IsSymbolInactive(symbol) {
				return WSHEarningsResult{}, &WSHError{Kind: WSHErrorConnectorInactive, Operation: "resolve_contract"}
			}
			return WSHEarningsResult{}, &WSHError{Kind: WSHErrorContractResolution, Operation: "resolve_contract"}
		}
		if detail == nil || detail.ConID <= 0 || (detail.SecType != "" && !strings.EqualFold(detail.SecType, "STK")) {
			return WSHEarningsResult{}, &WSHError{Kind: WSHErrorUnsupportedSecurity, Operation: "resolve_contract"}
		}
		if c.IsSymbolInactive(symbol) {
			return WSHEarningsResult{}, &WSHError{Kind: WSHErrorConnectorInactive, Operation: "resolve_contract"}
		}
		eventConID = detail.ConID
	}

	now := time.Now().UTC()
	if c.wshNow != nil {
		now = c.wshNow().UTC()
	}
	eventTag, metadataReady := c.wshMetadataEarningsTag(conn, now)
	if !metadataReady {
		var err error
		eventTag, err = c.refreshWSHMetadata(ctx, conn, now)
		if err != nil {
			return result, err
		}
	}

	for attempt := range 2 {
		request := wshEventRequest{conID: eventConID, limit: wshRequestLimit, eventTag: eventTag}
		events, err := fetchWSHJSON(ctx, conn, wshPhase{
			operation:  "event_data",
			responseID: msgWSHEventData,
			send: func(ctx context.Context, reqID int) error {
				return conn.sendWSHEventDataRequest(ctx, reqID, request)
			},
			cancel: conn.cancelWSHEventDataRequest,
		})
		if err == nil {
			if !json.Valid([]byte(events)) {
				return result, &WSHError{Kind: WSHErrorMalformedResponse, Operation: "event_data"}
			}
			result.EventJSON = events
			return result, nil
		}
		var typed *WSHError
		if !errors.As(err, &typed) || typed.Kind != WSHErrorMetadataRequired {
			return result, err
		}
		// TWS can invalidate its metadata cache without dropping the socket.
		c.resetWSHMetadataReadiness()
		if attempt == 1 {
			return result, err
		}
		eventTag, err = c.refreshWSHMetadata(ctx, conn, now)
		if err != nil {
			return result, err
		}
	}
	return result, &WSHError{Kind: WSHErrorProviderFailure, Operation: "event_data"}
}

func (c *Connector) refreshWSHMetadata(ctx context.Context, conn *Connection, now time.Time) (string, error) {
	metadata, err := fetchWSHJSON(ctx, conn, wshPhase{
		operation:  "metadata",
		responseID: msgWSHMetaData,
		send:       conn.sendWSHMetaDataRequest,
		cancel:     conn.cancelWSHMetaDataRequest,
	})
	if err != nil {
		return "", err
	}
	if !json.Valid([]byte(metadata)) {
		return "", &WSHError{Kind: WSHErrorMalformedResponse, Operation: "metadata"}
	}
	eventTag, ok := wshMetadataEarningsEventTag(metadata)
	if !ok {
		return "", &WSHError{Kind: WSHErrorEventTypeUnavailable, Operation: "metadata"}
	}
	c.markWSHMetadataReady(conn, now, eventTag)
	return eventTag, nil
}

func (c *Connector) acquireWSH(ctx context.Context) (func(), error) {
	c.wshGateMu.Lock()
	if c.wshGate == nil {
		c.wshGate = make(chan struct{}, 1)
		c.wshGate <- struct{}{}
	}
	gate := c.wshGate
	c.wshGateMu.Unlock()

	select {
	case <-ctx.Done():
		return nil, newWSHContextError("acquire", ctx.Err())
	case <-gate:
		return func() { gate <- struct{}{} }, nil
	}
}

func (c *Connector) resetWSHMetadataReadiness() {
	if c == nil {
		return
	}
	c.wshGateMu.Lock()
	c.wshMetadataConn = nil
	c.wshMetadataReadyAt = time.Time{}
	c.wshEarningsEventTag = ""
	c.wshGateMu.Unlock()
}

func (c *Connector) wshMetadataEarningsTag(conn *Connection, now time.Time) (string, bool) {
	c.wshGateMu.Lock()
	defer c.wshGateMu.Unlock()
	if conn == nil || c.wshMetadataConn != conn || c.wshMetadataReadyAt.IsZero() || !validWSHEarningsEventTag(c.wshEarningsEventTag) {
		return "", false
	}
	age := now.Sub(c.wshMetadataReadyAt)
	return c.wshEarningsEventTag, age >= 0 && age < wshMetadataReadyTTL
}

func (c *Connector) markWSHMetadataReady(conn *Connection, now time.Time, eventTags ...string) {
	eventTag := "wsh_ed"
	if len(eventTags) > 0 {
		eventTag = eventTags[0]
	}
	if !validWSHEarningsEventTag(eventTag) {
		return
	}
	c.wshGateMu.Lock()
	c.wshMetadataConn = conn
	c.wshMetadataReadyAt = now
	c.wshEarningsEventTag = eventTag
	c.wshGateMu.Unlock()
}

func (c *Connector) resolveWSHStockContract(ctx context.Context, symbol string, timeout time.Duration) (*ContractDetailsLite, error) {
	if c.IsSymbolInactive(symbol) {
		return nil, ErrSymbolInactive
	}
	if detail := c.cachedContractDetail(symbol); detail != nil && detail.ConID > 0 {
		copy := *detail
		return &copy, nil
	}
	_, exchange, currency, primary := classifySymbol(symbol)
	contract := Contract{
		Symbol:      dualClassWireSymbol(symbol),
		SecType:     "STK",
		Exchange:    exchange,
		PrimaryExch: primary,
		Currency:    currency,
	}
	detail, err := c.ContractDetailsFirst(ctx, contract, timeout)
	if err != nil {
		return nil, err
	}
	if detail != nil && detail.ConID > 0 {
		c.contractMu.Lock()
		if existing, ok := c.contractCache[symbol]; !ok || existing.ConID == 0 {
			c.contractCache[symbol] = *detail
		}
		c.contractMu.Unlock()
	}
	return detail, nil
}

func (c *Connector) resolveWSHExactStockContract(ctx context.Context, symbol string, conID int, timeout time.Duration) (*ContractDetailsLite, error) {
	if conID <= 0 {
		return nil, &WSHError{Kind: WSHErrorUnsupportedSecurity, Operation: "resolve_contract"}
	}
	lookup := Contract{ConID: conID, SecType: "STK"}
	// A position-seeded cache row may supply a currency constraint, but it is
	// never returned as identity evidence. The exact response must independently
	if cached := c.cachedContractDetail(symbol); cached != nil && cached.ConID == conID &&
		(cached.SecType == "" || strings.EqualFold(cached.SecType, "STK")) {
		lookup.Currency = strings.TrimSpace(cached.Currency)
	}
	return c.ContractDetailsFirst(ctx, lookup, timeout)
}

func (c *Connector) resolveWSHStockIdentity(ctx context.Context, symbol string, conID int, timeout time.Duration) (*ContractDetailsLite, error) {
	if c == nil {
		return nil, &WSHError{Kind: WSHErrorTransport, Operation: "resolve_contract"}
	}
	if err := ctx.Err(); err != nil {
		return nil, newWSHContextError("resolve_contract", err)
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	secType, _, _, _ := classifySymbol(symbol)
	if symbol == "" || conID <= 0 || !strings.EqualFold(secType, "STK") {
		return nil, &WSHError{Kind: WSHErrorUnsupportedSecurity, Operation: "resolve_contract"}
	}
	resolver := c.resolveWSHExactContract
	if resolver == nil {
		resolver = c.resolveWSHExactStockContract
	}
	detail, err := resolver(ctx, symbol, conID, timeout)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, newWSHContextError("resolve_contract", err)
		}
		return nil, &WSHError{Kind: WSHErrorContractResolution, Operation: "resolve_contract"}
	}
	if !validWSHExactStockIdentity(symbol, conID, detail) {
		return nil, &WSHError{Kind: WSHErrorContractResolution, Operation: "resolve_contract"}
	}
	copy := *detail
	return &copy, nil
}

func validWSHExactStockIdentity(symbol string, conID int, detail *ContractDetailsLite) bool {
	if conID <= 0 || detail == nil || detail.ConID != conID || !strings.EqualFold(strings.TrimSpace(detail.SecType), "STK") {
		return false
	}
	wantSymbol := strings.ToUpper(strings.TrimSpace(dualClassWireSymbol(symbol)))
	gotSymbol := strings.ToUpper(strings.TrimSpace(detail.Symbol))
	return wantSymbol != "" && gotSymbol == wantSymbol
}

func boundedWSHResolveTimeout(ctx context.Context, fallback time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 && remaining < fallback {
			return remaining
		}
	}
	return fallback
}

type wshEventRequest struct {
	conID    int
	limit    int
	eventTag string
}

type wshPhase struct {
	operation  string
	responseID int
	send       func(context.Context, int) error
	cancel     func(context.Context, int) error
}

type wshPhaseResult struct {
	data string
	err  error
}

func fetchWSHJSON(ctx context.Context, conn *Connection, phase wshPhase) (string, error) {
	if conn == nil {
		return "", &WSHError{Kind: WSHErrorTransport, Operation: phase.operation}
	}
	reqID, err := conn.nextRequestID()
	if err != nil {
		return "", &WSHError{Kind: WSHErrorTransport, Operation: phase.operation}
	}
	resultCh := make(chan wshPhaseResult, 1)
	deliver := func(result wshPhaseResult) {
		select {
		case resultCh <- result:
		default:
		}
	}

	responseHandler := conn.RegisterHandler(phase.responseID, func(fields []string) {
		data, ok := parseWSHDataFields(fields, reqID)
		if ok {
			deliver(wshPhaseResult{data: data})
		}
	})
	errorHandler := conn.RegisterHandler(msgErrMsg, func(fields []string) {
		rid, code, message, ok := parseWSHErrorFields(fields)
		if ok && rid == reqID {
			deliver(wshPhaseResult{err: classifyWSHBrokerError(phase.operation, code, message)})
		}
	})
	systemHandler := conn.RegisterHandler(msgSystemNotification, func(fields []string) {
		if len(fields) < 2 {
			return
		}
		note, parseErr := parseSystemNotificationPayload([]byte(fields[1]))
		if parseErr == nil && note.tickerID == int64(reqID) {
			deliver(wshPhaseResult{err: classifyWSHBrokerError(phase.operation, note.code, note.message)})
		}
	})
	defer conn.UnregisterHandler(phase.responseID, responseHandler)
	defer conn.UnregisterHandler(msgErrMsg, errorHandler)
	defer conn.UnregisterHandler(msgSystemNotification, systemHandler)

	if err := phase.send(ctx, reqID); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", newWSHContextError(phase.operation, err)
		}
		if typed, ok := errors.AsType[*WSHError](err); ok {
			return "", typed
		}
		return "", &WSHError{Kind: WSHErrorTransport, Operation: phase.operation}
	}

	select {
	case result := <-resultCh:
		if result.err != nil {
			return "", result.err
		}
		if strings.TrimSpace(result.data) == "" {
			return "", &WSHError{Kind: WSHErrorMalformedResponse, Operation: phase.operation}
		}
		return strings.TrimSpace(result.data), nil
	case <-ctx.Done():
		cancelCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = phase.cancel(cancelCtx, reqID)
		cancel()
		return "", newWSHContextError(phase.operation, ctx.Err())
	}
}

func (c *Connection) sendWSHMetaDataRequest(ctx context.Context, reqID int) error {
	serverVersion := c.ServerVersion()
	if serverVersion < minServerVerWSHCalendar || serverVersion > maxClientVersion {
		return &WSHError{Kind: WSHErrorUnsupportedProtocol, Operation: "metadata"}
	}
	return c.sendMessageWithTypeContext(ctx, c.encodeMsg(reqWSHMetaData, reqID), RequestTypeGeneral)
}

func (c *Connection) cancelWSHMetaDataRequest(ctx context.Context, reqID int) error {
	return c.sendMessageWithTypeContext(ctx, c.encodeMsg(cancelWSHMetaData, reqID), RequestTypeGeneral)
}

func (c *Connection) sendWSHEventDataRequest(ctx context.Context, reqID int, request wshEventRequest) error {
	serverVersion := c.ServerVersion()
	if serverVersion < minServerVerWSHEventFilters || serverVersion > maxClientVersion {
		return &WSHError{Kind: WSHErrorUnsupportedProtocol, Operation: "event_data"}
	}
	if request.conID <= 0 {
		return &WSHError{Kind: WSHErrorUnsupportedSecurity, Operation: "event_data"}
	}
	eventTag := request.eventTag
	if eventTag == "" {
		eventTag = "wsh_ed"
	}
	if !validWSHEarningsEventTag(eventTag) {
		return &WSHError{Kind: WSHErrorEventTypeUnavailable, Operation: "event_data"}
	}
	filterJSON, err := json.Marshal(map[string]any{
		"country":      "All",
		"watchlist":    []string{strconv.Itoa(request.conID)},
		"limit_region": request.limit,
		"limit":        request.limit,
		eventTag:       "true",
	})
	if err != nil {
		return &WSHError{Kind: WSHErrorMalformedResponse, Operation: "event_data"}
	}
	fields := []any{
		reqWSHEventData,
		reqID,
		0, // conId and filter are mutually exclusive; watchlist carries it.
		string(filterJSON),
		false,
		false,
		false,
	}
	if serverVersion >= minServerVerWSHEventFilterDates {
		// Filter mode and conId/date mode are mutually exclusive. The conId is
		// inside the allowlisted watchlist filter, so date bounds stay empty.
		fields = append(fields, "", "", request.limit)
	}
	return c.sendMessageWithTypeContext(ctx, c.encodeMsg(fields...), RequestTypeGeneral)
}

func (c *Connection) cancelWSHEventDataRequest(ctx context.Context, reqID int) error {
	return c.sendMessageWithTypeContext(ctx, c.encodeMsg(cancelWSHEventData, reqID), RequestTypeGeneral)
}

func parseWSHDataFields(fields []string, expectedReqID int) (string, bool) {
	if len(fields) < 3 {
		return "", false
	}
	reqID, err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil || reqID != expectedReqID {
		return "", false
	}
	return fields[2], true
}

func parseWSHErrorFields(fields []string) (reqID, code int, message string, ok bool) {
	if len(fields) >= 5 {
		reqID, reqErr := strconv.Atoi(strings.TrimSpace(fields[2]))
		code, codeErr := strconv.Atoi(strings.TrimSpace(fields[3]))
		if reqErr == nil && codeErr == nil {
			return reqID, code, fields[4], true
		}
	}
	if len(fields) >= 4 {
		reqID, reqErr := strconv.Atoi(strings.TrimSpace(fields[1]))
		code, codeErr := strconv.Atoi(strings.TrimSpace(fields[2]))
		if reqErr == nil && codeErr == nil {
			return reqID, code, fields[3], true
		}
	}
	return 0, 0, "", false
}

func classifyWSHBrokerError(operation string, code int, _ string) error {
	kind := WSHErrorProviderFailure
	switch code {
	case 10278, 10281:
		kind = WSHErrorDuplicateRequest
	case 10282:
		kind = WSHErrorMetadataRequired
	case 354, 10089, 10090, 10186, 10276, 10277:
		kind = WSHErrorEntitlementRequired
	case 200, 203:
		kind = WSHErrorUnsupportedSecurity
	case 502, 504:
		kind = WSHErrorTransport
	case 503:
		kind = WSHErrorUnsupportedProtocol
	case 10279, 10280, 10283, 10284:
		kind = WSHErrorProviderFailure
	}
	return &WSHError{Kind: kind, Operation: operation, Code: code}
}

func newWSHContextError(operation string, err error) error {
	kind := WSHErrorCanceled
	if errors.Is(err, context.DeadlineExceeded) {
		kind = WSHErrorTimeout
	}
	return &WSHError{Kind: kind, Operation: operation, cause: err}
}

func wshMetadataEarningsEventTag(raw string) (string, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &envelope) != nil {
		return "", false
	}
	if nestedRaw, ok := envelope["meta_data"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(nestedRaw, &nested) != nil {
			return "", false
		}
		envelope = nested
	}
	eventTypes, ok := envelope["event_types"]
	if !ok || len(eventTypes) == 0 {
		return "", false
	}
	var entries []json.RawMessage
	if json.Unmarshal(eventTypes, &entries) != nil {
		return "", false
	}
	hasCurrent, hasLegacy := false, false
	for _, entry := range entries {
		switch typedWSHMetadataEventTag(entry) {
		case "wshe_ed":
			hasCurrent = true
		case "wsh_ed":
			hasLegacy = true
		}
	}
	if hasCurrent {
		return "wshe_ed", true
	}
	if hasLegacy {
		return "wsh_ed", true
	}
	return "", false
}

func typedWSHMetadataEventTag(raw json.RawMessage) string {
	var direct string
	if json.Unmarshal(raw, &direct) == nil {
		direct = strings.TrimSpace(direct)
		if validWSHEarningsEventTag(direct) {
			return direct
		}
		return ""
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return ""
	}
	values := make([]string, 0, 2)
	for _, key := range []string{"tag", "code"} {
		valueRaw, ok := object[key]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(valueRaw, &value) != nil {
			return ""
		}
		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}
	if len(values) == 0 || len(values) == 2 && values[0] != values[1] {
		return ""
	}
	if validWSHEarningsEventTag(values[0]) {
		return values[0]
	}
	return ""
}

func validWSHEarningsEventTag(tag string) bool {
	return tag == "wsh_ed" || tag == "wshe_ed"
}
