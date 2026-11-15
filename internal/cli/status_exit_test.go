package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/osauer/canary/v3/internal/rpc"
	"strings"
	"testing"
)

type statusExitConn struct{ connected bool }

func (c statusExitConn) Call(_ context.Context, method string, _ any, out any) error {
	if method == rpc.MethodStatusHealth {
		*out.(*rpc.HealthResult) = rpc.HealthResult{Connected: c.connected, GatewayPhase: rpc.GatewayPhasePortDown, LastError: "synthetic unreachable", Verdict: rpc.HealthVerdict{State: "OFFLINE"}}
	}
	return nil
}
func (statusExitConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}
func TestStatusExitCodeDoesNotDependOnFormat(t *testing.T) {
	for _, connected := range []bool{false, true} {
		for _, args := range [][]string{nil, {"--json"}} {
			var out, err bytes.Buffer
			env := &Env{Conn: statusExitConn{connected}, Stdout: &out, Stderr: &err}
			got := runStatus(t.Context(), env, args)
			want := 1
			if connected {
				want = 0
			}
			if got != want {
				t.Fatalf("connected=%v args=%v exit%d want%d", connected, args, got, want)
			}
			if len(args) > 0 && !json.Valid(out.Bytes()) {
				t.Fatal("invalid JSON")
			}
		}
	}
}
func TestCoreMandateUsesAccountCurrency(t *testing.T) {
	for _, ccy := range []string{"EUR", "USD", ""} {
		res := goldenCore()
		res.Mandate.BaseCurrency = ccy
		var out bytes.Buffer
		env := &Env{Stdout: &out}
		renderCore(env, res)
		want := ccy
		if want == "" {
			want = "currency unavailable"
		}
		if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), "34,500 "+want) {
			t.Fatalf("mandate unit %q: %s", ccy, out.String())
		}
	}
}
