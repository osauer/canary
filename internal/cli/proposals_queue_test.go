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

// An exit plan arms all its records from one object; mixing the two forms,
// or half of the plan form, never reaches the daemon.
func TestQueueArmExitPlanInput(t *testing.T) {
	var out bytes.Buffer
	c := &preparedProposalCLIConn{}
	stdin := `{"queued_refs":["canaryqa1.a.x","canaryqa1.b.y"],"plan_digest":"abc","desk_action_id":"act-1","credential":"passkey","envelope":"sig"}`
	env := &Env{Conn: c, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &out, Origin: rpc.OrderOriginAgent}
	if exit := Run(t.Context(), env, "proposals", []string{"queue", "arm", "--stdin", "--json"}); exit != 0 || c.calls != 1 {
		t.Fatalf("plan arm exit=%d calls=%d output=%s", exit, c.calls, &out)
	}
	p, ok := c.params.(rpc.TradeProposalQueueArmParams)
	if !ok || len(p.QueuedRefs) != 2 || p.PlanDigest != "abc" || p.QueuedRef != "" || p.Origin != rpc.OrderOriginAgent {
		t.Fatalf("plan arm params = %+v", c.params)
	}
	for _, stdin := range []string{
		`{"queued_refs":["r"]}`,
		`{"plan_digest":"d"}`,
		`{"queued_refs":["r"],"plan_digest":"d","queued_ref":"r"}`,
		`{"queued_refs":["r"],"plan_digest":"d","terms_digest":"d"}`,
	} {
		c := &preparedProposalCLIConn{}
		env := &Env{Conn: c, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &out}
		if Run(t.Context(), env, "proposals", []string{"queue", "arm", "--stdin"}) == 0 || c.calls != 0 {
			t.Fatalf("%s reached RPC", stdin)
		}
	}
}

// A trail record says it has no minimum and where its line stands, never a
// floor of zero.
func TestRenderQueuedAuthTrail(t *testing.T) {
	var out bytes.Buffer
	q := rpc.QueuedAuth{State: rpc.QueuedAuthArmed,
		Terms: rpc.QueuedAuthTerms{QueueID: "q2", Version: 3, Action: rpc.OrderActionSell, MaxQuantity: 20, Currency: "USD",
			Contract:  rpc.ContractParams{Symbol: "SYNA", SecType: "OPT", Expiry: "20261218", Strike: 55, Right: "C"},
			NotBefore: time.Date(2026, 11, 24, 14, 50, 0, 0, time.UTC), NotAfter: time.Date(2026, 11, 24, 21, 0, 0, 0, time.UTC),
			OwnerOrder: &rpc.QueuedOwnerOrder{StockTrail: &rpc.QueuedStockTrail{Underlying: rpc.ContractParams{Symbol: "SYNA", Currency: "USD"}, TrailPercent: 3, InitialStopPrice: 49.5}}},
		Watch: &rpc.QueuedTrailWatch{State: rpc.QueuedWatchWatching, StopPrice: 50.83, HighWater: 52.4, StockPrice: 52.1}}
	renderQueuedAuth(&out, q)
	got := out.String()
	if strings.Contains(got, "not below") {
		t.Fatalf("a trail rendered a floor: %q", got)
	}
	for _, want := range []string{"sell at most 20 SYNA 20261218 55 C, no minimum, once SYNA is at or below its line (from 49.5 USD, trailing 3%)", "Exit line: watching 50.83 USD, high 52.4 USD, stock 52.1 USD"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered %q, want %q", got, want)
		}
	}
}
