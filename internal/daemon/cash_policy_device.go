package daemon

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Canary's own check of the owner's device confirmation (owner decision
// 2026-10-07 08:33 CEST: Desk's Settings writes the order caps, so Canary
// verifies the confirmation itself rather than keeping it for audit). Desk
// enrols the owner's credential, a passkey or the paired companion's Secure
// Enclave key, and Canary never sees that enrolment; the owner pins the
// credential's public key in the constitution's [desk_device] once, by hand,
// from the key Desk shows (Settings shows the exact line; the companion's
// id is base64url of the first 16 bytes of the point's SHA-256, the
// passkey's its WebAuthn credential id). A malformed line never refuses the
// constitution: the loader keeps the policy and this file refuses cap changes
// with the line's own error. From then on a save's envelope must carry the
// signature the device made over Desk's digest, and Canary recomputes that
// digest from the request's own terms: the chain the companion checks before
// it signs (Desk: cmd/desk-presence/Policy.swift, execution_policy.go). No
// pairing, challenge or key exchange runs between Desk and Canary; Desk's
// challenge stays Desk's and only binds the signature to one action.
//
// What is proven: the pinned key signed a text that names the Desk action,
// a digest over exactly these terms and the review shown with them. What is
// not proven: that the review's wording was Canary's (Desk builds it from
// Canary's check; Canary checks only that it lists the terms' changes), nor
// where the key lives.

const (
	// deskPolicyDigestPrefix opens the text Desk's digest hashes; the two
	// signing domains keep a policy signature apart from an order's.
	deskPolicyDigestPrefix      = "desk-policy-digest-v1"
	deskCompanionPolicyPrefix   = "desk-companion-policy-v1"
	deskPasskeyPolicyPrefix     = "desk-passkey-policy-v1"
	deskPasskeyRelyingParty     = "localhost"
	deskPasskeyNonceBytes       = 32
	cashPolicyEnvelopeMaxBytes  = 64 << 10
	cashPolicyReviewMaxBytes    = 48 << 10
	cashPolicyCredentialMaxByte = 256
)

// errCashPolicyEnvelopeUnsigned marks an envelope that carries no signature
// material (a Desk that predates the verification): nothing to verify.
var errCashPolicyEnvelopeUnsigned = errors.New("the envelope carries no signature")

// cashPolicyDeviceKeys reads the pinned credentials from the constitution in
// force, with Canary's sentence when there are none to show in the snapshot.
func cashPolicyDeviceKeys(c *risk.Constitution) ([]risk.DeskDeviceKey, string) {
	if c == nil {
		return nil, "Canary cannot verify your device yet: there is no risk constitution in force."
	}
	keys, err := c.DeskDevice.Keys()
	switch {
	case err != nil:
		return nil, "Canary cannot verify your device: " + err.Error() + "."
	case len(keys) == 0:
		return nil, "Canary cannot verify your device yet. Add the key Desk shows for it to risk-policy.toml under [desk_device] and raise policy_version; until then a save from Desk cannot change an order cap."
	}
	return keys, ""
}

// cashPolicyEnvelopeSigned reports whether an envelope carries signature
// material Canary can check: a companion signature or a passkey assertion.
func cashPolicyEnvelopeSigned(envelope string) bool {
	fields, err := decodeCashPolicyEnvelope(envelope)
	if err != nil {
		return false
	}
	switch fields["credential"] {
	case "companion":
		return fields["signature"] != "" && fields["key_id"] != "" && fields["challenge"] != "" && fields["review_json"] != ""
	case "passkey":
		return fields["signature"] != "" && fields["credential_id"] != "" && fields["authenticator_data"] != "" && fields["client_data_json"] != "" && fields["review_json"] != ""
	}
	return false
}

func decodeCashPolicyEnvelope(envelope string) (map[string]string, error) {
	if len(envelope) > cashPolicyEnvelopeMaxBytes {
		return nil, errors.New("the envelope is larger than Canary accepts")
	}
	var fields map[string]string
	if err := json.Unmarshal([]byte(envelope), &fields); err != nil || fields == nil {
		return nil, errors.New("the envelope is not a JSON object of strings")
	}
	return fields, nil
}

// verifyCashPolicyDevice checks a confirmation against the pinned keys and
// the request's terms, and names the credential it verified
// (companion:<key id> or passkey:<credential id>). errCashPolicyEnvelopeUnsigned
// says the envelope carries nothing to check; any other error says why the
// confirmation is refused.
func verifyCashPolicyDevice(keys []risk.DeskDeviceKey, c *rpc.CashPolicyConfirmation, terms string) (string, error) {
	if c == nil {
		return "", errors.New("no confirmation")
	}
	fields, err := decodeCashPolicyEnvelope(c.Envelope)
	if err != nil {
		return "", err
	}
	class := fields["credential"]
	if class != "companion" && class != "passkey" {
		return "", errCashPolicyEnvelopeUnsigned
	}
	if !cashPolicyEnvelopeSigned(c.Envelope) {
		return "", errCashPolicyEnvelopeUnsigned
	}
	id := fields["key_id"]
	if class == "passkey" {
		id = fields["credential_id"]
	}
	if id == "" || len(id) > cashPolicyCredentialMaxByte || c.Credential != class+":"+id {
		return "", errors.New("the envelope names a credential other than the one the save says confirmed it")
	}
	i := slices.IndexFunc(keys, func(k risk.DeskDeviceKey) bool { return k.Class == class && k.ID == id })
	if i < 0 {
		return "", fmt.Errorf("the constitution's [desk_device] pins no key for %s:%s", class, id)
	}
	actionID := strings.TrimSpace(c.DeskActionID)
	if actionID == "" || len(actionID) > cashPolicyCredentialMaxByte {
		return "", errors.New("the confirmation names no Desk action")
	}
	review := fields["review_json"]
	if len(review) == 0 || len(review) > cashPolicyReviewMaxBytes {
		return "", errors.New("the envelope carries no review, so the digest the device signed cannot be recomputed")
	}
	if err := cashPolicyReviewListsTerms(review, terms); err != nil {
		return "", err
	}
	digest := deskPolicyDigest(actionID, terms, review)
	switch class {
	case "companion":
		err = verifyDeskCompanionSignature(keys[i].Key, actionID, digest, fields["challenge"], fields["signature"])
	default:
		err = verifyDeskPasskeyAssertion(keys[i].Key, actionID, digest, fields["authenticator_data"], fields["client_data_json"], fields["signature"])
	}
	if err != nil {
		return "", err
	}
	return keys[i].Credential(), nil
}

// deskPolicyDigest is Desk's digest of one save: its prefix, the action id,
// Canary's terms digest, Desk's reference hash (the hex SHA-256 of the
// terms), the terms and the review, joined by line feeds, hashed, in hex.
func deskPolicyDigest(actionID, terms, review string) string {
	ref := sha256.Sum256([]byte(terms))
	sum := sha256.Sum256([]byte(deskPolicyDigestPrefix + "\n" + actionID + "\n" + cashPolicyDigest([]byte(terms)) + "\n" + hex.EncodeToString(ref[:]) + "\n" + terms + "\n" + review))
	return hex.EncodeToString(sum[:])
}

// cashPolicyReviewListsTerms checks that the review the device signed lists
// exactly the terms' changes, each with the value Canary will write, as the
// companion checks before it signs: a review of other changes cannot stand
// for these terms.
func cashPolicyReviewListsTerms(review, terms string) error {
	var t cashPolicyTerms
	if err := json.Unmarshal([]byte(terms), &t); err != nil {
		return errors.New("the terms do not decode")
	}
	var r struct {
		Kind    string `json:"kind"`
		Changes []struct {
			Key string          `json:"key"`
			To  json.RawMessage `json:"to"`
		} `json:"changes"`
	}
	if err := json.Unmarshal([]byte(review), &r); err != nil || r.Kind != "cash_policy" {
		return errors.New("the review the device signed is not a cash settings review")
	}
	if len(r.Changes) != len(t.Changes) {
		return errors.New("the review the device signed does not list these terms' changes")
	}
	seen := map[string]bool{}
	for _, ch := range r.Changes {
		raw, ok := t.Changes[ch.Key]
		if !ok || seen[ch.Key] {
			return errors.New("the review the device signed does not list these terms' changes")
		}
		seen[ch.Key] = true
		var a, b any
		if json.Unmarshal(raw, &a) != nil || json.Unmarshal(cashPolicyNullIfEmpty(ch.To), &b) != nil || !cashPolicyValuesEqual(a, b) {
			return errors.New("the review the device signed shows a value other than the one Canary would write")
		}
	}
	return nil
}

// cashPolicyValuesEqual compares two decoded JSON values: numbers by value,
// everything else structurally. Unlike cashPolicySame it never applies == to
// a value an envelope may shape as an object or a list.
func cashPolicyValuesEqual(a, b any) bool {
	fa, aNum := cashPolicyNumber(a)
	fb, bNum := cashPolicyNumber(b)
	if aNum || bNum {
		return aNum && bNum && fa == fb
	}
	return reflect.DeepEqual(a, b)
}

func cashPolicyNullIfEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// verifyDeskCompanionSignature checks the companion's raw r||s P-256
// signature over SHA-256 of "desk-companion-policy-v1\n<id>\n<digest>\n
// <challenge>". The challenge is Desk's 32 random bytes in base64url; it
// binds the signature to one action and is spent by Desk, so Canary checks
// its shape only.
func verifyDeskCompanionSignature(key *ecdsa.PublicKey, actionID, digest, challenge, signature string) error {
	if nonce, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(challenge, "=")); err != nil || len(nonce) != deskPasskeyNonceBytes {
		return errors.New("the companion's challenge is not 32 bytes in base64url")
	}
	sig, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(signature, "="))
	if err != nil || len(sig) != 64 {
		return errors.New("the companion's signature is not 64 bytes in base64url")
	}
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	order := elliptic.P256().Params().N
	if r.Sign() == 0 || s.Sign() == 0 || r.Cmp(order) >= 0 || s.Cmp(order) >= 0 {
		return errors.New("the companion's signature is not a P-256 signature")
	}
	hash := sha256.Sum256([]byte(deskCompanionPolicyPrefix + "\n" + actionID + "\n" + digest + "\n" + challenge))
	if !ecdsa.Verify(key, hash[:], r, s) {
		return errors.New("the companion's signature does not verify against the key pinned for it")
	}
	return nil
}

