package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type armedLocksConn struct {
	calls  int
	method string
	raw    json.RawMessage
	err    error
}

func (c *armedLocksConn) Call(_ context.Context, method string, params, out any) error {
	c.calls++
	c.method = method
	if method != rpc.MethodHoldingLocksReplace && method != rpc.MethodHoldingLocksList {
		return errors.New("unexpected method")
	}
	c.raw, _ = json.Marshal(params)
	if c.err != nil {
		return c.err
	}
	*out.(*rpc.HoldingLocks) = rpc.HoldingLocks{Version: 1, Revision: 8, AutomaticLocksInitialized: true,
		AutomaticLocks: []rpc.AutomaticLock{{Scope: "position", Symbol: "SYNA", ConID: 9001, AccountID: "synthetic", AccountMode: "paper"}}}
	return nil
}

func (*armedLocksConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestArmedAutomaticLockCLIReadsOnlyAutomaticAuthority(t *testing.T) {
	c := &armedLocksConn{}
	var stdout, stderr bytes.Buffer
	env := &Env{Conn: c, Stdout: &stdout, Stderr: &stderr}
	if Run(t.Context(), env, "locks", []string{"automatic-list", "--json"}) != 0 || c.calls != 1 || c.method != rpc.MethodHoldingLocksList {
		t.Fatalf("automatic read calls=%d method=%q: %s", c.calls, c.method, stderr.String())
	}
	var params map[string]bool
	if json.Unmarshal(c.raw, &params) != nil || len(params) != 1 || !params["automatic_only"] {
		t.Fatalf("automatic list fetched broker-decorated keep list: %s", c.raw)
	}
	var out rpc.HoldingLocks
	if json.Unmarshal(stdout.Bytes(), &out) != nil || out.Version != 1 || out.Revision != 8 || !out.AutomaticLocksInitialized || !out.AutomaticMatch("synthetic", "paper", "SYNA", 9001) {
		t.Fatalf("automatic authority lost in CLI response: %s", stdout.String())
	}
	for _, args := range [][]string{{"automatic-list"}, {"automatic-list", "extra", "--json"}, {"automatic-list", "--json", "--con-id", "9001"}, {"automatic-list", "--json", "--expected-revision", "8"}, {"automatic-list", "--json", "--request-id", "synthetic"}} {
		c.calls = 0
		if Run(t.Context(), env, "locks", args) == 0 || c.calls != 0 {
			t.Fatalf("invalid automatic read reached daemon: %v", args)
		}
	}
	c.calls, c.err = 0, errors.New("synthetic read unavailable")
	stderr.Reset()
	if Run(t.Context(), env, "locks", []string{"automatic-list", "--json"}) == 0 || c.calls != 1 || strings.Contains(stderr.String(), "retry unchanged terms") {
		t.Fatalf("read failure treated as an unacknowledged mutation: %s", stderr.String())
	}
}

func TestArmedAutomaticLockCLIUsesSingleFencedMutation(t *testing.T) {
	input := `{"automatic":{"action":"lock","lock":{"scope":"position","symbol":"SYNA","con_id":9001,"account_id":"synthetic","account_mode":"paper"}},"expected_revision":7,"request_id":"synthetic-lock"}`
	c := &armedLocksConn{}
	var stdout, stderr bytes.Buffer
	env := &Env{Conn: c, Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(input)}
	if Run(t.Context(), env, "locks", []string{"automatic-update", "--json"}) != 0 || c.calls != 1 || c.method != rpc.MethodHoldingLocksReplace {
		t.Fatalf("%d calls: %s", c.calls, stderr.String())
	}
	var sent rpc.HoldingLocksReplaceRequest
	if json.Unmarshal(c.raw, &sent) != nil || sent.ExpectedRevision != 7 || sent.RequestID != "synthetic-lock" || sent.Automatic == nil || sent.Automatic.Lock.ConID != 9001 {
		t.Fatal("mutation identity or exact contract changed")
	}
	for _, bad := range []string{`{}`, `{"automatic":{"action":"initialize"},"request_id":"missing-revision"}`, input + ` {}`, strings.Replace(input, `"automatic":`, `"authority":"human","automatic":`, 1), strings.Repeat(" ", 32769)} {
		c.calls = 0
		env.Stdin = strings.NewReader(bad)
		if Run(t.Context(), env, "locks", []string{"automatic-update", "--json"}) == 0 || c.calls != 0 {
			t.Fatal("invalid automatic mutation reached daemon")
		}
	}
	c.calls, c.err = 0, errors.New("synthetic lost acknowledgement")
	env.Stdin = strings.NewReader(input)
	if Run(t.Context(), env, "locks", []string{"automatic-update", "--json"}) == 0 || c.calls != 1 {
		t.Fatal("lost acknowledgement hidden or automatically retried")
	}
}
