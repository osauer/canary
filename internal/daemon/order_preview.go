package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	orderPreviewTokenVersion = 4
	orderPreviewTokenPrefix  = "ibkrp4"
	orderPreviewTokenTTL     = 10 * time.Minute
	orderPreviewKeyBytes     = 32
	orderPreviewDefaultWait  = 5 * time.Second
	orderPreviewMaxWait      = 3 * time.Minute
)

type orderPreviewTokenPayload struct {
	Version                  int                                   `json:"version"`
	AuthorityEpoch           string                                `json:"authority_epoch"`
	SignerGeneration         int64                                 `json:"signer_generation"`
	TokenID                  string                                `json:"token_id"`
	Scope                    string                                `json:"scope"`
	IssuedAt                 time.Time                             `json:"issued_at"`
	ExpiresAt                time.Time                             `json:"expires_at"`
	Mode                     string                                `json:"mode"`
	Account                  string                                `json:"account"`
	Endpoint                 string                                `json:"endpoint"`
	ClientID                 int                                   `json:"client_id"`
	Draft                    rpc.OrderDraft                        `json:"draft"`
	Quote                    rpc.OrderQuoteSnapshot                `json:"quote"`
	Position                 rpc.OrderPositionImpact               `json:"position"`
	PortfolioGeneration      uint64                                `json:"portfolio_generation"`
	PortfolioAccount         string                                `json:"portfolio_account"`
	PortfolioEvidenceAt      time.Time                             `json:"portfolio_evidence_at"`
	BaseCurrency             string                                `json:"base_currency"`
	BaseCurrencyProvenance   ibkrlib.AccountBaseCurrencyProvenance `json:"base_currency_provenance"`
	TradingControlGeneration uint64                                `json:"trading_control_generation"`
	Notional                 float64                               `json:"notional"`
	NotionalAuthority        orderNotionalAuthority                `json:"notional_authority"`
	WhatIf                   rpc.OrderWhatIfResult                 `json:"what_if"`
	WhatIfStatus             string                                `json:"what_if_status,omitempty"`
	Replace                  orderPreviewReplaceTarget             `json:"replace"`
	ExerciseKey              string                                `json:"exercise_key,omitempty"`
	ExerciseRevision         string                                `json:"exercise_revision,omitempty"`
	ExerciseUnderlying       rpc.ContractParams                    `json:"exercise_underlying,omitzero"`
}

type orderPreviewReplaceTarget struct {
	OrderRef        string              `json:"order_ref,omitempty"`
	ReservedOrderID int                 `json:"reserved_order_id,omitempty"`
	PermID          int                 `json:"perm_id,omitempty"`
	ClientID        int                 `json:"client_id,omitempty"`
	Account         string              `json:"account,omitempty"`
	Endpoint        string              `json:"endpoint,omitempty"`
	Mode            string              `json:"mode,omitempty"`
	Status          string              `json:"status,omitempty"`
	LifecycleStatus string              `json:"lifecycle_status,omitempty"`
	ConID           int                 `json:"con_id,omitempty"`
	Symbol          string              `json:"symbol,omitempty"`
	SecType         string              `json:"sec_type,omitempty"`
	Exchange        string              `json:"exchange,omitempty"`
	PrimaryExch     string              `json:"primary_exchange,omitempty"`
	Currency        string              `json:"currency,omitempty"`
	LocalSymbol     string              `json:"local_symbol,omitempty"`
	TradingClass    string              `json:"trading_class,omitempty"`
	Expiry          string              `json:"expiry,omitempty"`
	Strike          float64             `json:"strike,omitempty"`
	Right           string              `json:"right,omitempty"`
	Multiplier      int                 `json:"multiplier,omitempty"`
	OrderType       string              `json:"order_type,omitempty"`
	TIF             string              `json:"tif,omitempty"`
	TriggerMethod   int                 `json:"trigger_method,omitempty"`
	Quantity        float64             `json:"quantity,omitempty"`
	Filled          float64             `json:"filled,omitempty"`
	Remaining       float64             `json:"remaining,omitempty"`
	LimitPrice      float64             `json:"limit_price,omitempty"`
	Trail           *rpc.OrderTrailSpec `json:"trail,omitempty"`
	OutsideRTH      bool                `json:"outside_rth,omitempty"`
}

type orderTokenSigner struct {
	key              []byte
	now              func() time.Time
	mu               sync.RWMutex
	authorityEpoch   string
	signerGeneration int64
}

// orderTokenKeyPathForDatabase keeps test/offline authorities self-contained:
// a custom daemon.db can never create or read the production state-root key.
func orderTokenKeyPathForDatabase(databasePath string) (string, error) {
	databasePath = strings.TrimSpace(databasePath)
	if databasePath == "" {
		return "", fmt.Errorf("daemon authority database path is empty")
	}
	return filepath.Join(filepath.Dir(databasePath), "order-preview-key-v2"), nil
}

func newOrderTokenSigner(path string, now func() time.Time) (*orderTokenSigner, error) {
	key, err := loadOrCreateOrderTokenKey(path)
	if err != nil {
		return nil, err
	}
	// Standalone construction is used by focused unit tests. The daemon binds
	return &orderTokenSigner{key: key, now: now, authorityEpoch: "standalone", signerGeneration: 1}, nil
}

func (s *orderTokenSigner) bindAuthority(authorityEpoch string, signerGeneration int64) error {
	if s == nil || len(s.key) == 0 {
		return fmt.Errorf("order preview token signer is not configured")
	}
	authorityEpoch = strings.TrimSpace(authorityEpoch)
	if authorityEpoch == "" || signerGeneration < 1 {
		return fmt.Errorf("order preview token authority identity is invalid")
	}
	s.mu.Lock()
	s.authorityEpoch = authorityEpoch
	s.signerGeneration = signerGeneration
	s.mu.Unlock()
	return nil
}

func (s *orderTokenSigner) authorityIdentity() (string, int64) {
	if s == nil {
		return "", 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authorityEpoch, s.signerGeneration
}

func loadOrCreateOrderTokenKey(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("order preview token key path is empty")
	}
	data, err := os.ReadFile(path)
	if err == nil {
		key, decErr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(data)))
		if decErr != nil {
			return nil, fmt.Errorf("decode order preview token key: %w", decErr)
		}
		if len(key) != orderPreviewKeyBytes {
			return nil, fmt.Errorf("order preview token key has %d bytes, want %d", len(key), orderPreviewKeyBytes)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read order preview token key: %w", err)
	}
	key := make([]byte, orderPreviewKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate order preview token key: %w", err)
	}
	encoded := []byte(base64.RawURLEncoding.EncodeToString(key) + "\n")
	if err := writePrivateStateAtomic(path, encoded); err != nil {
		return nil, fmt.Errorf("write order preview token key: %w", err)
	}
	return key, nil
}

