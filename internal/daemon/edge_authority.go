package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v2/internal/edge"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	edgePublicationStateKind = "edge_publication"
	edgeBarCacheStateKind    = "edge_bar_cache"
	edgePublicationVersion   = 1
	edgeBarCacheVersion      = 2
	edgeDailyLookbackDays    = 35
	edgeFullLookbackDays     = 400
	edgeFullRevalidateAfter  = 30 * 24 * time.Hour
	edgeDailyRefreshAfter    = 18 * time.Hour
	edgeStatementLoadTimeout = 30 * time.Second
)

type edgePublication struct {
	Version              int                        `json:"version"`
	ScopeFingerprint     string                     `json:"scope_fingerprint"`
	State                string                     `json:"state"`
	Reason               string                     `json:"reason,omitempty"`
	MissingRequirements  []string                   `json:"missing_requirements,omitempty"`
	EvidenceFingerprint  string                     `json:"evidence_fingerprint,omitempty"`
	Windows              map[string]edgecore.Result `json:"windows,omitempty"`
	LastFullRevalidation time.Time                  `json:"last_full_revalidation,omitzero"`
	UpdatedAt            time.Time                  `json:"updated_at"`
}

type edgeBarCache struct {
	Version              int                      `json:"version"`
	ScopeFingerprint     string                   `json:"scope_fingerprint"`
	LastFullRevalidation time.Time                `json:"last_full_revalidation,omitzero"`
	Contracts            map[string]edgeBarSeries `json:"contracts"`
}

type edgeBarSeries struct {
	ConID             int64               `json:"conid"`
	Symbol            string              `json:"symbol"`
	Currency          string              `json:"currency"`
	FetchedAt         time.Time           `json:"fetched_at"`
	FullRevalidatedAt time.Time           `json:"full_revalidated_at,omitzero"`
	Bars              []edgecore.DailyBar `json:"bars"`
}

type edgeContractSpec struct {
	ConID    int64
	Symbol   string
	Currency string
}

func (s *Server) startEdgeWorker(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	s.mu.Lock()
	if s.edgeWake != nil {
		s.mu.Unlock()
		return
	}
	s.edgeWake = make(chan struct{}, 1)
	wake := s.edgeWake
	s.edgeWorkerWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.edgeWorkerWG.Done()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			s.edgeBusy.Store(true)
			if err := s.rebuildEdgePublication(ctx); err != nil && ctx.Err() == nil {
				s.warnf("Canary Edge rebuild failed: %v", err)
				s.publishEdgeUnavailable(context.WithoutCancel(ctx), "authority_unavailable")
			}
			s.edgeBusy.Store(false)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-wake:
			}
		}
	}()
}

