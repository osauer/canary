package ibkr

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// histTGateway is a loopback TWS for the history paths: it decodes every
// outbound frame, keeps it, and hands it to the test's script, which answers
// through the connector's own inbound handlers the way the socket reader
// would. Answers sent from inside Write land before the request returns, as a
// fast gateway's do.
type histTGateway struct {
	c       *Connector
	conn    *Connection
	mu      sync.Mutex
	frames  [][]string
	onFrame func(fields []string)
}

func (g *histTGateway) Write(p []byte) (int, error) {
	for off := 0; off+4 <= len(p); {
		n := int(binary.BigEndian.Uint32(p[off : off+4]))
		fields := g.conn.decodeMessage(p[off+4 : off+4+n])
		off += 4 + n
		g.mu.Lock()
		g.frames = append(g.frames, fields)
		handler := g.onFrame
		g.mu.Unlock()
		if handler != nil {
			handler(fields)
		}
	}
	return len(p), nil
}

// framesByID returns the outbound frames with the given message id, in order.
func (g *histTGateway) framesByID(msgID int) [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	want := strconv.Itoa(msgID)
	var out [][]string
	for _, f := range g.frames {
		if len(f) > 0 && f[0] == want {
			out = append(out, f)
		}
	}
	return out
}

// waitFramesByID polls for at least n frames with msgID.
func (g *histTGateway) waitFramesByID(t *testing.T, msgID, n int) [][]string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if frames := g.framesByID(msgID); len(frames) >= n {
			return frames
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("wanted %d frames of msgID %d, got %v", n, msgID, g.framesByID(msgID))
	return nil
}

func (g *histTGateway) script(onFrame func(fields []string)) {
	g.mu.Lock()
	g.onFrame = onFrame
	g.mu.Unlock()
}

// histTFixture is a ready connector whose socket is a histTGateway.
func histTFixture(t *testing.T) (*Connector, *Connection, *histTGateway) {
	t.Helper()
	conn, c, _, _, _ := newQueuedInstructionReconnectFixture(t)
	g := &histTGateway{c: c, conn: conn}
	conn.writer = bufio.NewWriter(g)
	return c, conn, g
}

// histTBar is one coherent daily bar: open, high, low and close all at value.
func histTBar(date string, value float64) HistoricalBar {
	return HistoricalBar{Date: date, Open: value, High: value, Low: value, Close: value, Volume: 1000, Average: value, BarCount: 10}
}

// histTHistoricalFrame is the modern (unversioned) historicalData frame.
func histTHistoricalFrame(reqID int, bars []HistoricalBar) []string {
	fields := []string{strconv.Itoa(msgHistoricalData), strconv.Itoa(reqID), strconv.Itoa(len(bars))}
	for _, b := range bars {
		fields = append(fields, b.Date,
			strconv.FormatFloat(b.Open, 'f', -1, 64), strconv.FormatFloat(b.High, 'f', -1, 64),
			strconv.FormatFloat(b.Low, 'f', -1, 64), strconv.FormatFloat(b.Close, 'f', -1, 64),
			strconv.FormatInt(b.Volume, 10), strconv.FormatFloat(b.Average, 'f', -1, 64), strconv.Itoa(b.BarCount))
	}
	return append(fields, "")
}

// histTAnswerHistory delivers bars and the end marker for a history request.
func histTAnswerHistory(c *Connector, reqID int, bars []HistoricalBar) {
	c.handleHistoricalData(histTHistoricalFrame(reqID, bars))
	c.handleHistoricalDataEnd([]string{strconv.Itoa(msgHistoricalDataEnd), strconv.Itoa(reqID), "20260901  00:00:00", "20260930  00:00:00"})
}

// histTAnswerRoute answers an exact-ConID contract-details request with the
// given details and the end marker.
func histTAnswerRoute(conn *Connection, reqID int, details ...[]string) {
	for _, d := range details {
		conn.dispatchHandlers(msgContractData, d, conn.BrokerSessionEpoch())
	}
	prewarmTestEnd(conn, reqID)
}

func histTFeeContract() Contract {
	return Contract{ConID: 4242, Symbol: "SYNX", SecType: "STK", PrimaryExch: "NASDAQ", Currency: "USD"}
}

func histTReqID(t *testing.T, fields []string, idx int) int {
	t.Helper()
	if len(fields) <= idx {
		t.Fatalf("frame %v has no field %d", fields, idx)
	}
	id, err := strconv.Atoi(fields[idx])
	if err != nil {
		t.Fatalf("frame %v field %d is not a request id: %v", fields, idx, err)
	}
	return id
}

func histTRequestError(t *testing.T, err error, category string) *HistoricalRequestError {
	t.Helper()
	reqErr, ok := errors.AsType[*HistoricalRequestError](err)
	if !ok || reqErr.Category != category {
		t.Fatalf("error = %v, want HistoricalRequestError category %s", err, category)
	}
	if reqErr.Message != "" {
		t.Fatalf("broker prose %q leaked through the sanitized error", reqErr.Message)
	}
	return reqErr
}

// histTFeeRateScript answers the route lookup with one SMART/NASDAQ listing
// and the FEE_RATE request with the given bars; it refuses no request.
func histTFeeRateScript(c *Connector, conn *Connection, bars []HistoricalBar) func([]string) {
	return func(fields []string) {
		switch fields[0] {
		case strconv.Itoa(reqContractData):
			reqID, _ := strconv.Atoi(fields[2])
			conID, _ := strconv.Atoi(fields[3])
			histTAnswerRoute(conn, reqID, orderContractDetailsFrame(reqID, conID, "SYNX", "STK", "SMART", "NASDAQ", "USD", "SYNX", "NMS", 0))
		case strconv.Itoa(reqHistoricalData):
			reqID, _ := strconv.Atoi(fields[1])
			histTAnswerHistory(c, reqID, bars)
		}
	}
}