func (s *orderTokenSigner) mint(payload orderPreviewTokenPayload) (token string, tokenID string, expiresAt time.Time, err error) {
	if s == nil || len(s.key) == 0 {
		return "", "", time.Time{}, fmt.Errorf("order preview token signer is not configured")
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	tokenID = payload.TokenID
	if tokenID == "" {
		tokenID, err = randomTokenID()
		if err != nil {
			return "", "", time.Time{}, err
		}
	}
	payload.Version = orderPreviewTokenVersion
	payload.AuthorityEpoch, payload.SignerGeneration = s.authorityIdentity()
	if payload.AuthorityEpoch == "" || payload.SignerGeneration < 1 {
		return "", "", time.Time{}, fmt.Errorf("order preview token authority identity is unavailable")
	}
	payload.TokenID = tokenID
	if payload.Scope == "" {
		payload.Scope = rpc.OrderTokenScopePlace
	}
	switch payload.Scope {
	case rpc.OrderTokenScopePlace, rpc.OrderTokenScopeModify, rpc.OrderTokenScopeExercise, rpc.OrderTokenScopeStrategy:
	default:
		return "", "", time.Time{}, fmt.Errorf("unsupported order preview token scope %q", payload.Scope)
	}
	payload.IssuedAt = now
	if payload.ExpiresAt.IsZero() {
		payload.ExpiresAt = now.Add(orderPreviewTokenTTL)
	}
	expiresAt = payload.ExpiresAt
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("marshal order preview token: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return orderPreviewTokenPrefix + "." + body + "." + sig, tokenID, expiresAt, nil
}

func (s *orderTokenSigner) verify(token string) (orderPreviewTokenPayload, error) {
	if s == nil || len(s.key) == 0 {
		return orderPreviewTokenPayload{}, fmt.Errorf("order preview token signer is not configured")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != orderPreviewTokenPrefix {
		return orderPreviewTokenPayload{}, fmt.Errorf("invalid order preview token shape")
	}
	body := parts[1]
	wantMAC := hmac.New(sha256.New, s.key)
	wantMAC.Write([]byte(body))
	wantSig := wantMAC.Sum(nil)
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("decode order preview token signature: %w", err)
	}
	if !hmac.Equal(gotSig, wantSig) {
		return orderPreviewTokenPayload{}, fmt.Errorf("invalid order preview token signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("decode order preview token payload: %w", err)
	}
	var payload orderPreviewTokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("decode order preview token payload: %w", err)
	}
	if payload.Version != orderPreviewTokenVersion {
		return orderPreviewTokenPayload{}, fmt.Errorf("unsupported order preview token version %d", payload.Version)
	}
	authorityEpoch, signerGeneration := s.authorityIdentity()
	if payload.AuthorityEpoch != authorityEpoch || payload.SignerGeneration != signerGeneration {
		return orderPreviewTokenPayload{}, fmt.Errorf("order preview token belongs to signer generation %d in a different authority epoch", payload.SignerGeneration)
	}
	switch payload.Scope {
	case rpc.OrderTokenScopePlace, rpc.OrderTokenScopeModify, rpc.OrderTokenScopeExercise, rpc.OrderTokenScopeStrategy:
	default:
		return orderPreviewTokenPayload{}, fmt.Errorf("unsupported order preview token scope %q", payload.Scope)
	}
	if payload.WhatIf.Status == "" && payload.WhatIfStatus != "" {
		payload.WhatIf.Status = payload.WhatIfStatus
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	if !payload.ExpiresAt.IsZero() && !now.Before(payload.ExpiresAt) {
		return orderPreviewTokenPayload{}, fmt.Errorf("order preview token expired at %s", payload.ExpiresAt.Format(time.RFC3339))
	}
	return payload, nil
}

func randomTokenID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate order preview token id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func (s *Server) handleOrderPreview(ctx context.Context, req *rpc.Request) (*rpc.OrderPreviewResult, error) {
	var p rpc.OrderPreviewParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.previewOrder(ctx, p)
}

func (s *Server) previewOrder(ctx context.Context, p rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	if s == nil {
		return nil, ErrTradingDisabled
	}
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.tradingStatus(ep)
	if status.Mode == config.TradingModeDisabled {
		return nil, fmt.Errorf("%w: set [trading].mode to paper or live before order preview", ErrTradingDisabled)
	}
	if status.Blocked {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(status.Blockers))
	}
	if s.orderTokens == nil {
		return nil, fmt.Errorf("%w: order preview token signer is unavailable", ErrTradingDisabled)
	}
	scope := rpc.OrderTokenScopePlace
	var replaceView rpc.OrderView
	replaceID := strings.TrimSpace(p.ReplaceID)
	if replaceID != "" {
		if !status.CanWrite {
			return nil, fmt.Errorf("%w: broker modify is not available", ErrTradingDisabled)
		}
		view, err := s.openOrderViewForWrite(replaceID, status)
		if err != nil {
			return nil, err
		}
		if !view.ModifyEligible {
			return nil, errBadRequest("replacement preview target is not modify-eligible")
		}
		if view.ConID <= 0 {
			return nil, errBadRequest("replacement preview target has no exact positive contract identity")
		}
		scope = rpc.OrderTokenScopeModify
		replaceView = view
	}

	cfg, tradingControlGeneration := s.effectiveTradingControlSnapshot()
	action, err := normalizeOrderAction(p.Action)
	if err != nil {
		return nil, err
	}
	contract, err := normalizePreviewContract(p.Contract)
	if err != nil {
		return nil, err
	}
	if p.Quantity <= 0 {
		return nil, errBadRequest("quantity must be positive")
	}
	if scope == rpc.OrderTokenScopeModify {
		contract = modifyContractForView(replaceView, contract)
	}
	timeout := orderPreviewTimeout(p.TimeoutMs)
	previewAuthority, err := s.captureOrderPreviewBrokerAuthority()
	if err != nil {
		return nil, err
	}
	contract, err = s.resolvePreviewOrderContract(ctx, previewAuthority, contract, min(timeout, previewMinTickTimeout))
	if err != nil {
		return nil, err
	}
	if contract.MinTick <= 0 && previewAuthority == nil {
		// Socket-free unit seams retain the historical helper. Production uses
		// the min tick returned by the exact session-bound resolver and lets
		// pricing's static grid handle an explicitly omitted broker MinTick.
		contract.MinTick = s.resolveContractMinTick(ctx, contract, previewMinTickTimeout)
	}
	// Position impact resolves before the size caps so sell-side apparent exits
	// manual TWS order has not already consumed the apparent exit capacity.
	var positionAuthority orderPositionAuthority
	if previewAuthority != nil {
		positionAuthority, err = s.captureBoundOrderPositionAuthority(ctx, previewAuthority.connector, previewAuthority.session, status, contract, action, p.Quantity)
	} else {
		positionAuthority, err = s.capturePreviewOrderPositionAuthority(ctx, status, contract, action, p.Quantity)
	}
	if err != nil {
		return nil, err
	}
	position := positionAuthority.Impact
	orderType := strings.ToUpper(strings.TrimSpace(p.OrderType))
	if orderType == "" {
		orderType = rpc.OrderTypeLMT
	}
	if !previewSupportedOrderType(orderType) {
		return nil, errBadRequest("order preview supports LMT, TRAIL, and TRAIL LIMIT orders only")
	}
	tif := strings.ToUpper(strings.TrimSpace(p.TIF))
	if tif == "" && scope == rpc.OrderTokenScopeModify {
		// Modify previews freeze TIF to the open order; defaulting to DAY here
		tif = strings.ToUpper(strings.TrimSpace(replaceView.TIF))
	}
	if tif == "" {
		tif = rpc.OrderTIFDay
	}
	if tif != rpc.OrderTIFDay && !(tif == rpc.OrderTIFGTC && isTrailOrderType(orderType)) {
		return nil, errBadRequest("order preview supports DAY time-in-force, or GTC for TRAIL and TRAIL LIMIT orders")
	}
	triggerMethod, err := previewOrderTriggerMethod(orderType, contract, p.TriggerMethod)
	if err != nil {
		return nil, err
	}
	if scope == rpc.OrderTokenScopeModify && p.TriggerMethod == rpc.OrderTriggerMethodDefault {
		triggerMethod = replaceView.TriggerMethod
	}

	quote, err := s.fetchPreviewQuoteBound(ctx, contract, timeout, previewAuthority)
	if err != nil {
		return nil, err
	}
	strategy, limit, trail, notionalPrice, err := previewOrderPricing(action, orderType, p.Strategy, p.LimitPrice, p.Trail, contract, quote)
	if err != nil {
		return nil, err
	}
	notional := float64(p.Quantity) * notionalPrice * float64(contractMultiplier(contract))

	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	draft := rpc.OrderDraft{
		Action:        action,
		Contract:      contract,
		Quantity:      p.Quantity,
		OrderType:     orderType,
		LimitPrice:    limit,
		Trail:         trail,
		TriggerMethod: triggerMethod,
		TIF:           tif,
		OutsideRTH:    p.OutsideRTH,
		Strategy:      strategy,
		OrderRef:      previewOrderRef(now),
		OpenClose:     orderOpenCloseForEffect(position.Effect),
		Source:        strings.TrimSpace(p.Source),
	}
	if scope == rpc.OrderTokenScopeModify {
		if err := validateModifyDraft(replaceView, draft); err != nil {
			return nil, err
		}
		// Position-mismatch gate: position.Before is this preview's fresh
		// positions read (the preview already failed closed above if
		// positions were unavailable).
		if err := validateProtectiveModifyQuantity(replaceView, draft, position.Before); err != nil {
			return nil, err
		}
	}
	notionalAuthority, err := s.captureOrderNotionalAuthority(ctx, previewAuthority, notional, contract.Currency, positionAuthority.BaseCurrency, timeout)
	if err != nil {
		return nil, err
	}
	if err := validateOrderRiskAuthority(cfg, draft, position, notionalAuthority, positionAuthority.BaseCurrency); err != nil {
		return nil, errBadRequest(err.Error())
	}
	var whatIf rpc.OrderWhatIfResult
	if scope == rpc.OrderTokenScopeModify {
		whatIf, err = s.fetchModifyPreviewWhatIfBound(ctx, status, replaceView, draft, timeout, previewAuthority)
	} else {
		whatIf, err = s.fetchPreviewWhatIfBound(ctx, status, draft, timeout, previewAuthority)
	}
	if err != nil {
		return nil, err
	}
	if previewAuthority != nil && !s.orderPreviewBrokerAuthorityCurrent(previewAuthority) {
		return nil, fmt.Errorf("%w: broker session changed before preview token mint", ErrTradingDisabled)
	}
	if _, currentControlGeneration := s.effectiveTradingControlSnapshot(); currentControlGeneration != tradingControlGeneration {
		return nil, fmt.Errorf("%w: trading controls changed during preview; refresh and retry", ErrTradingDisabled)
	}
	token, tokenID, expiresAt, err := s.orderTokens.mint(orderPreviewTokenPayload{
		Scope:                    scope,
		Mode:                     status.Mode,
		Account:                  status.Account,
		Endpoint:                 status.Endpoint,
		ClientID:                 status.ClientID,
		Draft:                    draft,
		Quote:                    quote,
		Position:                 position,
		PortfolioGeneration:      positionAuthority.Generation,
		PortfolioAccount:         positionAuthority.Health.Account,
		PortfolioEvidenceAt:      positionAuthority.EvidenceAt,
		BaseCurrency:             positionAuthority.BaseCurrency,
		BaseCurrencyProvenance:   positionAuthority.BaseCurrencyProvenance,
		TradingControlGeneration: tradingControlGeneration,
		Notional:                 notional,
		NotionalAuthority:        notionalAuthority,
		WhatIf:                   whatIf,
		WhatIfStatus:             whatIf.Status,
		Replace:                  replaceTargetFromView(replaceView),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTradingDisabled, err)
	}
	if err := s.orderJournal.Append(orderJournalEvent{
		At:             now,
		Type:           orderJournalEventPreviewed,
		OrderRef:       draft.OrderRef,
		PreviewTokenID: tokenID,
		ClientID:       status.ClientID,
		Account:        status.Account,
		Endpoint:       status.Endpoint,
		Mode:           status.Mode,
		Symbol:         draft.Contract.Symbol,
		SecType:        draft.Contract.SecType,
		ConID:          draft.Contract.ConID,
		Exchange:       draft.Contract.Exchange,
		PrimaryExch:    draft.Contract.PrimaryExch,
		Currency:       draft.Contract.Currency,
		LocalSymbol:    draft.Contract.LocalSymbol,
		TradingClass:   draft.Contract.TradingClass,
		Expiry:         draft.Contract.Expiry,
		Strike:         draft.Contract.Strike,
		Right:          draft.Contract.Right,
		Multiplier:     draft.Contract.Multiplier,
		Action:         draft.Action,
		OrderType:      draft.OrderType,
		TIF:            draft.TIF,
		TriggerMethod:  draft.TriggerMethod,
		OutsideRTH:     draft.OutsideRTH,
		Quantity:       float64(draft.Quantity),
		LimitPrice:     draft.LimitPrice,
		Trail:          cloneTrailSpec(draft.Trail),
		OpenClose:      draft.OpenClose,
		Source:         draft.Source,
		Message:        previewWhatIfJournalMessage(whatIf),
	}); err != nil {
		return nil, fmt.Errorf("%w: append preview journal: %v", ErrTradingDisabled, err)
	}

	warnings := append([]rpc.DataWarning{}, quote.Warnings...)
	warnings = append(warnings, previewWhatIfWarnings(whatIf)...)
	if p.OutsideRTH {
		warnings = append(warnings, rpc.DataWarning{
			Code:     "outside_rth_requested",
			Severity: "info",
			Message:  "outside_rth=true was explicitly requested.",
		})
	}
	warnings = append(warnings, rulebookPreviewWarnings(s.rulesForPreview(ctx), draft, position)...)
	warnings = append(warnings, s.riskPolicyPreviewWarnings(draft, position)...)
	tokenMinted := token != "" && tokenID != ""
	submitEligible := tokenMinted && whatIf.Status == rpc.OrderWhatIfStatusAccepted && !whatIf.RequiredForSubmit

	maxNotional := cfg.MaxNotional
	return &rpc.OrderPreviewResult{
		PreviewToken:          token,
		PreviewTokenID:        tokenID,
		PreviewTokenScope:     scope,
		PreviewTokenExpiresAt: expiresAt,
		TokenMinted:           tokenMinted,
		SubmitEligible:        submitEligible,
		Executable:            submitEligible,
		Mode:                  status.Mode,
		Account:               status.Account,
		Endpoint:              status.Endpoint,
		ClientID:              status.ClientID,
		Draft:                 draft,
		Quote:                 quote,
		Position:              position,
		Notional:              notional,
		NotionalCurrency:      notionalAuthority.ContractCurrency,
		NotionalBase:          notionalAuthority.BaseNotional,
		BaseCurrency:          notionalAuthority.BaseCurrency,
		FXRate:                notionalAuthority.BasePerContract,
		FXEvidenceAt:          notionalAuthority.EvidenceAt,
		FXDataType:            notionalAuthority.DataType,
		FXSource:              notionalAuthority.Source,
		MaxNotional:           maxNotional,
		WhatIf:                whatIf,
		Warnings:              warnings,
		AsOf:                  now,
	}, nil
}

//lint:ignore U1000 used by trading-tag order placement paths
func (s *Server) fetchPreviewQuote(ctx context.Context, contract rpc.ContractParams, timeout time.Duration) (rpc.OrderQuoteSnapshot, error) {
	return s.fetchPreviewQuoteBound(ctx, contract, timeout, nil)
}

func (s *Server) fetchPreviewQuoteBound(ctx context.Context, contract rpc.ContractParams, timeout time.Duration, authority *orderPreviewBrokerAuthority) (rpc.OrderQuoteSnapshot, error) {
	if s.orderPreviewQuote != nil {
		return s.orderPreviewQuote(ctx, contract, timeout)
	}
	if authority != nil {
		return s.previewExactSessionQuote(ctx, authority, contract, timeout)
	}
	return s.previewQuote(ctx, contract, timeout)
}

func orderPreviewTimeout(timeoutMs int) time.Duration {
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		return orderPreviewDefaultWait
	}
	return min(timeout, orderPreviewMaxWait)
}

func (s *Server) fetchPreviewWhatIfBound(ctx context.Context, status rpc.TradingStatus, draft rpc.OrderDraft, timeout time.Duration, authority *orderPreviewBrokerAuthority) (rpc.OrderWhatIfResult, error) {
	whatIfCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if s.orderPreviewWhatIf != nil {
		return s.orderPreviewWhatIf(whatIfCtx, draft)
	}
	var c *ibkrlib.Connector
	if authority != nil {
		c = authority.connector
	} else {
		c = s.gatewayConnector()
	}
	if c == nil {
		return previewWhatIfUnavailableWithMessage("Broker WhatIf unavailable: gateway connector is not ready."), nil
	}
	var result ibkrlib.OrderWhatIfResult
	var err error
	if authority != nil {
		result, err = c.PreviewOrderWhatIfForSession(whatIfCtx, authority.session, previewIBKRStrategyContract(draft), previewIBKROrderForStatus(draft, status))
	} else {
		result, err = c.PreviewOrderWhatIf(whatIfCtx, previewIBKRStrategyContract(draft), previewIBKROrderForStatus(draft, status))
	}
	if err != nil {
		return rpc.OrderWhatIfResult{}, err
	}
	if authority != nil && !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return previewWhatIfUnavailableWithMessage("Broker WhatIf unavailable: broker session changed during preview."), nil
	}
	return rpcWhatIfResultFromBroker(result), nil
}

