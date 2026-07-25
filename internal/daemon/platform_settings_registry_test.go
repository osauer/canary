package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// validSettingsValue returns a kind-appropriate JSON value for a registry key.
func validSettingsValue(t *testing.T, kind rpc.SettingsKeyKind) json.RawMessage {
	t.Helper()
	switch kind {
	case rpc.SettingsKindBool:
		return json.RawMessage(`true`)
	case rpc.SettingsKindFloat:
		return json.RawMessage(`1500.5`)
	case rpc.SettingsKindInt:
		return json.RawMessage(`3`)
	case rpc.SettingsKindDateMap:
		return json.RawMessage(`{"AAPL":"2026-08-04Tamc"}`)
	default:
		t.Fatalf("unhandled settings kind %q — extend validSettingsValue", kind)
		return nil
	}
}

// nestedPatch builds {"a":{"b":{"c":<value>}}} from a dotted key.
func nestedPatch(t *testing.T, key string, value json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	parts := strings.Split(key, ".")
	raw := value
	for i := len(parts) - 1; i > 0; i-- {
		obj, err := json.Marshal(map[string]json.RawMessage{parts[i]: raw})
		if err != nil {
			t.Fatal(err)
		}
		raw = obj
	}
	return map[string]json.RawMessage{parts[0]: raw}
}

// TestSettingsRegistryParity is the drift gate between the shared settings
// registry (internal/rpc), the daemon's patch flattener, and the per-key
// appliers: every registry key must flatten to itself and apply cleanly,
// and null must be accepted everywhere (null clears a runtime override).
func TestSettingsRegistryParity(t *testing.T) {
	for _, spec := range rpc.SettingsKeys() {
		t.Run(spec.Key, func(t *testing.T) {
			for name, value := range map[string]json.RawMessage{
				"valid": validSettingsValue(t, spec.Kind),
				"null":  json.RawMessage(`null`),
			} {
				flat, err := flattenSettingsPatch(nestedPatch(t, spec.Key, value))
				if err != nil {
					t.Fatalf("%s flatten: %v", name, err)
				}
				raw, ok := flat[spec.Key]
				if !ok || len(flat) != 1 {
					t.Fatalf("%s flatten = %v, want exactly %q", name, flat, spec.Key)
				}
				next := &platformSettingsData{Version: platformSettingsDocVersion}
				if err := applySettingsKey(next, spec.Key, raw); err != nil {
					t.Fatalf("%s apply: %v", name, err)
				}
			}
			if spec.Doc == "" {
				t.Error("registry key has no Doc — the generated reference and --help would render an empty description")
			}
		})
	}
}

func TestSettingsPatchUnknownAndReadOnlyFields(t *testing.T) {
	cases := []struct {
		patch string
		want  string
	}{
		{`{"bogus":true}`, `unknown settings field bogus`},
		{`{"features":{"nope":true}}`, `unknown settings field features.nope`},
		{`{"features":{"rulebook":{"nope":true}}}`, `unknown settings field features.rulebook.nope`},
		{`{"regime":{"journal":{"nope":true}}}`, `unknown settings field regime.journal.nope`},
		{`{"trading":{"mode":"live"}}`, `settings field trading.mode is read-only`},
		{`{"trading":{"limits":{"nope":1}}}`, `unknown settings field trading.limits.nope`},
		{`{"features":5}`, `features must be an object`},
	}
	for _, tc := range cases {
		var patch map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.patch), &patch); err != nil {
			t.Fatal(err)
		}
		_, err := flattenSettingsPatch(patch)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("flatten(%s) error = %v, want containing %q", tc.patch, err, tc.want)
		}
	}
}
