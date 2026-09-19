package daemon

import (
	"strings"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Sector names follow GICS. Single stocks in the S&P 500 take the sector
// Wikipedia lists for them; every other stock is mapped from the broker's
// industry and category; index funds spread over their published weights.
const (
	sectorUnclassified = "Unclassified"
	sectorFunds        = "Funds (no look-through)"
)

// underlyingClassification is what the sector projection knows about one
// underlying symbol. Fund reports a pooled vehicle; LookThrough is present
// only for the funds whose sector weights are embedded below.
type underlyingClassification struct {
	Sector      string
	Fund        bool
	LookThrough *fundSectorWeights
}

// fundSectorWeights is one fund's GICS sector split in percent of fund value.
// Weights need not sum to 100: the residual is cash or derivatives, which is
// not sector exposure. AsOf is the issuer's or index vendor's date, not the
// observation time of the book.
type fundSectorWeights struct {
	AsOf    string
	Source  string
	Weights map[string]float64
}

// fundLookThrough is the embedded sector table for the index funds this book
// holds. It is refreshed by hand from the named sources; the as-of date
// travels to consumers so a stale split is visible rather than silent.
var fundLookThrough = map[string]fundSectorWeights{
	"SPY": {
		AsOf:   "2026-09-17",
		Source: "State Street SPY daily holdings weighted by Wikipedia GICS sectors",
		Weights: map[string]float64{
			"Information Technology": 38.34, "Financials": 11.94, "Communication Services": 9.94,
			"Health Care": 9.22, "Consumer Discretionary": 8.80, "Industrials": 8.05,
			"Consumer Staples": 4.46, "Energy": 3.53, "Utilities": 1.96, "Materials": 1.76, "Real Estate": 1.76,
		},
	},
	"QQQ": {
		AsOf:   "2026-09-18",
		Source: "Nasdaq-100 constituent weights (Slickcharts) weighted by Wikipedia GICS sectors",
		Weights: map[string]float64{
			"Information Technology": 57.98, "Communication Services": 15.97, "Consumer Discretionary": 11.95,
			"Industrials": 6.64, "Consumer Staples": 3.96, "Health Care": 2.10, "Utilities": 0.58,
			"Materials": 0.50, "Energy": 0.27, "Financials": 0.11,
		},
	},
	"IWM": {
		AsOf:   "2026-09-17",
		Source: "iShares IWM published sector breakdown",
		Weights: map[string]float64{
			"Health Care": 20.39, "Financials": 19.37, "Industrials": 13.88, "Information Technology": 13.21,
			"Consumer Discretionary": 8.82, "Energy": 6.95, "Real Estate": 5.66, "Materials": 4.28,
			"Utilities": 2.77, "Communication Services": 2.44, "Consumer Staples": 1.96,
		},
	},
}

// classifyUnderlying resolves one symbol in precedence order: embedded fund
// table, broker stock type, Wikipedia GICS sector, broker industry map. The
// broker fields may be zero when no lookup ran; the free sources still apply.
func classifyUnderlying(symbol string, mc ibkrlib.MarketClassification) underlyingClassification {
	key := strings.ToUpper(strings.TrimSpace(symbol))
	if w, ok := fundLookThrough[key]; ok {
		return underlyingClassification{Fund: true, LookThrough: &w}
	}
	if isFundStockType(mc.StockType) {
		return underlyingClassification{Fund: true}
	}
	if s, ok := spx.SectorOf(key); ok {
		return underlyingClassification{Sector: s}
	}
	if s, ok := gicsFromIBKR(mc.Industry, mc.Category); ok {
		return underlyingClassification{Sector: s}
	}
	return underlyingClassification{}
}

// classificationSettledWithoutBroker reports whether the free sources alone
// decide a symbol, so the snapshot can skip the contract-details round-trip.
func classificationSettledWithoutBroker(symbol string) bool {
	key := strings.ToUpper(strings.TrimSpace(symbol))
	if _, ok := fundLookThrough[key]; ok {
		return true
	}
	_, ok := spx.SectorOf(key)
	return ok
}

func isFundStockType(stockType string) bool {
	t := strings.ToUpper(strings.TrimSpace(stockType))
	switch t {
	case "ETF", "ETN", "ETC", "CEF", "FUND":
		return true
	}
	return strings.Contains(t, "FUND")
}

// gicsFromIBKR maps the broker's Bloomberg-style industry, refined by its
// category where one industry straddles GICS sectors, onto GICS. The map is
// deliberately conservative: an unknown pairing stays unclassified rather
// than landing in a plausible sector.
func gicsFromIBKR(industry, category string) (string, bool) {
	ind := strings.ToUpper(strings.TrimSpace(industry))
	cat := strings.ToUpper(strings.TrimSpace(category))
	switch ind {
	case "TECHNOLOGY":
		return "Information Technology", true
	case "COMMUNICATIONS":
		if strings.Contains(cat, "INTERNET") && !strings.Contains(cat, "MEDIA") {
			// Broker files online retailers and marketplaces here; GICS does not.
			return "Consumer Discretionary", true
		}
		return "Communication Services", true
	case "CONSUMER, CYCLICAL":
		return "Consumer Discretionary", true
	case "CONSUMER, NON-CYCLICAL":
		switch {
		case strings.Contains(cat, "PHARMA"), strings.Contains(cat, "BIOTECH"), strings.Contains(cat, "HEALTH"):
			return "Health Care", true
		case strings.Contains(cat, "COMMERCIAL SERVICES"):
			return "Industrials", true
		}
		return "Consumer Staples", true
	case "FINANCIAL":
		if strings.Contains(cat, "REIT") {
			return "Real Estate", true
		}
		return "Financials", true
	case "INDUSTRIAL":
		return "Industrials", true
	case "ENERGY":
		return "Energy", true
	case "BASIC MATERIALS":
		return "Materials", true
	case "UTILITIES":
		return "Utilities", true
	}
	return "", false
}
