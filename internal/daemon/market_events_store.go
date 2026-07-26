package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const marketEventStateVersion = 1
const marketEventBorrowFeesStateVersion = 2

const (
	marketEventRegSHOScope                    = "market/events/reg-sho"
	marketEventRegSHOStateKind                = "reg_sho.current.v1"
	marketEventRegSHOObservationKind          = "reg_sho.snapshot.v1"
	marketEventRegSHOSource                   = "nasdaq.reg_sho_threshold"
	marketEventHaltsScope                     = "market/events/halts"
	marketEventHaltsStateKind                 = "trading_halts.current.v1"
	marketEventHaltsObservationKind           = "trading_halts.snapshot.v1"
	marketEventHaltsSource                    = "nasdaq.trade_halts"
	marketEventBorrowFeesScope                = "market/events/borrow-fees"
	marketEventBorrowFeesStateKind            = "borrow_fees.current.v1"
	marketEventBorrowFeesObservationKind      = "borrow_fees.fetch_outcome.v2"
	marketEventBorrowFeesSource               = "ibkr.short_stock_availability"
	marketEventBorrowInventoryScope           = "market/events/borrow-inventory"
	marketEventBorrowInventoryStateKind       = "borrow_inventory.current.v1"
	marketEventBorrowInventoryObservationKind = "borrow_inventory.snapshot.v1"
	marketEventBorrowInventorySource          = "ibkr.tws.generic_tick_236"
)

type marketEventRegSHOState struct {
	Version int                    `json:"version"`
	Entry   marketEventRegSHOEntry `json:"entry"`
}

type marketEventHaltsState struct {
	Version int                   `json:"version"`
	Entry   marketEventHaltsEntry `json:"entry"`
}

type marketEventBorrowFeesStateV1 struct {
	Version int                       `json:"version"`
	Entry   marketEventBorrowFeeEntry `json:"entry"`
}

type marketEventBorrowFeesState struct {
	Version     int                          `json:"version"`
	LastGood    *marketEventBorrowFeeEntry   `json:"last_good,omitempty"`
	LastAttempt *marketEventBorrowFeeAttempt `json:"last_attempt,omitempty"`
}

type marketEventBorrowFeeAttempt struct {
	Outcome     string             `json:"outcome"`
	AttemptedAt time.Time          `json:"attempted_at"`
	CompletedAt time.Time          `json:"completed_at"`
	NextAttempt *time.Time         `json:"next_attempt,omitempty"`
	Failure     *rpc.SourceFailure `json:"failure,omitempty"`
}

type marketEventBorrowFeeOutcome struct {
	Version int                         `json:"version"`
	Attempt marketEventBorrowFeeAttempt `json:"attempt"`
	Entry   *marketEventBorrowFeeEntry  `json:"entry,omitempty"`
}

const (
	marketEventBorrowFeeOutcomeSuccess = "success"
	marketEventBorrowFeeOutcomeFailure = "failure"
)

type marketEventBorrowInventoryState struct {
	Version    int                                         `json:"version"`
	ObservedAt time.Time                                   `json:"observed_at"`
	Records    map[string]marketEventBorrowInventoryRecord `json:"records"`
}

type marketEventBorrowInventoryRecord struct {
	Symbol          string    `json:"symbol"`
	ShortableShares int64     `json:"shortable_shares"`
	AsOf            time.Time `json:"as_of"`
	DataType        string    `json:"data_type,omitempty"`
	Delayed         bool      `json:"delayed,omitempty"`
}

// UseCoreStore atomically swaps the cache projection to daemon.db. Every
// existing document is decoded and validated before any in-memory value or
// authority pointer changes. Borrow-fee retry/failure state is durable source
// evidence; only the other sources' retry state and negative-absence cache are
// intentionally reset.
func (c *marketEventCache) UseCoreStore(store *corestore.Store) error {
	if c == nil {
		return errors.New("market events: nil cache")
	}
	if store == nil {
		return errors.New("market events: nil corestore")
	}
	regSHO, err := loadMarketEventRegSHO(store)
	if err != nil {
		return err
	}
	halts, err := loadMarketEventHalts(store)
	if err != nil {
		return err
	}
	if err := validateStoredBorrowInventory(store); err != nil {
		return err
	}
	borrowFees, borrowFeesRevision, err := loadMarketEventBorrowFees(store)
	if err != nil {
		return err
	}
	loadedAt := c.now().UTC()
	feeRates, feeRateRevision, err := loadMarketEventFeeRateState(store, loadedAt)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.authority = store
	c.regSHO = regSHO
	c.halts = halts
	c.borrowFees = marketEventBorrowFeeLastGood(borrowFees)
	c.borrowFeesLastAttempt = cloneBorrowFeeAttempt(borrowFees.LastAttempt)
	c.borrowFeesRevision = borrowFeesRevision
	c.borrowFeeFallback = feeRates
	c.borrowFeeFallbackRevision = feeRateRevision
	c.borrowFeeFallbackLoadedAt = loadedAt
	c.borrowFeeFallbackCurrent = map[string]ibkrlib.HistoricalSessionBinding{}
	c.shortableAbsent = nil
	c.regSHOFailedAt = time.Time{}
	c.haltsFailedAt = time.Time{}
	c.mu.Unlock()
	return nil
}