func (s *Server) fetchModifyPreviewWhatIfBound(ctx context.Context, status rpc.TradingStatus, view rpc.OrderView, draft rpc.OrderDraft, timeout time.Duration, authority *orderPreviewBrokerAuthority) (rpc.OrderWhatIfResult, error) {
	whatIfCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if s.orderPreviewWhatIf != nil {
		return s.orderPreviewWhatIf(whatIfCtx, draft)
	}
	var c *ibkrlib.Connector
	if authority != nil {
		c = authority.connector
	} else {
		c = s.gatewayConnector()
	}
	if c == nil {
		return previewWhatIfUnavailableWithMessage("Broker WhatIf unavailable: gateway connector is not ready."), nil
	}
	var result ibkrlib.OrderWhatIfResult
	var err error
	if authority != nil {
		result, err = c.PreviewOrderWhatIfWithOrderIDForSession(whatIfCtx, authority.session, previewIBKRStrategyContract(draft), previewIBKROrderForStatus(draft, status), view.ReservedOrderID)
	} else {
		result, err = c.PreviewOrderWhatIfWithOrderID(whatIfCtx, previewIBKRStrategyContract(draft), previewIBKROrderForStatus(draft, status), view.ReservedOrderID)
	}
	if err != nil {
		return rpc.OrderWhatIfResult{}, err
	}
	if authority != nil && !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return previewWhatIfUnavailableWithMessage("Broker WhatIf unavailable: broker session changed during preview."), nil
	}
	return rpcWhatIfResultFromBroker(result), nil
}

