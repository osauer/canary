package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const preparedProposalKind = "prepared_proposal_v1"
const preparedProposalPrefix = "canarypp1"

type preparedProposalRecord struct {
	Version       int                          `json:"version"`
	ReferenceHash [sha256.Size]byte            `json:"reference_hash"`
	Preparation   rpc.TradeProposalPreparation `json:"preparation"`
	Proposal      rpc.TradeProposal            `json:"proposal"`
	Preview       rpc.OrderPreviewResult       `json:"preview"`
}

// Prepare retains the successful existing preview; it never places an order.
func (e *proposalEngine) Prepare(ctx context.Context, p rpc.TradeProposalPreviewParams) (rpc.TradeProposalPrepareResult, error) {
	p.FastPath = false // A backend preparation always resolves current evidence.
	var out rpc.TradeProposalPrepareResult
	preview, err := e.preview(ctx, p, func(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) error {
		ref, preparation, err := e.retainPreparation(ctx, prop, preview)
		if err != nil {
			return err
		}
		out.PreparedRef, out.Preparation = ref, preparation
		return nil
	})
	out.TradeProposalPreviewResult = preview
	return out, err
}

func (e *proposalEngine) preparationAuthority() (*corestore.Store, error) {
	if e == nil || e.server == nil || e.server.coreStore == nil {
		return nil, fmt.Errorf("prepared proposal authority is unavailable")
	}
	store, err := e.server.orderJournal.coreStore()
	if err != nil {
		return nil, err
	}
	if store != e.server.coreStore {
		return nil, fmt.Errorf("prepared proposal and order authorities differ")
	}
	return store, nil
}

func preparedDraftFingerprint(draft rpc.OrderDraft) (string, error) {
	raw, err := json.Marshal(draft)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (e *proposalEngine) retainPreparation(ctx context.Context, prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) (string, *rpc.TradeProposalPreparation, error) {
	store, err := e.preparationAuthority()
	if err != nil {
		return "", nil, err
	}
	if preview == nil || !preview.SubmitEligible || preview.PreviewToken == "" || preview.PreviewTokenID == "" || prop.Key == "" || prop.Revision == "" {
		return "", nil, fmt.Errorf("submit-eligible proposal preview is required")
	}
	id, err := randomTokenID()
	if err != nil {
		return "", nil, err
	}
	secret, err := randomTokenID()
	if err != nil {
		return "", nil, err
	}
	fingerprint, err := preparedDraftFingerprint(preview.Draft)
	if err != nil {
		return "", nil, err
	}
	reference := preparedProposalPrefix + "." + id + "." + secret
	meta := rpc.TradeProposalPreparation{ID: id, Key: prop.Key, Revision: prop.Revision, DraftFingerprint: fingerprint, ExpiresAt: preview.PreviewTokenExpiresAt, State: "prepared", Consumed: new(false)}
	record := preparedProposalRecord{Version: 1, ReferenceHash: sha256.Sum256([]byte(reference)), Preparation: meta, Proposal: prop, Preview: *preview}
	raw, err := json.Marshal(record)
	if err != nil {
		return "", nil, err
	}
	if _, err := store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: "proposal-preparation:" + id, Kind: preparedProposalKind, JSON: raw}); err != nil {
		return "", nil, err
	}
	return reference, &meta, nil
}

func parsePreparedReference(reference string) (string, error) {
	parts := strings.Split(reference, ".")
	if len(parts) != 3 || parts[0] != preparedProposalPrefix {
		return "", fmt.Errorf("invalid prepared proposal reference")
	}
	for _, part := range parts[1:] {
		raw, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || len(raw) != 16 || base64.RawURLEncoding.EncodeToString(raw) != part {
			return "", fmt.Errorf("invalid prepared proposal reference")
		}
	}
	return parts[1], nil
}

