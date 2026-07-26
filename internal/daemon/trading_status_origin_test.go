package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestLiveOriginBlockersMatrix(t *testing.T) {
	t.Parallel()
	live := rpc.TradingStatus{Mode: config.TradingModeLive, Account: "U7654321"}
	paper := rpc.TradingStatus{Mode: config.TradingModePaper, Account: "DU1234567"}

	cases := []struct {
		name   string
		status rpc.TradingStatus
		origin string
	}{
		{name: "paper agent unrestricted", status: paper, origin: rpc.OrderOriginAgent},
		{name: "paper empty origin unrestricted", status: paper, origin: ""},
		{name: "live agent inherits base gates", status: live, origin: rpc.OrderOriginAgent},
		{name: "live empty origin inherits base gates", status: live, origin: ""},
		{name: "live unknown origin inherits base gates", status: live, origin: "human-definitely"},
		{name: "live human inherits base gates", status: live, origin: rpc.OrderOriginHumanTTY},
		{name: "live paired device inherits base gates", status: live, origin: rpc.OrderOriginPairedDevice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blockers := liveOriginBlockers(tc.status, tc.origin)
			if len(blockers) != 0 {
				t.Fatalf("liveOriginBlockers = %+v, want none", blockers)
			}
		})
	}
}

func TestNormalizedWriteOrigin(t *testing.T) {
	t.Parallel()
	if got := normalizedWriteOrigin(""); got != rpc.OrderOriginAgent {
		t.Fatalf("empty origin = %q, want agent", got)
	}
	if got := normalizedWriteOrigin("script"); got != rpc.OrderOriginAgent {
		t.Fatalf("unknown origin = %q, want agent", got)
	}
	if got := normalizedWriteOrigin(rpc.OrderOriginHumanTTY); got != rpc.OrderOriginHumanTTY {
		t.Fatalf("human-tty origin = %q, want passthrough", got)
	}
	if got := normalizedWriteOrigin(rpc.OrderOriginPairedDevice); got != rpc.OrderOriginPairedDevice {
		t.Fatalf("paired origin = %q, want passthrough", got)
	}
}
