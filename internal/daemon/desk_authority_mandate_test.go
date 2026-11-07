package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Each breach episode of the mandate's loss room holds a full mandate at
// protection, and it stays held after the episode ends until the owner
// confirms full again on a device (owner decision 2026-10-10 07:51 CEST for
// the brake; the hold reads the mandate since 2026-10-31, build B5).
// Protection keeps running.
func TestDeskAuthorityMandateBreachHoldsFullUntilReconfirmed(t *testing.T) {
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	s.startedAt = cashPolicyTestNow
	state, episode := rpc.MandateReductionWithin, ""
	s.mandateBreachForTest = func() (string, string) { return state, episode }
	head, err := s.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("synthetic-private-controller-", 2)
	hash := sha256.Sum256([]byte(secret))
	terms := rpc.DeskAuthorityTerms{AuthorityEpoch: head.AuthorityEpoch, Version: 1, Kind: "desk_automatic_authority", ID: "synthetic-breach", AccountID: testLiveObserveScope.Account, AccountMode: testLiveObserveScope.Mode, MaximumScope: "full", ControllerHash: hex.EncodeToString(hash[:]), ConfirmBefore: cashPolicyTestNow.Add(time.Minute), Pricing: "fixed-midpoint", Algorithm: "Adaptive", TIF: "DAY", Protection: "existing-policy", Capacity: "existing-policy-reuse-confirmed-funds", Selection: "desk-deterministic-controller"}
	row := deskAuthorityRecord{Terms: &terms, Scope: "full", Running: true, PreferencesHash: strings.Repeat("a", 64), AdaptivePriority: "Normal", ControlProcess: s.startedAt}
	if _, err := s.saveDeskAuthority(t.Context(), 0, row, "test-confirmed"); err != nil {
		t.Fatal(err)
	}

	status := func() rpc.DeskAuthorityStatus {
		t.Helper()
		got, err := s.handleDeskAuthorityStatus(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	trading := rpc.TradingStatus{Account: testLiveObserveScope.Account, Mode: testLiveObserveScope.Mode}
	fence := func(action string) error {
		f := deskAuthorityFence{ID: terms.ID, Generation: 1, PreferencesHash: row.PreferencesHash, AdaptivePriority: "Normal", Action: action, ValidUntil: cashPolicyTestNow.Add(time.Hour)}
		release, err := s.lockDeskAuthorityForWire(t.Context(), &f, trading)
		if release != nil {
			release()
		}
		return err
	}
	if got := status(); got.Scope != "full" || got.HeldBy != "" || fence("entry") != nil {
		t.Fatalf("before any breach: %+v, want full", got)
	}

	// The reduction opens an episode: the second consecutive reduce verdict.
	state, episode = rpc.MandateReductionReducing, "mandate-20261031T140000Z-aaaaaaaaaaaa"
	if got := status(); got.Scope != "protect" || got.HeldBy != rpc.DeskAuthorityHeldByMandate {
		t.Fatalf("breach open: %+v, want protect held by the mandate", got)
	}
	if fence("entry") == nil || fence("protection") != nil || fence("reduction") != nil {
		t.Fatal("under the breach an entry passed the wire or protection did not")
	}
	raw, _ := json.Marshal(rpc.DeskAuthorityControlParams{ID: terms.ID, Capability: secret, ExpectedGeneration: 1, RequestID: "upgrade", Scope: "full", Running: true, PreferencesHash: row.PreferencesHash, AdaptivePriority: "Normal"})
	if _, err := s.handleDeskAuthorityControl(t.Context(), &rpc.Request{Params: raw}); err == nil {
		t.Fatal("the controller restored full scope without a device confirmation")
	}
	rawTerms, _ := json.Marshal(terms)
	confirm, _ := json.Marshal(rpc.DeskAuthorityConfirmParams{Terms: string(rawTerms), Digest: cashPolicyDigest(rawTerms)})
	if _, err := s.handleDeskAuthorityConfirm(t.Context(), &rpc.Request{Params: confirm}); err == nil || !strings.Contains(err.Error(), "within the mandate's loss room") {
		t.Fatalf("confirming full under the breach: %v, want a refusal naming the loss room", err)
	}

	// The episode ends: the loss measure is back within the room. The hold
	// outlasts it, across a daemon restart, until the owner confirms again.
	state, episode = rpc.MandateReductionWithin, ""
	if got := status(); got.Scope != "protect" || got.HeldBy != rpc.DeskAuthorityHeldByMandate || fence("entry") == nil {
		t.Fatalf("after the breach ended: %+v, want protect until confirmed again", got)
	}
	s.mandateEpisode = mandateEpisodeClock{}
	if got := status(); got.Scope != "protect" || got.HeldBy != rpc.DeskAuthorityHeldByMandate {
		t.Fatalf("after a restart: %+v, want the hold kept from the state document", got)
	}

	// A confirmation after the episode ended records it and restores full.
	last, engaged, known := s.deskAuthorityHold(t.Context())
	if last != "mandate-20261031T140000Z-aaaaaaaaaaaa" || engaged || !known {
		t.Fatalf("hold after the episode: last %q engaged %v known %v", last, engaged, known)
	}
	current, err := s.readDeskAuthority(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reconfirmed := deskAuthorityRecord{Terms: &terms, Scope: "full", Running: true, PreferencesHash: row.PreferencesHash, AdaptivePriority: "Normal", ControlProcess: s.startedAt, MandateEpisode: last}
	if _, err := s.saveDeskAuthority(t.Context(), current.Generation, reconfirmed, "test-reconfirmed"); err != nil {
		t.Fatal(err)
	}
	if got := status(); got.Scope != "full" || got.HeldBy != "" {
		t.Fatalf("after reconfirmation: %+v, want full", got)
	}
	// A new episode holds it again; a verdict that cannot be measured holds too.
	state, episode = rpc.MandateReductionReducing, "mandate-20261105T140000Z-bbbbbbbbbbbb"
	if got := status(); got.Scope != "protect" || got.HeldBy != rpc.DeskAuthorityHeldByMandate {
		t.Fatalf("second episode: %+v, want protect held by the mandate", got)
	}
	state, episode = rpc.MandateReductionWithin, ""
	if got := status(); got.Scope != "protect" {
		t.Fatalf("after the second episode: %+v, want protect", got)
	}
	reconfirmed.MandateEpisode = "mandate-20261105T140000Z-bbbbbbbbbbbb"
	current, _ = s.readDeskAuthority(t.Context())
	if _, err := s.saveDeskAuthority(t.Context(), current.Generation, reconfirmed, "test-reconfirmed-2"); err != nil {
		t.Fatal(err)
	}
	if got := status(); got.Scope != "full" {
		t.Fatalf("after the second reconfirmation: %+v, want full", got)
	}
	state = rpc.MandateReductionUnknown
	if got := status(); got.Scope != "protect" || got.HeldBy != rpc.DeskAuthorityHeldByMandate {
		t.Fatalf("verdict unknown: %+v, want protect while it cannot be read", got)
	}
}

// Preparing Fully armed during a breach is refused before the device is asked
// to sign (2026-11-06: the refusal came only after the signature, so the owner
// would confirm a review the daemon was certain to refuse). Protection stays
// available throughout, and a verdict that cannot be read refuses as confirm does.
func TestDeskAuthorityPrepareFullRefusedWhileMandateBreached(t *testing.T) {
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	s.cfg = &config.Resolved{Gateway: config.Gateway{Account: "DU1234567"}, Trading: config.Trading{Mode: "paper"}}
	s.startedAt = cashPolicyTestNow
	state, episode := rpc.MandateReductionReducing, "mandate-20261106T140000Z-aaaaaaaaaaaa"
	s.mandateBreachForTest = func() (string, string) { return state, episode }
	prepare := func(scope string) error {
		t.Helper()
		raw, _ := json.Marshal(rpc.DeskAuthorityPrepareParams{Scope: scope, ControllerHash: strings.Repeat("ab", 32)})
		_, err := s.handleDeskAuthorityPrepare(t.Context(), &rpc.Request{Params: raw})
		return err
	}
	if err := prepare("full"); err == nil || !strings.Contains(err.Error(), "loss room") {
		t.Fatalf("full during a breach: %v, want the loss-room refusal", err)
	}
	if err := prepare("protect"); err != nil {
		t.Fatalf("protection during a breach: %v", err)
	}
	state, episode = rpc.MandateReductionUnknown, ""
	if err := prepare("full"); err == nil || !strings.Contains(err.Error(), "loss room") {
		t.Fatalf("full with no readable verdict: %v, want the loss-room refusal", err)
	}
	state = rpc.MandateReductionWithin
	if err := prepare("full"); err != nil {
		t.Fatalf("full back within the room: %v", err)
	}
}
