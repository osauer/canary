package main

import (
	"strings"
	"testing"
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

func TestIsBacktestDaemonInvocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"offline stress stays local", []string{"stress", "--input", "rows.jsonl"}, false},
		{"capture opportunity needs daemon", []string{"capture-opportunity", "--symbols", "SPY"}, true},
		{"export opportunity bars needs daemon", []string{"export-opportunity-bars", "--symbols", "SPY"}, true},
		{"subcommand help stays local", []string{"export-opportunity-bars", "--help"}, false},
		{"top level help stays local", []string{"--help"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isBacktestDaemonInvocation(tc.args); got != tc.want {
				t.Fatalf("isBacktestDaemonInvocation(%v) = %v, want %v", tc.args, got, tc.want)
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
