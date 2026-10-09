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
	hash := sha256.Sum256([]byte(deskAuthorityDigestPrefix + "\n" + actionID + "\n" + cashPolicyDigest([]byte(terms)) + "\n" + terms + "\n" + review))
	return hex.EncodeToString(hash[:])
}

// Only the device-signed exact mandate can establish persistent authority.
// The domain differs from cash settings, queues and individual orders.
func verifyDeskAuthorityDevice(keys []risk.DeskDeviceKey, c rpc.CashPolicyConfirmation, terms string) (string, error) {
	fields, err := decodeCashPolicyEnvelope(c.Envelope)
	if err != nil {
		return "", err
	}
	if c.ConfirmedBy != "" || !cashPolicyEnvelopeSigned(c.Envelope) {
		return "", errors.New("a fresh device signature is required to arm trading")
	}
	class := fields["credential"]
	id := fields["key_id"]
	if class == "passkey" {
		id = fields["credential_id"]
	}
	if id == "" || len(id) > cashPolicyCredentialMaxByte || c.Credential != class+":"+id {
		return "", errors.New("the device credential does not match the mandate confirmation")
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
	if len(review) > cashPolicyReviewMaxBytes || json.Unmarshal([]byte(review), &shown) != nil || shown.Kind != "automatic_authority" || shown.Terms != terms {
		return "", errors.New("the device review must show the exact automatic trading mandate")
	}
	digest := deskAuthorityDigest(c.DeskActionID, terms, review)
	if class == "companion" {
		err = verifyDeskCompanionSignatureDomain(deskAuthorityCompanionPrefix, keys[at].Key, c.DeskActionID, digest, fields["challenge"], fields["signature"])
	} else {
		err = verifyDeskPasskeyAssertionDomain(deskAuthorityPasskeyPrefix, keys[at].Key, c.DeskActionID, digest, fields["authenticator_data"], fields["client_data_json"], fields["signature"])
	}
	if err != nil {
		return "", err
	}
	return keys[at].Credential(), nil
}
