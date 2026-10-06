//go:build trading

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// levelingBundleRig serves the two-conversion repayment of a synthetic USD
// loan (−14,000) from CHF (+6,000, 0%) and EUR (+60,000, 1.4%) on the
// trading rig, with live IDEALPRO quotes for both pairs; each conversion
// stays under the rig's own order cap in force (10,000 USD). The rig's clock
// is a Thursday morning, inside IDEALPRO's hours.
func levelingBundleRig(t *testing.T) (*automaticTestRig, []rpc.TradeProposal, rpc.TradeProposalCurrencyLevelingBundle, *brokerCallLog, *atomic.Int32) {
	t.Helper()
	rig := newAutomaticTradingRig(t, "")
	broker := &brokerCallLog{}
	broker.install(rig.server)
	whatIfs := &atomic.Int32{}
	original := rig.server.orderPreviewWhatIf
	rig.server.orderPreviewWhatIf = func(ctx context.Context, draft rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		whatIfs.Add(1)
		return original(ctx, draft)
	}
	rig.server.orderContractResolverForTest = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.ContractParams, error) {
		c.MinTick = 0.00005
		return c, nil
	}
	rig.server.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 1.16995, 1.17005 // EUR.USD
		if c.Symbol == "USD" {
			bid, ask = 0.79870, 0.79880 // USD.CHF
		}
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: rig.now, AsOf: rig.now}, nil
	}
	rig.server.orderPreviewPositionImpact = fixedPreviewPosition(0, 0, rpc.OrderPositionEffectOpenShort)
	in := levelingBook(map[string]float64{"EUR": 60000, "CHF": 6000, "USD": -14000})
	rows := levelingRows(t, in, "USD")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the CHF and EUR conversions", len(rows))
	}
	var bundle rpc.TradeProposalCurrencyLevelingBundle
	rig.installWith(func(snap *rpc.TradeProposalSnapshot) {
		bundle = currencyLevelingBundleEntry(snap.Proposals)
		snap.CurrencyLeveling = &rpc.TradeProposalCurrencyLevelingStatus{BaseCurrency: "EUR", Bundles: []rpc.TradeProposalCurrencyLevelingBundle{bundle}}
		currencyLevelingBundleRevisions(snap.CurrencyLeveling, snap.Proposals)
		bundle = snap.CurrencyLeveling.Bundles[0]
	}, rows...)
	served := rig.engine.Snapshot(false).Proposals
	return rig, served, bundle, broker, whatIfs
}

