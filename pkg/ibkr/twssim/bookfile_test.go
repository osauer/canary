package twssim

import (
	"math"
	"os"
	"strings"
	"testing"
)

// The committed example book loads: six long calls, two puts and the SYNB
// bear put spread beside 100 SYNA shares.
func TestOwnerMixBookLoads(t *testing.T) {
	b, err := LoadBook("testdata/owner-mix-book.json")
	if err != nil {
		t.Fatal(err)
	}
	calls, puts, short := 0, 0, 0
	for _, r := range b.portfolioLocked() {
		switch {
		case r.in.SecType == "OPT" && r.in.Right == "C" && r.pos.Quantity > 0:
			calls++
		case r.in.SecType == "OPT" && r.in.Right == "P" && r.pos.Quantity > 0:
			puts++
		case r.in.SecType == "OPT" && r.pos.Quantity < 0:
			short++
		}
	}
	if calls != 6 || puts != 3 || short != 1 || b.Cash("USD") != 80000 {
		t.Fatalf("calls %d, long puts %d (two plus the spread's), short %d, cash %v", calls, puts, short, b.Cash("USD"))
	}
	if p, ok := b.Position(900001); !ok || p.Quantity != 100 {
		t.Fatalf("SYNA shares %+v", p)
	}
}

// The loader refuses a real-looking account and any unknown field.
func TestBookFileIsStrict(t *testing.T) {
	raw, err := os.ReadFile("testdata/owner-mix-book.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(string) string{
		"another account": func(s string) string { return strings.Replace(s, `"U1234567"`, `"U7654321"`, 1) },
		"unknown field":   func(s string) string { return strings.Replace(s, `"cash"`, `"cash_extra": 1, "cash"`, 1) },
		"option without identity": func(s string) string {
			return strings.Replace(s, `"right": "C", "strike": 55,`, `"strike": 55,`, 1)
		},
	} {
		if _, err := ParseBook([]byte(mutate(string(raw)))); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// A EUR-base book holding USD options lists the EUR.USD pair at its stated
// rate and reports the rate on the ledger; without the rate it is refused
// with the sentence naming it.
func TestEURBaseBookListsItsPair(t *testing.T) {
	b, err := LoadBook("testdata/eur-base-book.json")
	if err != nil {
		t.Fatal(err)
	}
	pairs := b.lookup(0, "EUR", "CASH", "USD")
	if b.Base() != "EUR" || len(pairs) != 1 || pairs[0].Exchange != "IDEALPRO" || pairs[0].LocalSymbol != "EUR.USD" || math.Abs(pairs[0].Price-1/0.92) > 1e-6 {
		t.Fatalf("base %s, EUR.USD %+v", b.Base(), pairs)
	}
	if q := quoteOf(pairs[0]); !(q.bid > 0 && q.bid < q.ask) {
		t.Fatalf("pair quote %+v", q)
	}
	rate := ""
	for _, row := range b.accountValuesLocked() {
		if row.key == "ExchangeRate" && row.currency == "USD" {
			rate = row.value
		}
	}
	if rate != num(0.92) {
		t.Fatalf("USD ExchangeRate row %q", rate)
	}
	raw, err := os.ReadFile("testdata/eur-base-book.json")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseBook([]byte(strings.Replace(string(raw), `"fx": {"USD": 0.92},`, "", 1)))
	if err == nil || !strings.Contains(err.Error(), "no fx rate for USD in a EUR book") {
		t.Fatalf("a book without its rate: %v", err)
	}
	for name, fx := range map[string]string{"base": `"fx": {"EUR": 1, "USD": 0.92},`, "zero": `"fx": {"USD": 0},`, "unmodelled": `"fx": {"USD": 0.92, "SEK": 0.09},`} {
		if _, err := ParseBook([]byte(strings.Replace(string(raw), `"fx": {"USD": 0.92},`, fx, 1))); err == nil {
			t.Errorf("fx %s: loaded", name)
		}
	}
}

// A book stock the catalogue already lists under another contract id takes
// the catalogue's id, and its options follow: the symbol names one contract.
func TestBookStockTakesTheCatalogueID(t *testing.T) {
	raw, err := os.ReadFile("testdata/eur-base-book.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseBook([]byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), `"con_id": 900002,`, `"con_id": 981001,`), `"underlying_con_id": 900002,`, `"underlying_con_id": 981001,`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := b.lookup(0, "SYNB", "STK", "USD"); len(got) != 1 || got[0].ConID != 900002 {
		t.Fatalf("SYNB listings %+v", got)
	}
	if in, ok := b.Contract(930009); !ok || in.UnderConID != 900002 {
		t.Fatalf("SYNB put %+v", in)
	}
}
