package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
)

// fakeHealthSession is a broker session for the data-health recorder: one
// binding, current or lost, with the backend link up or down.
type fakeHealthSession struct {
	binding broker.SessionBinding
	current bool
	down    bool
}

func (f *fakeHealthSession) Capture() (broker.SessionBinding, bool) { return f.binding, f.current }
func (f *fakeHealthSession) Current(b broker.SessionBinding) bool   { return f.current && b == f.binding }
func (f *fakeHealthSession) Health() broker.SessionHealth {
	return broker.SessionHealth{Kind: broker.KindIBKR, Connected: true, Ready: f.current, BackendDown: f.down}
}

// The account and positions observations carry the producer's own clock and
// belong to the session the read ran in: a binding the session does not
// hold records nothing, and once the session is lost or its backend link
// drops, the row reads as a previous-session observation.
func TestAccountObservationFollowsTheSeamSession(t *testing.T) {
	now := time.Date(2026, 11, 4, 16, 0, 0, 0, time.UTC)
	asOf := now.Add(-10 * time.Second)
	s := &Server{now: func() time.Time { return now }}
	session := &fakeHealthSession{binding: fakeSession, current: true}
	authority := func() *rpc.AccountDataAuthority {
		return &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent, AsOf: asOf}
	}
	s.observeAccountRPC(&rpc.AccountResult{Authority: authority()}, session, fakeSession)
	row, ok := s.observedDataHealth("account", now)
	if !ok || row.State != "current" || !row.ReceivedAt.Equal(asOf) || !row.CheckedAt.Equal(now) || !row.ValidUntil.Equal(now.Add(time.Minute)) || row.Provider != "IBKR" {
		t.Fatalf("account observation: %v %+v", ok, row)
	}

	other := broker.SessionBinding{Kind: broker.KindIBKR, Generation: fakeSession.Generation, Epoch: fakeSession.Epoch + 1}
	s.observeAccountRPC(&rpc.PositionsResult{Authority: authority()}, session, other)
	if _, ok := s.observedDataHealth("positions", now); ok {
		t.Fatal("observation recorded against a binding the session does not hold")
	}
	s.observeAccountRPC(&rpc.PositionsResult{Authority: authority()}, nil, fakeSession)
	if _, ok := s.observedDataHealth("positions", now); ok {
		t.Fatal("observation recorded without a session")
	}

	session.current = false
	row, _ = s.observedDataHealth("account", now)
	if row.State != "unknown" || row.Receiving != "Previous session observation" || len(row.ProblemIDs) != 0 {
		t.Fatalf("lost session still reads as current: %+v", row)
	}
	session.current, session.down = true, true
	row, _ = s.observedDataHealth("account", now)
	if row.State != "unknown" || row.Receiving != "Previous session observation" {
		t.Fatalf("backend down still reads as current: %+v", row)
	}
	session.down = false
	row, _ = s.observedDataHealth("account", now)
	if row.State != "current" {
		t.Fatalf("recovered session lost its observation: %+v", row)
	}
}
