package daemon

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// sendOrderStatus writes one text orderStatus frame (server version 176)
// for an order no client placed to every live session.
func (f *fakeTWS) sendOrderStatus(orderID int) {
	f.mu.Lock()
	sessions := slices.Clone(f.sessions)
	f.mu.Unlock()
	body := fmt.Appendf(binary.BigEndian.AppendUint32(nil, 3), "%d\x00Submitted\x000\x001\x000\x00%d\x000\x000\x0015\x00\x000\x00", orderID, 7000+orderID)
	for _, c := range sessions {
		_ = writeTWSFrame(c, body)
	}
}

// readerFrameBound is how long an order frame may take to reach the
// Connector's order-callback frontier while an alert commit is in progress.
// A frame alone takes a few milliseconds; the bound leaves room for load.
const readerFrameBound = 300 * time.Millisecond

// An alert commit judged on broker evidence waits for the alert producer
// (another observation holds it) or is slow itself (a one-second commit that
// does not hold the store's writer: a frame's own journal append still shares
// that writer). Neither may hold the broker reader: an order frame that arrives meanwhile reaches the order
// frontier within a small bound, and the account-summary path (the
// publication write side, WithBrokerEvidenceMutation) is not held either.
// The commit still lands, since its evidence was current when it was checked.
// On 2026-11-24 a commit held the reader this way for 19.5 s.
func TestSlowAlertCommitDoesNotHoldTheBrokerReader(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order bool
	}{{"broker evidence", false}, {"broker and order evidence", true}} {
		for _, wait := range []string{"producer held", "slow commit"} {
			t.Run(tc.name+", "+wait, func(t *testing.T) {
				f := startFakeTWS(t)
				s, _ := sessionLossServerWith(t, f, "2026-10-07T18:00:00Z", func(s *Server) {
					s.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "orders"))
				})
				c := s.gatewayConnectorForTest()
				evidence, ok := c.CaptureBrokerEvidence()
				if !ok {
					t.Fatal("ready connector gave no evidence binding")
				}
				s.mu.Lock()
				epoch := s.connectorEpoch
				s.mu.Unlock()
				binding := daemonBrokerEvidenceBinding{scope: s.currentBrokerStateScope(), connector: c, connectorEpoch: epoch, broker: evidence}
				if !brokerScopeConcrete(binding.scope) {
					t.Fatalf("scope %+v is not concrete", binding.scope)
				}
				head, err := s.orderJournal.AuthorityHead()
				if err != nil {
					t.Fatal(err)
				}

				// producer stands for the alert producer another observation holds.
				producer := make(chan struct{})
				committing, landed := make(chan struct{}), make(chan bool, 1)
				commit := func() error {
					close(committing)
					if wait == "producer held" {
						<-producer
					} else {
						time.Sleep(time.Second)
					}
					return nil
				}
				go func() {
					var ok bool
					var err error
					if tc.order {
						ok, err = s.withCheckedBrokerAndOrderEvidence(binding, s.orderJournal, head.LastEventSeq, commit)
					} else {
						ok, err = s.withCheckedBrokerEvidence(binding, commit)
					}
					landed <- ok && err == nil
				}()
				<-committing

				before := c.OrderLifecycleGeneration()
				began := time.Now()
				f.sendOrderStatus(42)
				for c.OrderLifecycleGeneration() == before {
					if time.Since(began) > 3*time.Second {
						close(producer)
						t.Fatalf("an order frame was held more than 3 s behind an alert commit (%s)", wait)
					}
					time.Sleep(time.Millisecond)
				}
				frameTook := time.Since(began)
				began = time.Now()
				c.WithBrokerEvidenceMutation(func() {})
				publicationTook := time.Since(began)
				close(producer)
				if !<-landed {
					t.Fatal("a commit whose evidence was current at its check did not land")
				}
				t.Logf("behind an alert commit (%s): order frame %s, publication side %s", wait, frameTook, publicationTook)
				if frameTook > readerFrameBound || publicationTook > readerFrameBound {
					t.Fatalf("behind an alert commit (%s) an order frame took %s and the publication side %s; bound %s", wait, frameTook, publicationTook, readerFrameBound)
				}
			})
		}
	}
}

// Evidence that moved before the check still drops the commit: the barrier
// keeps judging an alert on evidence that did not change underneath it.
func TestCheckedAlertCommitDropsOnMovedEvidence(t *testing.T) {
	f := startFakeTWS(t)
	s, _ := sessionLossServer(t, f, "2026-10-07T18:00:00Z")
	c := s.gatewayConnectorForTest()
	evidence, ok := c.CaptureBrokerEvidence()
	if !ok {
		t.Fatal("ready connector gave no evidence binding")
	}
	s.mu.Lock()
	epoch := s.connectorEpoch
	s.mu.Unlock()
	binding := daemonBrokerEvidenceBinding{scope: s.currentBrokerStateScope(), connector: c, connectorEpoch: epoch, broker: evidence}
	before := c.OrderLifecycleGeneration()
	f.sendOrderStatus(43)
	deadline := time.Now().Add(3 * time.Second)
	for c.OrderLifecycleGeneration() == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	called := false
	ok, err := s.withCheckedBrokerEvidence(binding, func() error { called = true; return nil })
	if ok || err != nil || called {
		t.Fatalf("a commit on moved order evidence ran: ok %v err %v called %v", ok, err, called)
	}
}

// Many alert commits, order frames, connector-publication mutations and
// evidence captures at once all finish: the new order (commit lock, then the
// barrier briefly, then the journal briefly, then the producer and the
// store) inverts against nothing the reader or publication holds.
func TestCheckedAlertCommitsStayLiveUnderFramesAndPublication(t *testing.T) {
	f := startFakeTWS(t)
	s, _ := sessionLossServerWith(t, f, "2026-10-07T18:00:00Z", func(s *Server) {
		s.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "orders"))
	})
	c := s.gatewayConnectorForTest()
	s.mu.Lock()
	epoch := s.connectorEpoch
	s.mu.Unlock()
	scope := s.currentBrokerStateScope()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := range 40 {
			wg.Go(func() {
				evidence, ok := c.CaptureBrokerEvidence()
				if !ok {
					return
				}
				binding := daemonBrokerEvidenceBinding{scope: scope, connector: c, connectorEpoch: epoch, broker: evidence}
				head, err := s.orderJournal.AuthorityHead()
				if err != nil {
					return
				}
				commit := func() error { time.Sleep(5 * time.Millisecond); return nil }
				if i%2 == 0 {
					_, _ = s.withCheckedBrokerAndOrderEvidence(binding, s.orderJournal, head.LastEventSeq, commit)
				} else {
					_, _ = s.withCheckedBrokerEvidence(binding, commit)
				}
			})
			wg.Go(func() { f.sendOrderStatus(100 + i) })
			wg.Go(func() { c.WithBrokerEvidenceMutation(func() {}) })
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("alert commits, order frames and publication did not finish together: a lock inversion")
	}
}
