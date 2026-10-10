package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Each drawdown brake engagement holds a full mandate at protection, and it
// stays held after the brake clears until the owner confirms full again on a
// device (owner decision 2026-10-10 07:51 CEST). Protection keeps running.
func TestDeskAuthorityBrakeHoldsFullUntilReconfirmed(t *testing.T) {
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	s.startedAt = cashPolicyTestNow
	head, err := s.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("synthetic-private-controller-", 2)
	hash := sha256.Sum256([]byte(secret))
	terms := rpc.DeskAuthorityTerms{AuthorityEpoch: head.AuthorityEpoch, Version: 1, Kind: "desk_automatic_authority", ID: "synthetic-brake", AccountID: testLiveObserveScope.Account, AccountMode: testLiveObserveScope.Mode, MaximumScope: "full", ControllerHash: hex.EncodeToString(hash[:]), ConfirmBefore: cashPolicyTestNow.Add(time.Minute), Pricing: "fixed-midpoint", Algorithm: "Adaptive", TIF: "DAY", Protection: "existing-policy", Capacity: "existing-policy-reuse-confirmed-funds", Selection: "desk-deterministic-controller"}
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
		t.Fatalf("before any brake: %+v, want full", got)
	}

	// The store's fixture engages the brake (its first episode).
	st, c, now := recoveryStore(t)
	s.riskCapital = st
	if got := status(); got.Scope != "protect" || got.HeldBy != "drawdown_brake" {
		t.Fatalf("brake engaged: %+v, want protect held by the brake", got)
	}
	if fence("entry") == nil || fence("protection") != nil || fence("reduction") != nil {
		t.Fatal("under the brake an entry passed the wire or protection did not")
	}
	raw, _ := json.Marshal(rpc.DeskAuthorityControlParams{ID: terms.ID, Capability: secret, ExpectedGeneration: 1, RequestID: "upgrade", Scope: "full", Running: true, PreferencesHash: row.PreferencesHash, AdaptivePriority: "Normal"})
	if _, err := s.handleDeskAuthorityControl(t.Context(), &rpc.Request{Params: raw}); err == nil {
		t.Fatal("the controller restored full scope without a device confirmation")
	}
	rawTerms, _ := json.Marshal(terms)
	confirm, _ := json.Marshal(rpc.DeskAuthorityConfirmParams{Terms: string(rawTerms), Digest: cashPolicyDigest(rawTerms)})
	if _, err := s.handleDeskAuthorityConfirm(t.Context(), &rpc.Request{Params: confirm}); err == nil || !strings.Contains(err.Error(), "after the drawdown brake clears") {
		t.Fatalf("confirming full under the brake: %v, want a refusal naming the brake", err)
	}

	if err := st.IncorporateStatementSnapshotForScope(statementCapitalSnapshot{Scope: testLiveObserveScope, CoverageTo: *now}, c); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	st.Observe(250000, *now, c, testLiveObserveScope, true)
	if rep := st.Report(c, nil, testLiveObserveScope); rep.BlockLatched {
		t.Fatalf("fixture did not release: %+v", rep)
	}
	if got := status(); got.Scope != "protect" || got.HeldBy != "drawdown_brake" || fence("entry") == nil {
		t.Fatalf("after the brake cleared: %+v, want protect until confirmed again", got)
	}

	// A confirmation after the brake cleared records the current engagement
	// and restores full scope.
	episode, _, _ := st.BrakeEpisodeForScope(testLiveObserveScope)
	current, err := s.readDeskAuthority(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reconfirmed := deskAuthorityRecord{Terms: &terms, Scope: "full", Running: true, PreferencesHash: row.PreferencesHash, AdaptivePriority: "Normal", ControlProcess: s.startedAt, BrakeEpisode: episode}
	if _, err := s.saveDeskAuthority(t.Context(), current.Generation, reconfirmed, "test-reconfirmed"); err != nil {
		t.Fatal(err)
	}
	if got := status(); got.Scope != "full" || got.HeldBy != "" {
		t.Fatalf("after reconfirmation: %+v, want full", got)
	}
	// A new engagement holds it again.
	*now = now.Add(time.Minute)
	st.Observe(230000, *now, c, testLiveObserveScope, true)
	if got := status(); got.Scope != "protect" || got.HeldBy != "drawdown_brake" {
		t.Fatalf("second engagement: %+v, want protect held by the brake", got)
	}
}
