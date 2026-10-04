package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	setupMarkoutScope        = "setups/markouts"
	setupMarkoutStateKind    = "setup_markouts.schedule.v1"
	setupMarkoutResolvedKind = "setup_markouts.target.v1"
	setupMarkoutSource       = "setup-markout"

	// setupMarkoutMaxPending bounds the durable schedule. Scheduling beyond it
	// resolves the oldest pending targets as missing rather than growing the
	// state document or dropping a fill silently.
	setupMarkoutMaxPending = 256
)

// setupMarkoutDocument is the bounded current schedule. Resolved targets
// leave it atomically with their append-only observation.
type setupMarkoutDocument struct {
	Version int `json:"version"`
	// TrackingSince is when this database began scheduling markouts. Fills
	// received earlier are never scheduled or backdated.
	TrackingSince time.Time                `json:"tracking_since"`
	Pending       []rpc.SetupMarkoutTarget `json:"pending"`
}

// setupMarkoutStore owns the markout schedule in daemon.db. The schedule is a
// state document; each resolved target (captured or missing) is an immutable
// non-decision observation. Neither advances the order-event frontier.
type setupMarkoutStore struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	doc      setupMarkoutDocument
	resolved map[string]bool
}

func setupMarkoutKey(t rpc.SetupMarkoutTarget) string {
	return t.OrderRef + "\x00" + t.ExecID + "\x00" + t.Horizon
}

func bindSetupMarkoutStore(ctx context.Context, core *corestore.Store, now time.Time) (*setupMarkoutStore, error) {
	if core == nil {
		return nil, fmt.Errorf("setup markout SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, setupMarkoutScope, setupMarkoutStateKind)
	if err != nil {
		return nil, fmt.Errorf("load setup markout schedule: %w", err)
	}
	state := setupMarkoutDocument{Version: 1, TrackingSince: now.UTC(), Pending: []rpc.SetupMarkoutTarget{}}
	if ok {
		if err := json.Unmarshal(doc.JSON, &state); err != nil {
			return nil, fmt.Errorf("decode setup markout schedule: %w", err)
		}
		if state.Version != 1 || state.TrackingSince.IsZero() {
			return nil, fmt.Errorf("setup markout schedule has unsupported version %d", state.Version)
		}
	} else {
		raw, _ := json.Marshal(state)
		doc, err = core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: setupMarkoutScope, Kind: setupMarkoutStateKind, JSON: raw})
		if err != nil {
			return nil, fmt.Errorf("initialize setup markout schedule: %w", err)
		}
	}
	st := &setupMarkoutStore{core: core, revision: doc.Revision, doc: state, resolved: map[string]bool{}}
	rows, err := st.loadResolved(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		st.resolved[setupMarkoutKey(row)] = true
	}
	return st, nil
}

func (st *setupMarkoutStore) loadResolved(ctx context.Context) ([]rpc.SetupMarkoutTarget, error) {
	var out []rpc.SetupMarkoutTarget
	var after int64
	for {
		page, err := st.core.ListObservations(ctx, corestore.ObservationQuery{
			ScopeKey: setupMarkoutScope, Source: setupMarkoutSource, Kind: setupMarkoutResolvedKind,
			AfterObservationID: after, Limit: 10000,
		})
		if err != nil {
			return nil, fmt.Errorf("load setup markout targets: %w", err)
		}
		for _, row := range page {
			var target rpc.SetupMarkoutTarget
			if err := json.Unmarshal(row.Payload, &target); err != nil {
				return nil, fmt.Errorf("decode setup markout target %d: %w", row.ID, err)
			}
			out = append(out, target)
			after = max(after, row.ID)
		}
		if len(page) < 10000 {
			return out, nil
		}
	}
}

func (st *setupMarkoutStore) trackingSince() time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.doc.TrackingSince
}

func (st *setupMarkoutStore) known(t rpc.SetupMarkoutTarget) bool {
	key := setupMarkoutKey(t)
	if st.resolved[key] {
		return true
	}
	return slices.ContainsFunc(st.doc.Pending, func(p rpc.SetupMarkoutTarget) bool { return setupMarkoutKey(p) == key })
}

func (st *setupMarkoutStore) pending() []rpc.SetupMarkoutTarget {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Clone(st.doc.Pending)
}