func loadMarketEventRegSHO(store *corestore.Store) (marketEventRegSHOEntry, error) {
	raw, ok, err := loadMarketState(store, marketEventRegSHOScope, marketEventRegSHOStateKind)
	if err != nil || !ok {
		return marketEventRegSHOEntry{}, err
	}
	var state marketEventRegSHOState
	if err := json.Unmarshal(raw, &state); err != nil {
		return marketEventRegSHOEntry{}, fmt.Errorf("decode Reg SHO authority: %w", err)
	}
	if state.Version != marketEventStateVersion {
		return marketEventRegSHOEntry{}, fmt.Errorf("invalid Reg SHO authority version %d", state.Version)
	}
	if err := validateRegSHOEntry(state.Entry); err != nil {
		return marketEventRegSHOEntry{}, fmt.Errorf("validate Reg SHO authority: %w", err)
	}
	return cloneRegSHOEntry(state.Entry), nil
}

func loadMarketEventHalts(store *corestore.Store) (marketEventHaltsEntry, error) {
	raw, ok, err := loadMarketState(store, marketEventHaltsScope, marketEventHaltsStateKind)
	if err != nil || !ok {
		return marketEventHaltsEntry{}, err
	}
	var state marketEventHaltsState
	if err := json.Unmarshal(raw, &state); err != nil {
		return marketEventHaltsEntry{}, fmt.Errorf("decode trading-halts authority: %w", err)
	}
	if state.Version != marketEventStateVersion {
		return marketEventHaltsEntry{}, fmt.Errorf("invalid trading-halts authority version %d", state.Version)
	}
	if err := validateHaltsEntry(state.Entry); err != nil {
		return marketEventHaltsEntry{}, fmt.Errorf("validate trading-halts authority: %w", err)
	}
	return cloneHaltsEntry(state.Entry), nil
}

func loadMarketEventBorrowFees(store *corestore.Store) (marketEventBorrowFeesState, int64, error) {
	for range marketAuthorityWriteAttempts {
		doc, ok, err := store.GetStateDocument(context.Background(), marketEventBorrowFeesScope, marketEventBorrowFeesStateKind)
		if err != nil {
			return marketEventBorrowFeesState{}, 0, err
		}
		if !ok {
			return marketEventBorrowFeesState{Version: marketEventBorrowFeesStateVersion}, 0, nil
		}
		var header struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(doc.JSON, &header); err != nil {
			return marketEventBorrowFeesState{}, 0, fmt.Errorf("decode borrow-fees authority header: %w", err)
		}
		switch header.Version {
		case marketEventStateVersion:
			var legacy marketEventBorrowFeesStateV1
			if err := decodeStrictMarketEventJSON(doc.JSON, &legacy); err != nil {
				return marketEventBorrowFeesState{}, 0, fmt.Errorf("decode borrow-fees v1 authority: %w", err)
			}
			if err := validateBorrowFeeEntry(legacy.Entry); err != nil {
				return marketEventBorrowFeesState{}, 0, fmt.Errorf("validate borrow-fees v1 authority: %w", err)
			}
			entry := cloneBorrowFeeEntry(legacy.Entry)
			state := marketEventBorrowFeesState{Version: marketEventBorrowFeesStateVersion, LastGood: &entry}
			raw, err := json.Marshal(state)
			if err != nil {
				return marketEventBorrowFeesState{}, 0, fmt.Errorf("encode borrow-fees v2 migration: %w", err)
			}
			saved, err := store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
				ScopeKey: marketEventBorrowFeesScope, Kind: marketEventBorrowFeesStateKind,
				ExpectedRevision: doc.Revision, JSON: raw,
			})
			if errors.Is(err, corestore.ErrRevisionConflict) {
				continue
			}
			if err != nil {
				return marketEventBorrowFeesState{}, 0, fmt.Errorf("migrate borrow-fees authority: %w", err)
			}
			return cloneBorrowFeesState(state), saved.Revision, nil
		case marketEventBorrowFeesStateVersion:
			var state marketEventBorrowFeesState
			if err := decodeStrictMarketEventJSON(doc.JSON, &state); err != nil {
				return marketEventBorrowFeesState{}, 0, fmt.Errorf("decode borrow-fees v2 authority: %w", err)
			}
			if err := validateBorrowFeesState(state); err != nil {
				return marketEventBorrowFeesState{}, 0, fmt.Errorf("validate borrow-fees v2 authority: %w", err)
			}
			return cloneBorrowFeesState(state), doc.Revision, nil
		default:
			return marketEventBorrowFeesState{}, 0, fmt.Errorf("invalid borrow-fees authority version %d", header.Version)
		}
	}
	return marketEventBorrowFeesState{}, 0, fmt.Errorf("migrate borrow-fees authority: %w after %d attempts", corestore.ErrRevisionConflict, marketAuthorityWriteAttempts)
}

