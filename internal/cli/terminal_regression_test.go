package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	selfupdate "github.com/osauer/canary/v3/internal/update"
)

func TestNonTerminalCannotClaimHumanOriginOrApproveUpdate(t *testing.T) {
	for _, key := range []string{"CANARY_AGENT_CONTEXT", "IBKR_AGENT_CONTEXT", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "OPENAI_CODEX"} {
		t.Setenv(key, "")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if got := DetectWriteOrigin(null); got != rpc.OrderOriginAgent {
		t.Errorf("/dev/null origin = %q", got)
	}
	var out bytes.Buffer
	fetched := false
	opts := updateOptions{in: null, out: &out, err: &out, isTTY: isStdinTTY(null), force: true}
	code := runUpdateCore(context.Background(), &opts, func(context.Context, string) (*selfupdate.Release, error) {
		fetched = true
		return &selfupdate.Release{TagName: "v3.22.2"}, nil
	}, nil, nil)
	if code != 2 || fetched {
		t.Errorf("nonterminal update exit=%d fetched=%v", code, fetched)
	}
	for _, input := range []string{"", " ", "\t", "\n", "yes\n", "n\n"} {
		if got, want := promptRestart(strings.NewReader(input), &out), input == "\n" || input == "yes\n"; got != want {
			t.Errorf("restart response %q=%v want %v", input, got, want)
		}
	}
}
