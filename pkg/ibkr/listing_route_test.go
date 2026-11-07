package ibkr

import (
	"errors"
	"strconv"
	"strings"
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

// heldListingBroker answers like the gateway for a holding read by contract id:
// an exact lookup with no exchange names the listing and its routes, and a
// quote or history request on SMART draws a code 200. It keeps which lookups it
// was asked.
func heldListingBroker(t *testing.T, c *Connector, conn *Connection, symbol, venue, currency, validExchanges string, smart bool) func([]string) {
	t.Helper()
	binding, _ := c.CaptureSession()
	refuse := func(id string) {
		go c.handleIBKRErrorFrom(binding, []string{"4", "2", id, "200", "No security definition has been found for the request"})
	}
	return func(fields []string) {
		switch fields[0] {
		case strconv.Itoa(reqContractData):
			reqID, _ := strconv.Atoi(fields[2])
			route := fields[10]
			if route == "SMART" && !smart {
				go c.failPendingContractDetails(reqID, 200, "No security definition has been found for the request")
				return
			}
			conID, _ := strconv.Atoi(fields[3])
			if route == "" {
				route = venue
			}
			histTAnswerRoute(conn, reqID, listingDetailsFrame(t, conn, reqID, conID, symbol, route, venue, currency, validExchanges))
		case strconv.Itoa(reqHistoricalData):
			reqID, _ := strconv.Atoi(fields[1])
			if fields[9] == "SMART" && !smart {
				refuse(fields[1])
				return
			}
			histTAnswerHistory(c, reqID, []HistoricalBar{histTBar("20260929", 101)})
		case strconv.Itoa(reqMktData):
			if fields[10] == "SMART" && !smart {
				refuse(fields[2])
			}
		}
	}
}

// A holding reaches Canary with its contract id, and many reads hard-code SMART
// for it: mandate history, the quote at a fill, the display line, proposals and
// Edge. Each one reads on the listing's own exchange in its own currency, by
// asking the broker once for that contract.
func TestReadsOfAHeldListingFollowTheBrokerWhenTheCallerSaysSMART(t *testing.T) {
	held := Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", PrimaryExch: "SEHK", Currency: "HKD"}

	t.Run("daily history", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNH", "SEHK", "HKD", "SEHK", false))
		bars, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), held, 30, 2*time.Second)
		if err != nil || len(bars) != 1 {
			t.Fatalf("bars = %+v, %v", bars, err)
		}
		assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{2, "4243", "conID"}, {9, "SEHK", "exchange"}, {10, "SEHK", "primary"}})
	})
	t.Run("forced trades history, as Edge reads it", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNH", "SGX", "SGD", "SGX", false))
		bars, err := c.FetchHistoricalDailyTradeBarsWithContract(t.Context(), Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", Currency: "SGD"}, 30, 2*time.Second)
		if err != nil || len(bars) != 1 {
			t.Fatalf("bars = %+v, %v", bars, err)
		}
		assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{9, "SGX", "exchange"}})
	})
	t.Run("chart series, as mandate history reads it", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNH", "SEHK", "HKD", "SEHK", false))
		series, err := c.FetchChartBars(t.Context(), held, 30, "1 day", 2*time.Second)
		if err != nil || len(series.Bars) != 1 || series.Contract.ConID != 4243 || series.Contract.PrimaryExch != "SEHK" || series.Contract.Currency != "HKD" {
			t.Fatalf("series = %+v, %v", series, err)
		}
		assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{2, "4243", "conID"}, {9, "SEHK", "exchange"}})
	})
	t.Run("quote line, as the display and the fill quote read it", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNH", "SEHK", "HKD", "SEHK", false))
		key, err := c.SubscribeMarketDataWithContract(t.Context(), Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", Currency: "HKD"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		frames := g.waitFramesByID(t, reqMktData, 1)
		assertFields(t, frames[0], []fieldAssertion{{3, "4243", "conID"}, {10, "SEHK", "exchange"}, {12, "HKD", "currency"}})
		if !strings.Contains(key, "SEHK") {
			t.Fatalf("subscription key %q does not name the route it holds", key)
		}
	})
	t.Run("a bare symbol the book holds", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		c.contractMu.Lock()
		c.contractCache["SYNH"] = ContractDetailsLite{Symbol: "SYNH", PrimaryExch: "SEHK", ConID: 4243, LocalSymbol: "SYNH"}
		c.contractMu.Unlock()
		g.script(heldListingBroker(t, c, conn, "SYNH", "SEHK", "HKD", "SEHK", false))
		bars, err := c.FetchHistoricalDailyBars(t.Context(), "SYNH", 30, 2*time.Second)
		if err != nil || len(bars) != 1 {
			t.Fatalf("bars = %+v, %v", bars, err)
		}
		assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{2, "4243", "conID"}, {9, "SEHK", "exchange"}, {11, "HKD", "currency"}})
	})
}

