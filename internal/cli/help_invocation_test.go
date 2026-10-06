package cli

import (
	"bytes"
	"context"
	"testing"
)

// Every help form cmd/canary routes before dialling must print its help from
// a handler with no daemon connection. On 2026-10-06 `canary policy help
// override` reached the dial, so asking for help started a daemon.
func TestHelpInvocationNeedsNoDaemon(t *testing.T) {
	forms := [][]string{{"help"}, {"--help"}}
	actions := map[string]string{"policy": "override", "recon": "show", "reporting": "fx"}
	for cmd, withAction := range positionalHelp {
		cases := forms
		if withAction {
			cases = append(cases, []string{"help", actions[cmd]})
		}
		for _, rest := range cases {
			if !HelpInvocation(cmd, rest) {
				t.Fatalf("HelpInvocation(%q, %q) = false, want true", cmd, rest)
			}
			var out, errOut bytes.Buffer
			env := &Env{Stdout: &out, Stderr: &errOut}
			if code := Run(context.Background(), env, cmd, rest); code != 0 || out.Len() == 0 {
				t.Fatalf("canary %s %q without a daemon: exit %d, stdout %q, stderr %q", cmd, rest, code, out.String(), errOut.String())
			}
		}
	}
}

func TestHelpInvocationLeavesOtherArgumentsToTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		rest []string
	}{
		{"policy", []string{"show"}},
		{"policy", []string{"help", "override", "extra"}},
		{"order", []string{"help", "preview"}},
		{"quote", []string{"help"}},
		{"status", nil},
	} {
		if HelpInvocation(tc.cmd, tc.rest) {
			t.Errorf("HelpInvocation(%q, %q) = true, want false", tc.cmd, tc.rest)
		}
	}
}
