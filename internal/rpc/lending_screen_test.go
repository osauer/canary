package rpc

import (
	"math"
	"testing"
)

func TestLendingScreenBoundsAndUnavailableContract(t *testing.T) {
	for _, p := range []LendingScreenParams{{MinRate: -1}, {MinRate: math.Inf(1)}, {Limit: 101}, {Limit: -1}, {Exclude: []string{"$BAD"}}} {
		if _, err := NormalizeLendingScreenParams(p); err == nil {
			t.Fatal("invalid screen accepted")
		}
	}
	p, err := NormalizeLendingScreenParams(LendingScreenParams{Exclude: []string{"bbb", "AAA", "aaa"}})
	if err != nil || p.Limit != 50 || len(p.Exclude) != 2 || p.Exclude[0] != "AAA" {
		t.Fatal("normalization")
	}
	r := LendingScreenResult{Kind: "lending_screen", Universe: "us_short_stock", Status: "unavailable", Params: p}
	if err := ValidateLendingScreenResult(r, p); err != nil {
		t.Fatal(err)
	}
	r.Status = "observed"
	if ValidateLendingScreenResult(r, p) == nil {
		t.Fatal("undated empty treated as observed")
	}
	r.Status = "unavailable"
	r.Params.Exclude = nil
	if ValidateLendingScreenResult(r, p) == nil {
		t.Fatal("exclusions changed")
	}
}

func TestLendingScreenSortingMissingLastAndBounds(t *testing.T) {
	for _, p := range []LendingScreenParams{{MinPrice: -1}, {MinAvgDollarVolume20D: math.NaN()}, {MinHighDates: 8}, {SortBy: "made_up"}, {SortDir: "sideways"}} {
		if _, err := NormalizeLendingScreenParams(p); err == nil {
			t.Fatal("invalid filter accepted")
		}
	}
	a := LendingScreenRow{Symbol: "AAA", FeeRate: new(60.0), Market: &LendingMarketRow{Price: new(10.0)}}
	b := LendingScreenRow{Symbol: "BBB", FeeRate: new(70.0)}
	for _, dir := range []string{"asc", "desc"} {
		if CompareLendingScreenRows(a, b, LendingScreenParams{SortBy: "price", SortDir: dir}) >= 0 {
			t.Fatal("missing sorted first")
		}
	}
	if LendingScreenMatchesMarket(nil, LendingScreenParams{MinPrice: 5}) {
		t.Fatal("unknown price passed")
	}
	if !LendingScreenMarketCovered(&LendingMarketRow{Status: "partial", Price: new(10.0)}, LendingScreenParams{MinPrice: 5}) {
		t.Fatal("unrelated missing YTD blocked price filter coverage")
	}
}