func (s *Server) kickEdgeRebuild() {
	if s == nil {
		return
	}
	s.mu.Lock()
	wake := s.edgeWake
	s.mu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (s *Server) rebuildEdgePublication(ctx context.Context) error {
	if s == nil || s.coreStore == nil {
		return fmt.Errorf("edge SQLite authority is unavailable")
	}
	configured := s.cfg != nil && s.cfg.Flex.Enabled && strings.TrimSpace(s.cfg.Flex.QueryID) != ""
	if !configured {
		return s.saveEdgePublication(ctx, edgePublication{Version: edgePublicationVersion, State: rpc.EdgeStateActionRequired, Reason: "flex_configuration_required", UpdatedAt: s.edgeNow()})
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return s.saveEdgePublication(ctx, edgePublication{Version: edgePublicationVersion, State: rpc.EdgeStateUnavailable, Reason: "account_scope_unavailable", UpdatedAt: s.edgeNow()})
	}
	scopeFingerprint := edgeScopeFingerprint(scope)
	acquisition, err := s.advanceEdgeFlexAcquisition(ctx, scopeFingerprint)
	if err != nil {
		return err
	}
	if acquisition.Pending {
		publication := edgePublication{Version: edgePublicationVersion, ScopeFingerprint: scopeFingerprint}
		if previous, ok, readErr := s.loadEdgePublication(ctx); readErr == nil && ok && previous.ScopeFingerprint == scopeFingerprint {
			publication = previous
		}
		publication.State = acquisition.State
		publication.Reason = acquisition.Reason
		publication.LastFullRevalidation = acquisition.LastFullRevalidation
		publication.UpdatedAt = s.edgeNow()
		return s.saveEdgePublication(ctx, publication)
	}
	loadCtx, cancel := context.WithTimeout(ctx, edgeStatementLoadTimeout)
	projected, err := s.loadEdgeProjectionEvidence(loadCtx, scope, scopeFingerprint)
	cancel()
	if err != nil {
		return err
	}
	statements, evidenceFingerprint := projected.Statements, projected.Fingerprint
	if len(statements) == 0 {
		return s.saveEdgePublication(ctx, edgePublication{Version: edgePublicationVersion, ScopeFingerprint: scopeFingerprint, State: rpc.EdgeStateBackfilling, Reason: "statement_account_pending", EvidenceFingerprint: evidenceFingerprint, LastFullRevalidation: acquisition.LastFullRevalidation, UpdatedAt: s.edgeNow()})
	}
	if missing := missingEdgeManifestSections(statements); len(missing) > 0 {
		return s.saveEdgePublication(ctx, edgePublication{Version: edgePublicationVersion, ScopeFingerprint: scopeFingerprint, State: rpc.EdgeStateActionRequired, Reason: "flex_query_incomplete", MissingRequirements: append([]string(nil), missing...), EvidenceFingerprint: evidenceFingerprint, LastFullRevalidation: acquisition.LastFullRevalidation, UpdatedAt: s.edgeNow()})
	}

	// Mark the prior result degraded before any HMDS work starts. A caller can
	// keep reading the last complete snapshot, but cannot mistake it for one
	// reconciled to the new statement fingerprint.
	if previous, ok, readErr := s.loadEdgePublication(ctx); readErr == nil && ok && previous.ScopeFingerprint == scopeFingerprint && previous.EvidenceFingerprint != evidenceFingerprint && len(previous.Windows) > 0 {
		previous.State = rpc.EdgeStateDegraded
		previous.Reason = "evidence_changed"
		previous.UpdatedAt = s.edgeNow()
		if err := s.saveEdgePublication(ctx, previous); err != nil {
			return err
		}
	}

	cache, err := s.loadEdgeBarCache(ctx)
	if err != nil {
		return err
	}
	if cache.ScopeFingerprint != scopeFingerprint {
		cache = edgeBarCache{Version: edgeBarCacheVersion, ScopeFingerprint: scopeFingerprint, Contracts: map[string]edgeBarSeries{}}
	}
	contracts := edgeContracts(statements, s.edgeNow().AddDate(0, 0, -edgeFullLookbackDays))
	now := s.edgeNow()
	full := cache.LastFullRevalidation.IsZero() || now.Sub(cache.LastFullRevalidation) >= edgeFullRevalidateAfter
	fetchFailures := 0
	fetchedAll := true
	for _, spec := range contracts {
		key := strconv.FormatInt(spec.ConID, 10)
		series := cache.Contracts[key]
		lookback, fetch := edgeBarRefreshPlan(now, full, series)
		if !fetch {
			continue
		}
		bars, fetchErr := s.fetchEdgeBars(ctx, spec, lookback)
		if fetchErr != nil {
			fetchFailures++
			fetchedAll = false
			continue
		}
		series = edgeBarSeries{
			ConID: spec.ConID, Symbol: spec.Symbol, Currency: spec.Currency,
			FetchedAt: now, FullRevalidatedAt: series.FullRevalidatedAt,
			Bars: mergeEdgeBars(series.Bars, bars),
		}
		if lookback == edgeFullLookbackDays {
			series.FullRevalidatedAt = now
		}
		cache.Contracts[key] = series
		if err := s.saveEdgeBarCache(ctx, cache); err != nil {
			return err
		}
	}
	if full && fetchedAll {
		cache.LastFullRevalidation = now
		if err := s.saveEdgeBarCache(ctx, cache); err != nil {
			return err
		}
	}

	bars := make(map[int64][]edgecore.DailyBar, len(cache.Contracts))
	for _, series := range cache.Contracts {
		bars[series.ConID] = append([]edgecore.DailyBar(nil), series.Bars...)
	}
	baseCurrency := inferEdgeBaseCurrency(statements)
	windows := make(map[string]edgecore.Result, 2)
	for _, window := range []struct {
		key  string
		days int
	}{{"90d", 90}, {"365d", 365}} {
		result, analyzeErr := edgecore.Analyze(edgecore.Input{WindowDays: window.days, BaseCurrency: baseCurrency, Statements: statements, Bars: bars})
		if analyzeErr != nil {
			return analyzeErr
		}
		windows[window.key] = result
	}
	state, reason := edgePublicationStatus(windows, fetchFailures)
	if !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return nil
	}
	lastFull := oldestEdgeRevalidation(acquisition.LastFullRevalidation, cache.LastFullRevalidation)
	return s.saveEdgePublication(ctx, edgePublication{Version: edgePublicationVersion, ScopeFingerprint: scopeFingerprint, State: state, Reason: reason, EvidenceFingerprint: evidenceFingerprint, Windows: windows, LastFullRevalidation: lastFull, UpdatedAt: s.edgeNow()})
}

// edgeBarRefreshPlan keeps full-history authority at contract granularity.
// A recent global cache refresh cannot prove a contract that appeared only
// after a corrected or expanded statement query received the 400-day seed.
func edgeBarRefreshPlan(now time.Time, globalFullDue bool, series edgeBarSeries) (lookback int, fetch bool) {
	seriesFullDue := series.FullRevalidatedAt.IsZero() || now.Sub(series.FullRevalidatedAt) >= edgeFullRevalidateAfter
	if !globalFullDue && !seriesFullDue && !series.FetchedAt.IsZero() && now.Sub(series.FetchedAt) < edgeDailyRefreshAfter {
		return 0, false
	}
	if globalFullDue || seriesFullDue {
		return edgeFullLookbackDays, true
	}
	return edgeDailyLookbackDays, true
}

