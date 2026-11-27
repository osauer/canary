package twssim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// BookFile is a book as JSON, for canary-sim -book: the account's cash by
// currency and its positions, each naming its contract in full. The
// catalogue of [Default] (the regime's inputs and SYNA, SYNB) stays listed
// beside it; the file's positions replace Default's. The loader is strict:
// an unknown field, a contract without its identity or an account other than
// [DefaultAccount] is refused, so a book can never carry a real account id.
// FX gives each currency other than the base its rate, in base per unit (as
// the broker's ExchangeRate rows read); every currency the cash or a
// position is in needs one, and each listed rate lists its IDEALPRO pair
// against the base (see [Book.SetFX]). A stock the catalogue already lists
// (same symbol and currency) takes the catalogue's contract id, and options
// naming the file's id follow it.
type BookFile struct {
	Account   string             `json:"account"`
	Base      string             `json:"base_currency"`
	Cash      map[string]float64 `json:"cash"`
	FX        map[string]float64 `json:"fx,omitempty"`
	Positions []BookPosition     `json:"positions"`
}

// BookPosition is one holding and its contract. An option's expiry is a
// date (YYYYMMDD) or "+Nw": the Friday N weeks after the first Friday after
// today (UTC), so a committed example never expires; quantity 0 lists the
// contract without holding it (an option's underlying, say). Options name their
// underlying's contract id, right, strike, expiry (YYYYMMDD), multiplier and
// trading class; a stock leaves them empty.
type BookPosition struct {
	ConID           int     `json:"con_id"`
	Symbol          string  `json:"symbol"`
	SecType         string  `json:"sec_type"`
	Currency        string  `json:"currency"`
	Exchange        string  `json:"exchange"`
	PrimaryExchange string  `json:"primary_exchange,omitempty"`
	TradingClass    string  `json:"trading_class,omitempty"`
	Quantity        float64 `json:"quantity"`
	AvgCost         float64 `json:"avg_cost"`
	Last            float64 `json:"last"`
	Bid             float64 `json:"bid,omitempty"`
	Ask             float64 `json:"ask,omitempty"`
	UnderConID      int     `json:"underlying_con_id,omitempty"`
	Right           string  `json:"right,omitempty"`
	Strike          float64 `json:"strike,omitempty"`
	Expiry          string  `json:"expiry,omitempty"`
	Multiplier      float64 `json:"multiplier,omitempty"`
	// DailyDrift is the daily bars' trend per session into Last (see
	// Instrument.Drift): -0.0005 makes the bars fall into Last by about 0.05% a session.
	DailyDrift float64 `json:"daily_drift,omitempty"`
	// PriorClose is the close of the last settled session (see
	// Instrument.PriorClose); unset, the daily bars end on Last.
	PriorClose float64 `json:"prior_close,omitempty"`
	// Defunct marks a dead line (a delisted name on a grey-market venue):
	// last 0, no quotes.
	Defunct bool `json:"defunct,omitempty"`
}

// LoadBook reads a book file (see [BookFile]).
func LoadBook(path string) (*Book, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseBook(raw)
}

