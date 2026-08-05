package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	marketEventFeeRateScope           = "market/events/borrow-fees-tws"
	marketEventFeeRateStateKind       = "borrow_fee_tws.current.v1"
	marketEventFeeRateObservationKind = "borrow_fee_tws.fetch_outcome.v1"
	marketEventFeeRateSource          = "ibkr.tws.historical_fee_rate"
)

type marketEventFeeRateObservation struct {
	StateKey string
	Attempt  marketEventFeeRateAttempt
	Record   *marketEventFeeRateRecord
}

func loadMarketEventFeeRateState(store *corestore.Store, now time.Time) (marketEventFeeRateState, int64, error) {
	state := newMarketEventFeeRateState()
	doc, ok, err := store.GetStateDocument(context.Background(), marketEventFeeRateScope, marketEventFeeRateStateKind)
	if err != nil || !ok {
		return state, 0, err
	}
	if err := decodeStrictMarketEventJSON(doc.JSON, &state); err != nil {
		return marketEventFeeRateState{}, 0, fmt.Errorf("decode TWS borrow-fee authority: %w", err)
	}
	if err := validateMarketEventFeeRateState(state, now); err != nil {
		return marketEventFeeRateState{}, 0, fmt.Errorf("validate TWS borrow-fee authority: %w", err)
	}
	return cloneMarketEventFeeRateState(state), doc.Revision, nil
}

func (c *marketEventCache) persistMarketEventFeeRateState(
	ctx context.Context,
	state marketEventFeeRateState,
	revision int64,
	observations []marketEventFeeRateObservation,
) (int64, error) {
	now := c.now().UTC()
	if err := validateMarketEventFeeRateState(state, now); err != nil {
		return 0, err
	}
	c.mu.Lock()
	store := c.authority
	currentRevision := c.borrowFeeFallbackRevision
	c.mu.Unlock()
	if currentRevision != revision {
		return 0, corestore.ErrRevisionConflict
	}
	if store == nil {
		return revision, nil
	}
	_ = observations
	statePayload, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	saved, err := store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: marketEventFeeRateScope, Kind: marketEventFeeRateStateKind,
		ExpectedRevision: revision, JSON: statePayload,
	})
	if err != nil {
		return 0, err
	}
	return saved.Revision, nil
}

func validateMarketEventFeeRateState(state marketEventFeeRateState, now time.Time) error {
	if state.Version != marketEventFeeRateStateVersion || state.LastGood == nil || state.LastAttempts == nil {
		return errors.New("invalid TWS borrow-fee envelope")
	}
	now = now.UTC()
	for key, record := range state.LastGood {
		if err := validateMarketEventFeeRateRecord(key, record, now); err != nil {
			return err
		}
		if _, ok := state.LastAttempts[key]; !ok {
			return fmt.Errorf("TWS borrow-fee record without attempt %q", key)
		}
	}
	for key, attempt := range state.LastAttempts {
		if !validFeeRateStateKey(key) || attempt.AttemptedAt.IsZero() || attempt.CompletedAt.IsZero() || attempt.CompletedAt.Before(attempt.AttemptedAt) || attempt.CompletedAt.After(now) {
			return fmt.Errorf("invalid TWS borrow-fee attempt %q", key)
		}
		if attempt.RuntimeNextAttempt != nil && (attempt.NextAttempt == nil || attempt.RuntimeNextAttempt.Before(*attempt.NextAttempt)) {
			return fmt.Errorf("invalid runtime TWS borrow-fee boundary %q", key)
		}
		switch attempt.Outcome {
		case marketEventFeeRateOutcomeSuccess:
			record, ok := state.LastGood[key]
			if !ok || record.ObservedAt != attempt.CompletedAt || attempt.Failure != nil || attempt.RuntimeNextAttempt != nil || attempt.NextAttempt == nil || !attempt.NextAttempt.After(attempt.CompletedAt) {
				return fmt.Errorf("invalid successful TWS borrow-fee attempt %q", key)
			}
		case marketEventFeeRateOutcomeFailure:
			if attempt.NextAttempt == nil || !attempt.NextAttempt.After(attempt.CompletedAt) ||
				(attempt.Failure != nil && (!validMarketEventFeeRateFailure(attempt.Failure) || attempt.Failure.FailedAt != attempt.CompletedAt)) {
				return fmt.Errorf("invalid failed TWS borrow-fee attempt %q", key)
			}
			if record, ok := state.LastGood[key]; ok && attempt.CompletedAt.Before(record.ObservedAt) {
				return fmt.Errorf("rolled-back TWS borrow-fee attempt %q", key)
			}
		default:
			return fmt.Errorf("invalid TWS borrow-fee outcome %q", attempt.Outcome)
		}
	}
	return nil
}

