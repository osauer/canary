package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

type sessionReadConn struct {
	marketReadConn
	method string
}

func (c *sessionReadConn) Call(_ context.Context, method string, p, out any) error {
	c.method = method
	if p != nil {
		panic("market read sends no params")
	}
	*out.(*rpc.MarketReadResult) = rpc.MarketReadResult{Session: rpc.MarketReadSession{Date: "2026-09-23", State: rpc.MarketReadPre}, Tags: []rpc.MarketReadTag{}}
	return nil
}

func TestMarketReadCLIPrintsTheReadAsJSON(t *testing.T) {
	for _, args := range [][]string{{"read", "--json"}, {"--json", "read"}, {"read"}} {
		conn := &sessionReadConn{}
		var out bytes.Buffer
		if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", args); code != 0 || conn.method != rpc.MethodMarketRead {
			t.Fatalf("%v: exit %d method %q: %s", args, code, conn.method, out.String())
		}
		var got rpc.MarketReadResult
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Session.State != rpc.MarketReadPre {
			t.Fatalf("%v: %v %s", args, err, out.String())
		}
	}
}
