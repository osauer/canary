package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
// behind one private reference, with the exact terms the owner confirms and
// their digest. submit_bundle is allowed once per reference: it refuses terms
// other than the retained ones, records the submission, runs every
// conversion's full prepared-submit check first, with nothing sent, then
// sends them in order, cheapest payer first, and stops at the first
// conversion that is not sent. Every conversion is reported in send order
// with what became of it: sent, refused (Canary refused it, so it did not
// reach the broker), not_sent (never attempted) or unknown (it may have
// reached the broker). A partly sent bundle is safe: each conversion stays
// within its own allotment and share of the target, nothing resends the
// rest, and the next cycle plans what remains once the ledger shows the
// fills. prepared_bundle_status reads the same outcomes back from Canary's
// records. The bundle never widens a gate: each conversion is still one
// order under every check a single prepared submit runs.

const (
	preparedBundleKind   = "prepared_leveling_bundle_v1"
	preparedBundlePrefix = "canarypb1"
	// preparedBundleVersion 2 carries the exact terms and the one
	// submission; a version 1 record, retained before them, is never sent.
	preparedBundleVersion = 2
	// bundleConfirmationMaxBytes bounds the owner's confirmation Canary keeps
	// for audit.
	bundleConfirmationMaxBytes = 16 << 10
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
	// Terms are the exact terms the owner confirms (rpc.LevelingBundleTerms)
	// and TermsDigest their digest.
	Terms       string `json:"terms"`
	TermsDigest string `json:"terms_digest"`
	// SubmittedAt marks the one submission a reference allows, recorded
	// before any conversion is checked again; Origin and Confirmation are
	// what that submission carried, the confirmation kept for audit only.
	SubmittedAt  time.Time                            `json:"submitted_at,omitzero"`
	Origin       string                               `json:"origin,omitempty"`
	Confirmation *rpc.TradeProposalBundleConfirmation `json:"confirmation,omitempty"`
	// FinishedAt and Outcomes record what became of each conversion, in send
	// order, once the submission finished.
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Outcomes   []string  `json:"outcomes,omitempty"`
}