func validateStoredBorrowInventory(store *corestore.Store) error {
	raw, ok, err := loadMarketState(store, marketEventBorrowInventoryScope, marketEventBorrowInventoryStateKind)
	if err != nil || !ok {
		return err
	}
	var state marketEventBorrowInventoryState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decode borrow-inventory authority: %w", err)
	}
	if err := validateBorrowInventoryState(state); err != nil {
		return fmt.Errorf("validate borrow-inventory authority: %w", err)
	}
	return nil
}

func (c *marketEventCache) persistRegSHO(ctx context.Context, entry marketEventRegSHOEntry) error {
	if c.authority == nil {
		return nil
	}
	if err := validateRegSHOEntry(entry); err != nil {
		return err
	}
	state := marketEventRegSHOState{Version: marketEventStateVersion, Entry: cloneRegSHOEntry(entry)}
	return saveMarketEventState(ctx, c.authority, marketEventRegSHOScope, marketEventRegSHOStateKind, marketEventRegSHOSource, marketEventRegSHOObservationKind, entry.FetchedAt, state, len(entry.Symbols), entry.SourceURL)
}

func (c *marketEventCache) persistHalts(ctx context.Context, entry marketEventHaltsEntry) error {
	if c.authority == nil {
		return nil
	}
	if err := validateHaltsEntry(entry); err != nil {
		return err
	}
	entry = normalizeHaltsEntry(entry)
	state := marketEventHaltsState{Version: marketEventStateVersion, Entry: entry}
	return saveMarketEventState(ctx, c.authority, marketEventHaltsScope, marketEventHaltsStateKind, marketEventHaltsSource, marketEventHaltsObservationKind, entry.FetchedAt, state, len(entry.Records), entry.SourceURL)
}

func (c *marketEventCache) persistBorrowFees(ctx context.Context, entry marketEventBorrowFeeEntry) error {
	return c.persistBorrowFeeSuccess(ctx, entry, entry.FetchedAt, entry.FetchedAt)
}

func (c *marketEventCache) persistBorrowFeeSuccess(ctx context.Context, entry marketEventBorrowFeeEntry, attemptedAt, completedAt time.Time) error {
	if err := validateBorrowFeeEntry(entry); err != nil {
		return err
	}
	attempt := marketEventBorrowFeeAttempt{
		Outcome: marketEventBorrowFeeOutcomeSuccess, AttemptedAt: attemptedAt, CompletedAt: completedAt,
	}
	revision, err := c.persistBorrowFeeState(ctx, &entry, attempt)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.borrowFees = cloneBorrowFeeEntry(entry)
	c.borrowFeesLastAttempt = cloneBorrowFeeAttempt(&attempt)
	c.borrowFeesRevision = revision
	c.mu.Unlock()
	return nil
}

func (c *marketEventCache) persistBorrowFeeFailure(ctx context.Context, cached marketEventBorrowFeeEntry, attempt marketEventBorrowFeeAttempt) error {
	var lastGood *marketEventBorrowFeeEntry
	if len(cached.Symbols) > 0 {
		entry := cloneBorrowFeeEntry(cached)
		lastGood = &entry
	}
	revision, err := c.persistBorrowFeeState(ctx, lastGood, attempt)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.borrowFeesLastAttempt = cloneBorrowFeeAttempt(&attempt)
	c.borrowFeesRevision = revision
	c.mu.Unlock()
	return nil
}

