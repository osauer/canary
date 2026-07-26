package cli

import (
	"io"
	"os"

	"github.com/osauer/canary/v2/internal/rpc"
)

// DetectWriteOrigin classifies this process for broker-write authorization.
// Any agent marker or a non-TTY stdin classifies as agent; nothing can force
// a human classification. The daemon treats unknown origins as agent for
// audit and any origin-specific policy, while broker-write readiness still
// comes from trading mode, pins, preview tokens, freeze state, and broker
// checks.
func DetectWriteOrigin(stdin io.Reader) string {
	// docgen:env CANARY_AGENT_CONTEXT | When set (any value), broker writes from this process are classified as agent-origin for audit and origin-specific policy. The variable can only restrict: no environment variable can claim a human origin.
	if os.Getenv("CANARY_AGENT_CONTEXT") != "" ||
		os.Getenv("IBKR_AGENT_CONTEXT") != "" || // retired restrict-only marker remains an anti-bypass input
		os.Getenv("CLAUDECODE") != "" ||
		os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "" ||
		os.Getenv("CODEX_SANDBOX") != "" ||
		os.Getenv("OPENAI_CODEX") != "" {
		return rpc.OrderOriginAgent
	}
	if !isStdinTTY(stdin) {
		return rpc.OrderOriginAgent
	}
	return rpc.OrderOriginHumanTTY
}
