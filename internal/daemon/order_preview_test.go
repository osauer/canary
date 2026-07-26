package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestPreviewLimitPriceDefaultsPatientLimit(t *testing.T) {
	t.Parallel()
	bid, ask := 100.10, 100.15
	quote := rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}

	got, err := previewLimitPrice(rpc.OrderActionBuy, rpc.OrderStrategyPatientLimit, nil, rpc.ContractParams{}, quote)
	if err != nil {
		t.Fatalf("previewLimitPrice buy: %v", err)
	}
	if got != 100.12 {
		t.Fatalf("buy patient-limit = %.4f, want 100.1200", got)
	}
	got, err = previewLimitPrice(rpc.OrderActionSell, rpc.OrderStrategyPatientLimit, nil, rpc.ContractParams{}, quote)
	if err != nil {
		t.Fatalf("previewLimitPrice sell: %v", err)
	}
	if got != 100.13 {
		t.Fatalf("sell patient-limit = %.4f, want 100.1300", got)
	}
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

func TestPreviewLimitPatientLimitOptionBandRounding(t *testing.T) {
	t.Parallel()
	opt := rpc.ContractParams{SecType: "OPT", Symbol: "MSFT"}
	cases := []struct {
		name     string
		contract rpc.ContractParams
		action   string
		bid, ask float64
		want     float64
	}{
		// Live-evidence shape (2026-07-02): nickel-quoted tape above $3.00
		// must not draft the raw penny midpoint (19.63 drew broker 110).
		{name: "sell above band on nickel tape rounds up to nickel", contract: opt, action: rpc.OrderActionSell, bid: 19.60, ask: 19.65, want: 19.65},
		{name: "buy above band on nickel tape rounds down to nickel", contract: opt, action: rpc.OrderActionBuy, bid: 19.60, ask: 19.65, want: 19.60},
		{name: "penny tape above band proves penny grid", contract: opt, action: rpc.OrderActionSell, bid: 19.62, ask: 19.64, want: 19.63},
		// Wire quotes are float32-truncated (live MSFT 260821C400 tape,
		// 2026-07-02): 19.05/19.60 read back off-grid and must not count
		// as penny proof.
		{name: "float32 wire noise is not penny proof", contract: opt, action: rpc.OrderActionSell, bid: 19.049999237060547, ask: 19.600000381469727, want: 19.35},
		{name: "below band keeps penny grid", contract: opt, action: rpc.OrderActionSell, bid: 2.40, ask: 2.43, want: 2.42},
		{name: "sub-dollar option never drafts sub-penny", contract: opt, action: rpc.OrderActionSell, bid: 0.40, ask: 0.45, want: 0.43},
		{name: "penny tape below band proves nothing above it", contract: opt, action: rpc.OrderActionBuy, bid: 2.98, ask: 3.15, want: 3.05},
		{name: "broker min-tick coarsens below band", contract: rpc.ContractParams{SecType: "OPT", Symbol: "XYZ", MinTick: 0.05}, action: rpc.OrderActionSell, bid: 2.40, ask: 2.45, want: 2.45},
		{name: "stock keeps static penny grid", contract: rpc.ContractParams{SecType: "STK", Symbol: "MSFT"}, action: rpc.OrderActionSell, bid: 19.60, ask: 19.65, want: 19.63},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			quote := rpc.OrderQuoteSnapshot{Bid: &tc.bid, Ask: &tc.ask, DataType: rpc.MarketDataLive}
			got, err := previewLimitPrice(tc.action, rpc.OrderStrategyPatientLimit, nil, tc.contract, quote)
			if err != nil {
				t.Fatalf("previewLimitPrice: %v", err)
			}
			if got != tc.want {
				t.Fatalf("%s %s bid %.2f ask %.2f = %.4f, want %.4f", tc.action, tc.contract.SecType, tc.bid, tc.ask, got, tc.want)
			}
		})
	}
}

func TestPreviewLimitRejectsDelayedPatientLimit(t *testing.T) {
	t.Parallel()
	bid, ask := 100.10, 100.15
	quote := rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataDelayed}

	if _, err := previewLimitPrice(rpc.OrderActionBuy, rpc.OrderStrategyPatientLimit, nil, rpc.ContractParams{}, quote); err == nil {
		t.Fatal("patient-limit on delayed data should fail")
	}
}

func TestPreviewLimitRejectsStaleOrClosedPatientLimit(t *testing.T) {
	t.Parallel()
	bid, ask := 100.10, 100.15
	stale := rpc.OrderQuoteSnapshot{
		Bid:         &bid,
		Ask:         &ask,
		DataType:    rpc.MarketDataLive,
		Stale:       true,
		StaleReason: "price timestamp is 20m old during market hours",
	}
	if _, err := previewLimitPrice(rpc.OrderActionBuy, rpc.OrderStrategyPatientLimit, nil, rpc.ContractParams{}, stale); err == nil || !strings.Contains(err.Error(), "fresh quote data") {
		t.Fatalf("stale patient-limit err = %v, want freshness rejection", err)
	}

	closed := rpc.OrderQuoteSnapshot{
		Bid:            &bid,
		Ask:            &ask,
		DataType:       rpc.MarketDataLive,
		SessionContext: &rpc.MarketSession{Market: "de", State: "closed", IsOpen: false},
	}
	if _, err := previewLimitPrice(rpc.OrderActionSell, rpc.OrderStrategyPatientLimit, nil, rpc.ContractParams{}, closed); err == nil || !strings.Contains(err.Error(), "open market session") {
		t.Fatalf("closed-session patient-limit err = %v, want session rejection", err)
	}
}