func validateMarketEventFeeRateRecord(key string, record marketEventFeeRateRecord, now time.Time) error {
	contract := ibkrlib.Contract{
		ConID: record.Contract.ConID, Symbol: record.Contract.Symbol, SecType: record.Contract.SecType,
		Expiry: record.Contract.Expiry, Strike: record.Contract.Strike, Right: record.Contract.Right, Multiplier: record.Contract.Multiplier,
		Exchange: record.Contract.Exchange, PrimaryExch: record.Contract.PrimaryExch,
		Currency: record.Contract.Currency, LocalSymbol: record.Contract.LocalSymbol,
		TradingClass: record.Contract.TradingClass, SecIDType: record.Contract.SecIDType, SecID: record.Contract.SecID,
	}
	normalized := normalizeFeeRateContract(contract)
	if !validSHA256Fingerprint(record.ScopeFingerprint) || !validSHA256Fingerprint(record.IdentityFingerprint) || !validSHA256Fingerprint(record.ContractFingerprint) ||
		key != record.ScopeFingerprint+":"+record.IdentityFingerprint ||
		record.IdentityFingerprint != marketEventFeeRateIdentityFingerprint(normalized) ||
		record.ContractFingerprint != marketEventFeeRateContractFingerprint(normalized) ||
		record.Contract != feeRateStoredContract(normalized) || normalized.ConID <= 0 || normalized.SecType != "STK" || normalized.Symbol == "" || normalized.Exchange == "" || normalized.Currency == "" ||
		!ibkrlib.HistoricalFeeRateUSRouteSupported(normalized, true) ||
		math.IsNaN(normalized.Strike) || math.IsInf(normalized.Strike, 0) {
		return fmt.Errorf("invalid TWS borrow-fee identity %q", key)
	}
	if record.SessionDate == "" || record.AsOf.IsZero() || record.ObservedAt.IsZero() || record.AsOf.After(record.ObservedAt) || record.ObservedAt.After(now) ||
		math.IsNaN(record.FeeRate) || math.IsInf(record.FeeRate, 0) || record.FeeRate < 0 || record.ScaleStatus != rpc.BorrowFeeScaleUnverified {
		return fmt.Errorf("invalid TWS borrow-fee record %q", key)
	}
	if _, err := time.Parse("2006-01-02", record.SessionDate); err != nil {
		return fmt.Errorf("invalid TWS borrow-fee session %q", key)
	}
	expectedClose := marketCloseForDate(marketcal.MarketUSEquity, record.SessionDate, record.ObservedAt)
	if expectedClose.IsZero() || !record.AsOf.Equal(expectedClose.UTC()) {
		return fmt.Errorf("invalid TWS borrow-fee session close %q", key)
	}
	return nil
}

func validMarketEventFeeRateFailure(failure *rpc.SourceFailure) bool {
	if !rpc.ValidSourceFailure(failure) {
		return false
	}
	switch failure.Code {
	case rpc.SourceFailureInvalidPayload:
		return failure.Stage == rpc.SourceFailureStageHistoricalFeeDecode
	case rpc.SourceFailureNoData:
		return failure.Stage == rpc.SourceFailureStageHistoricalFeeRequest || failure.Stage == rpc.SourceFailureStageHistoricalFeeDecode
	case rpc.SourceFailureTimeout, rpc.SourceFailureTransportFailed, rpc.SourceFailureProtocolRejected,
		rpc.SourceFailureNotEntitled, rpc.SourceFailurePacing, rpc.SourceFailureGatewayUnavailable,
		rpc.SourceFailureContractUnavailable:
		return failure.Stage == rpc.SourceFailureStageHistoricalFeeRequest
	default:
		return false
	}
}

func validFeeRateStateKey(key string) bool {
	parts := strings.Split(key, ":")
	return len(parts) == 4 && parts[0] == "sha256" && parts[2] == "sha256" && len(parts[1]) == 64 && len(parts[3]) == 64 && validSHA256Fingerprint(parts[0]+":"+parts[1]) && validSHA256Fingerprint(parts[2]+":"+parts[3])
}

func validSHA256Fingerprint(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
