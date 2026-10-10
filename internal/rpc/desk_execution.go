package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Desk execution is the one daemon path through which Desk sends several
// orders: a manual batch the owner confirms once on a device, and the
// automatic controller acting under its standing mandate (DeskAuthority*).
// The two authorisations stay distinct inputs; the items, the admission
// gates, shared capacity, request deduplication, episode consumption and
// receipts are the same machinery. Every item is sent on its own: a refusal
// never holds the others, and nothing is all-or-nothing.
//
// Contract only (2026-10-10): no handler dispatches these methods yet. A
// daemon that does not list them in DeskExecutionCapabilities is unavailable
// to Desk, never a silent fallback to another write path.
const (
	MethodDeskExecutionCapabilities = "desk.execution.capabilities"
	MethodDeskExecutionPrepareBatch = "desk.execution.prepare_batch"
	MethodDeskExecutionSubmit       = "desk.execution.submit"
	MethodDeskExecutionLookup       = "desk.execution.lookup"
)

// DeskExecutionContractVersion names this contract; a caller refuses a
// daemon that reports another version.
const DeskExecutionContractVersion = 1

// Item classes. Canary classifies the exact order itself at admission; an
// item whose order Canary classifies differently from its class is refused
// (DeskBlockerClassMismatch), never re-labelled.
const (
	DeskClassProtect = "protect"
	DeskClassReduce  = "reduce"
	DeskClassAdd     = "add"
)

// Item outcomes. Accepted means a known broker order exists (working, partly
// filled or filled) under OrderRef. Refused is definitive: Canary's own
// records establish that nothing reached the broker for this request.
// Unknown means the request may have reached the broker and nothing proves
// either way; it is resolved only by Lookup, never by a retry under a new
// request ID. Absent is a Lookup answer only: Canary's durable request
// journal, written before any dispatch, holds no record of this request ID.
// A lookup that cannot read that journal returns an error, never Absent.
const (
	DeskOutcomeAccepted = "accepted"
	DeskOutcomeRefused  = "refused"
	DeskOutcomeUnknown  = "unknown"
	DeskOutcomeAbsent   = "absent"
)

// Typed blocker codes a Desk adapter may branch on. Other TradingBlocker
// codes keep their existing meaning; an unrecognised code is a refusal.
const (
	// DeskBlockerDrawdownBrake is the hard drawdown brake refusing a
	// risk-adding order (the existing order-path code).
	DeskBlockerDrawdownBrake = "drawdown_brake"
	// DeskBlockerEpisodeConsumed: an earlier request of this episode was
	// accepted or is unknown; an episode yields at most one order.
	DeskBlockerEpisodeConsumed = "episode_consumed"
	// DeskBlockerRequestConflict: the request ID was used before with a
	// different intent digest. Nothing is sent.
	DeskBlockerRequestConflict = "request_conflict"
	// DeskBlockerAuthorityChanged: the mandate's generation, scope,
	// preferences or running state no longer matches, checked at the wire.
	DeskBlockerAuthorityChanged = "authority_changed"
	// DeskBlockerClassMismatch: Canary classifies the exact order as a
	// different class than the item claims.
	DeskBlockerClassMismatch = "class_mismatch"
	// DeskBlockerPriorUnknown: an earlier item of the same submission ended
	// unknown, so later additions are not sent; protection and reductions
	// still are.
	DeskBlockerPriorUnknown = "prior_outcome_unknown"
	// DeskBlockerProposalChanged: the named proposal revision is no longer
	// current.
	DeskBlockerProposalChanged = "proposal_changed"
)

// DeskExecutionCapabilitiesResult is what this daemon serves. Methods lists
// the dispatched desk.execution.* methods; Authorities lists "batch" and/or
// "automatic".
type DeskExecutionCapabilitiesResult struct {
	Version     int       `json:"version"`
	Methods     []string  `json:"methods"`
	Authorities []string  `json:"authorities"`
	AsOf        time.Time `json:"as_of"`
}

// DeskExecutionIntent is one order's exact intent. Exactly one source is set:
// Proposal for an existing Canary protection or reduction proposal, Add for an
// automatic stock entry. Canary sizes, prices and gates the order at
// submission; the intent never carries a quantity or a price.
type DeskExecutionIntent struct {
	Class    string                 `json:"class"`
	Proposal *DeskExecutionProposal `json:"proposal,omitempty"`
	Add      *DeskExecutionAdd      `json:"add,omitempty"`
}

// DeskExecutionProposal names one served proposal revision.
type DeskExecutionProposal struct {
	Key      string `json:"key"`
	Revision string `json:"revision"`
}