// servedLevelingBundle refreshes and returns the served bundle id at
// revision, its rows in send order and the account's base currency.
func (e *proposalEngine) servedLevelingBundle(ctx context.Context, id, revision string) (rpc.TradeProposalCurrencyLevelingBundle, []rpc.TradeProposal, string, []rpc.TradingBlocker, error) {
	var none rpc.TradeProposalCurrencyLevelingBundle
	id, revision = strings.TrimSpace(id), strings.TrimSpace(revision)
	if id == "" || revision == "" {
		return none, nil, "", preparedBlocker("bad_request", "bundle id and revision are required"), nil
	}
	snap, err := e.bundleSnapshot(ctx)
	if err != nil && len(snap.Proposals) == 0 {
		return none, nil, "", snap.Blockers, err
	}
	if len(snap.Blockers) > 0 && len(snap.Proposals) == 0 {
		return none, nil, "", snap.Blockers, nil
	}
	if len(snap.AutoTrade.Blockers) > 0 {
		return none, nil, "", snap.AutoTrade.Blockers, nil
	}
	var bundle *rpc.TradeProposalCurrencyLevelingBundle
	base := ""
	if st := snap.CurrencyLeveling; st != nil {
		base = st.BaseCurrency
		for i := range st.Bundles {
			if st.Bundles[i].ID == id {
				bundle = &st.Bundles[i]
			}
		}
	}
	if bundle == nil {
		return none, nil, "", preparedBlocker("bundle_not_found", "The repayment is not in the current proposals; refresh them."), nil
	}
	if bundle.Revision != revision {
		return none, nil, "", preparedBlocker("stale_revision", "The repayment changed; refresh proposals before preparing or sending it."), nil
	}
	rows := make([]rpc.TradeProposal, 0, len(bundle.Keys))
	for _, key := range bundle.Keys {
		row, ok := servedProposal(snap, key)
		if !ok {
			return none, nil, "", preparedBlocker("bundle_not_found", "A conversion of the repayment is not in the current proposals; refresh them."), nil
		}
		rows = append(rows, row)
	}
	return *bundle, rows, base, nil, nil
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
// places an order. The private reference, the exact terms and their digest
// are returned only when every conversion is prepared.
func (e *proposalEngine) PrepareBundle(ctx context.Context, p rpc.TradeProposalPrepareBundleParams) (rpc.TradeProposalPrepareBundleResult, error) {
	out := rpc.TradeProposalPrepareBundleResult{BundleID: p.BundleID, Revision: p.Revision, Legs: []rpc.TradeProposalPreviewResult{}, AsOf: e.clock()}
	bundle, rows, base, blockers, err := e.servedLevelingBundle(ctx, p.BundleID, p.Revision)
	if err != nil || len(blockers) > 0 {
		out.Blockers = blockers
		return out, err
	}
	prepared := make([]rpc.TradeProposalPrepareResult, 0, len(rows))
	for i, row := range rows {
		res, err := e.prepare(ctx, rpc.TradeProposalPreviewParams{Key: row.Key, Revision: row.Revision, TimeoutMs: p.TimeoutMs}, nil)
		out.Legs = append(out.Legs, res.TradeProposalPreviewResult)
		if err != nil || !res.Accepted || res.PreparedRef == "" || res.Preparation == nil {
			out.Blockers = []rpc.TradingBlocker{{Code: "bundle_conversion_not_prepared",
				Message: fmt.Sprintf("conversion %d of %d could not be prepared, so the repayment cannot be approved; its own blockers say why", i+1, len(rows)),
				Action:  "Refresh proposals and prepare the repayment again."}}
			return out, err
		}
		prepared = append(prepared, res)
	}
	id, err := randomTokenID()
	if err != nil {
		return out, err
	}
	secret, err := randomTokenID()
	if err != nil {
		return out, err
	}
	terms, err := levelingBundleTerms(id, bundle, base, prepared)
	if err != nil {
		out.Blockers = preparedBlocker("bundle_terms_unavailable", "The repayment's exact terms cannot be stated ("+err.Error()+"), so it cannot be approved.")
		return out, nil
	}
	raw, err := json.Marshal(terms)
	if err != nil {
		return out, err
	}
	reference := preparedBundlePrefix + "." + id + "." + secret
	record := preparedBundleRecord{Version: preparedBundleVersion, ReferenceHash: sha256.Sum256([]byte(reference)), ID: id, BundleID: bundle.ID, Revision: bundle.Revision,
		ExpiresAt: terms.ExpiresAt, Terms: string(raw), TermsDigest: levelingBundleDigest(raw)}
	for _, leg := range terms.Legs {
		record.Keys, record.Preparations = append(record.Keys, leg.Key), append(record.Preparations, leg.PreparationID)
	}
	if _, err := e.saveBundle(ctx, record, 0); err != nil {
		out.Blockers = preparedBlocker("preparation_not_persisted", "The prepared repayment could not be retained; nothing can be sent from it.")
		return out, err
	}
	out.Accepted, out.BundleRef, out.ExpiresAt, out.AsOf = true, reference, record.ExpiresAt, e.clock()
	out.PreparationID, out.Terms, out.TermsDigest = record.ID, record.Terms, record.TermsDigest
	return out, nil
}

// levelingBundleDigest is the digest of the exact terms bytes.
func levelingBundleDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// levelingBundleTerms states a prepared bundle's exact terms from its served
// entry and its conversions' preparations: the broker scope, the loan, each
// conversion's identity and order, and the figures at each price limit
// (pays, receives, what the payer keeps, where the loan lands). Every figure
// is copied from Canary's rows and previews or bounded from them, rounded
// against the owner: an amount paid up, an amount received or kept down.
func levelingBundleTerms(id string, bundle rpc.TradeProposalCurrencyLevelingBundle, base string, legs []rpc.TradeProposalPrepareResult) (rpc.LevelingBundleTerms, error) {
	var t rpc.LevelingBundleTerms
	if len(legs) == 0 || legs[0].Preview == nil || legs[0].Proposal.CurrencyLeveling == nil {
		return t, errors.New("no conversion")
	}
	scope, loan := legs[0].Preview, legs[0].Proposal.CurrencyLeveling
	t = rpc.LevelingBundleTerms{Kind: rpc.LevelingBundleTermsKind, Version: rpc.LevelingBundleTermsVersion,
		AccountID: scope.Account, AccountMode: scope.Mode, Endpoint: scope.Endpoint, ClientID: scope.ClientID,
		BundleID: bundle.ID, Revision: bundle.Revision, PreparationID: id, BaseCurrency: base,
		Currency: loan.Currency, Cash: loan.Cash, CashBase: levelingCents(loan.Cash * loan.ExchangeRate),
		LoanRate: loan.LoanRate, LoanRateThrough: loan.LoanRateThrough, LoanRateBound: loan.LoanRateBound,
		TriggerBase: loan.TriggerBase, CushionBase: loan.CushionBase, OrderCapBase: loan.OrderCapBase,
		SavingBase: bundle.SavingBase, CostBase: bundle.CostBase, PaybackDays: bundle.PaybackDays,
		HeldToCap: loan.HeldToCap, FundingShort: loan.FundingShort, Legs: make([]rpc.LevelingBundleTermsLeg, 0, len(legs))}
	if base == "" || loan.ExchangeRate <= 0 {
		return t, errors.New("the loan's base currency or ledger rate is unknown")
	}
	t.Cushion = levelingCents(loan.CushionBase / loan.ExchangeRate)
	lands := loan.Cash
	var expires time.Time
	for i, leg := range legs {
		pv, b, prep := leg.Preview, leg.Proposal.CurrencyLeveling, leg.Preparation
		if pv == nil || b == nil || prep == nil {
			return t, fmt.Errorf("conversion %d has no preparation", i+1)
		}
		d := pv.Draft
		switch {
		case pv.Account != t.AccountID || pv.Mode != t.AccountMode || pv.Endpoint != t.Endpoint || pv.ClientID != t.ClientID:
			return t, errors.New("the conversions name different broker scopes")
		case b.BundleID != bundle.ID || b.Currency != t.Currency || b.Cash != t.Cash:
			return t, fmt.Errorf("conversion %d repays another loan", i+1)
		case leg.Proposal.Key != bundle.Keys[i]:
			return t, fmt.Errorf("conversion %d is out of send order", i+1)
		case d.FX == nil || d.FX.Bid == nil || d.FX.Ask == nil:
			return t, fmt.Errorf("conversion %d carries no live bid and ask", i+1)
		case d.Quantity <= 0 || d.LimitPrice <= 0:
			return t, fmt.Errorf("conversion %d has no order", i+1)
		}
		pays, receives, err := levelingBundleAmounts(d, b)
		if err != nil {
			return t, fmt.Errorf("conversion %d: %w", i+1, err)
		}
		lands += receives.Amount
		quoteAt := pv.Quote.PriceAt
		if quoteAt.IsZero() {
			quoteAt = pv.Quote.AsOf
		}
		t.Legs = append(t.Legs, rpc.LevelingBundleTermsLeg{Leg: i + 1, Key: leg.Proposal.Key, Revision: leg.Proposal.Revision,
			PreparationID: prep.ID, DraftFingerprint: prep.DraftFingerprint, PreviewTokenID: pv.PreviewTokenID, OrderRef: d.OrderRef,
			Pair: b.Pair, ConID: d.Contract.ConID, Exchange: d.Contract.Exchange, Action: d.Action, Quantity: d.Quantity,
			OrderType: d.OrderType, TIF: d.TIF, LimitPrice: d.LimitPrice, Bid: *d.FX.Bid, Ask: *d.FX.Ask, QuoteAt: quoteAt.UTC().Truncate(time.Second),
			MaxSlippageBP: b.MaxSlippageBP, Currency: b.Currency, Target: b.Target, FundingCurrency: b.FundingCurrency, Allotment: b.Allotment,
			KeepsAtLeast: levelingCentsDown(b.FundingCash - b.FundingCommitted - pays.Amount),
			FundingRate:  b.FundingRate, FundingRateThrough: b.FundingRateThrough, FundingRateBound: b.FundingRateBound,
			Pays: pays, Receives: receives, SavingBase: b.SavingBase, CostBase: b.CostBase})
		if expires.IsZero() || prep.ExpiresAt.Before(expires) {
			expires = prep.ExpiresAt
		}
	}
	t.LandsAtLeast = levelingCentsDown(lands)
	t.ExpiresAt = expires.UTC().Truncate(time.Second)
	return t, nil
}

// levelingBundleAmounts is what a conversion pays and receives at its price
// limit. A BUY of the borrowed currency receives its quantity exactly and
// pays at most quantity × limit of the payer; a SELL of the payer pays its
// quantity exactly and receives at least quantity × limit of the borrowed
// currency.
func levelingBundleAmounts(d rpc.OrderDraft, b *rpc.TradeProposalCurrencyLeveling) (pays, receives rpc.LevelingBundleAmount, err error) {
	q, at := float64(d.Quantity), float64(d.Quantity)*d.LimitPrice
	switch {
	case d.Action == rpc.OrderActionBuy && d.Contract.Symbol == b.Currency && d.Contract.Currency == b.FundingCurrency:
		return rpc.LevelingBundleAmount{Amount: levelingCentsUp(at), Currency: b.FundingCurrency, Bound: rpc.LevelingBundleBoundAtMost},
			rpc.LevelingBundleAmount{Amount: q, Currency: b.Currency, Bound: rpc.LevelingBundleBoundExact}, nil
	case d.Action == rpc.OrderActionSell && d.Contract.Symbol == b.FundingCurrency && d.Contract.Currency == b.Currency:
		return rpc.LevelingBundleAmount{Amount: q, Currency: b.FundingCurrency, Bound: rpc.LevelingBundleBoundExact},
			rpc.LevelingBundleAmount{Amount: levelingCentsDown(at), Currency: b.Currency, Bound: rpc.LevelingBundleBoundAtLeast}, nil
	}
	return pays, receives, fmt.Errorf("%s %s is not a conversion of %s into %s", d.Action, d.Contract.LocalSymbol, b.FundingCurrency, b.Currency)
}

// levelingCents rounds an amount to cents; levelingCentsUp and
// levelingCentsDown round against the owner, so an amount paid at most never
// reads lower and one received or kept at least never reads higher.
func levelingCents(v float64) float64     { return math.Round(v*100) / 100 }
func levelingCentsUp(v float64) float64   { return math.Ceil(v*100-1e-6) / 100 }
func levelingCentsDown(v float64) float64 { return math.Floor(v*100+1e-6) / 100 }

// saveBundle writes the record at expected, its current revision (zero
// creates it), and returns the saved document.
func (e *proposalEngine) saveBundle(ctx context.Context, record preparedBundleRecord, expected int64) (corestore.StateDocument, error) {
	store, err := e.preparationAuthority()
	if err != nil {
		return corestore.StateDocument{}, err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return corestore.StateDocument{}, err
	}
	return store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: "proposal-bundle:" + record.ID, Kind: preparedBundleKind, ExpectedRevision: expected, JSON: raw})
}