// A FEE_RATE read of a contract without an executable route first asks the
// gateway for the exact ConID's listing, routes the history request by the
// answer, and returns the daily rates in the bars' closes.
func TestHistoricalFeeRateResolvesRouteThenRequestsFeeRateBars(t *testing.T) {
	c, conn, g := histTFixture(t)
	rates := []HistoricalBar{histTBar("20260928", 0.25), histTBar("20260929", 0.3125)}
	g.script(histTFeeRateScript(c, conn, rates))

	bars, err := c.FetchHistoricalDailyFeeRates(t.Context(), histTFeeContract(), 7, 2*time.Second)
	if err != nil {
		t.Fatalf("fee rates = %v", err)
	}
	if len(bars) != 2 || bars[0].Close != 0.25 || bars[1].Close != 0.3125 || bars[1].Time.Format("2006-01-02") != "2026-09-29" {
		t.Fatalf("bars = %+v, want the two daily rates", bars)
	}

	routes := g.framesByID(reqContractData)
	if len(routes) != 1 {
		t.Fatalf("route lookups = %v, want one", routes)
	}
	assertFields(t, routes[0], []fieldAssertion{
		{1, "8", "route version"}, {3, "4242", "route conID"}, {4, "SYNX", "route symbol"}, {5, "STK", "route secType"},
		{10, "", "route exchange left for the gateway"}, {11, "NASDAQ", "route primary"}, {12, "USD", "route currency"},
	})
	history := g.framesByID(reqHistoricalData)
	if len(history) != 1 {
		t.Fatalf("history requests = %v, want one", history)
	}
	assertFields(t, history[0], []fieldAssertion{
		{2, "4242", "conID"}, {3, "SYNX", "symbol"}, {4, "STK", "secType"}, {5, "", "expiry"}, {6, "", "strike"}, {7, "", "right"},
		{8, "", "multiplier"}, {9, "SMART", "exchange from the listing"}, {10, "NASDAQ", "primary"}, {11, "USD", "currency"},
		{12, "SYNX", "localSymbol from the listing"}, {13, "NMS", "tradingClass from the listing"}, {14, "0", "includeExpired"},
		{15, "", "endDateTime"}, {16, "1 day", "barSizeSetting"}, {17, "7 D", "durationStr"}, {18, "1", "useRTH"},
		{19, "FEE_RATE", "whatToShow"}, {20, "2", "formatDate"}, {21, "0", "keepUpToDate"}, {22, "", "chartOptions"},
	})
}

// Identical concurrent FEE_RATE reads share one flight, the answer stays
// good for the broker's identical-request cooldown, and the route listing
// outlives it: a later read re-requests the rates but not the listing.
func TestHistoricalFeeRateConcurrentCallersShareOneFlightAndTheCooldown(t *testing.T) {
	c, conn, g := histTFixture(t)
	g.script(histTFeeRateScript(c, conn, []HistoricalBar{histTBar("20260929", 0.5)}))
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var clock sync.Mutex
	c.historicalNow = func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		return now
	}

	results := make(chan error, 3)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			bars, err := c.FetchHistoricalDailyFeeRates(t.Context(), histTFeeContract(), 7, 2*time.Second)
			if err == nil && (len(bars) != 1 || bars[0].Close != 0.5) {
				err = errors.New("wrong bars")
			}
			results <- err
		})
	}
	wg.Wait()
	for range 3 {
		if err := <-results; err != nil {
			t.Fatalf("shared read = %v", err)
		}
	}
	if n := len(g.framesByID(reqHistoricalData)); n != 1 {
		t.Fatalf("history requests = %d, want one shared flight", n)
	}

	if _, err := c.FetchHistoricalDailyFeeRates(t.Context(), histTFeeContract(), 7, time.Second); err != nil {
		t.Fatalf("cooldown read = %v", err)
	}
	if n := len(g.framesByID(reqHistoricalData)); n != 1 {
		t.Fatalf("history requests inside the cooldown = %d, want still one", n)
	}

	clock.Lock()
	now = now.Add(historicalIdenticalRequestCooldown + time.Second)
	clock.Unlock()
	if _, err := c.FetchHistoricalDailyFeeRates(t.Context(), histTFeeContract(), 7, time.Second); err != nil {
		t.Fatalf("read after the cooldown = %v", err)
	}
	if n := len(g.framesByID(reqHistoricalData)); n != 2 {
		t.Fatalf("history requests after the cooldown = %d, want two", n)
	}
	if n := len(g.framesByID(reqContractData)); n != 1 {
		t.Fatalf("route lookups = %d, want the listing kept past the cooldown", n)
	}
}

