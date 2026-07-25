package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestOpenQuarantinesOnlyAlertDeliveryTypedDecodeFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rawAlertDelivery := json.RawMessage(`{
    "version": 17,
    "generation": 9,
    "private_marker": "typed-decode-private"
  }`)
	var typed alertDeliveryData
	if err := json.Unmarshal(rawAlertDelivery, &typed); err == nil {
		t.Fatal("fixture must fail typed alert delivery decode")
	}
	writeAlertDeliveryQuarantineFixture(t, dir, rawAlertDelivery, AlertModeWatchAndAct)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open isolated typed failure: %v", err)
	}
	assertAlertDeliveryQuarantined(t, store)
	if history := store.AlertHistory(10); len(history) != 1 || history[0].ID != "legacy-canary" {
		t.Fatalf("legacy Canary state unavailable after quarantine: %+v", history)
	}
	assertAlertDeliveryQuarantineArtifact(t, dir, rawAlertDelivery)
}

func TestOpenArchivesValidLegacyUnscopedLedgerAndRecoversFromScopedV3(t *testing.T) {
	t.Parallel()
	seedDir := t.TempDir()
	seed, err := Open(seedDir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 21, 6, 0, 0, 0, time.UTC)
	legacyCandidate := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "legacy-unscoped", "open", at)
	if _, err := seed.ObserveAlertSnapshot(testAlertSnapshot(at, []rpc.AlertSource{legacyCandidate.Source}, []rpc.AlertSource{legacyCandidate.Source}, rpc.AlertCoverageCurrent, legacyCandidate)); err != nil {
		t.Fatal(err)
	}
	seedRaw, err := os.ReadFile(filepath.Join(seedDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seedTop map[string]json.RawMessage
	if err := json.Unmarshal(seedRaw, &seedTop); err != nil {
		t.Fatal(err)
	}
	var legacyLedger map[string]json.RawMessage
	if err := json.Unmarshal(seedTop["alert_delivery"], &legacyLedger); err != nil {
		t.Fatal(err)
	}
	legacyLedger["version"] = json.RawMessage(`"` + legacyAlertDeliveryVersionV1 + `"`)
	delete(legacyLedger, "source_watermarks_by_scope")
	delete(legacyLedger, "previous_contexts")
	delete(legacyLedger, "previous_context_high_water_seq")
	delete(legacyLedger, "baselines")
	legacyLedger["attention_v2_high_water_seq"] = legacyLedger["attention_high_water_seq"]
	legacyLedger["attention_v2_read_through_seq"] = legacyLedger["attention_read_through_seq"]
	delete(legacyLedger, "attention_high_water_seq")
	delete(legacyLedger, "attention_read_through_seq")
	var legacySnapshot map[string]json.RawMessage
	if err := json.Unmarshal(legacyLedger["snapshot"], &legacySnapshot); err != nil {
		t.Fatal(err)
	}
	legacySnapshot["schema_version"] = json.RawMessage(`"` + legacyAlertSnapshotVersionV1 + `"`)
	delete(legacySnapshot, "authority_scope")
	delete(legacySnapshot, "sources")
	var legacyCandidates []map[string]json.RawMessage
	if err := json.Unmarshal(legacySnapshot["candidates"], &legacyCandidates); err != nil {
		t.Fatal(err)
	}
	for i := range legacyCandidates {
		delete(legacyCandidates[i], "presentation_code")
		legacyCandidates[i]["delivery_preference"] = json.RawMessage(`"unapproved"`)
	}
	legacySnapshot["candidates"], err = json.Marshal(legacyCandidates)
	if err != nil {
		t.Fatal(err)
	}
	legacyLedger["snapshot"], err = json.Marshal(legacySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	var legacyEpisodes []map[string]json.RawMessage
	if err := json.Unmarshal(legacyLedger["episodes"], &legacyEpisodes); err != nil {
		t.Fatal(err)
	}
	for i := range legacyEpisodes {
		delete(legacyEpisodes[i], "authority_scope")
	}
	legacyLedger["episodes"], err = json.Marshal(legacyEpisodes)
	if err != nil {
		t.Fatal(err)
	}
	var legacyOccurrences []map[string]json.RawMessage
	if err := json.Unmarshal(legacyLedger["occurrences"], &legacyOccurrences); err != nil {
		t.Fatal(err)
	}
	for i := range legacyOccurrences {
		delete(legacyOccurrences[i], "authority_scope")
		delete(legacyOccurrences[i], "presentation_code")
		delete(legacyOccurrences[i], "disposition")
		legacyOccurrences[i]["delivery_preference"] = json.RawMessage(`"unapproved"`)
		legacyOccurrences[i]["transport_eligible"] = json.RawMessage(`false`)
		legacyOccurrences[i]["attention_v2_seq"] = legacyOccurrences[i]["attention_seq"]
		delete(legacyOccurrences[i], "attention_seq")
		var occurrenceKey string
		if err := json.Unmarshal(legacyOccurrences[i]["occurrence_key"], &occurrenceKey); err != nil {
			t.Fatal(err)
		}
		legacyOccurrences[i]["display_id"], err = json.Marshal(legacyAlertDeliveryDisplayID(occurrenceKey))
		if err != nil {
			t.Fatal(err)
		}
	}
	legacyLedger["occurrences"], err = json.Marshal(legacyOccurrences)
	if err != nil {
		t.Fatal(err)
	}
	rawLegacy, err := json.Marshal(legacyLedger)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	writeAlertDeliveryQuarantineFixture(t, dir, rawLegacy, AlertModeWatchAndAct)
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open recognized v1 ledger: %v", err)
	}
	if store.alertDeliveryQuarantinedLocked() {
		t.Fatal("valid recognized v1 ledger remained blocked instead of being archived")
	}
	if view := store.AlertDelivery(at); view.Initialized || view.AuthorityScope != "" || view.CurrentState == rpc.AlertSnapshotClear || len(view.Occurrences) != 0 || len(store.AlertDeliveriesDue(at)) != 0 {
		t.Fatalf("legacy ledger was silently assigned a scope: %+v", view)
	}
	artifact := assertAlertDeliveryQuarantineArtifact(t, dir, rawLegacy)
	if _, err := os.Stat(artifact); err != nil {
		t.Fatal(err)
	}
	stateRaw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var currentTop map[string]json.RawMessage
	if err := json.Unmarshal(stateRaw, &currentTop); err != nil {
		t.Fatal(err)
	}
	if _, exists := currentTop["alert_delivery"]; exists {
		t.Fatalf("archived legacy ledger remained live: %s", stateRaw)
	}
	if history := store.AlertHistory(10); len(history) != 1 || history[0].ID != "legacy-canary" {
		t.Fatalf("unrelated app authority was not preserved: %+v", history)
	}

	currentAt := at.Add(time.Minute)
	currentCandidate := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "scoped-v3", "open", currentAt)
	view, err := store.ObserveAlertSnapshot(testAlertSnapshot(currentAt, []rpc.AlertSource{currentCandidate.Source}, []rpc.AlertSource{currentCandidate.Source}, rpc.AlertCoverageCurrent, currentCandidate))
	if err != nil {
		t.Fatalf("first scoped v3 observation did not recover: %v", err)
	}
	if !view.Initialized || view.AuthorityScope == "" || view.CurrentState != rpc.AlertSnapshotActive || len(view.Occurrences) != 1 || view.Occurrences[0].Disposition != AlertDispositionCutoverExisting {
		t.Fatalf("scoped v3 recovery view=%+v", view)
	}
	assertAlertDeliveryQuarantineArtifact(t, dir, rawLegacy)
}

