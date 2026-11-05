package rpc

import "strings"

// Broker names on an InstrumentID, as HealthResult.Broker gives them.
const (
	InstrumentBrokerIBKR  = "ibkr"
	InstrumentBrokerOpenD = "opend"
)

// InstrumentID is the broker-neutral identity of an instrument. Present when
// the daemon knows both the broker's own id and the listing venue; absent on a
// row without a listing venue and on answers from a daemon that predates it, so
// con_id stays the IBKR identity and stays optional.
//
// It carries no listing symbol. To net one stock across two daemons, group
// stock rows on JoinKey with the row's own symbol, never on ID: ID's form is
// per broker ("265598", "HK.00700").
//
// A request may give it instead of con_id on the methods that resolve it. It is
// a plain value, not a pointer, because ContractParams is compared and used as
// a map key.
type InstrumentID struct {
	Broker   string `json:"broker"`             // ibkr | opend
	ID       string `json:"id"`                 // ibkr: ConID as text; opend: "HK.00700"
	Market   string `json:"market,omitempty"`   // us | de | uk | hk | sg | cn | jp
	Venue    string `json:"venue,omitempty"`    // the listing venue: SEHK, NASDAQ, SGX, XETRA ...
	Currency string `json:"currency,omitempty"` // the trading currency
}

// JoinKey is the identity two brokers share for one listing: the venue and the
// row's symbol. A numeric symbol loses its leading zeros, because IBKR writes
// Tencent 700 and OpenD writes it 00700. Empty when the venue is unknown.
func (id InstrumentID) JoinKey(symbol string) string {
	symbol = joinSymbol(symbol)
	if id.Venue == "" || symbol == "" {
		return ""
	}
	return id.Venue + ":" + symbol
}

// SameListedSymbol says whether two symbols name one listing under JoinKey's rule.
func SameListedSymbol(a, b string) bool { return joinSymbol(a) == joinSymbol(b) }

func joinSymbol(symbol string) string {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" || strings.Trim(symbol, "0123456789") != "" {
		return symbol
	}
	if trimmed := strings.TrimLeft(symbol, "0"); trimmed != "" {
		return trimmed
	}
	return "0"
}

// venueAliases are the listing venues IBKR names differently from OpenD and the
// exchanges themselves.
var venueAliases = map[string]string{"IBIS": "XETRA", "ISLAND": "NASDAQ", "TSEJ": "TSE"}

// venueMarkets are the route families of the listing venues Canary reads.
var venueMarkets = map[string]string{
	"NASDAQ": "us", "NYSE": "us", "AMEX": "us", "ARCA": "us", "BATS": "us", "PINK": "us",
	"XETRA": "de", "LSE": "uk", "LSEETF": "uk",
	"SEHK": "hk", "SGX": "sg", "SSE": "cn", "SZSE": "cn", "TSE": "jp",
}

// ListingVenue is the venue a row is listed on: its primary listing, else its
// exchange. A smart route names no listing, so it yields nothing rather than a
// venue no other broker would join on.
func ListingVenue(primary, exchange string) string {
	for _, venue := range []string{primary, exchange} {
		venue = strings.ToUpper(strings.TrimSpace(venue))
		if venue == "" || venue == "SMART" {
			continue
		}
		if alias, ok := venueAliases[venue]; ok {
			return alias
		}
		return venue
	}
	return ""
}

// NewInstrumentID builds the identity of a row. Without an id or a listing venue
// it is zero, and the row carries none: a row with an identity always joins.
func NewInstrumentID(broker, id, venue, currency string) InstrumentID {
	if id == "" || venue == "" {
		return InstrumentID{}
	}
	return InstrumentID{Broker: broker, ID: id, Market: venueMarkets[venue], Venue: venue, Currency: currency}
}