// loadBundle resolves a private reference to its record and the record's
// current revision.
func (e *proposalEngine) loadBundle(ctx context.Context, reference string) (preparedBundleRecord, int64, error) {
	var record preparedBundleRecord
	parts := strings.Split(reference, ".")
	if len(parts) != 3 || parts[0] != preparedBundlePrefix {
		return record, 0, fmt.Errorf("invalid prepared bundle reference")
	}
	for _, part := range parts[1:] {
		raw, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || len(raw) != 16 || base64.RawURLEncoding.EncodeToString(raw) != part {
			return record, 0, fmt.Errorf("invalid prepared bundle reference")
		}
	}
	store, err := e.preparationAuthority()
	if err != nil {
		return record, 0, err
	}
	doc, found, err := store.GetStateDocument(ctx, "proposal-bundle:"+parts[1], preparedBundleKind)
	switch {
	case err != nil:
		return record, 0, err
	case !found:
		return record, 0, fmt.Errorf("prepared bundle reference is unavailable")
	}
	if err := json.Unmarshal(doc.JSON, &record); err != nil {
		return preparedBundleRecord{}, 0, fmt.Errorf("prepared bundle record is unreadable")
	}
	digest := sha256.Sum256([]byte(reference))
	if record.Version != preparedBundleVersion || record.ID != parts[1] || subtle.ConstantTimeCompare(record.ReferenceHash[:], digest[:]) != 1 ||
		len(record.Keys) == 0 || len(record.Keys) != len(record.Preparations) || record.TermsDigest != levelingBundleDigest([]byte(record.Terms)) {
		return preparedBundleRecord{}, 0, fmt.Errorf("prepared bundle reference is unavailable")
	}
	return record, doc.Revision, nil
}