func (s *Server) fetchEdgeBars(ctx context.Context, spec edgeContractSpec, lookback int) ([]edgecore.DailyBar, error) {
	contract := ibkrlib.Contract{ConID: int(spec.ConID), Symbol: spec.Symbol, SecType: "STK", Exchange: "SMART", Currency: spec.Currency}
	var (
		raw []ibkrlib.HistoricalBar
		err error
	)
	if s.edgeFetchBarsFn != nil {
		raw, err = s.edgeFetchBarsFn(ctx, contract, lookback)
	} else {
		connector := s.gatewayConnector()
		if connector == nil {
			return nil, fmt.Errorf("gateway unavailable")
		}
		raw, err = connector.FetchHistoricalDailyTradeBarsWithContract(ctx, contract, lookback, 0)
	}
	if err != nil {
		return nil, err
	}
	out := make([]edgecore.DailyBar, 0, len(raw))
	for _, bar := range raw {
		label := historyBarSessionDate(bar)
		day, parseErr := time.Parse("2006-01-02", label)
		if parseErr != nil || bar.Close <= 0 {
			continue
		}
		out = append(out, edgecore.DailyBar{ConID: spec.ConID, Day: day, Close: bar.Close})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no usable daily bars")
	}
	return out, nil
}

func edgeContracts(statements []flexstmt.Statement, from time.Time) []edgeContractSpec {
	byConID := map[int64]edgeContractSpec{}
	for _, statement := range statements {
		for _, trade := range statement.Trades {
			if trade.ConID == 0 || trade.ExecutedAt.Before(from) || (trade.AssetClass != "STK" && trade.AssetClass != "ETF") {
				continue
			}
			byConID[trade.ConID] = edgeContractSpec{ConID: trade.ConID, Symbol: trade.Symbol, Currency: trade.Currency}
		}
	}
	ids := make([]int64, 0, len(byConID))
	for conid := range byConID {
		ids = append(ids, conid)
	}
	slices.Sort(ids)
	out := make([]edgeContractSpec, 0, len(ids))
	for _, conid := range ids {
		out = append(out, byConID[conid])
	}
	return out
}

func mergeEdgeBars(old, fresh []edgecore.DailyBar) []edgecore.DailyBar {
	byDay := map[string]edgecore.DailyBar{}
	for _, rows := range [][]edgecore.DailyBar{old, fresh} {
		for _, row := range rows {
			if row.Close > 0 && !row.Day.IsZero() {
				byDay[row.Day.UTC().Format("2006-01-02")] = row
			}
		}
	}
	keys := make([]string, 0, len(byDay))
	for key := range byDay {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 500 {
		keys = keys[len(keys)-500:]
	}
	out := make([]edgecore.DailyBar, 0, len(keys))
	for _, key := range keys {
		out = append(out, byDay[key])
	}
	return out
}

func missingEdgeManifestSections(statements []flexstmt.Statement) []string {
	return flexstmt.MissingQueryRequirements(statements)
}

func inferEdgeBaseCurrency(statements []flexstmt.Statement) string {
	counts := map[string]int{}
	for _, statement := range statements {
		for _, rate := range statement.FXRates {
			if rate.ToCurrency != "" {
				counts[rate.ToCurrency]++
			}
		}
	}
	best := ""
	for currency, count := range counts {
		if count > counts[best] || count == counts[best] && currency < best {
			best = currency
		}
	}
	if best != "" {
		return best
	}
	// A single currency carrying broker-stated fxRateToBase=1 is usable
	// evidence for an otherwise all-base-currency account. Multiple candidates
	// are ambiguous and deliberately remain unlabeled/unscored.
	candidates := map[string]bool{}
	for _, statement := range statements {
		for _, trade := range statement.Trades {
			if trade.FXRateToBase != nil && *trade.FXRateToBase == 1 && trade.Currency != "" {
				candidates[strings.ToUpper(trade.Currency)] = true
			}
		}
		for _, position := range statement.Positions {
			if position.FXRateToBase != nil && *position.FXRateToBase == 1 && position.Currency != "" {
				candidates[strings.ToUpper(position.Currency)] = true
			}
		}
	}
	if len(candidates) == 1 {
		for currency := range candidates {
			return currency
		}
	}
	return ""
}

func edgeHasReason(windows map[string]edgecore.Result, reason string) bool {
	for _, result := range windows {
		if result.Coverage.ReasonCounts[reason] > 0 {
			return true
		}
	}
	return false
}

func edgePublicationStatus(windows map[string]edgecore.Result, fetchFailures int) (string, string) {
	fullWindow := windows["365d"]
	if !slices.Contains(fullWindow.Coverage.PresentSections, "trades") {
		return rpc.EdgeStateInsufficient, "trade_history_unproved"
	}
	if fullWindow.Coverage.TradeChanges == 0 {
		return rpc.EdgeStateCurrent, "no_trade_changes"
	}
	if edgeHasReason(windows, edgecore.ReasonMarketDataUnavailable) {
		if fetchFailures > 0 {
			return rpc.EdgeStateDegraded, "market_data_unavailable"
		}
		return rpc.EdgeStateBackfilling, "market_data_backfill_pending"
	}
	if fetchFailures > 0 {
		return rpc.EdgeStateDegraded, "market_data_partial"
	}
	return rpc.EdgeStateCurrent, ""
}

func edgeStatementsForScope(statements []flexstmt.Statement, scope brokerStateScope) []flexstmt.Statement {
	out := make([]flexstmt.Statement, 0, len(statements))
	for _, statement := range statements {
		if strings.EqualFold(strings.TrimSpace(statement.AccountID), strings.TrimSpace(scope.Account)) {
			out = append(out, statement)
		}
	}
	return out
}

func edgeScopeFingerprint(scope brokerStateScope) string {
	h := sha256.New()
	h.Write([]byte("canary.edge.scope.v1"))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToUpper(strings.TrimSpace(scope.Account))))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(scope.Mode))))
	return "scope_" + hex.EncodeToString(h.Sum(nil)[:16])
}

