package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The mandate's breach episodes and the hold on Fully armed (build B5, owner
// 2026-10-31: one loss ladder). The reduction opens an episode at the second
// consecutive reduce verdict and ends it when the loss measure is back within
// the loss room (mandate_reduction.go). The arming hold needs to know whether
// a breach has happened since the owner confirmed full scope, so the last
// episode that opened is kept here, in a daemon state document across
// restarts, and compared with the episode recorded at confirmation.

const (
	stateKindMandateEpisode = "mandate_breach_episode"
	mandateEpisodeDocVer    = 1
)

type mandateEpisodeDocV1 struct {
	Version int       `json:"version"`
	Episode string    `json:"episode"`
	At      time.Time `json:"at"`
}

// mandateEpisodeClock is the persisted last episode and when it was seen.
type mandateEpisodeClock struct {
	mu       sync.Mutex
	loaded   bool
	revision int64
	doc      mandateEpisodeDocV1
	// pending is a breach observed before its durable receipt completed.
	// A later within reading must finish that receipt before clearing a hold.
	pending mandateEpisodeDocV1
}

// mandateBreach reads the reduction's state and open episode from the
// proposals snapshot; ok is false when there is no snapshot to read.
func (s *Server) mandateBreach() (state, episode string, ok bool) {
	if s.mandateBreachForTest != nil {
		state, episode = s.mandateBreachForTest()
		return state, episode, true
	}
	if s.tradeProposals == nil {
		return "", "", false
	}
	st := s.tradeProposals.snapshotForRead(false, false).MandateReduction
	if st == nil {
		// No [mandate] in the constitution: nothing to breach.
		return "", "", true
	}
	return st.State, st.Episode, true
}

// lastMandateEpisodeLocked records the episode now open, when there is one,
// and returns only the last durably recorded episode. The caller holds mu
// through reading or replacing the proposals snapshot, so an older status
// read cannot overwrite the receipt for a newer published breach.
func (s *Server) lastMandateEpisodeLocked(ctx context.Context, open string, now time.Time) (string, error) {
	clock := &s.mandateEpisode
	if open != "" && open != clock.doc.Episode {
		clock.pending = mandateEpisodeDocV1{Version: mandateEpisodeDocVer, Episode: open, At: now.UTC()}
	}
	if s.coreStore == nil {
		return "", errors.New("mandate episode authority is unavailable")
	}
	if !clock.loaded {
		doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, stateKindMandateEpisode)
		if err != nil {
			return "", fmt.Errorf("read mandate episode clock: %w", err)
		}
		var kept mandateEpisodeDocV1
		if ok {
			if err := json.Unmarshal(doc.JSON, &kept); err != nil || kept.Version != mandateEpisodeDocVer || kept.Episode == "" || kept.At.IsZero() {
				return "", errors.New("mandate episode clock is unreadable")
			}
		}
		clock.doc, clock.revision, clock.loaded = kept, doc.Revision, true
	}
	if clock.pending.Episode == "" || clock.pending.Episode == clock.doc.Episode {
		clock.pending = mandateEpisodeDocV1{}
		return clock.doc.Episode, nil
	}
	raw, err := json.Marshal(clock.pending)
	if err != nil {
		return clock.doc.Episode, err
	}
	saved, err := s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: stateKindMandateEpisode, ExpectedRevision: clock.revision, JSON: raw,
	})
	if err != nil {
		if _, conflict := errors.AsType[*corestore.RevisionConflictError](err); conflict {
			// Reload the durable document before retrying a conflicting write.
			clock.loaded = false
		}
		return clock.doc.Episode, fmt.Errorf("persist mandate episode clock: %w", err)
	}
	clock.doc, clock.revision = clock.pending, saved.Revision
	clock.pending = mandateEpisodeDocV1{}
	return clock.doc.Episode, nil
}

// deskAuthorityHold reads what holds Fully armed: engaged while a breach
// episode is open, last the last episode that opened (empty when none ever
// did). known is false while the mandate's verdict cannot be measured, has
// not been measured yet, or there is no snapshot to read; the caller then
// holds, as it did for an unreadable brake.
func (s *Server) deskAuthorityHold(ctx context.Context) (last string, engaged, known bool) {
	s.mandateEpisode.mu.Lock()
	defer s.mandateEpisode.mu.Unlock()
	state, open, ok := s.mandateBreach()
	if !ok || state == rpc.MandateReductionUnknown || state == rpc.MandateReductionNoVerdict {
		return "", false, false
	}
	last, err := s.lastMandateEpisodeLocked(ctx, open, s.orderNow())
	if err != nil {
		s.warnf("mandate episode clock unavailable: %v", err)
	}
	return last, open != "", err == nil
}
