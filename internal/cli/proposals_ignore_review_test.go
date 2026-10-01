package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type refusedProposalIgnoreConn struct{}

func (refusedProposalIgnoreConn) Call(_ context.Context, method string, _, out any) error {
	if method != rpc.MethodTradeProposalsIgnore {
		return fmt.Errorf("unexpected method %s", method)
	}
	*out.(*rpc.TradeProposalIgnoreResult) = rpc.TradeProposalIgnoreResult{Accepted: false, Key: "proposal", Message: "proposal revision is stale"}
	return nil
}

func (refusedProposalIgnoreConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return fmt.Errorf("unexpected stream")
}

func TestProposalsIgnoreRefusalIsNotRenderedAsSuccess(t *testing.T) {
	var out, errs bytes.Buffer
	env := &Env{Conn: refusedProposalIgnoreConn{}, Stdout: &out, Stderr: &errs}
	if code := runProposalsIgnore(t.Context(), env, []string{"proposal", "stale"}); code == 0 || strings.Contains(out.String(), "Ignored") || !strings.Contains(errs.String(), "stale") {
		t.Fatalf("refusal reported success: code=%d stdout=%q stderr=%q", code, out.String(), errs.String())
	}
	out.Reset()
	errs.Reset()
	if code := runProposalsIgnore(t.Context(), env, []string{"--json", "proposal", "stale"}); code != 0 || !strings.Contains(out.String(), `"accepted": false`) {
		t.Fatalf("JSON refusal contract lost: code=%d stdout=%q", code, out.String())
	}
}
