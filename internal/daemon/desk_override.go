package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The owner's one-click mandate override from Desk's console (owner,
// 2026-10-29 15:31 CET: "one-click: yes, ok"; design
// internal-docs/design/mandate-risk-budget.md). policy.override stays
// human-origin only, and no origin a caller claims reaches this path: the
// proof is the owner's device signature, which Canary verifies itself
// against the key pinned in [desk_device], as it does for an arming and an
// order cap change. Only mandate.vol_target_pct takes it. What still binds
// while it runs is what binds a terminal override: the drawdown brake and
// the no-room hold at the maximum loss, the owner sleeve's stop, every order
// cap, the device confirmation of every order and the override's expiry.

const (
	deskOverrideDigestPrefix    = "desk-override-digest-v1"
	deskOverrideCompanionPrefix = "desk-companion-override-v1"
	deskOverridePasskeyPrefix   = "desk-passkey-override-v1"
	// deskOverrideConfirmWindow is how long prepared terms wait for the
	// owner's device, as an arming's do.
	deskOverrideConfirmWindow = 10 * time.Minute
)

var deskOverrideDomain = deskSigningDomain{what: "mandate override", review: rpc.DeskOverrideReviewKind,
	digest: deskOverrideDigestPrefix, companion: deskOverrideCompanionPrefix, passkey: deskOverridePasskeyPrefix}

func deskOverrideDigest(actionID, terms, review string) string {
	return deskSignedDigest(deskOverrideDigestPrefix, actionID, terms, review)
}

// verifyDeskOverrideDevice checks the owner's fresh device signature over
// Desk's review of exact override terms.
func verifyDeskOverrideDevice(keys []risk.DeskDeviceKey, c rpc.CashPolicyConfirmation, terms string) (string, error) {
	return verifyDeskSignedTerms(deskOverrideDomain, keys, c, terms)
}

// errDeskOverrideControl refuses any control but the volatility target.
var errDeskOverrideControl = errBadRequest("Desk's console grants an override of " + risk.MandateAdditionsOverrideControl + " only; grant any other override from a terminal")

func (s *Server) handleDeskOverridePrepare(_ context.Context, req *rpc.Request) (rpc.DeskOverridePrepared, error) {
	var in rpc.DeskOverridePrepareParams
	if err := decodeDeskAuthority(req.Params, &in); err != nil {
		return rpc.DeskOverridePrepared{}, err
	}
	if strings.TrimSpace(in.Control) != risk.MandateAdditionsOverrideControl {
		return rpc.DeskOverridePrepared{}, errDeskOverrideControl
	}
	if s.riskPolicies == nil || s.riskCapital == nil {
		return rpc.DeskOverridePrepared{}, errBadRequest("the risk policy is unavailable, so no override can be granted")
	}
	control, reason, err := validateOverride(rpc.OverrideParams{Control: in.Control, Reason: in.Reason, Hours: in.Hours}, s.riskPolicies.snapshot().policy)
	if err != nil {
		return rpc.DeskOverridePrepared{}, errBadRequest(err.Error())
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return rpc.DeskOverridePrepared{}, errBadRequest("an override needs one selected broker account")
	}
	id, err := randomTokenID()
	if err != nil {
		return rpc.DeskOverridePrepared{}, err
	}
	terms := rpc.DeskOverrideTerms{Version: 1, Kind: rpc.DeskOverrideTermsKind, ID: id, AccountID: scope.Account, AccountMode: scope.Mode,
		Control: control, Reason: reason, Hours: in.Hours, ConfirmBefore: s.nowUTC().Add(deskOverrideConfirmWindow)}
	raw, err := json.Marshal(terms)
	if err != nil {
		return rpc.DeskOverridePrepared{}, err
	}
	return rpc.DeskOverridePrepared{Terms: string(raw), Digest: cashPolicyDigest(raw)}, nil
}

func (s *Server) handleDeskOverrideConfirm(_ context.Context, req *rpc.Request) (*rpc.RiskPolicyWriteResult, error) {
	var in rpc.DeskOverrideConfirmParams
	if err := decodeDeskAuthority(req.Params, &in); err != nil {
		return nil, err
	}
	var terms rpc.DeskOverrideTerms
	if decodeDeskAuthority([]byte(in.Terms), &terms) != nil || terms.Version != 1 || terms.Kind != rpc.DeskOverrideTermsKind || terms.ID == "" ||
		in.Digest != cashPolicyDigest([]byte(in.Terms)) {
		return nil, errBadRequest("exact override terms are required")
	}
	if terms.Control != risk.MandateAdditionsOverrideControl {
		return nil, errDeskOverrideControl
	}
	if s.riskPolicies == nil || s.riskCapital == nil {
		return nil, errBadRequest("the risk policy is unavailable, so no override can be granted")
	}
	c := s.riskPolicies.snapshot().policy
	keys, why := cashPolicyDeviceKeys(c)
	if why != "" {
		return nil, &rpc.Error{Code: rpc.CodeConfirmationUnverifiable, Message: why + " No override was granted."}
	}
	verified, err := verifyDeskOverrideDevice(keys, in.Confirmation, in.Terms)
	if err != nil {
		return nil, &rpc.Error{Code: rpc.CodeConfirmationUnverifiable, Message: "Canary could not verify your device's confirmation: " + err.Error() + ". No override was granted."}
	}
	scope := s.currentBrokerStateScope()
	if !sameBrokerScope(scope, brokerStateScope{Account: terms.AccountID, Mode: terms.AccountMode}) {
		return nil, errBadRequest("the selected broker account differs from the one the override names; review it again")
	}
	p := rpc.OverrideParams{Control: terms.Control, Reason: terms.Reason, Hours: terms.Hours}
	g := consoleOverrideGrant{termsID: terms.ID, deskActionID: in.Confirmation.DeskActionID, verified: verified}
	if now := s.nowUTC(); !now.Before(terms.ConfirmBefore) || terms.ConfirmBefore.After(now.Add(deskOverrideConfirmWindow)) {
		// A retry of terms that already granted answers with that grant.
		if rec, ok := s.riskCapital.consoleOverrideByTerms(scope, g.termsID); ok {
			return deskOverrideResult(rec, true), nil
		}
		return nil, errBadRequest("the override's confirmation window passed; review it again")
	}
	rec, replay, err := s.riskCapital.GrantConsoleOverrideForScope(p, c, scope, g)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	return deskOverrideResult(rec, replay), nil
}

func deskOverrideResult(rec rpc.OverrideRecord, replay bool) *rpc.RiskPolicyWriteResult {
	message := "override granted from the console on your device and journaled; it expires on its own"
	if replay {
		message = "this confirmation already granted the override; nothing new was granted"
	}
	return &rpc.RiskPolicyWriteResult{OK: true, At: rec.GrantedAt, Override: &rec, Message: message}
}
