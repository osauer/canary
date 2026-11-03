package rpc

import "strings"

// CurrencyDecimals is the number of minor-unit digits a money figure in the
// currency is written with: none for yen and won, two for every other
// currency Canary renders. Renderers consult it so a JPY amount never reads
// "¥ 1,234.00".
func CurrencyDecimals(currency string) int {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "JPY", "KRW":
		return 0
	default:
		return 2
	}
}
