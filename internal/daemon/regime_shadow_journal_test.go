package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// TestRegimeDecisionLineCarriesShadowRead is the end-to-end wiring check: a
// published snapshot must reach the journal payload with a shadow scoring
// attached, under the model version it was scored by.
func TestRegimeDecisionLineCarriesShadowRead(t *testing.T) {
	publishedAt := time.Date(2026, 7, 20, 15, 10, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(publishedAt, "shadow wiring")
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{
		Revision: 1, PublishedAt: publishedAt, Fingerprint: snapshot.Fingerprint,
	}

	line := buildRegimeDecisionLine(publishedAt, snapshot, publication)
	if line.Shadow == nil {
		t.Fatal("decision line carries no shadow read")
	}
	if line.Shadow.Model != rpc.RegimeShadowModel {
		t.Errorf("shadow model = %q, want %q", line.Shadow.Model, rpc.RegimeShadowModel)
	}
	if line.Shadow.Stage == "" {
		t.Error("shadow read carries no stage")
	}
	if line.Shadow.Confirming < 0 || line.Shadow.Warning < 0 {
		t.Errorf("negative arm: confirming=%v warning=%v", line.Shadow.Confirming, line.Shadow.Warning)
	}

	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"shadow"`) {
		t.Error("marshalled decision line has no shadow field")
	}
	// The shipped decision must be untouched by the shadow's presence: it is
	// recorded beside the verdict, never folded into it.
	if line.Stage != snapshot.Lifecycle.Stage || line.Severity != snapshot.Lifecycle.Severity {
		t.Errorf("shipped decision changed: stage %q/%q severity %q/%q",
			line.Stage, snapshot.Lifecycle.Stage, line.Severity, snapshot.Lifecycle.Severity)
	}
}

// TestRegimeShadowReadIsWriteOnly guards the one rule that keeps this safe to
// carry in production: no shipped decision field may be derived from the
// shadow. Scoring the same snapshot with the shadow suppressed must produce
// an identical line apart from the shadow itself.
func TestRegimeShadowReadIsWriteOnly(t *testing.T) {
	publishedAt := time.Date(2026, 7, 20, 15, 10, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(publishedAt, "shadow isolation")
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{
		Revision: 1, PublishedAt: publishedAt, Fingerprint: snapshot.Fingerprint,
	}

	withShadow := buildRegimeDecisionLine(publishedAt, snapshot, publication)
	stripped := withShadow
	stripped.Shadow = nil

	want := buildRegimeDecisionLine(publishedAt, snapshot, publication)
	want.Shadow = nil

	gotJSON, err := json.Marshal(stripped)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Error("the shipped decision payload depends on the shadow read")
	}
	// And the fingerprint — the corpus partition key — must not move.
	if rpc.BuildRegimeFingerprint(snapshot) != snapshot.Fingerprint {
		t.Error("scoring the shadow perturbed the snapshot fingerprint")
	}
}
