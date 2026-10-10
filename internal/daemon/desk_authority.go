package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const deskAuthorityStateKind = "desk_authority_v1"

type deskAuthorityRecord struct {
	rpc.DeskAuthorityStatus
	LastRequestID   string    `json:"last_request_id,omitempty"`
	LastRequestHash string    `json:"last_request_hash,omitempty"`
	ControlProcess  time.Time `json:"control_process,omitzero"`
	// BrakeEpisode is the drawdown brake's engagement count when full scope
	// was confirmed; a later engagement holds full at protection.
	BrakeEpisode uint64                     `json:"brake_episode,omitempty"`
	Confirmation rpc.CashPolicyConfirmation `json:"confirmation"`
}

func decodeDeskAuthority(raw []byte, out any) error {
	if len(raw) > 128<<10 {
		return errBadRequest("automatic authority request is too large")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errBadRequest("invalid automatic authority request")
	}
	if d.Decode(new(any)) != io.EOF {
		return errBadRequest("one automatic authority object is required")
	}
	return nil
}

func (s *Server) readDeskAuthority(ctx context.Context) (deskAuthorityRecord, error) {
	out := deskAuthorityRecord{Scope: "off"}
	if s.coreStore == nil {
		return out, errors.New("automatic authority storage is unavailable")
	}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, deskAuthorityStateKind)
	if err != nil || !ok {
		return out, err
	}
	if json.Unmarshal(doc.JSON, &out) != nil || out.Generation != doc.Revision || out.Terms == nil || !validDeskAuthorityTerms(*out.Terms) || !slices.Contains([]string{"off", "protect", "full"}, out.Scope) || (out.Scope == "full" && out.Terms.MaximumScope != "full") {
		return deskAuthorityRecord{}, errors.New("automatic authority storage cannot be validated")
	}
	head, err := s.coreStore.AuthorityHead(ctx)
	if err != nil {
		return deskAuthorityRecord{}, err
	}
	if out.Terms.AuthorityEpoch != head.AuthorityEpoch {
		return deskAuthorityRecord{}, errors.New("automatic authority belongs to a different authority store; arm again")
	}
	// A saved AUTO bit cannot resume the controller in a new daemon process.
	if s.startedAt.IsZero() || !out.ControlProcess.Equal(s.startedAt) {
		out.Running = false
	}
	// Each brake engagement holds full scope at protection until the owner
	// confirms full again on a device after the brake clears (owner decision
	// 2026-10-10 07:51 CEST). Protection and reductions keep running.
	if out.Scope == "full" {
		if episode, engaged, known := s.deskAuthorityBrake(out.Terms); !known || engaged || episode != out.BrakeEpisode {
			out.Scope, out.HeldBy = "protect", "drawdown_brake"
		}
	}
	return out, nil
}

// deskAuthorityBrake reads the drawdown brake for the mandate's account.
// known is false when the brake cannot be read; the caller then holds.
func (s *Server) deskAuthorityBrake(terms *rpc.DeskAuthorityTerms) (episode uint64, engaged, known bool) {
	if s.riskCapital == nil {
		return 0, false, true
	}
	if terms == nil {
		return 0, false, false
	}
	episode, engaged, err := s.riskCapital.BrakeEpisodeForScope(brokerStateScope{Account: terms.AccountID, Mode: terms.AccountMode})
	return episode, engaged, err == nil
}

func validDeskAuthorityTerms(t rpc.DeskAuthorityTerms) bool {
	hash, err := hex.DecodeString(t.ControllerHash)
	return t.AuthorityEpoch != "" && err == nil && len(hash) == sha256.Size && t.ID != "" && len(t.ID) <= 128 && t.AccountID != "" && len(t.AccountID) <= 128 && (t.AccountMode == "live" || t.AccountMode == "paper") && t.Version == 1 && t.Kind == "desk_automatic_authority" && (t.MaximumScope == "protect" || t.MaximumScope == "full") && t.Generation >= 0 && !t.ConfirmBefore.IsZero() && t.Pricing == "fixed-midpoint" && t.Algorithm == "Adaptive" && t.TIF == "DAY" && t.Protection == "existing-policy" && t.Capacity == "existing-policy-reuse-confirmed-funds" && t.Selection == "desk-deterministic-controller"
}