func oldestEdgeRevalidation(left, right time.Time) time.Time {
	if left.IsZero() || right.IsZero() {
		return time.Time{}
	}
	if left.Before(right) {
		return left
	}
	return right
}

func (s *Server) edgeNow() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

type edgeProjectionEvidence struct {
	Statements  []flexstmt.Statement
	Fingerprint string
}

func (s *Server) loadEdgeProjectionEvidence(ctx context.Context, scope brokerStateScope, scopeFingerprint string) (edgeProjectionEvidence, error) {
	var out edgeProjectionEvidence
	projectionScope := s.activeStatementProjectionScope()
	snapshot, err := s.coreStore.LoadStatementProjectionSnapshot(ctx, projectionScope, statementProjectionMaxRows*25, statementProjectionMaxRows)
	if err != nil {
		return out, err
	}
	if len(snapshot.Records) == statementProjectionMaxRows*25 {
		return out, fmt.Errorf("edge statement projection exceeds supported size")
	}
	if len(snapshot.EquityDays) == statementProjectionMaxRows {
		return out, fmt.Errorf("edge statement equity projection exceeds supported size")
	}
	statements, err := edgeStatementsFromProjection(snapshot, scope)
	if err != nil {
		return out, err
	}
	out.Statements = statements
	out.Fingerprint = edgeProjectionFingerprintSnapshot(snapshot, scope, scopeFingerprint)
	return out, nil
}

func (s *Server) edgeProjectionFingerprint(ctx context.Context, scope brokerStateScope, scopeFingerprint string) (string, error) {
	evidence, err := s.loadEdgeProjectionEvidence(ctx, scope, scopeFingerprint)
	if err != nil {
		return "", err
	}
	return evidence.Fingerprint, nil
}

func edgeProjectionFingerprintSnapshot(snapshot corestore.StatementProjectionSnapshot, scope brokerStateScope, scopeFingerprint string) string {
	h := sha256.New()
	h.Write([]byte("canary.edge.projection.v1"))
	h.Write([]byte{0})
	h.Write([]byte(scopeFingerprint))
	for _, record := range snapshot.Records {
		if !strings.EqualFold(strings.TrimSpace(record.AccountKey), strings.TrimSpace(scope.Account)) {
			continue
		}
		h.Write([]byte{0})
		h.Write([]byte(record.Kind))
		h.Write([]byte{0})
		h.Write([]byte(record.RecordKey))
		h.Write([]byte{0})
		h.Write([]byte(record.GeneratedAt.UTC().Format(time.RFC3339Nano)))
		digest := sha256.Sum256(record.RawJSON)
		h.Write(digest[:])
	}
	for _, row := range snapshot.EquityDays {
		if !strings.EqualFold(strings.TrimSpace(row.AccountKey), strings.TrimSpace(scope.Account)) {
			continue
		}
		h.Write([]byte{0})
		h.Write([]byte("equity"))
		h.Write([]byte{0})
		h.Write([]byte(row.Day))
		h.Write([]byte{0})
		h.Write([]byte(row.EquityBaseText))
		h.Write([]byte{0})
		h.Write([]byte(row.GeneratedAt.UTC().Format(time.RFC3339Nano)))
	}
	return "evidence_" + hex.EncodeToString(h.Sum(nil)[:16])
}