// verifyDeskPasskeyAssertion checks a WebAuthn assertion the owner's passkey
// made for Desk: the relying party is localhost, the user was present and
// verified, the client data is a webauthn.get whose challenge is Desk's
// nonce followed by the SHA-256 that binds this action and digest, and the
// DER signature verifies over the authenticator data and the SHA-256 of the
// client data.
func verifyDeskPasskeyAssertion(key *ecdsa.PublicKey, actionID, digest, authData, clientData, signature string) error {
	auth, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(authData, "="))
	if err != nil || len(auth) < 37 {
		return errors.New("the passkey's authenticator data is not base64url of at least 37 bytes")
	}
	rp := sha256.Sum256([]byte(deskPasskeyRelyingParty))
	if !bytes.Equal(auth[:32], rp[:]) {
		return errors.New("the passkey's assertion is not for Desk's relying party")
	}
	if flags := auth[32]; flags&0x01 == 0 || flags&0x04 == 0 {
		return errors.New("the passkey's assertion does not record user presence and verification")
	}
	client, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(clientData, "="))
	if err != nil {
		return errors.New("the passkey's client data is not base64url")
	}
	var cd struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(client, &cd) != nil || cd.Type != "webauthn.get" {
		return errors.New("the passkey's client data is not a webauthn.get")
	}
	challenge, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(cd.Challenge, "="))
	if err != nil || len(challenge) != deskPasskeyNonceBytes+sha256.Size {
		return errors.New("the passkey's challenge is not Desk's nonce and binding")
	}
	binding := sha256.Sum256(append([]byte(deskPasskeyPolicyPrefix+"\n"+actionID+"\n"+digest+"\n"), challenge[:deskPasskeyNonceBytes]...))
	if !bytes.Equal(challenge[deskPasskeyNonceBytes:], binding[:]) {
		return errors.New("the passkey's challenge does not bind this action and these terms")
	}
	sig, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(signature, "="))
	if err != nil {
		return errors.New("the passkey's signature is not base64url")
	}
	clientHash := sha256.Sum256(client)
	signed := sha256.Sum256(append(append([]byte{}, auth...), clientHash[:]...))
	if !ecdsa.VerifyASN1(key, signed[:], sig) {
		return errors.New("the passkey's signature does not verify against the key pinned for it")
	}
	return nil
}
