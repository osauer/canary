package daemon

import (
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestDefaultPolicyTOMLRoundTrips gates the dump command: the printed TOML
// must decode back into exactly the embedded default, with no undecoded keys
// and passing the same validation the policy manager applies to user files —
// otherwise `canary policy default > file` would hand the user a broken start.
func TestDefaultPolicyTOMLRoundTrips(t *testing.T) {
	t.Run("protection", func(t *testing.T) {
		raw, err := DefaultPolicyTOML("protection")
		if err != nil {
			t.Fatalf("dump: %v", err)
		}
		var decoded protectionPolicy
		md, err := toml.Decode(string(raw), &decoded)
		if err != nil {
			t.Fatalf("decode dumped TOML: %v", err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			t.Fatalf("dumped TOML has undecoded keys: %v", undecoded)
		}
		if want := defaultProtectionPolicy(); !reflect.DeepEqual(decoded, want) {
			t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", decoded, want)
		}
		if err := validateProtectionPolicy(decoded); err != nil {
			t.Fatalf("dumped default fails its own validation: %v", err)
		}
	})
	t.Run("opportunity", func(t *testing.T) {
		raw, err := DefaultPolicyTOML("opportunity")
		if err != nil {
			t.Fatalf("dump: %v", err)
		}
		var decoded opportunityPolicy
		md, err := toml.Decode(string(raw), &decoded)
		if err != nil {
			t.Fatalf("decode dumped TOML: %v", err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			t.Fatalf("dumped TOML has undecoded keys: %v", undecoded)
		}
		if want := defaultOpportunityPolicy(); !reflect.DeepEqual(decoded, want) {
			t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", decoded, want)
		}
		if err := validateOpportunityPolicy(decoded); err != nil {
			t.Fatalf("dumped default fails its own validation: %v", err)
		}
	})
	if _, err := DefaultPolicyTOML("rulebook"); err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Fatalf("unknown policy name must error, got %v", err)
	}
}
