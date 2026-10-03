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
