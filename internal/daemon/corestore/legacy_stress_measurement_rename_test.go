package corestore

import (
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// legacy label triple written by the pre-rename cutover importer.
const (
	preRenameMeasurementScope  = "market/legacy/canary-measurements"
	preRenameMeasurementSource = "legacy.canary_decision_journal"
	preRenameMeasurementKind   = "canary_market_measurement.v1"
)

// TestLegacyStressMeasurementRenamePreservesEvidence proves that the
// observations an operator already has under the pre-rename labels survive
// migration 3 intact: they are relabelled in place, keeping their payloads,
// digests, timestamps, metadata, and eligibility, and every observation from
// every other source is left alone.
func TestLegacyStressMeasurementRenamePreservesEvidence(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(privateTempDir(t), "daemon.db")
	plan := currentMigrationPlan()

	seedV1Authority(t, path, plan)
	seedLegacyMeasurementObservations(t, path, plan)

	db := rawDB(t, path)
	defer db.Close()
	before := snapshotTable(t, db, "observations")
	if len(before) != 4 {
		t.Fatalf("fixture observations=%d want 4", len(before))
	}

	if _, err := migrate(ctx, db, plan, time.Now().UTC()); err != nil {
		t.Fatalf("apply legacy stress measurement rename: %v", err)
	}

	// Exactly the three label columns move, and only on rows carrying the whole
	// pre-rename triple. Everything else — payload, digest, metadata,
	// observed_at, recorded_at, content_type, decision_eligible, and every
	// unrelated row — compares byte-equal.
	want := make([]map[string]any, 0, len(before))
	relabelled := 0
	for _, row := range before {
		copied := maps.Clone(row)
		if row["scope_key"] == preRenameMeasurementScope &&
			row["source"] == preRenameMeasurementSource &&
			row["kind"] == preRenameMeasurementKind {
			copied["scope_key"] = "market/legacy/stress-measurements"
			copied["source"] = "legacy.stress_decision_journal"
			copied["kind"] = "stress_market_measurement.v1"
			relabelled++
		}
		want = append(want, copied)
	}
	if relabelled != 2 {
		t.Fatalf("fixture relabelled %d rows, want 2", relabelled)
	}
	after := snapshotTable(t, db, "observations")
	if !reflect.DeepEqual(want, after) {
		t.Fatalf("observations changed beyond the three label columns:\nwant=%v\ngot =%v", want, after)
	}

	// Decoys that share two of the three label columns must not have moved: a
	// looser predicate would have swept them up.
	for _, decoy := range []struct{ column, value string }{
		{"source", "legacy.regime_decision_journal"},
		{"kind", "canary_market_measurement.v1"},
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM observations WHERE scope_key = ? AND `+decoy.column+` = ?`,
			preRenameMeasurementScope, decoy.value,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("decoy observation (%s=%s) count=%d want 1", decoy.column, decoy.value, n)
		}
	}

	// The append-only guard is armed again on the other side of the migration.
	for _, stmt := range []string{
		`UPDATE observations SET kind='tampered'`,
		`DELETE FROM observations`,
	} {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("observations append-only guard did not re-arm: %q succeeded", stmt)
		} else if !strings.Contains(err.Error(), "append-only") {
			t.Errorf("unexpected refusal for %q: %v", stmt, err)
		}
	}

	// Re-running the migration plan changes nothing further.
	if _, err := migrate(ctx, db, plan, time.Now().UTC()); err != nil {
		t.Fatalf("second migrate on a current database: %v", err)
	}
	if got := snapshotTable(t, db, "observations"); !reflect.DeepEqual(want, got) {
		t.Fatal("re-running migrate rewrote observations")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The relabelled rows are readable under the new identity, the old identity
	// is gone, and the payload digests still verify.
	store, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open migrated authority: %v", err)
	}
	defer store.Close()
	moved, err := store.ListObservations(ctx, ObservationQuery{
		ScopeKey: "market/legacy/stress-measurements",
		Source:   "legacy.stress_decision_journal",
		Kind:     "stress_market_measurement.v1",
	})
	if err != nil || len(moved) != 2 {
		t.Fatalf("relabelled observations are not readable: n=%d err=%v", len(moved), err)
	}
	for i, observation := range moved {
		if observation.DecisionEligible {
			t.Errorf("observation %d became decision-eligible across the rename", i)
		}
		if len(observation.Payload) == 0 || len(observation.MetadataJSON) == 0 {
			t.Errorf("observation %d lost payload or metadata across the rename", i)
		}
	}
	stale, err := store.ListObservations(ctx, ObservationQuery{ScopeKey: preRenameMeasurementScope})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 2 {
		t.Fatalf("pre-rename scope holds %d rows, want the 2 decoys only", len(stale))
	}
	report, err := store.CheckIntegrity(ctx)
	if err != nil || !report.OK() {
		t.Fatalf("integrity after migration: %+v err=%v", report, err)
	}
}

// seedLegacyMeasurementObservations writes two observations under the complete
// pre-rename label triple plus two decoys that match it only partially.
func seedLegacyMeasurementObservations(t *testing.T, path string, plan []migration) {
	t.Helper()
	ctx := t.Context()
	store, err := openWithPlan(ctx, Options{Path: path}, plan[:1])
	if err != nil {
		t.Fatalf("reopen v1 authority: %v", err)
	}
	at := time.Unix(1_700_000_000, 0).UTC()
	inputs := []ObservationInput{{
		ScopeKey: preRenameMeasurementScope, Source: preRenameMeasurementSource,
		Kind: preRenameMeasurementKind, ObservedAt: at, ContentType: "application/json",
		Payload:      []byte(`{"v":1,"session_key":"2026-06-30","market":{"red_clusters":2}}`),
		MetadataJSON: []byte(`{"imported_from_legacy":true,"source_line":1}`),
	}, {
		ScopeKey: preRenameMeasurementScope, Source: preRenameMeasurementSource,
		Kind: preRenameMeasurementKind, ObservedAt: at.Add(time.Minute), ContentType: "application/json",
		Payload:      []byte(`{"v":1,"session_key":"2026-06-30","market":{"red_clusters":3}}`),
		MetadataJSON: []byte(`{"imported_from_legacy":true,"source_line":2}`),
	}, {
		// Same scope, different source: must not move.
		ScopeKey: preRenameMeasurementScope, Source: "legacy.regime_decision_journal",
		Kind: preRenameMeasurementKind, ObservedAt: at.Add(2 * time.Minute), ContentType: "application/json",
		Payload:      []byte(`{"v":1,"session_key":"2026-06-30"}`),
		MetadataJSON: []byte(`{"imported_from_legacy":true}`),
	}, {
		// Same scope and source, different kind: must not move.
		ScopeKey: preRenameMeasurementScope, Source: preRenameMeasurementSource,
		Kind: "regime_measurement.v1", ObservedAt: at.Add(3 * time.Minute), ContentType: "application/json",
		Payload:      []byte(`{"v":1,"session_key":"2026-06-30"}`),
		MetadataJSON: []byte(`{"imported_from_legacy":true}`),
	}}
	if _, err := store.AppendObservations(ctx, inputs); err != nil {
		t.Fatalf("seed legacy measurement observations: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
