package ibkr

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// systemNoticeFrame encodes a msg-204 body as the Gateway sends it: the
// protobuf payload follows the four-byte message id without a delimiter.
func systemNoticeFrame(id int64, code int, message string) []byte {
	tid := uint64(id)
	if id < 0 {
		tid = math.MaxUint64
	}
	payload := binary.AppendUvarint(nil, 1<<3)
	payload = binary.AppendUvarint(payload, tid)
	payload = binary.AppendUvarint(payload, 3<<3)
	payload = binary.AppendUvarint(payload, uint64(code))
	payload = binary.AppendUvarint(payload, 4<<3|2)
	payload = binary.AppendUvarint(payload, uint64(len(message)))
	payload = append(payload, message...)
	return append(binary.BigEndian.AppendUint32(nil, uint32(msgSystemNotification)), payload...)
}

func wshKind(t *testing.T, err error, kind WSHErrorKind, operation string) *WSHError {
	t.Helper()
	typed, ok := errors.AsType[*WSHError](err)
	if !ok {
		t.Fatalf("err = %v (%T), want *WSHError %s/%s", err, err, kind, operation)
	}
	if typed.Kind != kind || typed.Operation != operation {
		t.Fatalf("err = %v, want kind %s operation %s", err, kind, operation)
	}
	return typed
}

// outboundFramesByID returns the decoded outbound frames with the given
// message id, in send order.
func outboundFramesByID(t *testing.T, conn *Connection, out *safeBuffer, msgID int) [][]string {
	t.Helper()
	var matched [][]string
	want := strconv.Itoa(msgID)
	for _, frame := range decodeOutboundFrames(t, conn, out.Bytes()) {
		if len(frame) > 0 && frame[0] == want {
			matched = append(matched, frame)
		}
	}
	return matched
}

// wireFieldCount is the number of delimited fields in a decoded outbound
// frame: decodeMessage keeps the empty token after the final delimiter.
func wireFieldCount(frame []string) int {
	if n := len(frame); n > 0 && frame[n-1] == "" {
		return n - 1
	}
	return len(frame)
}

func TestWSHErrorIsSanitizedAndUnwrapsOnlyTheCallerCause(t *testing.T) {
	var nilErr *WSHError
	if nilErr.Error() == "" || nilErr.Unwrap() != nil {
		t.Fatalf("nil receiver: %q %v", nilErr.Error(), nilErr.Unwrap())
	}
	coded := &WSHError{Kind: WSHErrorEntitlementRequired, Operation: "event_data", Code: 10089}
	if got := coded.Error(); !strings.Contains(got, "event_data") || !strings.Contains(got, "entitlement_required") || !strings.Contains(got, "10089") {
		t.Fatalf("coded error = %q", got)
	}
	plain := &WSHError{Kind: WSHErrorTransport, Operation: "metadata"}
	if got := plain.Error(); strings.Contains(got, "code") || !strings.Contains(got, "transport_failure") {
		t.Fatalf("plain error = %q", got)
	}
	timeout := newWSHContextError("acquire", context.DeadlineExceeded)
	if !errors.Is(timeout, context.DeadlineExceeded) {
		t.Fatalf("deadline cause not exposed: %v", timeout)
	}
	wshKind(t, timeout, WSHErrorTimeout, "acquire")
	canceled := newWSHContextError("acquire", context.Canceled)
	if !errors.Is(canceled, context.Canceled) {
		t.Fatalf("cancel cause not exposed: %v", canceled)
	}
	wshKind(t, canceled, WSHErrorCanceled, "acquire")
}

// Broker codes map to a closed set of kinds and the prose is dropped. The
// entitlement codes matter most: Wall Street Horizon is an optional
// subscription, and an account without it must learn that as a typed
// outcome, not a failure of the connection.
func TestClassifyWSHBrokerErrorMapsCodesWithoutKeepingProse(t *testing.T) {
	cases := map[int]WSHErrorKind{
		10278: WSHErrorDuplicateRequest, 10281: WSHErrorDuplicateRequest,
		10282: WSHErrorMetadataRequired,
		354:   WSHErrorEntitlementRequired, 10089: WSHErrorEntitlementRequired, 10090: WSHErrorEntitlementRequired,
		10186: WSHErrorEntitlementRequired, 10276: WSHErrorEntitlementRequired, 10277: WSHErrorEntitlementRequired,
		200: WSHErrorUnsupportedSecurity, 203: WSHErrorUnsupportedSecurity,
		502: WSHErrorTransport, 504: WSHErrorTransport,
		503:   WSHErrorUnsupportedProtocol,
		10279: WSHErrorProviderFailure, 10280: WSHErrorProviderFailure, 10283: WSHErrorProviderFailure, 10284: WSHErrorProviderFailure,
		999: WSHErrorProviderFailure,
	}
	for code, kind := range cases {
		err := classifyWSHBrokerError("event_data", code, "Requested market data requires additional subscription for API. DU1234567")
		typed := wshKind(t, err, kind, "event_data")
		if typed.Code != code {
			t.Fatalf("code %d kept as %d", code, typed.Code)
		}
		if strings.Contains(err.Error(), "DU1234567") || strings.Contains(err.Error(), "subscription for API") {
			t.Fatalf("broker prose leaked into %q", err.Error())
		}
	}
}