// DeskExecutionAdd is an automatic stock entry: the exact contract and the
// Desk rule revision and evidence that qualified it. Canary sizes it with
// add.plan at maximum under existing policy, at a submission-time midpoint
// limit, IBKR Adaptive, DAY. RuleID, RuleRevision and EvidenceID are audit
// identity; Canary does not establish the signal.
type DeskExecutionAdd struct {
	ConID        int    `json:"con_id"`
	Symbol       string `json:"symbol"`
	Currency     string `json:"currency"`
	RuleID       string `json:"rule_id"`
	RuleRevision uint64 `json:"rule_revision"`
	EvidenceID   string `json:"evidence_id"`
}

// DeskExecutionItem is one order request. RequestID is the caller's stable
// idempotency key (at most 128 bytes): the same ID with the same intent
// returns the original receipt and never a second order; the same ID with a
// different intent is refused (DeskBlockerRequestConflict). EpisodeID is the
// consumption key: once an item of an episode is accepted or unknown, every
// later request of that episode is refused (DeskBlockerEpisodeConsumed),
// including after partial fills or a cancelled remainder.
type DeskExecutionItem struct {
	RequestID string              `json:"request_id"`
	EpisodeID string              `json:"episode_id"`
	Intent    DeskExecutionIntent `json:"intent"`
}

// DeskExecutionPrepareBatchParams names the proposals the owner selected for
// one manual batch, by key and served revision.
type DeskExecutionPrepareBatchParams struct {
	Proposals []DeskExecutionProposal `json:"proposals"`
	TimeoutMs int                     `json:"timeout_ms,omitempty"`
}

// DeskExecutionBatchTerms are what the owner confirms on the device: the
// account scope and every item in send order, each with its previewed order.
// Send order is reductions, then protection, then additions (owner decision
// for manual batches, 2026-10-09 19:20 CEST); within a class, the order the
// proposals were named. The terms are compact JSON in this field order;
// TermsDigest is "sha256:" and the hex SHA-256 of those exact bytes.
type DeskExecutionBatchTerms struct {
	Kind        string                   `json:"kind"`
	Version     int                      `json:"version"`
	BatchID     string                   `json:"batch_id"`
	AccountID   string                   `json:"account_id"`
	AccountMode string                   `json:"account_mode"`
	ExpiresAt   time.Time                `json:"expires_at"`
	Items       []DeskExecutionBatchItem `json:"items"`
}

// DeskExecutionBatchTermsKind names DeskExecutionBatchTerms.
const DeskExecutionBatchTermsKind = "canary.desk_batch"

// DeskExecutionBatchItem is one confirmed item: its daemon-assigned request
// and episode identity, its intent and the order it previewed to.
type DeskExecutionBatchItem struct {
	DeskExecutionItem
	Draft OrderDraft `json:"draft"`
}

// DeskExecutionPrepareBatchResult carries the terms and a private BatchRef.
// BatchRef authorises one submission, and like PreparedRef must never reach
// a browser, a log or argv. Refused lists the named proposals that could not
// be prepared, with their blockers; they are not in the terms and do not
// hold the others back. BatchRef is empty when no item was prepared.
type DeskExecutionPrepareBatchResult struct {
	BatchRef    string                 `json:"batch_ref,omitempty"`
	Terms       string                 `json:"terms,omitempty"`
	TermsDigest string                 `json:"terms_digest,omitempty"`
	Refused     []DeskExecutionReceipt `json:"refused,omitempty"`
	AsOf        time.Time              `json:"as_of"`
}

// DeskBatchAuthorization is the one-use manual authorisation: the prepared
// batch and the owner's device confirmation of its exact terms. Canary
// verifies the signature against the pinned device keys in its own domain,
// distinct from cash settings, single orders and the standing mandate.
type DeskBatchAuthorization struct {
	BatchRef     string                 `json:"batch_ref"`
	TermsDigest  string                 `json:"terms_digest"`
	Confirmation CashPolicyConfirmation `json:"confirmation"`
}

// DeskAutomaticAuthorization is the standing mandate as the controller holds
// it: the terms ID, its private capability (never browser, model or argv
// output) and the daemon generation and preferences hash it reconciled
// against. Canary rechecks all four under the wire lock before each item's
// first byte; locks and rule withdrawals change the preferences hash.
type DeskAutomaticAuthorization struct {
	ID                 string `json:"id"`
	Capability         string `json:"capability"`
	ExpectedGeneration int64  `json:"expected_generation"`
	PreferencesHash    string `json:"preferences_hash"`
}

// DeskExecutionSubmitParams sends items under exactly one authorisation.
// Batch submits the prepared batch's items in its terms' order and carries
// no Items. Automatic carries Items in the controller's selection order;
// Canary refuses an addition listed before a protection or reduction of the
// same submission and otherwise keeps that order. Retrying the same
// submission returns the original receipts.
type DeskExecutionSubmitParams struct {
	Batch     *DeskBatchAuthorization     `json:"batch,omitempty"`
	Automatic *DeskAutomaticAuthorization `json:"automatic,omitempty"`
	Items     []DeskExecutionItem         `json:"items,omitempty"`
	TimeoutMs int                         `json:"timeout_ms,omitempty"`
}

