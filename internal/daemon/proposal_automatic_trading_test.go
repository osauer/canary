//go:build trading

package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// newAutomaticTradingRig builds the hermetic rig over the trading-build
// test server: real preview tokens, a real order journal in daemon.db and a
// fake broker socket, so a submission travels the production write path up
// to the wire.
func newAutomaticTradingRig(t *testing.T, authority string) *automaticTestRig {
	t.Helper()
	// A protective SELL on a long stock passes the daemon's conservative
	// short check only with allow_stock_short, as on the desk's own config.
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, AllowStockShort: true})
	ctx := context.Background()
	if err := initializeCleanProposalOpportunityAuthority(ctx, srv.coreStore); err != nil {
		t.Fatalf("initialize clean authority: %v", err)
	}
	// The preview-token signer runs on the server's fixed clock; the rig
	// clock starts there and advances from it.
	rig := &automaticTestRig{t: t, core: srv.coreStore, now: srv.orderNow(), scope: brokerStateScope{Account: "DU1234567", Mode: "paper"}}
	srv.now = func() time.Time { return rig.now }
	srv.protectionPolicies = writePreAuthPolicy(t, preAuthPolicyTOML(authority, 1))
	srv.protectionPolicies.reload()
	if st := srv.protectionPolicies.Status(); st.Status != rpc.ProtectionPolicyStatusActive {
		t.Fatalf("test policy status = %+v", st)
	}
	srv.orderPreviewQuote = func(_ context.Context, contract rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 24.9, 25.1
		mid := 25.0
		return rpc.OrderQuoteSnapshot{Symbol: contract.Symbol, Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
	}
	srv.orderPreviewPositionImpact = fixedPreviewPosition(40, 0, rpc.OrderPositionEffectClose)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	nextID := 1000
	srv.orderReserveBrokerID = func(context.Context) (int, error) { nextID++; return nextID, nil }
	rig.server = srv
	rig.engine = rig.newEngine()
	srv.tradeProposals = rig.engine
	return rig
}

type brokerCallLog struct {
	mu     sync.Mutex
	orders []ibkrlib.RawOrder
}

func (l *brokerCallLog) install(srv *Server) {
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.orders = append(l.orders, *order)
		return nil
	}
}

func (l *brokerCallLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.orders)
}

func journalEventsForToken(t *testing.T, srv *Server, tokenID string) []orderJournalEvent {
	t.Helper()
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	var out []orderJournalEvent
	for _, ev := range events {
		if ev.PreviewTokenID == tokenID {
			out = append(out, ev)
		}
	}
	return out
}

func TestAutomaticPreAuthorisedBucketSubmitsExactlyOncePerKeyAndRevision(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	rig.notice(rec)
	// Inside the window nothing is placed, however many cycles run.
	rig.advance(29 * time.Minute)
	rig.cycle()
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("broker calls inside the window = %d", broker.count())
	}
	// The window closes: one order, journaled with the daemon origin.
	rig.advance(2 * time.Minute)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after the window = %d, want 1; record = %+v", broker.count(), rig.record(prop.Key, revision))
	}
	rec = rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticSubmitted || rec.SubmittedAt.IsZero() || rec.OrderRef == "" || rec.PreviewTokenID == "" {
		t.Fatalf("record after submission = %+v", rec)
	}
	attempted := false
	for _, ev := range journalEventsForToken(t, rig.server, rec.PreviewTokenID) {
		if ev.Type == orderJournalEventSendAttempted {
			attempted = true
			if ev.Origin != rpc.OrderOriginDaemonPreAuthorised || ev.Source != proposalOrderSource || ev.OrderRef != rec.OrderRef {
				t.Fatalf("journaled attempt = origin %q source %q ref %q", ev.Origin, ev.Source, ev.OrderRef)
			}
		}
	}
	if !attempted {
		t.Fatal("no send-attempted journal event for the automatic submission")
	}
	if got := broker.orders[0]; got.TotalQty != 40 || !strings.EqualFold(got.Action, rpc.OrderActionSell) {
		t.Fatalf("broker order = %+v, want SELL 40", got)
	}
	// Later cycles at the same key and revision never place again, and the
	// served row shows the outcome.
	rig.advance(time.Hour)
	rig.cycle()
	rig.restart()
	rig.install(prop)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls after restart = %d, want still 1", broker.count())
	}
	if a := rig.engine.Snapshot(false).Proposals[0].Automatic; a == nil || a.State != rpc.TradeProposalAutomaticSubmitted || a.OrderReference != rec.OrderRef {
		t.Fatalf("served automatic after submission = %+v", a)
	}
	if rig.server.automaticGrant.Load() != nil {
		t.Fatal("write grant left installed after the submission")
	}
}

func TestAutomaticLatchedBrakeSubmitsWithoutWaiting(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	rig.latched = true
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	if broker.count() != 1 {
		t.Fatalf("broker calls with the brake latched = %d, want 1 in the same cycle", broker.count())
	}
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticSubmitted || !rec.LatchSkippedWindow || !rec.NoticedAt.IsZero() {
		t.Fatalf("latched record = %+v", rec)
	}
}

