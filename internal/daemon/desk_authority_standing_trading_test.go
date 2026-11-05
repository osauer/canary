//go:build trading

package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The owner's rule (2026-11-04 20:17 CET): Canary does not sell by itself
// unless it was armed. The policy list says which buckets may go; only the
// device-confirmed standing authority says Canary may act at all.
func TestAutomaticDisarmedPreAuthorisedRowNeverReachesTheBroker(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	rig.disarmStanding()
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.noRecord(prop.Key, revision)
	rig.advance(2 * time.Hour)
	rig.cycle()
	rig.noRecord(prop.Key, revision)
	if broker.count() != 0 {
		t.Fatalf("broker calls while disarmed = %d, want 0", broker.count())
	}
	// The row stays the owner's to confirm: it says it is not pre-authorised.
	if a := rig.engine.Snapshot(false).Proposals[0].Automatic; a != nil && a.PreAuthorised {
		t.Fatalf("served automatic while disarmed = %+v, want not pre-authorised", a)
	}
}

// Arming starts a full veto window even for a row whose clock had run out
// before the arming: nothing goes out in the cycle after the signature.
func TestAutomaticArmingStartsAFullWindow(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(29 * time.Minute)
	rig.disarmStanding()
	rig.cycle()
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticSuperseded || rec.CarryClock {
		t.Fatalf("record after disarm = %+v, want superseded with no clock to carry", rec)
	}
	rig.advance(time.Hour)
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("broker calls while disarmed = %d, want 0", broker.count())
	}
	rig.armStanding("protect")
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	rig.notice(rec)
	rec = rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticPending || rec.SubmitAt.Before(rig.now.Add(30*time.Minute)) {
		t.Fatalf("record after arming = %+v, want a full window from now %v", rec, rig.now)
	}
	rig.advance(29 * time.Minute)
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("broker calls inside the new window = %d, want 0", broker.count())
	}
	rig.advance(2 * time.Minute)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after the new window = %d, want 1; record = %+v", broker.count(), rig.record(prop.Key, revision))
	}
}

// A disarm and a new arming between two scheduler passes leave the record
// waiting with its old clock, and the new arming still starts a full window
// (independent review, 2026-11-05: one order went out at once on the rearm).
func TestAutomaticRearmBetweenCyclesStartsAFullWindow(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(31 * time.Minute)
	rig.disarmStanding()
	rig.advance(time.Second)
	rig.armStanding("protect")
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("broker calls right after the rearm = %d, want 0; record = %+v", broker.count(), rig.record(prop.Key, revision))
	}
	rig.notice(rig.record(prop.Key, revision))
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticPending || rec.SubmitAt.Before(rig.now.Add(30*time.Minute)) {
		t.Fatalf("record after the rearm = %+v, want a full window from %v", rec, rig.now)
	}
	rig.advance(31 * time.Minute)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after the new window = %d, want 1", broker.count())
	}
}

// Standing authority survives a daemon restart and needs no running
// controller; a breach holding Fully armed at Protection still allows it.
func TestAutomaticStandingAuthorityStandsWithoutAController(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	rig.armStanding("full")
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.restart()
	rig.install(prop)
	if a := rig.engine.Snapshot(false).Proposals[0].Automatic; a == nil || !a.PreAuthorised {
		t.Fatalf("served automatic after restart = %+v, want pre-authorised", a)
	}
	rig.advance(31 * time.Minute)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after restart = %d, want 1; record = %+v", broker.count(), rig.record(prop.Key, revision))
	}
}

// A disarm landing after the cycle's read is still refused at the origin
// gate and at the wire: the final check before the first byte reads it.
func TestAutomaticDisarmReachesTheWireGate(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	status := rig.server.currentTradingStatus()
	rig.armStanding("protect")
	rig.server.automaticGrant.Store(&automaticWriteGrant{Key: "k", Revision: "r", Bucket: preAuthorisedBucketTrailingStop, ArmedAt: rig.server.standingAuthorityConfirmedAt(t.Context())})
	defer rig.server.automaticGrant.Store(nil)
	if got := rig.server.standingAuthorityBlocker(t.Context(), rig.scope.Account, rig.scope.Mode); got != nil {
		t.Fatalf("armed: %+v", got)
	}
	unlock, err := rig.server.lockStandingAuthorityForWire(t.Context(), rpc.TradingStatus{Account: rig.scope.Account, Mode: rig.scope.Mode})
	if err != nil {
		t.Fatalf("wire lock while armed: %v", err)
	}
	unlock()
	rig.disarmStanding()
	blockers := rig.server.daemonPreAuthorisedOriginBlockers(status)
	if !automaticDisarmedRefusal(blockers) {
		t.Fatalf("origin blockers while disarmed = %+v", blockers)
	}
	if _, err := rig.server.lockStandingAuthorityForWire(t.Context(), rpc.TradingStatus{Account: rig.scope.Account, Mode: rig.scope.Mode}); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("wire lock while disarmed: %v", err)
	}
	rig.armStanding("protect")
	if got := rig.server.standingAuthorityBlocker(t.Context(), "another-account", rig.scope.Mode); got == nil {
		t.Fatal("authority for another account passed")
	}
	// A rearm landing after the cycle's read: the grant names the old arming.
	rig.advance(time.Second)
	rig.disarmStanding()
	rig.armStanding("protect")
	if blockers := rig.server.daemonPreAuthorisedOriginBlockers(status); !automaticDisarmedRefusal(blockers) {
		t.Fatalf("origin blockers for a grant of the old arming = %+v", blockers)
	}
	if _, err := rig.server.lockStandingAuthorityForWire(t.Context(), rpc.TradingStatus{Account: rig.scope.Account, Mode: rig.scope.Mode}); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("wire lock for a grant of the old arming: %v", err)
	}
}
