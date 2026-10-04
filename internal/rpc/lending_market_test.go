package rpc

import (
	"testing"
	"time"
)

func TestLendingMarketRejectsScopeAndMissingClocks(t *testing.T) {
	r := LendingMarketResult{Kind: "lending_market", Symbols: []string{"AAA"}, Rows: []LendingMarketRow{{Symbol: "AAA", Status: "pending"}}}
	if ValidateLendingMarketResult(r, []string{"AAA"}) != nil {
		t.Fatal("pending invalid")
	}
	if ValidateLendingMarketResult(r, []string{"BBB"}) == nil {
		t.Fatal("scope mismatch accepted")
	}
	r.Rows[0].Price = new(5.0)
	if ValidateLendingMarketResult(r, []string{"AAA"}) == nil {
		t.Fatal("undated price accepted")
	}
	r.Rows[0].PriceAt = time.Now()
	r.Rows[0].PriceKind = "close"
	if ValidateLendingMarketResult(r, []string{"AAA"}) == nil {
		t.Fatal("unidentified value accepted")
	}
}