func (c *marketEventCache) persistBorrowFeeState(ctx context.Context, lastGood *marketEventBorrowFeeEntry, attempt marketEventBorrowFeeAttempt) (int64, error) {
	state := marketEventBorrowFeesState{Version: marketEventBorrowFeesStateVersion, LastGood: cloneBorrowFeeEntryPtr(lastGood), LastAttempt: cloneBorrowFeeAttempt(&attempt)}
	if err := validateBorrowFeesState(state); err != nil {
		return 0, err
	}
	c.mu.Lock()
	store := c.authority
	revision := c.borrowFeesRevision
	c.mu.Unlock()
	if store == nil {
		return revision, nil
	}
	statePayload, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	outcome := marketEventBorrowFeeOutcome{Version: marketEventBorrowFeesStateVersion, Attempt: *cloneBorrowFeeAttempt(&attempt)}
	if attempt.Outcome == marketEventBorrowFeeOutcomeSuccess {
		outcome.Entry = cloneBorrowFeeEntryPtr(lastGood)
	}
	outcomePayload, err := json.Marshal(outcome)
	if err != nil {
		return 0, err
	}
	records := 0
	sourceURL := ""
	if outcome.Entry != nil {
		records = len(outcome.Entry.Symbols)
		sourceURL = outcome.Entry.SourceURL
	}
	metadata, err := json.Marshal(struct {
		Version   int    `json:"version"`
		Outcome   string `json:"outcome"`
		Records   int    `json:"records"`
		SourceURL string `json:"source_url,omitempty"`
	}{marketEventBorrowFeesStateVersion, attempt.Outcome, records, sourceURL})
	if err != nil {
		return 0, err
	}
	saved, _, err := store.CompareAndSwapStateDocumentWithObservations(ctx, corestore.StateDocumentCAS{
		ScopeKey: marketEventBorrowFeesScope, Kind: marketEventBorrowFeesStateKind,
		ExpectedRevision: revision, JSON: statePayload,
	}, []corestore.ObservationInput{{
		ScopeKey: marketEventBorrowFeesScope, Source: marketEventBorrowFeesSource,
		Kind: marketEventBorrowFeesObservationKind, ObservedAt: attempt.CompletedAt,
		ContentType: "application/json", Payload: outcomePayload, MetadataJSON: metadata,
		DecisionEligible: true,
	}})
	if err != nil {
		return 0, err
	}
	return saved.Revision, nil
}

func (c *marketEventCache) persistBorrowInventory(ctx context.Context, observedAt time.Time, records map[string]marketEventBorrowInventoryRecord) error {
	if c.authority == nil || len(records) == 0 {
		return nil
	}
	state := marketEventBorrowInventoryState{Version: marketEventStateVersion, ObservedAt: observedAt, Records: records}
	if err := validateBorrowInventoryState(state); err != nil {
		return err
	}
	return saveMarketEventState(ctx, c.authority, marketEventBorrowInventoryScope, marketEventBorrowInventoryStateKind, marketEventBorrowInventorySource, marketEventBorrowInventoryObservationKind, observedAt, state, len(records), "IBKR generic tick 236")
}

func saveMarketEventState(ctx context.Context, store *corestore.Store, scope, stateKind, source, observationKind string, observedAt time.Time, value any, records int, sourceURL string) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(struct {
		Version   int    `json:"version"`
		Records   int    `json:"records"`
		SourceURL string `json:"source_url,omitempty"`
	}{marketEventStateVersion, records, sourceURL})
	if err != nil {
		return err
	}
	return saveMarketStateContext(ctx, store, scope, stateKind, corestore.ObservationInput{
		ScopeKey: scope, Source: source, Kind: observationKind,
		ObservedAt: observedAt, ContentType: "application/json",
		Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
	})
}

func validateRegSHOEntry(entry marketEventRegSHOEntry) error {
	if entry.FetchedAt.IsZero() || entry.AsOf.IsZero() || strings.TrimSpace(entry.SourceURL) == "" || entry.Symbols == nil {
		return errors.New("invalid Reg SHO envelope")
	}
	for key, row := range entry.Symbols {
		if key == "" || key != normSym(key) || row.Symbol != key {
			return fmt.Errorf("invalid Reg SHO row %q", key)
		}
	}
	return nil
}