// bundleConfirmationBlockers refuses a confirmation too large to keep.
func bundleConfirmationBlockers(c *rpc.TradeProposalBundleConfirmation) []rpc.TradingBlocker {
	if c != nil && len(c.DeskActionID)+len(c.Credential)+len(c.Envelope) > bundleConfirmationMaxBytes {
		return preparedBlocker("bad_request", "The confirmation is larger than Canary keeps; nothing was sent.")
	}
	return nil
}

// SubmitBundle sends a prepared bundle once. It refuses a reference already
// submitted, other terms than the retained ones and an expired review, then
// records the submission before anything else, checks every conversion,
// sending nothing until all pass, and sends them in order, stopping at the
// first that is not sent. It runs inside brokerWriteMu, like every proposal
// submit.
func (e *proposalEngine) SubmitBundle(ctx context.Context, p rpc.TradeProposalSubmitBundleParams) (rpc.TradeProposalSubmitBundleResult, error) {
	out := rpc.TradeProposalSubmitBundleResult{BundleID: p.BundleID, Outcome: rpc.BundleOutcomeNotSent, Legs: []rpc.TradeProposalSubmitResult{}, AsOf: e.clock()}
	record, revision, err := e.loadBundle(ctx, p.BundleRef)
	if err != nil {
		out.Blockers = preparedBlocker("prepared_reference_unavailable", "The prepared repayment reference cannot be resolved safely; nothing was sent.")
		return out, nil
	}
	consumed := []rpc.TradingBlocker{{Code: "prepared_reference_consumed",
		Message: "This repayment was already submitted once, and its reference never sends again.",
		Action:  "Read what it sent with canary proposals bundle-status; never send it again."}}
	n := len(record.Keys)
	legs := make([]rpc.TradeProposalSubmitResult, n)
	for i := range legs {
		legs[i] = rpc.TradeProposalSubmitResult{Leg: i + 1, Key: record.Keys[i], Outcome: rpc.BundleOutcomeNotSent, AsOf: e.clock()}
	}
	// refused answers a request refused before the submission starts:
	// nothing was sent by it, and the bundle stays as it was.
	refused := func(blockers []rpc.TradingBlocker) (rpc.TradeProposalSubmitBundleResult, error) {
		out.Legs, out.Blockers = legs, blockers
		return out, nil
	}
	switch {
	case record.BundleID != p.BundleID || record.Revision != p.Revision:
		return refused(preparedBlocker("prepared_binding_mismatch", "Bundle id and revision must match the prepared repayment; nothing was sent."))
	case p.TermsDigest != record.TermsDigest:
		return refused(preparedBlocker("prepared_terms_mismatch", "The confirmed terms are not the prepared repayment's terms, so nothing was sent; prepare and confirm it again."))
	case !record.SubmittedAt.IsZero():
		// Whatever the first submission did, this one sent nothing; the
		// bundle's outcome is the status read's to say, never this refusal's.
		out.Outcome, out.Blockers = "", consumed
		return out, nil
	case !e.clock().Before(record.ExpiresAt):
		return refused(preparedBlocker("prepared_reference_expired", "The review expired, so nothing was sent; prepare the repayment again and confirm it again."))
	}
	if blockers := bundleConfirmationBlockers(p.Confirmation); len(blockers) > 0 {
		return refused(blockers)
	}
	// The one submission this reference allows starts here, before any
	// conversion is checked again: a second submission is refused as
	// consumed, and the status reads what this one did.
	record.SubmittedAt, record.Origin, record.Confirmation = e.clock().UTC(), normalizedWriteOrigin(p.Origin), p.Confirmation
	saved, err := e.saveBundle(ctx, record, revision)
	if err != nil {
		if _, conflict := errors.AsType[*corestore.RevisionConflictError](err); conflict {
			out.Outcome, out.Blockers = "", consumed
			return out, nil
		}
		return refused(preparedBlocker("preparation_not_persisted", "The submission could not be recorded, so nothing was sent."))
	}
	revision = saved.Revision
	e.bundlesSending.Store(record.ID, struct{}{})
	defer e.bundlesSending.Delete(record.ID)
	finish := func(blockers []rpc.TradingBlocker) (rpc.TradeProposalSubmitBundleResult, error) {
		out.Legs, out.Blockers, out.AsOf = legs, blockers, e.clock()
		outcomes := make([]string, n)
		for i, leg := range legs {
			outcomes[i] = leg.Outcome
		}
		out.Outcome, out.Sent = levelingBundleOutcome(outcomes)
		out.Accepted = out.Outcome == rpc.BundleOutcomeSent
		record.FinishedAt, record.Outcomes = e.clock().UTC(), outcomes
		// The outcome is recorded even when the request's deadline passed
		// while sending.
		if _, err := e.saveBundle(context.WithoutCancel(ctx), record, revision); err != nil && e.server != nil {
			e.server.warnf("leveling bundle %s: record its outcome: %v; its status reads the order journal instead", record.ID, err)
		}
		return out, nil
	}
	notSent := func(from int, why string) {
		for j := from; j < n; j++ {
			if legs[j].Outcome == rpc.BundleOutcomeNotSent {
				legs[j].Message = why
			}
		}
	}

	_, rows, _, blockers, err := e.servedLevelingBundle(ctx, p.BundleID, p.Revision)
	if err != nil || len(blockers) > 0 {
		if len(blockers) == 0 {
			blockers = preparedBlocker("proposal_refresh_failed", "The current proposals could not be read, so nothing was sent.")
		}
		notSent(0, "Not sent: the repayment could not be checked again.")
		return finish(blockers)
	}
	if len(rows) != n {
		notSent(0, "Not sent: the repayment's conversions changed.")
		return finish(preparedBlocker("prepared_proposal_changed", "The repayment's conversions changed, so nothing was sent; prepare and confirm it again."))
	}
	type checkedLeg struct {
		params rpc.TradeProposalSubmitParams
		record preparedProposalRecord
		prop   rpc.TradeProposal
	}
	checked := make([]checkedLeg, 0, n)
	for i, row := range rows {
		started := time.Now()
		leg := rpc.TradeProposalSubmitResult{Leg: i + 1, Key: record.Keys[i], AsOf: e.clock()}
		rec, err := e.loadPreparationRecord(ctx, record.Preparations[i])
		var params rpc.TradeProposalSubmitParams
		ok := false
		if err == nil && rec.Preparation.Key == record.Keys[i] && rec.Preparation.Key == row.Key && rec.Preparation.Revision == row.Revision {
			params = rpc.TradeProposalSubmitParams{Key: rec.Preparation.Key, Revision: rec.Preparation.Revision, FastPath: p.FastPath, TimeoutMs: p.TimeoutMs, Origin: p.Origin}
			var prop rpc.TradeProposal
			prop, ok, err = e.preparedSubmitCheck(ctx, params, rec, &leg, nil)
			if ok {
				checked = append(checked, checkedLeg{params: params, record: rec, prop: prop})
				continue
			}
		} else {
			err = nil
			leg.Blockers = preparedBlocker("prepared_binding_mismatch", "A retained conversion does not match the repayment as served; prepare and confirm it again.")
		}
		if err != nil && len(leg.Blockers) == 0 {
			leg.Blockers = preparedBlocker("submit_check_failed", "Canary could not check this conversion again: "+err.Error())
		}
		leg.Leg, leg.Key, leg.Outcome = i+1, record.Keys[i], rpc.BundleOutcomeRefused
		e.finishSubmitIn(record.ID, "submit_bundle", params, &leg, err, started)
		legs[i] = leg
		notSent(0, fmt.Sprintf("Not sent: conversion %d of the repayment did not pass its checks, so none was sent.", i+1))
		return finish([]rpc.TradingBlocker{{Code: "bundle_conversion_refused",
			Message: fmt.Sprintf("conversion %d of %d did not pass its checks, so nothing was sent; its own blockers say why", i+1, n),
			Action:  "Refresh proposals and prepare the repayment again."}})
	}
	for i := range checked {
		started := time.Now()
		c := &checked[i]
		leg := rpc.TradeProposalSubmitResult{AsOf: e.clock()}
		placeErr := e.preparedSubmitPlace(ctx, c.params, c.record, c.prop, &leg)
		leg.Leg, leg.Key = i+1, record.Keys[i]
		leg.Outcome = e.bundleLegOutcome(leg, placeErr, c.record.Preview.PreviewTokenID)
		switch leg.Outcome {
		case rpc.BundleOutcomeRefused:
			why := "Canary refused this conversion while sending it, so it did not reach the broker"
			if placeErr != nil {
				why += ": " + placeErr.Error()
			}
			leg.Blockers = append(withoutBlocker(leg.Blockers, "submit_failed"), rpc.TradingBlocker{Code: "submit_refused", Message: why})
		case rpc.BundleOutcomeUnknown:
			leg.Blockers = append(withoutBlocker(leg.Blockers, "submit_failed"), rpc.TradingBlocker{Code: "submit_outcome_unknown",
				Message: "This conversion may have reached the broker; Canary cannot confirm whether it was sent.",
				Action:  "Read the repayment's status before anything else; it is never sent again."})
		}
		e.finishSubmitIn(record.ID, "submit_bundle", c.params, &leg, nil, started)
		legs[i] = leg
		switch leg.Outcome {
		case rpc.BundleOutcomeSent:
			continue
		case rpc.BundleOutcomeRefused:
			notSent(i+1, fmt.Sprintf("Not sent: Canary stopped at conversion %d, which it refused.", i+1))
			if i == 0 {
				return finish([]rpc.TradingBlocker{{Code: "bundle_not_sent",
					Message: fmt.Sprintf("conversion 1 of %d was refused while sending, so nothing was sent; its own blockers say why", n),
					Action:  "Refresh proposals; the next cycle plans the repayment again."}})
			}
			return finish([]rpc.TradingBlocker{{Code: "bundle_partly_sent",
				Message: fmt.Sprintf("conversion %d of %d was refused while sending and did not reach the broker; %s, and the rest were not sent", i+1, n, levelingSentBefore(i)),
				Action:  "Inspect each conversion's receipt; nothing resends the rest, and the next cycle plans what remains once the ledger shows the fills."}})
		default:
			notSent(i+1, fmt.Sprintf("Not sent: Canary stopped at conversion %d, whose outcome is not confirmed.", i+1))
			sent := "nothing before it was sent"
			if i > 0 {
				sent = levelingSentBefore(i)
			}
			return finish([]rpc.TradingBlocker{{Code: "bundle_outcome_unknown",
				Message: fmt.Sprintf("conversion %d of %d may have reached the broker and its outcome is not confirmed; %s, and the rest were not sent", i+1, n, sent),
				Action:  "Read the repayment's status (canary proposals bundle-status) before anything else; nothing is sent again."}})
		}
	}
	return finish(nil)
}

