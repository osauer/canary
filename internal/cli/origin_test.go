package cli

import (
	"bytes"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestDetectWriteOriginClassifiesAgents(t *testing.T) {
	clearAgentEnv := func(t *testing.T) {
		for _, name := range []string{"CANARY_AGENT_CONTEXT", "IBKR_AGENT_CONTEXT", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "OPENAI_CODEX"} {
			t.Setenv(name, "")
		}
	}

	t.Run("non-tty stdin is agent even without markers", func(t *testing.T) {
		clearAgentEnv(t)
		if got := DetectWriteOrigin(&bytes.Buffer{}); got != rpc.OrderOriginAgent {
			t.Fatalf("DetectWriteOrigin = %q, want agent", got)
		}
	})

	for _, marker := range []string{"CANARY_AGENT_CONTEXT", "IBKR_AGENT_CONTEXT", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "OPENAI_CODEX"} {
		t.Run("marker "+marker, func(t *testing.T) {
			clearAgentEnv(t)
			t.Setenv(marker, "1")
			if got := DetectWriteOrigin(&bytes.Buffer{}); got != rpc.OrderOriginAgent {
				t.Fatalf("DetectWriteOrigin with %s = %q, want agent", marker, got)
			}
		})
	}
}
