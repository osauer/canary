package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
)

// cashSweepFeeRatesKind is the daemon.db state document that keeps the sweep's
// fee evidence across restarts.
const cashSweepFeeRatesKind = "cash_sweep_fee_rates"

// cashSweepFeeRate is the broker fee per unit of face one accepted WhatIf of
// a sweep buy implied, and when it was observed.
type cashSweepFeeRate struct {
	PerFace    float64   `json:"fee_per_face"`
	ObservedAt time.Time `json:"observed_at"`
}

func (r cashSweepFeeRate) plausible() bool {
	return r.PerFace >= 0 && !math.IsInf(r.PerFace, 0) && !math.IsNaN(r.PerFace) && !r.ObservedAt.IsZero()
}

// cashSweepFeeMemory keeps, per currency and bill instrument, the broker fee
// per unit of face from the last accepted WhatIf of a sweep buy, so the next
// cycle's bill selection weighs the fee before any preview (owner decision
// 2026-10-23 16:30 CEST). The rates are evidence kept in daemon.db, so a
// restart remembers them and a sweep that loses after the fee stays unlisted;
// every preview still checks the exact fee. With no rate observed, selection
// ranks without a fee and the preview's floor blocks a loss.
type cashSweepFeeMemory struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	rates    map[string]cashSweepFeeRate
}

func (m *cashSweepFeeMemory) snapshot() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(m.rates))
	for k, r := range m.rates {
		out[k] = r.PerFace
	}
	return out
}

// bindCore loads the stored rates; an implausible stored rate is dropped.
func (m *cashSweepFeeMemory) bindCore(ctx context.Context, core *corestore.Store) error {
	if core == nil {
		return errors.New("sweep fee store is not attached")
	}
	rates, revision, err := loadCashSweepFeeRates(ctx, core)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.core, m.revision, m.rates = core, revision, rates
	m.mu.Unlock()
	return nil
}

func loadCashSweepFeeRates(ctx context.Context, core *corestore.Store) (map[string]cashSweepFeeRate, int64, error) {
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, cashSweepFeeRatesKind)
	if err != nil || !ok {
		return map[string]cashSweepFeeRate{}, 0, err
	}
	var stored map[string]cashSweepFeeRate
	if err := json.Unmarshal(doc.JSON, &stored); err != nil {
		return nil, 0, fmt.Errorf("decode sweep fee rates: %w", err)
	}
	rates := map[string]cashSweepFeeRate{}
	for k, r := range stored {
		if r.plausible() {
			rates[k] = r
		}
	}
	return rates, doc.Revision, nil
}

// note records one rate in memory and, once bound, in daemon.db. An
// implausible rate is never kept; a stored rate observed later than this
// process's wins the merge on a concurrent write.
func (m *cashSweepFeeMemory) note(ctx context.Context, key string, rate cashSweepFeeRate) error {
	if !rate.plausible() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rates == nil {
		m.rates = map[string]cashSweepFeeRate{}
	}
	m.rates[key] = rate
	if m.core == nil {
		return nil
	}
	for range 3 {
		raw, err := json.Marshal(m.rates)
		if err != nil {
			return err
		}
		doc, err := m.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: cashSweepFeeRatesKind, ExpectedRevision: m.revision, JSON: raw})
		if err == nil {
			m.revision = doc.Revision
			return nil
		}
		if _, conflict := errors.AsType[*corestore.RevisionConflictError](err); !conflict {
			return err
		}
		stored, revision, err := loadCashSweepFeeRates(ctx, m.core)
		if err != nil {
			return err
		}
		for k, r := range stored {
			if mine, ok := m.rates[k]; !ok || r.ObservedAt.After(mine.ObservedAt) {
				m.rates[k] = r
			}
		}
		m.revision = revision
	}
	return errors.New("sweep fee rates kept changing during the write")
}

// noteSweepFee records a fresh invest preview's fee bound per unit of face.
func (e *proposalEngine) noteSweepFee(ctx context.Context, prop rpc.TradeProposal, preview *rpc.OrderPreviewResult, now time.Time) {
	s := prop.CashSweep
	if e == nil || s == nil || s.Side != rpc.CashSweepSideInvest || preview == nil || preview.Draft.Bond == nil {
		return
	}
	fee, ok := cashSweepFeeUpper(preview, s.Currency)
	face := float64(preview.Draft.Quantity) * preview.Draft.Bond.FacePerUnit
	if !ok || !positiveFinite(face) {
		return
	}
	err := e.sweepFees.note(ctx, cashSweepBillUnitKey(s.Currency, s.Instrument), cashSweepFeeRate{PerFace: fee / face, ObservedAt: now.UTC()})
	if err != nil && e.server != nil && e.server.logger != nil {
		e.server.logger.Warnf("cash sweep fee rate kept in memory only: %v", err)
	}
}