func (e *proposalEngine) loadPreparation(ctx context.Context, reference string) (preparedProposalRecord, error) {
	var record preparedProposalRecord
	id, err := parsePreparedReference(reference)
	if err != nil {
		return record, err
	}
	store, err := e.preparationAuthority()
	if err != nil {
		return record, err
	}
	doc, found, err := store.GetStateDocument(ctx, "proposal-preparation:"+id, preparedProposalKind)
	if err != nil {
		return record, err
	}
	if !found {
		return record, fmt.Errorf("prepared proposal reference is unavailable")
	}
	if err := json.Unmarshal(doc.JSON, &record); err != nil {
		return record, fmt.Errorf("prepared proposal record is unreadable")
	}
	digest := sha256.Sum256([]byte(reference))
	if record.Version != 1 || record.Preparation.ID != id || subtle.ConstantTimeCompare(record.ReferenceHash[:], digest[:]) != 1 {
		return preparedProposalRecord{}, fmt.Errorf("prepared proposal reference is unavailable")
	}
	fingerprint, err := preparedDraftFingerprint(record.Preview.Draft)
	if err != nil || fingerprint != record.Preparation.DraftFingerprint || record.Proposal.Key != record.Preparation.Key || record.Proposal.Revision != record.Preparation.Revision || !record.Preview.PreviewTokenExpiresAt.Equal(record.Preparation.ExpiresAt) {
		return preparedProposalRecord{}, fmt.Errorf("prepared proposal binding is invalid")
	}
	return record, nil
}

func (e *proposalEngine) preparationScopeMatches(record preparedProposalRecord) bool {
	status := e.server.currentTradingStatus()
	p := record.Preview
	return p.Account != "" && p.Mode != "" && p.Endpoint != "" && p.Account == status.Account && p.Mode == status.Mode && p.Endpoint == status.Endpoint && p.ClientID == status.ClientID
}

func preparedBlocker(code, message string) []rpc.TradingBlocker {
	return []rpc.TradingBlocker{{Code: code, Message: message}}
}

// PreparedStatus consults local authority only; it never re-prepares or sends.
func (e *proposalEngine) PreparedStatus(ctx context.Context, reference string) (rpc.TradeProposalPreparedStatusResult, error) {
	record, err := e.loadPreparation(ctx, reference)
	if err != nil {
		return rpc.TradeProposalPreparedStatusResult{AsOf: e.clock(), Blockers: preparedBlocker("prepared_reference_unavailable", "The prepared proposal reference cannot be resolved safely.")}, nil
	}
	return e.preparedStatus(ctx, record), nil
}

func (e *proposalEngine) preparedStatus(ctx context.Context, record preparedProposalRecord) rpc.TradeProposalPreparedStatusResult {
	out := rpc.TradeProposalPreparedStatusResult{AsOf: e.clock()}
	if !e.preparationScopeMatches(record) {
		out.Blockers = preparedBlocker("prepared_scope_mismatch", "The prepared proposal belongs to a different broker scope.")
		return out
	}
	meta := record.Preparation
	meta.State, meta.Consumed = "unavailable", nil
	out.Preparation = &meta
	store, err := e.preparationAuthority()
	if err != nil {
		out.Blockers = preparedBlocker("journal_authority_unavailable", "Order consumption cannot be established; do not retry submission.")
		return out
	}
	consumed, err := store.PreviewTokenConsumed(ctx, corestore.HashPreviewTokenID(record.Preview.PreviewTokenID))
	if err != nil {
		out.Blockers = preparedBlocker("journal_authority_unavailable", "Order consumption cannot be established; do not retry submission.")
		return out
	}
	meta.Consumed = new(consumed)
	if !consumed {
		meta.State = "prepared"
		if !e.clock().Before(meta.ExpiresAt) {
			meta.State = "expired"
		}
		return out
	}
	meta.State = "consumed"
	views, eventsByKey, err := e.server.loadOrderViews()
	if err != nil {
		out.Blockers = preparedBlocker("order_receipt_unavailable", "The reference is consumed but its order receipt cannot currently be read; do not resend.")
		return out
	}
	p := record.Preview
	scope := brokerStateScope{Account: p.Account, Mode: p.Mode}
	out.Order = &rpc.OrderStatusResult{AsOf: e.clock(), Account: p.Account, Mode: p.Mode, NotBrokerStatement: orderHistoryNotBrokerStatement(), Limitations: orderHistoryLimitations()}
	for _, view := range views {
		if view.OrderRef != p.Draft.OrderRef || !orderViewMatchesBrokerScope(view, scope) || view.Endpoint != p.Endpoint || view.ClientID != p.ClientID {
			continue
		}
		events := append([]rpc.OrderEvent(nil), eventsByKey[orderViewKey(view)]...)
		out.Order.Found, out.Order.Order, out.Order.Events = true, view, events
		out.Order.LastLocalEventAt = latestOrderEventAt(events)
		break
	}
	return out
}