func previewIBKRContract(contract rpc.ContractParams) *ibkrlib.Contract {
	secType := strings.ToUpper(strings.TrimSpace(contract.SecType))
	if secType == "" {
		secType = "STK"
	}
	exchange := strings.TrimSpace(contract.Exchange)
	if exchange == "" {
		exchange = "SMART"
	}
	currency := strings.ToUpper(strings.TrimSpace(contract.Currency))
	if currency == "" {
		currency = "USD"
	}
	multiplier := contract.Multiplier
	if secType != "OPT" {
		multiplier = 0
	}
	out := &ibkrlib.Contract{
		ConID:        contract.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(contract.Symbol)),
		SecType:      secType,
		Exchange:     exchange,
		PrimaryExch:  strings.TrimSpace(contract.PrimaryExch),
		Currency:     currency,
		LocalSymbol:  strings.TrimSpace(contract.LocalSymbol),
		TradingClass: strings.TrimSpace(contract.TradingClass),
		Expiry:       strings.TrimSpace(contract.Expiry),
		Strike:       contract.Strike,
		Right:        strings.ToUpper(strings.TrimSpace(contract.Right)),
		Multiplier:   multiplier,
	}
	return out
}

func previewIBKRStrategyContract(draft rpc.OrderDraft) *ibkrlib.Contract {
	contract := previewIBKRContract(draft.Contract)
	if draft.StrategyGroup == nil {
		return contract
	}
	contract.ComboLegs = make([]ibkrlib.ComboLeg, 0, len(draft.StrategyGroup.Legs))
	for _, leg := range draft.StrategyGroup.Legs {
		action := rpc.OrderActionBuy
		if leg.Ratio < 0 {
			action = rpc.OrderActionSell
		}
		exchange := strings.ToUpper(strings.TrimSpace(leg.Contract.Exchange))
		if exchange == "" {
			exchange = "SMART"
		}
		contract.ComboLegs = append(contract.ComboLegs, ibkrlib.ComboLeg{
			ConID: leg.Contract.ConID, Ratio: absOrderRatio(leg.Ratio), Action: action, Exchange: exchange, OpenClose: 2,
		})
	}
	return contract
}

func absOrderRatio(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func previewIBKROrder(draft rpc.OrderDraft) *ibkrlib.RawOrder {
	order := &ibkrlib.RawOrder{
		Action:        strings.ToUpper(strings.TrimSpace(draft.Action)),
		TotalQty:      draft.Quantity,
		OrderType:     strings.ToUpper(strings.TrimSpace(draft.OrderType)),
		LmtPrice:      draft.LimitPrice,
		LmtPriceSet:   strings.EqualFold(strings.TrimSpace(draft.OrderType), rpc.OrderTypeLMT),
		TIF:           strings.ToUpper(strings.TrimSpace(draft.TIF)),
		TriggerMethod: draft.TriggerMethod,
		OrderRef:      draft.OrderRef,
		OutsideRth:    draft.OutsideRTH,
		OpenClose:     strings.ToUpper(strings.TrimSpace(draft.OpenClose)),
	}
	if draft.Trail != nil {
		order.TrailStopPrice = draft.Trail.InitialStopPrice
		if draft.Trail.TrailingPercent != nil {
			order.TrailingPercent = *draft.Trail.TrailingPercent
		}
		if draft.Trail.TrailingAmount != nil {
			order.AuxPrice = *draft.Trail.TrailingAmount
		}
		if draft.Trail.LimitOffset != nil {
			order.LmtPriceOffset = *draft.Trail.LimitOffset
		}
	}
	return order
}

func previewIBKROrderForStatus(draft rpc.OrderDraft, status rpc.TradingStatus) *ibkrlib.RawOrder {
	order := previewIBKROrder(draft)
	order.Account = status.Account
	order.ClientID = status.ClientID
	return order
}

func rpcWhatIfResultFromBroker(result ibkrlib.OrderWhatIfResult) rpc.OrderWhatIfResult {
	switch result.Status {
	case ibkrlib.OrderWhatIfStatusAccepted:
		return rpc.OrderWhatIfResult{
			Status:            rpc.OrderWhatIfStatusAccepted,
			RequiredForSubmit: false,
			Available:         true,
			Message:           strings.TrimSpace(result.Message),
			Margin:            rpcMarginFromBroker(result.Margin),
		}
	case ibkrlib.OrderWhatIfStatusRejected:
		msg := strings.TrimSpace(result.Message)
		if msg == "" {
			msg = "Broker WhatIf rejected this order draft."
		}
		return rpc.OrderWhatIfResult{
			Status:             rpc.OrderWhatIfStatusRejected,
			RequiredForSubmit:  true,
			Available:          true,
			Message:            msg,
			AdvancedRejectJSON: strings.TrimSpace(result.AdvancedRejectJSON),
			Action:             "Adjust the draft and run order preview again before any submit attempt.",
			Margin:             rpcMarginFromBroker(result.Margin),
		}
	default:
		msg := strings.TrimSpace(result.Message)
		if msg == "" {
			msg = "Broker WhatIf did not return an accepted preview."
		}
		return previewWhatIfUnavailableWithMessage(msg)
	}
}

func rpcMarginFromBroker(in ibkrlib.OrderWhatIfMargin) *rpc.OrderMarginImpact {
	out := &rpc.OrderMarginImpact{
		Currency:                in.Currency,
		InitialMarginBefore:     cloneFloat64Ptr(in.InitialMarginBefore),
		InitialMarginAfter:      cloneFloat64Ptr(in.InitialMarginAfter),
		MaintenanceMarginBefore: cloneFloat64Ptr(in.MaintenanceMarginBefore),
		MaintenanceMarginAfter:  cloneFloat64Ptr(in.MaintenanceMarginAfter),
		EquityWithLoanBefore:    cloneFloat64Ptr(in.EquityWithLoanBefore),
		EquityWithLoanAfter:     cloneFloat64Ptr(in.EquityWithLoanAfter),
		Commission:              cloneFloat64Ptr(in.Commission),
		MinCommission:           cloneFloat64Ptr(in.MinCommission),
		MaxCommission:           cloneFloat64Ptr(in.MaxCommission),
		CommissionCurrency:      in.CommissionCurrency,
		WarningText:             in.WarningText,
	}
	if out.Currency == "" &&
		out.InitialMarginBefore == nil &&
		out.InitialMarginAfter == nil &&
		out.MaintenanceMarginBefore == nil &&
		out.MaintenanceMarginAfter == nil &&
		out.EquityWithLoanBefore == nil &&
		out.EquityWithLoanAfter == nil &&
		out.Commission == nil &&
		out.MinCommission == nil &&
		out.MaxCommission == nil &&
		out.CommissionCurrency == "" &&
		out.WarningText == "" {
		return nil
	}
	return out
}

func cloneFloat64Ptr(in *float64) *float64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func firstTradingBlockerMessage(blockers []rpc.TradingBlocker) string {
	if len(blockers) == 0 {
		return "trading gate is blocked"
	}
	return blockers[0].Message
}

func normalizeOrderAction(action string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(action)) {
	case "BUY":
		return rpc.OrderActionBuy, nil
	case "SELL":
		return rpc.OrderActionSell, nil
	default:
		return "", errBadRequest("action must be buy or sell")
	}
}