func TestPreviewTrailSpecUsesBidAskAndIBKRPercentUnits(t *testing.T) {
	t.Parallel()
	bid, ask, pctValue := 100.0, 101.0, 2.0
	quote := rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}
	sellTrail, err := previewTrailSpec(rpc.OrderActionSell, rpc.OrderTypeTRAIL, &rpc.OrderTrailSpec{
		OffsetType:      rpc.OrderTrailOffsetPercent,
		TrailingPercent: &pctValue,
	}, rpc.ContractParams{SecType: "STK"}, quote)
	if err != nil {
		t.Fatalf("sell previewTrailSpec: %v", err)
	}
	if sellTrail.InitialStopPrice != 98 {
		t.Fatalf("SELL initial stop = %.2f, want bid-based 98.00", sellTrail.InitialStopPrice)
	}

	buyTrail, err := previewTrailSpec(rpc.OrderActionBuy, rpc.OrderTypeTRAIL, &rpc.OrderTrailSpec{
		OffsetType:      rpc.OrderTrailOffsetPercent,
		TrailingPercent: &pctValue,
	}, rpc.ContractParams{SecType: "STK"}, quote)
	if err != nil {
		t.Fatalf("buy previewTrailSpec: %v", err)
	}
	if buyTrail.InitialStopPrice != 103.02 {
		t.Fatalf("BUY initial stop = %.2f, want ask-based 103.02", buyTrail.InitialStopPrice)
	}
}

func TestPreviewTrailSpecAcceptsUnavailableQuoteContextAndRejectsLimitRuleDrift(t *testing.T) {
	t.Parallel()
	bid, ask, pctValue, offset := 100.0, 101.0, 2.0, 0.05
	delayed := rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataDelayed}
	stock := rpc.ContractParams{SecType: "STK"}
	// A percent trail with no seedable stop can never transmit (the wire
	// validators reject trailStopPrice <= 0), so the preview must say why
	// instead of leaving a zero stop for the broker to reject confusingly.
	if _, err := previewTrailSpec(rpc.OrderActionSell, rpc.OrderTypeTRAIL, &rpc.OrderTrailSpec{TrailingPercent: &pctValue}, stock, delayed); err == nil || !strings.Contains(err.Error(), "live bid/ask") {
		t.Fatalf("TRAIL preview on delayed data err = %v, want live-reference bad request", err)
	}

	live := rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}
	if _, err := previewTrailSpec(rpc.OrderActionSell, rpc.OrderTypeTRAILLIMIT, &rpc.OrderTrailSpec{TrailingPercent: &pctValue}, stock, live); err == nil {
		t.Fatal("TRAIL LIMIT without limit_offset succeeded")
	}
	trail, err := previewTrailSpec(rpc.OrderActionSell, rpc.OrderTypeTRAILLIMIT, &rpc.OrderTrailSpec{TrailingPercent: &pctValue, LimitOffset: &offset}, stock, live)
	if err != nil {
		t.Fatalf("TRAIL LIMIT with offset: %v", err)
	}
	if trail.LimitOffset == nil || *trail.LimitOffset != offset {
		t.Fatalf("limit offset = %v, want %.2f", trail.LimitOffset, offset)
	}

	limit := 99.0
	if _, _, _, _, err := previewOrderPricing(rpc.OrderActionSell, rpc.OrderTypeTRAILLIMIT, rpc.OrderStrategyBrokerTrail, &limit, &rpc.OrderTrailSpec{TrailingPercent: &pctValue, LimitOffset: &offset}, stock, live); err == nil {
		t.Fatal("TRAIL LIMIT with explicit limit_price succeeded")
	}

	stale := live
	stale.Stale = true
	stale.StaleReason = "market is open but quote data is frozen"
	if _, err := previewTrailSpec(rpc.OrderActionSell, rpc.OrderTypeTRAIL, &rpc.OrderTrailSpec{TrailingPercent: &pctValue}, stock, stale); err == nil || !strings.Contains(err.Error(), "live bid/ask") {
		t.Fatalf("TRAIL preview on stale data err = %v, want live-reference bad request", err)
	}

	closed := live
	closed.SessionContext = &rpc.MarketSession{Market: "de", State: "closed", IsOpen: false}
	if _, err := previewTrailSpec(rpc.OrderActionSell, rpc.OrderTypeTRAIL, &rpc.OrderTrailSpec{TrailingPercent: &pctValue}, stock, closed); err == nil || !strings.Contains(err.Error(), "live bid/ask") {
		t.Fatalf("TRAIL preview on closed session err = %v, want live-reference bad request", err)
	}

	// An explicit initial stop keeps off-hours placement available: the
	// caller supplied the reference, so no live quote is required.
	explicitStop := rpc.OrderTrailSpec{TrailingPercent: &pctValue, InitialStopPrice: 95}
	seeded, err := previewTrailSpec(rpc.OrderActionSell, rpc.OrderTypeTRAIL, &explicitStop, stock, delayed)
	if err != nil {
		t.Fatalf("TRAIL preview with explicit stop on delayed data: %v", err)
	}
	if seeded.InitialStopPrice != 95 {
		t.Fatalf("explicit initial stop = %.2f, want 95 preserved", seeded.InitialStopPrice)
	}
}

