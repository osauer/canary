package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	deskAuthorityDigestPrefix    = "desk-authority-digest-v1"
	deskAuthorityCompanionPrefix = "desk-companion-authority-v1"
	deskAuthorityPasskeyPrefix   = "desk-passkey-authority-v1"
)

func deskAuthorityDigest(actionID, terms, review string) string {
	return deskSignedDigest(deskAuthorityDigestPrefix, actionID, terms, review)
}

// deskSignedDigest is Desk's digest of signed terms: the prefix, the action
// id, the terms' digest, the terms and the review, joined by line feeds,
// hashed, in hex.
func deskSignedDigest(prefix, actionID, terms, review string) string {
	hash := sha256.Sum256([]byte(prefix + "\n" + actionID + "\n" + cashPolicyDigest([]byte(terms)) + "\n" + terms + "\n" + review))
	return hex.EncodeToString(hash[:])
}

// deskSigningDomain is one kind of terms the owner's device signs from Desk:
// the review kind the device is shown, the digest prefix and the companion
// and passkey signing domains. Each kind has its own, so a signature for
// one never stands for another, nor for a cash settings save or an order.
type deskSigningDomain struct {
	what, review, digest, companion, passkey string
}

var deskAuthorityDomain = deskSigningDomain{what: "automatic trading mandate", review: "automatic_authority",
	digest: deskAuthorityDigestPrefix, companion: deskAuthorityCompanionPrefix, passkey: deskAuthorityPasskeyPrefix}

// Only the device-signed exact mandate can establish persistent authority.
// The domain differs from cash settings, queues and individual orders.
func verifyDeskAuthorityDevice(keys []risk.DeskDeviceKey, c rpc.CashPolicyConfirmation, terms string) (string, error) {
	return verifyDeskSignedTerms(deskAuthorityDomain, keys, c, terms)
}

// verifyDeskSignedTerms checks a fresh device signature over Desk's review
// of exact terms in one signing domain against the pinned keys, and names
// the credential it verified.
func verifyDeskSignedTerms(domain deskSigningDomain, keys []risk.DeskDeviceKey, c rpc.CashPolicyConfirmation, terms string) (string, error) {
	fields, err := decodeCashPolicyEnvelope(c.Envelope)
	if err != nil {
		return "", err
	}
	if c.ConfirmedBy != "" || !cashPolicyEnvelopeSigned(c.Envelope) {
		return "", errors.New("a fresh device signature is required for the " + domain.what)
	}
	class := fields["credential"]
	id := fields["key_id"]
	if class == "passkey" {
		id = fields["credential_id"]
	}
	if id == "" || len(id) > cashPolicyCredentialMaxByte || c.Credential != class+":"+id {
		return "", errors.New("the device credential does not match the " + domain.what + " confirmation")
	}
	at := slices.IndexFunc(keys, func(k risk.DeskDeviceKey) bool { return k.Class == class && k.ID == id })
	if at < 0 {
		return "", errors.New("no pinned Canary key matches this confirmation device")
	}
	if c.DeskActionID == "" || strings.TrimSpace(c.DeskActionID) != c.DeskActionID || len(c.DeskActionID) > cashPolicyCredentialMaxByte {
		return "", errors.New("invalid confirmation action")
	}
	review := fields["review_json"]
	var shown struct {
		Kind  string `json:"kind"`
		Terms string `json:"terms"`
	}
	if len(review) > cashPolicyReviewMaxBytes || json.Unmarshal([]byte(review), &shown) != nil || shown.Kind != domain.review || shown.Terms != terms {
		return "", errors.New("the device review must show the exact " + domain.what)
	}
	digest := deskSignedDigest(domain.digest, c.DeskActionID, terms, review)
	if class == "companion" {
		err = verifyDeskCompanionSignatureDomain(domain.companion, keys[at].Key, c.DeskActionID, digest, fields["challenge"], fields["signature"])
	} else {
		err = verifyDeskPasskeyAssertionDomain(domain.passkey, keys[at].Key, c.DeskActionID, digest, fields["authenticator_data"], fields["client_data_json"], fields["signature"])
	}
	if err != nil {
		return "", err
	}
	return keys[at].Credential(), nil
}