// DeskExecutionReceipt is one item's result. Digest is DeskIntentDigest of
// the item; a receipt whose RequestID or Digest differs from the request is
// not that request's receipt. Blockers carry typed codes on refusal and on
// unknown. Order is Canary's local order receipt when one exists; it is not
// a broker statement.
type DeskExecutionReceipt struct {
	RequestID string             `json:"request_id"`
	EpisodeID string             `json:"episode_id,omitempty"`
	Digest    string             `json:"digest"`
	Class     string             `json:"class"`
	Outcome   string             `json:"outcome"`
	OrderRef  string             `json:"order_ref,omitempty"`
	Order     *OrderStatusResult `json:"order,omitempty"`
	Blockers  []TradingBlocker   `json:"blockers,omitempty"`
	Message   string             `json:"message,omitempty"`
	AsOf      time.Time          `json:"as_of"`
}

// DeskExecutionSubmitResult reports every item in send order.
type DeskExecutionSubmitResult struct {
	BatchID  string                 `json:"batch_id,omitempty"`
	Receipts []DeskExecutionReceipt `json:"receipts"`
	AsOf     time.Time              `json:"as_of"`
}

// DeskExecutionLookupParams names requests by ID. Lookup is read-only: it
// never prepares, sends or retries.
type DeskExecutionLookupParams struct {
	RequestIDs []string `json:"request_ids"`
}

// DeskExecutionLookupResult answers every named request, in the order named.
type DeskExecutionLookupResult struct {
	Receipts []DeskExecutionReceipt `json:"receipts"`
	AsOf     time.Time              `json:"as_of"`
}

// DeskIntentDigest is "sha256:" and the hex SHA-256 of the item's compact
// JSON (request ID, episode ID and intent), the binding between a request
// and its receipt on both sides.
func DeskIntentDigest(item DeskExecutionItem) string {
	raw, _ := json.Marshal(item)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Validate refuses an item a daemon must never act on: missing identity, an
// unknown class, not exactly one source, or a source that does not fit the
// class (proposals are protection or reductions; Add is a stock entry).
func (it DeskExecutionItem) Validate() error {
	if it.RequestID == "" || len(it.RequestID) > 128 || strings.TrimSpace(it.RequestID) != it.RequestID ||
		it.EpisodeID == "" || len(it.EpisodeID) > 128 {
		return fmt.Errorf("desk execution item needs a request ID and an episode ID of at most 128 bytes")
	}
	in := it.Intent
	switch {
	case (in.Proposal == nil) == (in.Add == nil):
		return fmt.Errorf("desk execution item %s needs exactly one source", it.RequestID)
	case in.Proposal != nil && (in.Class != DeskClassProtect && in.Class != DeskClassReduce || in.Proposal.Key == "" || in.Proposal.Revision == ""):
		return fmt.Errorf("desk execution item %s: a proposal is a protection or reduction with key and revision", it.RequestID)
	case in.Add != nil && (in.Class != DeskClassAdd || in.Add.ConID <= 0 || in.Add.Symbol == "" || in.Add.Currency == "" ||
		in.Add.RuleID == "" || in.Add.RuleRevision == 0 || in.Add.EvidenceID == ""):
		return fmt.Errorf("desk execution item %s: an addition needs the exact stock, rule revision and evidence", it.RequestID)
	}
	return nil
}

// Validate refuses a receipt that does not answer item under the outcome
// rules: Accepted needs an order reference, Refused and Unknown carry none and
// say why, Absent appears only on lookup.
func (r DeskExecutionReceipt) Validate(item DeskExecutionItem, lookup bool) error {
	if r.RequestID != item.RequestID || r.Digest != DeskIntentDigest(item) {
		return fmt.Errorf("receipt does not answer request %s", item.RequestID)
	}
	ok := false
	switch r.Outcome {
	case DeskOutcomeAccepted:
		ok = r.OrderRef != ""
	case DeskOutcomeRefused, DeskOutcomeUnknown:
		ok = r.OrderRef == "" && (len(r.Blockers) > 0 || r.Message != "")
	case DeskOutcomeAbsent:
		ok = lookup && r.OrderRef == ""
	}
	if !ok {
		return fmt.Errorf("receipt for %s has an invalid %q outcome", item.RequestID, r.Outcome)
	}
	return nil
}

// ValidateAutomaticOrder refuses an automatic submission that lists an
// addition before a protection or reduction. It sets no order between
// protection and reductions.
func ValidateAutomaticOrder(items []DeskExecutionItem) error {
	firstAdd := slices.IndexFunc(items, func(it DeskExecutionItem) bool { return it.Intent.Class == DeskClassAdd })
	if firstAdd >= 0 && slices.ContainsFunc(items[firstAdd:], func(it DeskExecutionItem) bool { return it.Intent.Class != DeskClassAdd }) {
		return fmt.Errorf("an addition is listed before a protection or reduction")
	}
	return nil
}