// ParseBook builds a book from [BookFile] JSON.
func ParseBook(raw []byte) (*Book, error) {
	var f BookFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("twssim: book: %w", err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("twssim: book: one JSON object expected")
	}
	if f.Account != DefaultAccount {
		return nil, fmt.Errorf("twssim: book: account %q refused; a book file carries the placeholder %s only", f.Account, DefaultAccount)
	}
	b := Default()
	b.SetPosition(900001, 0, 0)
	if f.Base != "" {
		b.base = strings.ToUpper(f.Base)
		b.fx = map[string]float64{b.base: 1}
		b.cash = map[string]float64{b.base: 0}
	}
	fx := map[string]bool{b.base: true}
	for ccy, rate := range f.FX {
		ccy = strings.ToUpper(ccy)
		switch {
		case ccy == b.base:
			return nil, fmt.Errorf("twssim: book: fx names the base currency %s, whose rate is 1", b.base)
		case !(rate > 0) || math.IsInf(rate, 0):
			return nil, fmt.Errorf("twssim: book: fx rate for %s must be a positive number of %s per %s", ccy, b.base, ccy)
		case !fxPairModelled(ccy, b.base):
			return nil, fmt.Errorf("twssim: book: no IDEALPRO pair of %s and %s is modelled", ccy, b.base)
		}
		fx[ccy] = true
		b.SetFX(ccy, rate)
	}
	var currencies []string
	for ccy, amount := range f.Cash {
		b.SetCash(strings.ToUpper(ccy), amount)
		currencies = append(currencies, strings.ToUpper(ccy))
	}
	// A stock the catalogue already lists (a regime input, SYNB) takes the
	// catalogue's contract id, and options on it follow, so a symbol never
	// names two contracts.
	listed := map[string]int{}
	for _, in := range b.instr {
		if in.SecType == "STK" {
			listed[in.Symbol+"/"+in.Currency] = in.ConID
		}
	}
	remap := map[int]int{}
	for _, p := range f.Positions {
		if id, ok := listed[strings.ToUpper(p.Symbol)+"/"+strings.ToUpper(p.Currency)]; ok && strings.EqualFold(p.SecType, "STK") && id != p.ConID {
			remap[p.ConID] = id
		}
	}
	for i, p := range f.Positions {
		if id, ok := remap[p.ConID]; ok && strings.EqualFold(p.SecType, "STK") {
			p.ConID = id
		}
		if id, ok := remap[p.UnderConID]; ok {
			p.UnderConID = id
		}
		if rest, ok := strings.CutPrefix(p.Expiry, "+"); ok {
			weeks, err := strconv.Atoi(strings.TrimSuffix(rest, "w"))
			if err != nil || !strings.HasSuffix(rest, "w") || weeks < 0 {
				return nil, fmt.Errorf("twssim: book: position %d: expiry %q (YYYYMMDD or +Nw)", i, p.Expiry)
			}
			short, _ := time.Parse("20060102", OptionExpiries(time.Now())[0])
			p.Expiry = short.AddDate(0, 0, 7*weeks).Format("20060102")
		}
		in := Instrument{ConID: p.ConID, Symbol: strings.ToUpper(p.Symbol), SecType: strings.ToUpper(p.SecType), Exchange: p.Exchange, PrimaryExch: p.PrimaryExchange,
			Currency: strings.ToUpper(p.Currency), TradingClass: p.TradingClass, LongName: "SYNTHETIC " + strings.ToUpper(p.Symbol), Price: p.Last, Bid: p.Bid, Ask: p.Ask,
			UnderConID: p.UnderConID, Right: strings.ToUpper(p.Right), Strike: p.Strike, Expiry: p.Expiry, Multiplier: p.Multiplier, Spread: 0.02, Drift: p.DailyDrift, PriorClose: p.PriorClose}
		switch {
		case in.ConID <= 0 || in.Symbol == "" || in.Currency == "" || in.Exchange == "" || (in.Price <= 0 && !p.Defunct) || (p.Defunct && (in.Price != 0 || in.SecType != "STK")):
			return nil, fmt.Errorf("twssim: book: position %d needs con_id, symbol, currency, exchange and last (0 only for a defunct stock)", i)
		case in.SecType == "OPT" && (in.UnderConID <= 0 || (in.Right != "C" && in.Right != "P") || in.Strike <= 0 || len(in.Expiry) != 8 || in.Multiplier <= 0 || in.TradingClass == ""):
			return nil, fmt.Errorf("twssim: book: option position %d needs underlying_con_id, right, strike, expiry, multiplier and trading_class", i)
		case in.SecType != "STK" && in.SecType != "OPT":
			return nil, fmt.Errorf("twssim: book: position %d: sec_type %q (STK or OPT)", i, p.SecType)
		}
		if in.SecType == "OPT" {
			in.LocalSymbol = fmt.Sprintf("%-6s%s%s%08d", in.Symbol, in.Expiry[2:], in.Right, int(in.Strike*1000+0.5))
		}
		if p.Defunct {
			in.Spread, in.Defunct = 0, true
		}
		b.AddInstrument(in)
		b.SetPosition(in.ConID, p.Quantity, p.AvgCost)
		currencies = append(currencies, in.Currency)
	}
	// Every currency held must have its rate: without it the account's
	// values in the base would be wrong and the daemon, which prices an
	// order in another currency through its IDEALPRO pair, refuses every
	// order in it.
	slices.Sort(currencies)
	for _, ccy := range currencies {
		if !fx[ccy] {
			return nil, fmt.Errorf("twssim: book: no fx rate for %s in a %s book; state it as \"fx\": {\"%s\": <%s per %s>}", ccy, b.base, ccy, b.base, ccy)
		}
	}
	return b, nil
}