func TestParseWSHErrorFieldsAcceptsBothErrorFrameShapes(t *testing.T) {
	reqID, code, msg, ok := parseWSHErrorFields([]string{"4", "2", "17", "10089", "needs subscription"})
	if !ok || reqID != 17 || code != 10089 || msg != "needs subscription" {
		t.Fatalf("versioned frame: %d %d %q %t", reqID, code, msg, ok)
	}
	reqID, code, msg, ok = parseWSHErrorFields([]string{"4", "17", "10282", "metadata first"})
	if !ok || reqID != 17 || code != 10282 || msg != "metadata first" {
		t.Fatalf("unversioned frame: %d %d %q %t", reqID, code, msg, ok)
	}
	for _, fields := range [][]string{
		{"4", "2", "x", "10089", "m"},
		{"4", "17", "z"},
		nil,
	} {
		if _, _, _, ok := parseWSHErrorFields(fields); ok {
			t.Fatalf("malformed frame %q accepted", fields)
		}
	}
	if data, ok := parseWSHDataFields([]string{"104", "17", `{"a":1}`}, 17); !ok || data != `{"a":1}` {
		t.Fatalf("data fields: %q %t", data, ok)
	}
	for _, fields := range [][]string{{"104", "18", "{}"}, {"104", "x", "{}"}, {"104", "17"}} {
		if _, ok := parseWSHDataFields(fields, 17); ok {
			t.Fatalf("frame %q accepted for request 17", fields)
		}
	}
}

