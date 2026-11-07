package ibkr

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// listingDetailsFrame is a full contract-details frame, decoded by the same
// parser the gateway's answers go through, for a stock whose broker-reported
// routes are validExchanges. The symbols and contract ids are invented.
func listingDetailsFrame(t *testing.T, conn *Connection, reqID, conID int, symbol, exchange, primary, currency, validExchanges string) []string {
	t.Helper()
	f := syntheticStockContractDetailsFields(conn.serverVersion, "COMMON")
	f[1], f[2], f[8], f[9], f[10], f[12] = strconv.Itoa(reqID), symbol, exchange, currency, symbol, symbol
	f[13], f[17], f[21] = strconv.Itoa(conID), validExchanges, primary
	detail, ok := parseContractDetailsLite(f, reqID, conn.serverVersion)
	if !ok || detail.ValidExchanges != validExchanges || detail.Exchange != exchange || detail.PrimaryExch != primary {
		t.Fatalf("listing frame does not decode as written: %+v (ok=%v)", detail, ok)
	}
	return f
}

// listingBroker answers like the gateway did on 2026-11-07 for a listing that
// SMART does not reach: details and history on the listing's own exchange
// succeed, and either request on SMART draws a code 200. When smart is true
// the listing is reachable both ways, as a Tokyo or US listing is.
func listingBroker(t *testing.T, c *Connector, conn *Connection, symbol, venue, currency, validExchanges string, smart bool) func([]string) {
	t.Helper()
	binding, _ := c.CaptureSession()
	const conID = 4243
	return func(fields []string) {
		switch fields[0] {
		case strconv.Itoa(reqContractData):
			reqID, _ := strconv.Atoi(fields[2])
			route := fields[10]
			if route == "SMART" && !smart {
				go c.failPendingContractDetails(reqID, 200, "No security definition has been found for the request")
				return
			}
			histTAnswerRoute(conn, reqID, listingDetailsFrame(t, conn, reqID, conID, symbol, route, venue, currency, validExchanges))
		case strconv.Itoa(reqHistoricalData):
			reqID, _ := strconv.Atoi(fields[1])
			if fields[9] == "SMART" && !smart {
				go c.handleIBKRErrorFrom(binding, []string{"4", "2", fields[1], "200", "No security definition has been found for the request"})
				return
			}
			histTAnswerHistory(c, reqID, []HistoricalBar{histTBar("20260929", 101)})
		}
	}
}

// IBKR refuses a Hong Kong or Singapore listing on SMART with code 200, and
// its contract details name only the listing's own exchange. A read that named
// that exchange must stay on it; turning it into SMART refused every daily
// read of SEHK and SGX from v3.33.0 until 2026-11-07.
func TestHistoryStaysOnTheExchangeWhenTheBrokerDoesNotListSMART(t *testing.T) {
	for _, tc := range []struct{ name, venue, currency string }{
		{"Hong Kong", "SEHK", "HKD"},
		{"Singapore", "SGX", "SGD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, conn, g := histTFixture(t)
			g.script(listingBroker(t, c, conn, "SYNH", tc.venue, tc.currency, tc.venue, false))

			bars, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), Contract{Symbol: "SYNH", SecType: "STK", Exchange: tc.venue, Currency: tc.currency}, 30, 2*time.Second)
			if err != nil || len(bars) != 1 || bars[0].Close != 101 {
				t.Fatalf("bars = %+v, %v", bars, err)
			}
			history := g.framesByID(reqHistoricalData)
			if len(history) != 1 {
				t.Fatalf("history requests = %v, want one", history)
			}
			assertFields(t, history[0], []fieldAssertion{{2, "4243", "conID from the discovery"}, {9, tc.venue, "exchange"}, {10, tc.venue, "primary"}})
		})
	}
}

// A listing the broker does list on SMART keeps SMART when the caller names its
// exchange: an account without that exchange's own data subscription reads it
// only that way (Tokyo, 2026-11-07), so the broker's list decides, not a region.
func TestHistoryUsesSMARTWhenTheBrokerListsIt(t *testing.T) {
	c, conn, g := histTFixture(t)
	g.script(listingBroker(t, c, conn, "SYNJ", "TSEJ", "JPY", "SMART,TSEJ,CHIXJ", true))

	bars, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), Contract{Symbol: "SYNJ", SecType: "STK", Exchange: "TSEJ", Currency: "JPY"}, 30, 2*time.Second)
	if err != nil || len(bars) != 1 {
		t.Fatalf("bars = %+v, %v", bars, err)
	}
	history := g.framesByID(reqHistoricalData)
	if len(history) != 1 {
		t.Fatalf("history requests = %v, want one", history)
	}
	assertFields(t, history[0], []fieldAssertion{{9, "SMART", "exchange"}, {10, "TSEJ", "primary"}})
}

