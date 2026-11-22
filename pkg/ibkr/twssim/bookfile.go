package twssim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// BookFile is a book as JSON, for canary-sim -book: the account's cash by
// currency and its positions, each naming its contract in full. The
// catalogue of [Default] (the regime's inputs and SYNA, SYNB) stays listed
// beside it; the file's positions replace Default's. The loader is strict:
// an unknown field, a contract without its identity or an account other than
// [DefaultAccount] is refused, so a book can never carry a real account id.
type BookFile struct {
	Account   string             `json:"account"`
	Base      string             `json:"base_currency"`
	Cash      map[string]float64 `json:"cash"`
	FX        map[string]float64 `json:"fx,omitempty"`
	Positions []BookPosition     `json:"positions"`
}

// BookPosition is one holding and its contract; quantity 0 lists the
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
	for ccy, rate := range f.FX {
		b.SetFX(strings.ToUpper(ccy), rate)
	}
	for ccy, amount := range f.Cash {
		b.SetCash(strings.ToUpper(ccy), amount)
	}
	for i, p := range f.Positions {
		in := Instrument{ConID: p.ConID, Symbol: strings.ToUpper(p.Symbol), SecType: strings.ToUpper(p.SecType), Exchange: p.Exchange, PrimaryExch: p.PrimaryExchange,
			Currency: strings.ToUpper(p.Currency), TradingClass: p.TradingClass, LongName: "SYNTHETIC " + strings.ToUpper(p.Symbol), Price: p.Last, Bid: p.Bid, Ask: p.Ask,
			UnderConID: p.UnderConID, Right: strings.ToUpper(p.Right), Strike: p.Strike, Expiry: p.Expiry, Multiplier: p.Multiplier, Spread: 0.02}
		switch {
		case in.ConID <= 0 || in.Symbol == "" || in.Currency == "" || in.Exchange == "" || in.Price <= 0:
			return nil, fmt.Errorf("twssim: book: position %d needs con_id, symbol, currency, exchange and last", i)
		case in.SecType == "OPT" && (in.UnderConID <= 0 || (in.Right != "C" && in.Right != "P") || in.Strike <= 0 || len(in.Expiry) != 8 || in.Multiplier <= 0 || in.TradingClass == ""):
			return nil, fmt.Errorf("twssim: book: option position %d needs underlying_con_id, right, strike, expiry, multiplier and trading_class", i)
		case in.SecType != "STK" && in.SecType != "OPT":
			return nil, fmt.Errorf("twssim: book: position %d: sec_type %q (STK or OPT)", i, p.SecType)
		}
		if in.SecType == "OPT" {
			in.LocalSymbol = fmt.Sprintf("%-6s%s%s%08d", in.Symbol, in.Expiry[2:], in.Right, int(in.Strike*1000+0.5))
		}
		b.AddInstrument(in)
		b.SetPosition(in.ConID, p.Quantity, p.AvgCost)
	}
	return b, nil
}