// Broker refusals come back as connector categories with the code and no
// broker prose: a code 200 on the listing, a code 162 no-data and a 366 on
// the rates themselves.
func TestHistoricalFeeRateBrokerRefusalsAreCategorizedWithoutProse(t *testing.T) {
	type refusal struct {
		name     string
		onRoute  int
		onRates  int
		message  string
		category string
	}
	for _, tc := range []refusal{
		{name: "listing unknown", onRoute: 200, message: "No security definition has been found for the request", category: HistoricalFailureContractUnavailable},
		{name: "no data", onRates: 162, message: "Historical Market Data Service error message:HMDS query returned no data", category: HistoricalFailureNoData},
		{name: "not entitled", onRates: 162, message: "Historical Market Data Service error message:No market data permissions for NASDAQ STK", category: HistoricalFailureNotEntitled},
		{name: "unknown request id", onRates: 366, message: "No historical data query found for ticker id:7", category: HistoricalFailureProtocolRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, conn, g := histTFixture(t)
			binding, _ := c.CaptureSession()
			answerRates := histTFeeRateScript(c, conn, []HistoricalBar{histTBar("20260929", 0.5)})
			g.script(func(fields []string) {
				switch {
				case fields[0] == strconv.Itoa(reqContractData) && tc.onRoute != 0:
					reqID := fields[2]
					go c.handleIBKRErrorFrom(binding, []string{"4", "2", reqID, strconv.Itoa(tc.onRoute), tc.message})
				case fields[0] == strconv.Itoa(reqHistoricalData) && tc.onRates != 0:
					reqID := fields[1]
					go c.handleIBKRErrorFrom(binding, []string{"4", "2", reqID, strconv.Itoa(tc.onRates), tc.message})
				default:
					answerRates(fields)
				}
			})
			_, err := c.FetchHistoricalDailyFeeRates(t.Context(), histTFeeContract(), 7, 2*time.Second)
			got := histTRequestError(t, err, tc.category)
			if want := max(tc.onRoute, tc.onRates); got.Code != want {
				t.Fatalf("code = %d, want %d", got.Code, want)
			}
			if tc.onRoute != 0 && len(g.framesByID(reqHistoricalData)) != 0 {
				t.Fatal("a refused listing still produced a history request")
			}
		})
	}
}

// A listing that names another contract, two listings that disagree on the
// route, and a listing on a venue outside the fee-rate calendar are all
// refused; the route is never guessed from them.
func TestHistoricalFeeRateRefusesListingsThatDoNotProveTheRoute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		details  func(reqID int) [][]string
		category string
	}{
		{name: "other conID", category: HistoricalFailureContractUnavailable, details: func(reqID int) [][]string {
			return [][]string{orderContractDetailsFrame(reqID, 4343, "SYNX", "STK", "SMART", "NASDAQ", "USD", "SYNX", "NMS", 0)}
		}},
		{name: "two routes", category: HistoricalFailureContractUnavailable, details: func(reqID int) [][]string {
			return [][]string{
				orderContractDetailsFrame(reqID, 4242, "SYNX", "STK", "SMART", "NASDAQ", "USD", "SYNX", "NMS", 0),
				orderContractDetailsFrame(reqID, 4242, "SYNX", "STK", "ARCA", "NASDAQ", "USD", "SYNX", "NMS", 0),
			}
		}},
		{name: "no listing", category: HistoricalFailureContractUnavailable, details: func(int) [][]string { return nil }},
		{name: "nine listings", category: HistoricalFailureInvalidPayload, details: func(reqID int) [][]string {
			var many [][]string
			for range 9 {
				many = append(many, orderContractDetailsFrame(reqID, 4242, "SYNX", "STK", "SMART", "NASDAQ", "USD", "SYNX", "NMS", 0))
			}
			return many
		}},
		{name: "foreign venue", category: HistoricalFailureInvalidPayload, details: func(reqID int) [][]string {
			return [][]string{orderContractDetailsFrame(reqID, 4242, "SYNX", "STK", "LSE", "NASDAQ", "USD", "SYNX", "NMS", 0)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, conn, g := histTFixture(t)
			g.script(func(fields []string) {
				if fields[0] != strconv.Itoa(reqContractData) {
					return
				}
				reqID, _ := strconv.Atoi(fields[2])
				histTAnswerRoute(conn, reqID, tc.details(reqID)...)
			})
			_, err := c.FetchHistoricalDailyFeeRates(t.Context(), histTFeeContract(), 7, 2*time.Second)
			histTRequestError(t, err, tc.category)
			if len(g.framesByID(reqHistoricalData)) != 0 {
				t.Fatal("an unproven route still produced a history request")
			}
		})
	}
}

// A listing that arrives after the broker session moved on proves nothing:
// the flight reports the gateway unavailable instead of routing the rates on
// a dead session's answer.
func TestHistoricalFeeRateSessionChangeDuringTheListingRefuses(t *testing.T) {
	c, conn, g := histTFixture(t)
	g.script(func(fields []string) {
		if fields[0] != strconv.Itoa(reqContractData) {
			return
		}
		reqID, _ := strconv.Atoi(fields[2])
		histTAnswerRoute(conn, reqID, orderContractDetailsFrame(reqID, 4242, "SYNX", "STK", "SMART", "NASDAQ", "USD", "SYNX", "NMS", 0))
		conn.brokerSessionEpoch.Add(1)
	})
	_, err := c.FetchHistoricalDailyFeeRates(t.Context(), histTFeeContract(), 7, 2*time.Second)
	histTRequestError(t, err, HistoricalFailureGatewayUnavailable)
	if len(g.framesByID(reqHistoricalData)) != 0 {
		t.Fatal("rates were requested on a retired session")
	}
}

// The caller's budget bounds the wait, not the flight's: a silent gateway
// returns the deadline, and the flight's late failure neither panics nor
// escapes the sanitizer when the next caller reads it inside the cooldown.
func TestHistoricalFeeRateCallerTimeoutAndLateFailure(t *testing.T) {
	c, _, g := histTFixture(t)
	contract := histTFeeContract()
	contract.Exchange = "SMART"
	_, err := c.FetchHistoricalDailyFeeRates(t.Context(), contract, 7, 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("silent gateway = %v, want the caller's deadline", err)
	}
	history := g.waitFramesByID(t, reqHistoricalData, 1)
	if len(g.framesByID(reqContractData)) != 0 {
		t.Fatal("a contract with an executable route still asked for its listing")
	}
	c.failHistoricalRequest(histTReqID(t, history[0], 1), &HistoricalRequestError{Code: 162, Message: "Historical Market Data Service error message:HMDS query returned no data"})

	_, err = c.FetchHistoricalDailyFeeRates(t.Context(), contract, 7, time.Second)
	if got := histTRequestError(t, err, HistoricalFailureNoData); got.Code != 162 {
		t.Fatalf("cached failure code = %d, want 162", got.Code)
	}
	if n := len(g.framesByID(reqHistoricalData)); n != 1 {
		t.Fatalf("history requests = %d, want the failed flight reused inside its cooldown", n)
	}
}

