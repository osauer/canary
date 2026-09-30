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
	// At a stress open the default send is 30 minutes after the open and
	// says why.
	stressSend := opens.Add(30 * time.Minute)
	stress := &rpc.TradeProposalReadiness{Code: rpc.ReadinessOpeningWindow, Market: "us_options", MarketLabel: "US listed options", SessionState: rpc.ReadinessSessionOpen, OpensAt: &opens, DefaultSendAt: &stressSend, StressOpen: true}
	if got, want := formatProposalReadiness(stress), "opening window · US listed options open · default send "+stressSend.Local().Format("15:04 MST")+" (stress open: options wait 30 minutes)"; got != want {
		t.Fatalf("stress open readiness = %q, want %q", got, want)
	}
	// A loss exit, expiry close or trailing stop keeps 15 minutes at a stress
	// open and says so.
	exitSend := opens.Add(15 * time.Minute)
	exempt := &rpc.TradeProposalReadiness{Code: rpc.ReadinessOpeningWindow, Market: "us_options", MarketLabel: "US listed options", SessionState: rpc.ReadinessSessionOpen, OpensAt: &opens, DefaultSendAt: &exitSend, StressOpenExempt: true}
	if got, want := formatProposalReadiness(exempt), "opening window · US listed options open · default send "+exitSend.Local().Format("15:04 MST")+" (stress open: exits keep 15 minutes)"; got != want {
		t.Fatalf("stress open exempt readiness = %q, want %q", got, want)
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
