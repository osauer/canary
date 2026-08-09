package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"math"
	"os"
	"path/filepath"

	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4001), ClientID: new(15)}}
	s := &Server{
		cfg: cfg, endpoint: discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15, PortOrigin: discover.OriginPinned},
		version: "test", streams: map[string]context.CancelFunc{}, logger: NewLogger(&bytes.Buffer{}, "error"),
		expiryIVs: newExpiryIVCache(), quoteLiquidity: newQuoteLiquidityCache(), prevCloses: newPrevCloseCache(), zeroGamma: newGammaZeroCache(),
	}
	s.installSubs()
	return s
}

func TestDecorateExactPreviewOptionUsesUSOptionsSessionAndQuality(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load New York location: %v", err)
	}
	bid, ask := 5.00, 5.20
	contract := rpc.ContractParams{ConID: 900001, Symbol: "SPY", SecType: "OPT", Currency: "USD"}
	q := &rpc.Quote{
		Symbol: contract.Symbol, Contract: contract, Bid: &bid, Ask: &ask,
		DataType: rpc.MarketDataLive, AsOf: time.Date(2026, 5, 26, 8, 15, 0, 0, loc),
	}
	(&Server{}).decorateExactPreviewQuote(q, contract)
	if q.SessionContext == nil {
		t.Fatal("off-hours exact option quote lacks options-session context")
	}
	if q.QuoteQuality == "" || !q.Indicative {
		t.Fatalf("exact option quality=%q indicative=%t, want decorated indicative state", q.QuoteQuality, q.Indicative)
	}
	if q.SpreadPct == nil || *q.SpreadPct <= 0 {
		t.Fatalf("exact option spread=%v, want decorated positive spread", q.SpreadPct)
	}
	foundOffHours := false
	for _, warning := range q.WarningDetails {
		foundOffHours = foundOffHours || warning.Code == "off_hours_quote"
	}
	if !foundOffHours {
		t.Fatalf("exact option warnings=%+v, want off_hours_quote", q.WarningDetails)
	}
	if err := requireFreshPreviewQuote(orderQuoteSnapshotFromQuote(q), "patient-limit"); err == nil || !strings.Contains(err.Error(), "open market session") {
		t.Fatalf("off-hours exact option fresh-quote gate err=%v, want open-session refusal", err)
	}
}

func TestClassifyPositionEffectBlocksShortFlip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		before float64
		after  float64
		want   string
		block  bool
	}{
		{0, 10, rpc.OrderPositionEffectOpen, false},
		{10, 3, rpc.OrderPositionEffectReduce, false},
		{10, 0, rpc.OrderPositionEffectClose, false},
		{10, -2, rpc.OrderPositionEffectFlip, true},
		{0, -1, rpc.OrderPositionEffectOpenShort, true},
	}
	for _, tc := range cases {
		got := classifyPositionEffect(tc.before, tc.after)
		if got != tc.want {
			t.Fatalf("classifyPositionEffect(%v,%v) = %q, want %q", tc.before, tc.after, got, tc.want)
		}
		if stockShortOrFlip(got) != tc.block {
			t.Fatalf("stockShortOrFlip(%q) = %v, want %v", got, stockShortOrFlip(got), tc.block)
		}
	}
}