// submitPrepared runs inside the same brokerWriteMu as ordinary proposal
// submit. The order journal is the durable single-winner boundary; no second
// execution ledger or fresh preview can replace the reviewed draft.
func (e *proposalEngine) submitPrepared(ctx context.Context, p rpc.TradeProposalSubmitParams) (rpc.TradeProposalSubmitResult, error) {
	out := rpc.TradeProposalSubmitResult{AsOf: e.clock()}
	record, err := e.loadPreparation(ctx, p.PreparedRef)
	if err != nil {
		out.Blockers = preparedBlocker("prepared_reference_unavailable", "The prepared proposal reference cannot be resolved safely.")
		return out, nil
	}
	if p.Key != record.Preparation.Key || p.Revision != record.Preparation.Revision || p.Quantity != 0 {
		out.Blockers = preparedBlocker("prepared_binding_mismatch", "Key and revision must match the prepared proposal, without a quantity override.")
		return out, nil
	}
	receipt := e.preparedStatus(ctx, record)
	out.Preparation, out.Order, out.Blockers = receipt.Preparation, receipt.Order, receipt.Blockers
	if receipt.Preparation != nil {
		out.Proposal, out.Preview, out.PreviewTokenID = record.Proposal, sanitizeProposalPreviewForProposal(&record.Preview, record.Proposal), record.Preview.PreviewTokenID
	}
	if receipt.Preparation == nil || len(receipt.Blockers) > 0 {
		return out, nil
	}
	if receipt.Preparation.State == "consumed" {
		out.Blockers = preparedBlocker("prepared_reference_consumed", "This reference is already consumed; inspect its order receipt instead of resending.")
		return out, nil
	}
	if receipt.Preparation.State == "expired" {
		out.Blockers = preparedBlocker("prepared_reference_expired", "Prepare a new preview and obtain a new confirmation.")
		return out, nil
	}
	if receipt.Preparation.State != "prepared" {
		out.Blockers = preparedBlocker("prepared_reference_unavailable", "The prepared proposal is unavailable.")
		return out, nil
	}
	out.Proposal, out.Preview, out.PreviewTokenID = record.Proposal, sanitizeProposalPreviewForProposal(&record.Preview, record.Proposal), record.Preview.PreviewTokenID
	if !e.server.cfg.AutoTrade.WithDefaults().FastPathEnabledResolved() || !p.FastPath {
		out.Blockers = preparedBlocker("fast_path_disabled", "Proposal submit remains disabled by its existing configuration or request gate.")
		return out, nil
	}
	if blockers := e.server.proposalSubmitWriteBlockers(p.Origin); len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	prop, blockers, err := e.resolveProposal(ctx, p.Key, p.Revision)
	if err != nil || len(blockers) > 0 {
		out.Blockers = blockers
		return out, err
	}
	if prop.Key != record.Preparation.Key || prop.Revision != record.Preparation.Revision {
		out.Blockers = preparedBlocker("stale_revision", "The current proposal no longer matches the prepared revision.")
		return out, nil
	}
	originalTerms, originalErr := json.Marshal(proposalOrderPreviewParams(record.Proposal, record.Preview.Draft.Quantity, 0))
	currentTerms, currentErr := json.Marshal(proposalOrderPreviewParams(prop, record.Preview.Draft.Quantity, 0))
	if originalErr != nil || currentErr != nil || !bytes.Equal(originalTerms, currentTerms) || prop.Quantity != record.Proposal.Quantity {
		out.Blockers = preparedBlocker("prepared_proposal_changed", "The current proposal terms differ from the reviewed proposal; prepare and confirm again.")
		return out, nil
	}
	for _, check := range [][]rpc.TradingBlocker{shadowProposalBlockers(prop), unitProposalOrderBlockers(prop), proposalPreviewSafetyBlockers(prop, &record.Preview)} {
		if len(check) > 0 {
			out.Blockers = check
			return out, nil
		}
	}
	if blockers := e.duplicateProtectiveBlockers(ctx, prop); len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	if blockers := e.checkOptionExitEconomics(ctx, prop, &record.Preview, false); len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	payload, err := e.server.verifyPreviewTokenForPlace(record.Preview.PreviewToken)
	if err != nil {
		out.Blockers = preparedBlocker("prepared_preview_invalid", "The original preview no longer passes its token, scope or eligibility gates.")
		return out, nil
	}
	fingerprint, err := preparedDraftFingerprint(payload.Draft)
	if err != nil || payload.TokenID != record.Preview.PreviewTokenID || fingerprint != record.Preparation.DraftFingerprint {
		out.Blockers = preparedBlocker("prepared_binding_mismatch", "The retained token does not describe the reviewed draft.")
		return out, nil
	}
	place, placeErr := e.server.proposalPlaceOrder(ctx, rpc.OrderPlaceParams{PreviewToken: record.Preview.PreviewToken, TimeoutMs: p.TimeoutMs, Origin: p.Origin})
	receipt = e.preparedStatus(ctx, record)
	out.Preparation, out.Order, out.Blockers, out.AsOf = receipt.Preparation, receipt.Order, receipt.Blockers, e.clock()
	if placeErr != nil {
		out.Blockers = append(out.Blockers, rpc.TradingBlocker{Code: "submit_failed", Message: "Submission did not return a confirmed result; inspect the durable preparation/order receipt before any further action."})
		return out, nil
	}
	if place == nil {
		out.Blockers = append(out.Blockers, rpc.TradingBlocker{Code: "submit_unavailable", Message: "Submission returned no result; inspect the durable receipt."})
		return out, nil
	}
	out.Accepted, out.Place, out.OrderRef, out.Message = place.Accepted, place, place.OrderRef, place.Message
	e.appendEvent(proposalEventForProposal("submitted", prop, e.clock(), record.Preview.PreviewTokenID, place.OrderRef, "prepared proposal submitted through its original preview"))
	if e.server.proposalOutcomes != nil {
		if err := e.server.proposalOutcomes.AppendMark(proposalOutcomeSubmitted(prop, &record.Preview, place, e.clock())); err != nil {
			e.server.warnf("trade proposal outcomes: append prepared submitted mark: %v", err)
		}
	}
	return out, nil
}

func (s *Server) handleTradeProposalsPrepare(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalPrepareResult, error) {
	var p rpc.TradeProposalPreviewParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalPrepareResult{AsOf: s.orderNow(), Blockers: preparedBlocker("proposal_engine_unavailable", "Proposal engine is unavailable.")}, nil
	}
	out, err := s.tradeProposals.Prepare(ctx, p)
	return &out, err
}

func (s *Server) handleTradeProposalsPreparedStatus(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalPreparedStatusResult, error) {
	var p rpc.TradeProposalPreparedStatusParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalPreparedStatusResult{AsOf: s.orderNow(), Blockers: preparedBlocker("proposal_engine_unavailable", "Proposal engine is unavailable.")}, nil
	}
	out, err := s.tradeProposals.PreparedStatus(ctx, p.PreparedRef)
	return &out, err
}