func normalizePreviewContract(in rpc.ContractParams) (rpc.ContractParams, error) {
	secType := strings.ToUpper(strings.TrimSpace(in.SecType))
	if secType == "" {
		secType = "STK"
	}
	switch secType {
	case "STK", "ETF":
		_, echo, _, err := normaliseStockQuoteContract(rpc.ContractParams{
			ConID:        in.ConID,
			Symbol:       in.Symbol,
			SecType:      secType,
			Market:       in.Market,
			Exchange:     in.Exchange,
			PrimaryExch:  in.PrimaryExch,
			Currency:     in.Currency,
			LocalSymbol:  in.LocalSymbol,
			TradingClass: in.TradingClass,
			Multiplier:   in.Multiplier,
		})
		if err != nil {
			return rpc.ContractParams{}, err
		}
		if echo.Exchange == "" {
			echo.Exchange = "SMART"
		}
		if echo.Currency == "" {
			echo.Currency = "USD"
		}
		echo.SecType = secType
		echo.ConID = in.ConID
		echo.Multiplier = max(in.Multiplier, 1)
		return echo, nil
	case "OPT":
		return normaliseOptionQuoteContract(in)
	default:
		return rpc.ContractParams{}, errBadRequest("order preview supports STK/ETF/OPT contracts only")
	}
}

func normalizePreviewStrategy(strategy string, limit *float64) string {
	strategy = strings.ToLower(strings.TrimSpace(strategy))
	if strategy == "" {
		if limit != nil {
			return rpc.OrderStrategyExplicitLimit
		}
		return rpc.OrderStrategyPatientLimit
	}
	return strategy
}

func previewSupportedOrderType(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeLMT, rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return true
	default:
		return false
	}
}

func previewOrderTriggerMethod(orderType string, contract rpc.ContractParams, requested int) (int, error) {
	if requested != rpc.OrderTriggerMethodDefault {
		if !isTrailOrderType(orderType) {
			return 0, errBadRequest("trigger_method is supported for TRAIL and TRAIL LIMIT orders only")
		}
		if !validOrderTriggerMethod(requested) {
			return 0, errBadRequest("trigger_method must be one of 1, 2, 3, 4, 7, or 8")
		}
		return requested, nil
	}
	if isTrailOrderType(orderType) && trailTriggerDefaultsToLast(contract) {
		return rpc.OrderTriggerMethodLast, nil
	}
	return rpc.OrderTriggerMethodDefault, nil
}

func validOrderTriggerMethod(method int) bool {
	switch method {
	case rpc.OrderTriggerMethodDoubleBidAsk,
		rpc.OrderTriggerMethodLast,
		rpc.OrderTriggerMethodDoubleLast,
		rpc.OrderTriggerMethodBidAsk,
		rpc.OrderTriggerMethodLastOrBidAsk,
		rpc.OrderTriggerMethodMidpoint:
		return true
	default:
		return false
	}
}

func trailTriggerDefaultsToLast(contract rpc.ContractParams) bool {
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "STOCK", "ETF":
		return true
	default:
		return false
	}
}

func previewOrderPricing(action, orderType, rawStrategy string, explicit *float64, rawTrail *rpc.OrderTrailSpec, contract rpc.ContractParams, quote rpc.OrderQuoteSnapshot) (strategy string, limit float64, trail *rpc.OrderTrailSpec, notionalPrice float64, err error) {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeLMT:
		if rawTrail != nil {
			return "", 0, nil, 0, errBadRequest("LMT order preview must not include trail fields")
		}
		strategy = normalizePreviewStrategy(rawStrategy, explicit)
		limit, err = previewLimitPrice(action, strategy, explicit, contract, quote)
		return strategy, limit, nil, limit, err
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		strategy = strings.ToLower(strings.TrimSpace(rawStrategy))
		if strategy == "" {
			strategy = rpc.OrderStrategyBrokerTrail
		}
		if strategy != rpc.OrderStrategyBrokerTrail {
			return "", 0, nil, 0, errBadRequest("TRAIL order preview requires broker-trail strategy")
		}
		if explicit != nil {
			return "", 0, nil, 0, errBadRequest("TRAIL order preview must not include limit_price")
		}
		trail, err = previewTrailSpec(action, orderType, rawTrail, contract, quote)
		if err != nil {
			return "", 0, nil, 0, err
		}
		notionalPrice = trailNotionalReferencePrice(action, trail, quote)
		return strategy, 0, trail, notionalPrice, nil
	default:
		return "", 0, nil, 0, errBadRequest("unsupported order type")
	}
}

func previewTrailSpec(action, orderType string, raw *rpc.OrderTrailSpec, contract rpc.ContractParams, quote rpc.OrderQuoteSnapshot) (*rpc.OrderTrailSpec, error) {
	if raw == nil {
		return nil, errBadRequest("TRAIL order preview requires trail fields")
	}
	out := cloneTrailSpec(raw)
	out.Basis = strings.ToLower(strings.TrimSpace(out.Basis))
	if out.Basis == "" {
		out.Basis = rpc.OrderTrailBasisInstrumentPrice
	}
	if out.Basis != rpc.OrderTrailBasisInstrumentPrice {
		return nil, errBadRequest("trail basis must be instrument_price")
	}
	out.OffsetType = strings.ToLower(strings.TrimSpace(out.OffsetType))
	hasPercent := out.TrailingPercent != nil
	hasAmount := out.TrailingAmount != nil
	switch {
	case hasPercent == hasAmount:
		return nil, errBadRequest("trail requires exactly one of trailing_percent or trailing_amount")
	case hasPercent:
		if !positiveFinite(*out.TrailingPercent) {
			return nil, errBadRequest("trailing_percent must be positive")
		}
		if out.OffsetType == "" {
			out.OffsetType = rpc.OrderTrailOffsetPercent
		}
		if out.OffsetType != rpc.OrderTrailOffsetPercent {
			return nil, errBadRequest("trail offset_type must match trailing_percent")
		}
	case hasAmount:
		if !positiveFinite(*out.TrailingAmount) {
			return nil, errBadRequest("trailing_amount must be positive")
		}
		if out.OffsetType == "" {
			out.OffsetType = rpc.OrderTrailOffsetAmount
		}
		if out.OffsetType != rpc.OrderTrailOffsetAmount {
			return nil, errBadRequest("trail offset_type must match trailing_amount")
		}
		amount := ceilPriceToTick(*out.TrailingAmount, trailMinimumTick(contract, *out.TrailingAmount))
		out.TrailingAmount = &amount
	}
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeTRAIL:
		if out.LimitOffset != nil {
			return nil, errBadRequest("TRAIL order preview must not include limit_offset")
		}
	case rpc.OrderTypeTRAILLIMIT:
		if out.LimitOffset == nil || !positiveFinite(*out.LimitOffset) {
			return nil, errBadRequest("TRAIL LIMIT order preview requires positive limit_offset")
		}
		offset := ceilPriceToTick(*out.LimitOffset, trailMinimumTick(contract, *out.LimitOffset))
		out.LimitOffset = &offset
	}
	if out.InitialStopPrice <= 0 {
		reference, err := trailQuoteReferencePrice(action, quote)
		if err != nil {
			// The wire validators reject trailStopPrice <= 0, so a draft
			// without a seedable stop can never transmit — surface the real
			// reason (no live reference) instead of a confusing broker
			// "trail stop price required" failure later.
			return nil, fmt.Errorf("%w; provide initial_stop_price explicitly or retry with live bid/ask", err)
		}
		offset := trailOffsetAmount(out, reference)
		out.InitialStopPrice = trailingStopInitialPriceForContract(action, reference, offset, contract)
	} else if !positiveFinite(out.InitialStopPrice) {
		return nil, errBadRequest("initial_stop_price must be positive")
	} else {
		out.InitialStopPrice = roundStopPriceForContract(action, out.InitialStopPrice, contract)
	}
	return out, nil
}