// The actual holder of a Hong Kong share: the portfolio stream has already
// cached the holding's contract id with no list of routes. That hint must not
// answer for a read naming the exchange; the read asks the broker.
func TestHistoryOfAHeldListingAsksTheBrokerBeforeLeavingANamedExchange(t *testing.T) {
	c, conn, g := histTFixture(t)
	c.contractMu.Lock()
	c.contractCache["SYNH"] = ContractDetailsLite{Symbol: "SYNH", PrimaryExch: "SEHK", ConID: 4243, LocalSymbol: "SYNH"}
	c.contractMu.Unlock()
	g.script(listingBroker(t, c, conn, "SYNH", "SEHK", "HKD", "SEHK", false))

	bars, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), Contract{Symbol: "SYNH", SecType: "STK", Exchange: "SEHK", Currency: "HKD"}, 30, 2*time.Second)
	if err != nil || len(bars) != 1 {
		t.Fatalf("bars = %+v, %v", bars, err)
	}
	if n := len(g.framesByID(reqContractData)); n != 1 {
		t.Fatalf("discoveries = %d, want the broker asked once", n)
	}
	assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{2, "4243", "conID"}, {9, "SEHK", "exchange"}})
}

// A US holder's SMART read is untouched: the same kind of hint still routes it
// with no extra broker request.
func TestHistoryOfAHeldUSListingStillUsesItsCachedIdentityOnSMART(t *testing.T) {
	c, conn, g := histTFixture(t)
	c.contractMu.Lock()
	c.contractCache["SYNX"] = ContractDetailsLite{Symbol: "SYNX", PrimaryExch: "NASDAQ", ConID: 4243, LocalSymbol: "SYNX"}
	c.contractMu.Unlock()
	g.script(listingBroker(t, c, conn, "SYNX", "NASDAQ", "USD", "SMART,NASDAQ", true))

	if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), Contract{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}, 30, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if n := len(g.framesByID(reqContractData)); n != 0 {
		t.Fatalf("discoveries = %d, want none for a cached SMART route", n)
	}
	assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{2, "4243", "conID"}, {9, "SMART", "exchange"}})
}

// A wrong currency stays the broker's definition verdict from the details step;
// nothing about the route change turns it into a history request.
func TestHistoryOfAWrongCurrencyStopsAtTheDetailsStep(t *testing.T) {
	c, _, g := histTFixture(t)
	g.script(func(fields []string) {
		if fields[0] == strconv.Itoa(reqContractData) {
			reqID, _ := strconv.Atoi(fields[2])
			go c.failPendingContractDetails(reqID, 200, "No security definition has been found for the request")
		}
	})
	_, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), Contract{Symbol: "SYNH", SecType: "STK", Exchange: "SEHK", Currency: "USD"}, 30, 2*time.Second)
	if !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("read = %v, want the definition verdict", err)
	}
	if len(g.framesByID(reqHistoricalData)) != 0 {
		t.Fatal("history was requested without a contract")
	}
}

// A quote naming the exchange follows the same rule, from the same cache: the
// held-position hint leaves the request as the caller named it, and a resolved
// listing the broker does not offer on SMART stays on its exchange.
func TestExplicitQuoteRouteFollowsTheBrokersListOfRoutes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cached       ContractDetailsLite
		requested    Contract
		wantExchange string
		wantConID    int
	}{
		{
			name:         "held listing with no list of routes keeps the named exchange",
			cached:       ContractDetailsLite{Symbol: "SYNH", PrimaryExch: "SEHK", ConID: 4243},
			requested:    Contract{Symbol: "SYNH", SecType: "STK", Exchange: "SEHK", Currency: "HKD"},
			wantExchange: "SEHK",
		},
		{
			name:         "resolved listing not offered on SMART",
			cached:       ContractDetailsLite{Symbol: "SYNH", Exchange: "SEHK", PrimaryExch: "SEHK", ConID: 4243, ValidExchanges: "SEHK"},
			requested:    Contract{Symbol: "SYNH", SecType: "STK", Exchange: "SEHK", Currency: "HKD"},
			wantExchange: "SEHK",
			wantConID:    4243,
		},
		{
			name:         "resolved listing offered on SMART",
			cached:       ContractDetailsLite{Symbol: "SYNJ", Exchange: "TSEJ", PrimaryExch: "TSEJ", ConID: 4243, ValidExchanges: "SMART,TSEJ"},
			requested:    Contract{Symbol: "SYNJ", SecType: "STK", Exchange: "TSEJ", Currency: "JPY"},
			wantExchange: "SMART",
			wantConID:    4243,
		},
		{
			name:         "held US listing on SMART",
			cached:       ContractDetailsLite{Symbol: "SYNX", PrimaryExch: "NASDAQ", ConID: 4243},
			requested:    Contract{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"},
			wantExchange: "SMART",
			wantConID:    4243,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := histTFixture(t)
			c.contractMu.Lock()
			c.contractCache[tc.cached.Symbol] = tc.cached
			c.contractMu.Unlock()
			got := c.hydrateExplicitMarketDataContract(tc.requested)
			if got.Exchange != tc.wantExchange || got.ConID != tc.wantConID {
				t.Fatalf("route = %s conID %d, want %s conID %d", got.Exchange, got.ConID, tc.wantExchange, tc.wantConID)
			}
		})
	}
}
