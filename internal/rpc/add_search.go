package rpc

import "time"

// MethodAddSearch finds stock listings for the owner's Add composer: the
// broker's symbol search, read-only.
const MethodAddSearch = "add.search"

// AddSearchParams carries the text the owner typed: 1-32 characters of
// letters, digits, dot, space or hyphen.
type AddSearchParams struct {
	Pattern string `json:"pattern"`
}

// AddSearchListing is one stock listing the search found. Exchange is where
// Add must ask about it (SMART, or the listing's own exchange where orders
// leave SMART); Market is its calendar key, empty when Canary has none.
type AddSearchListing struct {
	Symbol          string `json:"symbol"`
	ConID           int    `json:"con_id"`
	Name            string `json:"name"`
	SecType         string `json:"sec_type"`
	Currency        string `json:"currency"`
	PrimaryExchange string `json:"primary_exchange"`
	Exchange        string `json:"exchange"`
	Market          string `json:"market"`
}

// AddSearchResult lists exact symbol matches first, then the broker's order,
// at most AddSearchLimit; Truncated says more stock listings matched.
type AddSearchResult struct {
	AsOf      time.Time          `json:"as_of"`
	Query     string             `json:"query"`
	Listings  []AddSearchListing `json:"listings"`
	Truncated bool               `json:"truncated"`
}

// AddSearchLimit is the most listings one search returns.
const AddSearchLimit = 12