func trailQuoteReferencePrice(action string, quote rpc.OrderQuoteSnapshot) (float64, error) {
	if err := requireFreshPreviewQuote(quote, "broker-trail"); err != nil {
		return 0, err
	}
	if !rpc.IsLiveDataType(quote.DataType) {
		return 0, errBadRequest("broker-trail requires live bid/ask data")
	}
	switch action {
	case rpc.OrderActionSell:
		if quote.Bid == nil || *quote.Bid <= 0 {
			return 0, errBadRequest("broker-trail SELL requires a positive bid")
		}
		return *quote.Bid, nil
	case rpc.OrderActionBuy:
		if quote.Ask == nil || *quote.Ask <= 0 {
			return 0, errBadRequest("broker-trail BUY requires a positive ask")
		}
		return *quote.Ask, nil
	default:
		return 0, errBadRequest("action must be buy or sell")
	}
}

func trailOffsetAmount(trail *rpc.OrderTrailSpec, reference float64) float64 {
	if trail == nil {
		return 0
	}
	if trail.TrailingPercent != nil {
		return reference * *trail.TrailingPercent / 100
	}
	if trail.TrailingAmount != nil {
		return *trail.TrailingAmount
	}
	return 0
}

func trailNotionalReferencePrice(action string, trail *rpc.OrderTrailSpec, quote rpc.OrderQuoteSnapshot) float64 {
	if ref, err := trailQuoteReferencePrice(action, quote); err == nil && ref > 0 {
		return ref
	}
	if trail != nil && trail.InitialStopPrice > 0 {
		return trail.InitialStopPrice
	}
	return 0
}

func positiveFinite(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func (s *Server) previewQuote(ctx context.Context, contract rpc.ContractParams, timeout time.Duration) (rpc.OrderQuoteSnapshot, error) {
	params := rpc.QuoteSnapshotParams{
		Contract:         contract,
		TimeoutMs:        int(timeout.Milliseconds()),
		IncludeLiquidity: true,
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return rpc.OrderQuoteSnapshot{}, fmt.Errorf("marshal quote params: %w", err)
	}
	q, err := s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
	if err != nil {
		return rpc.OrderQuoteSnapshot{}, err
	}
	if q == nil {
		return rpc.OrderQuoteSnapshot{}, errBadRequest("quote snapshot unavailable")
	}
	return orderQuoteSnapshotFromQuote(q), nil
}

func orderQuoteSnapshotFromQuote(q *rpc.Quote) rpc.OrderQuoteSnapshot {
	if q == nil {
		return rpc.OrderQuoteSnapshot{}
	}
	out := rpc.OrderQuoteSnapshot{
		Symbol:         q.Symbol,
		Bid:            q.Bid,
		Ask:            q.Ask,
		Last:           q.Last,
		Mark:           q.Mark,
		DataType:       q.DataType,
		QuoteQuality:   q.QuoteQuality,
		SpreadPct:      q.SpreadPct,
		PriceAt:        q.PriceAt,
		PriceAsOf:      q.PriceAsOf,
		Stale:          q.Stale,
		StaleReason:    q.StaleReason,
		AsOf:           q.AsOf,
		SessionContext: q.SessionContext,
		Warnings:       append([]rpc.DataWarning{}, q.WarningDetails...),
	}
	if q.Bid != nil && q.Ask != nil && *q.Bid > 0 && *q.Ask > *q.Bid {
		mid := (*q.Bid + *q.Ask) / 2
		out.Midpoint = &mid
	}
	return out
}

func (s *Server) previewExactSessionQuote(ctx context.Context, authority *orderPreviewBrokerAuthority, contract rpc.ContractParams, timeout time.Duration) (rpc.OrderQuoteSnapshot, error) {
	if authority == nil || authority.connector == nil || contract.ConID <= 0 || !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return rpc.OrderQuoteSnapshot{}, fmt.Errorf("%w: exact broker quote authority is unavailable", ErrTradingDisabled)
	}
	return s.previewExactSessionContractQuote(ctx, authority, contract, timeout)
}

// previewExactSessionFXQuote captures one direct CASH/IDEALPRO pair from the
// same physical session as the order preview. Currency-pair identity is fully
// specified by Symbol/Currency/SecType/Exchange; unlike stocks and options it
// does not need a contract-details round trip or positive ConID before market
// data can be requested.
func (s *Server) previewExactSessionFXQuote(ctx context.Context, authority *orderPreviewBrokerAuthority, contract rpc.ContractParams, timeout time.Duration) (rpc.OrderQuoteSnapshot, error) {
	if authority == nil || authority.connector == nil ||
		!strings.EqualFold(strings.TrimSpace(contract.SecType), "CASH") ||
		!strings.EqualFold(strings.TrimSpace(contract.Exchange), "IDEALPRO") ||
		strings.TrimSpace(contract.Symbol) == "" || strings.TrimSpace(contract.Currency) == "" ||
		strings.EqualFold(strings.TrimSpace(contract.Symbol), strings.TrimSpace(contract.Currency)) ||
		!s.orderPreviewBrokerAuthorityCurrent(authority) {
		return rpc.OrderQuoteSnapshot{}, fmt.Errorf("%w: exact broker FX quote authority is unavailable", ErrTradingDisabled)
	}
	return s.previewExactSessionContractQuoteWithReady(ctx, authority, contract, timeout, func(q *rpc.Quote) bool {
		return q.Bid != nil && q.Ask != nil
	})
}

func (s *Server) previewExactSessionContractQuote(ctx context.Context, authority *orderPreviewBrokerAuthority, contract rpc.ContractParams, timeout time.Duration) (rpc.OrderQuoteSnapshot, error) {
	return s.previewExactSessionContractQuoteWithReady(ctx, authority, contract, timeout, nil)
}

func (s *Server) previewExactSessionContractQuoteWithReady(ctx context.Context, authority *orderPreviewBrokerAuthority, contract rpc.ContractParams, timeout time.Duration, ready func(*rpc.Quote) bool) (rpc.OrderQuoteSnapshot, error) {
	quoteCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	key, err := authority.connector.SubscribeMarketDataWithContractForSession(quoteCtx, authority.session, *previewIBKRContract(contract), defaultGenericTicks)
	if err != nil {
		return rpc.OrderQuoteSnapshot{}, fmt.Errorf("%w: exact contract quote request failed: %v", ErrTradingDisabled, err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		_ = authority.connector.UnsubscribeMarketDataForSession(cleanupCtx, authority.session, key)
	}()
	q := &rpc.Quote{Symbol: contract.Symbol, Contract: contract, IVStatus: "unavailable", AsOf: s.orderNow()}
	started := time.Now()
	if err := pollMarketData(quoteCtx, authority.connector, key, started.Add(timeout), func(data *ibkrlib.MarketData) bool {
		fillQuoteMarketData(q, data)
		hasPrice := q.Bid != nil || q.Ask != nil || q.Last != nil || q.Mark != nil
		if hasPrice {
			q.DataType = quoteDataTypeName(authority.connector.MarketDataTypeForSymbol(key), true, false)
		}
		if ready != nil {
			return ready(q)
		}
		return hasPrice
	}); err != nil {
		return rpc.OrderQuoteSnapshot{}, fmt.Errorf("%w: exact contract quote unavailable: %v", ErrTradingDisabled, err)
	}
	if !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return rpc.OrderQuoteSnapshot{}, fmt.Errorf("%w: broker session changed during exact quote", ErrTradingDisabled)
	}
	q.AsOf = s.orderNow()
	s.decorateExactPreviewQuote(q, contract)
	return orderQuoteSnapshotFromQuote(q), nil
}