func TestPreviewIBKRContractOmitsStockMultiplier(t *testing.T) {
	t.Parallel()

	stock := previewIBKRContract(rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD", Multiplier: 1})
	if stock.Multiplier != 0 {
		t.Fatalf("stock multiplier = %d, want omitted", stock.Multiplier)
	}

	option := previewIBKRContract(rpc.ContractParams{Symbol: "AAPL", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: "20260619", Right: "C", Strike: 200, Multiplier: 100})
	if option.Multiplier != 100 {
		t.Fatalf("option multiplier = %d, want 100", option.Multiplier)
	}
}

func TestContractMultiplierForcesStockToOne(t *testing.T) {
	t.Parallel()

	if got := contractMultiplier(rpc.ContractParams{Symbol: "SAP", SecType: "STK", Multiplier: 100}); got != 1 {
		t.Fatalf("stock multiplier = %d, want 1", got)
	}
	if got := contractMultiplier(rpc.ContractParams{Symbol: "SPY", SecType: "OPT", Multiplier: 10}); got != 10 {
		t.Fatalf("option multiplier = %d, want 10", got)
	}
}

func TestPreviewIBKROrderForStatusBindsAccountAndClient(t *testing.T) {
	t.Parallel()

	order := previewIBKROrderForStatus(
		rpc.OrderDraft{
			Action:     rpc.OrderActionBuy,
			Quantity:   1,
			OrderType:  rpc.OrderTypeLMT,
			LimitPrice: 1,
			TIF:        rpc.OrderTIFDay,
			OrderRef:   "ibkr-20260607-050000",
		},
		rpc.TradingStatus{
			Account:  "DU1234567",
			ClientID: 31,
		},
	)
	if order.Account != "DU1234567" || order.ClientID != 31 {
		t.Fatalf("order account/client = %q/%d, want DU1234567/31", order.Account, order.ClientID)
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

func TestStockPositionQuantityMatchesContractIdentity(t *testing.T) {
	t.Parallel()
	positions := []*ibkrlib.RawPosition{
		{
			Contract: ibkrlib.Contract{
				Symbol:      "SAP",
				SecType:     "STK",
				Exchange:    "NYSE",
				Currency:    "USD",
				LocalSymbol: "SAP",
			},
			Position: 100,
		},
		{
			Contract: ibkrlib.Contract{
				Symbol:      "SAP",
				SecType:     "STK",
				Exchange:    "IBIS",
				PrimaryExch: "IBIS",
				Currency:    "EUR",
				LocalSymbol: "SAP",
			},
			Position: 25,
		},
		{
			Contract: ibkrlib.Contract{
				Symbol:   "SAP",
				SecType:  "OPT",
				Exchange: "SMART",
				Currency: "USD",
			},
			Position: 7,
		},
		{
			Contract: ibkrlib.Contract{
				Symbol:   "SIE",
				SecType:  "STK",
				Exchange: "SMART",
				Currency: "EUR",
			},
			Position: 12,
		},
	}

	eurContract := rpc.ContractParams{
		Symbol:      "SAP",
		SecType:     "STK",
		Exchange:    "SMART",
		PrimaryExch: "IBIS",
		Currency:    "EUR",
	}
	if got := stockPositionQuantity(positions, eurContract); got != 25 {
		t.Fatalf("EUR/IBIS SAP quantity = %v, want 25", got)
	}

	usdContract := rpc.ContractParams{
		Symbol:   "SAP",
		SecType:  "STK",
		Exchange: "SMART",
		Currency: "USD",
	}
	if got := stockPositionQuantity(positions, usdContract); got != 100 {
		t.Fatalf("USD SAP quantity = %v, want 100", got)
	}

	routedContract := rpc.ContractParams{
		Symbol:      "SIE",
		SecType:     "STK",
		Exchange:    "SMART",
		PrimaryExch: "IBIS",
		Currency:    "EUR",
	}
	if got := stockPositionQuantity(positions, routedContract); got != 12 {
		t.Fatalf("SMART-routed EUR SIE quantity = %v, want 12", got)
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

func TestOrderTokenSignerGenerationRotationInvalidatesMintedToken(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	token, _, _, err := srv.orderTokens.mint(orderPreviewTokenPayload{
		Mode: "paper", Account: "DU1234567", Endpoint: "127.0.0.1:4002", ClientID: 31,
		Draft:  rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK"}, Quantity: 1, OrderType: rpc.OrderTypeLMT, LimitPrice: 100, TIF: rpc.OrderTIFDay},
		WhatIf: previewWhatIfUnavailable(),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	authority, err := srv.orderJournal.coreStore()
	if err != nil {
		t.Fatalf("order authority: %v", err)
	}
	head, err := authority.AuthorityHead(t.Context())
	if err != nil {
		t.Fatalf("authority head: %v", err)
	}
	rotated, err := authority.AdvanceSignerGeneration(t.Context(), head.SignerGeneration, head.SignerGeneration+1)
	if err != nil {
		t.Fatalf("advance signer generation: %v", err)
	}
	if err := srv.orderTokens.bindAuthority(rotated.AuthorityEpoch, rotated.SignerGeneration); err != nil {
		t.Fatalf("bind rotated signer generation: %v", err)
	}
	if _, err := srv.orderTokens.verify(token); err == nil || !strings.Contains(err.Error(), "different authority epoch") {
		t.Fatalf("old-generation verify err = %v, want authority mismatch", err)
	}
}

func TestOrderTokenSignerRejectsTamperedAndExpired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 8, 30, 0, 0, time.UTC)
	signer, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "order-preview-key"), func() time.Time { return now })
	if err != nil {
		t.Fatalf("newOrderTokenSigner: %v", err)
	}
	token, _, _, err := signer.mint(orderPreviewTokenPayload{
		ExpiresAt:    now.Add(time.Minute),
		Mode:         "paper",
		Account:      "DU1234567",
		Endpoint:     "127.0.0.1:4002",
		ClientID:     31,
		Draft:        rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK"}, Quantity: 1, OrderType: rpc.OrderTypeLMT, LimitPrice: 1, TIF: rpc.OrderTIFDay},
		WhatIf:       previewWhatIfUnavailable(),
		WhatIfStatus: rpc.OrderWhatIfStatusUnavailable,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := signer.verify(token + "x"); err == nil {
		t.Fatal("tampered token should fail verification")
	}

	expiredSigner, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "order-preview-key"), func() time.Time { return now.Add(2 * time.Minute) })
	if err != nil {
		t.Fatalf("new expired signer: %v", err)
	}
	expiredSigner.key = signer.key
	if _, err := expiredSigner.verify(token); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token err = %v, want expired", err)
	}
}

func TestOrderPreviewDisabledGateFailsBeforeMarketData(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModeDisabled})
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("quote hook should not be called while trading is disabled")
		return rpc.OrderQuoteSnapshot{}, nil
	}

	_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:   "buy",
		Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity: 1,
	})
	if !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("previewOrder err = %v, want ErrTradingDisabled", err)
	}
}

