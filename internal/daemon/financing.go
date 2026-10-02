package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/financing"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func financingEvidenceScope(scope brokerStateScope, projectionScope string) string {
	return edgeScopeFingerprint(scope) + "\x00" + projectionScope
}

func (s *Server) loadFinancingStatements(ctx context.Context, scope brokerStateScope) ([]flexstmt.Statement, error) {
	if s.coreStore == nil || !brokerScopeConcrete(scope) {
		return nil, fmt.Errorf("financing authority unavailable")
	}
	selection := s.flexEvidenceSelection()
	projectionScope := statementProjectionScopeForSelection(selection)
	records, err := s.coreStore.LoadStatementRecords(ctx, projectionScope, []string{corestore.StatementRecordMetadata}, statementProjectionMaxRows)
	if err != nil || len(records) == statementProjectionMaxRows {
		return nil, fmt.Errorf("financing projection unavailable")
	}
	statements := []flexstmt.Statement{}
	for _, record := range records {
		if record.AccountKey != scope.Account {
			continue
		}
		var item statementMetadataProjectionPayload
		if err := json.Unmarshal(record.RawJSON, &item); err != nil || item.Version != statementProjectionVersion || item.QueryFingerprint != selection.ActiveQueryFingerprint || item.FromDate.IsZero() || item.ToDate.Before(item.FromDate) {
			return nil, fmt.Errorf("financing projection metadata invalid")
		}
		statements = append(statements, flexstmt.Statement{AccountID: record.AccountKey, FromDate: item.FromDate, ToDate: item.ToDate,
			WhenGenerated: record.GeneratedAt, Positions: item.PositionSnapshot, Financing: item.Financing})
	}
	if !sameBrokerScope(scope, s.currentBrokerStateScope()) || projectionScope != s.activeStatementProjectionScope() {
		return nil, fmt.Errorf("financing scope changed")
	}
	return statements, nil
}

func (s *Server) attachLendingAnnotations(ctx context.Context, positions *rpc.PositionsResult, scope brokerStateScope) {
	if s.cfg == nil || !s.cfg.Flex.Enabled || !currentPortfolioAuthority(positions.Authority) {
		return
	}
	statements, err := s.loadFinancingStatements(ctx, scope)
	if err != nil {
		return
	}
	annotations := financing.LoanAnnotations(statements, latestCompletedFlexDate(s.edgeNow()))
	for i := range positions.Stocks {
		row := &positions.Stocks[i]
		annotation, ok := annotations[int64(row.ConID)]
		if !ok || row.SecType != "STOCK" && row.SecType != "STK" || annotation.OwnedQuantity == nil || row.Quantity != *annotation.OwnedQuantity || row.Currency != annotation.Currency {
			continue
		}
		row.Lending = &annotation
	}
	for i := range positions.ByUnderlying {
		stock := positions.ByUnderlying[i].Stock
		if stock == nil {
			continue
		}
		for _, row := range positions.Stocks {
			if row.ConID == stock.ConID {
				stock.Lending = row.Lending
				break
			}
		}
	}
}

func (s *Server) financingPeriod(ctx context.Context, scope brokerStateScope, window string) (time.Time, time.Time, string) {
	to := latestCompletedFlexDate(s.edgeNow())
	days := 365
	if window == "90d" {
		days = 90
	}
	from, base := to.AddDate(0, 0, -days), ""
	publication, ok, err := s.loadEdgePublication(ctx)
	if err == nil && ok && publication.ScopeFingerprint == edgeScopeFingerprint(scope) {
		if result, ok := publication.Windows[window]; ok && result.Account != nil {
			from, to, base = result.Account.ActualFrom, result.Account.ActualTo, result.Account.BaseCurrency
		}
	}
	return from, to, base
}

func (s *Server) handleFinancingFees(ctx context.Context, req *rpc.Request) (*rpc.FinancingFeesResult, error) {
	var params rpc.FinancingFeesParams
	if err := decodeParams(req.Params, &params); err != nil {
		return nil, err
	}
	params, err := rpc.NormalizeFinancingFeesParams(params)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	scope := s.currentBrokerStateScope()
	projectionScope := s.activeStatementProjectionScope()
	if s.cfg == nil || !s.cfg.Flex.Enabled || strings.TrimSpace(s.cfg.Flex.QueryID) == "" || !brokerScopeConcrete(scope) {
		return nil, errBadRequest("financing reporting or account scope unavailable")
	}
	statements, err := s.loadFinancingStatements(ctx, scope)
	if err != nil {
		return nil, err
	}
	from, to, base := s.financingPeriod(ctx, scope, params.Window)
	if params.From != "" {
		from, _ = time.Parse(time.DateOnly, params.From)
		to, _ = time.Parse(time.DateOnly, params.To)
	}
	if !to.After(from) {
		return nil, errBadRequest("financing equity boundaries unavailable")
	}
	result := financing.Calculate(statements, financingEvidenceScope(scope, projectionScope), from, to, base)
	page, err := financingFeePage(result, params)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	if !sameBrokerScope(scope, s.currentBrokerStateScope()) || projectionScope != s.activeStatementProjectionScope() {
		return nil, errBadRequest("financing snapshot changed; reload evidence")
	}
	if err := rpc.ValidateFinancingFeesResult(page); err != nil {
		return nil, fmt.Errorf("invalid financing publication: %w", err)
	}
	return &page, nil
}

func financingFeePage(result financing.Result, params rpc.FinancingFeesParams) (rpc.FinancingFeesResult, error) {
	page := rpc.FinancingFeesResult{Summary: result.Summary, ConID: params.ConID, Fees: []rpc.FinancingFee{}}
	if params.Fingerprint != "" && params.Fingerprint != result.Summary.Fingerprint {
		return page, fmt.Errorf("financing snapshot changed; reload evidence")
	}
	filtered := []rpc.FinancingFee{}
	for _, fee := range result.Fees {
		if params.ConID == 0 || fee.ConID == params.ConID {
			filtered = append(filtered, fee)
		}
	}
	page.FilteredCount = len(filtered)
	key := financing.CursorScope(result.Summary.Fingerprint, params.ConID)
	offset := 0
	if params.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(params.Cursor)
		prefix, index, found := strings.Cut(string(raw), ":")
		value, parseErr := strconv.Atoi(index)
		if err != nil || !found || prefix != key || parseErr != nil || value < 1 || value >= len(filtered) {
			return page, fmt.Errorf("financing cursor expired or invalid; reload evidence")
		}
		offset = value
	}
	end := min(offset+params.Limit, len(filtered))
	page.Fees = append(page.Fees, filtered[offset:end]...)
	if end < len(filtered) {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(key + ":" + strconv.Itoa(end)))
	}
	return page, nil
}
