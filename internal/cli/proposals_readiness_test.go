package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestFormatProposalReadiness(t *testing.T) {
	opens := time.Date(2026, 9, 28, 13, 30, 0, 0, time.UTC)
	send := opens.Add(15 * time.Minute)
	closed := &rpc.TradeProposalReadiness{Code: rpc.ReadinessMarketClosed, Market: "us_options", MarketLabel: "US listed options", SessionState: rpc.ReadinessSessionPreOpen, OpensAt: &opens, DefaultSendAt: &send}
	want := "market closed · US listed options pre open · opens " + opens.Local().Format("Mon 2 Jan 15:04 MST") + " · default send " + send.Local().Format("15:04 MST")
	if got := formatProposalReadiness(closed, true); got != want {
		t.Fatalf("closed market readiness = %q, want %q", got, want)
	}
	// At a stress open the default send is 30 minutes after the open and
	// says why.
	stressSend := opens.Add(30 * time.Minute)
	stress := &rpc.TradeProposalReadiness{Code: rpc.ReadinessOpeningWindow, Market: "us_options", MarketLabel: "US listed options", SessionState: rpc.ReadinessSessionOpen, OpensAt: &opens, DefaultSendAt: &stressSend, StressOpen: true}
	if got, want := formatProposalReadiness(stress, true), "opening window · US listed options open · default send "+stressSend.Local().Format("15:04 MST")+" (stress open: options wait 30 minutes)"; got != want {
		t.Fatalf("stress open readiness = %q, want %q", got, want)
	}
	// A loss exit, expiry close or trailing stop keeps 15 minutes at a stress
	// open and says so.
	exitSend := opens.Add(15 * time.Minute)
	exempt := &rpc.TradeProposalReadiness{Code: rpc.ReadinessOpeningWindow, Market: "us_options", MarketLabel: "US listed options", SessionState: rpc.ReadinessSessionOpen, OpensAt: &opens, DefaultSendAt: &exitSend, StressOpenExempt: true}
	if got, want := formatProposalReadiness(exempt, true), "opening window · US listed options open · default send "+exitSend.Local().Format("15:04 MST")+" (stress open: exits keep 15 minutes)"; got != want {
		t.Fatalf("stress open exempt readiness = %q, want %q", got, want)
	}
	// A row nothing sends by itself names the earliest a hand may send it.
	if got, want := formatProposalReadiness(closed, false), "market closed · US listed options pre open · opens "+opens.Local().Format("Mon 2 Jan 15:04 MST")+" · earliest send "+send.Local().Format("15:04 MST"); got != want {
		t.Fatalf("hand-sent readiness = %q, want %q", got, want)
	}
	hard := &rpc.TradeProposalReadiness{Code: rpc.ReadinessNotExecutable, SessionState: rpc.ReadinessSessionUnknown, Message: "proposal revision is stale; refresh proposals before preview or submit"}
	if got := formatProposalReadiness(hard, false); !strings.HasPrefix(got, "not executable · proposal revision is stale") {
		t.Fatalf("hard refusal readiness = %q", got)
	}
	for _, r := range []*rpc.TradeProposalReadiness{nil, {Code: rpc.ReadinessReady, SessionState: rpc.ReadinessSessionOpen}} {
		if got := formatProposalReadiness(r, false); got != "" {
			t.Fatalf("ready rows print no readiness line, got %q", got)
		}
	}
}

// Something sends a queued row, a pre-authorised row and the core's row (the
// standing-mandate controller) at the default send time; every other row
// waits for a hand.
func TestProposalSendsByDefault(t *testing.T) {
	for name, tc := range map[string]struct {
		p    rpc.TradeProposal
		want bool
	}{
		"plain reduction":    {rpc.TradeProposal{Bucket: rpc.TradeProposalBucketMandateReduction}, false},
		"queued":             {rpc.TradeProposal{Bucket: rpc.TradeProposalBucketBudgetReduction, Queued: &rpc.TradeProposalQueued{}}, true},
		"pre-authorised":     {rpc.TradeProposal{Bucket: rpc.TradeProposalBucketTrailingStop, Automatic: &rpc.TradeProposalAutomatic{PreAuthorised: true}}, true},
		"not pre-authorised": {rpc.TradeProposal{Bucket: rpc.TradeProposalBucketTrailingStop, Automatic: &rpc.TradeProposalAutomatic{}}, false},
		"the core's row":     {rpc.TradeProposal{Bucket: rpc.TradeProposalBucketCoreRebalance}, true},
	} {
		if got := proposalSendsByDefault(tc.p); got != tc.want {
			t.Fatalf("%s: sends by default %v, want %v", name, got, tc.want)
		}
	}
}