func TestOrderPreviewTIFGate(t *testing.T) {
	t.Parallel()
	pct := 8.0
	trail := &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &pct}
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, AllowStockShort: true})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(10, 0, rpc.OrderPositionEffectClose)

	res, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:    "sell",
		Contract:  rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:  10,
		OrderType: rpc.OrderTypeTRAIL,
		Trail:     trail,
		TIF:       "gtc",
	})
	if err != nil {
		t.Fatalf("GTC TRAIL preview: %v", err)
	}
	if res.Draft.TIF != rpc.OrderTIFGTC {
		t.Fatalf("draft TIF = %q, want GTC", res.Draft.TIF)
	}
	if res.Draft.TriggerMethod != rpc.OrderTriggerMethodLast {
		t.Fatalf("draft trigger method = %d, want LAST", res.Draft.TriggerMethod)
	}

	res, err = srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:        "sell",
		Contract:      rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:      10,
		OrderType:     rpc.OrderTypeTRAIL,
		Trail:         trail,
		TriggerMethod: rpc.OrderTriggerMethodBidAsk,
	})
	if err != nil {
		t.Fatalf("explicit trigger TRAIL preview: %v", err)
	}
	if res.Draft.TriggerMethod != rpc.OrderTriggerMethodBidAsk {
		t.Fatalf("explicit trigger method = %d, want BID_ASK", res.Draft.TriggerMethod)
	}

	if _, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:   "sell",
		Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity: 10,
		TIF:      rpc.OrderTIFGTC,
	}); err == nil || !strings.Contains(err.Error(), "GTC for TRAIL") {
		t.Fatalf("GTC LMT err = %v, want trail-only GTC rejection", err)
	}

	if _, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:        "sell",
		Contract:      rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:      10,
		TriggerMethod: rpc.OrderTriggerMethodLast,
	}); err == nil || !strings.Contains(err.Error(), "TRAIL and TRAIL LIMIT") {
		t.Fatalf("LMT trigger method err = %v, want trail-only trigger rejection", err)
	}

	if _, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:    "sell",
		Contract:  rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:  10,
		OrderType: rpc.OrderTypeTRAIL,
		Trail:     trail,
		TIF:       "IOC",
	}); err == nil || !strings.Contains(err.Error(), "time-in-force") {
		t.Fatalf("IOC err = %v, want time-in-force rejection", err)
	}
}

func TestOrderPreviewPaperMintsTokenAndJournal(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	bid, ask := 100.10, 100.15
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		if c.Symbol != "AAPL" || c.Exchange != "SMART" || c.Currency != "USD" {
			t.Fatalf("contract = %+v, want SMART/USD AAPL", c)
		}
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{
			Symbol:       "AAPL",
			Bid:          &bid,
			Ask:          &ask,
			Midpoint:     &mid,
			DataType:     rpc.MarketDataLive,
			QuoteQuality: "firm",
			AsOf:         srv.now(),
		}, nil
	}
	srv.orderPreviewPositionImpact = func(_ context.Context, contract rpc.ContractParams, action string, qty int) (rpc.OrderPositionImpact, error) {
		if contract.Symbol != "AAPL" || contract.Exchange != "SMART" || contract.Currency != "USD" || action != rpc.OrderActionBuy || qty != 10 {
			t.Fatalf("position hook args = %+v %s %d", contract, action, qty)
		}
		return rpc.OrderPositionImpact{Before: 0, After: 10, Effect: rpc.OrderPositionEffectOpen}, nil
	}

	res, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:   "buy",
		Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity: 10,
	})
	if err != nil {
		t.Fatalf("previewOrder: %v", err)
	}
	if !res.TokenMinted || res.PreviewToken == "" || res.PreviewTokenID == "" {
		t.Fatalf("preview token not minted: %+v", res)
	}
	if !strings.HasPrefix(res.Draft.OrderRef, "canary-20260528-084500-") {
		t.Fatalf("new order ref = %q, want canonical Canary prefix", res.Draft.OrderRef)
	}
	if res.SubmitEligible || res.Executable {
		t.Fatalf("unavailable WhatIf preview should not be submit eligible: %+v", res)
	}
	if res.Draft.Strategy != rpc.OrderStrategyPatientLimit || res.Draft.TIF != rpc.OrderTIFDay || res.Draft.OutsideRTH {
		t.Fatalf("unexpected defaults in draft: %+v", res.Draft)
	}
	if res.Draft.LimitPrice != 100.12 || res.Notional != 1001.20 {
		t.Fatalf("pricing/notional = %.4f %.4f, want 100.12 1001.20", res.Draft.LimitPrice, res.Notional)
	}
	if res.WhatIf.Status != rpc.OrderWhatIfStatusUnavailable || !res.WhatIf.RequiredForSubmit {
		t.Fatalf("WhatIf = %+v, want unavailable required", res.WhatIf)
	}
	payload, err := srv.orderTokens.verify(res.PreviewToken)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if payload.TokenID != res.PreviewTokenID || payload.Account != "DU1234567" || payload.Endpoint != "127.0.0.1:4002" || payload.ClientID != 31 {
		t.Fatalf("token binding mismatch: %+v", payload)
	}
	if payload.Draft.LimitPrice != res.Draft.LimitPrice || payload.WhatIfStatus != rpc.OrderWhatIfStatusUnavailable {
		t.Fatalf("token draft mismatch: %+v", payload)
	}
	if payload.Quote.Symbol != "AAPL" || payload.Position.Effect != rpc.OrderPositionEffectOpen || payload.Notional != res.Notional || payload.WhatIf.Status != rpc.OrderWhatIfStatusUnavailable {
		t.Fatalf("token preview evidence mismatch: %+v", payload)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("journal events = %d, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Type != orderJournalEventPreviewed || ev.PreviewTokenID != res.PreviewTokenID || ev.OrderRef != res.Draft.OrderRef || ev.Action != rpc.OrderActionBuy {
		t.Fatalf("journal event mismatch: %+v", ev)
	}
}

