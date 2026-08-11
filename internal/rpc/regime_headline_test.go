package rpc

import "testing"

// A provisional red is one the cluster logic itself declined to count; the
// headline must read as a watch, not as present stress. Eligible or
// still-red clusters keep the louder wording.
func TestRegimeHeadlineUnconfirmedRedReadsWatch(t *testing.T) {
	c := RegimeComposite{ClusterRankedCount: 6, ClusterProvisionalRedCount: 1}
	if got := RegimeHeadline(c, "early_warning"); got != "Watch: one unconfirmed stress signal" {
		t.Fatalf("provisional-only headline = %q", got)
	}
	c.ClusterProvisionalRedCount = 2
	if got := RegimeHeadline(c, "early_warning"); got != "Watch: unconfirmed stress signals" {
		t.Fatalf("plural provisional headline = %q", got)
	}
	c.ClusterProvisionalRedCount = 1
	c.ClusterEligibleRedCount = 1
	if got := RegimeHeadline(c, "early_warning"); got != "Stress signal present" {
		t.Fatalf("eligible-red headline = %q", got)
	}
	c.ClusterEligibleRedCount = 0
	c.ClusterRedCount = 1
	if got := RegimeHeadline(c, "early_warning"); got != "Stress signal present" {
		t.Fatalf("confirmed-band headline = %q", got)
	}
}