func (s *Server) decorateExactPreviewQuote(q *rpc.Quote, contract rpc.ContractParams) {
	if market, ok := quoteSessionMarketForContract(contract); ok {
		s.attachQuoteSessionContext(q, market)
		s.decorateQuote(q, market)
	}
}

func previewLimitPrice(action, strategy string, explicit *float64, contract rpc.ContractParams, quote rpc.OrderQuoteSnapshot) (float64, error) {
	switch strategy {
	case rpc.OrderStrategyExplicitLimit:
		if explicit == nil {
			return 0, errBadRequest("explicit-limit strategy requires --limit")
		}
		if *explicit <= 0 || math.IsNaN(*explicit) || math.IsInf(*explicit, 0) {
			return 0, errBadRequest("limit price must be positive")
		}
		return roundPrice(*explicit), nil
	case rpc.OrderStrategyPatientLimit:
		if explicit != nil {
			return 0, errBadRequest("--limit uses explicit-limit strategy; omit --strategy or set --strategy explicit-limit")
		}
		if err := requireFreshPreviewQuote(quote, "patient-limit"); err != nil {
			return 0, err
		}
		if !rpc.IsLiveDataType(quote.DataType) {
			return 0, errBadRequest("patient-limit requires live bid/ask data; use --limit for explicit-limit preview on stale or delayed data")
		}
		if quote.Bid == nil || quote.Ask == nil || *quote.Bid <= 0 || *quote.Ask <= *quote.Bid {
			return 0, errBadRequest("patient-limit requires a positive two-sided bid/ask")
		}
		mid := (*quote.Bid + *quote.Ask) / 2
		tick := patientLimitTick(contract, quote, mid)
		switch action {
		case rpc.OrderActionBuy:
			return roundPrice(max(floorPriceToTick(mid, tick), *quote.Bid)), nil
		case rpc.OrderActionSell:
			return roundPrice(min(ceilPriceToTick(mid, tick), *quote.Ask)), nil
		default:
			return 0, errBadRequest("action must be buy or sell")
		}
	default:
		return 0, errBadRequest("strategy must be patient-limit or explicit-limit")
	}
}

func requireFreshPreviewQuote(quote rpc.OrderQuoteSnapshot, useCase string) error {
	if quote.Stale {
		reason := strings.TrimSpace(quote.StaleReason)
		if reason == "" {
			reason = "quote data is stale"
		}
		return errBadRequest(fmt.Sprintf("%s requires fresh quote data: %s", useCase, reason))
	}
	if quote.SessionContext != nil && !quote.SessionContext.IsOpen {
		label := strings.TrimSpace(quote.SessionContext.State)
		if label == "" {
			label = "market is closed"
		}
		return errBadRequest(fmt.Sprintf("%s requires an open market session: %s", useCase, label))
	}
	return nil
}

func priceTick(price float64) float64 {
	if price < 1 {
		return 0.0001
	}
	return 0.01
}

// US option exchanges enforce minimum price variations in bands: $0.01 below
// alone under-rounds above the band boundary — broker error 110 at WhatIf.
const (
	optionPennyBandCeiling = 3.00
	optionCoarseTick       = 0.05
)

// patientLimitTick returns the price grid a patient-limit draft must land on.
// there, never finer than $0.01, and never finer than the broker-reported
// and broker WhatIf stays the fail-closed backstop.
func patientLimitTick(contract rpc.ContractParams, quote rpc.OrderQuoteSnapshot, mid float64) float64 {
	if strings.ToUpper(strings.TrimSpace(contract.SecType)) != "OPT" {
		return priceTick(mid)
	}
	tick := 0.01
	if mid >= optionPennyBandCeiling && !quoteProvesPennyIncrements(quote) {
		tick = optionCoarseTick
	}
	return max(tick, contract.MinTick)
}

// quoteProvesPennyIncrements reports whether the live tape shows the venue
// at/above $3.00 sitting off the nickel grid can only print for a class
func quoteProvesPennyIncrements(quote rpc.OrderQuoteSnapshot) bool {
	for _, side := range []*float64{quote.Bid, quote.Ask} {
		if side == nil || *side < optionPennyBandCeiling {
			continue
		}
		// Wire quotes arrive float32-truncated (19.05 reads back as
		// 19.049999...): snap to the penny grid before testing nickel
		// alignment, or the noise itself reads as penny proof.
		cents := math.Round(*side * 100)
		if math.Mod(cents, 5) != 0 {
			return true
		}
	}
	return false
}

func roundPrice(price float64) float64 {
	return math.Round(price*10000) / 10000
}

func trailMinimumTick(contract rpc.ContractParams, price float64) float64 {
	// Prefer the broker-reported venue increment (MiFID-banded grids on
	// broker error 110 at WhatIf/place). Zero means unresolved: fall back
	// to the static US-style grid and let WhatIf fail closed.
	if contract.MinTick > 0 {
		return contract.MinTick
	}
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "ETF":
		return 0.01
	default:
		return priceTick(price)
	}
}

func ceilPriceToTick(price, tick float64) float64 {
	if tick <= 0 {
		return roundPrice(price)
	}
	return roundPrice(math.Ceil((price-1e-9)/tick) * tick)
}

func floorPriceToTick(price, tick float64) float64 {
	if tick <= 0 {
		return roundPrice(price)
	}
	return roundPrice(math.Floor((price+1e-9)/tick) * tick)
}

func roundStopPriceForContract(action string, price float64, contract rpc.ContractParams) float64 {
	tick := trailMinimumTick(contract, price)
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		return ceilPriceToTick(price, tick)
	}
	return floorPriceToTick(price, tick)
}

func trailingStopInitialPriceForContract(action string, reference, trailAbs float64, contract rpc.ContractParams) float64 {
	if reference <= 0 || trailAbs <= 0 {
		return 0
	}
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		return roundStopPriceForContract(action, reference+trailAbs, contract)
	}
	return roundStopPriceForContract(action, max(reference-trailAbs, 0.0001), contract)
}

func contractMultiplier(contract rpc.ContractParams) int {
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "ETF":
		return 1
	case "OPT":
		if contract.Multiplier > 0 {
			return contract.Multiplier
		}
		return 100
	default:
		if contract.Multiplier > 0 {
			return contract.Multiplier
		}
		return 1
	}
}

func classifyPositionEffect(before, after float64) string {
	switch {
	case before == 0 && after > 0:
		return rpc.OrderPositionEffectOpen
	case before > 0 && after > before:
		return rpc.OrderPositionEffectIncrease
	case before > 0 && after > 0 && after < before:
		return rpc.OrderPositionEffectReduce
	case before > 0 && after == 0:
		return rpc.OrderPositionEffectClose
	case before == 0 && after < 0:
		return rpc.OrderPositionEffectOpenShort
	case before >= 0 && after < 0:
		return rpc.OrderPositionEffectFlip
	case before < 0 && after > 0:
		return rpc.OrderPositionEffectFlip
	case before < 0 && after == 0:
		return rpc.OrderPositionEffectClose
	case before < 0 && after < 0 && math.Abs(after) < math.Abs(before):
		return rpc.OrderPositionEffectReduce
	case before < 0 && after < before:
		return rpc.OrderPositionEffectIncrease
	default:
		return rpc.OrderPositionEffectIncrease
	}
}

func stockShortOrFlip(effect string) bool {
	return effect == rpc.OrderPositionEffectFlip || effect == rpc.OrderPositionEffectOpenShort
}

// isRiskReducing describes the arithmetic effect against the current position.
// account-global for this daemon, so pre-trade enforcement evaluates apparent
// exits conservatively in validateOrderRiskAuthority.
func isRiskReducing(effect string) bool {
	return effect == rpc.OrderPositionEffectClose || effect == rpc.OrderPositionEffectReduce
}

