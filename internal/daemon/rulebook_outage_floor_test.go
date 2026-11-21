package daemon

import (
	"testing"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// A stage blanked because an outage hid a confirmation holds the rulebook's
// latch, but never below the bucket of the stage it replaced. Before: the
// proxy's latched red with no witness standing reads quiet, so the latch is
// calm. Then the proxy goes dark and VVIX turns an eligible red: the blank
// replaces an early warning, and the latch moves to early warning rather than
// staying calm. A confirmed latch stays confirmed. (Fable review 2026-11-21.)
func TestOutageBlankOnlyTightensTheRulebookLatch(t *testing.T) {
	blank := rpc.LifecycleState{Stage: rpc.LifecycleDataQuality, Governors: []rpc.GovernorAction{{
		Action: rpc.RegimeGovernorStageBlanked, From: rpc.LifecycleEarlyWarning, To: rpc.LifecycleDataQuality,
		Reason: rpc.RegimeGovernorOutageHidesConfirmation, Clusters: []string{"credit"},
	}}}
	for _, tc := range []struct {
		held, bucket, stage string
	}{
		{risk.RegimeBucketCalm, risk.RegimeBucketEarlyWarning, rpc.LifecycleEarlyWarning},
		{"", risk.RegimeBucketEarlyWarning, rpc.LifecycleEarlyWarning},
		{risk.RegimeBucketEarlyWarning, "", ""},
		{risk.RegimeBucketConfirmed, "", ""},
	} {
		if bucket, stage := rulesRegimeProjectedBucket(blank, tc.held); bucket != tc.bucket || stage != tc.stage {
			t.Errorf("held %q: latched %q under %q, want %q under %q", tc.held, bucket, stage, tc.bucket, tc.stage)
		}
	}
	// Any other data_quality still holds whatever was latched.
	if bucket, _ := rulesRegimeProjectedBucket(rpc.LifecycleState{Stage: rpc.LifecycleDataQuality}, risk.RegimeBucketCalm); bucket != "" {
		t.Errorf("plain data_quality latched %q, want the latch held", bucket)
	}
}