func TestOrderPreviewReplaceMintsModifyScopedToken(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 8, 45, 0, 0, time.UTC)
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
	srv.now = func() time.Time { return now }
	srv.orderWritesEnabled = func() bool { return true }
	srv.orderPreviewQuote = fixedPreviewQuote(99, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 1, rpc.OrderPositionEffectOpen)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	if err := srv.orderJournal.Append(orderJournalEvent{
		At:              now.Add(-time.Minute),
		Type:            orderJournalEventBrokerAcknowledged,
		OrderRef:        "ord-1",
		ReservedOrderID: 1001,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Symbol:          "AAPL",
		SecType:         "STK",
		ConID:           265598,
		Exchange:        "SMART",
		Currency:        "USD",
		Action:          "BUY",
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        1,
		Remaining:       1,
		LimitPrice:      100,
		Status:          "Submitted",
		SendState:       orderSendStateBrokerAcknowledged,
	}); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	limit := 99.5
	res, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:     rpc.OrderActionBuy,
		Contract:   rpc.ContractParams{ConID: 265598, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		Quantity:   1,
		OrderType:  rpc.OrderTypeLMT,
		LimitPrice: &limit,
		TIF:        rpc.OrderTIFDay,
		ReplaceID:  "ord-1",
		TimeoutMs:  100,
	})
	if err != nil {
		t.Fatalf("previewOrder replace err = %v", err)
	}
	if res.PreviewTokenScope != rpc.OrderTokenScopeModify || !res.SubmitEligible {
		t.Fatalf("preview result = %+v, want modify submit-eligible token", res)
	}
	payload, err := srv.orderTokens.verify(res.PreviewToken)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if payload.Scope != rpc.OrderTokenScopeModify || payload.Replace.ReservedOrderID != 1001 || payload.Replace.OrderRef != "ord-1" {
		t.Fatalf("payload = %+v, want modify target ord-1/1001", payload)
	}
}

func TestOrderPreviewBindsAcceptedBrokerWhatIf(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 1, rpc.OrderPositionEffectOpen)
	commission := 1.25
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{
			Status:            rpc.OrderWhatIfStatusAccepted,
			Available:         true,
			RequiredForSubmit: false,
			Margin: &rpc.OrderMarginImpact{
				Currency:           "USD",
				Commission:         &commission,
				CommissionCurrency: "USD",
			},
		}, nil
	}

	limit := 100.0
	res, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:     "buy",
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:   1,
		LimitPrice: &limit,
	})
	if err != nil {
		t.Fatalf("previewOrder: %v", err)
	}
	if res.WhatIf.Status != rpc.OrderWhatIfStatusAccepted || res.WhatIf.RequiredForSubmit {
		t.Fatalf("WhatIf = %+v, want accepted submit-ready", res.WhatIf)
	}
	if !res.TokenMinted || !res.SubmitEligible || !res.Executable {
		t.Fatalf("accepted WhatIf preview should be submit eligible: %+v", res)
	}
	for _, w := range res.Warnings {
		if w.Code == "broker_what_if_unavailable" {
			t.Fatalf("accepted WhatIf should not carry unavailable warning: %+v", res.Warnings)
		}
	}
	payload, err := srv.orderTokens.verify(res.PreviewToken)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if payload.WhatIf.Status != rpc.OrderWhatIfStatusAccepted || payload.WhatIf.Margin == nil || payload.WhatIf.Margin.Commission == nil || *payload.WhatIf.Margin.Commission != commission {
		t.Fatalf("token did not bind accepted WhatIf margin: %+v", payload.WhatIf)
	}
}

func TestOrderPreviewTimeoutAppliesToWhatIf(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 1, rpc.OrderPositionEffectOpen)
	srv.orderPreviewWhatIf = func(ctx context.Context, _ rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		<-ctx.Done()
		return rpc.OrderWhatIfResult{
			Status:  rpc.OrderWhatIfStatusUnavailable,
			Message: "test WhatIf timeout",
		}, nil
	}

	limit := 100.0
	start := time.Now()
	res, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:     "buy",
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:   1,
		LimitPrice: &limit,
		TimeoutMs:  20,
	})
	if err != nil {
		t.Fatalf("previewOrder: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("previewOrder ignored WhatIf timeout, elapsed %s", elapsed)
	}
	if res.WhatIf.Status != rpc.OrderWhatIfStatusUnavailable || res.SubmitEligible {
		t.Fatalf("WhatIf = %+v submitEligible=%v, want unavailable/not eligible", res.WhatIf, res.SubmitEligible)
	}
}