// schedule adds new targets idempotently. A target already resolved is
// returned in its incoming state: callers resolve such targets immediately.
// Overflow beyond the pending bound resolves the oldest pending targets as
// missing in the same commit.
func (st *setupMarkoutStore) schedule(ctx context.Context, targets []rpc.SetupMarkoutTarget, now time.Time) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := slices.Clone(st.doc.Pending)
	var resolved []rpc.SetupMarkoutTarget
	for _, t := range targets {
		if st.known(t) || slices.ContainsFunc(next, func(p rpc.SetupMarkoutTarget) bool { return setupMarkoutKey(p) == setupMarkoutKey(t) }) ||
			slices.ContainsFunc(resolved, func(p rpc.SetupMarkoutTarget) bool { return setupMarkoutKey(p) == setupMarkoutKey(t) }) {
			continue
		}
		if t.Status != rpc.SetupMarkoutPending {
			resolved = append(resolved, t)
			continue
		}
		next = append(next, t)
	}
	if len(next) > setupMarkoutMaxPending {
		slices.SortStableFunc(next, func(a, b rpc.SetupMarkoutTarget) int {
			return cmp.Or(a.ScheduledAt.Compare(b.ScheduledAt), a.FillAt.Compare(b.FillAt), a.TargetAt.Compare(b.TargetAt))
		})
		overflow := len(next) - setupMarkoutMaxPending
		for _, t := range next[:overflow] {
			resolved = append(resolved, setupMarkoutMissing(t, "backlog_full", now))
		}
		next = slices.Clone(next[overflow:])
	}
	if len(resolved) == 0 && len(next) == len(st.doc.Pending) {
		return nil
	}
	return st.commitLocked(ctx, next, resolved)
}

// resolve removes targets from the schedule and appends their terminal
// records. Targets no longer pending are ignored, so a late capture cannot
// overwrite a missing record or record a target twice.
func (st *setupMarkoutStore) resolve(ctx context.Context, records []rpc.SetupMarkoutTarget) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	byKey := make(map[string]rpc.SetupMarkoutTarget, len(records))
	for _, r := range records {
		byKey[setupMarkoutKey(r)] = r
	}
	next := make([]rpc.SetupMarkoutTarget, 0, len(st.doc.Pending))
	var resolved []rpc.SetupMarkoutTarget
	for _, p := range st.doc.Pending {
		if r, ok := byKey[setupMarkoutKey(p)]; ok && r.Status != rpc.SetupMarkoutPending {
			resolved = append(resolved, r)
			continue
		}
		next = append(next, p)
	}
	if len(resolved) == 0 {
		return nil
	}
	return st.commitLocked(ctx, next, resolved)
}

func (st *setupMarkoutStore) commitLocked(ctx context.Context, pending, resolved []rpc.SetupMarkoutTarget) error {
	if st.core == nil {
		return fmt.Errorf("setup markout SQLite authority is unavailable")
	}
	doc := setupMarkoutDocument{Version: 1, TrackingSince: st.doc.TrackingSince, Pending: pending}
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode setup markout schedule: %w", err)
	}
	update := corestore.StateDocumentCAS{ScopeKey: setupMarkoutScope, Kind: setupMarkoutStateKind, ExpectedRevision: st.revision, JSON: raw}
	var saved corestore.StateDocument
	if len(resolved) == 0 {
		saved, err = st.core.CompareAndSwapStateDocument(ctx, update)
	} else {
		inputs := make([]corestore.ObservationInput, 0, len(resolved))
		for _, r := range resolved {
			payload, err := json.Marshal(r)
			if err != nil {
				return fmt.Errorf("encode setup markout target: %w", err)
			}
			observed := r.FillAt
			if observed.IsZero() {
				observed = r.ScheduledAt
			}
			inputs = append(inputs, corestore.ObservationInput{
				ScopeKey: setupMarkoutScope, Source: setupMarkoutSource, Kind: setupMarkoutResolvedKind,
				ObservedAt: observed, ContentType: "application/json", Payload: payload,
				// An entry diagnostic is never decision or accounting authority.
				DecisionEligible: false,
			})
		}
		saved, _, err = st.core.CompareAndSwapStateDocumentWithObservations(ctx, update, inputs)
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return fmt.Errorf("setup markout schedule changed outside its owner: %w", err)
	}
	if err != nil {
		return fmt.Errorf("persist setup markout schedule: %w", err)
	}
	st.revision = saved.Revision
	st.doc = doc
	for _, r := range resolved {
		st.resolved[setupMarkoutKey(r)] = true
	}
	return nil
}