func TestOrderTokenSignerBindsDraft(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 8, 30, 0, 0, time.UTC)
	signer, err := newOrderTokenSigner(t.TempDir()+"/order-preview-key", func() time.Time { return now })
	if err != nil {
		t.Fatalf("newOrderTokenSigner: %v", err)
	}
	draft := rpc.OrderDraft{
		Action:     rpc.OrderActionBuy,
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		Quantity:   10,
		OrderType:  rpc.OrderTypeLMT,
		LimitPrice: 100.12,
		TIF:        rpc.OrderTIFDay,
		Strategy:   rpc.OrderStrategyPatientLimit,
		OrderRef:   "ibkr-20260528-083000",
	}
	token, tokenID, expiresAt, err := signer.mint(orderPreviewTokenPayload{
		Mode:         "paper",
		Account:      "DU1234567",
		Endpoint:     "127.0.0.1:4002",
		ClientID:     31,
		Draft:        draft,
		Quote:        rpc.OrderQuoteSnapshot{Symbol: "AAPL"},
		Position:     rpc.OrderPositionImpact{Before: 0, After: 10, Effect: rpc.OrderPositionEffectOpen},
		Notional:     1001.20,
		WhatIf:       previewWhatIfUnavailable(),
		WhatIfStatus: rpc.OrderWhatIfStatusUnavailable,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tokenID == "" || token == "" {
		t.Fatalf("mint returned empty token or id")
	}
	if !expiresAt.Equal(now.Add(orderPreviewTokenTTL)) {
		t.Fatalf("expiresAt = %s, want %s", expiresAt, now.Add(orderPreviewTokenTTL))
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != orderPreviewTokenPrefix {
		t.Fatalf("token shape = %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload orderPreviewTokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.TokenID != tokenID || payload.Draft.Contract.Symbol != "AAPL" || payload.Draft.LimitPrice != 100.12 {
		t.Fatalf("payload did not bind draft/token: %+v", payload)
	}
	if payload.Quote.Symbol != "AAPL" || payload.Position.Effect != rpc.OrderPositionEffectOpen || payload.Notional != 1001.20 || payload.WhatIf.Status != rpc.OrderWhatIfStatusUnavailable {
		t.Fatalf("payload did not bind preview evidence: %+v", payload)
	}
	verified, err := signer.verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.TokenID != tokenID || verified.Draft.Contract.Symbol != "AAPL" {
		t.Fatalf("verified payload mismatch: %+v", verified)
	}
}

func TestConfirmPreviewTokenForPlaceRequiresAcceptedWhatIf(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	token := mintPreviewTokenForConfirmTest(t, srv, previewWhatIfUnavailable())

	_, err := srv.confirmPreviewTokenForPlace(token)
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "accepted broker WhatIf") {
		t.Fatalf("confirmPreviewTokenForPlace err = %v, want accepted WhatIf blocker", err)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("journal events = %+v, want none on rejected confirmation", events)
	}
}

func TestConfirmPreviewTokenForPlaceIsSingleUse(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{
		Status:            rpc.OrderWhatIfStatusAccepted,
		Available:         true,
		RequiredForSubmit: false,
	})

	payload, err := srv.confirmPreviewTokenForPlaceWithOrderID(token, 1001, "test broker transmit")
	if err != nil {
		t.Fatalf("confirmPreviewTokenForPlace first use: %v", err)
	}
	if payload.TokenID == "" || payload.Draft.Contract.Symbol == "" {
		t.Fatalf("confirmed payload missing token/draft: %+v", payload)
	}
	_, err = srv.confirmPreviewTokenForPlaceWithOrderID(token, 1001, "test broker transmit")
	if !errors.Is(err, ErrTradingDisabled) || !errors.Is(err, errOrderPreviewTokenAlreadyUsed) {
		t.Fatalf("confirmPreviewTokenForPlace second use err = %v, want token-used blocker", err)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 1 || events[0].Type != orderJournalEventTokenConfirmed || events[0].PreviewTokenID != payload.TokenID {
		t.Fatalf("journal events = %+v, want one token-confirmed event", events)
	}
}

func TestOrderPreviewRejectsMaxNotional(t *testing.T) {
	t.Parallel()
	tr := config.Trading{Mode: config.TradingModePaper, MaxNotional: 500}
	srv := newOrderPreviewTestServer(t, tr)
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 6, rpc.OrderPositionEffectOpen)

	limit := 100.0
	_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:     "buy",
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:   6,
		LimitPrice: &limit,
	})
	var bad *badRequestError
	if !errors.As(err, &bad) || !strings.Contains(err.Error(), "max_notional") {
		t.Fatalf("previewOrder err = %v, want max_notional bad request", err)
	}
}

