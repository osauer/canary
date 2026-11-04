package broker

import (
	"os/exec"
	"strings"
	"testing"
)

func TestKindZeroValueIsIBKR(t *testing.T) {
	if Kind("").Normalized() != KindIBKR {
		t.Fatalf("zero kind normalizes to %q, want ibkr", Kind("").Normalized())
	}
	if Kind(" OpenD ").Normalized() != KindOpenD {
		t.Fatalf("mixed-case kind normalizes to %q", Kind(" OpenD ").Normalized())
	}
	for _, k := range []Kind{"", "ibkr", "opend"} {
		if !k.Valid() {
			t.Fatalf("kind %q should be valid", k)
		}
	}
	if Kind("tws").Valid() {
		t.Fatal("unknown kind should be invalid")
	}
}

func TestSessionBindingIsAValue(t *testing.T) {
	var zero SessionBinding
	if !zero.IsZero() {
		t.Fatal("zero binding must report IsZero")
	}
	a := SessionBinding{Kind: KindIBKR, Generation: 1, Epoch: 7}
	b := SessionBinding{Kind: KindIBKR, Generation: 1, Epoch: 7}
	if a != b {
		t.Fatal("equal bindings must compare equal")
	}
	// A connector rebuild bumps the daemon's generation while the new
	// connection's epoch counter may repeat the old value: the bindings
	// differ by generation alone.
	c := SessionBinding{Kind: KindIBKR, Generation: 2, Epoch: 7}
	if a == c {
		t.Fatal("bindings on different generations must differ")
	}
	if c.IsZero() || a.IsZero() {
		t.Fatal("a captured binding is never zero")
	}
	seen := map[SessionBinding]bool{a: true}
	if !seen[b] || seen[c] {
		t.Fatal("bindings must work as map keys by value")
	}
}

func TestInstrumentCanonicalString(t *testing.T) {
	cases := []struct {
		in   Instrument
		want string
	}{
		{Instrument{Broker: KindIBKR, ID: "265598", Symbol: "AAPL"}, "ibkr:265598"},
		{Instrument{ID: "265598"}, "ibkr:265598"},
		{Instrument{Broker: KindOpenD, ID: "HK.00700"}, "opend:HK.00700"},
		{Instrument{Symbol: "spy"}, "ibkr:SPY/STK"},
		{Instrument{Broker: KindOpenD, Symbol: "00700", SecType: "stk"}, "opend:00700/STK"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.in, got, c.want)
		}
	}
	if n, ok := (Instrument{ID: "265598"}).IDInt(); !ok || n != 265598 {
		t.Fatalf("IDInt: got %d %v", n, ok)
	}
	if _, ok := (Instrument{ID: "HK.00700"}).IDInt(); ok {
		t.Fatal("a market-prefixed code is not an integer id")
	}
}

func TestBaseCurrencyProvenance(t *testing.T) {
	if !BaseCurrencyExplicitTag.Proven() || !BaseCurrencyValueSuffix.Proven() {
		t.Fatal("tag and suffix provenance prove the base currency")
	}
	if BaseCurrencyUnknown.Proven() || BaseCurrencyUnitExchangeRate.Proven() {
		t.Fatal("unknown and unit-rate provenance never prove it")
	}
}

// TestNoBrokerClientImports pins the seam's boundary: the type package must
// not depend on a broker client or on the cross-surface contract.
func TestNoBrokerClientImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for dep := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		for _, forbidden := range []string{"/pkg/ibkr", "/pkg/futu", "/internal/rpc", "/internal/daemon"} {
			if strings.Contains(dep, forbidden) {
				t.Fatalf("internal/broker depends on %s through %s", forbidden, dep)
			}
		}
	}
}
