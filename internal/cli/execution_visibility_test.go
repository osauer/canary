package cli

import (
	"github.com/osauer/canary/v3/internal/rpc"
	"strings"
	"testing"
)

func TestUnprovenExecutionVisibilityDoesNotClaimOwnOnly(t *testing.T) {
	// This is the actual state returned before any foreign-client fill has
	// been observed, including a correctly configured master with no such fill.
	v := rpc.ExecutionVisibilityHealth{Visibility: rpc.ExecutionVisibilityUnproven, ClientID: 15, Setting: rpc.ExecutionVisibilitySetting}
	got := formatExecutionVisibilityValue(&Env{}, v)
	t.Logf("visibility=%s rendered=%q", v.Visibility, got)
	if strings.Contains(got, "own orders only") || strings.Contains(got, "· set TWS") {
		t.Fatal("unproven observation was rendered as a proven restriction and unconditional setting-change instruction")
	}
}
