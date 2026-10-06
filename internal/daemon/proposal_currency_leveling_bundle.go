package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// One approval for a loan repaid from several currencies (owner answer
// 2026-10-06 06:38 CEST, "Several sources ... but one approval"; confirmed
// 2026-10-06 07:11 CEST). prepare_bundle prepares every conversion of a
// served bundle through the ordinary prepare path and retains them together
// behind one private reference. submit_bundle runs every conversion's full
// prepared-submit check first, with nothing sent, then sends them in order,
// cheapest payer first, and stops at the first refusal. A partly sent bundle
// is safe: each conversion stays within its own allotment and share of the
// band, and the next cycle plans what remains once the ledger shows the
// fills. The bundle never widens a gate: each conversion is still one order
// under every check a single prepared submit runs.

const (
	preparedBundleKind   = "prepared_leveling_bundle_v1"
	preparedBundlePrefix = "canarypb1"
)

type preparedBundleRecord struct {
	Version       int               `json:"version"`
	ReferenceHash [sha256.Size]byte `json:"reference_hash"`
	ID            string            `json:"id"`
	BundleID      string            `json:"bundle_id"`
	Revision      string            `json:"revision"`
	// Keys and Preparations name the conversions in send order: each row's
	// key and the ID of its retained preparation.
	Keys         []string  `json:"keys"`
	Preparations []string  `json:"preparations"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// servedLevelingBundle refreshes and returns the served bundle id at
// revision with its rows in send order.
func (e *proposalEngine) servedLevelingBundle(ctx context.Context, id, revision string) ([]rpc.TradeProposal, []rpc.TradingBlocker, error) {
	id, revision = strings.TrimSpace(id), strings.TrimSpace(revision)
	if id == "" || revision == "" {
		return nil, preparedBlocker("bad_request", "bundle id and revision are required"), nil
	}
	snap, err := e.bundleSnapshot(ctx)
	if err != nil && len(snap.Proposals) == 0 {
		return nil, snap.Blockers, err
	}
	if len(snap.Blockers) > 0 && len(snap.Proposals) == 0 {
		return nil, snap.Blockers, nil
	}
	if len(snap.AutoTrade.Blockers) > 0 {
		return nil, snap.AutoTrade.Blockers, nil
	}
	var bundle *rpc.TradeProposalCurrencyLevelingBundle
	if st := snap.CurrencyLeveling; st != nil {
		for i := range st.Bundles {
			if st.Bundles[i].ID == id {
				bundle = &st.Bundles[i]
			}
		}
	}
	if bundle == nil {
		return nil, preparedBlocker("bundle_not_found", "The repayment is not in the current proposals; refresh them."), nil
	}
	if bundle.Revision != revision {
		return nil, preparedBlocker("stale_revision", "The repayment changed; refresh proposals before preparing or sending it."), nil
	}
	rows := make([]rpc.TradeProposal, 0, len(bundle.Keys))
	for _, key := range bundle.Keys {
		row, ok := servedProposal(snap, key)
		if !ok {
			return nil, preparedBlocker("bundle_not_found", "A conversion of the repayment is not in the current proposals; refresh them."), nil
		}
		rows = append(rows, row)
	}
	return rows, nil, nil
}

// bundleSnapshot is what a bundle resolves against: a fresh refresh, as a
// single row's revalidation does, or the installed snapshot when a test
// injects the row resolver.
func (e *proposalEngine) bundleSnapshot(ctx context.Context) (rpc.TradeProposalSnapshot, error) {
	if e.resolve != nil || e.revalidateForTest != nil {
		return e.Snapshot(false), nil
	}
	return e.Refresh(ctx, false)
}

// PrepareBundle prepares every conversion of a served bundle; it never
// places an order. The private reference is returned only when all are.
func (e *proposalEngine) PrepareBundle(ctx context.Context, p rpc.TradeProposalPrepareBundleParams) (rpc.TradeProposalPrepareBundleResult, error) {
	out := rpc.TradeProposalPrepareBundleResult{BundleID: p.BundleID, Revision: p.Revision, Legs: []rpc.TradeProposalPreviewResult{}, AsOf: e.clock()}
	rows, blockers, err := e.servedLevelingBundle(ctx, p.BundleID, p.Revision)
	if err != nil || len(blockers) > 0 {
		out.Blockers = blockers
		return out, err
	}
	ids := make([]string, 0, len(rows))
	var expires time.Time
	for i, row := range rows {
		res, err := e.prepare(ctx, rpc.TradeProposalPreviewParams{Key: row.Key, Revision: row.Revision, TimeoutMs: p.TimeoutMs}, nil)
		out.Legs = append(out.Legs, res.TradeProposalPreviewResult)
		if err != nil || !res.Accepted || res.PreparedRef == "" || res.Preparation == nil {
			out.Blockers = []rpc.TradingBlocker{{Code: "bundle_conversion_not_prepared",
				Message: fmt.Sprintf("conversion %d of %d could not be prepared, so the repayment cannot be approved; its own blockers say why", i+1, len(rows)),
				Action:  "Refresh proposals and prepare the repayment again."}}
			return out, err
		}
		ids = append(ids, res.Preparation.ID)
		if expires.IsZero() || res.Preparation.ExpiresAt.Before(expires) {
			expires = res.Preparation.ExpiresAt
		}
	}
	ref, err := e.retainBundle(ctx, p.BundleID, p.Revision, rows, ids, expires)
	if err != nil {
		out.Blockers = preparedBlocker("preparation_not_persisted", "The prepared repayment could not be retained; nothing can be sent from it.")
		return out, err
	}
	out.Accepted, out.BundleRef, out.ExpiresAt, out.AsOf = true, ref, expires, e.clock()
	return out, nil
}

func (e *proposalEngine) retainBundle(ctx context.Context, bundleID, revision string, rows []rpc.TradeProposal, ids []string, expires time.Time) (string, error) {
	store, err := e.preparationAuthority()
	if err != nil {
		return "", err
	}
	id, err := randomTokenID()
	if err != nil {
		return "", err
	}
	secret, err := randomTokenID()
	if err != nil {
		return "", err
	}
	reference := preparedBundlePrefix + "." + id + "." + secret
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.Key)
	}
	record := preparedBundleRecord{Version: 1, ReferenceHash: sha256.Sum256([]byte(reference)), ID: id, BundleID: bundleID, Revision: revision,
		Keys: keys, Preparations: slices.Clone(ids), ExpiresAt: expires}
	raw, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	if _, err := store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: "proposal-bundle:" + id, Kind: preparedBundleKind, JSON: raw}); err != nil {
		return "", err
	}
	return reference, nil
}

func (e *proposalEngine) loadBundle(ctx context.Context, reference string) (preparedBundleRecord, error) {
	var record preparedBundleRecord
	parts := strings.Split(reference, ".")
	if len(parts) != 3 || parts[0] != preparedBundlePrefix {
		return record, fmt.Errorf("invalid prepared bundle reference")
	}
	for _, part := range parts[1:] {
		raw, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || len(raw) != 16 || base64.RawURLEncoding.EncodeToString(raw) != part {
			return record, fmt.Errorf("invalid prepared bundle reference")
		}
	}
	store, err := e.preparationAuthority()
	if err != nil {
		return record, err
	}
	doc, found, err := store.GetStateDocument(ctx, "proposal-bundle:"+parts[1], preparedBundleKind)
	switch {
	case err != nil:
		return record, err
	case !found:
		return record, fmt.Errorf("prepared bundle reference is unavailable")
	}
	if err := json.Unmarshal(doc.JSON, &record); err != nil {
		return preparedBundleRecord{}, fmt.Errorf("prepared bundle record is unreadable")
	}
	digest := sha256.Sum256([]byte(reference))
	if record.Version != 1 || record.ID != parts[1] || subtle.ConstantTimeCompare(record.ReferenceHash[:], digest[:]) != 1 ||
		len(record.Keys) == 0 || len(record.Keys) != len(record.Preparations) {
		return preparedBundleRecord{}, fmt.Errorf("prepared bundle reference is unavailable")
	}
	return record, nil
}

// SubmitBundle checks every conversion of a prepared bundle, sending
// nothing until all pass, then sends them in order and stops at the first
// refusal. It runs inside brokerWriteMu, like every proposal submit.
func (e *proposalEngine) SubmitBundle(ctx context.Context, p rpc.TradeProposalSubmitBundleParams) (rpc.TradeProposalSubmitBundleResult, error) {
	out := rpc.TradeProposalSubmitBundleResult{BundleID: p.BundleID, Legs: []rpc.TradeProposalSubmitResult{}, AsOf: e.clock()}
	record, err := e.loadBundle(ctx, p.BundleRef)
	switch {
	case err != nil:
		out.Blockers = preparedBlocker("prepared_reference_unavailable", "The prepared repayment reference cannot be resolved safely.")
		return out, nil
	case record.BundleID != p.BundleID || record.Revision != p.Revision:
		out.Blockers = preparedBlocker("prepared_binding_mismatch", "Bundle id and revision must match the prepared repayment.")
		return out, nil
	case !e.clock().Before(record.ExpiresAt):
		out.Blockers = preparedBlocker("prepared_reference_expired", "Prepare the repayment again and confirm it again.")
		return out, nil
	}
	rows, blockers, err := e.servedLevelingBundle(ctx, p.BundleID, p.Revision)
	if err != nil || len(blockers) > 0 {
		out.Blockers = blockers
		return out, err
	}
	if len(rows) != len(record.Keys) {
		out.Blockers = preparedBlocker("prepared_proposal_changed", "The repayment's conversions changed; prepare and confirm it again.")
		return out, nil
	}
	type checkedLeg struct {
		params rpc.TradeProposalSubmitParams
		record preparedProposalRecord
		prop   rpc.TradeProposal
		out    rpc.TradeProposalSubmitResult
	}
	legs := make([]checkedLeg, 0, len(rows))
	for i, row := range rows {
		started := time.Now()
		leg := checkedLeg{out: rpc.TradeProposalSubmitResult{AsOf: e.clock()}}
		rec, err := e.loadPreparationRecord(ctx, record.Preparations[i])
		if err != nil || rec.Preparation.Key != record.Keys[i] || rec.Preparation.Key != row.Key || rec.Preparation.Revision != row.Revision {
			leg.out.Blockers = preparedBlocker("prepared_binding_mismatch", "A retained conversion does not match the repayment as served; prepare and confirm it again.")
			return e.refuseBundle(out, leg.out, leg.params, started, i, len(rows)), nil
		}
		leg.params = rpc.TradeProposalSubmitParams{Key: rec.Preparation.Key, Revision: rec.Preparation.Revision, FastPath: p.FastPath, TimeoutMs: p.TimeoutMs, Origin: p.Origin}
		prop, ok, err := e.preparedSubmitCheck(ctx, leg.params, rec, &leg.out, nil)
		if !ok {
			return e.refuseBundle(out, leg.out, leg.params, started, i, len(rows)), err
		}
		leg.record, leg.prop = rec, prop
		legs = append(legs, leg)
	}
	for i := range legs {
		started := time.Now()
		leg := &legs[i]
		e.preparedSubmitPlace(ctx, leg.params, leg.record, leg.prop, &leg.out)
		e.finishSubmit("submit_bundle", leg.params, &leg.out, nil, started)
		out.Legs = append(out.Legs, leg.out)
		if leg.out.Accepted && len(leg.out.Blockers) == 0 {
			continue
		}
		for _, rest := range legs[i+1:] {
			out.Legs = append(out.Legs, rpc.TradeProposalSubmitResult{Proposal: rest.prop, AsOf: e.clock(),
				Blockers: preparedBlocker("bundle_conversion_not_sent", fmt.Sprintf("not sent: conversion %d of the repayment was refused first", i+1))})
		}
		out.Blockers = []rpc.TradingBlocker{{Code: "bundle_partly_sent",
			Message: fmt.Sprintf("conversion %d of %d was refused; the %d before it were sent, the rest were not", i+1, len(legs), i),
			Action:  "Inspect each conversion's receipt; the next cycle plans what remains once the ledger shows the fills."}}
		out.AsOf = e.clock()
		return out, nil
	}
	out.Accepted, out.AsOf = true, e.clock()
	return out, nil
}

// refuseBundle records a conversion that failed its checks and reports the
// bundle as refused with nothing sent.
func (e *proposalEngine) refuseBundle(out rpc.TradeProposalSubmitBundleResult, leg rpc.TradeProposalSubmitResult, params rpc.TradeProposalSubmitParams, started time.Time, i, n int) rpc.TradeProposalSubmitBundleResult {
	e.finishSubmit("submit_bundle", params, &leg, nil, started)
	out.Legs = append(out.Legs, leg)
	out.Blockers = []rpc.TradingBlocker{{Code: "bundle_conversion_refused",
		Message: fmt.Sprintf("conversion %d of %d did not pass its checks, so nothing was sent; its own blockers say why", i+1, n),
		Action:  "Refresh proposals and prepare the repayment again."}}
	out.AsOf = e.clock()
	return out
}

func (s *Server) handleTradeProposalsPrepareBundle(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalPrepareBundleResult, error) {
	var p rpc.TradeProposalPrepareBundleParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalPrepareBundleResult{AsOf: s.orderNow(), Blockers: preparedBlocker("proposal_engine_unavailable", "Proposal engine is unavailable.")}, nil
	}
	out, err := s.tradeProposals.PrepareBundle(ctx, p)
	return &out, err
}

func (s *Server) handleTradeProposalsSubmitBundle(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalSubmitBundleResult, error) {
	var p rpc.TradeProposalSubmitBundleParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalSubmitBundleResult{AsOf: s.orderNow(), Blockers: preparedBlocker("proposal_engine_unavailable", "Proposal engine is unavailable.")}, nil
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	out, err := s.tradeProposals.SubmitBundle(ctx, p)
	return &out, err
}
