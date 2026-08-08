package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunPolicyGroupHelpExplainsActionsWithoutRPC(t *testing.T) {
	t.Parallel()
	for _, help := range []string{"--help", "-h", "-help", "help"} {
		t.Run(help, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &stderr}
			if code := Run(context.Background(), env, "policy", []string{help}); code != 0 {
				t.Fatalf("Run(policy, %s)=%d, want 0", help, code)
			}
			got := stdout.String()
			for _, want := range []string{
				"Start here:",
				"Human-only policy actions",
				"reset-drawdown",
				"correct-peak",
				"retained broker statements",
				"canary policy help <action>",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("policy help missing %q:\n%s", want, got)
				}
			}
			if strings.Contains(got, "Usage of canary policy show") {
				t.Fatalf("group help fell through to show help:\n%s", got)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q, want empty", stderr.String())
			}
		})
	}
}

func TestRunPolicyActionHelpExplainsEffectsWithoutRPC(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "reset flag",
			args: []string{"reset-drawdown", "--help"},
			want: []string{"release the latched drawdown brake", "re-bases the", "does not change policy", "does not clear"},
		},
		{
			name: "reset help verb",
			args: []string{"help", "reset-drawdown"},
			want: []string{"--reason TEXT", "trading.freeze", "interactive terminal"},
		},
		{
			name: "capital event",
			args: []string{"capital-event", "--help"},
			want: []string{"provisional bridge", "do not", "clear a drawdown latch", "extends the clock automatically"},
		},
		{
			name: "correct peak",
			args: []string{"correct-peak", "--help"},
			want: []string{"Prefer --from-statements", "may only lower", "does not clear"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &stderr}
			if code := Run(context.Background(), env, "policy", tc.args); code != 0 {
				t.Fatalf("Run(policy, %v)=%d, want 0; stderr=%s", tc.args, code, stderr.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("policy action help missing %q:\n%s", want, stdout.String())
				}
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q, want empty", stderr.String())
			}
		})
	}
}

func TestRunReconHelpExplainsGroupAndDismissWithoutRPC(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "group", args: []string{"--help"}, want: []string{"compare retained broker statements", "extends the policy clock automatically", "Only dismiss is a human-only write"}},
		{name: "dismiss flag", args: []string{"dismiss", "--help"}, want: []string{"--line ID --reason TEXT", "does not delete or alter", "human-only action"}},
		{name: "dismiss help verb", args: []string{"help", "dismiss"}, want: []string{"canary recon show", "retained broker statement"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &stderr}
			if code := Run(context.Background(), env, "recon", tc.args); code != 0 {
				t.Fatalf("Run(recon, %v)=%d, want 0; stderr=%s", tc.args, code, stderr.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("recon help missing %q:\n%s", want, stdout.String())
				}
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q, want empty", stderr.String())
			}
		})
	}
}
