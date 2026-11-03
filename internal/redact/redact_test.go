package redact

import (
	"strings"
	"testing"
)

// The monitor's rule set is the one the scheduled log monitor pinned before
// it moved here (scripts/log-monitor, 2026-10-07): every row is byte for byte
// what the monitor still expects.
func TestMonitorRedactsOrderReferencesInFreeText(t *testing.T) {
	r := Monitor(Holdings{})
	for in, out := range map[string]string{
		"IBKR sent for order 48213: notice 2109: outside regular hours":                                            "IBKR sent for order [ref]: notice 2109: outside regular hours",
		"OrderId 48213 that needs to be cancelled cannot be cancelled":                                             "OrderId [ref] that needs to be cancelled cannot be cancelled",
		"open order orderId=48213 permId=91827364 status=Submitted":                                                "open order orderId=[ref] permId=[ref] status=Submitted",
		"Order permId =91827364 is not cancellable":                                                                "Order permId =[ref] is not cancellable",
		"dropping callback for broker order 48213 (perm 91827364)":                                                 "dropping callback for broker order [ref] (perm [ref])",
		"order #48213 rejected":                                                                                    "order #[ref] rejected",
		"order history unavailable; 3 orders cancelled":                                                            "order history unavailable; 3 orders cancelled",
		"order 2 of 3 in the plan follows after this fill":                                                         "order 2 of 3 in the plan follows after this fill",
		"order 48213 of 100 shares rejected":                                                                       "order [ref] of 100 shares rejected",
		"modify preview token targets broker order ID 48213, current order is 48214":                               "modify preview token targets broker order ID [ref], current order is [ref]",
		"modify preview token targets permanent ID 91827364, current order is 91827365":                            "modify preview token targets permanent ID [ref], current order is [ref]",
		"child order parentId 48213 not found":                                                                     "child order parentId [ref] not found",
		"openOrder 48213 arrived for an unknown contract":                                                          "openOrder [ref] arrived for an unknown contract",
		"reorder 5 legs by expiry":                                                                                 "reorder 5 legs by expiry",
		"unexpected feed state for DU1234567; protocol misalignment for IWM via reqMktData symbol=IWM reqid=12":    "unexpected feed state for [account]; protocol misalignment for [symbol] via reqMktData symbol=[redacted] reqid=[redacted]",
		"retry in 15m16s at 2026-08-08T06:00:00Z from 127.0.0.1:4002 pid 4711 (cached 3 so far) count=12 code=200": "retry in [duration] at [time] from [addr] pid N (cached N so far) count=N code=200",
	} {
		if got := r.Text(in); got != out {
			t.Errorf("Text(%q) = %q, want %q", in, got, out)
		}
	}
}

func TestHoldingsMaskKeepsVocabularyAndRoots(t *testing.T) {
	held := NewHoldings([]string{"SYNTHQ", "SYNTHA B", "SPY", "SYNTHQ 260918C00005000", "x"})
	if held.Count() != 4 {
		t.Fatalf("count = %d, want 4 (SYNTHQ, SYNTHA, SPY, X)", held.Count())
	}
	got := Monitor(held).Text("stale quote for SYNTHQ, SYNTHA and SPY; SYNTHQX is another name")
	want := "stale quote for [holding], [holding] and SPY; SYNTHQX is another name"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !held.Holds("SPY") || held.Holds("SYNTHQX") {
		t.Fatal("Holds should answer for the roots on the list only")
	}
	if (Holdings{}).Mask("SYNTHQ") != "SYNTHQ" {
		t.Fatal("the zero Holdings must mask nothing")
	}
}

// Outbound text leaves the machine: planted identity, money and path values
// must be gone and the surrounding words must stay.
func TestOutboundDropsIdentityMoneyHostsAndPaths(t *testing.T) {
	id := Identity{Home: "/Users/synthuser", User: "synthuser", Hostname: "synth-mbp.local", Hosts: []string{"gateway.synth.lan"}}
	r := Outbound(id, NewHoldings([]string{"SYNTHQ"}))
	in := strings.Join([]string{
		"config at /Users/synthuser/.config/ibkr/config.toml for Synthuser on synth-mbp",
		"dialled gateway.synth.lan:4001 and 192.168.1.20:4002 then api.example.com",
		"NLV 1,234,567.89 EUR cash USD 98,765 and 250000 CHF; code=10167 IBKR 354 kept",
		"account U1234567 order 48213 holding SYNTHQ at /home/other/data",
		"version v3.33.0 API server 203 stays",
	}, " ")
	got := r.Text(in)
	for _, leaked := range []string{"synthuser", "Synthuser", "synth-mbp", "gateway.synth.lan", "192.168.1.20", "api.example.com", ":4001", "1,234,567", "98,765", "250000", "U1234567", "48213", "SYNTHQ", "/home/other"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q leaked into %q", leaked, got)
		}
	}
	for _, kept := range []string{"~/.config/ibkr/config.toml", "[user]", "[host]", "[addr]", "[amount]", "code=10167", "IBKR 354", "[account]", "order [ref]", "[holding]", "v3.33.0 API server 203"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q missing from %q", kept, got)
		}
	}
}

func TestOutboundLeavesOrdinaryTextAlone(t *testing.T) {
	r := Outbound(Identity{User: "o"}, Holdings{})
	in := "option chain unavailable: gateway did not deliver security definitions in time (IBKR 162), 3 of 5 sources current"
	if got := r.Text(in); got != in {
		t.Fatalf("got %q, want unchanged", got)
	}
}

func TestLocalIdentityReadsTheProcess(t *testing.T) {
	t.Setenv("USER", "synthuser")
	id := LocalIdentity()
	if id.User != "synthuser" || id.Home == "" {
		t.Fatalf("identity = %+v", id)
	}
}