func edgeStatementsFromProjection(snapshot corestore.StatementProjectionSnapshot, scope brokerStateScope) ([]flexstmt.Statement, error) {
	account := strings.TrimSpace(scope.Account)
	statement := flexstmt.Statement{AccountID: account}
	positionSnapshots := []flexstmt.Statement{}
	have := false
	for _, record := range snapshot.Records {
		if !strings.EqualFold(strings.TrimSpace(record.AccountKey), account) {
			continue
		}
		have = true
		if record.GeneratedAt.After(statement.WhenGenerated) {
			statement.WhenGenerated = record.GeneratedAt.UTC()
		}
		switch record.Kind {
		case corestore.StatementRecordMetadata:
			var item statementMetadataProjectionPayload
			if err := json.Unmarshal(record.RawJSON, &item); err != nil || item.Version != statementProjectionVersion || item.FromDate.IsZero() || item.ToDate.IsZero() || item.FromDate.After(item.ToDate) || item.QueryFingerprint != "" && !validFlexQueryFingerprint(item.QueryFingerprint) {
				return nil, fmt.Errorf("decode Edge statement metadata projection")
			}
			if statement.FromDate.IsZero() || item.FromDate.Before(statement.FromDate) {
				statement.FromDate = item.FromDate.UTC()
			}
			if item.ToDate.After(statement.ToDate) {
				statement.ToDate = item.ToDate.UTC()
			}
			if item.ManifestVersion != "" {
				statement.ManifestVersion = item.ManifestVersion
			}
			for _, coverage := range item.Coverage {
				if coverage.Key != "open_positions" {
					statement.Coverage = append(statement.Coverage, coverage)
				}
			}
			if statementSectionPresent(item.Coverage, "open_positions") {
				positions := append([]flexstmt.OpenPosition{}, item.PositionSnapshot...)
				for i := range positions {
					positions[i].AccountID = account
				}
				positionCoverage := make([]flexstmt.SectionCoverage, 0, 1)
				for _, coverage := range item.Coverage {
					if coverage.Key == "open_positions" {
						positionCoverage = append(positionCoverage, coverage)
					}
				}
				positionSnapshots = append(positionSnapshots, flexstmt.Statement{
					AccountID: account, FromDate: item.FromDate.UTC(), ToDate: item.ToDate.UTC(),
					WhenGenerated: record.GeneratedAt.UTC(), ManifestVersion: item.ManifestVersion,
					Coverage: positionCoverage, Positions: positions,
				})
			}
		case corestore.StatementRecordTrade:
			var item flexstmt.Trade
			if err := json.Unmarshal(record.RawJSON, &item); err != nil {
				return nil, fmt.Errorf("decode Edge trade projection: %w", err)
			}
			item.AccountID = account
			statement.Trades = append(statement.Trades, item)
		case corestore.StatementRecordInstrument:
			var item flexstmt.Instrument
			if err := json.Unmarshal(record.RawJSON, &item); err != nil {
				return nil, fmt.Errorf("decode Edge instrument projection: %w", err)
			}
			statement.Instruments = append(statement.Instruments, item)
		case corestore.StatementRecordPosition:
			var item flexstmt.OpenPosition
			if err := json.Unmarshal(record.RawJSON, &item); err != nil {
				return nil, fmt.Errorf("decode Edge position projection: %w", err)
			}
			// Exact per-statement position snapshots live in metadata. Individual
			// rows remain projected and versioned for audit, but cannot represent
			// the broker-significant empty-snapshot case on their own.
		case corestore.StatementRecordOptionEvent:
			var item flexstmt.OptionEvent
			if err := json.Unmarshal(record.RawJSON, &item); err != nil {
				return nil, fmt.Errorf("decode Edge option-event projection: %w", err)
			}
			item.AccountID = account
			statement.OptionEvents = append(statement.OptionEvents, item)
		case corestore.StatementRecordCorporateAction:
			var item flexstmt.CorporateAction
			if err := json.Unmarshal(record.RawJSON, &item); err != nil {
				return nil, fmt.Errorf("decode Edge corporate-action projection: %w", err)
			}
			item.AccountID = account
			statement.CorporateActions = append(statement.CorporateActions, item)
		case corestore.StatementRecordTransfer:
			var item flexstmt.Transfer
			if err := json.Unmarshal(record.RawJSON, &item); err != nil {
				return nil, fmt.Errorf("decode Edge transfer projection: %w", err)
			}
			item.AccountID = account
			statement.Transfers = append(statement.Transfers, item)
		case corestore.StatementRecordCash:
			var item flexstmt.CashLine
			if err := json.Unmarshal(record.RawJSON, &item); err != nil {
				return nil, fmt.Errorf("decode Edge cash projection: %w", err)
			}
			statement.Cash = append(statement.Cash, item)
		case corestore.StatementRecordFXRate:
			var item flexstmt.FXRate
			if err := json.Unmarshal(record.RawJSON, &item); err != nil {
				return nil, fmt.Errorf("decode Edge FX projection: %w", err)
			}
			statement.FXRates = append(statement.FXRates, item)
		default:
			return nil, fmt.Errorf("unsupported Edge statement projection kind %q", record.Kind)
		}
	}
	for _, row := range snapshot.EquityDays {
		if !strings.EqualFold(strings.TrimSpace(row.AccountKey), account) {
			continue
		}
		have = true
		day, err := time.Parse(time.DateOnly, row.Day)
		if err != nil {
			return nil, fmt.Errorf("decode Edge equity projection date: %w", err)
		}
		total, err := strconv.ParseFloat(row.EquityBaseText, 64)
		if err != nil {
			return nil, fmt.Errorf("decode Edge equity projection amount: %w", err)
		}
		statement.Equity = append(statement.Equity, flexstmt.EquityRow{ReportDate: day.UTC(), TotalBase: total})
		if row.GeneratedAt.After(statement.WhenGenerated) {
			statement.WhenGenerated = row.GeneratedAt.UTC()
		}
	}
	if !have {
		return nil, nil
	}
	return append([]flexstmt.Statement{statement}, positionSnapshots...), nil
}

func (s *Server) loadEdgePublication(ctx context.Context) (edgePublication, bool, error) {
	var out edgePublication
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, edgePublicationStateKind)
	if err != nil || !ok {
		return out, ok, err
	}
	if err := json.Unmarshal(doc.JSON, &out); err != nil {
		return out, false, fmt.Errorf("decode Edge publication: %w", err)
	}
	if out.Version != edgePublicationVersion {
		return out, false, fmt.Errorf("unsupported Edge publication version")
	}
	return out, true, nil
}

func (s *Server) saveEdgePublication(ctx context.Context, publication edgePublication) error {
	publication.Version = edgePublicationVersion
	if publication.UpdatedAt.IsZero() {
		publication.UpdatedAt = s.edgeNow()
	}
	raw, err := json.Marshal(publication)
	if err != nil {
		return err
	}
	return s.replaceEdgeStateDocument(ctx, edgePublicationStateKind, raw)
}