func TestOrderPreviewAllowsSingleLegOption(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	bid := 2.05
	ask := 2.15
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		if c.SecType != "OPT" || c.Expiry != "20260619" || c.Right != "C" || c.Strike != 520 {
			t.Fatalf("option contract not preserved: %+v", c)
		}
		return rpc.OrderQuoteSnapshot{Symbol: "SPY_20260619C520", Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}, nil
	}
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)

	limit := 2.10
	res, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action: "buy",
		Contract: rpc.ContractParams{
			Symbol: "SPY", SecType: "OPT", Expiry: "20260619", Right: "C", Strike: 520, Multiplier: 100,
		},
		Quantity:   1,
		LimitPrice: &limit,
	})
	if err != nil {
		t.Fatalf("previewOrder option: %v", err)
	}
	if res.Draft.OpenClose != "C" || res.Notional != 210 {
		t.Fatalf("option preview open_close/notional = %q %.2f, want C 210.00", res.Draft.OpenClose, res.Notional)
	}
}

func newOrderPreviewTestServer(t *testing.T, trading config.Trading) *Server {
	t.Helper()
	now := time.Date(2026, 5, 28, 8, 45, 0, 0, time.UTC)
	signer, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "order-preview-key"), func() time.Time { return now })
	if err != nil {
		t.Fatalf("newOrderTokenSigner: %v", err)
	}
	journal := newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl"))
	authority, err := journal.coreStore()
	if err != nil {
		t.Fatalf("test order authority: %v", err)
	}
	head, err := authority.AuthorityHead(t.Context())
	if err != nil {
		t.Fatalf("test order authority head: %v", err)
	}
	if err := signer.bindAuthority(head.AuthorityEpoch, head.SignerGeneration); err != nil {
		t.Fatalf("bind test signer: %v", err)
	}
	trading = trading.WithDefaults()
	return &Server{
		cfg: &config.Resolved{
			Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4002), ClientID: new(31), Account: "DU1234567"},
			Trading: trading,
		},
		endpoint:               discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 31, Account: "DU1234567", PortOrigin: discover.OriginPinned},
		now:                    func() time.Time { return now },
		orderJournal:           journal,
		orderTokens:            signer,
		coreStore:              authority,
		gatewayReadyForTrading: func() bool { return true },
	}
}

func fixedPreviewQuote(bid, ask float64) func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
	return func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{
			Symbol:       "AAPL",
			Bid:          &bid,
			Ask:          &ask,
			Midpoint:     &mid,
			DataType:     rpc.MarketDataLive,
			QuoteQuality: "firm",
		}, nil
	}
}

func fixedPreviewPosition(before, after float64, effect string) func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error) {
	return func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error) {
		return rpc.OrderPositionImpact{Before: before, After: after, Effect: effect}, nil
	}
}

func mintPreviewTokenForConfirmTest(t *testing.T, srv *Server, whatIf rpc.OrderWhatIfResult) string {
	t.Helper()
	if whatIf.Status == "" {
		whatIf = previewWhatIfUnavailable()
	}
	draft := rpc.OrderDraft{
		Action:     rpc.OrderActionBuy,
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		Quantity:   1,
		OrderType:  rpc.OrderTypeLMT,
		LimitPrice: 100,
		TIF:        rpc.OrderTIFDay,
		Strategy:   rpc.OrderStrategyExplicitLimit,
		OrderRef:   "ibkr-20260528-084500",
	}
	token, _, _, err := srv.orderTokens.mint(orderPreviewTokenPayload{
		Mode:     "paper",
		Account:  "DU1234567",
		Endpoint: "127.0.0.1:4002",
		ClientID: 31,
		Draft:    draft,
		Quote:    rpc.OrderQuoteSnapshot{Symbol: "AAPL"},
		Position: rpc.OrderPositionImpact{
			Before: 0,
			After:  1,
			Effect: rpc.OrderPositionEffectOpen,
		},
		Notional:     100,
		WhatIf:       whatIf,
		WhatIfStatus: whatIf.Status,
	})
	if err != nil {
		t.Fatalf("mint preview token: %v", err)
	}
	return token
}

func testAccountSnapshot(at time.Time, value float64) *ibkrlib.RawAccountSummary {
	return &ibkrlib.RawAccountSummary{
		AccountID: "DU123", Currency: "USD", NetLiquidation: &value, AsOf: at,
		CurrencyLedger: map[string]ibkrlib.CurrencyLedger{"EUR": {ExchangeRate: 1.1}},
		Raw:            map[string]string{"NetLiquidation": "100"},
	}
}