func (s *Server) handleDeskAuthorityPrepare(ctx context.Context, req *rpc.Request) (rpc.DeskAuthorityPrepared, error) {
	var in rpc.DeskAuthorityPrepareParams
	if err := decodeDeskAuthority(req.Params, &in); err != nil {
		return rpc.DeskAuthorityPrepared{}, err
	}
	s.deskAuthorityMu.Lock()
	defer s.deskAuthorityMu.Unlock()
	current, err := s.readDeskAuthority(ctx)
	if err != nil {
		return rpc.DeskAuthorityPrepared{}, err
	}
	status := s.currentTradingStatus()
	id, err := randomTokenID()
	if err != nil {
		return rpc.DeskAuthorityPrepared{}, err
	}
	head, err := s.coreStore.AuthorityHead(ctx)
	if err != nil {
		return rpc.DeskAuthorityPrepared{}, err
	}
	terms := rpc.DeskAuthorityTerms{AuthorityEpoch: head.AuthorityEpoch, Version: 1, Kind: "desk_automatic_authority", ID: id, AccountID: status.Account, AccountMode: status.Mode, MaximumScope: in.Scope, ControllerHash: in.ControllerHash, Generation: current.Generation, ConfirmBefore: s.orderNow().Add(queuedArmWindow), Pricing: "fixed-midpoint", Algorithm: "Adaptive", TIF: "DAY", Protection: "existing-policy", Capacity: "existing-policy-reuse-confirmed-funds", Selection: "desk-deterministic-controller"}
	if !validDeskAuthorityTerms(terms) {
		return rpc.DeskAuthorityPrepared{}, errBadRequest("a current account, armed scope and private controller identity are required")
	}
	raw, err := json.Marshal(terms)
	if err != nil {
		return rpc.DeskAuthorityPrepared{}, err
	}
	return rpc.DeskAuthorityPrepared{Terms: string(raw), Digest: cashPolicyDigest(raw)}, nil
}

func (s *Server) saveDeskAuthority(ctx context.Context, old int64, next deskAuthorityRecord, action string) (rpc.DeskAuthorityStatus, error) {
	next.Generation = old + 1
	next.HeldBy = "" // derived at read, never stored
	raw, err := json.Marshal(next)
	if err != nil {
		return rpc.DeskAuthorityStatus{}, err
	}
	eventKey := fmt.Sprintf("desk-authority:%s:%d", next.Terms.ID, next.Generation)
	_, _, err = s.coreStore.CompareAndSwapStateDocumentWithEvents(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: deskAuthorityStateKind, ExpectedRevision: old, JSON: raw}, []corestore.EventInput{{ScopeKey: daemonStateScope, EventKey: eventKey, Type: "desk_authority", Action: coreEventActionRecord, Origin: coreEventOriginDaemon, OccurredAt: s.orderNow(), PayloadJSON: json.RawMessage(fmt.Sprintf(`{"action":%q,"generation":%d,"scope":%q,"running":%t}`, action, next.Generation, next.Scope, next.Running))}})
	return next.DeskAuthorityStatus, err
}

func (s *Server) handleDeskAuthorityConfirm(ctx context.Context, req *rpc.Request) (rpc.DeskAuthorityStatus, error) {
	var in rpc.DeskAuthorityConfirmParams
	if err := decodeDeskAuthority(req.Params, &in); err != nil {
		return rpc.DeskAuthorityStatus{}, err
	}
	var terms rpc.DeskAuthorityTerms
	if decodeDeskAuthority([]byte(in.Terms), &terms) != nil || !validDeskAuthorityTerms(terms) || in.Digest != cashPolicyDigest([]byte(in.Terms)) {
		return rpc.DeskAuthorityStatus{}, errBadRequest("exact automatic authority terms are required")
	}
	episode, engaged, known := s.deskAuthorityBrake(&terms)
	if terms.MaximumScope == "full" && (engaged || !known) {
		return rpc.DeskAuthorityStatus{}, errBadRequest("full automatic trading can be confirmed after the drawdown brake clears; protection can be armed now")
	}
	s.deskAuthorityMu.Lock()
	defer s.deskAuthorityMu.Unlock()
	current, err := s.readDeskAuthority(ctx)
	if err != nil {
		return rpc.DeskAuthorityStatus{}, err
	}
	if current.Terms != nil && current.Terms.ID == terms.ID && current.Confirmation == in.Confirmation {
		raw, _ := json.Marshal(current.Terms)
		if string(raw) == in.Terms {
			return current.DeskAuthorityStatus, nil
		}
	}
	head, err := s.coreStore.AuthorityHead(ctx)
	if err != nil {
		return rpc.DeskAuthorityStatus{}, err
	}
	if terms.AuthorityEpoch != head.AuthorityEpoch {
		return rpc.DeskAuthorityStatus{}, errBadRequest("automatic authority store changed; review again")
	}
	if terms.Generation != current.Generation || !s.orderNow().Before(terms.ConfirmBefore) || terms.ConfirmBefore.After(s.orderNow().Add(queuedArmWindow)) {
		return rpc.DeskAuthorityStatus{}, errBadRequest("authority changed or confirmation expired; review again")
	}
	status := s.currentTradingStatus()
	if status.Account != terms.AccountID || status.Mode != terms.AccountMode {
		return rpc.DeskAuthorityStatus{}, errBadRequest("the current account differs from the confirmed mandate")
	}
	if s.riskPolicies == nil {
		return rpc.DeskAuthorityStatus{}, errors.New("device verification policy is unavailable")
	}
	keys, why := cashPolicyDeviceKeys(s.riskPolicies.snapshot().policy)
	if why != "" {
		return rpc.DeskAuthorityStatus{}, errors.New(why)
	}
	verified, err := verifyDeskAuthorityDevice(keys, in.Confirmation, in.Terms)
	if err != nil {
		return rpc.DeskAuthorityStatus{}, err
	}
	next := deskAuthorityRecord{Terms: &terms, Scope: terms.MaximumScope, ConfirmedAt: s.orderNow(), Verified: verified, Confirmation: in.Confirmation, BrakeEpisode: episode}
	// Confirmation does not resume execution. The reconciled controller must
	// explicitly fence its current preferences before any subsequent dispatch.
	return s.saveDeskAuthority(ctx, current.Generation, next, "confirmed")
}