// A FEE_RATE read with no bars is no data, and a disconnected connector
// refuses before the wire.
func TestHistoricalFeeRateEmptyAnswerAndNoSession(t *testing.T) {
	c, conn, g := histTFixture(t)
	g.script(histTFeeRateScript(c, conn, nil))
	_, err := c.FetchHistoricalDailyFeeRates(t.Context(), histTFeeContract(), 7, 2*time.Second)
	histTRequestError(t, err, HistoricalFailureNoData)

	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	_, err = c.FetchHistoricalDailyFeeRates(t.Context(), histTFeeContract(), 7, time.Second)
	histTRequestError(t, err, HistoricalFailureGatewayUnavailable)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.FetchHistoricalDailyFeeRates(ctx, histTFeeContract(), 7, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %v, want context.Canceled", err)
	}
}

// ResolveExactHistoricalStockRoute completes only a missing exchange, only
// from the broker's listing of the exact ConID, and refuses what it cannot
// prove before touching the wire.
func TestResolveExactHistoricalStockRouteCompletesOnlyAMissingExchange(t *testing.T) {
	c, conn, g := histTFixture(t)
	g.script(histTFeeRateScript(c, conn, nil))

	routed := histTFeeContract()
	routed.Exchange = "ARCA"
	got, err := c.ResolveExactHistoricalStockRoute(t.Context(), routed, time.Second)
	if err != nil || got.Exchange != "ARCA" || len(g.framesByID(reqContractData)) != 0 {
		t.Fatalf("routed contract = %+v, %v, lookups=%d; want it returned without a lookup", got, err, len(g.framesByID(reqContractData)))
	}

	got, err = c.ResolveExactHistoricalStockRoute(t.Context(), histTFeeContract(), time.Second)
	if err != nil || got.Exchange != "SMART" || got.LocalSymbol != "SYNX" || got.TradingClass != "NMS" || got.ConID != 4242 {
		t.Fatalf("resolved = %+v, %v", got, err)
	}

	noID := histTFeeContract()
	noID.ConID = 0
	_, err = c.ResolveExactHistoricalStockRoute(t.Context(), noID, time.Second)
	histTRequestError(t, err, HistoricalFailureContractUnavailable)
	foreign := histTFeeContract()
	foreign.Exchange = "LSE"
	_, err = c.ResolveExactHistoricalStockRoute(t.Context(), foreign, time.Second)
	if validation, ok := errors.AsType[*HistoricalDataValidationError](err); !ok || validation.Reason != "unsupported_market_calendar" {
		t.Fatalf("foreign route = %v, want unsupported_market_calendar", err)
	}
	if n := len(g.framesByID(reqContractData)); n != 1 {
		t.Fatalf("lookups = %d, want exactly the one for the missing exchange", n)
	}
}

// histTRoutedScript answers a routed stock discovery (reqContractData with
// conID 0) with one NASDAQ listing and history requests with bars by
// whatToShow; a nil entry for a whatToShow leaves that request unanswered.
func histTRoutedScript(c *Connector, conn *Connection, byWhat map[string][]HistoricalBar) func([]string) {
	return func(fields []string) {
		switch fields[0] {
		case strconv.Itoa(reqContractData):
			reqID, _ := strconv.Atoi(fields[2])
			histTAnswerRoute(conn, reqID, orderContractDetailsFrame(reqID, 4242, "SYNX", "STK", "NASDAQ", "NASDAQ", "USD", "SYNX", "NMS", 0))
		case strconv.Itoa(reqHistoricalData):
			reqID, _ := strconv.Atoi(fields[1])
			if bars, ok := byWhat[fields[19]]; ok {
				histTAnswerHistory(c, reqID, bars)
			}
		}
	}
}

// A routed read without a ConID discovers the stock on its primary venue,
// routes the history request SMART over that primary, and asks for TRADES
// first; the discovery is sent for the venue the caller named, not SMART.
func TestHistoricalRoutedReadDiscoversTheContractThenRequestsTradesOnSMART(t *testing.T) {
	c, conn, g := histTFixture(t)
	g.script(histTRoutedScript(c, conn, map[string][]HistoricalBar{"TRADES": {histTBar("20260929", 101)}}))

	bars, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), Contract{Symbol: "synx", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}, 30, 2*time.Second)
	if err != nil || len(bars) != 1 || bars[0].Close != 101 {
		t.Fatalf("bars = %+v, %v", bars, err)
	}
	discovery := g.framesByID(reqContractData)
	if len(discovery) != 1 {
		t.Fatalf("discoveries = %v, want one", discovery)
	}
	assertFields(t, discovery[0], []fieldAssertion{{3, "0", "discovery conID"}, {4, "SYNX", "discovery symbol"}, {10, "NASDAQ", "discovery exchange is the primary"}, {11, "", "discovery primary blank"}})
	history := g.framesByID(reqHistoricalData)
	if len(history) != 1 {
		t.Fatalf("history requests = %v, want one", history)
	}
	assertFields(t, history[0], []fieldAssertion{
		{2, "4242", "conID from the discovery"}, {9, "SMART", "exchange"}, {10, "NASDAQ", "primary"}, {12, "SYNX", "localSymbol"}, {13, "NMS", "tradingClass"},
		{16, "1 day", "barSize"}, {17, "30 D", "duration"}, {18, "1", "useRTH"}, {19, "TRADES", "whatToShow"}, {20, "1", "formatDate"}, {21, "0", "keepUpToDate"},
	})
}

