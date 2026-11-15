package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

type macroReadConn struct {
	riskReadConn
	params any
}

func (c *macroReadConn) Call(ctx context.Context, method string, in, out any) error {
	c.params = in
	return c.riskReadConn.Call(ctx, method, in, out)
}

func TestMacroCLIPreservesWindowAndFailedPrimaryEvidence(t *testing.T) {
	want := rpc.MacroSnapshotResult{CoverageStatus: "partial", Truncated: true, PublicationsTruncated: true, Sources: []rpc.MacroSource{{ID: "bls-calendar", Availability: "unavailable", ConsecutiveFailures: 3}, {ID: "nyfed-calendar", Availability: "available", WindowStart: "2026-09-01", WindowEnd: "2026-09-30"}}}
	conn := &macroReadConn{result: want}
	var out, stderr bytes.Buffer
	env := &Env{Conn: conn, Stdout: &out, Stderr: &stderr}
	if Run(t.Context(), env, "macro", []string{"--window-start", "2026-09-11", "--window-end", "2026-09-11", "--json"}) != 0 {
		t.Fatal(stderr.String())
	}
	var got rpc.MacroSnapshotResult
	if json.Unmarshal(out.Bytes(), &got) != nil || !reflect.DeepEqual(want, got) || !reflect.DeepEqual(conn.params, rpc.MacroSnapshotParams{WindowStart: "2026-09-11", WindowEnd: "2026-09-11"}) {
		t.Fatal("CLI lost window, source failure or independent coverage")
	}
	if !reflect.DeepEqual(conn.calls, []string{rpc.MethodMacroSnapshot}) {
		t.Fatal("macro read contacted another authority")
	}
}