// submitBundle sends a prepared bundle with the terms it was prepared with,
// as the owner confirmed them.
func submitBundle(t *testing.T, rig *automaticTestRig, prepared rpc.TradeProposalPrepareBundleResult, bundle rpc.TradeProposalCurrencyLevelingBundle) *rpc.TradeProposalSubmitBundleResult {
	t.Helper()
	raw, err := json.Marshal(rpc.TradeProposalSubmitBundleParams{BundleRef: prepared.BundleRef, BundleID: bundle.ID, Revision: bundle.Revision, TermsDigest: prepared.TermsDigest,
		FastPath: true, Origin: rpc.OrderOriginHumanTTY})
	if err != nil {
		t.Fatal(err)
	}
	out, err := rig.server.handleTradeProposalsSubmitBundle(t.Context(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A conversion that is one of several is never prepared or sent on its own;
// the bundle is prepared whole, then sent whole, cheapest payer first, and
// its reference is spent.
func TestCurrencyLevelingBundleOneApproval(t *testing.T) {
	rig, rows, bundle, broker, whatIfs := levelingBundleRig(t)
	for _, row := range rows {
		single, err := rig.engine.Prepare(t.Context(), rpc.TradeProposalPreviewParams{Key: row.Key, Revision: row.Revision})
		if err != nil || single.Accepted || single.PreparedRef != "" || !hasBlockerCode(single.Blockers, rpc.CurrencyLevelingBlockerBundle) {
			t.Fatalf("single prepare of %s = %+v, %v; want the bundle refusal", row.Symbol, single.Blockers, err)
		}
		fast, err := rig.engine.Submit(t.Context(), rpc.TradeProposalSubmitParams{Key: row.Key, Revision: row.Revision, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
		if err != nil || fast.Accepted || !hasBlockerCode(fast.Blockers, rpc.CurrencyLevelingBlockerBundle) {
			t.Fatalf("single submit of %s = %+v, %v; want the bundle refusal", row.Symbol, fast.Blockers, err)
		}
	}
	if whatIfs.Load() != 0 || broker.count() != 0 {
		t.Fatalf("a refused single approval reached the broker: %d previews, %d orders", whatIfs.Load(), broker.count())
	}
	prepared, err := rig.engine.PrepareBundle(t.Context(), rpc.TradeProposalPrepareBundleParams{BundleID: bundle.ID, Revision: bundle.Revision})
	if err != nil || !prepared.Accepted || prepared.BundleRef == "" || len(prepared.Legs) != 2 || whatIfs.Load() != 2 {
		t.Fatalf("prepare bundle = %+v, %v (previews %d)", prepared, err, whatIfs.Load())
	}
	for _, leg := range prepared.Legs {
		if !leg.Accepted || !leg.SubmitEligible {
			t.Fatalf("leg %s not prepared: %+v", leg.Proposal.Symbol, leg.Blockers)
		}
	}
	if raw, _ := json.Marshal(prepared.Legs); strings.Contains(string(raw), preparedProposalPrefix+".") {
		t.Fatal("a conversion's private reference left the daemon")
	}
	out := submitBundle(t, rig, prepared, bundle)
	if !out.Accepted || len(out.Legs) != 2 || broker.count() != 2 {
		t.Fatalf("submit bundle = %+v (orders %d)", out, broker.count())
	}
	if first := broker.orders[0]; first.Action != rpc.OrderActionBuy {
		t.Fatalf("first order = %+v, want the CHF conversion (BUY USD.CHF) first", first)
	}
	// The reference is single-use: a second send is refused as consumed, so
	// the owner is never told that a sent repayment was not sent (defect 3,
	// verified 2026-10-06 17:55 CEST: it answered bundle_conversion_refused,
	// "nothing was sent").
	again := submitBundle(t, rig, prepared, bundle)
	if again.Accepted || broker.count() != 2 || !hasBlockerCode(again.Blockers, "prepared_reference_consumed") ||
		strings.Contains(firstBlockerMessage(again.Blockers), "nothing was sent") {
		t.Fatalf("second submit = %+v (orders %d)", again, broker.count())
	}
}

func firstBlockerMessage(blockers []rpc.TradingBlocker) string {
	if len(blockers) == 0 {
		return ""
	}
	return blockers[0].Message
}

// placeAnswers makes the broker answer each order in turn: nil sends it, an
// error stands for the transport's answer.
func placeAnswers(rig *automaticTestRig, broker *brokerCallLog, answers ...error) {
	var n atomic.Int32
	rig.server.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		i := int(n.Add(1)) - 1
		broker.mu.Lock()
		broker.orders = append(broker.orders, *order)
		broker.mu.Unlock()
		if i < len(answers) {
			return answers[i]
		}
		return nil
	}
}

// Defect 1 (verified 2026-10-06 17:55 CEST): a conversion whose transmit
// returned an error may have reached the broker. It was reported as refused
// (bundle_partly_sent, "was refused"): preparedSubmitPlace turned any place
// error into submit_failed and SubmitBundle called that conversion the
// refused one. Its outcome is unknown, and the bundle's too.
func TestCurrencyLevelingBundleTransportFailureIsUnknownNotRefused(t *testing.T) {
	rig, _, bundle, broker, _ := levelingBundleRig(t)
	prepared, err := rig.engine.PrepareBundle(t.Context(), rpc.TradeProposalPrepareBundleParams{BundleID: bundle.ID, Revision: bundle.Revision})
	if err != nil || !prepared.Accepted {
		t.Fatalf("prepare bundle = %+v, %v", prepared.Blockers, err)
	}
	placeAnswers(rig, broker, errors.New("broker connection reset while writing the order"))
	out := submitBundle(t, rig, prepared, bundle)
	if out.Accepted || hasBlockerCode(out.Blockers, "bundle_partly_sent") || !hasBlockerCode(out.Blockers, "bundle_outcome_unknown") ||
		strings.Contains(firstBlockerMessage(out.Blockers), "was refused") {
		t.Fatalf("a conversion that may have reached the broker reads %+v", out.Blockers)
	}
	if broker.count() != 1 {
		t.Fatalf("orders %d; the bundle must stop after a conversion whose outcome is unknown", broker.count())
	}
}

// Defect 2 (verified 2026-10-06 17:55 CEST): when the first conversion is
// refused while placing, nothing was sent; the bundle read
// bundle_partly_sent, "the 0 before it were sent".
func TestCurrencyLevelingBundleFirstRefusalWhilePlacingSendsNothing(t *testing.T) {
	rig, _, bundle, broker, _ := levelingBundleRig(t)
	prepared, err := rig.engine.PrepareBundle(t.Context(), rpc.TradeProposalPrepareBundleParams{BundleID: bundle.ID, Revision: bundle.Revision})
	if err != nil || !prepared.Accepted {
		t.Fatalf("prepare bundle = %+v, %v", prepared.Blockers, err)
	}
	placeAnswers(rig, broker, ibkrlib.WithSendDisposition(errors.New("refused before any byte left"), ibkrlib.SendDispositionDefinitelyUnsent))
	out := submitBundle(t, rig, prepared, bundle)
	if out.Accepted || hasBlockerCode(out.Blockers, "bundle_partly_sent") || !hasBlockerCode(out.Blockers, "bundle_not_sent") ||
		strings.Contains(firstBlockerMessage(out.Blockers), "the 0 before it") {
		t.Fatalf("a first refusal while placing reads %+v", out.Blockers)
	}
}

// A bundle whose second conversion no longer passes its checks sends
// nothing at all, lists every conversion with what became of it, and spends
// its reference.
func TestCurrencyLevelingBundleSendsNothingUnlessEveryConversionPasses(t *testing.T) {
	rig, rows, bundle, broker, _ := levelingBundleRig(t)
	prepared := prepareBundle(t, rig, bundle)
	// A wrong revision is refused before the submission starts, so the
	// reference stays unspent.
	stale := bundle
	stale.Revision = "other"
	if out := submitBundle(t, rig, prepared, stale); out.Accepted || !hasBlockerCode(out.Blockers, "prepared_binding_mismatch") {
		t.Fatalf("submit with another revision = %+v", out)
	}
	second := rows[1]
	rig.engine.revalidateForTest = func(_ context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		for _, row := range rows {
			if row.Key == key {
				if key == second.Key {
					return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "stale"}}, nil
				}
				return row, nil, nil
			}
		}
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "gone"}}, nil
	}
	out := submitBundle(t, rig, prepared, bundle)
	if out.Accepted || broker.count() != 0 || !hasBlockerCode(out.Blockers, "bundle_conversion_refused") ||
		out.Outcome != rpc.BundleOutcomeNotSent || out.Sent != 0 {
		t.Fatalf("submit with a stale second conversion = %+v (orders %d)", out, broker.count())
	}
	if got := legOutcomes(out.Legs); !slices.Equal(got, []string{rpc.BundleOutcomeNotSent, rpc.BundleOutcomeRefused}) || out.Legs[0].Key != bundle.Keys[0] {
		t.Fatalf("legs = %v %+v, want every conversion in send order", got, out.Legs)
	}
	if again := submitBundle(t, rig, prepared, bundle); !hasBlockerCode(again.Blockers, "prepared_reference_consumed") || broker.count() != 0 {
		t.Fatalf("a second submission after a refusal = %+v", again)
	}
	status := bundleStatus(t, rig, prepared)
	if status.Outcome != rpc.BundleOutcomeNotSent || !slices.Equal(legStatusOutcomes(status.Legs), []string{rpc.BundleOutcomeNotSent, rpc.BundleOutcomeRefused}) {
		t.Fatalf("status after a refused check = %+v", status)
	}
}

