package main

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestIsWatchDaemonInvocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"list stays local", []string{"--list"}, false},
		{"list json stays local", []string{"--list", "--json"}, false},
		{"add stays local", []string{"AAPL", "--add"}, false},
		{"default watch needs daemon", nil, true},
		{"json default needs daemon", []string{"--json"}, true},
		{"timeout default needs daemon", []string{"--timeout", "2s"}, true},
		{"quotes needs daemon", []string{"--quotes"}, true},
		{"quotes true needs daemon", []string{"--quotes=true"}, true},
		{"watch needs daemon", []string{"--watch"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isWatchDaemonInvocation(tc.args); got != tc.want {
				t.Fatalf("isWatchDaemonInvocation(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestRetiredCanaryDoesNotUseStressUnaryBudget(t *testing.T) {
	t.Parallel()
	current := unaryInvocationBudget("stress", nil)
	retired := unaryInvocationBudget("canary", nil)
	ordinary := unaryInvocationBudget("unknown", nil)
	if retired != ordinary || retired == current {
		t.Fatalf("retired canary budget=%s ordinary=%s stress=%s", retired, ordinary, current)
	}
}

func TestUnaryInvocationBudgetsOutliveDaemonMethods(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		cmd    string
		args   []string
		method string
		want   time.Duration
	}{
		{name: "history cold HMDS", cmd: "history", method: rpc.MethodHistoryDaily, want: 60 * time.Second},
		{name: "order WhatIf preview", cmd: "order", args: []string{"preview"}, method: rpc.MethodOrderPreview, want: 60 * time.Second},
		{name: "proposal refresh", cmd: "proposals", args: []string{"refresh"}, method: rpc.MethodTradeProposalsRefresh, want: 60 * time.Second},
		{name: "opportunity refresh", cmd: "opportunities", args: []string{"refresh"}, method: rpc.MethodOpportunitiesRefresh, want: 60 * time.Second},
		{name: "brief composition", cmd: "brief", method: rpc.MethodBriefSnapshot, want: 90 * time.Second},
		{name: "paper smoke", cmd: "trading", args: []string{"paper-smoke"}, method: rpc.MethodTradingPaperSmoke, want: 120 * time.Second},
		{name: "portfolio reduce", cmd: "proposals", args: []string{"reduce", "--portfolio"}, method: rpc.MethodTradeProposalsReducePortfolioPreview, want: 150 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := unaryInvocationBudget(tc.cmd, tc.args)
			if got != tc.want {
				t.Fatalf("unaryInvocationBudget(%q, %v) = %s, want %s", tc.cmd, tc.args, got, tc.want)
			}
			timing, ok := rpc.LookupMethodTiming(tc.method)
			if !ok {
				t.Fatalf("missing timing for %s", tc.method)
			}
			if got <= timing.DaemonTimeout {
				t.Fatalf("CLI budget %s must outlive %s daemon timeout %s", got, tc.method, timing.DaemonTimeout)
			}
		})
	}
}

func TestCLIInvocationTimingDeclaresCataloguedMethods(t *testing.T) {
	t.Parallel()
	for _, cmd := range []struct {
		name string
		args []string
	}{
		{name: "status"}, {name: "account"}, {name: "positions"}, {name: "quote"},
		{name: "watch"}, {name: "calendar"}, {name: "chain"}, {name: "history"},
		{name: "technical"}, {name: "market-events"}, {name: "breadth"}, {name: "gamma"},
		{name: "regime"}, {name: "stress"}, {name: "brief"}, {name: "rules"},
		{name: "alerts"}, {name: "policy"}, {name: "recon"}, {name: "proposals"},
		{name: "proposals", args: []string{"reduce", "--portfolio"}},
		{name: "opportunities"}, {name: "purge"},
		{name: "size"}, {name: "trading"}, {name: "trading", args: []string{"paper-smoke"}}, {name: "settings"},
		{name: "orders"}, {name: "order"}, {name: "order", args: []string{"preview"}},
		{name: "order", args: []string{"place"}}, {name: "order", args: []string{"modify"}}, {name: "order", args: []string{"cancel"}},
	} {
		methods, headroom, floor := cliInvocationTiming(cmd.name, cmd.args)
		if len(methods) == 0 {
			t.Errorf("daemon-backed command %q declares no RPC methods", cmd.name)
			continue
		}
		if headroom <= 0 {
			t.Errorf("daemon-backed command %q has non-positive headroom %s", cmd.name, headroom)
		}
		budget := cliMethodBudget(methods, headroom, floor)
		for _, method := range methods {
			timing, ok := rpc.LookupMethodTiming(method)
			if !ok {
				t.Errorf("command %q declares uncatalogued method %q", cmd.name, method)
				continue
			}
			if timing.Lifetime == rpc.MethodLifetimeUnary && budget <= timing.DaemonTimeout {
				t.Errorf("command %q budget %s does not outlive %q daemon timeout %s", cmd.name, budget, method, timing.DaemonTimeout)
			}
		}
	}
}

func TestRetiredProductEnvironmentIsRejectedBeforeUse(t *testing.T) {
	t.Parallel()
	for _, env := range retiredProductEnv {
		t.Run(env.retired, func(t *testing.T) {
			t.Parallel()
			err := retiredProductEnvError(func(name string) (string, bool) {
				return "redacted", name == env.retired
			})
			if err == nil || !strings.Contains(err.Error(), env.retired) || !strings.Contains(err.Error(), env.canonical) {
				t.Fatalf("retired env error=%v, want %s -> %s", err, env.retired, env.canonical)
			}
			if strings.Contains(err.Error(), "redacted") {
				t.Fatalf("retired env error exposed the value: %v", err)
			}
		})
	}
	if err := retiredProductEnvError(func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("clean environment rejected: %v", err)
	}
}