// A code 200 on the discovery is the broker's verdict: the read fails with
// the definition error, and the next read within the backoff is refused
// before the wire.
func TestHistoricalRoutedReadRecordsADefinitionMissAndSuppressesTheRetry(t *testing.T) {
	c, _, g := histTFixture(t)
	g.script(func(fields []string) {
		if fields[0] != strconv.Itoa(reqContractData) {
			return
		}
		reqID, _ := strconv.Atoi(fields[2])
		go c.failPendingContractDetails(reqID, 200, "No security definition has been found for the request")
	})
	contract := Contract{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
	_, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), contract, 30, 2*time.Second)
	if !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("first read = %v, want the definition verdict", err)
	}
	_, err = c.FetchHistoricalDailyBarsWithContract(t.Context(), contract, 30, 2*time.Second)
	if miss, ok := errors.AsType[*ContractResolutionMissError](err); !ok || miss.Symbol == "" {
		t.Fatalf("second read = %v, want the miss backoff", err)
	}
	if n := len(g.framesByID(reqContractData)); n != 1 {
		t.Fatalf("discoveries = %d, want the second read suppressed", n)
	}
	if len(g.framesByID(reqHistoricalData)) != 0 {
		t.Fatal("history was requested without a contract")
	}
}

// A pacing or no-data 162 on one price basis moves on to the next; a 366 is
// final. The retry never re-discovers the contract.
func TestHistoricalRoutedReadWalksThePriceBasesOn162AndStopsOn366(t *testing.T) {
	c, conn, g := histTFixture(t)
	binding, _ := c.CaptureSession()
	answer := histTRoutedScript(c, conn, map[string][]HistoricalBar{"ADJUSTED_LAST": {histTBar("20260929", 99)}})
	g.script(func(fields []string) {
		if fields[0] == strconv.Itoa(reqHistoricalData) && fields[19] == "TRADES" {
			go c.handleIBKRErrorFrom(binding, []string{"4", "2", fields[1], "162", "Historical Market Data Service error message:HMDS query returned no data"})
			return
		}
		answer(fields)
	})
	bars, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), Contract{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}, 30, 2*time.Second)
	if err != nil || len(bars) != 1 || bars[0].Close != 99 {
		t.Fatalf("bars = %+v, %v", bars, err)
	}
	history := g.framesByID(reqHistoricalData)
	if len(history) != 2 || history[0][19] != "TRADES" || history[1][19] != "ADJUSTED_LAST" || history[0][2] != history[1][2] {
		t.Fatalf("history requests = %v, want TRADES then ADJUSTED_LAST for the same contract", history)
	}
	if n := len(g.framesByID(reqContractData)); n != 1 {
		t.Fatalf("discoveries = %d, want one", n)
	}

	c2, _, g2 := histTFixture(t)
	binding2, _ := c2.CaptureSession()
	g2.script(func(fields []string) {
		if fields[0] == strconv.Itoa(reqHistoricalData) {
			go c2.handleIBKRErrorFrom(binding2, []string{"4", "2", fields[1], "366", "No historical data query found for ticker id:" + fields[1]})
		}
	})
	exact := Contract{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
	_, err = c2.FetchHistoricalDailyTradeBarsWithContract(t.Context(), exact, 30, 2*time.Second)
	reqErr, ok := errors.AsType[*HistoricalRequestError](err)
	if !ok || reqErr.Code != 366 {
		t.Fatalf("366 read = %v, want the broker code", err)
	}
	if n := len(g2.framesByID(reqHistoricalData)); n != 1 {
		t.Fatalf("history requests after a 366 = %d, want no retry", n)
	}
	if _, err := c2.FetchHistoricalDailyTradeBarsWithContract(t.Context(), Contract{Symbol: "SYNX", SecType: "STK", Currency: "USD"}, 30, time.Second); err == nil {
		t.Fatal("TRADES history without a ConID was accepted")
	}
}

// A forced price basis is sent as given and never falls back; an unknown
// basis is refused before the wire; a run of empty answers is no data.
func TestHistoricalForcedPriceBasisIsSentOnceAndEmptyAnswersAreNoData(t *testing.T) {
	c, conn, g := histTFixture(t)
	c.contractCache["SYNX"] = ContractDetailsLite{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD", LocalSymbol: "SYNX", TradingClass: "NMS"}
	g.script(histTRoutedScript(c, conn, map[string][]HistoricalBar{"MIDPOINT": nil}))

	_, err := c.FetchHistoricalDailyBarsWhatToShow(t.Context(), "synx", 30, "midpoint", 2*time.Second)
	if err == nil || len(g.framesByID(reqHistoricalData)) != 1 {
		t.Fatalf("empty forced read = %v with %d requests, want one request and an unavailable error", err, len(g.framesByID(reqHistoricalData)))
	}
	history := g.framesByID(reqHistoricalData)
	assertFields(t, history[0], []fieldAssertion{{2, "4242", "conID from the cache"}, {19, "MIDPOINT", "forced basis"}})
	if len(g.framesByID(reqContractData)) != 0 {
		t.Fatal("a cached contract was discovered again")
	}

	if _, err := c.FetchHistoricalDailyBarsWhatToShow(t.Context(), "SYNX", 30, "FEE_RATE", time.Second); err == nil {
		t.Fatal("FEE_RATE was accepted as a daily price basis")
	}
	if n := len(g.framesByID(reqHistoricalData)); n != 1 {
		t.Fatalf("requests = %d, want the refused basis kept off the wire", n)
	}
}

// A history request the gateway never answers is cancelled on the wire when
// the caller's budget or context ends, and the error names which.
func TestHistoricalUnansweredRequestIsCancelledOnTimeoutAndOnContextEnd(t *testing.T) {
	c, _, g := histTFixture(t)
	exact := Contract{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}

	_, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), exact, 30, 150*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout read = %v, want the deadline", err)
	}
	history := g.waitFramesByID(t, reqHistoricalData, 1)
	cancels := g.waitFramesByID(t, cancelHistoricalData, 1)
	if cancels[0][2] != history[0][1] {
		t.Fatalf("cancel %v does not name request %v", cancels[0], history[0])
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := c.FetchHistoricalDailyBarsWithContract(ctx, exact, 30, 5*time.Second)
		done <- err
	}()
	history = g.waitFramesByID(t, reqHistoricalData, 2)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %v, want context.Canceled", err)
	}
	cancels = g.waitFramesByID(t, cancelHistoricalData, 2)
	if cancels[1][2] != history[1][1] {
		t.Fatalf("cancel %v does not name request %v", cancels[1], history[1])
	}
}