// levelingSentBefore says how many conversions before the i-th (0-based)
// were sent.
func levelingSentBefore(i int) string {
	if i == 1 {
		return "the one before it was sent"
	}
	return fmt.Sprintf("the %d before it were sent", i)
}

// withoutBlocker drops the blockers with code.
func withoutBlocker(blockers []rpc.TradingBlocker, code string) []rpc.TradingBlocker {
	return slices.DeleteFunc(slices.Clone(blockers), func(b rpc.TradingBlocker) bool { return b.Code == code })
}

// bundleLegOutcome is what became of a conversion Canary tried to send:
// sent once the broker took it; otherwise what the order journal proves about
// its preview token: refused when nothing reached the broker (the place was
// refused before its attempt was staged, or the send failed with nothing
// written), sent when the broker answered, unknown when the attempt may have
// reached it or the journal cannot be read.
func (e *proposalEngine) bundleLegOutcome(leg rpc.TradeProposalSubmitResult, placeErr error, tokenID string) string {
	if placeErr == nil && leg.Accepted && leg.Place != nil {
		return rpc.BundleOutcomeSent
	}
	if e.server == nil || e.server.orderJournal == nil {
		return rpc.BundleOutcomeUnknown
	}
	journal, err := e.server.orderJournal.LoadEvents(0)
	if err != nil {
		return rpc.BundleOutcomeUnknown
	}
	// Canary attempted it: a send the journal never staged was refused.
	return bundleJournalOutcome(journal, tokenID, rpc.BundleOutcomeRefused)
}

