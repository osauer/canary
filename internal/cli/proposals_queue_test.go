package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// The queued reference and the signature envelope travel on standard input
// only; the request keeps the caller's origin.
func TestQueueArmReadsItsObjectFromStdinAndKeepsTheOrigin(t *testing.T) {
	const ref = "canaryqa1.synthetic.private"
	var out bytes.Buffer
	c := &preparedProposalCLIConn{}
	stdin := `{"queued_ref":"` + ref + `","terms_digest":"abc","desk_action_id":"act-1","credential":"companion","envelope":"sig"}`
	args := []string{"queue", "arm", "--stdin", "--json"}
	env := &Env{Conn: c, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &out, Origin: rpc.OrderOriginAgent}
	if exit := Run(t.Context(), env, "proposals", args); exit != 0 || c.calls != 1 || c.method != rpc.MethodTradeProposalsQueueArm {
		t.Fatalf("arm exit=%d calls=%d method=%s output=%s", exit, c.calls, c.method, &out)
	}
	p, ok := c.params.(rpc.TradeProposalQueueArmParams)
	if !ok || p.QueuedRef != ref || p.TermsDigest != "abc" || p.DeskActionID != "act-1" || p.Credential != "companion" || p.Envelope != "sig" || p.Origin != rpc.OrderOriginAgent {
		t.Fatalf("arm params = %+v", c.params)
	}
	if strings.Contains(out.String(), ref) {
		t.Fatal("the private reference escaped into the output")
	}
}

func TestQueueCLIRefusesAmbiguousInputsBeforeRPC(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		stdin string
	}{
		{[]string{"queue", "prepare", "key", "revision"}, ""},
		{[]string{"queue", "prepare", "--stdin", "--json"}, `{"owner_order":false}`},
		{[]string{"queue", "prepare", "--stdin", "--json"}, `{"owner_order":true,"unknown":1}`},
		{[]string{"queue", "prepare", "--stdin", "--json"}, `{"owner_order":true} {}`},
		{[]string{"queue", "prepare", "--stdin", "--json", "--quantity", "3"}, `{"owner_order":true}`},
		{[]string{"queue", "prepare", "--stdin", "--json"}, strings.Repeat(" ", queueArmInputMaxBytes+1)},
		{[]string{"queue", "arm", "--json"}, `{"queued_ref":"r","terms_digest":"d"}`},
		{[]string{"queue", "arm", "--stdin", "canaryqa1.in.argv"}, `{"queued_ref":"r","terms_digest":"d"}`},
		{[]string{"queue", "arm", "--stdin"}, `{"queued_ref":"r"}`},
		{[]string{"queue", "arm", "--stdin"}, `{"queued_ref":"r","terms_digest":"d","extra":1}`},
		{[]string{"queue", "arm", "--stdin"}, strings.Repeat(" ", queueArmInputMaxBytes+1)},
		{[]string{"queue", "cancel"}, ""},
		{[]string{"queue", "cancel", "id", "--all"}, ""},
		{[]string{"queue", "status"}, ""},
		{[]string{"queue"}, ""},
	} {
		var out bytes.Buffer
		c := &preparedProposalCLIConn{}
		env := &Env{Conn: c, Stdin: strings.NewReader(tc.stdin), Stdout: &out, Stderr: &out}
		if Run(t.Context(), env, "proposals", tc.args) == 0 || c.calls != 0 {
			t.Fatalf("%v reached RPC", tc.args)
		}
	}
}