func (s *Server) publishEdgeUnavailable(ctx context.Context, reason string) {
	publication, ok, _ := s.loadEdgePublication(ctx)
	if !ok {
		publication = edgePublication{Version: edgePublicationVersion}
	}
	publication.State, publication.Reason, publication.UpdatedAt = rpc.EdgeStateUnavailable, reason, s.edgeNow()
	_ = s.saveEdgePublication(ctx, publication)
}

func (s *Server) edgeSubsystemHealth() rpc.SubsystemHealth {
	sub := rpc.SubsystemHealth{Name: "edge"}
	if s == nil || s.coreStore == nil {
		sub.Status, sub.Message = "unavailable", "snapshot authority unavailable"
		return sub
	}
	if s.cfg == nil || !s.cfg.Flex.Enabled || strings.TrimSpace(s.cfg.Flex.QueryID) == "" {
		sub.Status, sub.Message = "action_required", "Flex setup required"
		return sub
	}
	publication, ok, err := s.loadEdgePublication(context.Background())
	if err != nil {
		sub.Status, sub.Message = "unavailable", "snapshot authority unreadable"
		return sub
	}
	if !ok {
		sub.Status, sub.Message = "computing", "initial broker-evidence backfill pending"
		return sub
	}
	if s.edgeBusy.Load() {
		sub.Status, sub.Message = "computing", "snapshot refresh in progress"
		return sub
	}
	switch publication.State {
	case rpc.EdgeStateCurrent:
		sub.Status = "ready"
	case rpc.EdgeStateBackfilling:
		sub.Status, sub.Message = "computing", "broker-evidence backfill in progress"
	case rpc.EdgeStateDegraded:
		sub.Status, sub.Message = "degraded", "last snapshot has incomplete or newer evidence"
	case rpc.EdgeStateInsufficient:
		sub.Status, sub.Message = "degraded", "completed Flex backfill returned no Trades section"
	case rpc.EdgeStateActionRequired:
		sub.Status, sub.Message = "action_required", "Flex query or credentials require attention"
	default:
		sub.Status, sub.Message = "unavailable", "no usable Edge snapshot"
	}
	return sub
}

func (s *Server) loadEdgeBarCache(ctx context.Context) (edgeBarCache, error) {
	out := edgeBarCache{Version: edgeBarCacheVersion, Contracts: map[string]edgeBarSeries{}}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, edgeBarCacheStateKind)
	if err != nil || !ok {
		return out, err
	}
	if err := json.Unmarshal(doc.JSON, &out); err != nil {
		return out, fmt.Errorf("decode Edge bar cache: %w", err)
	}
	if out.Version != 1 && out.Version != edgeBarCacheVersion {
		return out, fmt.Errorf("unsupported Edge bar cache version")
	}
	// Version 1 tracked only a global full refresh. Existing series therefore
	// remain deliberately unproved until each one earns the v2 marker.
	out.Version = edgeBarCacheVersion
	if out.Contracts == nil {
		out.Contracts = map[string]edgeBarSeries{}
	}
	return out, nil
}

func (s *Server) saveEdgeBarCache(ctx context.Context, cache edgeBarCache) error {
	cache.Version = edgeBarCacheVersion
	raw, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	return s.replaceEdgeStateDocument(ctx, edgeBarCacheStateKind, raw)
}

func (s *Server) replaceEdgeStateDocument(ctx context.Context, kind string, raw []byte) error {
	for range 4 {
		doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, kind)
		if err != nil {
			return err
		}
		expected := int64(0)
		if ok {
			expected = doc.Revision
		}
		_, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: kind, ExpectedRevision: expected, JSON: raw})
		if err == nil {
			return nil
		}
		if _, conflict := err.(*corestore.RevisionConflictError); !conflict {
			return err
		}
	}
	return fmt.Errorf("edge state update conflicted repeatedly")
}

func (s *Server) handleEdgeSnapshot(ctx context.Context, req *rpc.Request) (*rpc.EdgeResult, error) {
	var params rpc.EdgeSnapshotParams
	if err := decodeParams(req.Params, &params); err != nil {
		return nil, err
	}
	params, err := rpc.NormalizeEdgeSnapshotParams(params)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	window, horizon, limit := params.Window, params.HorizonSessions, params.Limit
	if s.cfg == nil || !s.cfg.Flex.Enabled || strings.TrimSpace(s.cfg.Flex.QueryID) == "" {
		return edgeStateOnlyResult(rpc.EdgeStateActionRequired, "flex_configuration_required", window, horizon), nil
	}
	if s.coreStore == nil {
		return nil, fmt.Errorf("edge authority is unavailable")
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return edgeStateOnlyResult(rpc.EdgeStateUnavailable, "account_scope_unavailable", window, horizon), nil
	}
	scopeFingerprint := edgeScopeFingerprint(scope)
	publication, ok, err := s.loadEdgePublication(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		state, reason := rpc.EdgeStateBackfilling, "snapshot_pending"
		return edgeStateOnlyResult(state, reason, window, horizon), nil
	}
	if publication.ScopeFingerprint != scopeFingerprint {
		return edgeStateOnlyResult(rpc.EdgeStateUnavailable, "account_scope_changed", window, horizon), nil
	}
	currentEvidence, fingerprintErr := s.edgeProjectionFingerprint(ctx, scope, scopeFingerprint)
	if fingerprintErr != nil {
		publication.State, publication.Reason = rpc.EdgeStateDegraded, "authority_unavailable"
	} else if publication.EvidenceFingerprint != "" && currentEvidence != publication.EvidenceFingerprint {
		publication.State, publication.Reason = rpc.EdgeStateDegraded, "evidence_changed"
	}
	coreResult, haveWindow := publication.Windows[window]
	result := edgeStateOnlyResult(publication.State, publication.Reason, window, horizon)
	if result.Setup != nil {
		result.Setup.MissingRequirements = append([]string(nil), publication.MissingRequirements...)
	}
	result.LastFullRevalidation = publication.LastFullRevalidation
	if haveWindow {
		populateRPCEdgeResult(result, coreResult, horizon, limit)
		result.State, result.Reason = publication.State, publication.Reason
	}
	if params.ChangeID != "" {
		if !haveWindow {
			return nil, errBadRequest("edge change is unavailable before a snapshot is published")
		}
		found := false
		for _, change := range coreResult.Changes {
			if change.ID == params.ChangeID {
				result.Change = rpcEdgeChange(change)
				found = true
				break
			}
		}
		if !found {
			return nil, errBadRequest("edge change id was not found in this window")
		}
	}
	if err := rpc.ValidateEdgeResult(*result); err != nil {
		return nil, fmt.Errorf("invalid Edge publication: %w", err)
	}
	return result, nil
}