// bundleJournalOutcome classifies a conversion's send from the order
// journal, which stages every attempt before its first frame. recorded is
// what the submission recorded for it; where the journal holds no attempt,
// it says whether the conversion was refused (attempted, or failed its
// checks) or never attempted, and a recorded send stands.
func bundleJournalOutcome(journal []orderJournalEvent, tokenID, recorded string) string {
	if tokenID == "" {
		// Without its preparation the journal cannot be asked; only a
		// recorded final outcome stands.
		switch recorded {
		case rpc.BundleOutcomeSent, rpc.BundleOutcomeRefused, rpc.BundleOutcomeNotSent:
			return recorded
		}
		return rpc.BundleOutcomeUnknown
	}
	switch outcome, _ := orderJournalSendOutcome(journal, tokenID); outcome {
	case orderSendReached:
		return rpc.BundleOutcomeSent
	case orderSendFailedUnsent:
		return rpc.BundleOutcomeRefused
	case orderSendUnclear:
		return rpc.BundleOutcomeUnknown
	}
	switch recorded {
	case rpc.BundleOutcomeSent:
		return rpc.BundleOutcomeSent
	case rpc.BundleOutcomeRefused, rpc.BundleOutcomeUnknown:
		return rpc.BundleOutcomeRefused
	}
	return rpc.BundleOutcomeNotSent
}