func TestOpenQuarantinesAlertDeliverySemanticCorruptionAndPreservesRawAcrossSaves(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rawAlertDelivery := json.RawMessage(`{
    "version" : "unsupported-private-version",
    "generation" : 41,
    "private_marker" : "semantic-private-value",
    "source_watermarks" : {},
    "retired_targets" : {}
  }`)
	var typed alertDeliveryData
	if err := json.Unmarshal(rawAlertDelivery, &typed); err != nil {
		t.Fatalf("fixture must reach semantic validation: %v", err)
	}
	writeAlertDeliveryQuarantineFixture(t, dir, rawAlertDelivery, AlertModeWatchAndAct)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open isolated semantic failure: %v", err)
	}
	assertAlertDeliveryQuarantined(t, store)
	artifactPath := assertAlertDeliveryQuarantineArtifact(t, dir, rawAlertDelivery)
	artifactBefore, err := os.Stat(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	viewBefore := store.AlertDelivery(time.Now().UTC())

	if err := store.SetAlertMode(AlertModeNone); err != nil {
		t.Fatalf("unrelated legacy save failed: %v", err)
	}
	assertMainStateAlertDeliveryRaw(t, dir, rawAlertDelivery)
	artifactBytes, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(artifactBytes, rawAlertDelivery) {
		t.Fatalf("artifact changed after unrelated save\ngot:  %q\nwant: %q", artifactBytes, rawAlertDelivery)
	}

	// A valid new observation is not a repair authority. The save boundary
	// rejects it and retains the exact quarantined value.
	at := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	candidate := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "quarantine", "repair-attempt", at)
	_, err = store.ObserveAlertSnapshot(testAlertSnapshot(at, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate))
	if !errors.Is(err, ErrAlertDeliveryUnavailable) {
		t.Fatalf("automatic replacement err=%v, want ErrAlertDeliveryUnavailable", err)
	}
	for name, err := range map[string]error{
		"prerequisite health": store.SetAlertDeliveryPrerequisiteHealth("", at),
		"target retirement":   store.RetireAlertDeliveryTarget(AlertDeliveryTargetRef("device", "subscription"), at),
		"recovery":            store.RecoverAlertDeliveries(at),
		"compaction":          store.CompactAlertDelivery(at),
	} {
		if !errors.Is(err, ErrAlertDeliveryUnavailable) {
			t.Fatalf("quarantined %s err=%v, want ErrAlertDeliveryUnavailable", name, err)
		}
	}
	if err := store.AddDevice(DeviceGrant{ID: "legacy-device", CreatedAt: at}); err != nil {
		t.Fatalf("legacy device add failed: %v", err)
	}
	legacySubscription := PushSubscription{ID: "legacy-subscription", DeviceID: "legacy-device", Endpoint: "https://push.example/legacy", P256DH: "key", Auth: "auth", CreatedAt: at}
	if err := store.AddPushSubscription(legacySubscription); err != nil {
		t.Fatalf("legacy subscription add failed: %v", err)
	}
	if err := store.RemovePushSubscriptionAt(legacySubscription.ID, at.Add(time.Second)); err != nil {
		t.Fatalf("legacy subscription retirement failed: %v", err)
	}
	assertAlertDeliveryQuarantined(t, store)
	assertMainStateAlertDeliveryRaw(t, dir, rawAlertDelivery)

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
	assertAlertDeliveryQuarantined(t, reopened)
	viewAfter := reopened.AlertDelivery(time.Now().UTC())
	if viewAfter.Generation != viewBefore.Generation || !viewAfter.DeliveryHealth.UpdatedAt.Equal(viewBefore.DeliveryHealth.UpdatedAt) {
		t.Fatalf("quarantine public identity drifted across restart: before=%+v after=%+v", viewBefore, viewAfter)
	}
	artifactAfter, err := os.Stat(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !artifactAfter.ModTime().Equal(artifactBefore.ModTime()) {
		t.Fatalf("idempotent restart rewrote immutable artifact: before=%s after=%s", artifactBefore.ModTime(), artifactAfter.ModTime())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	quarantineCount := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), alertDeliveryQuarantinePrefix) {
			quarantineCount++
		}
	}
	if quarantineCount != 1 {
		t.Fatalf("stable evidence hash produced %d quarantine artifacts", quarantineCount)
	}
}