func TestAutomaticFreezeBlocksSubmissionAndTheRecordSaysSo(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	broker := &brokerCallLog{}
	broker.install(rig.server)
	rig.server.platformSettings = &platformSettingsStore{data: platformSettingsData{Version: platformSettingsDocVersion, Trading: platformTradingSettingsData{Freeze: new(true)}}}
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(31 * time.Minute)
	rig.cycle()
	if broker.count() != 0 {
		t.Fatalf("frozen desk placed %d order(s)", broker.count())
	}
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticFailed || !strings.Contains(rec.Reason, tradingFrozenBlockerCode) {
		t.Fatalf("record under freeze = %+v, want failed naming %s", rec, tradingFrozenBlockerCode)
	}
	// Lifting the freeze does not retry: the key and revision are spent.
	rig.server.platformSettings = nil
	rig.advance(time.Hour)
	rig.cycle()
	if broker.count() != 0 {
		t.Fatal("a failed record was retried after the freeze lifted")
	}
}

func TestAutomaticOriginIsAcceptedOnlyUnderAGrantForAListedBucket(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, `pre_authorised = ["option_loss_exit"]`)
	status := rig.server.currentTradingStatus()
	// No scheduler grant: any caller claiming the daemon origin is refused.
	if blockers := rig.server.brokerWriteOriginBlockers(status, rpc.OrderOriginDaemonPreAuthorised); len(blockers) != 1 || blockers[0].Code != "daemon_origin_unauthorised" {
		t.Fatalf("origin blockers without grant = %+v", blockers)
	}
	auth := rig.server.brokerWriteAuthorizationForRequest(rpc.OrderOriginDaemonPreAuthorised)
	if auth.Allowed {
		t.Fatal("daemon origin authorised without a grant")
	}
	// A grant for a bucket the policy does not list is refused too.
	rig.server.automaticGrant.Store(&automaticWriteGrant{Key: "k", Revision: "r", Bucket: preAuthorisedBucketTrailingStop})
	if blockers := rig.server.brokerWriteOriginBlockers(status, rpc.OrderOriginDaemonPreAuthorised); len(blockers) != 1 || blockers[0].Code != "bucket_not_pre_authorised" {
		t.Fatalf("origin blockers for unlisted bucket = %+v", blockers)
	}
	// A grant for the listed bucket passes the origin gate; the other
	// gates are untouched by the origin.
	rig.server.automaticGrant.Store(&automaticWriteGrant{Key: "k", Revision: "r", Bucket: preAuthorisedBucketOptionLossExit})
	if blockers := rig.server.brokerWriteOriginBlockers(status, rpc.OrderOriginDaemonPreAuthorised); len(blockers) != 0 {
		t.Fatalf("origin blockers for listed bucket = %+v", blockers)
	}
	if !rig.server.brokerWriteAuthorizationForRequest(rpc.OrderOriginDaemonPreAuthorised).Allowed {
		t.Fatal("listed bucket under grant not authorised on a ready paper desk")
	}
	// Human origins never see the daemon-origin blockers.
	for _, origin := range []string{rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice, rpc.OrderOriginAgent} {
		if blockers := rig.server.brokerWriteOriginBlockers(status, origin); len(blockers) != 0 {
			t.Fatalf("origin %q blockers = %+v", origin, blockers)
		}
	}
	rig.server.automaticGrant.Store(nil)
	// A human submit of a proposal whose bucket is not listed still works
	// exactly as before: the automatic machinery never touches it.
	broker := &brokerCallLog{}
	broker.install(rig.server)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.noRecord(prop.Key, revision)
	res, err := rig.engine.Submit(context.Background(), rpc.TradeProposalSubmitParams{Key: prop.Key, Revision: revision, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
	if err != nil || !res.Accepted || broker.count() != 1 {
		t.Fatalf("human submit = %+v err = %v calls = %d", res, err, broker.count())
	}
}

func TestAutomaticRestartBetweenIntentAndBrokerAcknowledgementPlacesNoSecondOrder(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
	release := make(chan struct{})
	reached := make(chan struct{})
	calls := 0
	var callMu sync.Mutex
	rig.server.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, _ *ibkrlib.RawOrder) error {
		callMu.Lock()
		calls++
		first := calls == 1
		callMu.Unlock()
		if first {
			close(reached)
			<-release
		}
		return nil
	}
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(31 * time.Minute)
	first := rig.engine
	done := make(chan struct{})
	go func() {
		defer close(done)
		first.runAutomaticCycle(context.Background())
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		close(release)
		<-done
		t.Fatalf("first process never reached the broker; record = %+v", rig.record(prop.Key, revision))
	}
	// The first process has persisted its intent and staged the attempt in
	// the journal, and is now waiting on the broker. A second process
	// starts on the same daemon.db.
	stuck, ok := first.automatic.get(prop.Key, revision)
	if !ok || stuck.State != rpc.TradeProposalAutomaticSubmitting || stuck.PreviewTokenID == "" {
		t.Fatalf("record at broker-call time = %+v, want submitting with a token id", stuck)
	}
	rig.advance(time.Minute)
	rig.restart()
	rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticFailed || !strings.Contains(rec.Reason, "unknown across the restart") || rec.OrderRef == "" {
		t.Fatalf("recovered record = %+v", rec)
	}
	rig.advance(time.Hour)
	rig.cycle()
	close(release)
	<-done
	callMu.Lock()
	defer callMu.Unlock()
	if calls != 1 {
		t.Fatalf("broker calls across the restart = %d, want exactly 1", calls)
	}
	if final := rig.record(prop.Key, revision); final.State != rpc.TradeProposalAutomaticFailed {
		t.Fatalf("first process overwrote the recovered outcome: %+v", final)
	}
}