func TestOwnerQueueCLIExactPrepareFromStdin(t *testing.T) {
	var out bytes.Buffer
	c := &preparedProposalCLIConn{}
	input := `{"owner_order":true,"account_id":"DU1234567","account_mode":"paper","worst_price":25,"order":{"contract":{"con_id":101,"symbol":"SYN","sec_type":"STK","currency":"USD"},"action":"BUY","quantity":2,"order_type":"LMT","tif":"DAY"}}`
	env := &Env{Conn: c, Stdin: strings.NewReader(input), Stdout: &out, Stderr: &out}
	if exit := Run(t.Context(), env, "proposals", []string{"queue", "prepare", "--stdin", "--json"}); exit != 0 || c.calls != 1 || c.method != rpc.MethodTradeProposalsQueuePrepare {
		t.Fatalf("prepare exit=%d calls=%d: %s", exit, c.calls, &out)
	}
	p, ok := c.params.(rpc.TradeProposalQueuePrepareParams)
	if !ok || !p.OwnerOrder || p.Order == nil || p.Order.Contract.ConID != 101 || p.Order.Quantity != 2 || p.WorstPrice != 25 || p.AccountMode != "paper" {
		t.Fatalf("lost exact owner terms: %+v", c.params)
	}
}

func TestQueueCLIRoutesPrepareCancelListAndStatus(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		method string
		check  func(any) bool
	}{
		{[]string{"queue", "prepare", "key", "rev", "--json", "--quantity", "3"}, rpc.MethodTradeProposalsQueuePrepare, func(p any) bool {
			q, ok := p.(rpc.TradeProposalQueuePrepareParams)
			return ok && q.Key == "key" && q.Revision == "rev" && q.Quantity == 3
		}},
		{[]string{"queue", "cancel", "q1", "--reason", "not today", "--json"}, rpc.MethodTradeProposalsQueueCancel, func(p any) bool {
			q, ok := p.(rpc.TradeProposalQueueCancelParams)
			return ok && q.QueueID == "q1" && !q.All && q.Reason == "not today" && q.Origin == rpc.OrderOriginHumanTTY
		}},
		{[]string{"queue", "cancel", "--all", "--json"}, rpc.MethodTradeProposalsQueueCancel, func(p any) bool {
			q, ok := p.(rpc.TradeProposalQueueCancelParams)
			return ok && q.All && q.QueueID == ""
		}},
		{[]string{"queue", "list", "--live", "--json"}, rpc.MethodTradeProposalsQueueList, func(p any) bool {
			q, ok := p.(rpc.TradeProposalQueueListParams)
			return ok && q.LiveOnly
		}},
		{[]string{"queue", "status", "q1", "--json"}, rpc.MethodTradeProposalsQueueStatus, func(p any) bool {
			q, ok := p.(rpc.TradeProposalQueueStatusParams)
			return ok && q.QueueID == "q1"
		}},
	} {
		var out bytes.Buffer
		c := &preparedProposalCLIConn{}
		env := &Env{Conn: c, Stdout: &out, Stderr: &out, Origin: rpc.OrderOriginHumanTTY}
		if exit := Run(t.Context(), env, "proposals", tc.args); exit != 0 || c.method != tc.method || !tc.check(c.params) {
			t.Fatalf("%v: exit=%d method=%s params=%+v output=%s", tc.args, exit, c.method, c.params, &out)
		}
	}
}

func TestRenderQueuedAuthSaysWhatMaySendAndWhen(t *testing.T) {
	var out bytes.Buffer
	q := rpc.QueuedAuth{State: rpc.QueuedAuthHeld, HoldCode: "spread_too_wide", HoldReason: "the spread is 3.1% of mid",
		Terms: rpc.QueuedAuthTerms{QueueID: "q1", Action: rpc.OrderActionSell, MaxQuantity: 10, WorstPrice: 18.75, Currency: "USD",
			Contract:  rpc.ContractParams{Symbol: "SYN", SecType: "STK"},
			NotBefore: time.Date(2026, 9, 28, 13, 35, 0, 0, time.UTC), NotAfter: time.Date(2026, 9, 28, 14, 35, 0, 0, time.UTC)}}
	renderQueuedAuth(&out, q)
	got := out.String()
	for _, want := range []string{"q1  held  sell at most 10 SYN, not below 18.75 USD", "Held: the spread is 3.1% of mid (spread_too_wide)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered %q, want %q", got, want)
		}
	}
}