func TestAccountSnapshotAuthorityDoesNotCrossBrokerScope(t *testing.T) {
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	authority := accountSnapshotAuthority{now: func() time.Time { return now }}
	var calls atomic.Int32
	fetch := func(context.Context) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error) {
		calls.Add(1)
		return testAccountSnapshot(now, 100), ibkrlib.AccountSummaryProvenanceRequest, nil
	}
	first := accountSnapshotSource{scope: brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}}
	second := accountSnapshotSource{scope: brokerStateScope{Account: "DU123", Mode: rpc.AccountModeLive}}
	if _, err := authority.read(t.Context(), t.Context(), first, fetch); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.read(t.Context(), t.Context(), second, fetch); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("broker fetches across paper/live scopes = %d, want 2", got)
	}
}

func newTestOrderJournalStore(t *testing.T, path string) *orderJournalStore {
	t.Helper()
	dbPath := filepath.Join(filepath.Dir(path), "authority", filepath.Base(path)+".db")
	store, err := corestore.Open(context.Background(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open test order authority: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	journal := newOrderJournalStore(path)
	if err := journal.UseCoreStore(store); err != nil {
		t.Fatalf("attach test order authority: %v", err)
	}
	return journal
}

func TestInitializeFreshTradingAuthorityNeverReadsAdjacentLegacyFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyOrderPath := filepath.Join(dir, "order-journal.jsonl")
	legacyPurgePath := filepath.Join(dir, "purge-ledger.json")
	if err := os.WriteFile(legacyOrderPath, []byte("malformed legacy order data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPurgePath, []byte("malformed legacy purge data\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	authority, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(dir, "daemon.db")})
	if err != nil {
		t.Fatalf("open authority: %v", err)
	}
	defer authority.Close()
	if err := initializeFreshTradingAuthority(t.Context(), authority); err != nil {
		t.Fatalf("initialize fresh trading authority: %v", err)
	}
	events, err := authority.LoadOrderEvents(t.Context(), corestore.OrderQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("fresh authority imported adjacent order events: %+v", events)
	}
	if err := initializeFreshTradingAuthority(t.Context(), authority); !errors.Is(err, corestore.ErrFreshAuthorityConflict) {
		t.Fatalf("second initialization error = %v, want fresh-authority conflict", err)
	}
}

func TestPreviewTokenRedemptionHasOneWinnerAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "authority", "daemon.db")
	authority, err := corestore.Open(ctx, corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open authority: %v", err)
	}
	journal := newOrderJournalStore(filepath.Join(t.TempDir(), "legacy.jsonl"))
	if err := journal.UseCoreStore(authority); err != nil {
		t.Fatalf("attach authority: %v", err)
	}
	head, err := authority.AuthorityHead(ctx)
	if err != nil {
		t.Fatalf("authority head: %v", err)
	}
	const contenders = 24
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- journal.StagePreTransmit("one-token", head.AuthorityEpoch, head.SignerGeneration, 1001, corestore.ActionPlace, corestore.OriginAgentCLI, []orderJournalEvent{{
				At: time.Date(2026, 7, 20, 11, 0, 0, i, time.UTC), Type: orderJournalEventSendAttempted,
				OrderRef: "one-winner", PreviewTokenID: "one-token", ReservedOrderID: 1001,
				Endpoint: "127.0.0.1:4002", ClientID: 31, Account: "DU123", Mode: "paper",
			}})
		}(i)
	}
	wg.Wait()
	close(results)
	winners, consumed := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, errOrderPreviewTokenAlreadyUsed):
			consumed++
		default:
			t.Fatalf("unexpected contender error: %v", err)
		}
	}
	if winners != 1 || consumed != contenders-1 {
		t.Fatalf("winners=%d consumed=%d, want 1/%d", winners, consumed, contenders-1)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("close authority: %v", err)
	}
	authority, err = corestore.Open(ctx, corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("reopen authority: %v", err)
	}
	defer authority.Close()
	reopened := newOrderJournalStore("")
	if err := reopened.UseCoreStore(authority); err != nil {
		t.Fatalf("attach reopened authority: %v", err)
	}
	head, err = authority.AuthorityHead(ctx)
	if err != nil {
		t.Fatalf("reopened head: %v", err)
	}
	err = reopened.StagePreTransmit("one-token", head.AuthorityEpoch, head.SignerGeneration, 1002, corestore.ActionPlace, corestore.OriginAgentCLI, []orderJournalEvent{{
		At: time.Date(2026, 7, 20, 11, 1, 0, 0, time.UTC), Type: orderJournalEventSendAttempted,
		OrderRef: "restart-loser", PreviewTokenID: "one-token", ReservedOrderID: 1002,
		Endpoint: "127.0.0.1:7497", ClientID: 44, Account: "DU999", Mode: "paper",
	}})
	if !errors.Is(err, errOrderPreviewTokenAlreadyUsed) {
		t.Fatalf("restarted redemption err = %v, want consumed", err)
	}
}

