package daemon

import (
	"fmt"
	"strings"

	"github.com/osauer/canary/v3/internal/rpc"
)

// adaptiveSupported is the one rule for whether an order on this contract can
// carry IBKR's Adaptive algorithm: the broker layer's contract shape (pkg/ibkr
// validateAdaptiveOrder), a stock or ETF routed SMART (an empty exchange is
// SMART by default). Any other contract (an option, a combination, a future,
// a listing on its own exchange) goes as a plain limit at the same reviewed
// price. The preview and add sizing refuse Adaptive on such a contract; the
// owner queue leaves it off. Reduce applies stricter conditions of its own
// (reduceAdaptiveSupported).
func adaptiveSupported(contract rpc.ContractParams) bool {
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "ETF":
	default:
		return false
	}
	exchange := strings.ToUpper(strings.TrimSpace(contract.Exchange))
	return exchange == "" || exchange == "SMART"
}

// adaptiveUnsupportedMessage refuses Adaptive on a contract that is not a stock
// or ETF.
const adaptiveUnsupportedMessage = "IBKR's Adaptive algorithm is sent only on a SMART-routed stock or ETF limit; leave Adaptive off to send a plain limit"

// adaptiveRefusal is the owner's sentence for an Adaptive request on this
// contract, or "" when the contract can carry it.
func adaptiveRefusal(contract rpc.ContractParams) string {
	secType := strings.ToUpper(strings.TrimSpace(contract.SecType))
	switch {
	case adaptiveSupported(contract):
		return ""
	case secType != "STK" && secType != "ETF":
		return adaptiveUnsupportedMessage
	default:
		return fmt.Sprintf("IBKR's Adaptive algorithm runs only on SMART, and this order goes to %s; leave Adaptive off to send a plain limit", contract.Exchange)
	}
}