func prepareBundle(t *testing.T, rig *automaticTestRig, bundle rpc.TradeProposalCurrencyLevelingBundle) rpc.TradeProposalPrepareBundleResult {
	t.Helper()
	prepared, err := rig.engine.PrepareBundle(t.Context(), rpc.TradeProposalPrepareBundleParams{BundleID: bundle.ID, Revision: bundle.Revision})
	if err != nil || !prepared.Accepted || prepared.BundleRef == "" || prepared.TermsDigest == "" {
		t.Fatalf("prepare bundle = %+v, %v", prepared.Blockers, err)
	}
	return prepared
}

func bundleStatus(t *testing.T, rig *automaticTestRig, prepared rpc.TradeProposalPrepareBundleResult) rpc.TradeProposalPreparedBundleStatusResult {
	t.Helper()
	raw, err := json.Marshal(rpc.TradeProposalPreparedBundleStatusParams{BundleRef: prepared.BundleRef})
	if err != nil {
		t.Fatal(err)
	}
	out, err := rig.server.handleTradeProposalsPreparedBundleStatus(t.Context(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return *out
}

func legOutcomes(legs []rpc.TradeProposalSubmitResult) []string {
	out := make([]string, 0, len(legs))
	for _, leg := range legs {
		out = append(out, leg.Outcome)
	}
	return out
}

func legStatusOutcomes(legs []rpc.TradeProposalBundleLeg) []string {
	out := make([]string, 0, len(legs))
	for _, leg := range legs {
		out = append(out, leg.Outcome)
	}
	return out
}

// A review past its ten minutes is refused before anything is checked; its
// status reads expired.
func TestCurrencyLevelingBundleExpiredReviewSendsNothing(t *testing.T) {
	rig, _, bundle, broker, _ := levelingBundleRig(t)
	prepared := prepareBundle(t, rig, bundle)
	if st := bundleStatus(t, rig, prepared); st.Outcome != rpc.BundleOutcomePrepared || len(st.Legs) != 2 ||
		st.Legs[0].Preparation == nil || st.Legs[0].Preparation.State != "prepared" {
		t.Fatalf("status of a fresh review = %+v", st)
	}
	rig.advance(24 * time.Hour)
	if out := submitBundle(t, rig, prepared, bundle); out.Accepted || !hasBlockerCode(out.Blockers, "prepared_reference_expired") || broker.count() != 0 {
		t.Fatalf("submit a day later = %+v (orders %d)", out, broker.count())
	}
	if st := bundleStatus(t, rig, prepared); st.Outcome != rpc.BundleOutcomeExpired || st.Sent != 0 {
		t.Fatalf("status a day later = %+v", st)
	}
}

// Terms other than the prepared ones send nothing and leave the reference
// unspent.
func TestCurrencyLevelingBundleRefusesOtherTerms(t *testing.T) {
	rig, _, bundle, broker, _ := levelingBundleRig(t)
	prepared := prepareBundle(t, rig, bundle)
	other := prepared
	other.TermsDigest = "sha256:" + strings.Repeat("0", 64)
	if out := submitBundle(t, rig, other, bundle); out.Accepted || !hasBlockerCode(out.Blockers, "prepared_terms_mismatch") || broker.count() != 0 {
		t.Fatalf("submit with other terms = %+v (orders %d)", out, broker.count())
	}
	other.TermsDigest = ""
	if out := submitBundle(t, rig, other, bundle); out.Accepted || !hasBlockerCode(out.Blockers, "prepared_terms_mismatch") || broker.count() != 0 {
		t.Fatalf("submit without terms = %+v (orders %d)", out, broker.count())
	}
	if st := bundleStatus(t, rig, prepared); st.Outcome != rpc.BundleOutcomePrepared {
		t.Fatalf("a refused digest spent the reference: %+v", st)
	}
	if out := submitBundle(t, rig, prepared, bundle); !out.Accepted || broker.count() != 2 {
		t.Fatalf("submit with the prepared terms = %+v (orders %d)", out, broker.count())
	}
}

// The exact terms state every conversion's identity and its figures at the
// limit, bounded against the owner, and their digest is of the exact bytes.
func TestCurrencyLevelingBundleTermsAreExact(t *testing.T) {
	rig, rows, bundle, _, _ := levelingBundleRig(t)
	prepared := prepareBundle(t, rig, bundle)
	sum := sha256.Sum256([]byte(prepared.Terms))
	if prepared.TermsDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s is not of the terms bytes", prepared.TermsDigest)
	}
	dec := json.NewDecoder(strings.NewReader(prepared.Terms))
	dec.DisallowUnknownFields()
	var terms rpc.LevelingBundleTerms
	if err := dec.Decode(&terms); err != nil {
		t.Fatal(err)
	}
	if again, _ := json.Marshal(terms); string(again) != prepared.Terms {
		t.Fatalf("terms are not canonical:\n%s\n%s", prepared.Terms, again)
	}
	if raw, _ := json.Marshal(prepared); strings.Contains(prepared.Terms, preparedBundlePrefix+".") || strings.Contains(prepared.Terms, preparedProposalPrefix+".") ||
		strings.Count(string(raw), preparedProposalPrefix+".") != 0 {
		t.Fatal("a private reference is in the terms or a conversion's result")
	}
	if len(prepared.Preparations) != len(terms.Legs) {
		t.Fatalf("preparations = %+v", prepared.Preparations)
	}
	for i, prep := range prepared.Preparations {
		if prep.ID != terms.Legs[i].PreparationID || prep.DraftFingerprint != terms.Legs[i].DraftFingerprint || prep.State != "prepared" || prep.Consumed == nil || *prep.Consumed {
			t.Fatalf("preparation %d = %+v", i+1, prep)
		}
	}
	if !strings.HasPrefix(prepared.BundleRef, preparedBundlePrefix+"."+prepared.PreparationID+".") {
		t.Fatal("the reference does not name its record")
	}
	if terms.Kind != rpc.LevelingBundleTermsKind || terms.Version != 1 || terms.BundleID != bundle.ID || terms.Revision != bundle.Revision ||
		terms.PreparationID != prepared.PreparationID || terms.BaseCurrency != "EUR" || terms.Currency != "USD" || len(terms.Legs) != len(rows) ||
		!terms.ExpiresAt.Equal(prepared.ExpiresAt) || terms.AccountID == "" || terms.AccountMode == "" {
		t.Fatalf("terms = %+v", terms)
	}
	lands, seen := terms.Cash, map[string]bool{}
	for i, leg := range terms.Legs {
		row := rows[i].CurrencyLeveling
		if leg.Leg != i+1 || leg.Key != bundle.Keys[i] || leg.PreparationID == "" || leg.DraftFingerprint == "" || leg.PreviewTokenID == "" ||
			leg.OrderType != rpc.OrderTypeLMT || leg.TIF != rpc.OrderTIFDay || !(leg.Bid < leg.Ask) || leg.LimitPrice <= 0 || leg.ConID <= 0 {
			t.Fatalf("leg %d = %+v", i+1, leg)
		}
		for _, id := range []string{leg.PreparationID, leg.PreviewTokenID, leg.OrderRef} {
			if seen[id] {
				t.Fatalf("leg %d repeats %s", i+1, id)
			}
			seen[id] = true
		}
		at := float64(leg.Quantity) * leg.LimitPrice
		switch leg.Action {
		case rpc.OrderActionBuy:
			if leg.Receives != (rpc.LevelingBundleAmount{Amount: float64(leg.Quantity), Currency: "USD", Bound: rpc.LevelingBundleBoundExact}) ||
				leg.Pays.Bound != rpc.LevelingBundleBoundAtMost || leg.Pays.Currency != row.FundingCurrency || leg.Pays.Amount < at || leg.Pays.Amount > at+0.01 {
				t.Fatalf("BUY leg %d pays %+v receives %+v at %v", i+1, leg.Pays, leg.Receives, at)
			}
		case rpc.OrderActionSell:
			if leg.Pays != (rpc.LevelingBundleAmount{Amount: float64(leg.Quantity), Currency: row.FundingCurrency, Bound: rpc.LevelingBundleBoundExact}) ||
				leg.Receives.Bound != rpc.LevelingBundleBoundAtLeast || leg.Receives.Currency != "USD" || leg.Receives.Amount > at || leg.Receives.Amount < at-0.01 {
				t.Fatalf("SELL leg %d pays %+v receives %+v at %v", i+1, leg.Pays, leg.Receives, at)
			}
		default:
			t.Fatalf("leg %d action %q", i+1, leg.Action)
		}
		if want := row.FundingCash - row.FundingCommitted - leg.Pays.Amount; leg.KeepsAtLeast > want || leg.KeepsAtLeast < want-0.01 {
			t.Fatalf("leg %d keeps at least %v, want %v", i+1, leg.KeepsAtLeast, want)
		}
		lands += leg.Receives.Amount
	}
	if terms.LandsAtLeast > lands || terms.LandsAtLeast < lands-0.01 || terms.Cushion <= 0 {
		t.Fatalf("lands at least %v (cash plus receipts %v), cushion %v", terms.LandsAtLeast, lands, terms.Cushion)
	}
}

// The owner's confirmation is kept with the bundle's one submission, for
// audit only, and the conversions' decisions name the bundle.
func TestCurrencyLevelingBundleKeepsTheConfirmation(t *testing.T) {
	rig, _, bundle, _, _ := levelingBundleRig(t)
	prepared := prepareBundle(t, rig, bundle)
	confirmation := &rpc.TradeProposalBundleConfirmation{DeskActionID: "desk-action-synthetic", Credential: "credential-synthetic", Envelope: "envelope-synthetic"}
	raw, _ := json.Marshal(rpc.TradeProposalSubmitBundleParams{BundleRef: prepared.BundleRef, BundleID: bundle.ID, Revision: bundle.Revision, TermsDigest: prepared.TermsDigest,
		Confirmation: confirmation, FastPath: true, Origin: rpc.OrderOriginAgent})
	out, err := rig.server.handleTradeProposalsSubmitBundle(t.Context(), &rpc.Request{Params: raw})
	if err != nil || !out.Accepted || out.Outcome != rpc.BundleOutcomeSent || out.Sent != 2 {
		t.Fatalf("submit = %+v, %v", out, err)
	}
	// Each receipt names its conversion: the row, its token and its order.
	for i, leg := range out.Legs {
		if leg.Proposal.Key != bundle.Keys[i] || leg.PreviewTokenID == "" || leg.Preview == nil || leg.OrderRef == "" || leg.Place == nil || leg.Place.PreviewTokenID != leg.PreviewTokenID {
			t.Fatalf("leg %d receipt = %+v", i+1, leg)
		}
	}
	record, _, err := rig.engine.loadBundle(t.Context(), prepared.BundleRef)
	if err != nil || record.Confirmation == nil || *record.Confirmation != *confirmation || record.Origin != rpc.OrderOriginAgent ||
		record.SubmittedAt.IsZero() || record.FinishedAt.IsZero() || !slices.Equal(record.Outcomes, []string{rpc.BundleOutcomeSent, rpc.BundleOutcomeSent}) {
		t.Fatalf("bundle record = %+v, %v", record, err)
	}
	st := bundleStatus(t, rig, prepared)
	if st.Outcome != rpc.BundleOutcomeSent || st.Sent != 2 || len(st.Legs) != 2 {
		t.Fatalf("status = %+v", st)
	}
	for _, leg := range st.Legs {
		if leg.Order == nil || !leg.Order.Found || leg.OrderRef == "" || leg.Preparation == nil || leg.Preparation.State != "consumed" {
			t.Fatalf("leg %d status = %+v", leg.Leg, leg)
		}
	}
}

// A partial send: conversion 2 is refused while sending after conversion 1
// was sent. The first stays sent, the second did not reach the broker, and
// the status reads the same.
func TestCurrencyLevelingBundlePartlySent(t *testing.T) {
	rig, _, bundle, broker, _ := levelingBundleRig(t)
	prepared := prepareBundle(t, rig, bundle)
	placeAnswers(rig, broker, nil, ibkrlib.WithSendDisposition(errors.New("socket closed before the order"), ibkrlib.SendDispositionDefinitelyUnsent))
	out := submitBundle(t, rig, prepared, bundle)
	if out.Accepted || out.Outcome != rpc.BundleOutcomePartlySent || out.Sent != 1 || !hasBlockerCode(out.Blockers, "bundle_partly_sent") ||
		!strings.Contains(firstBlockerMessage(out.Blockers), "the one before it was sent") {
		t.Fatalf("partly sent bundle = %+v", out)
	}
	if got := legOutcomes(out.Legs); !slices.Equal(got, []string{rpc.BundleOutcomeSent, rpc.BundleOutcomeRefused}) || !hasBlockerCode(out.Legs[1].Blockers, "submit_refused") {
		t.Fatalf("legs = %v %+v", got, out.Legs)
	}
	st := bundleStatus(t, rig, prepared)
	if st.Outcome != rpc.BundleOutcomePartlySent || st.Sent != 1 || !slices.Equal(legStatusOutcomes(st.Legs), []string{rpc.BundleOutcomeSent, rpc.BundleOutcomeRefused}) {
		t.Fatalf("status = %+v", st)
	}
}

// A transport failure after a sent conversion leaves the bundle's outcome
// unknown, never partly sent, and the status says the same until the
// journal settles it.
func TestCurrencyLevelingBundleTransportFailureMidBundle(t *testing.T) {
	rig, _, bundle, broker, _ := levelingBundleRig(t)
	prepared := prepareBundle(t, rig, bundle)
	placeAnswers(rig, broker, nil, errors.New("broker connection reset while writing the order"))
	out := submitBundle(t, rig, prepared, bundle)
	if out.Accepted || out.Outcome != rpc.BundleOutcomeUnknown || out.Sent != 1 || !hasBlockerCode(out.Blockers, "bundle_outcome_unknown") ||
		hasBlockerCode(out.Blockers, "bundle_partly_sent") || !strings.Contains(firstBlockerMessage(out.Blockers), "the one before it was sent") {
		t.Fatalf("transport failure mid-bundle = %+v", out)
	}
	if got := legOutcomes(out.Legs); !slices.Equal(got, []string{rpc.BundleOutcomeSent, rpc.BundleOutcomeUnknown}) {
		t.Fatalf("legs = %v", got)
	}
	if st := bundleStatus(t, rig, prepared); st.Outcome != rpc.BundleOutcomeUnknown || st.Sent != 1 {
		t.Fatalf("status = %+v", st)
	}
}

// The status reads every outcome from Canary's own records: a bundle whose
// first conversion was refused while sending reads not sent, one being sent
// now reads unknown with why, and one a stopped daemon left unfinished
// reads what the journal holds.
func TestCurrencyLevelingBundleStatusOutcomes(t *testing.T) {
	t.Run("first refused", func(t *testing.T) {
		rig, _, bundle, broker, _ := levelingBundleRig(t)
		prepared := prepareBundle(t, rig, bundle)
		placeAnswers(rig, broker, ibkrlib.WithSendDisposition(errors.New("refused before any byte left"), ibkrlib.SendDispositionDefinitelyUnsent))
		out := submitBundle(t, rig, prepared, bundle)
		if out.Outcome != rpc.BundleOutcomeNotSent || out.Sent != 0 || !slices.Equal(legOutcomes(out.Legs), []string{rpc.BundleOutcomeRefused, rpc.BundleOutcomeNotSent}) {
			t.Fatalf("submit = %+v", out)
		}
		// The conversion never sent still names itself, as it was checked.
		if second := out.Legs[1]; second.Proposal.Key != bundle.Keys[1] || second.PreviewTokenID == "" || second.Accepted || second.Message == "" {
			t.Fatalf("not sent leg = %+v", second)
		}
		if st := bundleStatus(t, rig, prepared); st.Outcome != rpc.BundleOutcomeNotSent || st.Sent != 0 {
			t.Fatalf("status = %+v", st)
		}
	})
	t.Run("being sent", func(t *testing.T) {
		rig, _, bundle, _, _ := levelingBundleRig(t)
		prepared := prepareBundle(t, rig, bundle)
		record, revision, err := rig.engine.loadBundle(t.Context(), prepared.BundleRef)
		if err != nil {
			t.Fatal(err)
		}
		record.SubmittedAt = rig.now
		if _, err := rig.engine.saveBundle(t.Context(), record, revision); err != nil {
			t.Fatal(err)
		}
		rig.engine.bundlesSending.Store(record.ID, struct{}{})
		if st := bundleStatus(t, rig, prepared); st.Outcome != rpc.BundleOutcomeUnknown || st.Message == "" {
			t.Fatalf("status while sending = %+v", st)
		}
		// The same record after the process stopped: nothing in the journal,
		// so nothing reached the broker and nothing resumes it.
		rig.engine.bundlesSending.Delete(record.ID)
		if st := bundleStatus(t, rig, prepared); st.Outcome != rpc.BundleOutcomeNotSent || st.Message != "" {
			t.Fatalf("status after a stop = %+v", st)
		}
		if again := submitBundle(t, rig, prepared, bundle); !hasBlockerCode(again.Blockers, "prepared_reference_consumed") {
			t.Fatalf("a bundle a stopped daemon left was sent again: %+v", again)
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		rig, _, _, _, _ := levelingBundleRig(t)
		st := bundleStatus(t, rig, rpc.TradeProposalPrepareBundleResult{BundleRef: preparedBundlePrefix + ".AAAAAAAAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA"})
		if !hasBlockerCode(st.Blockers, "prepared_reference_unavailable") || st.Outcome != "" {
			t.Fatalf("status of an unknown reference = %+v", st)
		}
	})
}