// levelingBundleOutcome is a submitted bundle's outcome from its
// conversions' and how many were sent: unknown while any is unknown, sent
// when all were, not_sent when none was, partly_sent otherwise.
func levelingBundleOutcome(outcomes []string) (string, int) {
	sent := 0
	unknown := false
	for _, o := range outcomes {
		switch o {
		case rpc.BundleOutcomeSent:
			sent++
		case rpc.BundleOutcomeUnknown:
			unknown = true
		}
	}
	switch {
	case unknown:
		return rpc.BundleOutcomeUnknown, sent
	case sent == len(outcomes) && sent > 0:
		return rpc.BundleOutcomeSent, sent
	case sent == 0:
		return rpc.BundleOutcomeNotSent, sent
	}
	return rpc.BundleOutcomePartlySent, sent
}

// PreparedBundleStatus reads what became of a prepared bundle from Canary's
// own records: the bundle's record, each conversion's preparation and the
// order journal. It never prepares or sends.
func (e *proposalEngine) PreparedBundleStatus(ctx context.Context, reference string) (rpc.TradeProposalPreparedBundleStatusResult, error) {
	out := rpc.TradeProposalPreparedBundleStatusResult{Legs: []rpc.TradeProposalBundleLeg{}, AsOf: e.clock()}
	record, _, err := e.loadBundle(ctx, reference)
	if err != nil {
		out.Blockers = preparedBlocker("prepared_reference_unavailable", "The prepared repayment reference cannot be resolved safely.")
		return out, nil
	}
	out.BundleID, out.Revision, out.PreparationID, out.TermsDigest = record.BundleID, record.Revision, record.ID, record.TermsDigest
	out.ExpiresAt, out.SubmittedAt = record.ExpiresAt, record.SubmittedAt
	var journal []orderJournalEvent
	journalErr := errors.New("order journal is unavailable")
	if e.server != nil && e.server.orderJournal != nil {
		journal, journalErr = e.server.orderJournal.LoadEvents(0)
	}
	outcomes := make([]string, 0, len(record.Keys))
	for i, id := range record.Preparations {
		leg := rpc.TradeProposalBundleLeg{Leg: i + 1, Key: record.Keys[i]}
		recorded := ""
		if i < len(record.Outcomes) {
			recorded = record.Outcomes[i]
		}
		tokenID := ""
		if prep, err := e.loadPreparationRecord(ctx, id); err == nil {
			st := e.preparedStatus(ctx, prep)
			leg.Preparation, leg.Order, leg.OrderRef, tokenID = st.Preparation, st.Order, prep.Preview.Draft.OrderRef, prep.Preview.PreviewTokenID
			for _, b := range st.Blockers {
				if !slices.ContainsFunc(out.Blockers, func(have rpc.TradingBlocker) bool { return have.Code == b.Code }) {
					out.Blockers = append(out.Blockers, b)
				}
			}
		}
		switch {
		case journalErr != nil && (recorded == rpc.BundleOutcomeSent || recorded == rpc.BundleOutcomeRefused || recorded == rpc.BundleOutcomeNotSent):
			leg.Outcome = recorded
		case journalErr != nil:
			leg.Outcome = rpc.BundleOutcomeUnknown
		default:
			leg.Outcome = bundleJournalOutcome(journal, tokenID, recorded)
		}
		outcomes = append(outcomes, leg.Outcome)
		out.Legs = append(out.Legs, leg)
	}
	if journalErr != nil && !slices.ContainsFunc(out.Blockers, func(b rpc.TradingBlocker) bool { return b.Code == "journal_authority_unavailable" }) {
		out.Blockers = append(out.Blockers, preparedBlocker("journal_authority_unavailable", "The order journal cannot be read, so only outcomes Canary recorded are certain; do not resend.")...)
	}
	out.Outcome, out.Sent = levelingBundleOutcome(outcomes)
	_, sending := e.bundlesSending.Load(record.ID)
	switch {
	case record.SubmittedAt.IsZero() && out.Outcome == rpc.BundleOutcomeNotSent:
		out.Outcome = rpc.BundleOutcomePrepared
		if !e.clock().Before(record.ExpiresAt) {
			out.Outcome = rpc.BundleOutcomeExpired
		}
	case sending && record.FinishedAt.IsZero():
		out.Outcome, out.Message = rpc.BundleOutcomeUnknown, "Canary is sending this repayment now; read its status again in a minute."
	}
	return out, nil
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

func (s *Server) handleTradeProposalsPreparedBundleStatus(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalPreparedBundleStatusResult, error) {
	var p rpc.TradeProposalPreparedBundleStatusParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if s.tradeProposals == nil {
		return &rpc.TradeProposalPreparedBundleStatusResult{AsOf: s.orderNow(), Blockers: preparedBlocker("proposal_engine_unavailable", "Proposal engine is unavailable.")}, nil
	}
	out, err := s.tradeProposals.PreparedBundleStatus(ctx, p.BundleRef)
	return &out, err
}
