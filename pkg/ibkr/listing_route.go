package ibkr

import (
	"strconv"
	"strings"
	"time"
)

const (
	// listingRouteWait bounds the one exact lookup a read can wait for.
	listingRouteWait = 3 * time.Second
	// listingRouteRetry spaces lookups of a contract the broker did not answer
	// for; a refused or lost lookup must not become a request on every read.
	listingRouteRetry = 15 * time.Minute
)

// listingRoute is what the broker said about one contract id: the listing's own
// exchange and currency, and whether its list of routes leaves SMART out. It is
// a fact about the listing, so it outlives a broker session.
type listingRoute struct {
	retryAt      time.Time
	primary      string
	currency     string
	known        bool
	refusesSMART bool
}

// usDollarStockExchanges are the listings a read need not ask about: US dollar
// stocks on US exchanges take SMART. This saves a request per holding and per
// breadth symbol; it decides no route, since any other listing goes to the broker.
var usDollarStockExchanges = map[string]struct{}{
	"AMEX": {}, "ARCA": {}, "BATS": {}, "IEX": {}, "ISLAND": {}, "NASDAQ": {}, "NYSE": {},
}

// routeListingForRead puts a read of an exact contract on a route the broker
// accepts. Holdings reach the reads with their contract id and, in many callers,
// a hard-coded SMART: mandate history, the quote at a fill, the display line,
// proposals, Edge. IBKR answers such a read of a Hong Kong or Singapore listing
// with code 200, so the listing's own list of routes decides, in one place. An
// unanswered lookup leaves the contract as it came.
//
// This is for reads only. An order's route is settled once, by
// ResolveOrderContractForSession, so the preview shows it and the token binds it;
// SubscribeMarketDataWithContractForSession then keeps that route.
func (c *Connector) routeListingForRead(contract Contract) Contract {
	if !c.listingRouteInQuestion(contract) {
		return contract
	}
	route := c.listingRouteFor(contract)
	if !route.known {
		return contract
	}
	if route.currency != "" {
		contract.Currency = route.currency
	}
	if route.refusesSMART {
		contract.Exchange, contract.PrimaryExch = route.primary, route.primary
	}
	return contract
}

// listingRouteInQuestion reports whether a read names an exact stock on SMART
// and the listing might not accept it. A US dollar contract with no exchange of
// its own, here or from the portfolio stream, is read as a US stock, as it always
// was: asking about each would cost a request per contract with nothing to gain.
func (c *Connector) listingRouteInQuestion(contract Contract) bool {
	secType := strings.ToUpper(strings.TrimSpace(contract.SecType))
	exchange := strings.TrimSpace(contract.Exchange)
	if contract.ConID <= 0 || (secType != "STK" && secType != "ETF") || (exchange != "" && !strings.EqualFold(exchange, "SMART")) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(contract.Currency), "USD") {
		return true
	}
	primary := strings.ToUpper(strings.TrimSpace(contract.PrimaryExch))
	if primary == "" {
		if cached := c.cachedContractDetail(strings.ToUpper(strings.TrimSpace(contract.Symbol))); cached != nil && cached.ConID == contract.ConID {
			primary = strings.ToUpper(strings.TrimSpace(cached.PrimaryExch))
		}
	}
	_, us := usDollarStockExchanges[primary]
	return primary != "" && !us
}

func (c *Connector) listingRouteFor(contract Contract) listingRoute {
	now := c.contractMissClock()
	c.contractMu.RLock()
	route, seen := c.listingRoutes[contract.ConID]
	cached, hasCached := c.contractCache[strings.ToUpper(strings.TrimSpace(contract.Symbol))]
	c.contractMu.RUnlock()
	if route.known || (seen && now.Before(route.retryAt)) {
		return route
	}
	if hasCached && cached.ConID == contract.ConID && cached.ValidExchanges != "" {
		return c.rememberListingRoute(contract.ConID, listingRouteFromDetail(cached))
	}
	if !c.isConnected() {
		return listingRoute{}
	}
	// By contract id alone: the caller's currency can be a symbol-only default,
	// and a lookup on SMART would draw the refusal it is here to avoid.
	exact := Contract{ConID: contract.ConID, Symbol: contract.Symbol, SecType: "STK"}
	details, err := c.coalesceContractDetails("listing\x00"+strconv.Itoa(contract.ConID), listingRouteWait, func(timeout time.Duration, observe func([]ContractDetailsLite)) ([]ContractDetailsLite, error) {
		return c.fetchContractDetailsForContractWire(exact, "", timeout, observe)
	})
	if err == nil {
		for _, detail := range details {
			if detail.ConID == contract.ConID {
				return c.rememberListingRoute(contract.ConID, listingRouteFromDetail(detail))
			}
		}
	}
	c.logDebug("Listing route for %s (conID=%d) unanswered (%v); the read keeps its route", contract.Symbol, contract.ConID, err)
	return c.rememberListingRoute(contract.ConID, listingRoute{retryAt: now.Add(listingRouteRetry)})
}

func listingRouteFromDetail(detail ContractDetailsLite) listingRoute {
	primary := strings.ToUpper(strings.TrimSpace(detail.PrimaryExch))
	if exchange := strings.ToUpper(strings.TrimSpace(detail.Exchange)); primary == "" && exchange != "SMART" {
		primary = exchange
	}
	return listingRoute{
		known:        true,
		primary:      primary,
		currency:     strings.ToUpper(strings.TrimSpace(detail.Currency)),
		refusesSMART: primary != "" && listingRefusesSMART(detail.ValidExchanges),
	}
}

func (c *Connector) rememberListingRoute(conID int, route listingRoute) listingRoute {
	c.contractMu.Lock()
	defer c.contractMu.Unlock()
	if c.listingRoutes == nil {
		c.listingRoutes = make(map[int]listingRoute)
	}
	c.listingRoutes[conID] = route
	return route
}