func edgeStateOnlyResult(state, reason, window string, horizon int) *rpc.EdgeResult {
	result := &rpc.EdgeResult{SchemaVersion: edgecore.SchemaVersion, State: state, Reason: reason, Window: window, HorizonSessions: horizon, ActionRollups: []rpc.EdgeActionRollup{}, Findings: []rpc.EdgeFinding{}, Options: []rpc.EdgeOptionResult{}, Coverage: rpc.EdgeCoverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}}, NotExecution: true}
	if state == rpc.EdgeStateActionRequired || state == rpc.EdgeStateInsufficient && reason == "trade_history_unproved" {
		result.Setup = edgeSetup(reason)
	}
	return result
}

func edgeSetup(reason string) *rpc.EdgeSetup {
	steps := flexstmt.SetupSteps()
	if reason == "trade_history_unproved" {
		steps = []string{
			"Open the saved Activity Flex Query in IBKR Client Portal.",
			"Under Trades choose Executions, choose Select All fields, and save the query.",
			"Canary detects the corrected report and rebuilds the 365-day database automatically; use canary reporting status to follow it. Edge needs no parameters or debug export.",
		}
	}
	setup := &rpc.EdgeSetup{ManifestVersion: flexstmt.ManifestVersion, Steps: steps}
	for _, section := range flexstmt.CanonicalQueryManifest() {
		setup.Sections = append(setup.Sections, rpc.EdgeSectionRequirement{Key: section.Key, Label: section.Label, LevelOfDetail: section.LevelOfDetail, Fields: append([]string(nil), section.RequiredFields...)})
	}
	return setup
}

func populateRPCEdgeResult(out *rpc.EdgeResult, in edgecore.Result, horizon, limit int) {
	out.AsOf, out.Account, out.Fingerprint = in.AsOf, rpcEdgeAccount(in.Account), in.Fingerprint
	out.Coverage = rpc.EdgeCoverage{TradeChanges: in.Coverage.TradeChanges, EligibleChanges: in.Coverage.EligibleChanges, ScoredByHorizon: cloneIntMap(in.Coverage.ScoredByHorizon), ReasonCounts: cloneStringIntMap(in.Coverage.ReasonCounts), PresentSections: append([]string(nil), in.Coverage.PresentSections...), MissingSections: append([]string(nil), in.Coverage.MissingSections...)}
	out.Method = rpc.EdgeMethod{Metric: in.Method.Metric, Counterfactual: in.Method.Counterfactual, HorizonDefinition: in.Method.HorizonDefinition, HeadlineSelection: in.Method.HeadlineSelection, FindingRanking: in.Method.FindingRanking, AccountDefinition: in.Method.AccountDefinition, Exclusions: in.Method.Exclusions, OptionsMethod: in.Method.OptionsMethod, NoCausalClaim: in.Method.NoCausalClaim, NoPredictiveClaim: in.Method.NoPredictiveClaim, NotInvestmentAdvice: in.Method.NotInvestmentAdvice}
	for _, rollup := range in.Rollups {
		row := rpc.EdgeActionRollup{Action: rollup.Action}
		for _, item := range rollup.Horizons {
			row.Horizons = append(row.Horizons, rpc.EdgeHorizonRollup{Sessions: item.Sessions, SampleCount: item.SampleCount, TotalBase: cloneAmount(item.TotalBase), MedianBase: cloneAmount(item.MedianBase)})
		}
		out.ActionRollups = append(out.ActionRollups, row)
	}
	for _, finding := range in.Findings {
		if finding.HorizonSessions != horizon || len(out.Findings) >= limit {
			continue
		}
		out.Findings = append(out.Findings, rpc.EdgeFinding{ChangeID: finding.ChangeID, Symbol: finding.Symbol, Action: finding.Action, Direction: finding.Direction, ExecutedAt: finding.ExecutedAt, HorizonSessions: finding.HorizonSessions, DecisionNotionalBase: finding.DecisionNotionalBase, DecisionImpactBase: finding.DecisionImpactBase, DecisionImpactPct: finding.DecisionImpactPct})
	}
	for i, option := range in.Options {
		if i == 20 {
			break
		}
		out.Options = append(out.Options, rpc.EdgeOptionResult{ID: option.ID, Grouping: option.Grouping, Symbol: option.Symbol, Underlying: option.Underlying, LegCount: option.LegCount, RealizedPNLBase: cloneAmount(option.RealizedPNLBase), OpenPNLBase: cloneAmount(option.OpenPNLBase), ActualPNLBase: cloneAmount(option.ActualPNLBase), ActualOnly: option.ActualOnly})
	}
	out.Headline = edgeHeadline(out)
}

