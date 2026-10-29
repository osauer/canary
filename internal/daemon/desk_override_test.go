package daemon

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// deskOverrideConfirmation is the owner's companion signing Desk's review of
// prepared override terms in the override's own signing domain.
func deskOverrideConfirmation(t *testing.T, d testDevice, actionID, terms string) rpc.CashPolicyConfirmation {
	t.Helper()
	review, _ := json.Marshal(map[string]string{"kind": rpc.DeskOverrideReviewKind, "terms": terms})
	nonce := make([]byte, 32)
	rand.Read(nonce)
	challenge := base64.RawURLEncoding.EncodeToString(nonce)
	digest := deskOverrideDigest(actionID, terms, string(review))
	hash := sha256.Sum256([]byte(deskOverrideCompanionPrefix + "\n" + actionID + "\n" + digest + "\n" + challenge))
	r, s, err := ecdsa.Sign(rand.Reader, d.key, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	envelope, _ := json.Marshal(map[string]string{"credential": "companion", "key_id": d.id, "review_json": string(review), "challenge": challenge, "signature": base64.RawURLEncoding.EncodeToString(sig)})
	return rpc.CashPolicyConfirmation{DeskActionID: actionID, Credential: d.credential(), Envelope: string(envelope)}
}

func deskOverrideServer(t *testing.T) (*Server, *riskCapitalStore, testDevice) {
	t.Helper()
	device := newTestDevice(t, "companion")
	c := testMandateConstitution()
	c.Override.MaxDurationHours = new(24)
	c.DeskDevice = &risk.ConstitutionDeskDevice{Companion: device.id + ":" + base64.RawURLEncoding.EncodeToString(device.point)}
	srv, st := mandateTestServer(t, c)
	return srv, st, device
}

func prepareDeskOverride(t *testing.T, s *Server, p rpc.DeskOverridePrepareParams) (rpc.DeskOverridePrepared, error) {
	t.Helper()
	return s.handleDeskOverridePrepare(t.Context(), rawParams(t, p))
}

func confirmDeskOverride(t *testing.T, s *Server, terms, digest string, c rpc.CashPolicyConfirmation) (*rpc.RiskPolicyWriteResult, error) {
	t.Helper()
	return s.handleDeskOverrideConfirm(t.Context(), rawParams(t, rpc.DeskOverrideConfirmParams{Terms: terms, Digest: digest, Confirmation: c}))
}

// The owner's one-click override from Desk's console (owner, 2026-10-29
// 15:31 CET: "one-click: yes, ok"): Canary grants mandate.vol_target_pct on
// a device signature it verifies itself against the key pinned in
// [desk_device], journals it as granted from the console, and grants
// nothing without that proof, for no other control, and never on an origin
// claim.
func TestDeskOverrideNeedsTheOwnersDeviceForTheVolatilityTargetOnly(t *testing.T) {
	s, st, device := deskOverrideServer(t)
	scope := s.currentBrokerStateScope()
	vol := rpc.DeskOverridePrepareParams{Control: risk.MandateAdditionsOverrideControl, Reason: "hedge before earnings", Hours: 4}
	prepared, err := prepareDeskOverride(t, s, vol)
	if err != nil {
		t.Fatal(err)
	}
	active := func() int { return len(st.ActiveOverridesForScope(scope)) }

	// policy.override stays human-only: an agent origin, an empty one or a
	// made-up one is refused for the volatility target too.
	for _, origin := range []string{rpc.OrderOriginAgent, "", "desk-console"} {
		if _, err := s.handleRiskPolicyOverride(t.Context(), rawParams(t, rpc.OverrideParams{Control: vol.Control, Reason: vol.Reason, Hours: vol.Hours, Origin: origin})); err == nil || !strings.Contains(err.Error(), "human-only") {
			t.Fatalf("policy.override from origin %q: %v, want human-only", origin, err)
		}
	}

	// Without the owner's device proof nothing is granted.
	stranger := newTestDevice(t, "companion")
	good := deskOverrideConfirmation(t, device, "synthetic-override-action", prepared.Terms)
	relied := good
	relied.ConfirmedBy = "desk-cash-policy-earlier"
	unsigned := rpc.CashPolicyConfirmation{DeskActionID: "synthetic-override-action", Credential: device.credential(), Envelope: `{"credential":"companion","key_id":"` + device.id + `"}`}
	review, _ := json.Marshal(map[string]string{"kind": rpc.DeskOverrideReviewKind, "terms": prepared.Terms})
	for name, c := range map[string]rpc.CashPolicyConfirmation{
		"no confirmation":         {},
		"unsigned envelope":       unsigned,
		"an earlier confirmation": relied,
		"an unpinned device":      deskOverrideConfirmation(t, stranger, "synthetic-override-action", prepared.Terms),
		"an arming signature":     authorityCompanionConfirmation(t, device, prepared.Terms),
		"a cash settings save":    *device.confirm(t, "synthetic-override-action", prepared.Terms, string(review)),
	} {
		if _, err := confirmDeskOverride(t, s, prepared.Terms, prepared.Digest, c); err == nil {
			t.Fatalf("%s granted an override", name)
		}
	}
	// The signature covers these exact terms: longer hours under it fail.
	var terms rpc.DeskOverrideTerms
	if err := json.Unmarshal([]byte(prepared.Terms), &terms); err != nil {
		t.Fatal(err)
	}
	longer := terms
	longer.Hours = 24
	raw, _ := json.Marshal(longer)
	if _, err := confirmDeskOverride(t, s, string(raw), cashPolicyDigest(raw), good); err == nil {
		t.Fatal("a signature over 4 hours granted 24")
	}
	if active() != 0 {
		t.Fatalf("overrides granted without proof: %+v", st.ActiveOverridesForScope(scope))
	}

	// Another control: refused before the device is asked, and refused even
	// when the device signed terms that name it.
	for _, control := range []string{"mandate.max_loss_pct", "drawdown.warn_consumed_pct", risk.OrderLimitFloorOverrideControl} {
		if _, err := prepareDeskOverride(t, s, rpc.DeskOverridePrepareParams{Control: control, Reason: "r", Hours: 1}); err == nil {
			t.Fatalf("prepared an override of %s", control)
		}
		other := terms
		other.ID, other.Control = "synthetic-other-"+control, control
		raw, _ := json.Marshal(other)
		if _, err := confirmDeskOverride(t, s, string(raw), cashPolicyDigest(raw), deskOverrideConfirmation(t, device, "synthetic-other", string(raw))); err == nil {
			t.Fatalf("a signed override of %s was granted", control)
		}
	}
	// Reason and hours stay mandatory, the hours capped by the policy.
	for _, p := range []rpc.DeskOverridePrepareParams{{Control: vol.Control, Hours: 4}, {Control: vol.Control, Reason: "r"}, {Control: vol.Control, Reason: "r", Hours: 25}} {
		if _, err := prepareDeskOverride(t, s, p); err == nil {
			t.Fatalf("prepared %+v", p)
		}
	}

	// With the proof: granted from the console, the credential verified.
	res, err := confirmDeskOverride(t, s, prepared.Terms, prepared.Digest, good)
	if err != nil {
		t.Fatal(err)
	}
	o := res.Override
	if o == nil || o.Control != risk.MandateAdditionsOverrideControl || o.GrantedFrom != rpc.OverrideGrantedFromConsole || o.Verified != device.credential() ||
		o.Reason != vol.Reason || o.ExpiresAt.Sub(o.GrantedAt) != 4*time.Hour || active() != 1 {
		t.Fatalf("console override: %+v", o)
	}
	// A retry of the same confirmation answers with the same override and
	// grants no second one.
	again, err := confirmDeskOverride(t, s, prepared.Terms, prepared.Digest, good)
	if err != nil || again.Override == nil || again.Override.ID != o.ID || active() != 1 {
		t.Fatalf("retry: %+v %v (active %d)", again, err, active())
	}

	// Terms past their confirmation window are refused.
	late, err := prepareDeskOverride(t, s, vol)
	if err != nil {
		t.Fatal(err)
	}
	now := s.nowUTC()
	s.now = func() time.Time { return now.Add(deskOverrideConfirmWindow + time.Second) }
	if _, err := confirmDeskOverride(t, s, late.Terms, late.Digest, deskOverrideConfirmation(t, device, "synthetic-late", late.Terms)); err == nil {
		t.Fatal("terms past their window granted an override")
	}
	if active() != 1 {
		t.Fatalf("active overrides: %d", active())
	}
}

// The override's signing domain is its own: its signature verifies neither
// as an arming nor as a cash settings save.
func TestDeskOverrideSignatureStaysInItsDomain(t *testing.T) {
	d := newTestDevice(t, "companion")
	key, err := risk.ParseDeskDeviceKey("companion", d.id+":"+base64.RawURLEncoding.EncodeToString(d.point))
	if err != nil {
		t.Fatal(err)
	}
	terms := `{"control":"mandate.vol_target_pct"}`
	c := deskOverrideConfirmation(t, d, "synthetic-override", terms)
	if got, err := verifyDeskOverrideDevice([]risk.DeskDeviceKey{key}, c, terms); err != nil || got != d.credential() {
		t.Fatalf("override signature: %s %v", got, err)
	}
	if _, err := verifyDeskAuthorityDevice([]risk.DeskDeviceKey{key}, c, terms); err == nil {
		t.Fatal("an override signature armed automatic trading")
	}
	if _, err := verifyCashPolicyDevice([]risk.DeskDeviceKey{key}, &c, terms); err == nil {
		t.Fatal("an override signature saved cash settings")
	}
}
