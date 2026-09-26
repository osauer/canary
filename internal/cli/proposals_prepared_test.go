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

type preparedProposalCLIConn struct {
	method string
	params any
	calls  int
}

func (c *preparedProposalCLIConn) Call(_ context.Context, method string, p, out any) error {
	c.method, c.params = method, p
	c.calls++
	return nil
}
func (c *preparedProposalCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return fmt.Errorf("unexpected stream")
}

func TestPreparedProposalCLIReferenceUsesStdinAndPreservesOrigin(t *testing.T) {
	const ref = "canarypp1.synthetic.private"
	for _, cmd := range []string{"submit", "prepared-status"} {
		var out bytes.Buffer
		c := &preparedProposalCLIConn{}
		args := []string{cmd, "--prepared-ref-stdin", "--json"}
		if cmd == "submit" {
			args = append(args, "key", "revision")
		}
		env := &Env{Conn: c, Stdin: strings.NewReader(ref + "\n"), Stdout: &out, Stderr: &out, Origin: rpc.OrderOriginAgent}
		if exit := Run(t.Context(), env, "proposals", args); exit != 0 || c.calls != 1 {
			t.Fatalf("routing exit=%d output=%s", exit, &out)
		}
		if strings.Contains(out.String(), ref) || strings.Contains(strings.Join(args, " "), ref) {
			t.Fatal("private reference escaped stdin")
		}
		switch p := c.params.(type) {
		case rpc.TradeProposalSubmitParams:
			if p.PreparedRef != ref || p.Origin != rpc.OrderOriginAgent || p.Quantity != 0 || !p.FastPath {
				t.Fatal("submit identity or origin changed")
			}
		case rpc.TradeProposalPreparedStatusParams:
			if p.PreparedRef != ref {
				t.Fatal("status reference changed")
			}
		default:
			t.Fatal("unexpected request type")
		}
	}
}

func TestPreparedProposalCLIRejectsAmbiguousInputsBeforeRPC(t *testing.T) {
	for _, args := range [][]string{{"prepare", "key", "revision"}, {"prepared-status", "private-in-argv", "--json"}, {"submit", "key", "revision", "--prepared-ref-stdin", "--quantity", "2"}, {"prepared-status", "--prepared-ref", "private-in-argv", "--json"}} {
		var out bytes.Buffer
		c := &preparedProposalCLIConn{}
		env := &Env{Conn: c, Stdin: strings.NewReader("synthetic"), Stdout: &out, Stderr: &out}
		if Run(t.Context(), env, "proposals", args) == 0 || c.calls != 0 {
			t.Fatal("ambiguous capability reached RPC")
		}
	}
	var out bytes.Buffer
	for _, input := range []string{"", strings.Repeat("x", 4097), "first\nsecond"} {
		if _, err := readPreparedProposalReference(&Env{Stdin: strings.NewReader(input), Stdout: &out, Stderr: &out}); err == nil {
			t.Fatal("unbounded/ambiguous stdin accepted")
		}
	}
}
