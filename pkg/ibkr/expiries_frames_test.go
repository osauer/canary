package ibkr

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// histTSecDefFrame is one securityDefinitionOptionParameter (msg 75) frame:
// reqId, exchange, underlying conId, trading class, multiplier, then the
// counted expirations and the counted strikes.
func histTSecDefFrame(reqID int, exchange, tradingClass string, expirations, strikes []string) []byte {
	fields := []string{strconv.Itoa(reqID), exchange, "4242", tradingClass, "100", strconv.Itoa(len(expirations))}
	fields = append(fields, expirations...)
	fields = append(fields, strconv.Itoa(len(strikes)))
	fields = append(fields, strikes...)
	return twsGatewayFrame(msgSecurityDefinitionOptionalParameter, fields...)
}

func histTExpiryFixture(t *testing.T) (*Connector, *Connection, *histTGateway) {
	t.Helper()
	c, conn, g := histTFixture(t)
	c.contractCache["SYNX"] = ContractDetailsLite{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD", LocalSymbol: "SYNX", TradingClass: "NMS"}
	return c, conn, g
}

// histTAnswerExpiries waits for the next reqSecDefOptParams after the given
// request id and answers it with two exchanges whose expirations overlap,
// a frame for another request, a malformed frame, a frame with an expiry
// the broker could not have sent, and a doubled end marker.
func histTAnswerExpiries(t *testing.T, conn *Connection, after int) int {
	t.Helper()
	reqID := waitForHandlerReqIDAfter(t, conn, msgSecurityDefinitionOptionalParameter, after)
	conn.processMessage(histTSecDefFrame(reqID, "SMART", "SYNX", []string{"20261218", "20270115"}, []string{"100", "105", "not-a-strike"}))
	conn.processMessage(histTSecDefFrame(reqID+1000, "SMART", "SYNX", []string{"20300101"}, []string{"1"}))
	conn.processMessage(twsGatewayFrame(msgSecurityDefinitionOptionalParameter, strconv.Itoa(reqID), "CBOE", "4242", "SYNX", "100", "5", "20261218"))
	conn.processMessage(histTSecDefFrame(reqID, "CBOE", "SYNXW", []string{"20261218", "20261224", "2026-12", "2026121X"}, []string{"105", "110"}))
	conn.processMessage(twsGatewayFrame(msgSecurityDefinitionOptionalParameterEnd, strconv.Itoa(reqID)))
	conn.processMessage(twsGatewayFrame(msgSecurityDefinitionOptionalParameterEnd, strconv.Itoa(reqID)))
	return reqID
}

// One reqSecDefOptParams per read, keyed to the underlying's ConID; the
// answers of every exchange merge into one sorted expiry list, one merged
// strike grid per expiry, and per-class grids that keep SYNX and SYNXW apart.
func TestFetchOptionExpiriesMergesExchangesAndKeepsTradingClassesApart(t *testing.T) {
	c, conn, g := histTExpiryFixture(t)

	type classedResult struct {
		classed map[string][]ExpiryClassedStrikes
		err     error
	}
	classedCh := make(chan classedResult, 1)
	go func() {
		classed, err := c.FetchOptionExpiryStrikesClassed("synx", 2*time.Second)
		classedCh <- classedResult{classed, err}
	}()
	reqID := histTAnswerExpiries(t, conn, 0)
	requests := g.framesByID(reqSecDefOptParams)
	if len(requests) != 1 {
		t.Fatalf("requests = %v, want one", requests)
	}
	assertFields(t, requests[0], []fieldAssertion{{1, strconv.Itoa(reqID), "reqId"}, {2, "SYNX", "underlying symbol"}, {3, "", "futFopExchange"}, {4, "STK", "underlying secType"}, {5, "4242", "underlying conId"}})
	got := <-classedCh
	if got.err != nil {
		t.Fatalf("classed = %v", got.err)
	}
	wantClassed := map[string][]ExpiryClassedStrikes{
		"2026-12-18": {{TradingClass: "SYNX", Strikes: []float64{100, 105}}, {TradingClass: "SYNXW", Strikes: []float64{105, 110}}},
		"2026-12-24": {{TradingClass: "SYNXW", Strikes: []float64{105, 110}}},
		"2027-01-15": {{TradingClass: "SYNX", Strikes: []float64{100, 105}}},
	}
	if !reflect.DeepEqual(got.classed, wantClassed) {
		t.Fatalf("classed = %+v, want %+v", got.classed, wantClassed)
	}

	expiriesCh := make(chan []string, 1)
	go func() {
		expiries, err := c.FetchOptionExpiries("SYNX", 2*time.Second)
		if err != nil {
			t.Errorf("expiries = %v", err)
		}
		expiriesCh <- expiries
	}()
	reqID = histTAnswerExpiries(t, conn, reqID)
	if expiries := <-expiriesCh; !reflect.DeepEqual(expiries, []string{"2026-12-18", "2026-12-24", "2027-01-15"}) {
		t.Fatalf("expiries = %v", expiries)
	}

	strikesCh := make(chan map[string][]float64, 1)
	go func() {
		strikes, err := c.FetchOptionExpiryStrikes("SYNX", 2*time.Second)
		if err != nil {
			t.Errorf("strikes = %v", err)
		}
		strikesCh <- strikes
	}()
	histTAnswerExpiries(t, conn, reqID)
	wantStrikes := map[string][]float64{"2026-12-18": {100, 105, 110}, "2026-12-24": {105, 110}, "2027-01-15": {100, 105}}
	if strikes := <-strikesCh; !reflect.DeepEqual(strikes, wantStrikes) {
		t.Fatalf("strikes = %v, want %v", strikes, wantStrikes)
	}
	if n := len(g.framesByID(reqSecDefOptParams)); n != 3 {
		t.Fatalf("requests = %d, want one per read", n)
	}
}

// Without the end marker, what the exchanges did send is still the answer
// after the budget; nothing at all is a timeout error.
func TestFetchOptionExpiriesPartialAnswerSurvivesTheTimeoutAndSilenceDoesNot(t *testing.T) {
	c, conn, _ := histTExpiryFixture(t)
	done := make(chan []string, 1)
	go func() {
		expiries, err := c.FetchOptionExpiries("SYNX", 300*time.Millisecond)
		if err != nil {
			t.Errorf("partial answer = %v, want the expiries seen so far", err)
		}
		done <- expiries
	}()
	reqID := waitForHandlerReqID(t, conn, msgSecurityDefinitionOptionalParameter)
	conn.processMessage(histTSecDefFrame(reqID, "SMART", "SYNX", []string{"20261218"}, []string{"100"}))
	if expiries := <-done; !reflect.DeepEqual(expiries, []string{"2026-12-18"}) {
		t.Fatalf("partial expiries = %v", expiries)
	}

	start := time.Now()
	_, err := c.FetchOptionExpiries("SYNX", 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("silent gateway = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("silent read waited %s past its budget", elapsed)
	}
}

// Refusals that need no gateway stay off the wire: no session, no symbol,
// an inactive symbol, and an underlying the broker never identified. A
// listing that lands during the grace window after a failed lookup is used.
func TestFetchOptionExpiriesRefusesBeforeTheWire(t *testing.T) {
	c, conn, g := histTFixture(t)
	if _, err := c.FetchOptionExpiries("  ", time.Second); err == nil {
		t.Fatal("a blank symbol was accepted")
	}
	if post := c.markSymbolInactivePostAction("ZETA", "delisted"); post != nil {
		post()
	}
	if _, err := c.FetchOptionExpiries("zeta", time.Second); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("inactive symbol = %v, want ErrSymbolInactive", err)
	}
	lookupErr := errors.New("synthetic lookup failure")
	c.fetchContractDetails = func(string, time.Duration) ([]ContractDetailsLite, error) { return nil, lookupErr }
	if _, err := c.FetchOptionExpiries("SYNX", 200*time.Millisecond); !errors.Is(err, lookupErr) {
		t.Fatalf("unidentified underlying = %v, want the lookup failure", err)
	}
	if len(g.frames) != 0 {
		t.Fatalf("refused reads wrote %v", g.frames)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		c.SeedContractDetails("SYNX", ContractDetailsLite{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"})
	}()
	done := make(chan error, 1)
	go func() {
		_, err := c.FetchOptionExpiries("SYNX", 2*time.Second)
		done <- err
	}()
	reqID := waitForHandlerReqID(t, conn, msgSecurityDefinitionOptionalParameter)
	conn.processMessage(histTSecDefFrame(reqID, "SMART", "SYNX", []string{"20261218"}, []string{"100"}))
	conn.processMessage(twsGatewayFrame(msgSecurityDefinitionOptionalParameterEnd, strconv.Itoa(reqID)))
	if err := <-done; err != nil {
		t.Fatalf("read after a late listing = %v", err)
	}
	if n := len(g.framesByID(reqSecDefOptParams)); n != 1 {
		t.Fatalf("requests = %d, want the one sent after the listing landed", n)
	}

	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	if _, err := c.FetchOptionExpiries("SYNX", time.Second); !errors.Is(err, ErrIBKRUnavailable) {
		t.Fatalf("not ready = %v, want ErrIBKRUnavailable", err)
	}
}