func TestRpcWhatIfResultFromBrokerMapsSubmitEligibility(t *testing.T) {
	t.Parallel()
	commission := 1.25
	accepted := rpcWhatIfResultFromBroker(ibkrlib.OrderWhatIfResult{
		Status:       ibkrlib.OrderWhatIfStatusAccepted,
		BrokerStatus: "Submitted",
		Margin: ibkrlib.OrderWhatIfMargin{
			Currency:           "USD",
			Commission:         &commission,
			CommissionCurrency: "USD",
		},
	})
	if accepted.Status != rpc.OrderWhatIfStatusAccepted || accepted.RequiredForSubmit || !accepted.Available {
		t.Fatalf("accepted broker result mapped wrong: %+v", accepted)
	}
	if accepted.Margin == nil || accepted.Margin.Commission == nil || *accepted.Margin.Commission != commission {
		t.Fatalf("accepted margin mapped wrong: %+v", accepted.Margin)
	}

	rejected := rpcWhatIfResultFromBroker(ibkrlib.OrderWhatIfResult{
		Status:             ibkrlib.OrderWhatIfStatusRejected,
		Message:            "insufficient buying power",
		AdvancedRejectJSON: `{"reason":"size"}`,
	})
	if rejected.Status != rpc.OrderWhatIfStatusRejected || !rejected.RequiredForSubmit || !rejected.Available {
		t.Fatalf("rejected broker result mapped wrong: %+v", rejected)
	}
	if !strings.Contains(rejected.Message, "insufficient buying power") {
		t.Fatalf("rejected message = %q", rejected.Message)
	}
	if rejected.AdvancedRejectJSON != `{"reason":"size"}` {
		t.Fatalf("advanced reject json = %q", rejected.AdvancedRejectJSON)
	}

	unavailable := rpcWhatIfResultFromBroker(ibkrlib.OrderWhatIfResult{
		Status:  ibkrlib.OrderWhatIfStatusUnavailable,
		Message: "timeout waiting for broker WhatIf response",
	})
	if unavailable.Status != rpc.OrderWhatIfStatusUnavailable || !unavailable.RequiredForSubmit || unavailable.Available {
		t.Fatalf("unavailable broker result mapped wrong: %+v", unavailable)
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

func TestConfirmPreviewTokenForPlaceRejectsGateMismatch(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{
		Status:            rpc.OrderWhatIfStatusAccepted,
		Available:         true,
		RequiredForSubmit: false,
	})
	srv.cfg.Gateway.Account = "DU7654321"

	_, err := srv.confirmPreviewTokenForPlace(token)
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "preview token was minted for") {
		t.Fatalf("confirmPreviewTokenForPlace err = %v, want gate mismatch", err)
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

func TestOrderPreviewRejectsStockFlipUnlessEnabled(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, -1, rpc.OrderPositionEffectFlip)

	limit := 100.0
	_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:     "sell",
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:   2,
		LimitPrice: &limit,
	})
	var bad *badRequestError
	if !errors.As(err, &bad) || !strings.Contains(err.Error(), "allow_stock_short") {
		t.Fatalf("previewOrder err = %v, want stock short/flip bad request", err)
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

// TestOrderPreviewAppliesControlsToApparentExits pins the fail-closed boundary:
// this client cannot prove future manual-TWS orders, so position-only
// close/reduce arithmetic never exempts a submission from ordinary controls.
func TestOrderPreviewAppliesControlsToApparentExits(t *testing.T) {
	t.Parallel()
	tr := config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000}
	limit := 480.0

	t.Run("stock reduce above cap passes", func(t *testing.T) {
		t.Parallel()
		srv := newOrderPreviewTestServer(t, tr)
		srv.orderPreviewQuote = fixedPreviewQuote(479.90, 480.10)
		srv.orderPreviewPositionImpact = fixedPreviewPosition(200, 50, rpc.OrderPositionEffectReduce)
		_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
			Action:     "sell",
			Contract:   rpc.ContractParams{Symbol: "AMD", SecType: "STK"},
			Quantity:   150,
			LimitPrice: &limit,
		})
		if err == nil || !strings.Contains(err.Error(), "max_notional") {
			t.Fatalf("reduce preview err = %v, want max_notional refusal", err)
		}
	})

	t.Run("stock close above cap passes", func(t *testing.T) {
		t.Parallel()
		srv := newOrderPreviewTestServer(t, tr)
		srv.orderPreviewQuote = fixedPreviewQuote(479.90, 480.10)
		srv.orderPreviewPositionImpact = fixedPreviewPosition(150, 0, rpc.OrderPositionEffectClose)
		_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
			Action:     "sell",
			Contract:   rpc.ContractParams{Symbol: "AMD", SecType: "STK"},
			Quantity:   150,
			LimitPrice: &limit,
		})
		if err == nil || !strings.Contains(err.Error(), "max_notional") {
			t.Fatalf("close preview err = %v, want max_notional refusal", err)
		}
	})

	t.Run("option close above both caps passes", func(t *testing.T) {
		t.Parallel()
		srv := newOrderPreviewTestServer(t, tr) // MaxOptionContracts defaults to 5
		srv.orderPreviewQuote = fixedPreviewQuote(3.95, 4.05)
		srv.orderPreviewPositionImpact = fixedPreviewPosition(30, 0, rpc.OrderPositionEffectClose)
		optLimit := 4.0
		_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
			Action: "sell",
			Contract: rpc.ContractParams{
				Symbol: "SPY", SecType: "OPT", Expiry: "20260619", Right: "C", Strike: 520, Multiplier: 100,
			},
			Quantity:   30,
			LimitPrice: &optLimit,
		})
		if err == nil || !strings.Contains(err.Error(), "max_option_contracts") {
			t.Fatalf("option close err = %v, want max_option_contracts refusal", err)
		}
	})

	t.Run("opening above cap still fails", func(t *testing.T) {
		t.Parallel()
		srv := newOrderPreviewTestServer(t, tr)
		srv.orderPreviewQuote = fixedPreviewQuote(479.90, 480.10)
		srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 150, rpc.OrderPositionEffectOpen)
		_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
			Action:     "buy",
			Contract:   rpc.ContractParams{Symbol: "AMD", SecType: "STK"},
			Quantity:   150,
			LimitPrice: &limit,
		})
		var bad *badRequestError
		if !errors.As(err, &bad) || !strings.Contains(err.Error(), "max_notional") {
			t.Fatalf("opening preview err = %v, want max_notional bad request", err)
		}
	})

	t.Run("flip above cap still fails even with shorting allowed", func(t *testing.T) {
		t.Parallel()
		srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000, AllowStockShort: true})
		srv.orderPreviewQuote = fixedPreviewQuote(479.90, 480.10)
		srv.orderPreviewPositionImpact = fixedPreviewPosition(10, -140, rpc.OrderPositionEffectFlip)
		_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
			Action:     "sell",
			Contract:   rpc.ContractParams{Symbol: "AMD", SecType: "STK"},
			Quantity:   150,
			LimitPrice: &limit,
		})
		var bad *badRequestError
		if !errors.As(err, &bad) || !strings.Contains(err.Error(), "max_notional") {
			t.Fatalf("flip preview err = %v, want max_notional bad request (flip is risk-increasing)", err)
		}
	})

	t.Run("option opening above qty cap still fails", func(t *testing.T) {
		t.Parallel()
		srv := newOrderPreviewTestServer(t, tr)
		srv.orderPreviewQuote = fixedPreviewQuote(3.95, 4.05)
		srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 6, rpc.OrderPositionEffectOpen)
		optLimit := 4.0
		_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
			Action: "buy",
			Contract: rpc.ContractParams{
				Symbol: "SPY", SecType: "OPT", Expiry: "20260619", Right: "C", Strike: 520, Multiplier: 100,
			},
			Quantity:   6,
			LimitPrice: &optLimit,
		})
		var bad *badRequestError
		if !errors.As(err, &bad) || !strings.Contains(err.Error(), "max_option_contracts") {
			t.Fatalf("option opening err = %v, want max_option_contracts bad request", err)
		}
	})

	t.Run("capped preview still echoes the cap", func(t *testing.T) {
		t.Parallel()
		srv := newOrderPreviewTestServer(t, tr)
		srv.orderPreviewQuote = fixedPreviewQuote(99.90, 100.10)
		srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 10, rpc.OrderPositionEffectOpen)
		smallLimit := 100.0
		res, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
			Action:     "buy",
			Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
			Quantity:   10,
			LimitPrice: &smallLimit,
		})
		if err != nil {
			t.Fatalf("small opening preview: %v", err)
		}
		if res.MaxNotional != 10_000 {
			t.Fatalf("capped preview MaxNotional = %.2f, want 10000.00", res.MaxNotional)
		}
	})

	t.Run("position unavailable fails closed before quote", func(t *testing.T) {
		t.Parallel()
		srv := newOrderPreviewTestServer(t, tr)
		srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
			t.Fatal("quote hook must not run when position impact is unavailable")
			return rpc.OrderQuoteSnapshot{}, nil
		}
		srv.orderPreviewPositionImpact = func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error) {
			return rpc.OrderPositionImpact{}, errors.New("cached positions unavailable")
		}
		_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
			Action:     "sell",
			Contract:   rpc.ContractParams{Symbol: "AMD", SecType: "STK"},
			Quantity:   150,
			LimitPrice: &limit,
		})
		if err == nil || !strings.Contains(err.Error(), "cached positions unavailable") {
			t.Fatalf("position-unavailable preview err = %v, want fail-closed error", err)
		}
	})
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