// The routed and symbol reads refuse before the wire when the connector is
// not ready, the contract has no symbol, or the route is marked inactive.
func TestHistoricalReadsRefuseBeforeTheWire(t *testing.T) {
	c, _, g := histTFixture(t)
	exact := Contract{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), Contract{SecType: "STK"}, 30, time.Second); err == nil {
		t.Fatal("a contract without a symbol was accepted")
	}
	if post := c.markSymbolInactivePostAction(MarketDataKeyForContract(normalizeMarketDataContract(exact)), "delisted"); post != nil {
		post()
	}
	if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), exact, 30, time.Second); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("inactive route = %v, want ErrSymbolInactive", err)
	}
	if post := c.markSymbolInactivePostAction("ZETA", "delisted"); post != nil {
		post()
	}
	if _, err := c.FetchHistoricalDailyBars(t.Context(), "zeta", 30, time.Second); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("inactive symbol = %v, want ErrSymbolInactive", err)
	}
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), exact, 30, time.Second); err == nil {
		t.Fatal("a routed read on a connector that is not ready was accepted")
	}
	if _, err := c.FetchHistoricalDailyBars(t.Context(), "SYNX", 30, time.Second); err == nil {
		t.Fatal("a symbol read on a connector that is not ready was accepted")
	}
	if len(g.frames) != 0 {
		t.Fatalf("refused reads wrote %v", g.frames)
	}
}

// ContractDetailsFirst returns the first listing the gateway sends for the
// contract as asked, reports a silent gateway as a timeout, and refuses
// without a connection.
func TestContractDetailsFirstReturnsTheFirstListing(t *testing.T) {
	c, conn, g := histTFixture(t)
	g.script(func(fields []string) {
		if fields[0] != strconv.Itoa(reqContractData) {
			return
		}
		reqID, _ := strconv.Atoi(fields[2])
		histTAnswerRoute(conn, reqID,
			orderContractDetailsFrame(reqID, 4242, "SYNX", "STK", "SMART", "NASDAQ", "USD", "SYNX", "NMS", 0),
			orderContractDetailsFrame(reqID, 4242, "SYNX", "STK", "ARCA", "NASDAQ", "USD", "SYNX", "NMS", 0))
	})
	detail, err := c.ContractDetailsFirst(t.Context(), Contract{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}, time.Second)
	if err != nil || detail == nil || detail.ConID != 4242 || detail.Exchange != "SMART" {
		t.Fatalf("first listing = %+v, %v", detail, err)
	}
	lookups := g.framesByID(reqContractData)
	if len(lookups) != 1 {
		t.Fatalf("lookups = %v, want one", lookups)
	}
	assertFields(t, lookups[0], []fieldAssertion{{4, "SYNX", "symbol"}, {5, "STK", "secType"}, {10, "SMART", "exchange"}, {12, "USD", "currency"}})

	g.script(nil)
	if _, err := c.ContractDetailsFirst(t.Context(), Contract{Symbol: "ZETA", SecType: "STK", Exchange: "SMART", Currency: "USD"}, 50*time.Millisecond); err == nil {
		t.Fatal("a silent gateway returned a listing")
	}
	conn.status = StatusDisconnected
	if _, err := c.ContractDetailsFirst(t.Context(), Contract{Symbol: "ZETA"}, time.Second); err == nil {
		t.Fatal("a disconnected connector returned a listing")
	}
}

// histTChartScript answers the exact contract resolution with one listing
// and every history request with the given bars.
func histTChartScript(c *Connector, conn *Connection, secType string, bars []HistoricalBar) func([]string) {
	return func(fields []string) {
		switch fields[0] {
		case strconv.Itoa(reqContractData):
			reqID, _ := strconv.Atoi(fields[2])
			histTAnswerRoute(conn, reqID, orderContractDetailsFrame(reqID, 4242, fields[4], secType, fields[10], "NASDAQ", fields[12], fields[4], "NMS", 0))
		case strconv.Itoa(reqHistoricalData):
			reqID, _ := strconv.Atoi(fields[1])
			histTAnswerHistory(c, reqID, bars)
		}
	}
}

