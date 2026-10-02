package cli

import (
	"bytes"
	"github.com/osauer/canary/v2/internal/rpc"
	"testing"
)

func TestCurrencySettlementProbeExplicitOneShotJSON(t *testing.T) {
	for _, args := range [][]string{{"--currency-settlement-probe"}, {"--currency-settlement-probe", "--json", "--watch"}, {"--currency-settlement-probe", "--json", "--settlement-probe"}, {"--currency-settlement-probe", "--json"}, {"--json"}} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			c := &settlementProbeClient{}
			var out, stderr bytes.Buffer
			code := runAccount(t.Context(), &Env{Conn: c, Stdout: &out, Stderr: &stderr}, args)
			valid := len(args) == 2 || len(args) == 1 && args[0] == "--json"
			if !valid {
				if code == 0 || c.calls != 0 {
					t.Fatal("invalid diagnostic reached broker", code, c.calls)
				}
				return
			}
			if code != 0 || c.calls != 1 {
				t.Fatal(code, c.calls, stderr.String())
			}
			if len(args) == 2 {
				p, ok := c.params.(rpc.AccountSummaryParams)
				if !ok || !p.CurrencySettlementProbe || p.SettlementProbe {
					t.Fatal(c.params)
				}
			} else if c.params != nil {
				t.Fatal("ordinary read enabled diagnostics")
			}
		})
	}
}