func TestBindPreviewOrderRiskAuthorityRejectsBaseCurrencyDriftAtRedemption(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	impact := rpc.OrderPositionImpact{Before: 0, After: 1, Effect: rpc.OrderPositionEffectOpen}
	srv.orderRiskAuthorityForTest = func(context.Context, rpc.TradingStatus, rpc.ContractParams, string, int) (orderPositionAuthority, error) {
		return orderPositionAuthority{
			Impact: impact, Generation: 7, TestOnly: true,
			Health:                 ibkrlib.PortfolioStreamHealth{Account: "DU1234567", ProjectionGeneration: 7},
			BaseCurrency:           "EUR",
			BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyValueSuffix,
		}, nil
	}
	binding := brokerWriteTransactionBinding{testOnly: true}
	payload := orderPreviewTokenPayload{
		Position: impact, PortfolioGeneration: 7, PortfolioAccount: "DU1234567",
		BaseCurrency: "USD", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag,
		Notional: 100,
	}
	draft := rpc.OrderDraft{
		Action: rpc.OrderActionBuy, Quantity: 1,
		Contract: rpc.ContractParams{ConID: 1, Symbol: "AAPL", SecType: "STK", Currency: "USD"},
	}
	err := srv.bindPreviewOrderRiskAuthority(context.Background(), &binding, rpc.TradingStatus{Account: "DU1234567", Mode: rpc.AccountModePaper}, payload, draft)
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "risk authority changed") {
		t.Fatalf("bindPreviewOrderRiskAuthority err = %v, want base-currency drift refusal", err)
	}
	if binding.riskBound {
		t.Fatal("base-currency drift unexpectedly produced a redeemed risk binding")
	}
}

func TestOrderPreviewConvertsCrossCurrencyNotionalToAccountBase(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 95})
	srv.orderPreviewQuote = fixedPreviewQuote(99, 101)
	impact := rpc.OrderPositionImpact{Before: 0, After: 1, Effect: rpc.OrderPositionEffectOpen}
	srv.orderRiskAuthorityForTest = func(context.Context, rpc.TradingStatus, rpc.ContractParams, string, int) (orderPositionAuthority, error) {
		return orderPositionAuthority{
			Impact: impact, Generation: 7, TestOnly: true,
			Health:                 ibkrlib.PortfolioStreamHealth{Account: "DU1234567", ProjectionGeneration: 7},
			BaseCurrency:           "EUR",
			BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyValueSuffix,
		}, nil
	}
	srv.orderFXRateForTest = func(context.Context, string, string, time.Duration) (float64, time.Time, error) {
		return 0.90, srv.orderNow(), nil
	}
	limit := 100.0
	res, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action: "buy", Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Currency: "USD"},
		Quantity: 1, LimitPrice: &limit,
	})
	if err != nil {
		t.Fatalf("cross-currency preview: %v", err)
	}
	if res.Notional != 100 || res.NotionalCurrency != "USD" || res.NotionalBase != 90 || res.BaseCurrency != "EUR" || res.FXRate != 0.90 {
		t.Fatalf("cross-currency notional result = %+v", res)
	}
	payload, err := srv.orderTokens.verify(res.PreviewToken)
	if err != nil {
		t.Fatalf("verify cross-currency token: %v", err)
	}
	if payload.NotionalAuthority.BaseNotional != 90 ||
		payload.NotionalAuthority.Source != orderFXSourceExactSessionQuote ||
		payload.NotionalAuthority.BaseCurrency != "EUR" {
		t.Fatalf("signed FX authority = %+v", payload.NotionalAuthority)
	}
}

