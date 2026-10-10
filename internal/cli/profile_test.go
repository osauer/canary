package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type profileTestConn struct {
	goldenConn
	calls  int
	params rpc.ProfileParams
}

func (c *profileTestConn) Call(ctx context.Context, method string, p any, out any) error {
	c.calls++
	c.params = p.(rpc.ProfileParams)
	return c.goldenConn.Call(ctx, method, p, out)
}

func TestProfileCommandValidatesBeforeCallingDaemon(t *testing.T) {
	c := &profileTestConn{goldenConn: goldenConn{rpc.MethodProfileCapture: rpc.ProfileResult{Kind: "allocs", PID: 123}}}
	var out, stderr bytes.Buffer
	env := &Env{Conn: c, Stdout: &out, Stderr: &stderr}
	for _, args := range [][]string{{"--duration", "0s"}, {"--duration", "121s"}, {"--duration", "1500ms"}, {"--kind", "heap"}, {"extra"}} {
		if Run(t.Context(), env, "profile", args) == 0 || c.calls != 0 {
			t.Fatal("invalid capture sent", args)
		}
	}
	if Run(t.Context(), env, "profile", []string{"--kind", "allocs", "--duration", "120s", "--json"}) != 0 || c.calls != 1 || c.params.Kind != "allocs" || c.params.Seconds != 120 {
		t.Fatalf("capture parameters: %+v", c)
	}
}
