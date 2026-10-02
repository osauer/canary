package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/financing"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

type lendingCLIConn struct {
	calls  int
	method string
	params rpc.FinancingFeesParams
	result rpc.FinancingFeesResult
}

func (c *lendingCLIConn) Call(_ context.Context, method string, params, out any) error {
	c.calls++
	c.method = method
	c.params = params.(rpc.FinancingFeesParams)
	*out.(*rpc.FinancingFeesResult) = c.result
	return nil
}
func (*lendingCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestLendingCLIReadOnlyTypedParityAndMissingStates(t *testing.T) {
	raw, err := os.ReadFile("../flexstmt/testdata/lending-synthetic.xml")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := flexstmt.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	core := financing.Calculate(statements, "synthetic-paper", from, from.AddDate(0, 0, 2), "EUR")
	want := rpc.FinancingFeesResult{Summary: core.Summary, Fees: core.Fees, FilteredCount: len(core.Fees)}
	conn := &lendingCLIConn{result: want}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "lending", []string{"fees", "--json", "--limit", "2"}); code != 0 {
		t.Fatal(stderr.String())
	}
	var got rpc.FinancingFeesResult
	if json.Unmarshal(stdout.Bytes(), &got) != nil || !reflect.DeepEqual(got, want) || conn.method != rpc.MethodFinancingFees || conn.params.Limit != 2 || conn.params.Window != "365d" {
		t.Fatal("CLI changed net-fee evidence")
	}
	for _, args := range [][]string{{"fees", "--limit", "101"}, {"fees", "--con-id", "-1"}, {"fees", "--from", "2026-09-29"}, {"enroll"}, {"fees", "--window", "all"}} {
		if code := Run(t.Context(), env, "lending", args); code == 0 || conn.calls != 1 {
			t.Fatal("invalid lending command reached daemon")
		}
	}
	stdout.Reset()
	if Run(t.Context(), env, "lending", []string{"fees"}) != 0 {
		t.Fatal(stderr.String())
	}
	if !strings.Contains(stdout.String(), "3.78") || !strings.Contains(stdout.String(), "do not add these fees to broker P/L") {
		t.Fatal("human output lost amount/evidence boundary")
	}
	missing := financing.Calculate(nil, "synthetic-paper", from, from.AddDate(0, 0, 2), "EUR")
	stdout.Reset()
	renderLendingSummary(env, missing.Summary)
	if !strings.Contains(stdout.String(), "Unavailable") || strings.Contains(stdout.String(), "0.00") {
		t.Fatal("missing fees rendered as zero")
	}
}
