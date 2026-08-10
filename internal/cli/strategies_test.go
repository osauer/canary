package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type strategyCLIConn struct {
	method string
}

func (c *strategyCLIConn) Call(_ context.Context, method string, _ any, out any) error {
	c.method = method
	result := out.(*rpc.PositionsResult)
	result.Strategies = []rpc.PositionStrategy{{ID: "strategy-test", Revision: 1, Actionable: true}}
	return nil
}

func (*strategyCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestStrategiesFindsSubcommandAfterHoistedFlag(t *testing.T) {
	conn := &strategyCLIConn{}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "strategies", []string{"list", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if conn.method != rpc.MethodPositionsList {
		t.Fatalf("method = %q", conn.method)
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"strategy-test"`)) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}
