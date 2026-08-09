package main

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestCLIInvocationTimingDeclaresCataloguedMethods(t *testing.T) {
	t.Parallel()
	for _, cmd := range []struct {
		name string
		args []string
	}{
		{name: "status"}, {name: "account"}, {name: "positions"}, {name: "technical"},
		{name: "brief"}, {name: "rules"}, {name: "policy"}, {name: "recon"}, {name: "proposals"},
		{name: "proposals", args: []string{"reduce", "--portfolio"}},
		{name: "opportunities"},
		{name: "trading"}, {name: "settings"},
		{name: "orders"}, {name: "order"}, {name: "order", args: []string{"cancel"}},
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