// The broker is asked once per contract, and only about a listing that might
// refuse SMART: a US dollar stock on a named US exchange costs no extra request.
func TestListingRouteLookupIsBoundedAndChangesNothingElse(t *testing.T) {
	t.Run("asked once", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNH", "SEHK", "HKD", "SEHK", false))
		held := Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", PrimaryExch: "SEHK", Currency: "HKD"}
		for range 3 {
			if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), held, 30, 2*time.Second); err != nil {
				t.Fatal(err)
			}
		}
		lookups := g.framesByID(reqContractData)
		if len(lookups) != 1 {
			t.Fatalf("lookups = %v, want one", lookups)
		}
		assertFields(t, lookups[0], []fieldAssertion{{3, "4243", "conID"}, {10, "", "no exchange"}, {11, "", "no primary"}, {12, "", "no currency"}})
	})
	t.Run("a US stock on a US exchange is not asked about", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNX", "NASDAQ", "USD", "SMART,NASDAQ", true))
		held := Contract{ConID: 4243, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
		if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), held, 30, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		if n := len(g.framesByID(reqContractData)); n != 0 {
			t.Fatalf("lookups = %d, want none", n)
		}
		assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{9, "SMART", "exchange"}})
	})
	t.Run("a US dollar stock listed in Singapore is", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNS", "SGX", "USD", "SGX", false))
		held := Contract{ConID: 4243, Symbol: "SYNS", SecType: "STK", Exchange: "SMART", PrimaryExch: "SGX", Currency: "USD"}
		if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), held, 30, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{9, "SGX", "exchange"}})
	})
	t.Run("a US dollar contract whose holding names Singapore is too", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		c.contractMu.Lock()
		c.contractCache["SYNS"] = ContractDetailsLite{Symbol: "SYNS", PrimaryExch: "SGX", ConID: 4243}
		c.contractMu.Unlock()
		g.script(heldListingBroker(t, c, conn, "SYNS", "SGX", "USD", "SGX", false))
		if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), Contract{ConID: 4243, Symbol: "SYNS", SecType: "STK", Exchange: "SMART", Currency: "USD"}, 30, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{9, "SGX", "exchange"}})
	})
	t.Run("a US dollar contract naming no exchange at all is read as before", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNX", "NASDAQ", "USD", "SMART,NASDAQ", true))
		if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), Contract{ConID: 4243, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}, 30, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		if n := len(g.framesByID(reqContractData)); n != 0 {
			t.Fatalf("lookups = %d, want none", n)
		}
	})
	t.Run("a listing the broker offers on SMART stays there", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNJ", "TSEJ", "JPY", "SMART,TSEJ", true))
		held := Contract{ConID: 4243, Symbol: "SYNJ", SecType: "STK", Exchange: "SMART", PrimaryExch: "TSEJ", Currency: "JPY"}
		if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), held, 30, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{9, "SMART", "exchange"}, {10, "TSEJ", "primary"}})
	})
	t.Run("a refused lookup leaves the route alone and is not repeated at once", func(t *testing.T) {
		c, _, g := histTFixture(t)
		binding, _ := c.CaptureSession()
		g.script(func(fields []string) {
			switch fields[0] {
			case strconv.Itoa(reqContractData):
				reqID, _ := strconv.Atoi(fields[2])
				go c.failPendingContractDetails(reqID, 200, "No security definition has been found for the request")
			case strconv.Itoa(reqHistoricalData):
				go c.handleIBKRErrorFrom(binding, []string{"4", "2", fields[1], "200", "No security definition has been found for the request"})
			}
		})
		held := Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", PrimaryExch: "SEHK", Currency: "HKD"}
		for range 2 {
			if _, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), held, 30, 2*time.Second); err == nil {
				t.Fatal("a contract the broker refuses read successfully")
			}
		}
		if n := len(g.framesByID(reqContractData)); n != 1 {
			t.Fatalf("lookups = %d, want one inside the retry window", n)
		}
		assertFields(t, g.framesByID(reqHistoricalData)[0], []fieldAssertion{{9, "SMART", "exchange"}})
	})
}

