package twssim

import (
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
