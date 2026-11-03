package rpc

import "testing"

func TestCurrencyDecimalsZeroForYenAndWonOnly(t *testing.T) {
	for ccy, want := range map[string]int{"JPY": 0, " jpy ": 0, "KRW": 0, "HKD": 2, "SGD": 2, "USD": 2, "EUR": 2, "": 2, "AUD": 2} {
		if got := CurrencyDecimals(ccy); got != want {
			t.Fatalf("CurrencyDecimals(%q) = %d, want %d", ccy, got, want)
		}
	}
}
