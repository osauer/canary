package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type lendingRatesConn struct {
	calls    int
	mismatch bool
}

func (c *lendingRatesConn) Call(_ context.Context, method string, params, out any) error {
	c.calls++
	if method != rpc.MethodMarketEventsSnapshot {
		panic("unexpected effectful method")
	}
	p := params.(rpc.MarketEventsParams)
	r := out.(*rpc.MarketEventsResult)
	r.Symbols = p.Symbols
	if c.mismatch {
		r.Symbols = []string{"OTHER"}
	}
	rate := 75.0
	r.BorrowFeeCoverage = []rpc.MarketEventBorrowFeeCoverage{{Symbol: "AAA", FeeRate: &rate, Status: rpc.BorrowFeeCoverageStale}, {Symbol: "BBB", Status: rpc.BorrowFeeCoverageMissing}}
	return nil
}
func (*lendingRatesConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}
func TestLendingRatesScopeReadOnlyAndMissingEvidence(t *testing.T) {
	c := &lendingRatesConn{}
	var out, errs bytes.Buffer
	env := &Env{Conn: c, Stdout: &out, Stderr: &errs}
	for _, args := range [][]string{{"rates"}, {"rates", "--symbols", "AAA,$BAD"}, {"rates", "--symbols", strings.Repeat("AAA,", 100) + ""}, {"rates", "--symbols", "AAA", "extra"}} {
		if Run(t.Context(), env, "lending", args) == 0 || c.calls != 0 {
			t.Fatal("invalid request reached daemon")
		}
	}
	if Run(t.Context(), env, "lending", []string{"rates", "--symbols", "bbb,AAA,aaa", "--json"}) != 0 {
		t.Fatal(errs.String())
	}
	var r rpc.MarketEventsResult
	if json.Unmarshal(out.Bytes(), &r) != nil || len(r.Symbols) != 2 || r.Symbols[0] != "AAA" || r.BorrowFeeCoverage[0].Status != "stale" || r.BorrowFeeCoverage[1].FeeRate != nil {
		t.Fatal("evidence changed")
	}
	c.mismatch = true
	if Run(t.Context(), env, "lending", []string{"rates", "--symbols", "AAA", "--json"}) == 0 {
		t.Fatal("scope substitution accepted")
	}
}

type lendingScreenConn struct{ calls int }

func (c *lendingScreenConn) Call(_ context.Context, method string, params, out any) error {
	c.calls++
	if method != rpc.MethodLendingScreen {
		panic("wrong method")
	}
	p := params.(rpc.LendingScreenParams)
	*out.(*rpc.LendingScreenResult) = rpc.LendingScreenResult{Kind: "lending_screen", Universe: "us_short_stock", Status: "unavailable", Params: p, Rows: []rpc.LendingScreenRow{}}
	return nil
}
func (*lendingScreenConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}
func TestLendingScreenCLIReadOnlyAndFlags(t *testing.T) {
	c := &lendingScreenConn{}
	var out, errs bytes.Buffer
	env := &Env{Conn: c, Stdout: &out, Stderr: &errs}
	for _, args := range [][]string{{"screen", "--limit", "101"}, {"screen", "--symbols", "AAA"}, {"fees", "--min-rate", "50"}, {"screen", "--exclude", "$BAD"}} {
		if Run(t.Context(), env, "lending", args) == 0 || c.calls != 0 {
			t.Fatal("invalid arguments reached daemon")
		}
	}
	if Run(t.Context(), env, "lending", []string{"screen", "--min-rate", "50", "--exclude", "bbb,aaa", "--limit", "10", "--json"}) != 0 {
		t.Fatal(errs.String())
	}
	var r rpc.LendingScreenResult
	if json.Unmarshal(out.Bytes(), &r) != nil || r.Status != "unavailable" || r.Params.Limit != 10 || r.Params.Exclude[0] != "AAA" {
		t.Fatal("scope or unavailable evidence changed")
	}
}

type lendingMarketConn struct {
	calls    int
	mismatch bool
}

func (c *lendingMarketConn) Call(_ context.Context, method string, params, out any) error {
	c.calls++
	if method != rpc.MethodLendingMarket {
		panic("wrong method")
	}
	p := params.(rpc.LendingMarketParams)
	result := rpc.LendingMarketResult{Kind: "lending_market", Symbols: p.Symbols, Rows: []rpc.LendingMarketRow{{Symbol: "AAA", Status: "pending"}}}
	if c.mismatch {
		result.Rows[0].Symbol = "BBB"
	}
	*out.(*rpc.LendingMarketResult) = result
	return nil
}
func (*lendingMarketConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}
func TestLendingMarketCLIContract(t *testing.T) {
	c := &lendingMarketConn{}
	var out, errs bytes.Buffer
	env := &Env{Conn: c, Stdout: &out, Stderr: &errs}
	for _, args := range [][]string{{"market"}, {"market", "--symbols", "AAA", "--limit", "10"}, {"market", "--symbols", "$BAD"}} {
		if Run(t.Context(), env, "lending", args) == 0 || c.calls != 0 {
			t.Fatal("invalid scope reached daemon")
		}
	}
	if Run(t.Context(), env, "lending", []string{"market", "--symbols", "aaa", "--json"}) != 0 {
		t.Fatal(errs.String())
	}
	var result rpc.LendingMarketResult
	if json.Unmarshal(out.Bytes(), &result) != nil || result.Rows[0].Status != "pending" {
		t.Fatal("pending evidence changed")
	}
	c.mismatch = true
	if Run(t.Context(), env, "lending", []string{"market", "--symbols", "AAA"}) == 0 {
		t.Fatal("wrong symbol accepted")
	}
}
