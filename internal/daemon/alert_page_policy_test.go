package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// D2 page policy: only confirmed stress at act severity and panic page;
// early-warning and watch-grade states stay panel-and-brief. The 950
// governor-held confirmed_stress@watch lines in the retained corpus must
// map to inactive.
func TestAlertShadowRegimeStagePolicyPagesOnlyConfirmedActAndPanic(t *testing.T) {
	cases := []struct {
		stage    string
		severity string
		active   bool
		valid    bool
	}{
		{rpc.LifecycleEarlyWarning, "watch", false, true},
		{rpc.LifecycleConfirmedStress, "watch", false, true},
		{rpc.LifecycleConfirmedStress, "act", true, true},
		{rpc.LifecyclePanic, "watch", false, true},
		{rpc.LifecyclePanic, "act", true, true},
		{rpc.LifecyclePanic, "urgent", true, true},
		{rpc.LifecycleQuiet, "observe", false, true},
		{rpc.LifecycleDataQuality, "watch", false, true},
		{rpc.LifecycleConfirmedStress, "bogus", false, false},
	}
	for _, tc := range cases {
		_, active, valid := alertShadowRegimeStagePolicy(rpc.LifecycleState{Stage: tc.stage, Severity: tc.severity})
		if active != tc.active || valid != tc.valid {
			t.Errorf("%s@%s: active=%v valid=%v, want active=%v valid=%v", tc.stage, tc.severity, active, valid, tc.active, tc.valid)
		}
	}
}

// The two-snapshot hold: confirmed-act ranks 1 (pages only after a prior
// page-worthy observation), panic ranks 2 (pages immediately), everything
// else ranks 0.
func TestAlertShadowRegimePageRank(t *testing.T) {
	rank := func(stage, sev string) int {
		return alertShadowRegimePageRank(rpc.LifecycleState{Stage: stage, Severity: sev})
	}
	if got := rank(rpc.LifecycleConfirmedStress, "act"); got != 1 {
		t.Fatalf("confirmed act rank = %d, want 1", got)
	}
	if got := rank(rpc.LifecyclePanic, "urgent"); got != 2 {
		t.Fatalf("panic urgent rank = %d, want 2", got)
	}
	if got := rank(rpc.LifecycleConfirmedStress, "watch"); got != 0 {
		t.Fatalf("confirmed watch rank = %d, want 0", got)
	}
	if got := rank(rpc.LifecycleEarlyWarning, "watch"); got != 0 {
		t.Fatalf("early warning rank = %d, want 0", got)
	}
}
