package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type settlementProbeClient struct {
	calls  int
	params any
}

func (c *settlementProbeClient) Call(_ context.Context, method string, params any, out any) error {
	c.calls++
	c.params = params
	if method != rpc.MethodAccountSummary {
		return errors.New("unexpected method")
	}
	raw := []byte(`{"normal_status":"completed","probe":{"status":"completed_empty","cancel_status":"sent"}}`)
	return json.Unmarshal(raw, out)
}
func (*settlementProbeClient) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return errors.New("unexpected stream")
}
func TestSettlementProbeCLIRequiresExplicitOneShotJSON(t *testing.T) {
	for _, args := range [][]string{{"--settlement-probe"}, {"--settlement-probe", "--json", "--watch"}, {"--settlement-probe", "--json"}, {"--json"}} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			c := &settlementProbeClient{}
			var out, stderr bytes.Buffer
			code := runAccount(t.Context(), &Env{Conn: c, Stdout: &out, Stderr: &stderr}, args)
			if (args[0] == "--settlement-probe" && len(args) == 1) || len(args) == 3 {
				if code == 0 || c.calls != 0 {
					t.Fatal("unsupported diagnostic mode reached broker", code, c.calls)
				}
				return
			}
			if code != 0 || c.calls != 1 {
				t.Fatal(code, c.calls, stderr.String())
			}
			if args[0] == "--settlement-probe" {
				if p, ok := c.params.(rpc.AccountSummaryParams); !ok || !p.SettlementProbe {
					t.Fatal("diagnostic opt-in absent", c.params)
				}
				var got rpc.AccountSettlementComparison
				if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Probe.Status != "completed_empty" {
					t.Fatal(out.String(), err)
				}
			} else if c.params != nil {
				t.Fatal("ordinary request params changed")
			}
		})
	}
}
