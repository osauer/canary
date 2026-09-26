package risk

import (
	"encoding/json"
	"strings"
	"testing"
)

// The stress policy keeps no second definition of what the Rulebook owns:
// concentration is rules 1 and 16 (amendment 15) and net exposure is rule 15
// (amendment 16), so no net-delta or single-name level remains in it.
func TestStressPolicyCarriesNoRulebookLevels(t *testing.T) {
	raw, err := json.Marshal(DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		if strings.HasPrefix(key, "net_delta") || strings.HasPrefix(key, "single_name") {
			t.Errorf("stress policy still carries %s; the Rulebook owns that level", key)
		}
	}
	if StressPolicyFingerprintVersion != "stress-policy-fp-v3" {
		t.Fatalf("stress policy fingerprint projection = %s, want stress-policy-fp-v3 after the net-delta levels left it", StressPolicyFingerprintVersion)
	}
}
