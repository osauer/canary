package risk

import (
	"encoding/json"
	"strings"
	"testing"
)

// drawdown.release accepts only its two values; anything else is refused
// rather than read as manual or automatic.
func TestConstitutionDrawdownReleaseIsAClosedSet(t *testing.T) {
	for _, value := range []string{"", DrawdownReleaseManual, DrawdownReleaseAutomatic} {
		c := Constitution{Drawdown: ConstitutionDrawdown{Release: value}}
		if err := c.validateDrawdownRelease(); err != nil {
			t.Fatalf("%q refused: %v", value, err)
		}
	}
	c := Constitution{Drawdown: ConstitutionDrawdown{Release: "auto"}}
	if err := c.validateDrawdownRelease(); err == nil || !strings.Contains(err.Error(), "manual or automatic") {
		t.Fatalf("an unknown release value was accepted: %v", err)
	}
	if (Constitution{}).EffectiveDrawdownRelease() != DrawdownReleaseManual || (&Constitution{}).ReleasesAutomatically() || (*Constitution)(nil).ReleasesAutomatically() {
		t.Fatal("the default is not manual")
	}
}

// A policy that never set drawdown.release keeps its fingerprint: the key is
// absent from the fingerprint's JSON until an owner writes it.
func TestConstitutionFingerprintIgnoresAnUnsetRelease(t *testing.T) {
	raw, err := json.Marshal(ConstitutionDrawdown{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "release") {
		t.Fatalf("an unset release entered the fingerprint: %s", raw)
	}
	set := Constitution{Drawdown: ConstitutionDrawdown{Release: DrawdownReleaseAutomatic}}
	if set.FingerprintKey() == (Constitution{}).FingerprintKey() {
		t.Fatal("setting release left the fingerprint unchanged")
	}
}