// The order path resolves and routes through its own calls: a listing SMART
// reaches keeps the route the caller gave, on the caller's one lookup.
func TestOrderContractResolutionKeepsTheCallersRoute(t *testing.T) {
	c, conn, g := histTFixture(t)
	g.script(heldListingBroker(t, c, conn, "SYNJ", "TSEJ", "JPY", "SMART,TSEJ", true))
	binding, ok := c.CaptureSession()
	if !ok {
		t.Fatal("no session")
	}
	resolved, err := c.ResolveOrderContractForSession(t.Context(), binding, Contract{ConID: 4243, Symbol: "SYNJ", SecType: "STK", Exchange: "SMART", Currency: "JPY"}, time.Second)
	if err != nil || resolved.Contract.Exchange != "SMART" {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	lookups := g.framesByID(reqContractData)
	if len(lookups) != 1 {
		t.Fatalf("lookups = %v, want only the caller's", lookups)
	}
	assertFields(t, lookups[0], []fieldAssertion{{10, "SMART", "caller's exchange"}})
}

// IBKR refuses a Hong Kong or Singapore listing on SMART with code 200, so an
// order built on SMART (a reduction from the portfolio row, canary add's
// default) could never reach it. A holding is asked for by its contract id on
// its own exchange, and the order takes the only route the broker lists.
func TestOrderOnAListingSMARTDoesNotReachMovesToTheListingsExchange(t *testing.T) {
	for _, tc := range []struct{ name, venue, currency string }{
		{"Hong Kong", "SEHK", "HKD"},
		{"Singapore", "SGX", "SGD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, conn, g := histTFixture(t)
			g.script(heldListingBroker(t, c, conn, "SYNH", tc.venue, tc.currency, tc.venue, false))
			binding, ok := c.CaptureSession()
			if !ok {
				t.Fatal("no session")
			}
			resolved, err := c.ResolveOrderContractForSession(t.Context(), binding, Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", PrimaryExch: tc.venue, Currency: tc.currency}, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if got := resolved.Contract; got.Exchange != tc.venue || got.PrimaryExch != tc.venue || got.Currency != tc.currency || got.ConID != 4243 {
				t.Fatalf("resolved contract = %+v", got)
			}
			lookups := g.framesByID(reqContractData)
			if len(lookups) != 2 {
				t.Fatalf("lookups = %v, want the caller's and one on the listing", lookups)
			}
			assertFields(t, lookups[0], []fieldAssertion{{10, "SMART", "caller's route first"}})
			assertFields(t, lookups[1], []fieldAssertion{{3, "4243", "contract id"}, {10, tc.venue, "the listing's exchange"}})
		})
	}
}