func TestOrderLifecycleReceiptFromUnpublishedConnectorLeavesJournalAndLatchesUntilCurrentReconcile(t *testing.T) {
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "stale-a", 701, now.Add(-2*time.Hour))

	srv.orderLifecycleSessionCurrentForTest = func(*ibkrlib.Connector, ibkrlib.ConnectorSessionBinding) bool { return true }
	connectorA := &ibkrlib.Connector{}
	connectorB := &ibkrlib.Connector{}
	srv.mu.Lock()
	srv.connector = connectorA
	srv.connectorEpoch = 1
	srv.mu.Unlock()
	srv.registerOrderLifecycleJournal(connectorA)
	srv.orderLifecycleHandlersMu.Lock()
	bindingA := srv.orderLifecycleHandlers[connectorA]
	srv.orderLifecycleHandlersMu.Unlock()
	if bindingA == nil {
		t.Fatal("missing lifecycle binding for Connector A")
	}
	handlerA := srv.boundOrderLifecycleHandler(connectorA, bindingA)
	before, err := srv.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatalf("journal head before stale receipt: %v", err)
	}

	if !srv.withConnectorEvidencePublication(connectorA, connectorB, func() {
		srv.connector = connectorB
		srv.connectorEpoch = 2
	}) {
		t.Fatal("Connector A unpublication did not apply")
	}

	handlerA(ibkrlib.OrderLifecycleReceipt{Event: ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventStatus, OrderID: 78, PermID: 701,
		ClientID: 15, ClientIDPresent: true, Status: "Cancelled",
	}})
	after, err := srv.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatalf("journal head after stale receipt: %v", err)
	}
	if after.LastEventSeq != before.LastEventSeq {
		t.Fatalf("stale Connector A receipt changed journal head %d -> %d", before.LastEventSeq, after.LastEventSeq)
	}
	if got := srv.orderLifecyclePersistenceFailures.Load(); got != 1 || !srv.orderLifecyclePersistenceUncertain.Load() {
		t.Fatalf("stale receipt failures=%d latch=%v, want 1/true", got, srv.orderLifecyclePersistenceUncertain.Load())
	}

	srv.orderSnapshotFn = reconcileTestSnapshot(true)
	srv.stableBrokerEvidenceForTest = func(binding daemonBrokerEvidenceBinding, commit func() error) (bool, error) {
		srv.mu.Lock()
		current := srv.connector == connectorB && srv.connectorEpoch == 2
		srv.mu.Unlock()
		if !current || binding.connector != connectorB || binding.connectorEpoch != 2 {
			return false, nil
		}
		return true, commit()
	}
	srv.reconcileOrderJournalWithBroker(context.Background())
	if srv.orderLifecyclePersistenceUncertain.Load() {
		t.Fatal("stable complete reconcile from current Connector B did not clear lifecycle persistence latch")
	}
}

func newOrderReconcileTestServer(t *testing.T, now time.Time) *Server {
	t.Helper()
	srv := newTestServer(t)
	srv.cfg.Gateway.Account = "DU1234567"
	srv.cfg.Trading.Mode = config.TradingModePaper
	srv.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl"))
	srv.now = func() time.Time { return now }
	return srv
}

