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

const pushDeliveryProofStateKind = "push_delivery_proof"

type pushDeliveryProofDocument struct {
	Version int                     `json:"version"`
	Status  *rpc.PushDeliveryStatus `json:"status,omitempty"`
}

// pushDeliveryProofAuthority retains the latest Web Push delivery proof the
// paired app host reported. The daemon never sends a push; it keeps the app's
// redacted evidence so `canary status`, Desk, and a later pre-authorised
// automation gate read one record of whether the phone channel is witnessed.
type pushDeliveryProofAuthority struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	status   *rpc.PushDeliveryStatus
}

// errPushDeliveryProofDiscarded reports a retained proof that no longer
// validates. The daemon starts without it: absent evidence reads as "not
// witnessed", which is the safe side, and the next app report replaces it.
var errPushDeliveryProofDiscarded = errors.New("retained push delivery proof discarded")

// bindCore loads the retained proof before the daemon publishes its socket.
// A missing document means no app host has reported yet; nothing is written
// until one does. An unreadable document is discarded, never fatal: a mirror
// of app evidence must not keep the daemon from starting.
func (a *pushDeliveryProofAuthority) bindCore(ctx context.Context, core *corestore.Store) error {
	if a == nil || core == nil {
		return errors.New("push delivery proof SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, pushDeliveryProofStateKind)
	if err != nil {
		return fmt.Errorf("load push delivery proof: %w", err)
	}
	var status *rpc.PushDeliveryStatus
	var revision int64
	var discarded error
	if ok {
		revision = doc.Revision
		status, discarded = decodePushDeliveryProofDocument(doc.JSON)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.core = core
	a.revision = revision
	a.status = status
	return discarded
}

func decodePushDeliveryProofDocument(raw []byte) (*rpc.PushDeliveryStatus, error) {
	var stored pushDeliveryProofDocument
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("%w: %v", errPushDeliveryProofDiscarded, err)
	}
	if stored.Version != 1 {
		return nil, fmt.Errorf("%w: unsupported version %d", errPushDeliveryProofDiscarded, stored.Version)
	}
	if stored.Status == nil {
		return nil, nil
	}
	if stored.Status.ReceivedAt.IsZero() {
		return nil, fmt.Errorf("%w: missing received_at", errPushDeliveryProofDiscarded)
	}
	if err := rpc.ValidatePushDeliveryProof(stored.Status.Proof); err != nil {
		return nil, fmt.Errorf("%w: %v", errPushDeliveryProofDiscarded, err)
	}
	return stored.Status, nil
}

// record keeps proof when it is at least as recent as the retained report.
// An older report is ignored rather than refused so a slow relay retry can
// never roll the evidence back.
func (a *pushDeliveryProofAuthority) record(ctx context.Context, proof rpc.PushDeliveryProof, now time.Time) (rpc.AlertDeliveryProofResult, error) {
	if err := rpc.ValidatePushDeliveryProof(proof); err != nil {
		return rpc.AlertDeliveryProofResult{}, errBadRequest(err.Error())
	}
	now = now.UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status != nil && proof.ReportedAt.Before(a.status.Proof.ReportedAt) {
		return rpc.AlertDeliveryProofResult{Accepted: false, ReceivedAt: a.status.ReceivedAt}, nil
	}
	next := &rpc.PushDeliveryStatus{Proof: proof, ReceivedAt: now}
	if a.core != nil {
		raw, err := json.Marshal(pushDeliveryProofDocument{Version: 1, Status: next})
		if err != nil {
			return rpc.AlertDeliveryProofResult{}, fmt.Errorf("encode push delivery proof: %w", err)
		}
		saved, err := a.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: pushDeliveryProofStateKind, ExpectedRevision: a.revision, JSON: raw,
		})
		if err != nil {
			return rpc.AlertDeliveryProofResult{}, fmt.Errorf("persist push delivery proof: %w", err)
		}
		a.revision = saved.Revision
	}
	a.status = next
	return rpc.AlertDeliveryProofResult{Accepted: true, ReceivedAt: now}, nil
}

// current returns a detached copy of the retained status, or nil when no app
// host has reported.
func (a *pushDeliveryProofAuthority) current() *rpc.PushDeliveryStatus {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status == nil {
		return nil
	}
	raw, err := json.Marshal(a.status)
	if err != nil {
		return nil
	}
	var out rpc.PushDeliveryStatus
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return &out
}

func (s *Server) handleAlertDeliveryProof(ctx context.Context, req *rpc.Request) (*rpc.AlertDeliveryProofResult, error) {
	var proof rpc.PushDeliveryProof
	if len(req.Params) == 0 {
		return nil, errBadRequest("push delivery proof is required")
	}
	if err := decodeParams(req.Params, &proof); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	res, err := s.pushDeliveryProof.record(ctx, proof, now)
	if err != nil {
		return nil, err
	}
	return &res, nil
}
