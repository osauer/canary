package daemon

import (
	"context"
	"encoding/json"
	"errors"
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

// lastMandateEpisode records the episode now open, when there is one, and
// returns the last episode that opened. Storage trouble is logged and never
// changes the answer for this process.
func (s *Server) lastMandateEpisode(ctx context.Context, open string, now time.Time) string {
	clock := &s.mandateEpisode
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if !clock.loaded {
		clock.loaded = true
		if s.coreStore != nil {
			doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, stateKindMandateEpisode)
			switch {
			case err != nil:
				s.warnf("mandate episode clock unavailable: %v", err)
			case ok:
				var kept mandateEpisodeDocV1
				if err := json.Unmarshal(doc.JSON, &kept); err != nil || kept.Version != mandateEpisodeDocVer {
					s.warnf("mandate episode clock unreadable; starting it again")
				} else {
					clock.doc = kept
				}
				clock.revision = doc.Revision
			}
		}
	}
	if open == "" || open == clock.doc.Episode {
		return clock.doc.Episode
	}
	clock.doc = mandateEpisodeDocV1{Version: mandateEpisodeDocVer, Episode: open, At: now.UTC()}
	if s.coreStore != nil {
		raw, err := json.Marshal(clock.doc)
		if err == nil {
			var saved corestore.StateDocument
			saved, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
				ScopeKey: daemonStateScope, Kind: stateKindMandateEpisode, ExpectedRevision: clock.revision, JSON: raw,
			})
			if conflict, ok := errors.AsType[*corestore.RevisionConflictError](err); ok {
				clock.revision = conflict.Actual
				saved, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
					ScopeKey: daemonStateScope, Kind: stateKindMandateEpisode, ExpectedRevision: clock.revision, JSON: raw,
				})
			}
			if err == nil {
				clock.revision = saved.Revision
			}
		}
		if err != nil {
			s.warnf("mandate episode clock not persisted: %v", err)
		}
	}
	return clock.doc.Episode
}

// deskAuthorityHold reads what holds Fully armed: engaged while a breach
// episode is open, last the last episode that opened (empty when none ever
// did). known is false while the mandate's verdict cannot be measured, has
// not been measured yet, or there is no snapshot to read; the caller then
// holds, as it did for an unreadable brake.
func (s *Server) deskAuthorityHold(ctx context.Context) (last string, engaged, known bool) {
	state, open, ok := s.mandateBreach()
	if !ok || state == rpc.MandateReductionUnknown || state == rpc.MandateReductionNoVerdict {
		return "", false, false
	}
	return s.lastMandateEpisode(ctx, open, s.orderNow()), open != "", true
}