func seedReconcileGhostRow(t *testing.T, srv *Server, ref string, permID int, at time.Time, extra ...orderJournalEvent) {
	t.Helper()
	base := orderJournalEvent{
		At:              at,
		Type:            orderJournalEventBrokerAcknowledged,
		OrderRef:        ref,
		ReservedOrderID: 78,
		ClientID:        15,
		PermID:          permID,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4001",
		Mode:            "paper",
		Symbol:          "AMD",
		SecType:         "STK",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeTRAIL,
		TIF:             rpc.OrderTIFGTC,
		Quantity:        20,
		Status:          "PreSubmitted",
		Remaining:       20,
		SendState:       orderSendStateBrokerAcknowledged,
	}
	if err := srv.orderJournal.Append(base); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	for _, ev := range extra {
		if ev.Endpoint == "" {
			ev.Endpoint = base.Endpoint
		}
		if ev.ClientID == 0 {
			ev.ClientID = base.ClientID
		}
		if ev.Account == "" {
			ev.Account = base.Account
		}
		if ev.Mode == "" {
			ev.Mode = base.Mode
		}
		if err := srv.orderJournal.Append(ev); err != nil {
			t.Fatalf("seed extra journal event: %v", err)
		}
	}
}

func reconcileTestSnapshot(complete bool, permIDs ...int) func(context.Context) (ibkrlib.OpenOrderSnapshot, error) {
	return func(context.Context) (ibkrlib.OpenOrderSnapshot, error) {
		snap := ibkrlib.OpenOrderSnapshot{Complete: complete, AsOf: time.Now().UTC()}
		for _, id := range permIDs {
			snap.Orders = append(snap.Orders, ibkrlib.OrderLifecycleEvent{
				Type:    ibkrlib.OrderLifecycleEventOpenOrder,
				OrderID: 9000 + id,
				PermID:  id,
			})
		}
		return snap, nil
	}
}

func loadSingleOrderView(t *testing.T, srv *Server, ref string) rpc.OrderView {
	t.Helper()
	views, _, err := srv.loadOrderViews()
	if err != nil {
		t.Fatalf("loadOrderViews: %v", err)
	}
	for _, v := range views {
		if v.OrderRef == ref {
			return v
		}
	}
	t.Fatalf("order view %q not found", ref)
	return rpc.OrderView{}
}

func TestReconcileHeadCASRejectsLateJournalMutation(t *testing.T) {
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "ghost-cas", 558, now.Add(-2*time.Hour))
	srv.orderSnapshotFn = reconcileTestSnapshot(true)
	srv.orderReconcileBeforeCommit = func() {
		if err := srv.orderJournal.Append(orderJournalEvent{
			At: now, Type: orderJournalEventStatusUpdated, OrderRef: "ghost-cas",
			ReservedOrderID: 78, ClientID: 15, PermID: 558, Account: "DU1234567",
			Endpoint: "127.0.0.1:4001", Mode: "paper", Status: "Submitted",
			Remaining: 20, SendState: orderSendStateBrokerAcknowledged,
		}); err != nil {
			t.Fatal(err)
		}
	}

	srv.reconcileOrderJournalWithBroker(t.Context())

	if view := loadSingleOrderView(t, srv, "ghost-cas"); !view.Open || view.LifecycleStatus == rpc.OrderLifecycleClosedReconciled {
		t.Fatalf("stale reconciliation closed a later journal head: %+v", view)
	}
}