func validateHaltsEntry(entry marketEventHaltsEntry) error {
	if entry.FetchedAt.IsZero() || entry.AsOf.IsZero() || strings.TrimSpace(entry.SourceURL) == "" || entry.Records == nil {
		return errors.New("invalid trading-halts envelope")
	}
	for i, row := range entry.Records {
		if row.Symbol == "" || row.Symbol != normSym(row.Symbol) || row.HaltedAt.IsZero() || strings.TrimSpace(row.ReasonCode) == "" {
			return fmt.Errorf("invalid trading-halts row %d", i)
		}
	}
	return nil
}

func validateBorrowFeeEntry(entry marketEventBorrowFeeEntry) error {
	if entry.FetchedAt.IsZero() || entry.AsOf.IsZero() || strings.TrimSpace(entry.SourceURL) == "" || len(entry.Symbols) == 0 {
		return errors.New("invalid borrow-fees envelope")
	}
	for key, row := range entry.Symbols {
		if key == "" || key != normSym(key) || row.Symbol != key || row.Available < 0 {
			return fmt.Errorf("invalid borrow-fees row %q", key)
		}
	}
	return nil
}

func validateBorrowFeesState(state marketEventBorrowFeesState) error {
	if state.Version != marketEventBorrowFeesStateVersion {
		return fmt.Errorf("invalid borrow-fees state version %d", state.Version)
	}
	if state.LastGood != nil {
		if err := validateBorrowFeeEntry(*state.LastGood); err != nil {
			return err
		}
	}
	if state.LastAttempt == nil {
		return nil
	}
	attempt := state.LastAttempt
	if attempt.AttemptedAt.IsZero() || attempt.CompletedAt.IsZero() || attempt.CompletedAt.Before(attempt.AttemptedAt) {
		return errors.New("invalid borrow-fee attempt timestamps")
	}
	switch attempt.Outcome {
	case marketEventBorrowFeeOutcomeSuccess:
		if state.LastGood == nil || attempt.Failure != nil || attempt.NextAttempt != nil {
			return errors.New("invalid successful borrow-fee attempt")
		}
	case marketEventBorrowFeeOutcomeFailure:
		if attempt.Failure == nil || attempt.NextAttempt == nil || !attempt.NextAttempt.After(attempt.CompletedAt) {
			return errors.New("invalid failed borrow-fee attempt")
		}
		if err := validateBorrowFeeSourceFailure(*attempt.Failure); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid borrow-fee attempt outcome %q", attempt.Outcome)
	}
	return nil
}

func marketEventBorrowFeeLastGood(state marketEventBorrowFeesState) marketEventBorrowFeeEntry {
	if state.LastGood == nil {
		return marketEventBorrowFeeEntry{}
	}
	return cloneBorrowFeeEntry(*state.LastGood)
}

func cloneBorrowFeesState(in marketEventBorrowFeesState) marketEventBorrowFeesState {
	return marketEventBorrowFeesState{
		Version: in.Version, LastGood: cloneBorrowFeeEntryPtr(in.LastGood), LastAttempt: cloneBorrowFeeAttempt(in.LastAttempt),
	}
}

func cloneBorrowFeeEntryPtr(in *marketEventBorrowFeeEntry) *marketEventBorrowFeeEntry {
	if in == nil {
		return nil
	}
	out := cloneBorrowFeeEntry(*in)
	return &out
}

func cloneBorrowFeeAttempt(in *marketEventBorrowFeeAttempt) *marketEventBorrowFeeAttempt {
	if in == nil {
		return nil
	}
	out := *in
	if in.NextAttempt != nil {
		next := *in.NextAttempt
		out.NextAttempt = &next
	}
	if in.Failure != nil {
		failure := *in.Failure
		out.Failure = &failure
	}
	return &out
}

func decodeStrictMarketEventJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateBorrowInventoryState(state marketEventBorrowInventoryState) error {
	if state.Version != marketEventStateVersion || state.ObservedAt.IsZero() || len(state.Records) == 0 {
		return errors.New("invalid borrow-inventory envelope")
	}
	for key, row := range state.Records {
		if key == "" || key != normSym(key) || row.Symbol != key || row.ShortableShares < 0 || row.AsOf.IsZero() {
			return fmt.Errorf("invalid borrow-inventory row %q", key)
		}
	}
	return nil
}

func normalizeHaltsEntry(entry marketEventHaltsEntry) marketEventHaltsEntry {
	entry = cloneHaltsEntry(entry)
	slices.SortFunc(entry.Records, func(a, b marketEventHaltRecord) int {
		if c := strings.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		if c := a.HaltedAt.Compare(b.HaltedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ReasonCode, b.ReasonCode)
	})
	return entry
}
