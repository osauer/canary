package ibkr

import (
	"strings"
	"testing"
)

func TestFetchHistoricalDailyTradeBarsRequiresExactContract(t *testing.T) {
	t.Parallel()
	connector := &Connector{}
	if _, err := connector.FetchHistoricalDailyTradeBarsWithContract(t.Context(), Contract{Symbol: "ACME", SecType: "STK"}, 20, 0); err == nil || !strings.Contains(err.Error(), "exact contract ID") {
		t.Fatalf("missing exact-contract error=%v", err)
	}
}