func TestPlatformSettingsTradingPatchOriginMatrix(t *testing.T) {
	t.Parallel()
	modes := []string{config.TradingModeDisabled, config.TradingModePaper, config.TradingModeLive}
	origins := []struct {
		name    string
		field   string
		allowed bool
	}{
		{name: "missing"},
		{name: "agent", field: `,"origin":"agent"`},
		{name: "human tty", field: `,"origin":"human-tty"`, allowed: true},
		{name: "paired device", field: `,"origin":"human-paired-device"`},
	}
	for _, mode := range modes {
		for _, origin := range origins {
			t.Run(mode+"/"+origin.name, func(t *testing.T) {
				srv := newPlatformSettingsTestServer(t, config.Trading{Mode: mode})
				params := `{"trading":{"freeze":true}` + origin.field + `}`
				_, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(params)})
				if !origin.allowed {
					if err == nil || !strings.Contains(err.Error(), "terminal-only") {
						t.Fatalf("trading patch err=%v, want terminal-only refusal", err)
					}
					if srv.tradingFrozen() || srv.platformSettings.tradingControlGeneration() != 0 {
						t.Fatal("refused origin mutated trading controls")
					}
					return
				}
				if err != nil {
					t.Fatalf("human trading patch: %v", err)
				}
				if !srv.tradingFrozen() || srv.platformSettings.tradingControlGeneration() != 1 {
					t.Fatal("human trading patch did not publish one control generation")
				}
			})
		}
	}

	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModeLive})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"features":{"stock_protection":{"enabled":true}},"origin":"agent"}`)}); err != nil {
		t.Fatalf("live agent feature patch err = %v, want success", err)
	}
}

func newPlatformSettingsTestServer(t *testing.T, tr config.Trading) *Server {
	t.Helper()
	store, err := newPlatformSettingsStore(filepath.Join(t.TempDir(), "platform-settings.json"))
	if err != nil {
		t.Fatalf("newPlatformSettingsStore: %v", err)
	}
	return &Server{
		cfg:              &config.Resolved{Trading: tr},
		platformSettings: store,
	}
}

func TestProtectionPolicyInvalidHigherVersionBlocksWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.toml")
	writePolicy(t, path, 2, -1)
	pm := newProtectionPolicyManager(path, true, time.Second, time.Now)
	pm.reload()
	_, st := pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusError {
		t.Fatalf("invalid policy status=%q, want error", st.Status)
	}
	if len(st.Blockers) == 0 {
		t.Fatal("invalid policy should expose blockers")
	}
}

func writePolicy(t *testing.T, path string, version int, theta float64) {
	t.Helper()
	body := []byte(`kind = "ibkr.protection_policy"
schema_version = 1
policy_id = "protection-mvp"
policy_version = ` + strconv.Itoa(version) + `
profile = "theta-priority-mvp"

[authority]
close_reduce_only = true
auto_submit = false

[buckets.theta_hygiene]
enabled = true
max_dte = 21
min_abs_theta_per_day = ` + strconv.FormatFloat(theta, 'f', 1, 64) + `
max_spread_pct_of_mid = 25.0

[buckets.risk_reduction]
enabled = true
single_name_target_pct_nlv = 25.0
max_order_notional = 10000.0
`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func reduceTestPortfolio(stocks, options []rpc.PositionView) *rpc.PositionsResult {
	return &rpc.PositionsResult{
		Stocks:    stocks,
		Options:   options,
		Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "USD"},
	}
}

func reduceFindCandidate(cands []reduceSweepCandidate, conID int) (reduceSweepCandidate, bool) {
	for _, c := range cands {
		if c.row.ConID == conID {
			return c, true
		}
	}
	return reduceSweepCandidate{}, false
}

func approxEqual(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestReduceSweep100PercentDoesNotFullyCloseWithHedgePresent(t *testing.T) {
	hedgeDelta := -0.5
	hedgeUnderlying := 100.0
	pos := reduceTestPortfolio(
		[]rpc.PositionView{
			{Symbol: "AAPL", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 1000, Mark: 50},
		},
		[]rpc.PositionView{

			{Symbol: "SPY", SecType: "OPTION", ConID: 2, Currency: "USD", Quantity: 2, Right: "P", Delta: &hedgeDelta, Underlying: &hedgeUnderlying, Multiplier: 100},
		},
	)
	cands, netDelta, _, target, blockers := reduceSweepCandidates(pos, 100)
	if len(blockers) > 0 {
		t.Fatalf("unexpected blockers: %+v", blockers)
	}
	if !approxEqual(netDelta, 40000, 0.01) {
		t.Fatalf("netDelta=%v, want 40000", netDelta)
	}
	if !approxEqual(target, 40000, 0.01) {
		t.Fatalf("target=%v, want 40000 (100%% of net 40000)", target)
	}
	if _, ok := reduceFindCandidate(cands, 2); ok {
		t.Fatalf("the long put hedge must never be selected, at any percent including 100")
	}
	stock, ok := reduceFindCandidate(cands, 1)
	if !ok {
		t.Fatalf("AAPL should be a candidate")
	}
	if stock.qty != 800 {
		t.Fatalf("AAPL qty=%d, want 800 — NOT the full 1000 held, because the hedge already absorbs part of net risk", stock.qty)
	}
	if stock.qty >= 1000 {
		t.Fatalf("100%% must not mean \"close everything eligible\" when a hedge offsets net exposure")
	}
}
