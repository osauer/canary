package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestSlowVersionProbeDoesNotConsumeCommandResponse(t *testing.T) {
	dir, err := os.MkdirTemp("", "canary-version-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(5 * time.Second))
	command, err := dial.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	served := make(chan error, 1)
	go func() {
		served <- func() error {
			actual, err := listener.Accept()
			if err != nil {
				return err
			}
			defer actual.Close()
			_ = actual.SetDeadline(time.Now().Add(5 * time.Second))
			probe, err := listener.Accept()
			if err != nil {
				return err
			}
			defer probe.Close()
			_ = probe.SetDeadline(time.Now().Add(5 * time.Second))
			var req rpc.Request
			if err := json.NewDecoder(probe).Decode(&req); err != nil {
				return err
			}
			if req.Method != rpc.MethodStatusHealth {
				return fmt.Errorf("probe method: %s", req.Method)
			}
			// Withhold the response until the version timeout closes its socket.
			var b [1]byte
			if _, err := probe.Read(b[:]); err == nil {
				return fmt.Errorf("version connection reused after timeout")
			} else if e, ok := err.(net.Error); ok && e.Timeout() {
				return fmt.Errorf("version connection not closed after timeout")
			}
			if err := json.NewDecoder(actual).Decode(&req); err != nil {
				return err
			}
			if req.Method != rpc.MethodMarketTape {
				return fmt.Errorf("command method: %s", req.Method)
			}
			return json.NewEncoder(actual).Encode(rpc.Response{ID: req.ID, Ok: true, Result: json.RawMessage(`{"history":{"rows":[]}}`)})
		}()
	}()
	warnIfDaemonVersionMismatch(path, "test-cli")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result rpc.MarketTapeResult
	if err := command.Call(ctx, rpc.MethodMarketTape, rpc.MarketTapeParams{History: true}, &result); err != nil {
		t.Fatal(err)
	}
	if result.History == nil {
		t.Fatal("command did not receive its history response")
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

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
