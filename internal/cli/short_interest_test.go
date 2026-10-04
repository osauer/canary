package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/osauer/canary/v2/internal/rpc"
	"testing"
)

type shortInterestConn struct {
	calls    int
	mismatch bool
}

func (c *shortInterestConn) Call(_ context.Context, method string, params, out any) error {
	c.calls++
	if method != rpc.MethodShortInterestScreen {
		panic("wrong method")
	}
	p := params.(rpc.ShortInterestScreenParams)
	if c.mismatch {
		p.SortDir = "asc"
	}
	*out.(*rpc.ShortInterestScreenResult) = rpc.ShortInterestScreenResult{Kind: "short_interest_screen", Status: "pending", Source: "FINRA equity short interest", SourceURL: "https://www.finra.org/finra-data/browse-catalog/equity-short-interest/files", Params: p, Coverage: rpc.ShortInterestCoverage{Complete: true}, Rows: []rpc.ShortInterestRow{}}
	return nil
}
func (*shortInterestConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestShortInterestCLI(t *testing.T) {
	conn := &shortInterestConn{}
	var output, errs bytes.Buffer
	env := &Env{Conn: conn, Stdout: &output, Stderr: &errs}
	for _, args := range [][]string{{"screen", "--limit", "101"}, {"screen", "--min-price", "-1"}, {"screen", "--sort-by", "short_interest_pct_float"}} {
		if Run(t.Context(), env, "short-interest", args) == 0 || conn.calls != 0 {
			t.Fatal("bad scope reached daemon")
		}
	}
	if Run(t.Context(), env, "short-interest", []string{"screen", "--listed-only", "--min-average-volume", "1000000", "--json"}) != 0 {
		t.Fatal(errs.String())
	}
	var r rpc.ShortInterestScreenResult
	if json.Unmarshal(output.Bytes(), &r) != nil || !r.Params.ListedOnly || r.Params.MinAverageVolume != 1000000 {
		t.Fatal("query lost")
	}
	conn.mismatch = true
	if Run(t.Context(), env, "short-interest", []string{"screen", "--json"}) == 0 {
		t.Fatal("wrong query response accepted")
	}
}
