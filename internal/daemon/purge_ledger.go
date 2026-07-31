package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	purgeLedgerKind                = "ibkr.purge_ledger"
	purgeLedgerSchemaVersion       = "purge-ledger-v2"
	legacyPurgeLedgerSchemaVersion = "purge-ledger-v1"

	purgeLedgerStatusActive   = "active"
	purgeLedgerStatusRestored = "restored"

	purgeRestoreSource = "purge_restore"

	purgeLedgerStateScope = "daemon"
	purgeLedgerStateKind  = "purge_ledger_v2"
)

type purgeLedgerStore struct {
	Path      string
	mu        sync.Mutex
	now       func() time.Time
	authority *corestore.Store
}

type purgeLedgerFile struct {
	Kind          string           `json:"kind"`
	SchemaVersion string           `json:"schema_version"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Rows          []purgeLedgerRow `json:"rows"`
}

type purgeLedgerRow struct {
	LegID               string                          `json:"leg_id"`
	PurgeID             string                          `json:"purge_id,omitempty"`
	Symbol              string                          `json:"symbol"`
	SecType             string                          `json:"sec_type"`
	Contract            rpc.ContractParams              `json:"contract"`
	Endpoint            string                          `json:"endpoint,omitempty"`
	ClientID            int                             `json:"client_id,omitempty"`
	Account             string                          `json:"account,omitempty"`
	Mode                string                          `json:"mode,omitempty"`
	Currency            string                          `json:"currency,omitempty"`
	OriginalSide        string                          `json:"original_side"`
	OriginalQuantity    float64                         `json:"original_quantity"`
	PurgeAction         string                          `json:"purge_action"`
	RestoreAction       string                          `json:"restore_action"`
	Multiplier          int                             `json:"multiplier"`
	PurgedQuantity      float64                         `json:"purged_quantity"`
	RestoredQuantity    float64                         `json:"restored_quantity"`
	RemainingQuantity   float64                         `json:"remaining_quantity"`
	PurgeValue          float64                         `json:"purge_value,omitempty"`
	RestoreValue        float64                         `json:"restore_value,omitempty"`
	ShadowPnL           float64                         `json:"shadow_pnl,omitempty"`
	Status              string                          `json:"status"`
	LastPurgeOrderRef   string                          `json:"last_purge_order_ref,omitempty"`
	LastRestoreOrderRef string                          `json:"last_restore_order_ref,omitempty"`
	CreatedAt           time.Time                       `json:"created_at"`
	UpdatedAt           time.Time                       `json:"updated_at"`
	Warnings            []string                        `json:"warnings,omitempty"`
	OrderFills          map[string]purgeLedgerOrderFill `json:"order_fills,omitempty"`
}

type purgeLedgerOrderFill struct {
	Source       string  `json:"source"`
	OrderRef     string  `json:"order_ref"`
	Filled       float64 `json:"filled"`
	AvgFillPrice float64 `json:"avg_fill_price,omitempty"`
}

type legacyPurgeImportParity struct {
	ActiveRows  int
	FillCursors int
}

func defaultPurgeLedgerPath() (string, error) {
	return defaultTradingStatePath("purge-ledger.json")
}

func newPurgeLedgerStore(path string, now func() time.Time) *purgeLedgerStore {
	return &purgeLedgerStore{Path: path, now: now}
}

func (s *purgeLedgerStore) UseCoreStore(store *corestore.Store) error {
	if s == nil || store == nil {
		return fmt.Errorf("purge ledger authority is unavailable")
	}
	if !store.Health().Ready {
		return fmt.Errorf("purge ledger authority is blocked")
	}
	s.mu.Lock()
	s.authority = store
	s.mu.Unlock()
	return nil
}

func (s *purgeLedgerStore) coreStore() (*corestore.Store, error) {
	if s == nil || s.authority == nil {
		return nil, fmt.Errorf("purge ledger authority is unavailable")
	}
	if !s.authority.Health().Ready {
		return nil, fmt.Errorf("purge ledger authority is blocked")
	}
	return s.authority, nil
}

func (s *purgeLedgerStore) Snapshot(scope brokerStateScope, purgeID string) ([]rpc.PurgeLedgerRow, rpc.PurgeLedgerTotals, error) {
	if s == nil {
		return nil, rpc.PurgeLedgerTotals{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ledger, _, err := s.loadLocked()
	if err != nil {
		return nil, rpc.PurgeLedgerTotals{}, err
	}
	rows := make([]rpc.PurgeLedgerRow, 0, len(ledger.Rows))
	for _, row := range ledger.Rows {
		if !purgeLedgerRowMatchesBrokerScope(row, scope) {
			continue
		}
		if purgeID != "" && !strings.EqualFold(row.PurgeID, purgeID) {
			continue
		}
		rows = append(rows, purgeLedgerRowToRPC(row))
	}
	sortPurgeLedgerRows(rows)
	return rows, purgeLedgerTotals(rows), nil
}

func (s *purgeLedgerStore) AllRows() ([]purgeLedgerRow, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ledger, _, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	return slices.Clone(ledger.Rows), nil
}

// CommitOrderLifecycle makes the append-only lifecycle event and the purge
// cumulative-fill cursor one SQLite transaction. A duplicate cumulative fill
// still records its legitimate lifecycle event but carries no state CAS.
func (s *purgeLedgerStore) CommitOrderLifecycle(orderStore *orderJournalStore, ev orderJournalEvent) error {
	if s == nil || orderStore == nil {
		return fmt.Errorf("order lifecycle authority is unavailable")
	}
	return orderStore.withEvidenceMutation(func() error {
		return s.commitOrderLifecycleLockedByJournal(orderStore, ev)
	})
}

func (s *purgeLedgerStore) commitOrderLifecycleLockedByJournal(orderStore *orderJournalStore, ev orderJournalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.coreStore()
	if err != nil {
		return err
	}
	orderAuthority, err := orderStore.coreStore()
	if err != nil {
		return err
	}
	if store != orderAuthority {
		return fmt.Errorf("order and purge state use different authorities")
	}

	for range 3 {
		record, normalized, err := coreOrderEventRecord(ev, "", "")
		if err != nil {
			return err
		}
		ev = normalized
		commit := corestore.LifecycleCommit{Scope: record.Scope, Events: []corestore.OrderEventRecord{record}}
		if ev.Filled > 0 && ev.OrderRef != "" && (ev.Source == purgeExecuteSource || ev.Source == purgeRestoreSource) {
			ledger, revision, err := s.loadLocked()
			if err != nil {
				return err
			}
			if applyPurgeLedgerFill(&ledger, ev, s.currentTime()) {
				ledger.UpdatedAt = s.currentTime()
				raw, err := marshalPurgeLedger(ledger)
				if err != nil {
					return err
				}
				commit.State = &corestore.StateDocumentCAS{
					ScopeKey: purgeLedgerStateScope, Kind: purgeLedgerStateKind,
					ExpectedRevision: revision, JSON: raw,
				}
			}
		}
		if _, err := store.CommitLifecycle(context.Background(), commit); err != nil {
			if errors.Is(err, corestore.ErrRevisionConflict) {
				continue
			}
			return fmt.Errorf("commit authoritative order lifecycle: %w", err)
		}
		return nil
	}
	return fmt.Errorf("purge ledger revision changed repeatedly")
}

func applyPurgeLedgerFill(ledger *purgeLedgerFile, ev orderJournalEvent, now time.Time) bool {
	if ledger == nil {
		return false
	}
	legID := strings.TrimSpace(ev.LegID)
	if legID == "" {
		legID = purgeLegIDForContract(contractParamsFromJournalEvent(ev))
	}
	idx := slices.IndexFunc(ledger.Rows, func(row purgeLedgerRow) bool {
		return row.LegID == legID &&
			strings.TrimSpace(row.Endpoint) == strings.TrimSpace(ev.Endpoint) &&
			row.ClientID == ev.ClientID &&
			strings.EqualFold(row.Account, ev.Account) &&
			strings.EqualFold(row.Mode, ev.Mode)
	})
	if idx < 0 {
		if ev.Source != purgeExecuteSource {
			return false
		}
		ledger.Rows = append(ledger.Rows, purgeLedgerRowFromPurgeFill(ev, legID, now))
		idx = len(ledger.Rows) - 1
	}
	row := &ledger.Rows[idx]
	if row.OrderFills == nil {
		row.OrderFills = map[string]purgeLedgerOrderFill{}
	}
	prior := row.OrderFills[ev.OrderRef]
	if ev.Filled <= prior.Filled+1e-9 {
		return false
	}
	delta := ev.Filled - prior.Filled
	price := purgeLedgerDeltaPrice(ev, prior, delta)
	if price <= 0 {
		row.Warnings = appendPurgeLedgerUnique(row.Warnings, "fill callback did not include a usable fill price")
	}
	row.OrderFills[ev.OrderRef] = purgeLedgerOrderFill{
		Source:       ev.Source,
		OrderRef:     ev.OrderRef,
		Filled:       ev.Filled,
		AvgFillPrice: purgeLedgerEventAvgPrice(ev, price),
	}
	switch ev.Source {
	case purgeExecuteSource:
		applyPurgeFillToLedgerRow(row, ev, delta, price, now)
	case purgeRestoreSource:
		applyRestoreFillToLedgerRow(row, ev, delta, price, now)
	}
	normalizePurgeLedgerRow(row)
	return true
}

func purgeLedgerRowFromPurgeFill(ev orderJournalEvent, legID string, now time.Time) purgeLedgerRow {
	contract := contractParamsFromJournalEvent(ev)
	originalSide := purgeOriginalSideLong
	restoreAction := rpc.OrderActionBuy
	originalQty := math.Abs(ev.Quantity)
	if strings.EqualFold(ev.Action, rpc.OrderActionBuy) {
		originalSide = purgeOriginalSideShort
		restoreAction = rpc.OrderActionSell
		originalQty = -math.Abs(ev.Quantity)
	}
	return purgeLedgerRow{
		LegID:             legID,
		PurgeID:           ev.PurgeID,
		Symbol:            contract.Symbol,
		SecType:           contract.SecType,
		Contract:          contract,
		Endpoint:          strings.TrimSpace(ev.Endpoint),
		ClientID:          ev.ClientID,
		Account:           ev.Account,
		Mode:              ev.Mode,
		Currency:          contract.Currency,
		OriginalSide:      originalSide,
		OriginalQuantity:  originalQty,
		PurgeAction:       strings.ToUpper(strings.TrimSpace(ev.Action)),
		RestoreAction:     restoreAction,
		Multiplier:        contractMultiplier(contract),
		Status:            purgeLedgerStatusActive,
		CreatedAt:         now,
		UpdatedAt:         now,
		LastPurgeOrderRef: ev.OrderRef,
	}
}

func applyPurgeFillToLedgerRow(row *purgeLedgerRow, ev orderJournalEvent, delta, price float64, now time.Time) {
	if ev.PurgeID != "" {
		row.PurgeID = ev.PurgeID
	}
	row.LastPurgeOrderRef = ev.OrderRef
	row.PurgedQuantity += delta
	if price > 0 {
		row.PurgeValue += delta * price * float64(max(row.Multiplier, 1))
	}
	row.UpdatedAt = now
}

func applyRestoreFillToLedgerRow(row *purgeLedgerRow, ev orderJournalEvent, delta, price float64, now time.Time) {
	row.LastRestoreOrderRef = ev.OrderRef
	applied := min(delta, max(row.PurgedQuantity-row.RestoredQuantity, 0))
	row.RestoredQuantity += applied
	if price > 0 {
		row.RestoreValue += applied * price * float64(max(row.Multiplier, 1))
	}
	row.UpdatedAt = now
}

func normalizePurgeLedgerRow(row *purgeLedgerRow) {
	if row == nil {
		return
	}
	if row.Multiplier <= 0 {
		row.Multiplier = contractMultiplier(row.Contract)
	}
	row.RemainingQuantity = max(row.PurgedQuantity-row.RestoredQuantity, 0)
	if row.RemainingQuantity <= 1e-9 {
		row.RemainingQuantity = 0
		row.Status = purgeLedgerStatusRestored
	} else {
		row.Status = purgeLedgerStatusActive
	}
	row.ShadowPnL = purgeLedgerShadowPnL(*row)
}

func purgeLedgerShadowPnL(row purgeLedgerRow) float64 {
	if row.PurgedQuantity <= 0 || row.RestoredQuantity <= 0 || row.PurgeValue <= 0 || row.RestoreValue <= 0 {
		return 0
	}
	multiplier := float64(max(row.Multiplier, 1))
	purgeAvg := row.PurgeValue / (row.PurgedQuantity * multiplier)
	restoreAvg := row.RestoreValue / (row.RestoredQuantity * multiplier)
	sign := 1.0
	if row.OriginalQuantity < 0 || strings.EqualFold(row.OriginalSide, purgeOriginalSideShort) {
		sign = -1
	}
	return (purgeAvg - restoreAvg) * row.RestoredQuantity * multiplier * sign
}

func purgeLedgerDeltaPrice(ev orderJournalEvent, prior purgeLedgerOrderFill, delta float64) float64 {
	if delta <= 0 {
		return 0
	}
	if ev.AvgFillPrice > 0 {
		priorValue := prior.Filled * prior.AvgFillPrice
		nextValue := ev.Filled * ev.AvgFillPrice
		if nextValue > priorValue {
			return (nextValue - priorValue) / delta
		}
		return ev.AvgFillPrice
	}
	if ev.LastFillPrice > 0 {
		return ev.LastFillPrice
	}
	return 0
}

func purgeLedgerEventAvgPrice(ev orderJournalEvent, fallback float64) float64 {
	if ev.AvgFillPrice > 0 {
		return ev.AvgFillPrice
	}
	return fallback
}

func contractParamsFromJournalEvent(ev orderJournalEvent) rpc.ContractParams {
	contract := rpc.ContractParams{
		ConID:        ev.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(ev.Symbol)),
		SecType:      strings.ToUpper(strings.TrimSpace(ev.SecType)),
		Exchange:     strings.TrimSpace(ev.Exchange),
		PrimaryExch:  strings.TrimSpace(ev.PrimaryExch),
		Currency:     strings.ToUpper(strings.TrimSpace(ev.Currency)),
		LocalSymbol:  strings.TrimSpace(ev.LocalSymbol),
		TradingClass: strings.TrimSpace(ev.TradingClass),
		Expiry:       strings.TrimSpace(ev.Expiry),
		Strike:       ev.Strike,
		Right:        strings.ToUpper(strings.TrimSpace(ev.Right)),
		Multiplier:   ev.Multiplier,
	}
	if contract.SecType == "" {
		contract.SecType = "STK"
	}
	if contract.Currency == "" {
		contract.Currency = "USD"
	}
	if contract.Exchange == "" {
		contract.Exchange = "SMART"
	}
	if contract.Multiplier == 0 {
		contract.Multiplier = contractMultiplier(contract)
	}
	return contract
}

func purgeLedgerRowToRPC(row purgeLedgerRow) rpc.PurgeLedgerRow {
	normalizePurgeLedgerRow(&row)
	multiplier := float64(max(row.Multiplier, 1))
	var purgeAvg, restoreAvg float64
	if row.PurgedQuantity > 0 && row.PurgeValue > 0 {
		purgeAvg = row.PurgeValue / (row.PurgedQuantity * multiplier)
	}
	if row.RestoredQuantity > 0 && row.RestoreValue > 0 {
		restoreAvg = row.RestoreValue / (row.RestoredQuantity * multiplier)
	}
	return rpc.PurgeLedgerRow{
		LegID:               row.LegID,
		PurgeID:             row.PurgeID,
		Symbol:              row.Symbol,
		SecType:             row.SecType,
		Contract:            row.Contract,
		Account:             row.Account,
		Mode:                row.Mode,
		Currency:            row.Currency,
		OriginalSide:        row.OriginalSide,
		OriginalQuantity:    row.OriginalQuantity,
		PurgeAction:         row.PurgeAction,
		RestoreAction:       row.RestoreAction,
		Multiplier:          row.Multiplier,
		PurgedQuantity:      row.PurgedQuantity,
		RestoredQuantity:    row.RestoredQuantity,
		RemainingQuantity:   row.RemainingQuantity,
		PurgeAvgPrice:       purgeAvg,
		RestoreAvgPrice:     restoreAvg,
		PurgeValue:          row.PurgeValue,
		RestoreValue:        row.RestoreValue,
		ShadowPnL:           row.ShadowPnL,
		Status:              row.Status,
		LastPurgeOrderRef:   row.LastPurgeOrderRef,
		LastRestoreOrderRef: row.LastRestoreOrderRef,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
		Warnings:            slices.Clone(row.Warnings),
	}
}

func purgeLedgerTotals(rows []rpc.PurgeLedgerRow) rpc.PurgeLedgerTotals {
	var totals rpc.PurgeLedgerTotals
	for _, row := range rows {
		if row.RemainingQuantity > 0 {
			totals.ActiveRows++
		} else {
			totals.RestoredRows++
		}
		totals.PurgedQuantity += row.PurgedQuantity
		totals.RestoredQuantity += row.RestoredQuantity
		totals.RemainingQuantity += row.RemainingQuantity
		totals.PurgeValue += row.PurgeValue
		totals.RestoreValue += row.RestoreValue
		totals.ShadowPnL += row.ShadowPnL
	}
	return totals
}

func sortPurgeLedgerRows(rows []rpc.PurgeLedgerRow) {
	slices.SortStableFunc(rows, func(a, b rpc.PurgeLedgerRow) int {
		if a.RemainingQuantity > 0 && b.RemainingQuantity <= 0 {
			return -1
		}
		if a.RemainingQuantity <= 0 && b.RemainingQuantity > 0 {
			return 1
		}
		if c := strings.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		if c := strings.Compare(a.Contract.SecType, b.Contract.SecType); c != 0 {
			return c
		}
		if c := strings.Compare(a.Contract.Expiry, b.Contract.Expiry); c != 0 {
			return c
		}
		if a.Contract.Strike < b.Contract.Strike {
			return -1
		}
		if a.Contract.Strike > b.Contract.Strike {
			return 1
		}
		if c := strings.Compare(a.Contract.Right, b.Contract.Right); c != 0 {
			return c
		}
		return strings.Compare(a.LegID, b.LegID)
	})
}

// loadLegacyPurgeImportSelection keeps only active restore authority and every
// cumulative per-order fill cursor belonging to those rows. The one historical
// v1 schema is accepted because its rows are a strict subset of v2: route
// identity was not stored on the ledger and is therefore re-derived from the
// complete legacy order evidence below. Unknown schema mismatches fail cutover;
// they are never reset or deleted.
func loadLegacyPurgeImportSelection(path string, orders legacyOrderImportSelection) (purgeLedgerFile, error) {
	empty := purgeLedgerFile{Kind: purgeLedgerKind, SchemaVersion: purgeLedgerSchemaVersion}
	if strings.TrimSpace(path) == "" {
		return purgeLedgerFile{}, fmt.Errorf("legacy purge ledger path is empty")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return empty, nil
		}
		return purgeLedgerFile{}, fmt.Errorf("read legacy purge ledger: %w", err)
	}
	var legacy purgeLedgerFile
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return purgeLedgerFile{}, fmt.Errorf("decode legacy purge ledger: %w", err)
	}
	if legacy.Kind != purgeLedgerKind {
		return purgeLedgerFile{}, fmt.Errorf("purge ledger kind is %q, want %q", legacy.Kind, purgeLedgerKind)
	}
	switch legacy.SchemaVersion {
	case legacyPurgeLedgerSchemaVersion, purgeLedgerSchemaVersion:
		// Both schemas carry the same restore quantities and cumulative fill
		// cursors. v1 omitted broker-route fields; every retained row is bound
		// from the order journal below before it becomes current v2 authority.
	default:
		return purgeLedgerFile{}, fmt.Errorf(
			"purge ledger schema is %q, want %q or %q",
			legacy.SchemaVersion,
			legacyPurgeLedgerSchemaVersion,
			purgeLedgerSchemaVersion,
		)
	}

	routesByRef := map[string]legacyOrderRoute{}
	ambiguousRef := map[string]bool{}
	for _, ev := range orders.SourceEvents {
		if ev.OrderRef == "" {
			continue
		}
		route := legacyOrderRouteFromEvent(ev)
		if !route.complete() {
			continue
		}
		if prior, ok := routesByRef[ev.OrderRef]; ok && prior.key() != route.key() {
			ambiguousRef[ev.OrderRef] = true
			continue
		}
		routesByRef[ev.OrderRef] = route
	}
	out := empty
	out.UpdatedAt = legacy.UpdatedAt
	for _, row := range legacy.Rows {
		normalizePurgeLedgerRow(&row)
		if row.Status != purgeLedgerStatusActive || row.RemainingQuantity <= 0 {
			continue
		}
		refs := []string{row.LastPurgeOrderRef, row.LastRestoreOrderRef}
		for ref := range row.OrderFills {
			refs = append(refs, ref)
		}
		var route legacyOrderRoute
		for _, ref := range refs {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			if ambiguousRef[ref] {
				return purgeLedgerFile{}, fmt.Errorf("active purge row %q references order %q in multiple broker routes", row.LegID, ref)
			}
			candidate, ok := routesByRef[ref]
			if !ok {
				continue
			}
			if route.complete() && route.key() != candidate.key() {
				return purgeLedgerFile{}, fmt.Errorf("active purge row %q spans multiple broker routes", row.LegID)
			}
			route = candidate
		}
		if !route.complete() {
			return purgeLedgerFile{}, fmt.Errorf("active purge row %q cannot be bound to endpoint/client/account/mode", row.LegID)
		}
		if row.Account != "" && !strings.EqualFold(row.Account, route.Account) {
			return purgeLedgerFile{}, fmt.Errorf("active purge row %q account conflicts with order evidence", row.LegID)
		}
		if row.Mode != "" && !strings.EqualFold(row.Mode, route.Mode) {
			return purgeLedgerFile{}, fmt.Errorf("active purge row %q mode conflicts with order evidence", row.LegID)
		}
		row.Endpoint, row.ClientID, row.Account, row.Mode = route.Endpoint, route.ClientID, route.Account, route.Mode
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

func importLegacyPurgeAuthority(ctx context.Context, store *corestore.Store, path string, orders legacyOrderImportSelection) (legacyPurgeImportParity, error) {
	var parity legacyPurgeImportParity
	if store == nil {
		return parity, fmt.Errorf("purge authority is unavailable")
	}
	ledger, err := loadLegacyPurgeImportSelection(path, orders)
	if err != nil {
		return parity, err
	}
	if ledger.UpdatedAt.IsZero() {
		// Keep the first-cutover document deterministic so a restart can
		// idempotently verify the same legacy selection. Missing legacy purge
		// state uses the newest order-source timestamp, or Unix epoch when the
		// entire legacy authority is empty.
		ledger.UpdatedAt = time.Unix(0, 0).UTC()
		for _, ev := range orders.SourceEvents {
			if ev.At.After(ledger.UpdatedAt) {
				ledger.UpdatedAt = ev.At.UTC()
			}
		}
	}
	raw, err := marshalPurgeLedger(ledger)
	if err != nil {
		return parity, err
	}
	_, err = store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: purgeLedgerStateScope, Kind: purgeLedgerStateKind, ExpectedRevision: 0, JSON: raw,
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		doc, ok, readErr := store.GetStateDocument(ctx, purgeLedgerStateScope, purgeLedgerStateKind)
		if readErr != nil {
			return parity, fmt.Errorf("read existing purge import: %w", readErr)
		}
		if !ok || !jsonEqual(doc.JSON, raw) {
			return parity, fmt.Errorf("existing purge authority conflicts with legacy import")
		}
		err = nil
	}
	if err != nil {
		return parity, fmt.Errorf("import legacy purge authority: %w", err)
	}
	doc, ok, err := store.GetStateDocument(ctx, purgeLedgerStateScope, purgeLedgerStateKind)
	if err != nil || !ok {
		return parity, fmt.Errorf("verify legacy purge authority: found=%v error=%w", ok, err)
	}
	var got purgeLedgerFile
	if err := json.Unmarshal(doc.JSON, &got); err != nil {
		return parity, fmt.Errorf("verify legacy purge authority payload: %w", err)
	}
	if !jsonEqual(doc.JSON, raw) {
		return parity, fmt.Errorf("legacy purge row and fill-cursor parity failed")
	}
	parity.ActiveRows = len(ledger.Rows)
	for _, row := range ledger.Rows {
		parity.FillCursors += len(row.OrderFills)
	}
	return parity, nil
}

func jsonEqual(a, b []byte) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

func (s *purgeLedgerStore) loadLocked() (purgeLedgerFile, int64, error) {
	store, err := s.coreStore()
	if err != nil {
		return purgeLedgerFile{}, 0, err
	}
	doc, ok, err := store.GetStateDocument(context.Background(), purgeLedgerStateScope, purgeLedgerStateKind)
	if err != nil {
		return purgeLedgerFile{}, 0, fmt.Errorf("read authoritative purge ledger: %w", err)
	}
	if !ok {
		return purgeLedgerFile{
			Kind: purgeLedgerKind, SchemaVersion: purgeLedgerSchemaVersion, UpdatedAt: s.currentTime(),
		}, 0, nil
	}
	var ledger purgeLedgerFile
	if err := json.Unmarshal(doc.JSON, &ledger); err != nil {
		return purgeLedgerFile{}, 0, fmt.Errorf("decode authoritative purge ledger: %w", err)
	}
	if ledger.Kind != purgeLedgerKind || ledger.SchemaVersion != purgeLedgerSchemaVersion {
		return purgeLedgerFile{}, 0, fmt.Errorf("purge ledger is %q/%q, want %q/%q", ledger.Kind, ledger.SchemaVersion, purgeLedgerKind, purgeLedgerSchemaVersion)
	}
	return ledger, doc.Revision, nil
}

func marshalPurgeLedger(ledger purgeLedgerFile) ([]byte, error) {
	ledger.Kind = purgeLedgerKind
	ledger.SchemaVersion = purgeLedgerSchemaVersion
	raw, err := json.Marshal(ledger)
	if err != nil {
		return nil, fmt.Errorf("marshal purge ledger: %w", err)
	}
	return raw, nil
}

func (s *purgeLedgerStore) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func appendPurgeLedgerUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}
