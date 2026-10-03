package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/osauer/canary/v2/internal/rpc"
	"strings"
	"testing"
)

type setupCLIConn struct {
	method  string
	params  rpc.SetupEvaluateParams
	options rpc.SetupOptionsParams
}

func (c *setupCLIConn) Call(_ context.Context, method string, params, out any) error {
	c.method = method
	if method == rpc.MethodSetupsOptions {
		c.options = params.(rpc.SetupOptionsParams)
		*out.(*rpc.SetupOptionsResult) = rpc.SetupOptionsResult{Version: 1, Underlying: c.options.Underlying, Expiries: []rpc.SetupOptionExpiry{}, Calls: []rpc.SetupOptionCall{}}
		return nil
	}
	c.params = params.(rpc.SetupEvaluateParams)
	*out.(*rpc.SetupResult) = rpc.SetupResult{State: "unavailable", Spec: c.params.Spec, Contract: c.params.Contract}
	return nil
}

func TestSetupOptionsCLIUsesFixedReadOnlyMethodAndExactTuple(t *testing.T) {
	for _, args := range [][]string{
		{"options", "--symbol", "SYNTH", "--con-id", "17", "--json"},
		{"options", "--symbol", "SYNTH", "--con-id", "17", "--expiry", "20351120", "--json"},
		{"options", "--symbol", "SYNTH", "--con-id", "17", "--expiry", "20351120", "--strike", "100", "--json"},
	} {
		var stdout, stderr bytes.Buffer
		c := &setupCLIConn{}
		env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: c}
		if code := Run(t.Context(), env, "setups", args); code != 0 {
			t.Fatal(code, stderr.String())
		}
		if c.method != rpc.MethodSetupsOptions || c.options.Underlying.ConID != 17 || !strings.Contains(stdout.String(), `"expiries": []`) || !strings.Contains(stdout.String(), `"calls": []`) {
			t.Fatal(c, stdout.String())
		}
		if len(args) > 9 && (c.options.Strike == nil || *c.options.Strike != 100) {
			t.Fatal("selected strike lost", c.options)
		}
	}
	for _, args := range [][]string{
		{"options", "--symbol", "SYNTH"},
		{"options", "--symbol", "SYNTH", "--con-id", "17", "--strike", "100"},
		{"options", "--symbol", "SYNTH", "--con-id", "17", "--expiry", "20000101"},
		{"options", "--symbol", "SYNTH", "--con-id", "17", "--expiry", "20351120", "--strike", "NaN"},
	} {
		c := &setupCLIConn{}
		env := &Env{Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer), Conn: c}
		if Run(t.Context(), env, "setups", args) == 0 || c.method != "" {
			t.Fatal("invalid tuple reached daemon", args)
		}
	}
}
func (*setupCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestSetupsCLIForwardsExactContractAndStrictSpec(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &setupCLIConn{}
	env := &Env{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(`{"revision":"synthetic-v1"}`), Conn: c}
	if code := Run(t.Context(), env, "setups", []string{"evaluate", "--spec", "-", "--symbol", "SYNTH", "--con-id", "17", "--json"}); code != 0 {
		t.Fatal(code, stderr.String())
	}
	if c.method != rpc.MethodSetupsEvaluate || c.params.Contract.ConID != 17 || c.params.Spec.SpikeMultiple != 3 {
		t.Fatal(c)
	}
	for _, bad := range []string{`{"revision":"v1","expression":"true"}`, `{"revision":"v1"} {}`, strings.Repeat(" ", 16385)} {
		c.method = ""
		env.Stdin = strings.NewReader(bad)
		if Run(t.Context(), env, "setups", []string{"evaluate", "--spec", "-", "--symbol", "SYNTH"}) == 0 || c.method != "" {
			t.Fatal("invalid spec reached daemon")
		}
	}
}
