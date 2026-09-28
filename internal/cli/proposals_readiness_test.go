package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestFormatProposalReadiness(t *testing.T) {
	opens := time.Date(2026, 9, 28, 13, 30, 0, 0, time.UTC)
	send := opens.Add(15 * time.Minute)
	closed := &rpc.TradeProposalReadiness{Code: rpc.ReadinessMarketClosed, Market: "us_options", MarketLabel: "US listed options", SessionState: rpc.ReadinessSessionPreOpen, OpensAt: &opens, DefaultSendAt: &send}
	want := "market closed · US listed options pre open · opens " + opens.Local().Format("Mon 2 Jan 15:04 MST") + " · default send " + send.Local().Format("15:04 MST")
	if got := formatProposalReadiness(closed); got != want {
		t.Fatalf("closed market readiness = %q, want %q", got, want)
	}
	hard := &rpc.TradeProposalReadiness{Code: rpc.ReadinessNotExecutable, SessionState: rpc.ReadinessSessionUnknown, Message: "proposal revision is stale; refresh proposals before preview or submit"}
	if got := formatProposalReadiness(hard); !strings.HasPrefix(got, "not executable · proposal revision is stale") {
		t.Fatalf("hard refusal readiness = %q", got)
	}
	for _, r := range []*rpc.TradeProposalReadiness{nil, {Code: rpc.ReadinessReady, SessionState: rpc.ReadinessSessionOpen}} {
		if got := formatProposalReadiness(r); got != "" {
			t.Fatalf("ready rows print no readiness line, got %q", got)
		}
	}
}
