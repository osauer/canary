package daemon

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/rpc/api"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

var addSearchPattern = regexp.MustCompile(`^[A-Za-z0-9. -]{1,32}$`)

// addSearchRetryError is a search IBKR's pacing or deadline stopped; the RPC
// layer classifies it as the timeout callers retry.
type addSearchRetryError struct{ cause error }

func (e *addSearchRetryError) Error() string {
	return "IBKR answers one listing search at a time; try again in a moment."
}

func (e *addSearchRetryError) Unwrap() error { return context.DeadlineExceeded }

// handleAddSearch finds stock listings for a symbol or name. Read-only.
func (s *Server) handleAddSearch(ctx context.Context, req *rpc.Request) (*rpc.AddSearchResult, error) {
	var p rpc.AddSearchParams
	if err := decodeFor(api.AddSearch, req.Params, &p); err != nil {
		return nil, err
	}
	pattern := strings.TrimSpace(p.Pattern)
	if !addSearchPattern.MatchString(pattern) {
		return nil, errBadRequest("Search with 1 to 32 letters, digits, dots, spaces or hyphens")
	}
	found, err := s.searchListings(ctx, pattern)
	if err != nil {
		if errors.Is(err, ibkr.ErrSymbolSearchBusy) || errors.Is(err, context.DeadlineExceeded) {
			return nil, &addSearchRetryError{cause: err}
		}
		return nil, err
	}
	listings, truncated := addSearchListings(pattern, found)
	return &rpc.AddSearchResult{AsOf: s.orderNow(), Query: pattern, Listings: listings, Truncated: truncated}, nil
}

func (s *Server) searchListings(ctx context.Context, pattern string) ([]broker.Listing, error) {
	if s.addSearchForTest != nil {
		return s.addSearchForTest(ctx, pattern)
	}
	reads := s.brokerReads()
	if reads == nil {
		return nil, s.sessionUnavailableError()
	}
	search, ok := reads.(broker.ListingSearch)
	if !ok {
		return nil, errBadRequest("Listing search runs on IBKR only")
	}
	binding, ok := reads.Capture()
	if !ok {
		return nil, s.sessionUnavailableError()
	}
	return search.SearchListings(ctx, binding, pattern)
}

// addSearchListings keeps the stock listings, exact symbol matches first and
// otherwise in the broker's order, at most rpc.AddSearchLimit.
func addSearchListings(pattern string, found []broker.Listing) ([]rpc.AddSearchListing, bool) {
	var exact, rest []rpc.AddSearchListing
	for _, l := range found {
		conID, ok := l.IDInt()
		if !strings.EqualFold(strings.TrimSpace(l.SecType), "STK") || !ok || conID <= 0 {
			continue
		}
		listing := addSearchListing(conID, l)
		if strings.EqualFold(listing.Symbol, pattern) {
			exact = append(exact, listing)
		} else {
			rest = append(rest, listing)
		}
	}
	all := append(exact, rest...)
	if len(all) > rpc.AddSearchLimit {
		return all[:rpc.AddSearchLimit], true
	}
	if all == nil {
		all = []rpc.AddSearchListing{}
	}
	return all, false
}

// addSearchListing names where Add asks about a listing: SMART for a US
// dollar listing, the listing's own exchange where orders in its currency
// leave SMART (ibkr.OrderHomeExchange), SMART otherwise.
func addSearchListing(conID int, l broker.Listing) rpc.AddSearchListing {
	currency := strings.ToUpper(strings.TrimSpace(l.Currency))
	primary := strings.ToUpper(strings.TrimSpace(l.Primary))
	exchange := "SMART"
	if home := ibkr.OrderHomeExchange(currency); currency != "USD" && home != "" {
		exchange = home
	}
	market := listingMarket(primary, "", currency)
	if market == marketcal.MarketUSEquity && currency != "USD" {
		market = "" // listingMarket's US default is no calendar for this listing
	}
	return rpc.AddSearchListing{
		Symbol: strings.ToUpper(strings.TrimSpace(l.Symbol)), ConID: conID, Name: strings.TrimSpace(l.Name),
		SecType: "STK", Currency: currency, PrimaryExchange: primary, Exchange: exchange, Market: string(market),
	}
}
