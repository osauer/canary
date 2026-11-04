package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// armStanding records the device-confirmed standing authority for the rig's
// account at scope, as handleDeskAuthorityConfirm would after a signature.
func (r *automaticTestRig) armStanding(scope string) {
	r.t.Helper()
	ctx := context.Background()
	head, err := r.server.coreStore.AuthorityHead(ctx)
	if err != nil {
		r.t.Fatalf("authority head: %v", err)
	}
	current, err := r.server.readDeskAuthority(ctx)
	if err != nil {
		r.t.Fatalf("read authority: %v", err)
	}
	hash := sha256.Sum256([]byte("synthetic-private-controller"))
	terms := rpc.DeskAuthorityTerms{AuthorityEpoch: head.AuthorityEpoch, Version: 1, Kind: "desk_automatic_authority", ID: "synthetic-standing", AccountID: r.scope.Account, AccountMode: r.scope.Mode, MaximumScope: scope, ControllerHash: hex.EncodeToString(hash[:]), Generation: current.Generation, ConfirmBefore: r.now.Add(10 * time.Minute), Pricing: "fixed-midpoint", Algorithm: "Adaptive", TIF: "DAY", Protection: "existing-policy", Capacity: "existing-policy-reuse-confirmed-funds", Selection: "desk-deterministic-controller"}
	if _, err := r.server.saveDeskAuthority(ctx, current.Generation, deskAuthorityRecord{Terms: &terms, Scope: scope, ConfirmedAt: r.now}, "test-confirmed"); err != nil {
		r.t.Fatalf("arm standing authority: %v", err)
	}
}

// disarmStanding lowers a recorded authority to off, as the owner's disarm
// does through the control. With no record the desk already reads off.
func (r *automaticTestRig) disarmStanding() {
	r.t.Helper()
	ctx := context.Background()
	current, err := r.server.readDeskAuthority(ctx)
	if err != nil {
		r.t.Fatalf("read authority: %v", err)
	}
	if current.Terms == nil {
		return
	}
	current.Scope, current.Running = "off", false
	if _, err := r.server.saveDeskAuthority(ctx, current.Generation, current, "test-disarmed"); err != nil {
		r.t.Fatalf("disarm standing authority: %v", err)
	}
}

// A reduction row's veto clock carried over a blocked spell does not outlive
// an arming: the first arming on 2026-11-04's book found superseded records
// whose clocks had run out, and none of them may send without a fresh window.
func TestMandateClockFromBeforeArmingIsNotInherited(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	row := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	rev := rig.install(row)
	rig.cycle()
	rig.notice(rig.record(row.Key, rev))
	rig.advance(10 * time.Minute)
	rig.install()
	rig.cycle()
	if gone := rig.record(row.Key, rev); gone.State != rpc.TradeProposalAutomaticSuperseded || !gone.CarryClock {
		t.Fatalf("the row that dropped out: %+v, want a carried clock", gone)
	}
	rig.advance(time.Hour)
	rig.armStanding("protect")
	back := rig.mandateProposal(automaticTestStockRow(), 20, 1, 1)
	rev = rig.install(back)
	rig.cycle()
	rec := rig.record(back.Key, rev)
	if rec.State != rpc.TradeProposalAutomaticPending || rec.SubmitAt.Before(rig.now.Add(30*time.Minute)) || !rec.NoticedAt.IsZero() {
		t.Fatalf("the row after arming: %+v, want a full window from %v and a new notice", rec, rig.now)
	}
}

// With the file listing buckets and nothing armed, the status says so instead
// of "every submission is a human instruction".
func TestProposalStatusNamesTheBucketsAwaitingArming(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	rig.disarmStanding()
	got := rig.server.autoTradeStatus()
	if len(got.PreAuthorised) != 0 || len(got.PreAuthorisedDisarmed) == 0 {
		t.Fatalf("status while disarmed: active %v, listed %v", got.PreAuthorised, got.PreAuthorisedDisarmed)
	}
}