func TestCanonicalOrderFXContractAndConservativeRate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		contractCurrency string
		baseCurrency     string
		wantSymbol       string
		wantCurrency     string
		wantInverted     bool
	}{
		{name: "EUR into USD is direct", contractCurrency: "EUR", baseCurrency: "USD", wantSymbol: "EUR", wantCurrency: "USD"},
		{name: "USD into EUR is inverse", contractCurrency: "USD", baseCurrency: "EUR", wantSymbol: "EUR", wantCurrency: "USD", wantInverted: true},
		{name: "USD into JPY is direct", contractCurrency: "USD", baseCurrency: "JPY", wantSymbol: "USD", wantCurrency: "JPY"},
		{name: "JPY into USD is inverse", contractCurrency: "JPY", baseCurrency: "USD", wantSymbol: "USD", wantCurrency: "JPY", wantInverted: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contract, inverted, err := canonicalOrderFXContract(tc.contractCurrency, tc.baseCurrency)
			if err != nil {
				t.Fatalf("canonicalOrderFXContract: %v", err)
			}
			if contract.Symbol != tc.wantSymbol || contract.Currency != tc.wantCurrency ||
				contract.SecType != "CASH" || contract.Exchange != "IDEALPRO" ||
				contract.PrimaryExch != "IDEALPRO" || inverted != tc.wantInverted {
				t.Fatalf("contract=%+v inverted=%v", contract, inverted)
			}
		})
	}

	bid, ask := 1.08, 1.10
	quote := rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask}
	if got := conservativeOrderFXRate(quote, false); got != ask {
		t.Fatalf("direct rate=%v, want ask=%v", got, ask)
	}
	if got, want := conservativeOrderFXRate(quote, true), 1/bid; math.Abs(got-want) > 1e-12 {
		t.Fatalf("inverse rate=%v, want 1/bid=%v", got, want)
	}
}

func TestCanonicalOrderFXContractRejectsUnsupportedPairs(t *testing.T) {
	t.Parallel()
	for _, tc := range [][2]string{{"USD", "USD"}, {"SEK", "EUR"}, {"USD", ""}} {
		if contract, inverted, err := canonicalOrderFXContract(tc[0], tc[1]); err == nil {
			t.Fatalf("pair %q/%q unexpectedly accepted as %+v inverted=%v", tc[0], tc[1], contract, inverted)
		}
	}
}

func TestBindPreviewOrderRiskAuthorityRefreshesFXAtRedemption(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 92})
	impact := rpc.OrderPositionImpact{Before: 0, After: 1, Effect: rpc.OrderPositionEffectOpen}
	srv.orderRiskAuthorityForTest = func(context.Context, rpc.TradingStatus, rpc.ContractParams, string, int) (orderPositionAuthority, error) {
		return orderPositionAuthority{
			Impact: impact, Generation: 7, TestOnly: true,
			Health:                 ibkrlib.PortfolioStreamHealth{Account: "DU1234567", ProjectionGeneration: 7},
			BaseCurrency:           "EUR",
			BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyValueSuffix,
		}, nil
	}
	srv.orderFXRateForTest = func(context.Context, string, string, time.Duration) (float64, time.Time, error) {
		return 0.95, srv.orderNow(), nil
	}
	draft := rpc.OrderDraft{
		Action: rpc.OrderActionBuy, Quantity: 1,
		Contract: rpc.ContractParams{ConID: 1, Symbol: "AAPL", SecType: "STK", Currency: "USD"},
	}
	payload := orderPreviewTokenPayload{
		Position: impact, PortfolioGeneration: 7, PortfolioAccount: "DU1234567",
		BaseCurrency: "EUR", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyValueSuffix,
		Notional: 100,
		NotionalAuthority: orderNotionalAuthority{
			QuoteNotional: 100, ContractCurrency: "USD", BaseNotional: 90, BaseCurrency: "EUR",
			BasePerContract: 0.90, EvidenceAt: srv.orderNow(), DataType: rpc.MarketDataLive, Source: orderFXSourceExactSessionQuote,
		},
	}
	binding := brokerWriteTransactionBinding{testOnly: true}
	err := srv.bindPreviewOrderRiskAuthority(context.Background(), &binding, rpc.TradingStatus{Account: "DU1234567", Mode: rpc.AccountModePaper}, payload, draft)
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "max_notional") {
		t.Fatalf("FX-refreshed redemption err = %v, want current base-cap rejection", err)
	}
	if binding.riskBound {
		t.Fatal("FX-refreshed cap rejection unexpectedly produced a risk binding")
	}
}

func TestCaptureWireOrderPositionAuthorityUsesRedeemedSessionBaseCurrency(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	binding := brokerWriteTransactionBinding{
		testOnly:                   true,
		riskPosition:               rpc.OrderPositionImpact{Before: 1, After: 0, Effect: rpc.OrderPositionEffectClose},
		riskPortfolioGeneration:    9,
		riskPortfolioAccount:       "DU1234567",
		riskBaseCurrency:           "EUR",
		riskBaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyValueSuffix,
	}
	got, err := srv.captureWireOrderPositionAuthority(binding, rpc.TradingStatus{Account: "DU1234567"}, rpc.OrderDraft{})
	if err != nil {
		t.Fatalf("captureWireOrderPositionAuthority: %v", err)
	}
	if got.BaseCurrency != "EUR" || got.BaseCurrencyProvenance != ibkrlib.AccountBaseCurrencyValueSuffix {
		t.Fatalf("wire base authority = %q/%q, want redeemed EUR/value-suffix binding", got.BaseCurrency, got.BaseCurrencyProvenance)
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
