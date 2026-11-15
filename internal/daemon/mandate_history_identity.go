package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

// A logical symbol is a display/risk-factor key, never a history identity.
// Retain old documents for validated migration, while new requests, attempts
// and documents use an identity-specific key. Missing identity fails closed.
func mandateHistoryLogicalKey(key string) string {
	key, _, _ = strings.Cut(key, "@")
	return key
}

func bindMandateHistoryNeed(need mandateHistoryNeed) mandateHistoryNeed {
	c := need.Contract
	c.Symbol, c.Currency = normSym(c.Symbol), normCcy(c.Currency)
	c.SecType = strings.ToUpper(strings.TrimSpace(c.SecType))
	if c.SecType == "ETF" {
		c.SecType = "STK"
	}
	c.Exchange = strings.ToUpper(strings.TrimSpace(c.Exchange))
	c.PrimaryExch = strings.ToUpper(strings.TrimSpace(c.PrimaryExch))
	need.Contract = c
	market := need.Market
	if market == "" {
		market = marketcal.MarketUSEquity
	}
	raw, _ := json.Marshal(struct {
		ConID                                            int `json:"con_id"`
		Symbol, SecType, Currency, Exchange, PrimaryExch string
		Market                                           marketcal.Market
	}{c.ConID, c.Symbol, c.SecType, c.Currency, c.Exchange, c.PrimaryExch, market})
	sum := sha256.Sum256(raw)
	need.Key = mandateHistoryLogicalKey(need.Key) + "@" + hex.EncodeToString(sum[:])
	return need
}

func mandateHistoryContractMatches(actual, want rpc.ContractParams) bool {
	secType := func(v string) string {
		v = strings.ToUpper(strings.TrimSpace(v))
		if v == "ETF" {
			return "STK"
		}
		return v
	}
	if normSym(want.Symbol) == "" || normCcy(want.Currency) == "" || secType(want.SecType) == "" ||
		normSym(actual.Symbol) != normSym(want.Symbol) || normCcy(actual.Currency) != normCcy(want.Currency) || secType(actual.SecType) != secType(want.SecType) {
		return false
	}
	if want.ConID > 0 && actual.ConID != want.ConID {
		return false
	}
	if venue := strings.TrimSpace(want.PrimaryExch); venue != "" {
		actualVenue := actual.PrimaryExch
		if actualVenue == "" && !strings.EqualFold(actual.Exchange, "SMART") {
			actualVenue = actual.Exchange
		}
		if !strings.EqualFold(strings.TrimSpace(actualVenue), venue) {
			return false
		}
	}
	return true
}

func mandateHistoryContractsConflict(a, b rpc.ContractParams) bool {
	if !strings.EqualFold(a.Symbol, b.Symbol) || !strings.EqualFold(a.Currency, b.Currency) || !strings.EqualFold(a.SecType, b.SecType) {
		return true
	}
	if a.ConID > 0 && b.ConID > 0 && a.ConID != b.ConID {
		return true
	}
	return a.PrimaryExch != "" && b.PrimaryExch != "" && !strings.EqualFold(a.PrimaryExch, b.PrimaryExch)
}

func mandateSeriesMatchesNeed(series *mandateHistorySeries, need mandateHistoryNeed) bool {
	if series == nil || need.Ambiguous || !mandateHistoryContractMatches(series.Contract, need.Contract) {
		return false
	}
	// Generic book needs do not declare a listing calendar. A matching
	// retained contract can carry its own; strategy needs pin theirs explicitly.
	return need.Market == "" || mandateSeriesMarket(series) == need.Market
}

func (s *Server) mandateHistoryForNeed(ctx context.Context, need mandateHistoryNeed) *mandateHistorySeries {
	if need.Ambiguous {
		return nil
	}
	need = bindMandateHistoryNeed(need)
	if series := s.mandateHistorySeries(ctx, need.Key); mandateSeriesMatchesNeed(series, need) {
		return series
	}
	// Old symbol-only documents remain reusable only with complete, matching
	// broker identity. Never relabel their closes to satisfy a different need.
	if series := s.mandateHistorySeries(ctx, mandateHistoryLogicalKey(need.Key)); mandateSeriesMatchesNeed(series, need) {
		return series
	}
	// A concrete held contract may already have been loaded with a more
	// detailed routing hint. Reuse only a proven matching positive ConID;
	// unresolved symbol requests must obtain their own broker resolution.
	if need.Contract.ConID > 0 {
		st := &s.mandateHistory
		st.mu.Lock()
		defer st.mu.Unlock()
		var latest *mandateHistorySeries
		for key, candidate := range st.series {
			if mandateHistoryLogicalKey(key) == mandateHistoryLogicalKey(need.Key) && mandateSeriesMatchesNeed(candidate, need) &&
				(latest == nil || candidate.FetchedAt.After(latest.FetchedAt)) {
				latest = candidate
			}
		}
		return latest
	}
	return nil
}

func mandateIndexHistoryNeed() mandateHistoryNeed {
	return mandateHistoryNeed{Key: "u:" + mandate.IndexSymbol, Contract: rpc.ContractParams{Symbol: mandate.IndexSymbol, SecType: "STK", Currency: "USD", Exchange: "SMART", PrimaryExch: "ARCA"}}
}
