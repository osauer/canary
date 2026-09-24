package ibkr

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestChartChunksKeepExactTimesUntilEnd(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	t.Cleanup(func() { conn.rateLimiter.Stop() })
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	c.conn, c.running, c.ready = conn, true, true
	req := c.createHistoricalRequestWithOptions(7109, "SYNTH", historicalRequestOptions{strictDaily: true, waitForEnd: true, formatDate: 2, chartBarSize: "5 mins"})
	for _, at := range []string{"1788958800", "1788959100"} {
		c.handleHistoricalData([]string{strconv.Itoa(msgHistoricalData), "7109", "1", at, "10", "12", "9", "11", "100", "10.5", "4", ""})
		select {
		case <-req.result:
			t.Fatal("chart completed before its end receipt")
		default:
		}
	}
	c.handleHistoricalDataEnd([]string{strconv.Itoa(msgHistoricalDataEnd), "7109", "1788958800", "1788959100", ""})
	got := <-req.result
	if got.err != nil || len(got.bars) != 2 || got.bars[1].Time.Unix() != 1788959100 {
		t.Fatalf("incomplete or misdated chart: %+v", got)
	}
}
func TestChartResolutionCannotAdoptAnotherSameSymbolContract(t *testing.T) {
	requested := Contract{ConID: 91, Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	for _, d := range []ContractDetailsLite{{ConID: 92, Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", Currency: "USD"}, {ConID: 91, Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", Currency: "EUR"}} {
		if _, err := exactOrderContract(requested, []ContractDetailsLite{d}); err == nil {
			t.Fatal("different identity accepted")
		}
	}
}

func TestHistoricalPayloadRejectsExtraFields(t *testing.T) {
	for _, trailing := range [][]string{{"extra"}, {"", ""}} {
		if historicalPayloadConsumed(trailing, 0) {
			t.Fatal("corrupt trailing fields accepted")
		}
	}
}

func TestFrontFutureUsesBrokerExpiryAndRejectsAmbiguity(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	want := Contract{Symbol: "SYNTH", SecType: "FUT", Currency: "USD", Exchange: "CME"}
	d := ContractDetailsLite{ConID: 1, Symbol: "SYNTH", TradingClass: "SYNTH", SecType: "FUT", Currency: "USD", Exchange: "CME", Expiry: "20260918"}
	later := d
	later.ConID = 2
	later.Expiry = "20261218"
	got, err := selectFrontFuture(want, []ContractDetailsLite{later, d}, now)
	if err != nil || got.ConID != 1 || got.Expiry != "20260918" {
		t.Fatal("wrong front contract", err)
	}
	later.Expiry = d.Expiry
	if _, err = selectFrontFuture(want, []ContractDetailsLite{later, d}, now); err == nil {
		t.Fatal("ambiguous future accepted")
	}
}

func TestHistoricalCountCannotAllocateBeyondPayload(t *testing.T) {
	for _, strict := range []bool{false, true} {
		idx := 0
		if bars, err := parseHistoricalBars([]string{""}, &idx, int(^uint(0)>>1), strict); err == nil || len(bars) != 0 {
			t.Fatal("untrusted count accepted")
		}
	}
}

func TestChartAcquisitionSizeLimit(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		c := NewConnector(&ConnectorConfig{})
		req := c.createHistoricalRequestWithOptions(7109, "SYNTH", historicalRequestOptions{strictDaily: true, waitForEnd: true, chartBarSize: "5 mins", maxBars: ChartMaxBars})
		fields := func(start, count int) []string {
			f := []string{strconv.Itoa(msgHistoricalData), "7109", strconv.Itoa(count)}
			for i := range count {
				f = append(f, strconv.FormatInt(1788958800+int64(start+i)*300, 10), "10", "12", "9", "11", "100", "10.5", "4")
			}
			return append(f, "")
		}
		if chunked {
			c.handleHistoricalData(fields(0, ChartMaxBars))
			select {
			case <-req.result:
				t.Fatal("exact-limit series rejected or completed before end")
			default:
			}
			c.handleHistoricalData(fields(ChartMaxBars, 1))
		} else {
			c.handleHistoricalData(fields(0, ChartMaxBars+1))
		}
		select {
		case got := <-req.result:
			validation, ok := got.err.(*HistoricalDataValidationError)
			if !ok || validation.Reason != "series_size_limit" || len(got.bars) != 0 {
				t.Fatalf("oversized history not rejected: err=%v bars=%d", got.err, len(got.bars))
			}
		default:
			t.Fatal("oversized history left pending")
		}
		if c.getHistoricalRequest(7109) != nil {
			t.Fatal("failed request retained")
		}
	}
}

// TestIndexDailyHistoryAsksTradesFirst witnesses the index daily-bar read that
// opened with MIDPOINT: an index quotes no bid/ask, so IBKR answered each such
// request with 162 and every read spent a second paced request on TRADES.
func TestIndexDailyHistoryAsksTradesFirst(t *testing.T) {
	c, conn, out, _ := newMissTestConnector(t)
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	contract := Contract{ConID: 90123, Symbol: "SYNTH", SecType: "IND", Exchange: "NASDAQ", Currency: "USD"}
	pending := func() int {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			c.historicalMu.Lock()
			for id := range c.historicalReqs {
				c.historicalMu.Unlock()
				return id
			}
			c.historicalMu.Unlock()
		}
		t.Fatal("no historical request was issued")
		return 0
	}
	// read refuses the first refusals requests of one daily read with code 162,
	// answers the next with a bar, and returns the whatToShow of each request.
	read := func(refusals int) []string {
		t.Helper()
		before := len(decodeOutboundFrames(t, conn, out.Bytes()))
		done := make(chan error, 1)
		go func() {
			bars, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), contract, 10, 5*time.Second)
			if err == nil && len(bars) != 1 {
				err = errors.New("daily read lost its bar")
			}
			done <- err
		}()
		for range refusals {
			c.failPendingHistorical(pending(), 162, "synthetic no historical data")
		}
		c.handleHistoricalData([]string{strconv.Itoa(msgHistoricalData), strconv.Itoa(pending()), "1", "20260923", "10", "12", "9", "11", "100", "10.5", "4"})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		var asked []string
		for _, frame := range decodeOutboundFrames(t, conn, out.Bytes())[before:] {
			if frame[0] != strconv.Itoa(reqHistoricalData) {
				continue
			}
			for _, what := range []string{"TRADES", "MIDPOINT", "ADJUSTED_LAST"} {
				if slices.Contains(frame, what) {
					asked = append(asked, what)
				}
			}
		}
		return asked
	}
	if asked := read(0); !slices.Equal(asked, []string{"TRADES"}) {
		t.Fatalf("index daily history asked for %v, want one TRADES request", asked)
	}
	if asked := read(1); !slices.Equal(asked, []string{"TRADES", "MIDPOINT"}) {
		t.Fatalf("refused index TRADES history asked for %v, want the MIDPOINT alternate", asked)
	}
}
