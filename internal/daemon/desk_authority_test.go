package daemon

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func authorityCompanionConfirmation(t *testing.T, d testDevice, terms string) rpc.CashPolicyConfirmation {
	t.Helper()
	actionID := "synthetic-authority-action"
	review, _ := json.Marshal(map[string]string{"kind": "automatic_authority", "terms": terms})
	nonce := make([]byte, 32)
	rand.Read(nonce)
	challenge := base64.RawURLEncoding.EncodeToString(nonce)
	digest := deskAuthorityDigest(actionID, terms, string(review))
	hash := sha256.Sum256([]byte(deskAuthorityCompanionPrefix + "\n" + actionID + "\n" + digest + "\n" + challenge))
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

func TestDeskAuthoritySignatureCannotAuthorizeOtherTerms(t *testing.T) {
	d := newTestDevice(t, "companion")
	key, err := risk.ParseDeskDeviceKey("companion", d.id+":"+base64.RawURLEncoding.EncodeToString(d.point))
	if err != nil {
		t.Fatal(err)
	}
	terms := `{"scope":"full","pricing":"fixed-midpoint"}`
	c := authorityCompanionConfirmation(t, d, terms)
	if got, err := verifyDeskAuthorityDevice([]risk.DeskDeviceKey{key}, c, terms); err != nil || got != d.credential() {
		t.Fatalf("signature: %s %v", got, err)
	}
	for _, change := range []func(*rpc.CashPolicyConfirmation){
		func(c *rpc.CashPolicyConfirmation) { c.DeskActionID = "another-action" },
		func(c *rpc.CashPolicyConfirmation) { c.Credential = "companion:untrusted" },
		func(c *rpc.CashPolicyConfirmation) { c.ConfirmedBy = "old-confirmation" },
		func(c *rpc.CashPolicyConfirmation) {
			c.Envelope = strings.ReplaceAll(c.Envelope, "fixed-midpoint", "market")
		},
	} {
		bad := c
		change(&bad)
		if _, err := verifyDeskAuthorityDevice([]risk.DeskDeviceKey{key}, bad, terms); err == nil {
			t.Fatal("tampered confirmation passed")
		}
	}
	if _, err := verifyDeskAuthorityDevice(nil, c, terms); err == nil {
		t.Fatal("unpinned credential passed")
	}
	if _, err := verifyCashPolicyDevice([]risk.DeskDeviceKey{key}, &c, terms); err == nil {
		t.Fatal("mandate signature crossed into cash policy")
	}
}

func TestDeskAuthorityControlFencesAndRestart(t *testing.T) {
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	s.startedAt = time.Now().UTC()
	capability := strings.Repeat("synthetic-private-controller-", 2)
	hash := sha256.Sum256([]byte(capability))
	head, err := s.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	terms := rpc.DeskAuthorityTerms{AuthorityEpoch: head.AuthorityEpoch, Version: 1, Kind: "desk_automatic_authority", ID: "synthetic-mandate", AccountID: "synthetic", AccountMode: "paper", MaximumScope: "full", ControllerHash: hex.EncodeToString(hash[:]), Generation: 0, ConfirmBefore: time.Now().Add(time.Minute), Pricing: "fixed-midpoint", Algorithm: "Adaptive", TIF: "DAY", Protection: "existing-policy", Capacity: "existing-policy-reuse-confirmed-funds", Selection: "desk-deterministic-controller"}
	initial := deskAuthorityRecord{Terms: &terms, Scope: "full"}
	if _, err := s.saveDeskAuthority(t.Context(), 0, initial, "test-confirmed"); err != nil {
		t.Fatal(err)
	}
	control := rpc.DeskAuthorityControlParams{ID: terms.ID, Capability: capability, ExpectedGeneration: 1, RequestID: "running", Scope: "full", Running: true, PreferencesHash: strings.Repeat("a", 64), AdaptivePriority: "Normal"}
	call := func(in rpc.DeskAuthorityControlParams) (rpc.DeskAuthorityStatus, error) {
		raw, _ := json.Marshal(in)
		return s.handleDeskAuthorityControl(t.Context(), &rpc.Request{Params: raw})
	}
	got, err := call(control)
	if err != nil || !got.Running || got.Generation != 2 {
		t.Fatalf("running: %+v %v", got, err)
	}
	replay, err := call(control)
	if err != nil || replay.Generation != 2 {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	bad := control
	bad.ExpectedGeneration = 2
	bad.RequestID = "invalid-preferences"
	bad.PreferencesHash = strings.Repeat("z", 64)
	if _, err := call(bad); err == nil {
		t.Fatal("non-hex preferences accepted")
	}
	bad = control
	bad.RequestID = "stale"
	if _, err := call(bad); err == nil {
		t.Fatal("stale generation passed")
	}
	bad = control
	bad.ExpectedGeneration = 2
	bad.RequestID = "forged"
	bad.Capability = "another-long-private-controller-capability"
	if _, err := call(bad); err == nil {
		t.Fatal("forged controller passed")
	}
	s.startedAt = s.startedAt.Add(time.Second)
	got, err = s.handleDeskAuthorityStatus(t.Context())
	if err != nil || got.Running || got.Scope != "full" {
		t.Fatalf("restart lost scope or resumed automatically: %+v %v", got, err)
	}
	control.ExpectedGeneration = 2
	control.RequestID = "disarm"
	control.Scope = "off"
	control.Running = false
	control.PreferencesHash, control.AdaptivePriority = "", ""
	got, err = call(control)
	if err != nil || got.Scope != "off" || got.Generation != 3 {
		t.Fatalf("disarm: %+v %v", got, err)
	}
	control.ExpectedGeneration = 3
	control.RequestID = "unauthorised-rearm"
	control.Scope = "full"
	control.Running = true
	if _, err := call(control); err == nil {
		t.Fatal("controller rearmed without device")
	}
}

func TestDeskAuthorityRejectsRestoredRecordFromOtherStore(t *testing.T) {
	first, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	second, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	head, err := first.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	terms := rpc.DeskAuthorityTerms{AuthorityEpoch: head.AuthorityEpoch, Version: 1, Kind: "desk_automatic_authority", ID: "synthetic-reset", AccountID: "synthetic", AccountMode: "paper", MaximumScope: "full", ControllerHash: strings.Repeat("a", 64), ConfirmBefore: time.Now().Add(time.Minute), Pricing: "fixed-midpoint", Algorithm: "Adaptive", TIF: "DAY", Protection: "existing-policy", Capacity: "existing-policy-reuse-confirmed-funds", Selection: "desk-deterministic-controller"}
	record := deskAuthorityRecord{Terms: &terms, Scope: "full"}
	if _, err := second.saveDeskAuthority(t.Context(), 0, record, "test-restore"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.readDeskAuthority(t.Context()); err == nil {
		t.Fatal("foreign authority epoch accepted after store replacement")
	}
}

func TestDeskAuthorityFinalFenceHoldsThroughWire(t *testing.T) {
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	s.startedAt = cashPolicyTestNow
	head, err := s.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("synthetic-private-controller-", 2)
	hash := sha256.Sum256([]byte(secret))
	terms := rpc.DeskAuthorityTerms{AuthorityEpoch: head.AuthorityEpoch, Version: 1, Kind: "desk_automatic_authority", ID: "synthetic-wire", AccountID: "synthetic", AccountMode: "paper", MaximumScope: "full", ControllerHash: hex.EncodeToString(hash[:]), ConfirmBefore: cashPolicyTestNow.Add(time.Minute), Pricing: "fixed-midpoint", Algorithm: "Adaptive", TIF: "DAY", Protection: "existing-policy", Capacity: "existing-policy-reuse-confirmed-funds", Selection: "desk-deterministic-controller"}
	row := deskAuthorityRecord{Terms: &terms, Scope: "full", Running: true, PreferencesHash: strings.Repeat("a", 64), AdaptivePriority: "Normal", ControlProcess: s.startedAt}
	if _, err := s.saveDeskAuthority(t.Context(), 0, row, "test-confirmed"); err != nil {
		t.Fatal(err)
	}
	status := rpc.TradingStatus{Account: "synthetic", Mode: "paper"}
	fence := deskAuthorityFence{ID: terms.ID, Generation: 1, PreferencesHash: row.PreferencesHash, AdaptivePriority: "Normal", Action: "entry", ValidUntil: cashPolicyTestNow.Add(time.Minute)}
	for name, change := range map[string]func(*deskAuthorityFence, *rpc.TradingStatus){
		"generation":  func(f *deskAuthorityFence, _ *rpc.TradingStatus) { f.Generation++ },
		"identity":    func(f *deskAuthorityFence, _ *rpc.TradingStatus) { f.ID = "other" },
		"preferences": func(f *deskAuthorityFence, _ *rpc.TradingStatus) { f.PreferencesHash = strings.Repeat("b", 64) },
		"priority":    func(f *deskAuthorityFence, _ *rpc.TradingStatus) { f.AdaptivePriority = "Urgent" },
		"action":      func(f *deskAuthorityFence, _ *rpc.TradingStatus) { f.Action = "unknown" },
		"expired":     func(f *deskAuthorityFence, _ *rpc.TradingStatus) { f.ValidUntil = cashPolicyTestNow },
		"account":     func(_ *deskAuthorityFence, st *rpc.TradingStatus) { st.Account = "other" },
		"mode":        func(_ *deskAuthorityFence, st *rpc.TradingStatus) { st.Mode = "live" },
	} {
		t.Run(name, func(t *testing.T) {
			bad, st := fence, status
			change(&bad, &st)
			release, err := s.lockDeskAuthorityForWire(t.Context(), &bad, st)
			if release != nil {
				release()
			}
			if err == nil {
				t.Fatal("changed authority allowed at wire")
			}
		})
	}
	release, err := s.lockDeskAuthorityForWire(t.Context(), &fence, status)
	if err != nil {
		t.Fatal(err)
	}
	// The reader lease must remain held until transport finishes. TryLock gives
	// a deterministic witness without a timing assertion or sleep.
	if s.deskAuthorityMu.TryLock() {
		s.deskAuthorityMu.Unlock()
		release()
		t.Fatal("revocation could finish before wire lease released")
	}
	release()
	raw, _ := json.Marshal(rpc.DeskAuthorityControlParams{ID: terms.ID, Capability: secret, ExpectedGeneration: 1, RequestID: "pause-at-wire", Scope: "full", Running: false})
	if _, err := s.handleDeskAuthorityControl(t.Context(), &rpc.Request{Params: raw}); err != nil {
		t.Fatal(err)
	}
	release, err = s.lockDeskAuthorityForWire(t.Context(), &fence, status)
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("old prepared order passed after pause acknowledgement")
	}
}

func TestDeskAuthorityDeviceConfirmationAndResetReplay(t *testing.T) {
	device := newTestDevice(t, "companion")
	makeServer := func() *Server {
		s, _, _, _ := cashPolicyServerWithConstitution(t, cashPolicyTestFile, pinned(cashPolicyTestConstitution, device))
		s.cfg = &config.Resolved{Gateway: config.Gateway{Account: "DU1234567"}, Trading: config.Trading{Mode: "paper"}}
		s.startedAt = cashPolicyTestNow
		return s
	}
	s := makeServer()
	capability := strings.Repeat("synthetic-secret-", 3)
	hash := sha256.Sum256([]byte(capability))
	raw, _ := json.Marshal(rpc.DeskAuthorityPrepareParams{Scope: "full", ControllerHash: hex.EncodeToString(hash[:])})
	prepared, err := s.handleDeskAuthorityPrepare(t.Context(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	confirmed := rpc.DeskAuthorityConfirmParams{Terms: prepared.Terms, Digest: prepared.Digest, Confirmation: authorityCompanionConfirmation(t, device, prepared.Terms)}
	raw, _ = json.Marshal(confirmed)
	status, err := s.handleDeskAuthorityConfirm(t.Context(), &rpc.Request{Params: raw})
	if err != nil || status.Scope != "full" || status.Running || status.Verified != device.credential() {
		t.Fatalf("confirmation: %+v %v", status, err)
	}
	fresh := makeServer()
	if _, err := fresh.handleDeskAuthorityConfirm(t.Context(), &rpc.Request{Params: raw}); err == nil {
		t.Fatal("signed authority replayed into a reset store")
	}
	control, _ := json.Marshal(rpc.DeskAuthorityControlParams{ID: status.Terms.ID, Capability: capability, ExpectedGeneration: status.Generation, RequestID: "immediate-disarm", Scope: "off"})
	if _, err := s.handleDeskAuthorityControl(t.Context(), &rpc.Request{Params: control}); err != nil {
		t.Fatal(err)
	}
	status, err = s.handleDeskAuthorityConfirm(t.Context(), &rpc.Request{Params: raw})
	if err != nil || status.Scope != "off" || status.Running {
		t.Fatalf("confirmation replay rearmed: %+v %v", status, err)
	}
	// Test the signature domain directly, independently of the review kind.
	fields, err := decodeCashPolicyEnvelope(confirmed.Confirmation.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	digest := deskAuthorityDigest(confirmed.Confirmation.DeskActionID, prepared.Terms, fields["review_json"])
	if err := verifyDeskCompanionSignature(&device.key.PublicKey, confirmed.Confirmation.DeskActionID, digest, fields["challenge"], fields["signature"]); err == nil {
		t.Fatal("authority signature accepted in cash-policy signing domain")
	}
}

func TestDeskAuthorityPasskeyUsesItsOwnSigningDomain(t *testing.T) {
	device := newTestDevice(t, "passkey")
	key, err := risk.ParseDeskDeviceKey("passkey", device.id+":"+base64.RawURLEncoding.EncodeToString(device.point))
	if err != nil {
		t.Fatal(err)
	}
	terms := `{"scope":"protect"}`
	actionID := "synthetic-passkey-authority"
	review, _ := json.Marshal(map[string]string{"kind": "automatic_authority", "terms": terms})
	digest := deskAuthorityDigest(actionID, terms, string(review))
	rp := sha256.Sum256([]byte(deskPasskeyRelyingParty))
	auth := append(append([]byte{}, rp[:]...), 0x05, 0, 0, 0, 7)
	nonce := make([]byte, 32)
	rand.Read(nonce)
	binding := sha256.Sum256(append([]byte(deskAuthorityPasskeyPrefix+"\n"+actionID+"\n"+digest+"\n"), nonce...))
	client, _ := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(append(nonce, binding[:]...)), "origin": "http://localhost:8791"})
	clientHash := sha256.Sum256(client)
	signed := sha256.Sum256(append(append([]byte{}, auth...), clientHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, device.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{"credential": "passkey", "credential_id": device.id, "review_json": string(review), "authenticator_data": base64.RawURLEncoding.EncodeToString(auth), "client_data_json": base64.RawURLEncoding.EncodeToString(client), "signature": base64.RawURLEncoding.EncodeToString(sig)}
	envelope, _ := json.Marshal(fields)
	c := rpc.CashPolicyConfirmation{DeskActionID: actionID, Credential: device.credential(), Envelope: string(envelope)}
	if _, err := verifyDeskAuthorityDevice([]risk.DeskDeviceKey{key}, c, terms); err != nil {
		t.Fatal(err)
	}
	if err := verifyDeskPasskeyAssertion(&device.key.PublicKey, actionID, digest, fields["authenticator_data"], fields["client_data_json"], fields["signature"]); err == nil {
		t.Fatal("mandate assertion crossed into cash-policy signing domain")
	}
	if _, err := verifyDeskAuthorityDevice([]risk.DeskDeviceKey{key}, c, `{"scope":"full"}`); err == nil {
		t.Fatal("protection signature upgraded to full")
	}
}