func TestWSHMetadataEarningsEventTagReadsEveryAdvertisedShape(t *testing.T) {
	cases := []struct {
		name, raw, want string
		ok              bool
	}{
		{"strings, current wins", `{"event_types":["wsh_ed","wshe_ed","wshe_dd"]}`, "wshe_ed", true},
		{"strings, legacy only", `{"event_types":["wshe_dd","wsh_ed"]}`, "wsh_ed", true},
		{"nested meta_data objects", `{"meta_data":{"event_types":[{"tag":"wshe_ed","code":"wshe_ed"}]}}`, "wshe_ed", true},
		{"object with tag only", `{"event_types":[{"tag":"wsh_ed"}]}`, "wsh_ed", true},
		{"object with code only", `{"event_types":[{"code":"wshe_ed"}]}`, "wshe_ed", true},
		{"object with conflicting tag and code", `{"event_types":[{"tag":"wsh_ed","code":"wshe_ed"}]}`, "", false},
		{"object with non-string tag", `{"event_types":[{"tag":7}]}`, "", false},
		{"no earnings type", `{"event_types":["wshe_dd"]}`, "", false},
		{"empty list", `{"event_types":[]}`, "", false},
		{"missing list", `{"other":1}`, "", false},
		{"not an array", `{"event_types":{"tag":"wsh_ed"}}`, "", false},
		{"malformed nested", `{"meta_data":[1]}`, "", false},
		{"not json", `metadata`, "", false},
	}
	for _, tc := range cases {
		got, ok := wshMetadataEarningsEventTag(tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: %q %t, want %q %t", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if typedWSHMetadataEventTag(json.RawMessage(`[1]`)) != "" {
		t.Fatal("an array is not an event tag")
	}
}

func TestBoundedWSHResolveTimeoutNeverExceedsTheCallerDeadline(t *testing.T) {
	if got := boundedWSHResolveTimeout(context.Background(), 5*time.Second); got != 5*time.Second {
		t.Fatalf("no deadline: %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if got := boundedWSHResolveTimeout(ctx, 5*time.Second); got <= 0 || got > time.Second {
		t.Fatalf("short deadline: %s", got)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Minute)
	defer cancel2()
	if got := boundedWSHResolveTimeout(ctx2, 5*time.Second); got != 5*time.Second {
		t.Fatalf("long deadline: %s", got)
	}
	expired, cancel3 := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel3()
	time.Sleep(time.Millisecond)
	if got := boundedWSHResolveTimeout(expired, 5*time.Second); got != 5*time.Second {
		t.Fatalf("expired deadline falls back: %s", got)
	}
}

func TestValidWSHExactStockIdentityRequiresConIDTypeAndSymbol(t *testing.T) {
	detail := &ContractDetailsLite{ConID: 4242, SecType: "STK", Symbol: "SYNX"}
	if !validWSHExactStockIdentity("synx", 4242, detail) {
		t.Fatal("matching identity refused")
	}
	dual := &ContractDetailsLite{ConID: 4243, SecType: "stk", Symbol: "SYN B"}
	if !validWSHExactStockIdentity("SYN.B", 4243, dual) {
		t.Fatal("dual-class wire spelling refused")
	}
	for name, tc := range map[string]struct {
		symbol string
		conID  int
		detail *ContractDetailsLite
	}{
		"nil detail":         {"SYNX", 4242, nil},
		"other conID":        {"SYNX", 4242, &ContractDetailsLite{ConID: 1, SecType: "STK", Symbol: "SYNX"}},
		"not a stock":        {"SYNX", 4242, &ContractDetailsLite{ConID: 4242, SecType: "OPT", Symbol: "SYNX"}},
		"other symbol":       {"SYNX", 4242, &ContractDetailsLite{ConID: 4242, SecType: "STK", Symbol: "SYNY"}},
		"non-positive conID": {"SYNX", 0, detail},
		"empty symbol":       {"", 4242, &ContractDetailsLite{ConID: 4242, SecType: "STK"}},
	} {
		if validWSHExactStockIdentity(tc.symbol, tc.conID, tc.detail) {
			t.Fatalf("%s accepted", name)
		}
	}
}

// The request encoders follow the TWS API field order: reqWshMetaData is
// [100, reqId]; reqWshEventData is [102, reqId, conId, filter, fillWatchlist,
// fillPortfolio, fillCompetitors] and from server version 173 adds startDate,
// endDate and totalLimit. Below the version that introduced each request the
// encoder refuses before a byte reaches the socket; a Gateway sent the frame
// would have answered with an unknown-message error and dropped the session.
func TestWSHRequestEncodersFollowServerVersionGates(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	ctx := t.Context()
	request := wshEventRequest{conID: 4242, limit: wshRequestLimit, eventTag: "wshe_ed"}

	conn.serverVersion = minServerVerWSHCalendar - 1
	wshKind(t, conn.sendWSHMetaDataRequest(ctx, 7), WSHErrorUnsupportedProtocol, "metadata")
	conn.serverVersion = minServerVerWSHEventFilters - 1
	wshKind(t, conn.sendWSHEventDataRequest(ctx, 7, request), WSHErrorUnsupportedProtocol, "event_data")
	conn.serverVersion = maxClientVersion + 1
	wshKind(t, conn.sendWSHMetaDataRequest(ctx, 7), WSHErrorUnsupportedProtocol, "metadata")
	wshKind(t, conn.sendWSHEventDataRequest(ctx, 7, request), WSHErrorUnsupportedProtocol, "event_data")
	conn.serverVersion = maxClientVersion
	wshKind(t, conn.sendWSHEventDataRequest(ctx, 7, wshEventRequest{conID: 0, limit: 10}), WSHErrorUnsupportedSecurity, "event_data")
	wshKind(t, conn.sendWSHEventDataRequest(ctx, 7, wshEventRequest{conID: 4242, limit: 10, eventTag: "wshe_dd"}), WSHErrorEventTypeUnavailable, "event_data")
	if len(out.Bytes()) != 0 {
		t.Fatalf("refused requests reached the socket: %x", out.Bytes())
	}

	conn.serverVersion = minServerVerWSHCalendar
	if err := conn.sendWSHMetaDataRequest(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if err := conn.cancelWSHMetaDataRequest(ctx, 7); err != nil {
		t.Fatal(err)
	}
	conn.serverVersion = minServerVerWSHEventFilterDates - 1
	if err := conn.sendWSHEventDataRequest(ctx, 8, wshEventRequest{conID: 4242, limit: 10}); err != nil {
		t.Fatal(err)
	}
	conn.serverVersion = minServerVerWSHEventFilterDates
	if err := conn.sendWSHEventDataRequest(ctx, 9, request); err != nil {
		t.Fatal(err)
	}
	if err := conn.cancelWSHEventDataRequest(ctx, 9); err != nil {
		t.Fatal(err)
	}
	frames := decodeOutboundFrames(t, conn, out.Bytes())
	if len(frames) != 5 {
		t.Fatalf("frames = %d: %q", len(frames), frames)
	}
	assertFields(t, frames[0], []fieldAssertion{{0, "100", "reqWshMetaData id"}, {1, "7", "reqWshMetaData reqId"}})
	// decodeMessage keeps the empty field after the final delimiter.
	if n := wireFieldCount(frames[0]); n != 2 {
		t.Fatalf("reqWshMetaData carries %d fields, want 2: %q", n, frames[0])
	}
	assertFields(t, frames[1], []fieldAssertion{{0, "101", "cancelWshMetaData id"}, {1, "7", "cancelWshMetaData reqId"}})
	short := frames[2]
	assertFields(t, short, []fieldAssertion{{0, "102", "reqWshEventData id"}, {1, "8", "reqId"}, {2, "0", "conId"}, {4, "0", "fillWatchlist"}, {5, "0", "fillPortfolio"}, {6, "0", "fillCompetitors"}})
	if n := wireFieldCount(short); n != 7 {
		t.Fatalf("server %d frame has %d fields, want 7 (no date filters): %q", minServerVerWSHEventFilterDates-1, n, short)
	}
	var filter map[string]any
	if err := json.Unmarshal([]byte(short[3]), &filter); err != nil {
		t.Fatalf("filter is not JSON: %v (%q)", err, short[3])
	}
	if list, _ := filter["watchlist"].([]any); len(list) != 1 || list[0] != "4242" || filter["wsh_ed"] != "true" || filter["country"] != "All" {
		t.Fatalf("filter = %v, want the conId in the watchlist and the legacy tag by default", filter)
	}
	dated := frames[3]
	if n := wireFieldCount(dated); n != 10 {
		t.Fatalf("server %d frame has %d fields, want 10: %q", minServerVerWSHEventFilterDates, n, dated)
	}
	assertFields(t, dated, []fieldAssertion{{1, "9", "reqId"}, {7, "", "startDate"}, {8, "", "endDate"}, {9, "10", "totalLimit"}})
	if !strings.Contains(dated[3], `"wshe_ed":"true"`) {
		t.Fatalf("event tag missing from filter: %q", dated[3])
	}
	assertFields(t, frames[4], []fieldAssertion{{0, "103", "cancelWshEventData id"}, {1, "9", "reqId"}})
}

// fetchWSHJSON correlates answers by request id: a frame for another
// request is ignored, an error frame for this request ends it with the
// classified kind, a msg-204 notice does the same, and a caller deadline
// cancels the request on the wire.
func TestFetchWSHJSONCorrelatesAnswersAndErrorsByRequestID(t *testing.T) {
	run := func(t *testing.T, respond func(conn *Connection, reqID int)) (string, *safeBuffer, *Connection, error) {
		conn, out := newReadyWireTestConnection(t)
		type result struct {
			data string
			err  error
		}
		done := make(chan result, 1)
		go func() {
			data, err := fetchWSHJSON(t.Context(), conn, wshPhase{operation: "metadata", responseID: msgWSHMetaData, send: conn.sendWSHMetaDataRequest, cancel: conn.cancelWSHMetaDataRequest})
			done <- result{data, err}
		}()
		reqID := waitForHandlerReqID(t, conn, msgWSHMetaData)
		respond(conn, reqID)
		select {
		case r := <-done:
			return r.data, out, conn, r.err
		case <-time.After(5 * time.Second):
			t.Fatal("fetchWSHJSON did not return")
			return "", nil, nil, nil
		}
	}

	t.Run("answer for this request", func(t *testing.T) {
		data, out, conn, err := run(t, func(conn *Connection, reqID int) {
			conn.processMessage(twsGatewayFrame(msgWSHMetaData, strconv.Itoa(reqID+1), `{"other":true}`))
			conn.processMessage(twsGatewayFrame(msgErrMsg, "2", strconv.Itoa(reqID+1), "10089", "another request"))
			conn.processMessage(twsGatewayFrame(msgWSHMetaData, strconv.Itoa(reqID), `  {"event_types":["wshe_ed"]} `))
		})
		if err != nil || data != `{"event_types":["wshe_ed"]}` {
			t.Fatalf("data=%q err=%v", data, err)
		}
		if frames := outboundFramesByID(t, conn, out, reqWSHMetaData); len(frames) != 1 {
			t.Fatalf("metadata requests sent = %d", len(frames))
		}
	})
	t.Run("empty answer is malformed", func(t *testing.T) {
		_, _, _, err := run(t, func(conn *Connection, reqID int) {
			conn.processMessage(twsGatewayFrame(msgWSHMetaData, strconv.Itoa(reqID), "   "))
		})
		wshKind(t, err, WSHErrorMalformedResponse, "metadata")
	})
	t.Run("legacy error frame", func(t *testing.T) {
		_, _, conn, err := run(t, func(conn *Connection, reqID int) {
			conn.processMessage(twsGatewayFrame(msgErrMsg, "2", strconv.Itoa(reqID), "10089", "WSH subscription required"))
		})
		if typed := wshKind(t, err, WSHErrorEntitlementRequired, "metadata"); typed.Code != 10089 {
			t.Fatalf("code = %d", typed.Code)
		}
		if !conn.IsConnected() {
			t.Fatal("a missing subscription ended the session")
		}
	})
	t.Run("system notice", func(t *testing.T) {
		_, _, _, err := run(t, func(conn *Connection, reqID int) {
			conn.processMessage(systemNoticeFrame(int64(reqID), 10282, "metadata must be requested first"))
		})
		if typed := wshKind(t, err, WSHErrorMetadataRequired, "metadata"); typed.Code != 10282 {
			t.Fatalf("code = %d", typed.Code)
		}
	})
	t.Run("caller deadline cancels on the wire", func(t *testing.T) {
		conn, out := newReadyWireTestConnection(t)
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer cancel()
		_, err := fetchWSHJSON(ctx, conn, wshPhase{operation: "event_data", responseID: msgWSHEventData, send: func(ctx context.Context, reqID int) error {
			return conn.sendWSHEventDataRequest(ctx, reqID, wshEventRequest{conID: 4242, limit: 10})
		}, cancel: conn.cancelWSHEventDataRequest})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the deadline cause", err)
		}
		wshKind(t, err, WSHErrorTimeout, "event_data")
		requests := outboundFramesByID(t, conn, out, reqWSHEventData)
		cancels := outboundFramesByID(t, conn, out, cancelWSHEventData)
		if len(requests) != 1 || len(cancels) != 1 || requests[0][1] != cancels[0][1] {
			t.Fatalf("requests=%q cancels=%q, want one cancel for the one request", requests, cancels)
		}
	})
	t.Run("send refusal is typed", func(t *testing.T) {
		conn, out := newReadyWireTestConnection(t)
		conn.serverVersion = minServerVerWSHCalendar - 1
		_, err := fetchWSHJSON(t.Context(), conn, wshPhase{operation: "metadata", responseID: msgWSHMetaData, send: conn.sendWSHMetaDataRequest, cancel: conn.cancelWSHMetaDataRequest})
		wshKind(t, err, WSHErrorUnsupportedProtocol, "metadata")
		if len(out.Bytes()) != 0 {
			t.Fatal("a refused request reached the socket")
		}
		_, err = fetchWSHJSON(t.Context(), conn, wshPhase{operation: "metadata", responseID: msgWSHMetaData, send: func(context.Context, int) error { return errors.New("socket gone") }})
		wshKind(t, err, WSHErrorTransport, "metadata")
		_, err = fetchWSHJSON(t.Context(), nil, wshPhase{operation: "metadata"})
		wshKind(t, err, WSHErrorTransport, "metadata")
	})
}

type wshFixture struct {
	c    *Connector
	conn *Connection
	out  *safeBuffer
}

func newWSHFixture(t *testing.T) *wshFixture {
	t.Helper()
	conn, c, out, _, _ := newQueuedInstructionReconnectFixture(t)
	c.resolveWSHContract = func(context.Context, string, time.Duration) (*ContractDetailsLite, error) {
		return &ContractDetailsLite{ConID: 4242, SecType: "STK", Symbol: "SYNX", Currency: "USD"}, nil
	}
	return &wshFixture{c: c, conn: conn, out: out}
}

// serve answers one metadata and one event request in the order the
// connector sends them; a nil reply leaves that request unanswered.
func (f *wshFixture) answerMetadata(t *testing.T, after int, body []byte) int {
	t.Helper()
	reqID := waitForHandlerReqIDAfter(t, f.conn, msgWSHMetaData, after)
	if body != nil {
		f.conn.processMessage(append(twsGatewayFrame(msgWSHMetaData, strconv.Itoa(reqID)), body...))
	}
	return reqID
}

func (f *wshFixture) answerEvents(t *testing.T, after int, body []byte) int {
	t.Helper()
	reqID := waitForHandlerReqIDAfter(t, f.conn, msgWSHEventData, after)
	if body != nil {
		f.conn.processMessage(append(twsGatewayFrame(msgWSHEventData, strconv.Itoa(reqID)), body...))
	}
	return reqID
}

func wshJSONBody(s string) []byte { return append([]byte(s), 0) }

const wshMetadataJSON = `{"event_types":[{"tag":"wshe_ed","code":"wshe_ed"},{"tag":"wshe_dd","code":"wshe_dd"}]}`
const wshEventsJSON = `{"wshe_ed":[{"conId":4242,"date":"2026-11-20"}]}`

func fetchEarningsAsync(f *wshFixture, symbol string) <-chan struct {
	json string
	err  error
} {
	done := make(chan struct {
		json string
		err  error
	}, 1)
	go func() {
		j, err := f.c.FetchWSHEarnings(context.Background(), symbol)
		done <- struct {
			json string
			err  error
		}{j, err}
	}()
	return done
}

func awaitEarnings[T any](t *testing.T, done <-chan T) T {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("WSH request did not finish")
		var zero T
		return zero
	}
}

// The event read is two requests: metadata once per session and day, then
// the filtered event calendar. The second symbol reuses the metadata; after
// the metadata TTL it is requested again.
func TestFetchWSHEarningsRequestsMetadataOncePerSessionAndDay(t *testing.T) {
	f := newWSHFixture(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	f.c.wshNow = func() time.Time { return now }

	done := fetchEarningsAsync(f, "synx")
	metaID := f.answerMetadata(t, 0, wshJSONBody(wshMetadataJSON))
	eventID := f.answerEvents(t, metaID, wshJSONBody(wshEventsJSON))
	r := awaitEarnings(t, done)
	if r.err != nil || r.json != wshEventsJSON {
		t.Fatalf("first read: %q %v", r.json, r.err)
	}
	events := outboundFramesByID(t, f.conn, f.out, reqWSHEventData)
	if len(events) != 1 || events[0][1] != strconv.Itoa(eventID) || !strings.Contains(events[0][3], `"wshe_ed":"true"`) || !strings.Contains(events[0][3], `"4242"`) {
		t.Fatalf("event request = %q", events)
	}

	done = fetchEarningsAsync(f, "synx")
	f.answerEvents(t, eventID, wshJSONBody(wshEventsJSON))
	if r := awaitEarnings(t, done); r.err != nil {
		t.Fatalf("second read: %v", r.err)
	}
	if n := len(outboundFramesByID(t, f.conn, f.out, reqWSHMetaData)); n != 1 {
		t.Fatalf("metadata requested %d times within the day", n)
	}

	now = now.Add(wshMetadataReadyTTL)
	done = fetchEarningsAsync(f, "synx")
	metaID = f.answerMetadata(t, eventID, wshJSONBody(wshMetadataJSON))
	f.answerEvents(t, metaID, wshJSONBody(wshEventsJSON))
	if r := awaitEarnings(t, done); r.err != nil {
		t.Fatalf("read after TTL: %v", r.err)
	}
	if n := len(outboundFramesByID(t, f.conn, f.out, reqWSHMetaData)); n != 2 {
		t.Fatalf("metadata requested %d times across the TTL, want 2", n)
	}
}

// TWS invalidates its metadata cache without dropping the socket and
// answers the event request with 10282. The connector refreshes the metadata
// and retries once; a second 10282 is returned as is.
func TestFetchWSHEarningsRefreshesMetadataOn10282Once(t *testing.T) {
	f := newWSHFixture(t)
	done := fetchEarningsAsync(f, "SYNX")
	metaID := f.answerMetadata(t, 0, wshJSONBody(wshMetadataJSON))
	eventID := f.answerEvents(t, metaID, nil)
	f.conn.processMessage(twsGatewayFrame(msgErrMsg, "2", strconv.Itoa(eventID), "10282", "metadata required"))
	metaID = f.answerMetadata(t, eventID, wshJSONBody(wshMetadataJSON))
	f.answerEvents(t, metaID, wshJSONBody(wshEventsJSON))
	if r := awaitEarnings(t, done); r.err != nil || r.json != wshEventsJSON {
		t.Fatalf("after one refresh: %q %v", r.json, r.err)
	}

	f.c.resetWSHMetadataReadiness()
	done = fetchEarningsAsync(f, "SYNX")
	last := metaID
	for range 2 {
		metaID = f.answerMetadata(t, last, wshJSONBody(wshMetadataJSON))
		eventID = f.answerEvents(t, metaID, nil)
		f.conn.processMessage(twsGatewayFrame(msgErrMsg, "2", strconv.Itoa(eventID), "10282", "metadata required"))
		last = eventID
	}
	r := awaitEarnings(t, done)
	wshKind(t, r.err, WSHErrorMetadataRequired, "event_data")
}

// An account without the Wall Street Horizon subscription learns that from
// the typed entitlement error: the read returns at once, the per-client
// request gate is released for the next caller, and the session stays up.
func TestFetchWSHEarningsWithoutSubscriptionIsATypedOutcome(t *testing.T) {
	f := newWSHFixture(t)
	done := fetchEarningsAsync(f, "SYNX")
	metaID := f.answerMetadata(t, 0, nil)
	f.conn.processMessage(twsGatewayFrame(msgErrMsg, "2", strconv.Itoa(metaID), "10089", "Requested market data requires additional subscription for API."))
	r := awaitEarnings(t, done)
	typed := wshKind(t, r.err, WSHErrorEntitlementRequired, "metadata")
	if typed.Code != 10089 || strings.Contains(r.err.Error(), "additional subscription") {
		t.Fatalf("entitlement error = %v", r.err)
	}
	if !f.conn.IsConnected() {
		t.Fatal("the missing subscription ended the broker session")
	}
	// The gate was released: a probe gets it at once and its own deadline,
	// not the gate, bounds the unanswered metadata request.
	probeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	wshKind(t, f.c.ProbeWSHEntitlement(probeCtx), WSHErrorTimeout, "metadata")
}

func TestProbeWSHEntitlementReadsMetadataOnce(t *testing.T) {
	f := newWSHFixture(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	f.c.wshNow = func() time.Time { return now }
	done := make(chan error, 1)
	go func() { done <- f.c.ProbeWSHEntitlement(context.Background()) }()
	metaID := f.answerMetadata(t, 0, wshJSONBody(wshMetadataJSON))
	if err := awaitEarnings(t, done); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := f.c.ProbeWSHEntitlement(context.Background()); err != nil {
		t.Fatalf("second probe within the day: %v", err)
	}
	if n := len(outboundFramesByID(t, f.conn, f.out, reqWSHMetaData)); n != 1 {
		t.Fatalf("metadata requested %d times", n)
	}

	f.c.resetWSHMetadataReadiness()
	go func() { done <- f.c.ProbeWSHEntitlement(context.Background()) }()
	id := waitForHandlerReqIDAfter(t, f.conn, msgWSHMetaData, metaID)
	f.conn.processMessage(twsGatewayFrame(msgErrMsg, "2", strconv.Itoa(id), "354", "not subscribed"))
	wshKind(t, awaitEarnings(t, done), WSHErrorEntitlementRequired, "metadata")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	wshKind(t, f.c.ProbeWSHEntitlement(canceled), WSHErrorCanceled, "acquire")
	var none *Connector
	wshKind(t, none.ProbeWSHEntitlement(context.Background()), WSHErrorTransport, "acquire")
	f.conn.serverVersion = minServerVerWSHEventFilters - 1
	wshKind(t, f.c.ProbeWSHEntitlement(context.Background()), WSHErrorUnsupportedProtocol, "metadata")
	f.conn.setStatus(StatusDisconnected)
	wshKind(t, f.c.ProbeWSHEntitlement(context.Background()), WSHErrorTransport, "metadata")
}

func TestFetchWSHEarningsRefusesBeforeTheWireWhenItCannotSucceed(t *testing.T) {
	f := newWSHFixture(t)
	bg := context.Background()
	var none *Connector
	if _, err := none.FetchWSHEarnings(bg, "SYNX"); err == nil {
		t.Fatal("nil connector succeeded")
	} else {
		wshKind(t, err, WSHErrorTransport, "acquire")
	}
	canceled, cancel := context.WithCancel(bg)
	cancel()
	_, err := f.c.FetchWSHEarnings(canceled, "SYNX")
	wshKind(t, err, WSHErrorCanceled, "acquire")
	_, err = f.c.FetchWSHEarnings(bg, "  ")
	wshKind(t, err, WSHErrorUnsupportedSecurity, "resolve_contract")
	_, err = f.c.FetchWSHEarnings(bg, "VIX")
	wshKind(t, err, WSHErrorUnsupportedSecurity, "resolve_contract")
	_, err = f.c.FetchWSHEarningsWithIdentity(bg, "SYNX", 0)
	wshKind(t, err, WSHErrorUnsupportedSecurity, "resolve_contract")

	f.c.resolveWSHContract = func(context.Context, string, time.Duration) (*ContractDetailsLite, error) {
		return nil, ErrSymbolInactive
	}
	_, err = f.c.FetchWSHEarnings(bg, "SYNX")
	wshKind(t, err, WSHErrorConnectorInactive, "resolve_contract")
	f.c.resolveWSHContract = func(context.Context, string, time.Duration) (*ContractDetailsLite, error) {
		return nil, context.DeadlineExceeded
	}
	_, err = f.c.FetchWSHEarnings(bg, "SYNX")
	wshKind(t, err, WSHErrorTimeout, "resolve_contract")
	f.c.resolveWSHContract = func(context.Context, string, time.Duration) (*ContractDetailsLite, error) {
		return nil, errors.New("no definition")
	}
	_, err = f.c.FetchWSHEarnings(bg, "SYNX")
	wshKind(t, err, WSHErrorContractResolution, "resolve_contract")
	f.c.resolveWSHContract = func(context.Context, string, time.Duration) (*ContractDetailsLite, error) {
		return &ContractDetailsLite{ConID: 4242, SecType: "OPT", Symbol: "SYNX"}, nil
	}
	_, err = f.c.FetchWSHEarnings(bg, "SYNX")
	wshKind(t, err, WSHErrorUnsupportedSecurity, "resolve_contract")
	f.c.resolveWSHContract = func(context.Context, string, time.Duration) (*ContractDetailsLite, error) {
		return &ContractDetailsLite{SecType: "STK", Symbol: "SYNX"}, nil
	}
	_, err = f.c.FetchWSHEarnings(bg, "SYNX")
	wshKind(t, err, WSHErrorUnsupportedSecurity, "resolve_contract")
	if len(f.out.Bytes()) != 0 {
		t.Fatalf("refused reads reached the socket: %q", decodeOutboundFrames(t, f.conn, f.out.Bytes()))
	}

	f.c.resolveWSHContract = func(context.Context, string, time.Duration) (*ContractDetailsLite, error) {
		return &ContractDetailsLite{ConID: 4242, SecType: "STK", Symbol: "SYNX"}, nil
	}
	f.conn.serverVersion = minServerVerWSHEventFilters - 1
	_, err = f.c.FetchWSHEarnings(bg, "SYNX")
	wshKind(t, err, WSHErrorUnsupportedProtocol, "event_data")
	f.conn.serverVersion = maxClientVersion
	f.conn.setStatus(StatusDisconnected)
	_, err = f.c.FetchWSHEarnings(bg, "SYNX")
	wshKind(t, err, WSHErrorTransport, "metadata")
}

// A symbol the connector marked inactive after repeated definition failures
// is refused without a request, and a resolver that resolves while the mark
// lands is refused too; the mark is connector state, never a WSH verdict.
func TestFetchWSHEarningsHonoursTheInactiveMark(t *testing.T) {
	f := newWSHFixture(t)
	for range inactiveConfirmations {
		if _, post := f.c.registerInactiveCandidatePostAction("SYNX", "No security definition has been found"); post != nil {
			post()
		}
	}
	if !f.c.IsSymbolInactive("SYNX") {
		t.Fatal("symbol not marked inactive")
	}
	_, err := f.c.FetchWSHEarnings(context.Background(), "synx")
	wshKind(t, err, WSHErrorConnectorInactive, "resolve_contract")
	if len(f.out.Bytes()) != 0 {
		t.Fatal("an inactive symbol reached the wire")
	}
	// The exact-identity path ignores the mark: the caller proved the conId.
	f.c.resolveWSHExactContract = func(context.Context, string, int, time.Duration) (*ContractDetailsLite, error) {
		return &ContractDetailsLite{ConID: 4242, SecType: "STK", Symbol: "SYNX"}, nil
	}
	type res struct {
		r   WSHEarningsResult
		err error
	}
	done := make(chan res, 1)
	go func() {
		r, err := f.c.FetchWSHEarningsWithIdentity(context.Background(), "synx", 4242)
		done <- res{r, err}
	}()
	metaID := f.answerMetadata(t, 0, wshJSONBody(wshMetadataJSON))
	f.answerEvents(t, metaID, wshJSONBody(wshEventsJSON))
	r := awaitEarnings(t, done)
	if r.err != nil || r.r.EventJSON != wshEventsJSON || r.r.StockIdentity == nil || r.r.StockIdentity.ConID != 4242 {
		t.Fatalf("exact read: %+v %v", r.r, r.err)
	}
}

// Exact identity is evidence, not a precondition: a resolution that fails or
// contradicts the caller's conId leaves StockIdentity nil while the events
// are still read for that conId. A caller cancellation during resolution
// ends the read.
func TestFetchWSHEarningsWithIdentityReportsOnlyAConfirmedIdentity(t *testing.T) {
	f := newWSHFixture(t)
	f.c.resolveWSHExactContract = func(_ context.Context, _ string, conID int, _ time.Duration) (*ContractDetailsLite, error) {
		return &ContractDetailsLite{ConID: conID, SecType: "STK", Symbol: "OTHER"}, nil
	}
	type res struct {
		r   WSHEarningsResult
		err error
	}
	done := make(chan res, 1)
	go func() {
		r, err := f.c.FetchWSHEarningsWithIdentity(context.Background(), "SYNX", 4242)
		done <- res{r, err}
	}()
	metaID := f.answerMetadata(t, 0, wshJSONBody(wshMetadataJSON))
	eventID := f.answerEvents(t, metaID, wshJSONBody(wshEventsJSON))
	r := awaitEarnings(t, done)
	if r.err != nil || r.r.StockIdentity != nil || r.r.EventJSON != wshEventsJSON {
		t.Fatalf("contradicted identity: %+v %v", r.r, r.err)
	}
	events := outboundFramesByID(t, f.conn, f.out, reqWSHEventData)
	if len(events) != 1 || !strings.Contains(events[0][3], `"4242"`) {
		t.Fatalf("event request = %q, want the caller's conId", events)
	}

	f.c.resolveWSHExactContract = func(context.Context, string, int, time.Duration) (*ContractDetailsLite, error) {
		return nil, context.Canceled
	}
	_, err := f.c.FetchWSHEarningsWithIdentity(context.Background(), "SYNX", 4242)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during resolution: %v", err)
	}
	if n := len(outboundFramesByID(t, f.conn, f.out, reqWSHEventData)); n != 1 {
		t.Fatalf("a canceled resolution still sent an event request (%d)", n)
	}
	_ = eventID
}

func TestResolveWSHStockIdentityValidatesTheBrokerAnswer(t *testing.T) {
	f := newWSHFixture(t)
	bg := context.Background()
	f.c.resolveWSHExactContract = func(_ context.Context, _ string, conID int, _ time.Duration) (*ContractDetailsLite, error) {
		return &ContractDetailsLite{ConID: conID, SecType: "STK", Symbol: "SYNX"}, nil
	}
	//lint:ignore SA1012 the nil-context refusal is the behaviour under test
	detail, err := f.c.ResolveWSHStockIdentity(nil, "synx", 4242)
	if err != nil || detail == nil || detail.ConID != 4242 {
		t.Fatalf("exact identity: %+v %v", detail, err)
	}
	for name, tc := range map[string]struct {
		symbol string
		conID  int
		kind   WSHErrorKind
	}{
		"empty symbol": {"", 4242, WSHErrorUnsupportedSecurity},
		"index":        {"VIX", 4242, WSHErrorUnsupportedSecurity},
		"no conID":     {"SYNX", 0, WSHErrorUnsupportedSecurity},
	} {
		if _, err := f.c.ResolveWSHStockIdentity(bg, tc.symbol, tc.conID); err == nil {
			t.Fatalf("%s resolved", name)
		} else {
			wshKind(t, err, tc.kind, "resolve_contract")
		}
	}
	f.c.resolveWSHExactContract = func(_ context.Context, _ string, conID int, _ time.Duration) (*ContractDetailsLite, error) {
		return &ContractDetailsLite{ConID: conID, SecType: "STK", Symbol: "OTHER"}, nil
	}
	_, err = f.c.ResolveWSHStockIdentity(bg, "SYNX", 4242)
	wshKind(t, err, WSHErrorContractResolution, "resolve_contract")
	f.c.resolveWSHExactContract = func(context.Context, string, int, time.Duration) (*ContractDetailsLite, error) {
		return nil, errors.New("not found")
	}
	_, err = f.c.ResolveWSHStockIdentity(bg, "SYNX", 4242)
	wshKind(t, err, WSHErrorContractResolution, "resolve_contract")
	f.c.resolveWSHExactContract = func(context.Context, string, int, time.Duration) (*ContractDetailsLite, error) {
		return nil, context.DeadlineExceeded
	}
	_, err = f.c.ResolveWSHStockIdentity(bg, "SYNX", 4242)
	wshKind(t, err, WSHErrorTimeout, "resolve_contract")
	canceled, cancel := context.WithCancel(bg)
	cancel()
	_, err = f.c.ResolveWSHStockIdentity(canceled, "SYNX", 4242)
	wshKind(t, err, WSHErrorCanceled, "resolve_contract")
	var none *Connector
	_, err = none.ResolveWSHStockIdentity(bg, "SYNX", 4242)
	wshKind(t, err, WSHErrorTransport, "resolve_contract")
}

// Metadata and event answers that are not JSON, or metadata without an
// earnings event type, are reported as such and never passed on.
func TestFetchWSHEarningsRejectsMalformedBrokerJSON(t *testing.T) {
	for _, tc := range []struct {
		name      string
		metadata  []byte
		events    []byte
		kind      WSHErrorKind
		operation string
	}{
		{"metadata not json", wshJSONBody("<html>"), nil, WSHErrorMalformedResponse, "metadata"},
		{"metadata without earnings", wshJSONBody(`{"event_types":["wshe_dd"]}`), nil, WSHErrorEventTypeUnavailable, "metadata"},
		{"events not json", wshJSONBody(wshMetadataJSON), wshJSONBody("{"), WSHErrorMalformedResponse, "event_data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWSHFixture(t)
			done := fetchEarningsAsync(f, "SYNX")
			metaID := f.answerMetadata(t, 0, tc.metadata)
			if tc.events != nil {
				f.answerEvents(t, metaID, tc.events)
			}
			r := awaitEarnings(t, done)
			wshKind(t, r.err, tc.kind, tc.operation)
			if r.json != "" {
				t.Fatalf("malformed answer passed on: %q", r.json)
			}
		})
	}
}

// Two readers share one WSH gate per client, as IBKR allows one request of
// each kind at a time; the second waits for the first and its own context
// bounds that wait.
func TestWSHGateSerializesReadersAndHonoursTheWaitersContext(t *testing.T) {
	f := newWSHFixture(t)
	release, err := f.c.acquireWSH(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = f.c.FetchWSHEarnings(ctx, "SYNX")
	wshKind(t, err, WSHErrorTimeout, "acquire")
	release()
	if _, err := f.c.acquireWSH(context.Background()); err != nil {
		t.Fatalf("gate not released: %v", err)
	}
	var none *Connector
	none.resetWSHMetadataReadiness()
}