func TestOpenFailsWhenAlertDeliveryQuarantineCannotBePreserved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rawAlertDelivery := json.RawMessage(`{"version":false,"private_marker":"must-not-be-dropped"}`)
	writeAlertDeliveryQuarantineFixture(t, dir, rawAlertDelivery, AlertModeWatchAndAct)
	artifactPath := filepath.Join(dir, alertDeliveryQuarantineArtifactName(rawAlertDelivery))
	if err := os.Mkdir(artifactPath, 0o700); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dir)
	if store != nil || !errors.Is(err, ErrInvalidPersistedState) {
		t.Fatalf("Open store=%v err=%v, want fatal preservation failure", store, err)
	}
	assertMainStateAlertDeliveryRaw(t, dir, rawAlertDelivery)
}

func TestOpenKeepsWholeFileAndLegacyAuthorityCorruptionFatal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "whole file syntax", raw: `{"alert_settings":`},
		{name: "whole file top level", raw: `[]`},
		{name: "legacy authority with corrupt alert delivery", raw: `{"alert_settings":{"mode":"surprise"},"alert_delivery":{"version":17,"private_marker":"must-not-isolate-legacy"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := Open(dir)
			if store != nil || err == nil {
				t.Fatalf("Open store=%v err=%v, want fatal corruption", store, err)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), alertDeliveryQuarantinePrefix) {
					t.Fatalf("legacy/whole-file corruption created quarantine artifact %q", entry.Name())
				}
			}
		})
	}
}

func writeAlertDeliveryQuarantineFixture(t *testing.T, dir string, rawAlertDelivery json.RawMessage, mode string) {
	t.Helper()
	if !json.Valid(rawAlertDelivery) {
		t.Fatal("invalid alert delivery test fixture")
	}
	raw := []byte(`{"alert_settings":{"mode":"` + mode + `"},"alert_history":[{"id":"legacy-canary","title":"legacy","body":"usable"}],"alert_delivery":`)
	raw = append(raw, rawAlertDelivery...)
	raw = append(raw, '}')
	if !json.Valid(raw) {
		t.Fatalf("invalid state fixture: %s", raw)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertAlertDeliveryQuarantined(t *testing.T, store *Store) {
	t.Helper()
	if store == nil || !store.alertDeliveryQuarantinedLocked() || store.data.AlertDelivery != nil {
		t.Fatalf("store did not retain private quarantine boundary: %+v", store)
	}
	view := store.AlertDelivery(time.Now().UTC())
	if view.Initialized || view.Generation != alertDeliveryQuarantineGeneration || view.AsOf != (time.Time{}) ||
		view.CurrentState != "" || len(view.Occurrences) != 0 || view.Attention.UnreadCount != 0 ||
		view.DeliveryHealth.State != AlertDeliveryHealthUnavailable || view.DeliveryHealth.Class != AlertDeliveryHealthClassInvalidPersistedState || view.DeliveryHealth.UpdatedAt.IsZero() {
		t.Fatalf("quarantine view is not uninitialized/default-deny: %+v", view)
	}
	if due := store.AlertDeliveriesDue(time.Now().UTC()); len(due) != 0 {
		t.Fatalf("quarantined delivery produced due work: %+v", due)
	}
}

func assertAlertDeliveryQuarantineArtifact(t *testing.T, dir string, rawAlertDelivery json.RawMessage) string {
	t.Helper()
	artifactPath := filepath.Join(dir, alertDeliveryQuarantineArtifactName(rawAlertDelivery))
	persisted, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read quarantine artifact: %v", err)
	}
	if !bytes.Equal(persisted, rawAlertDelivery) {
		t.Fatalf("artifact did not preserve exact raw JSON value\ngot:  %q\nwant: %q", persisted, rawAlertDelivery)
	}
	info, err := os.Stat(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode=%v, want private regular 0600", info.Mode())
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode=%04o, want 0700", dirInfo.Mode().Perm())
	}
	return artifactPath
}

func assertMainStateAlertDeliveryRaw(t *testing.T, dir string, want json.RawMessage) {
	t.Helper()
	persisted, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(persisted, &topLevel); err != nil {
		t.Fatalf("decode saved state: %v\n%s", err, persisted)
	}
	if !bytes.Equal(topLevel["alert_delivery"], want) {
		t.Fatalf("main state normalized quarantined raw value\ngot:  %q\nwant: %q", topLevel["alert_delivery"], want)
	}
}
