//go:build trading

package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func preparedTestRig(t *testing.T) (*automaticTestRig, rpc.TradeProposalPrepareResult, *brokerCallLog, *atomic.Int32) {
	t.Helper()
	rig := newAutomaticTradingRig(t, "")
	broker := &brokerCallLog{}
	broker.install(rig.server)
	previews := &atomic.Int32{}
	original := rig.server.orderPreviewWhatIf
	rig.server.orderPreviewWhatIf = func(ctx context.Context, draft rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		previews.Add(1)
		return original(ctx, draft)
	}
	prop := rig.stopProposal()
	revision := rig.install(prop)
	prepared, err := rig.engine.Prepare(t.Context(), rpc.TradeProposalPreviewParams{Key: prop.Key, Revision: revision, FastPath: true})
	if err != nil || !prepared.Accepted || !prepared.SubmitEligible || prepared.PreparedRef == "" || prepared.Preparation == nil || prepared.Preparation.Consumed == nil || *prepared.Preparation.Consumed {
		t.Fatalf("prepare failed: accepted=%v blockers=%v err=%v", prepared.Accepted, prepared.Blockers, err)
	}
	return rig, prepared, broker, previews
}

func preparedSubmit(t *testing.T, rig *automaticTestRig, p rpc.TradeProposalPrepareResult) *rpc.TradeProposalSubmitResult {
	t.Helper()
	raw, err := json.Marshal(rpc.TradeProposalSubmitParams{PreparedRef: p.PreparedRef, Key: p.Proposal.Key, Revision: p.Proposal.Revision, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
	if err != nil {
		t.Fatal(err)
	}
	out, err := rig.server.handleTradeProposalsSubmit(t.Context(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func reopenPreparedRig(t *testing.T, rig *automaticTestRig) {
	t.Helper()
	path := rig.server.orderJournal.Path
	head, err := rig.core.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.core.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(filepath.Dir(path), "authority", filepath.Base(path)+".db"), MinimumHead: &head})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	journal := newOrderJournalStore(path)
	if err := journal.UseCoreStore(reopened); err != nil {
		t.Fatal(err)
	}
	rig.core, rig.server.coreStore, rig.server.orderJournal = reopened, reopened, journal
	rig.restart()
}

func TestPreparedProposalSubmitsReviewedDraftOnce(t *testing.T) {
	rig, p, broker, previews := preparedTestRig(t)
	if rig.revalidations != 1 {
		t.Fatalf("prepare did not resolve fresh evidence: %d", rig.revalidations)
	}
	if _, err := rig.server.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: p.PreparedRef, Origin: rpc.OrderOriginHumanTTY}); err == nil || broker.count() != 0 {
		t.Fatal("private proposal reference bypassed proposal validation through generic place")
	}
	out := preparedSubmit(t, rig, p)
	if !out.Accepted || out.Preparation == nil || out.Preparation.State != "consumed" || out.Order == nil || !out.Order.Found || broker.count() != 1 || previews.Load() != 1 {
		t.Fatalf("submit: accepted=%v blockers=%v preparation=%+v calls=%d previews=%d", out.Accepted, out.Blockers, out.Preparation, broker.count(), previews.Load())
	}
	if out.PreviewTokenID != p.PreviewTokenID || out.Preparation.DraftFingerprint != p.Preparation.DraftFingerprint || !out.Preparation.ExpiresAt.Equal(p.Preparation.ExpiresAt) || !reflect.DeepEqual(out.Place.Draft, p.Preview.Draft) {
		t.Fatal("submitted a different draft or identity")
	}
	expected := previewIBKROrder(p.Preview.Draft)
	actual := broker.orders[0]
	expected.OrderID, expected.ClientID, expected.Account = actual.OrderID, actual.ClientID, actual.Account
	if !reflect.DeepEqual(*expected, actual) {
		t.Fatal("broker payload differed from the reviewed draft")
	}
	retry := preparedSubmit(t, rig, p)
	if retry.Accepted || retry.Preparation == nil || retry.Preparation.State != "consumed" || retry.Proposal.Key != p.Proposal.Key || retry.Proposal.Revision != p.Proposal.Revision || broker.count() != 1 || previews.Load() != 1 {
		t.Fatal("consumed retry lost original identity or sent again")
	}
	status, err := rig.engine.PreparedStatus(t.Context(), p.PreparedRef)
	if err != nil || status.Order == nil || !status.Order.Found || status.Preparation.ID != p.Preparation.ID {
		t.Fatal("passive status lost durable receipt")
	}
}

func TestPreparedProposalConcurrentRedemptionHasOneWinner(t *testing.T) {
	rig, p, broker, previews := preparedTestRig(t)
	var wg sync.WaitGroup
	accepted := atomic.Int32{}
	for range 12 {
		wg.Go(func() {
			if preparedSubmit(t, rig, p).Accepted {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 || broker.count() != 1 || previews.Load() != 1 {
		t.Fatalf("winners=%d sends=%d previews=%d", accepted.Load(), broker.count(), previews.Load())
	}
}

func TestPreparedProposalRestartAndUncertainSendNeverRepreview(t *testing.T) {
	rig, p, broker, previews := preparedTestRig(t)
	reopenPreparedRig(t, rig)
	var sends atomic.Int32
	rig.server.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		sends.Add(1)
		return ibkrlib.WithSendDisposition(errors.New("synthetic connection lost"), ibkrlib.SendDispositionMayHaveWritten)
	}
	out := preparedSubmit(t, rig, p)
	if out.Accepted || sends.Load() != 1 || out.Preparation == nil || out.Preparation.State != "consumed" || out.Order == nil || !out.Order.Found || out.Order.Order.LifecycleStatus != rpc.OrderLifecycleUnknownReconcileRequired {
		t.Fatalf("uncertain send lost: blockers=%v preparation=%+v order=%+v", out.Blockers, out.Preparation, out.Order)
	}
	reopenPreparedRig(t, rig)
	rig.advance(11 * time.Minute)
	rig.server.platformSettings = frozenPlatformSettings()
	retry := preparedSubmit(t, rig, p)
	if retry.Accepted || retry.Preparation == nil || retry.Preparation.State != "consumed" || sends.Load() != 1 || previews.Load() != 1 || broker.count() != 0 {
		t.Fatal("restart, expiry or freeze allowed consumed reference to send/reprepare")
	}
}

func TestPreparedProposalRefusalsNeverCreateAnotherPreview(t *testing.T) {
	for _, kind := range []string{"revision", "terms", "quantity", "scope", "expiry", "freeze", "disabled", "reference", "missing_store", "policy", "portfolio", "duplicate", "signer", "origin"} {
		t.Run(kind, func(t *testing.T) {
			rig, p, broker, previews := preparedTestRig(t)
			params := rpc.TradeProposalSubmitParams{PreparedRef: p.PreparedRef, Key: p.Proposal.Key, Revision: p.Proposal.Revision, FastPath: true, Origin: rpc.OrderOriginHumanTTY}
			switch kind {
			case "revision":
				rig.install()
			case "terms":
				original := rig.engine.revalidateForTest
				rig.engine.revalidateForTest = func(ctx context.Context, k, r string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
					prop, blockers, err := original(ctx, k, r)
					prop.Contract.Symbol = "CHANGED"
					return prop, blockers, err
				}
			case "quantity":
				params.Quantity = 1
			case "scope":
				rig.server.cfg.Gateway.Account = "DU7654321"
				rig.server.endpoint.Account = "DU7654321"
			case "expiry":
				rig.advance(11 * time.Minute)
			case "freeze":
				rig.server.platformSettings = frozenPlatformSettings()
			case "disabled":
				rig.server.cfg.AutoTrade.FastPathEnabled = new(false)
			case "reference":
				params.PreparedRef += "changed"
			case "missing_store":
				rig.server.coreStore = nil
			case "policy":
				rig.engine.revalidateForTest = func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
					return p.Proposal, preparedBlocker("policy_unavailable", "synthetic changed policy"), nil
				}
			case "portfolio":
				rig.server.orderPreviewPositionImpact = fixedPreviewPosition(20, -20, rpc.OrderPositionEffectFlip)
			case "duplicate":
				ev := orderJournalEventForDraft(p.Preview.Draft, orderJournalEventBrokerAcknowledged, rig.server.currentTradingStatus(), "other-preview", 7001, rig.now)
				ev.OrderRef, ev.Status, ev.Remaining = "synthetic-competing-protection", "Submitted", float64(p.Preview.Draft.Quantity)
				if err := rig.server.orderJournal.Append(ev); err != nil {
					t.Fatal(err)
				}
			case "signer":
				if err := rig.server.orderTokens.bindAuthority("changed-test-epoch", 2); err != nil {
					t.Fatal(err)
				}
			case "origin":
				params.Origin = rpc.OrderOriginDaemonPreAuthorised
			}
			raw, _ := json.Marshal(params)
			out, err := rig.server.handleTradeProposalsSubmit(t.Context(), &rpc.Request{Params: raw})
			if err != nil || out.Accepted || len(out.Blockers) == 0 || broker.count() != 0 || previews.Load() != 1 {
				t.Fatalf("refusal %s: err=%v accepted=%v blockers=%v calls=%d previews=%d", kind, err, out.Accepted, out.Blockers, broker.count(), previews.Load())
			}
		})
	}
}

func TestPreparedProposalPersistenceFailureReturnsNoCapability(t *testing.T) {
	rig, _, broker, _ := preparedTestRig(t)
	path := rig.server.orderJournal.Path
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(path), "authority", filepath.Base(path)+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Fail only the new preparation insert, after the ordinary preview succeeds.
	if _, err := db.Exec(`CREATE TRIGGER reject_test_preparation BEFORE INSERT ON state_documents
 WHEN NEW.kind='prepared_proposal_v1' BEGIN SELECT RAISE(ABORT, 'synthetic persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	prop := rig.engine.Snapshot(false).Proposals[0]
	out, err := rig.engine.Prepare(t.Context(), rpc.TradeProposalPreviewParams{Key: prop.Key, Revision: prop.Revision})
	if err == nil || out.Accepted || out.SubmitEligible || out.PreparedRef != "" || out.Preparation != nil || broker.count() != 0 || len(out.Blockers) != 1 || out.Blockers[0].Code != "preparation_not_persisted" {
		t.Fatalf("failed durable preparation escaped: accepted=%v capability=%v blockers=%v err=%v", out.Accepted, out.PreparedRef != "", out.Blockers, err)
	}
}

func TestPreparedProposalMissingAuthorityRefusesPreparation(t *testing.T) {
	rig, _, broker, _ := preparedTestRig(t)
	rig.server.coreStore = nil
	prop := rig.engine.Snapshot(false).Proposals[0]
	out, _ := rig.engine.Prepare(t.Context(), rpc.TradeProposalPreviewParams{Key: prop.Key, Revision: prop.Revision})
	if out.Accepted || out.SubmitEligible || out.PreparedRef != "" || out.Preparation != nil || broker.count() != 0 || len(out.Blockers) == 0 {
		t.Fatal("missing authority issued a capability")
	}
}

func TestPreparedProposalUnreadableAuthorityIsNotUnused(t *testing.T) {
	rig, p, _, _ := preparedTestRig(t)
	if err := rig.core.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := rig.engine.PreparedStatus(t.Context(), p.PreparedRef)
	if err != nil || len(status.Blockers) == 0 || status.Preparation != nil && status.Preparation.Consumed != nil && !*status.Preparation.Consumed {
		t.Fatal("unreadable authority reported an unused capability")
	}
}

func TestPreparedProposalLateFreezeKeepsConsumedReceiptAndNeverRetries(t *testing.T) {
	rig, p, broker, previews := preparedTestRig(t)
	rig.server.orderWriteBeforeBrokerSend = func() { rig.server.platformSettings = frozenPlatformSettings() }
	out := preparedSubmit(t, rig, p)
	if out.Accepted || out.Preparation == nil || out.Preparation.State != "consumed" || out.Order == nil || !out.Order.Found || broker.count() != 0 {
		t.Fatal("late freeze did not preserve the spent, unsent attempt")
	}
	rig.server.platformSettings = nil
	rig.server.orderWriteBeforeBrokerSend = nil
	retry := preparedSubmit(t, rig, p)
	if retry.Accepted || broker.count() != 0 || previews.Load() != 1 {
		t.Fatal("lifting freeze retried the prepared attempt")
	}
}

func TestPreparedProposalStatusIsPassiveAndNeverEchoesCapability(t *testing.T) {
	rig, p, _, previews := preparedTestRig(t)
	preparedSubmit(t, rig, p)
	before := rig.revalidations
	inventory := rig.inventoryReads
	status, err := rig.engine.PreparedStatus(t.Context(), p.PreparedRef)
	raw, marshalErr := json.Marshal(status)
	if err != nil || marshalErr != nil || status.Preparation == nil || status.Preparation.State != "consumed" || rig.revalidations != before || rig.inventoryReads != inventory || previews.Load() != 1 {
		t.Fatal("passive receipt performed an active operation")
	}
	if bytes.Contains(raw, []byte(p.PreparedRef)) || bytes.Contains(raw, []byte("ibkrp4.")) {
		t.Fatal("status exposed a private capability")
	}
}
