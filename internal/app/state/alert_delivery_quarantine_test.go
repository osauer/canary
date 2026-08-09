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
)

func TestOpenQuarantinesUnsupportedAlertLedgerWithoutClearingIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"typed decode", json.RawMessage(`{"version":17,"generation":9,"private_marker":"typed"}`)},
		{"unsupported schema", json.RawMessage(`{"version":"alert-delivery-v3","generation":41,"private_marker":"old"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAlertDeliveryQuarantineFixture(t, dir, tc.raw)
			store, err := Open(dir)
			if err != nil {
				t.Fatalf("Open isolated alert ledger: %v", err)
			}
			assertAlertDeliveryQuarantined(t, store)
			if history := store.AlertHistory(10); len(history) != 1 || history[0].ID != "existing-alert" {
				t.Fatalf("unrelated app state unavailable after quarantine: %+v", history)
			}
			artifact := filepath.Join(dir, alertDeliveryQuarantineArtifactName(tc.raw))
			assertExactPrivateFile(t, artifact, tc.raw)

			if err := store.SetAlertMode(AlertModeNone); err != nil {
				t.Fatalf("save unrelated state: %v", err)
			}
			persisted, err := os.ReadFile(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			var top map[string]json.RawMessage
			if err := json.Unmarshal(persisted, &top); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(top["alert_delivery"], tc.raw) {
				t.Fatalf("unsupported alert ledger was normalized or cleared: %s", top["alert_delivery"])
			}
		})
	}
}

func TestOpenFailsWhenAlertLedgerCannotBePreserved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := json.RawMessage(`{"version":false,"private_marker":"must-not-be-dropped"}`)
	writeAlertDeliveryQuarantineFixture(t, dir, raw)
	if err := os.Mkdir(filepath.Join(dir, alertDeliveryQuarantineArtifactName(raw)), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if store != nil || !errors.Is(err, ErrInvalidPersistedState) {
		t.Fatalf("Open store=%v err=%v, want fatal preservation failure", store, err)
	}
}

func TestOpenDoesNotIsolateWholeFileOrSettingsCorruption(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"alert_settings":`,
		`[]`,
		`{"alert_settings":{"mode":"surprise"},"alert_delivery":{"version":17}}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := Open(dir)
		if store != nil || err == nil {
			t.Fatalf("Open store=%v err=%v, want fatal corruption", store, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), alertDeliveryQuarantinePrefix) {
				t.Fatalf("whole-file corruption created quarantine artifact %q", entry.Name())
			}
		}
	}
}

func writeAlertDeliveryQuarantineFixture(t *testing.T, dir string, alertDelivery json.RawMessage) {
	t.Helper()
	raw := append([]byte(`{"alert_settings":{"mode":"watch_and_act"},"alert_history":[{"id":"existing-alert","title":"existing","body":"usable"}],"alert_delivery":`), alertDelivery...)
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
		t.Fatalf("store did not retain quarantine boundary: %+v", store)
	}
	view := store.AlertDelivery(time.Now().UTC())
	if view.Initialized || view.Generation != alertDeliveryQuarantineGeneration || len(view.Occurrences) != 0 ||
		view.Attention.UnreadCount != 0 || view.DeliveryHealth.State != AlertDeliveryHealthUnavailable ||
		view.DeliveryHealth.Class != AlertDeliveryHealthClassInvalidPersistedState {
		t.Fatalf("quarantine view is not uninitialized/default-deny: %+v", view)
	}
	if due := store.AlertDeliveriesDue(time.Now().UTC()); len(due) != 0 {
		t.Fatalf("quarantined delivery produced due work: %+v", due)
	}
}

func assertExactPrivateFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("preserved bytes changed\ngot: %q\nwant: %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("preserved artifact mode=%v", info.Mode())
	}
}