func (s *Server) handleDeskAuthorityStatus(ctx context.Context) (rpc.DeskAuthorityStatus, error) {
	s.deskAuthorityMu.Lock()
	defer s.deskAuthorityMu.Unlock()
	row, err := s.readDeskAuthority(ctx)
	return row.DeskAuthorityStatus, err
}

func (s *Server) handleDeskAuthorityControl(ctx context.Context, req *rpc.Request) (rpc.DeskAuthorityStatus, error) {
	var in rpc.DeskAuthorityControlParams
	if err := decodeDeskAuthority(req.Params, &in); err != nil {
		return rpc.DeskAuthorityStatus{}, err
	}
	s.deskAuthorityMu.Lock()
	defer s.deskAuthorityMu.Unlock()
	current, err := s.readDeskAuthority(ctx)
	if err != nil {
		return rpc.DeskAuthorityStatus{}, err
	}
	if current.Terms == nil || current.Terms.ID != in.ID || len(in.Capability) < 32 || len(in.Capability) > 256 {
		return rpc.DeskAuthorityStatus{}, errBadRequest("private controller authority is required")
	}
	hash := sha256.Sum256([]byte(in.Capability))
	expected, _ := hex.DecodeString(current.Terms.ControllerHash)
	if subtle.ConstantTimeCompare(hash[:], expected) != 1 {
		return rpc.DeskAuthorityStatus{}, errBadRequest("private controller authority does not match")
	}
	intent := in
	intent.Capability = ""
	raw, _ := json.Marshal(intent)
	requestHash := cashPolicyDigest(raw)
	if in.RequestID == current.LastRequestID {
		if requestHash != current.LastRequestHash {
			return rpc.DeskAuthorityStatus{}, errBadRequest("replayed control intent differs")
		}
		return current.DeskAuthorityStatus, nil
	}
	if in.RequestID == "" || len(in.RequestID) > 128 || in.ExpectedGeneration != current.Generation {
		return rpc.DeskAuthorityStatus{}, errBadRequest("automatic authority generation changed")
	}
	if !slices.Contains([]string{"off", "protect", "full"}, in.Scope) || (in.Scope == "full" && current.Scope != "full") || (current.Scope == "off" && in.Scope != "off") {
		return rpc.DeskAuthorityStatus{}, errBadRequest("arming or upgrading needs a fresh device confirmation")
	}
	preferences, hashErr := hex.DecodeString(in.PreferencesHash)
	if in.Running && (in.Scope == "off" || !slices.Contains([]string{"Patient", "Normal", "Urgent"}, in.AdaptivePriority) || hashErr != nil || len(preferences) != sha256.Size) {
		return rpc.DeskAuthorityStatus{}, errBadRequest("valid execution preferences and scope are required")
	}
	current.Scope, current.Running, current.PreferencesHash, current.AdaptivePriority = in.Scope, in.Running, in.PreferencesHash, in.AdaptivePriority
	current.LastRequestID, current.LastRequestHash, current.ControlProcess = in.RequestID, requestHash, s.startedAt
	return s.saveDeskAuthority(ctx, current.Generation, current, "controlled")
}