func optionSellToOpen(action, effect string) bool {
	return action == rpc.OrderActionSell && (effect == rpc.OrderPositionEffectOpenShort || effect == rpc.OrderPositionEffectFlip)
}

func orderOpenCloseForEffect(effect string) string {
	switch effect {
	case rpc.OrderPositionEffectClose, rpc.OrderPositionEffectReduce:
		return "C"
	default:
		return "O"
	}
}

func previewWhatIfUnavailable() rpc.OrderWhatIfResult {
	return rpc.OrderWhatIfResult{
		Status:            rpc.OrderWhatIfStatusUnavailable,
		RequiredForSubmit: true,
		Available:         false,
		Message:           "Broker WhatIf did not return an accepted preview; no broker order was placed.",
		Action:            "Run order preview again with a ready broker WhatIf path before any submit attempt.",
	}
}

func previewWhatIfUnavailableWithMessage(message string) rpc.OrderWhatIfResult {
	out := previewWhatIfUnavailable()
	if msg := strings.TrimSpace(message); msg != "" {
		out.Message = msg
		out.Action = "Resolve broker WhatIf availability and run order preview again before any submit attempt."
	}
	return out
}

func previewWhatIfJournalMessage(whatIf rpc.OrderWhatIfResult) string {
	switch whatIf.Status {
	case rpc.OrderWhatIfStatusAccepted:
		return "preview token minted with broker WhatIf acceptance; no broker order was placed"
	case rpc.OrderWhatIfStatusRejected:
		return "preview token minted with broker WhatIf rejection; later place gate must reject this token"
	default:
		return "preview token minted; broker WhatIf unavailable or not accepted"
	}
}

func previewWhatIfWarnings(whatIf rpc.OrderWhatIfResult) []rpc.DataWarning {
	switch whatIf.Status {
	case rpc.OrderWhatIfStatusAccepted:
		return nil
	case rpc.OrderWhatIfStatusRejected:
		msg := strings.TrimSpace(whatIf.Message)
		if msg == "" {
			msg = "Broker WhatIf rejected this order draft."
		}
		action := strings.TrimSpace(whatIf.Action)
		if action == "" {
			action = "Adjust the draft and preview again before any submit attempt."
		}
		return []rpc.DataWarning{{
			Code:     "broker_what_if_rejected",
			Severity: "error",
			Message:  msg,
			Action:   action,
		}}
	default:
		return []rpc.DataWarning{{
			Code:     "broker_what_if_unavailable",
			Severity: "warning",
			Message:  "Broker WhatIf did not accept this draft; this preview token cannot bypass the later broker-WhatIf submit gate.",
			Action:   "Use this token only as preview evidence until broker WhatIf returns accepted for the exact draft.",
		}}
	}
}

func previewOrderRef(now time.Time) string {
	tokenID, err := randomTokenID()
	if err != nil {
		return "canary-" + now.UTC().Format("20060102-150405")
	}
	return "canary-" + now.UTC().Format("20060102-150405") + "-" + tokenID[:8]
}

func replaceTargetFromView(view rpc.OrderView) orderPreviewReplaceTarget {
	if view.OrderRef == "" && view.ReservedOrderID == 0 && view.PermID == 0 {
		return orderPreviewReplaceTarget{}
	}
	return orderPreviewReplaceTarget{
		OrderRef:        view.OrderRef,
		ReservedOrderID: view.ReservedOrderID,
		PermID:          view.PermID,
		ClientID:        view.ClientID,
		Account:         view.Account,
		Endpoint:        view.Endpoint,
		Mode:            view.Mode,
		Status:          view.Status,
		LifecycleStatus: view.LifecycleStatus,
		ConID:           view.ConID,
		Symbol:          view.Symbol,
		SecType:         view.SecType,
		Exchange:        view.Exchange,
		PrimaryExch:     view.PrimaryExch,
		Currency:        view.Currency,
		LocalSymbol:     view.LocalSymbol,
		TradingClass:    view.TradingClass,
		Expiry:          view.Expiry,
		Strike:          view.Strike,
		Right:           view.Right,
		Multiplier:      view.Multiplier,
		OrderType:       view.OrderType,
		TIF:             view.TIF,
		TriggerMethod:   view.TriggerMethod,
		Quantity:        view.Quantity,
		Filled:          view.Filled,
		Remaining:       view.Remaining,
		LimitPrice:      view.LimitPrice,
		Trail:           cloneTrailSpec(view.Trail),
		OutsideRTH:      view.OutsideRTH,
	}
}

// previewMinTickTimeout bounds the one-off broker contract-details fetch a
// preview pays on a cold cache. Trail rounding falls back to the static grid
// when it expires; broker WhatIf stays the fail-closed backstop.
const previewMinTickTimeout = 2 * time.Second

// resolveContractMinTick returns the venue minimum price increment for a
// contract: the cached value when known, otherwise one broker
// contract-details fetch per conID, cached for the daemon lifetime (venue
// ticks are static for practical purposes). Returns 0 when unresolved.
//
// Generation and preview must round trail prices on the same grid — the
// proposal-vs-preview drift gate compares them exactly — which is why both
// paths resolve through this single cache.
func (s *Server) resolveContractMinTick(ctx context.Context, contract rpc.ContractParams, timeout time.Duration) float64 {
	if s == nil {
		return 0
	}
	if tick := s.cachedContractMinTick(contract.ConID); tick > 0 {
		return tick
	}
	c := s.gatewayConnector()
	if c == nil || contract.Symbol == "" {
		return 0
	}
	detail, err := c.ContractDetailsFirst(ctx, *previewIBKRContract(contract), timeout)
	if err != nil || detail == nil || detail.MinTick <= 0 {
		return 0
	}
	tick := detail.MinTick
	// IBKR's contract-details MinTick is the minimum across *all* valid
	// venues (midpoint/dark pools report 0.0001 for EUR names) — finer than
	// the lit-venue trading grid. Clamp stocks to the cent floor so
	// resolution can only coarsen rounding, never emit sub-cent prices the
	// venue rejects; broker WhatIf remains the fail-closed arbiter.
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "ETF":
		tick = max(tick, 0.01)
	}
	conID := contract.ConID
	if conID == 0 {
		conID = detail.ConID
	}
	s.storeContractMinTick(conID, tick)
	return tick
}

func (s *Server) cachedContractMinTick(conID int) float64 {
	if s == nil || conID == 0 {
		return 0
	}
	s.minTickMu.Lock()
	defer s.minTickMu.Unlock()
	return s.minTickByConID[conID]
}

// trailRedemptionGuard re-checks a trailing-stop draft against the live
// market at token redemption so a moved market cannot turn a protective
// stop into an immediate trigger.
//
//lint:ignore U1000 used by the trading-tag order redemption path
func (s *Server) trailRedemptionGuard(ctx context.Context, draft rpc.OrderDraft) string {
	if draft.Trail == nil || draft.Trail.InitialStopPrice <= 0 {
		return ""
	}
	quote, err := s.fetchPreviewQuote(ctx, draft.Contract, previewMinTickTimeout)
	if err != nil {
		return ""
	}
	stop := draft.Trail.InitialStopPrice
	if strings.EqualFold(draft.Action, rpc.OrderActionSell) {
		if quote.Bid != nil && *quote.Bid > 0 && stop >= *quote.Bid {
			return fmt.Sprintf("stale trail reference: initial stop %.4f is at/above the current bid %.4f and would trigger immediately; preview again", stop, *quote.Bid)
		}
		return ""
	}
	if quote.Ask != nil && *quote.Ask > 0 && stop <= *quote.Ask {
		return fmt.Sprintf("stale trail reference: initial stop %.4f is at/below the current ask %.4f and would trigger immediately; preview again", stop, *quote.Ask)
	}
	return ""
}

func (s *Server) storeContractMinTick(conID int, tick float64) {
	if s == nil || conID == 0 || tick <= 0 {
		return
	}
	s.minTickMu.Lock()
	defer s.minTickMu.Unlock()
	if s.minTickByConID == nil {
		s.minTickByConID = map[int]float64{}
	}
	s.minTickByConID[conID] = tick
}