// A chart read resolves the exact contract on the session first, then asks
// for bounded bars at the requested interval: intraday bars outside regular
// hours with epoch timestamps, daily bars inside them, and MIDPOINT for FX.
func TestFetchChartBarsResolvesTheContractThenRequestsBoundedBars(t *testing.T) {
	c, conn, g := histTFixture(t)
	intraday := []HistoricalBar{histTBar("1759300200", 100), histTBar("1759300500", 101)}
	g.script(histTChartScript(c, conn, "STK", intraday))

	series, err := c.FetchChartBars(t.Context(), Contract{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}, 2, "5 mins", 2*time.Second)
	if err != nil || series.Contract.ConID != 4242 || series.WhatToShow != "TRADES" || len(series.Bars) != 2 {
		t.Fatalf("series = %+v, %v", series, err)
	}
	if series.Bars[1].Time.Unix() != 1759300500 {
		t.Fatalf("bar time = %v, want the epoch timestamp", series.Bars[1].Time)
	}
	history := g.framesByID(reqHistoricalData)
	if len(history) != 1 {
		t.Fatalf("history requests = %v, want one", history)
	}
	assertFields(t, history[0], []fieldAssertion{{2, "4242", "conID"}, {9, "SMART", "exchange"}, {16, "5 mins", "barSize"}, {17, "2 D", "duration"}, {18, "0", "useRTH outside regular hours"}, {19, "TRADES", "whatToShow"}, {20, "2", "formatDate"}})
	if n := len(g.framesByID(reqContractData)); n != 1 {
		t.Fatalf("resolutions = %d, want one", n)
	}

	g.script(histTChartScript(c, conn, "STK", []HistoricalBar{histTBar("20260929", 100)}))
	series, err = c.FetchChartBars(t.Context(), Contract{Symbol: "SYNX", SecType: "ETF", Exchange: "SMART", Currency: "USD"}, 30, "1 day", 2*time.Second)
	if err != nil || len(series.Bars) != 1 || series.Contract.SecType != "STK" {
		t.Fatalf("daily series = %+v, %v", series, err)
	}
	history = g.framesByID(reqHistoricalData)
	assertFields(t, history[1], []fieldAssertion{{16, "1 day", "barSize"}, {17, "30 D", "duration"}, {18, "1", "useRTH inside regular hours"}})

	g.script(histTChartScript(c, conn, "CASH", []HistoricalBar{histTBar("20260929", 1.1)}))
	series, err = c.FetchChartBars(t.Context(), Contract{Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD"}, 5, "1 day", 2*time.Second)
	if err != nil || series.WhatToShow != "MIDPOINT" || len(series.Bars) != 1 {
		t.Fatalf("fx series = %+v, %v", series, err)
	}
	if history = g.framesByID(reqHistoricalData); history[2][19] != "MIDPOINT" {
		t.Fatalf("fx request = %v, want MIDPOINT", history[2])
	}
	g.script(histTChartScript(c, conn, "STK", nil))
	if classification, err := c.MarketClassification(t.Context(), Contract{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}, time.Second); err != nil || classification != (MarketClassification{}) {
		t.Fatalf("classification = %+v, %v; want the listing's empty classification", classification, err)
	}
}

// A chart read refuses an unsupported range or interval and a missing
// session before the wire, and refuses a series the gateway announces beyond
// the chart bound instead of reading it.
func TestFetchChartBarsRefusesBadRangesAndOversizedSeries(t *testing.T) {
	c, conn, g := histTFixture(t)
	contract := Contract{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	for _, tc := range []struct {
		days     int
		interval string
	}{{0, "1 day"}, {1831, "1 day"}, {8, "5 mins"}, {3, "1 min"}, {3, "30 mins"}} {
		if tc.interval == "30 mins" {
			tc.days = 8
		}
		if _, err := c.FetchChartBars(t.Context(), contract, tc.days, tc.interval, time.Second); err == nil {
			t.Fatalf("range %d/%s was accepted", tc.days, tc.interval)
		}
	}
	if len(g.frames) != 0 {
		t.Fatalf("refused ranges wrote %v", g.frames)
	}

	g.script(func(fields []string) {
		if fields[0] == strconv.Itoa(reqContractData) {
			reqID, _ := strconv.Atoi(fields[2])
			go c.failPendingContractDetails(reqID, 200, "No security definition has been found for the request")
		}
	})
	if _, err := c.FetchChartBars(t.Context(), contract, 7, "1 day", 2*time.Second); err == nil {
		t.Fatal("a chart for a contract the broker does not know was read")
	}
	if len(g.framesByID(reqHistoricalData)) != 0 {
		t.Fatal("history was requested for an unresolved contract")
	}

	g.script(func(fields []string) {
		switch fields[0] {
		case strconv.Itoa(reqContractData):
			reqID, _ := strconv.Atoi(fields[2])
			histTAnswerRoute(conn, reqID, orderContractDetailsFrame(reqID, 4242, "SYNX", "STK", "SMART", "NASDAQ", "USD", "SYNX", "NMS", 0))
		case strconv.Itoa(reqHistoricalData):
			reqID, _ := strconv.Atoi(fields[1])
			c.handleHistoricalData([]string{strconv.Itoa(msgHistoricalData), strconv.Itoa(reqID), strconv.Itoa(ChartMaxBars + 1), ""})
		}
	})
	_, err := c.FetchChartBars(t.Context(), contract, 7, "30 mins", 2*time.Second)
	if validation, ok := errors.AsType[*HistoricalDataValidationError](err); !ok || validation.Reason != "series_size_limit" {
		t.Fatalf("oversized series = %v, want series_size_limit", err)
	}

	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	if _, err := c.FetchChartBars(t.Context(), contract, 7, "1 day", time.Second); err == nil {
		t.Fatal("a chart read without a session was accepted")
	}
	if _, err := c.MarketClassification(t.Context(), contract, time.Second); err == nil {
		t.Fatal("a classification without a session was accepted")
	}
}

// The FEE_RATE encoder refuses a retired socket generation, an unnegotiated
// or old server and a venue outside the fee-rate calendar before the wire;
// a contract with a security id carries it in the IBKR field order, and the
// epoch-bound cancel names the request.
func TestHistoricalFeeRateEncoderRefusalsAndSecurityIDFields(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	conn.observeNextValidOrderID(100)
	epoch := conn.BrokerSessionEpoch()
	contract := Contract{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD", SecIDType: "ISIN", SecID: "US0000000000"}

	_, err := conn.requestHistoricalDailyFeeRateForEpoch(t.Context(), contract, 7, epoch+1, nil)
	histTRequestError(t, err, HistoricalFailureGatewayUnavailable)
	foreign := contract
	foreign.Exchange = "LSE"
	if _, err := conn.requestHistoricalDailyFeeRateForEpoch(t.Context(), foreign, 7, epoch, nil); err == nil {
		t.Fatal("a foreign venue was encoded as FEE_RATE")
	}
	negotiated := conn.serverVersion
	conn.serverVersion = minServerVersionRequired - 1
	if _, err := conn.requestHistoricalDailyFeeRateForEpoch(t.Context(), contract, 7, epoch, nil); err == nil {
		t.Fatal("an old server was asked for FEE_RATE")
	}
	conn.serverVersion = negotiated
	if out.Len() != 0 {
		t.Fatal("a refused request reached the socket")
	}

	registered := 0
	reqID, err := conn.requestHistoricalDailyFeeRateForEpoch(t.Context(), contract, 400, epoch, func(id int) { registered = id })
	if err != nil || registered != reqID || reqID <= 0 {
		t.Fatalf("reqID=%d registered=%d err=%v", reqID, registered, err)
	}
	if err := conn.cancelHistoricalDataForEpoch(t.Context(), 0, epoch); err != nil {
		t.Fatalf("cancel of nothing = %v", err)
	}
	if err := conn.cancelHistoricalDataForEpoch(t.Context(), reqID, epoch+1); err == nil {
		t.Fatal("a cancel on a retired generation was sent")
	}
	if err := conn.cancelHistoricalDataForEpoch(t.Context(), reqID, epoch); err != nil {
		t.Fatalf("cancel = %v", err)
	}
	frames := decodeOutboundFrames(t, conn, out.Bytes())
	if len(frames) != 2 {
		t.Fatalf("frames = %v, want the request and its cancel", frames)
	}
	assertFields(t, frames[0], []fieldAssertion{
		{0, "20", "reqHistoricalData"}, {1, strconv.Itoa(reqID), "reqId"}, {2, "4242", "conID"}, {14, "0", "includeExpired"},
		{15, "ISIN", "secIdType"}, {16, "US0000000000", "secId"}, {17, "", "endDateTime"}, {18, "1 day", "barSize"},
		{19, "2 Y", "duration for 400 days"}, {20, "1", "useRTH"}, {21, "FEE_RATE", "whatToShow"}, {22, "2", "formatDate"}, {23, "0", "keepUpToDate"}, {24, "", "chartOptions"},
	})
	assertFields(t, frames[1], []fieldAssertion{{0, "25", "cancelHistoricalData"}, {1, "1", "version"}, {2, strconv.Itoa(reqID), "reqId"}})
}

// histTOptionDetailFrame is one option listing of the SYNTH 2099-10-16 100 call.
func histTOptionDetailFrame(reqID, conID int, exchange, tradingClass string) []string {
	f := make([]string, 29)
	f[0] = strconv.Itoa(msgContractData)
	f[1] = strconv.Itoa(reqID)
	f[2] = "SYNTH"
	f[3] = "OPT"
	f[4] = "20991016"
	f[6] = "100"
	f[7] = "C"
	f[8] = exchange
	f[9] = "USD"
	f[10] = "SYNTH 991016C00100000"
	f[12] = tradingClass
	f[13] = strconv.Itoa(conID)
	f[15] = "100"
	f[21] = exchange
	return f
}

// Among several listings of one option, the one on the requested venue is
// chosen and a listing of another trading class never is; no acceptable
// listing is a plain unavailability, silence a timeout.
func TestFetchOptionContractDetailPrefersTheRequestedRoute(t *testing.T) {
	_, conn, g := histTFixture(t)
	contract := prewarmTestContract()
	g.script(func(fields []string) {
		if fields[0] != strconv.Itoa(reqContractData) {
			return
		}
		reqID, _ := strconv.Atoi(fields[2])
		histTAnswerRoute(conn, reqID,
			histTOptionDetailFrame(reqID, 1, "SMART", "SYNTH"),
			histTOptionDetailFrame(reqID, 2, "CBOE", "SYNTH"),
			histTOptionDetailFrame(reqID, 3, "CBOE", "SYNTHW"))
	})
	detail, err := conn.fetchOptionContractDetail(t.Context(), contract, time.Second)
	if err != nil || detail == nil || detail.ConID != 2 {
		t.Fatalf("detail = %+v, %v; want the CBOE listing of the requested class", detail, err)
	}
	lookups := g.framesByID(reqContractData)
	if len(lookups) != 1 {
		t.Fatalf("lookups = %v, want one", lookups)
	}
	assertFields(t, lookups[0], []fieldAssertion{{4, "SYNTH", "symbol"}, {5, "OPT", "secType"}, {6, "20991016", "expiry"}, {9, "100", "multiplier"}, {10, "CBOE", "exchange"}, {14, "SYNTH", "tradingClass"}})

	g.script(func(fields []string) {
		if fields[0] == strconv.Itoa(reqContractData) {
			reqID, _ := strconv.Atoi(fields[2])
			histTAnswerRoute(conn, reqID, histTOptionDetailFrame(reqID, 3, "CBOE", "SYNTHW"))
		}
	})
	if _, err := conn.fetchOptionContractDetail(t.Context(), contract, time.Second); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("other-class listing = %v, want unavailable", err)
	}
	g.script(nil)
	if _, err := conn.fetchOptionContractDetail(t.Context(), contract, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("silence = %v, want a timeout", err)
	}
}
