package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
)

// Mandate status clock (internal-docs/design/mandate-risk-budget.md): when
// the verdict's status (within, over, reduce, unknown) last changed, kept
// across evaluations and restarts in a daemon state document, so the verdict
// can say how long it has read as it does. The owner asked how often and
// how long the verdict is unknown, because a machine sleeve cannot trade
// while it is (2026-10-18); each change is also journaled in the risk policy
// journal as mandate_status_changed, with how long the old status held.

const (
	stateKindMandateStatus = "mandate_status"
	mandateStatusDocVer    = 1
)

type mandateStatusDocV1 struct {
	Version int       `json:"version"`
	Status  string    `json:"status"`
	Since   time.Time `json:"since"`
}

// mandateStatusClock is the persisted status and when it began.
type mandateStatusClock struct {
	mu       sync.Mutex
	loaded   bool
	revision int64
	doc      mandateStatusDocV1
}

// mandateStatusSince records one evaluation reading status at `at` and
// returns when that status has held since: `at` on a change, the kept time
// while it holds. Storage trouble is logged and never changes the verdict.
func (s *Server) mandateStatusSince(ctx context.Context, status string, at time.Time) time.Time {
	if s == nil || status == "" {
		return time.Time{}
	}
	clock := &s.mandateStatus
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if !clock.loaded {
		clock.loaded = true
		if s.coreStore != nil {
			doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, stateKindMandateStatus)
			switch {
			case err != nil:
				s.warnf("mandate status clock unavailable: %v", err)
			case ok:
				var kept mandateStatusDocV1
				if err := json.Unmarshal(doc.JSON, &kept); err != nil || kept.Version != mandateStatusDocVer {
					s.warnf("mandate status clock unreadable; starting it again")
				} else {
					clock.doc = kept
				}
				clock.revision = doc.Revision
			}
		}
	}
	if clock.doc.Status == status && !clock.doc.Since.IsZero() && !at.Before(clock.doc.Since) {
		return clock.doc.Since
	}
	previous := clock.doc
	clock.doc = mandateStatusDocV1{Version: mandateStatusDocVer, Status: status, Since: at.UTC()}
	if previous.Status != "" && s.riskCapital != nil {
		s.riskCapital.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": at.UTC(), "kind": "mandate_status_changed",
			"from": previous.Status, "to": status, "held_seconds": int64(at.Sub(previous.Since) / time.Second),
		})
	}
	if s.coreStore != nil {
		raw, err := json.Marshal(clock.doc)
		if err == nil {
			var saved corestore.StateDocument
			saved, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
				ScopeKey: daemonStateScope, Kind: stateKindMandateStatus, ExpectedRevision: clock.revision, JSON: raw,
			})
			if conflict, ok := errors.AsType[*corestore.RevisionConflictError](err); ok {
				// Another writer moved the document; take its revision and
				// write again once, keeping what this evaluation read.
				clock.revision = conflict.Actual
				saved, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
					ScopeKey: daemonStateScope, Kind: stateKindMandateStatus, ExpectedRevision: clock.revision, JSON: raw,
				})
			}
			if err == nil {
				clock.revision = saved.Revision
			}
		}
		if err != nil {
			s.warnf("mandate status clock not persisted: %v", err)
		}
	}
	return clock.doc.Since
}