func edgeHeadline(result *rpc.EdgeResult) string {
	if len(result.Findings) > 0 {
		currency := "base"
		if result.Account != nil && result.Account.BaseCurrency != "" {
			currency = result.Account.BaseCurrency
		}
		var selected *rpc.EdgeActionRollup
		var selectedHorizon *rpc.EdgeHorizonRollup
		for i := range result.ActionRollups {
			for j := range result.ActionRollups[i].Horizons {
				candidate := &result.ActionRollups[i].Horizons[j]
				if candidate.Sessions != result.HorizonSessions || candidate.SampleCount == 0 || candidate.TotalBase == nil || candidate.MedianBase == nil {
					continue
				}
				if selectedHorizon == nil || candidate.SampleCount > selectedHorizon.SampleCount {
					selected, selectedHorizon = &result.ActionRollups[i], candidate
				}
			}
		}
		if selected != nil && selectedHorizon != nil {
			pattern := "Mixed observed pattern"
			switch {
			case *selectedHorizon.TotalBase > 0 && *selectedHorizon.MedianBase > 0:
				pattern = "Observed strength"
			case *selectedHorizon.TotalBase < 0 && *selectedHorizon.MedianBase < 0:
				pattern = "Observed drag"
			}
			return fmt.Sprintf("%s: across %d clean %s, %d-session Decision price impact totaled %+.2f %s; median %+.2f %s.", pattern, selectedHorizon.SampleCount, edgeActionPlural(selected.Action), result.HorizonSessions, *selectedHorizon.TotalBase, currency, *selectedHorizon.MedianBase, currency)
		}
	}
	if slices.Contains(result.Coverage.MissingSections, "trades") {
		return "The completed one-year broker report returned no Trades section, so Canary cannot reconstruct past decisions. If this account traded during the period, verify Trades at execution detail in the saved Activity Flex Query; otherwise there is no trade history to score."
	}
	if result.Coverage.TradeChanges == 0 {
		return fmt.Sprintf("No stock or ETF position changes were observed in returned IBKR evidence for this %s window; account P/L remains separate when proven.", result.Window)
	}
	count := result.Coverage.ScoredByHorizon[result.HorizonSessions]
	return fmt.Sprintf("No repeated %d-session pattern has complete, non-overlapping evidence yet: %d of %d changes were scored.", result.HorizonSessions, count, result.Coverage.TradeChanges)
}

func edgeActionPlural(action string) string {
	switch action {
	case edgecore.ActionOpen:
		return "opens"
	case edgecore.ActionAdd:
		return "adds"
	case edgecore.ActionTrim:
		return "trims"
	case edgecore.ActionExit:
		return "exits"
	default:
		return "changes"
	}
}

func rpcEdgeAccount(in *edgecore.AccountResult) *rpc.EdgeAccountResult {
	if in == nil {
		return nil
	}
	return &rpc.EdgeAccountResult{BaseCurrency: in.BaseCurrency, RequestedFrom: in.RequestedFrom, ActualFrom: in.ActualFrom, ActualTo: in.ActualTo, StartingEquityBase: in.StartingEquityBase, EndingEquityBase: in.EndingEquityBase, ExternalFlowsBase: in.ExternalFlowsBase, ProfitLossBase: in.ProfitLossBase, Definition: in.Definition}
}

func rpcEdgeChange(in edgecore.Change) *rpc.EdgeChangeDetail {
	out := &rpc.EdgeChangeDetail{ID: in.ID, Symbol: in.Symbol, AssetClass: in.AssetClass, Currency: in.Currency, Action: in.Action, Direction: in.Direction, ExecutedAt: in.ExecutedAt, DeltaQuantity: in.DeltaQuantity, PositionBefore: in.PositionBefore, PositionAfter: in.PositionAfter, ExecutionVWAP: cloneAmount(in.ExecutionVWAP), Multiplier: cloneAmount(in.Multiplier), DirectCostsBase: cloneAmount(in.DirectCostsBase)}
	for _, score := range in.Scores {
		row := rpc.EdgeHorizonScore{Sessions: score.Sessions, HorizonClose: cloneAmount(score.HorizonClose), HorizonFX: cloneAmount(score.HorizonFX), DecisionNotionalBase: cloneAmount(score.DecisionNotionalBase), DecisionImpactBase: cloneAmount(score.DecisionImpactBase), DecisionImpactPct: cloneAmount(score.DecisionImpactPct), Reason: score.Reason}
		if score.HorizonDay != nil {
			day := *score.HorizonDay
			row.HorizonDay = &day
		}
		out.Scores = append(out.Scores, row)
	}
	return out
}

func cloneAmount(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneIntMap(in map[int]int) map[int]int {
	out := make(map[int]int, len(in))
	maps.Copy(out, in)
	return out
}

func cloneStringIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	maps.Copy(out, in)
	return out
}