// A holding with no primary exchange on its row is asked for by contract id
// alone, the lookup the read path already uses.
func TestOrderOnAHoldingWithNoVenueAsksByContractIDAlone(t *testing.T) {
	c, conn, g := histTFixture(t)
	g.script(heldListingBroker(t, c, conn, "SYNH", "SEHK", "HKD", "SEHK", false))
	binding, _ := c.CaptureSession()
	resolved, err := c.ResolveOrderContractForSession(t.Context(), binding, Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", Currency: "HKD"}, 2*time.Second)
	if err != nil || resolved.Contract.Exchange != "SEHK" || resolved.Contract.PrimaryExch != "SEHK" {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	lookups := g.framesByID(reqContractData)
	if len(lookups) != 2 {
		t.Fatalf("lookups = %v", lookups)
	}
	assertFields(t, lookups[1], []fieldAssertion{{3, "4243", "contract id"}, {10, "", "no exchange"}})
}

// canary add names a symbol and a currency and defaults to SMART. With no
// contract id to ask by, the currency says where to ask; the broker's list still
// decides the route.
func TestOrderBySymbolAsksOnTheCurrencysHomeExchange(t *testing.T) {
	for _, tc := range []struct{ venue, currency string }{{"SEHK", "HKD"}, {"SGX", "SGD"}} {
		t.Run(tc.currency, func(t *testing.T) {
			c, conn, g := histTFixture(t)
			g.script(listingBroker(t, c, conn, "SYNH", tc.venue, tc.currency, tc.venue, false))
			binding, _ := c.CaptureSession()
			resolved, err := c.ResolveOrderContractForSession(t.Context(), binding, Contract{Symbol: "SYNH", SecType: "STK", Exchange: "SMART", Currency: tc.currency}, 2*time.Second)
			if err != nil || resolved.Contract.Exchange != tc.venue || resolved.Contract.ConID != 4243 {
				t.Fatalf("resolved = %+v, %v", resolved, err)
			}
		})
	}
}

// Should the broker answer the SMART lookup but leave SMART off its own list,
// the list still decides: no second request, and no order on a refused route.
func TestOrderFollowsTheListWhenTheSMARTLookupIsAnswered(t *testing.T) {
	c, conn, g := histTFixture(t)
	g.script(heldListingBroker(t, c, conn, "SYNH", "SEHK", "HKD", "SEHK", true))
	binding, _ := c.CaptureSession()
	resolved, err := c.ResolveOrderContractForSession(t.Context(), binding, Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", Currency: "HKD"}, 2*time.Second)
	if err != nil || resolved.Contract.Exchange != "SEHK" {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	if lookups := g.framesByID(reqContractData); len(lookups) != 1 {
		t.Fatalf("lookups = %v, want one", lookups)
	}
}

// A listing SMART reaches, and a refusal with nothing else to ask, send exactly
// what they sent before: the second lookup exists only for a listing that fails.
func TestOrderLookupsThatWorkedStayAsTheyWere(t *testing.T) {
	t.Run("US stock keeps SMART on one lookup", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNU", "ARCA", "USD", "SMART,ARCA", true))
		binding, _ := c.CaptureSession()
		resolved, err := c.ResolveOrderContractForSession(t.Context(), binding, Contract{ConID: 4243, Symbol: "SYNU", SecType: "STK", Exchange: "SMART", Currency: "USD"}, 2*time.Second)
		if err != nil || resolved.Contract.Exchange != "SMART" {
			t.Fatalf("resolved = %+v, %v", resolved, err)
		}
		lookups := g.framesByID(reqContractData)
		if len(lookups) != 1 {
			t.Fatalf("lookups = %v", lookups)
		}
		assertFields(t, lookups[0], []fieldAssertion{{10, "SMART", "exchange"}})
	})
	for _, tc := range []struct {
		name     string
		contract Contract
	}{
		{"unknown US symbol", Contract{Symbol: "SYNU", SecType: "STK", Exchange: "SMART", Currency: "USD"}},
		{"US holding", Contract{ConID: 4243, Symbol: "SYNU", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"}},
		{"euro symbol", Contract{Symbol: "SYNE", SecType: "STK", Exchange: "SMART", Currency: "EUR"}},
		{"a named exchange", Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SEHKNTL", Currency: "CNH"}},
	} {
		t.Run(tc.name+" refused once", func(t *testing.T) {
			c, _, g := histTFixture(t)
			g.script(func(fields []string) {
				if fields[0] == strconv.Itoa(reqContractData) {
					reqID, _ := strconv.Atoi(fields[2])
					go c.failPendingContractDetails(reqID, 200, "No security definition has been found for the request")
				}
			})
			binding, _ := c.CaptureSession()
			_, err := c.ResolveOrderContractForSession(t.Context(), binding, tc.contract, 2*time.Second)
			if !errors.Is(err, ErrContractNoDefinition) {
				t.Fatalf("err = %v, want the broker's verdict", err)
			}
			if lookups := g.framesByID(reqContractData); len(lookups) != 1 {
				t.Fatalf("lookups = %v, want one", lookups)
			}
		})
	}
}

// The second lookup's refusal is the broker's verdict too, and a listing in
// another currency than the order named is no match, on either route.
func TestOrderSecondLookupFailsClosed(t *testing.T) {
	t.Run("refused twice", func(t *testing.T) {
		c, _, g := histTFixture(t)
		g.script(func(fields []string) {
			if fields[0] == strconv.Itoa(reqContractData) {
				reqID, _ := strconv.Atoi(fields[2])
				go c.failPendingContractDetails(reqID, 200, "No security definition has been found for the request")
			}
		})
		binding, _ := c.CaptureSession()
		_, err := c.ResolveOrderContractForSession(t.Context(), binding, Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", Currency: "HKD"}, 2*time.Second)
		if !errors.Is(err, ErrContractNoDefinition) {
			t.Fatalf("err = %v", err)
		}
		if lookups := g.framesByID(reqContractData); len(lookups) != 2 {
			t.Fatalf("lookups = %v, want two", lookups)
		}
	})
	t.Run("another currency", func(t *testing.T) {
		c, conn, g := histTFixture(t)
		g.script(heldListingBroker(t, c, conn, "SYNH", "SEHK", "USD", "SEHK", false))
		binding, _ := c.CaptureSession()
		if resolved, err := c.ResolveOrderContractForSession(t.Context(), binding, Contract{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", Currency: "HKD"}, 2*time.Second); err == nil {
			t.Fatalf("resolved = %+v, want no match", resolved)
		}
	})
}