// setupMarkoutFilter selects read-path rows; zero values are open.
type setupMarkoutFilter struct {
	OrderRef string
	Since    time.Time
	Symbol   string
}

func (f setupMarkoutFilter) match(t rpc.SetupMarkoutTarget) bool {
	return (f.OrderRef == "" || t.OrderRef == f.OrderRef) &&
		(f.Since.IsZero() || !t.FillAt.Before(f.Since)) &&
		(f.Symbol == "" || strings.EqualFold(t.Contract.Symbol, f.Symbol))
}

// list returns pending and resolved targets matching f in fill order.
func (st *setupMarkoutStore) list(ctx context.Context, f setupMarkoutFilter) ([]rpc.SetupMarkoutTarget, setupMarkoutDocument, error) {
	st.mu.Lock()
	doc := setupMarkoutDocument{Version: st.doc.Version, TrackingSince: st.doc.TrackingSince, Pending: slices.Clone(st.doc.Pending)}
	st.mu.Unlock()
	resolved, err := st.loadResolved(ctx)
	if err != nil {
		return nil, doc, err
	}
	out := []rpc.SetupMarkoutTarget{}
	for _, t := range append(resolved, doc.Pending...) {
		if f.match(t) {
			out = append(out, t)
		}
	}
	slices.SortStableFunc(out, func(a, b rpc.SetupMarkoutTarget) int {
		return cmp.Or(a.FillAt.Compare(b.FillAt), strings.Compare(a.OrderRef, b.OrderRef), strings.Compare(a.ExecID, b.ExecID), a.TargetAt.Compare(b.TargetAt))
	})
	return out, doc, nil
}

func setupMarkoutMissing(t rpc.SetupMarkoutTarget, reason string, now time.Time) rpc.SetupMarkoutTarget {
	t.Status = rpc.SetupMarkoutMissing
	t.Reason = reason
	t.ResolvedAt = now.UTC()
	t.Bid, t.Ask, t.BidSize, t.AskSize, t.MarkPrice, t.Markout = nil, nil, nil, nil, nil, nil
	return t
}

// handleSetupMarkouts is the read-only markout ledger. It never schedules,
// captures or reads the broker.
func (s *Server) handleSetupMarkouts(ctx context.Context, req *rpc.Request) (*rpc.SetupMarkoutsResult, error) {
	var p rpc.SetupMarkoutsParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	p, err := rpc.NormalizeSetupMarkoutsParams(p)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	since, _ := rpc.SetupMarkoutsSince(p.Since)
	rt := s.setupMarkouts
	if rt == nil || rt.store == nil {
		return nil, fmt.Errorf("setup markout ledger is unavailable")
	}
	rows, doc, err := rt.store.list(ctx, setupMarkoutFilter{OrderRef: p.OrderRef, Since: since, Symbol: p.Symbol})
	if err != nil {
		return nil, err
	}
	out := &rpc.SetupMarkoutsResult{
		Version: 1, Kind: rpc.SetupMarkoutsKind, AsOf: s.setupMarkoutNow(), Targets: rows,
		Note: "Entry diagnostic: a displayed quote marked against the actual fill, not a fill, realized profit or accounting. Missing targets are never estimated.",
		Clock: rpc.SetupMarkoutsClock{
			TrackingSince: doc.TrackingSince, Pending: len(doc.Pending), MaxPending: setupMarkoutMaxPending,
			CaptureLeadSeconds: int(setupMarkoutQuoteBudget / time.Second), MaxQuoteAgeSeconds: int(setupMarkoutMaxQuoteAge / time.Second),
			RegularMinutes: int(setupMarkoutRegularMinutes / time.Minute),
		},
	}
	for _, t := range doc.Pending {
		if out.Clock.NextTargetAt == nil || t.TargetAt.Before(*out.Clock.NextTargetAt) {
			out.Clock.NextTargetAt = new(t.TargetAt)
		}
	}
	return out, nil
}
