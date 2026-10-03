package rpc

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Watchlist methods, conflict code and fixed inventory bound belong to the owner preference surface.
const (
	MethodWatchlistList    = "watchlist.list"
	MethodWatchlistReplace = "watchlist.replace"
	MethodWatchlistAdd     = "watchlist.add"
	MethodWatchlistRemove  = "watchlist.remove"
	CodeWatchlistConflict  = "watchlist_conflict"
	WatchlistLimit         = 20
)

// WatchlistContract is an owner-selected underlying, not broker resolution.
// ConID zero explicitly means unresolved; positive identities are preserved.
type WatchlistContract struct {
	Symbol   string `json:"symbol"`
	ConID    int    `json:"con_id"`
	SecType  string `json:"sec_type"`
	Currency string `json:"currency"`
	Exchange string `json:"exchange"`
}

// Watchlist is the currently accepted owner list and optional mutation receipt.
type Watchlist struct {
	Version       int                 `json:"version"`
	Revision      int64               `json:"revision"`
	Symbols       []WatchlistContract `json:"symbols"`
	AsOf          time.Time           `json:"as_of"`
	RequestID     string              `json:"request_id,omitempty"`
	SavedRevision int64               `json:"saved_revision,omitempty"`
	Replay        bool                `json:"replay,omitempty"`
}

// WatchlistReplaceRequest replaces the ordered list under an immutable revision fence.
type WatchlistReplaceRequest struct {
	Symbols          []WatchlistContract `json:"symbols"`
	ExpectedRevision int64               `json:"expected_revision"`
	RequestID        string              `json:"request_id"`
}

// WatchlistAddRequest appends one underlying without rebinding an existing symbol.
type WatchlistAddRequest struct {
	Contract         WatchlistContract `json:"contract"`
	ExpectedRevision int64             `json:"expected_revision"`
	RequestID        string            `json:"request_id"`
}

// WatchlistRemoveRequest removes one symbol under a revision fence.
type WatchlistRemoveRequest struct {
	Symbol           string `json:"symbol"`
	ExpectedRevision int64  `json:"expected_revision"`
	RequestID        string `json:"request_id"`
}

var watchlistSymbol = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.]{0,31}$`)

var watchlistRequestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ValidateWatchlistMutation checks the bounded revision and immutable receipt identity.
func ValidateWatchlistMutation(revision int64, requestID string) error {
	if revision < 0 || revision == math.MaxInt64 || !watchlistRequestID.MatchString(requestID) {
		return fmt.Errorf("nonnegative expected_revision below MaxInt64 and immutable request_id required")
	}
	return nil
}

// NormalizeWatchlistContract restricts owner input to supported stock identities.
func NormalizeWatchlistContract(c WatchlistContract) (WatchlistContract, error) {
	c.Symbol = strings.ToUpper(strings.TrimSpace(c.Symbol))
	if !watchlistSymbol.MatchString(c.Symbol) {
		return c, fmt.Errorf("symbol must match [A-Z0-9][A-Z0-9.]{0,31}")
	}
	if c.SecType == "" {
		c.SecType = "STK"
	}
	if c.Currency == "" {
		c.Currency = "USD"
	}
	if c.Exchange == "" {
		c.Exchange = "SMART"
	}
	if c.SecType != "STK" || c.Currency != "USD" || c.Exchange != "SMART" || c.ConID < 0 || c.ConID > math.MaxInt32 {
		return c, fmt.Errorf("watchlist supports USD SMART STK underlyings with nonnegative contract IDs only")
	}
	return c, nil
}

// NormalizeWatchlistSymbols preserves order while rejecting duplicates and oversized lists.
func NormalizeWatchlistSymbols(in []WatchlistContract) ([]WatchlistContract, error) {
	if in == nil || len(in) > WatchlistLimit {
		return nil, fmt.Errorf("symbols must be an array of at most 20 underlyings")
	}
	out := make([]WatchlistContract, 0, len(in))
	symbols, ids := map[string]bool{}, map[int]bool{}
	for _, c := range in {
		c, err := NormalizeWatchlistContract(c)
		if err != nil {
			return nil, err
		}
		if symbols[c.Symbol] || (c.ConID > 0 && ids[c.ConID]) {
			return nil, fmt.Errorf("watchlist symbols and resolved contract IDs must be distinct")
		}
		symbols[c.Symbol], ids[c.ConID] = true, true
		out = append(out, c)
	}
	return out, nil
}
