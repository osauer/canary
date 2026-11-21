package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

// One joined worker owns checks. Reads never wake it; explicit checks and the
// due timer coalesce. Each pass admits at most eight reference quote probes and,
// when no chart read has proven it, one reference history read; derivatives,
// instrument history and analytics stay with their existing acquisition owners.
func (s *Server) startDataHealthChecks(ctx context.Context) {
	s.loadDataHealthHistory()
	s.dataHealth.mu.Lock()
	s.dataHealth.wake = make(chan struct{}, 1)
	wake := s.dataHealth.wake
	s.dataHealth.mu.Unlock()
	s.dataHealth.loopWG.Go(func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !s.productionStateDatabase {
					continue
				}
			case <-wake:
			}
			if ctx.Err() != nil {
				return
			}
			s.dataHealth.mu.Lock()
			s.dataHealth.latest = rpc.DataHealthResult{}
			s.dataHealth.checkResult = rpc.DataCheckResult{StartedAt: s.orderNow(), Running: true, Detail: "Checking due quote capabilities; other producers retain their schedules"}
			s.dataHealth.mu.Unlock()
			checkCtx, cancel := context.WithTimeout(ctx, 24*time.Second)
			result := s.runDataHealthCheck(checkCtx)
			cancel()
			if s.opend == nil && s.brokerReadyForChecks() {
				s.checkHistorySource(ctx, s.fetchMarketHistoryDays)
			}
			persistCtx, persistCancel := context.WithTimeout(ctx, 2*time.Second)
			s.persistDataHealthHistory(persistCtx)
			persistCancel()
			s.dataHealth.mu.Lock()
			s.dataHealth.checkResult = result
			s.dataHealth.latest = rpc.DataHealthResult{}
			s.dataHealth.mu.Unlock()
		}
	})
}

func (s *Server) requestDataHealthCheck() (rpc.DataCheckResult, error) {
	s.dataHealth.mu.Lock()
	defer s.dataHealth.mu.Unlock()
	if s.dataHealth.wake == nil {
		return rpc.DataCheckResult{}, errBadRequest("data check worker unavailable")
	}
	result := s.dataHealth.checkResult
	if !result.Running {
		// A recently completed check is an explicit reuse, not an unbounded probe.
		if !result.CompletedAt.IsZero() && s.orderNow().Sub(result.CompletedAt) < 30*time.Second {
			return result, nil
		}
		select {
		case s.dataHealth.wake <- struct{}{}:
		default:
		}
		result = rpc.DataCheckResult{StartedAt: s.orderNow(), Running: true, Detail: "Required-data check queued"}
		s.dataHealth.checkResult = result
	}
	return result, nil
}

func (s *Server) runDataHealthCheck(ctx context.Context) rpc.DataCheckResult {
	if s.opend != nil {
		return s.runOpenDDataCheck(ctx)
	}
	now := s.orderNow()
	result := rpc.DataCheckResult{StartedAt: now, Detail: "Source checks reuse Canary quote observations and retry windows. Instrument coverage stays with quotes and charts; other sources retain their producer schedules."}
	c := s.gatewayConnector()
	if c == nil || !c.IsReady() || c.BackendLink().Down || !s.postConnectSetupDone.Load() {
		result.Deferred = 1
		result.Detail = "Broker connection unavailable; required checks deferred. No acquisition attempted."
		result.CompletedAt = s.orderNow()
		return result
	}
	binding, ok := c.CaptureSession()
	if !ok {
		result.Deferred = 1
		result.CompletedAt = s.orderNow()
		return result
	}
	// Source checks use the existing market reference probes, never a holdings
	// inventory. Quote consumers retain all instrument-specific diagnostics.
	row := s.feedDataHealth(quoteSourceID, now)
	if row.State != "unknown" && now.Before(row.ValidUntil) {
		result.Reused = 1
		result.CompletedAt = s.orderNow()
		return result
	}
	result.Checked = 1
	attempts, answered, interrupted := 0, 0, false
	for _, probe := range marketReferences() {
		if attempts >= 8 || ctx.Err() != nil {
			break
		}
		attempts++
		contract := probe.Quote.Contract
		var err error
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if contract.SecType == "FUT" && contract.Expiry == "" {
			var resolved ibkr.Contract
			resolved, err = c.FrontFuture(readCtx, ibkr.Contract{Symbol: contract.Symbol, SecType: contract.SecType, Exchange: contract.Exchange, Currency: contract.Currency}, now)
			if err == nil {
				contract = displayContract(resolved)
			}
		}
		if err == nil {
			raw, _ := json.Marshal(rpc.QuoteSnapshotParams{Contract: contract, TimeoutMs: 1500})
			if q, qerr := s.handleQuoteSnapshot(readCtx, &rpc.Request{Params: raw}); qerr == nil && q != nil && q.Price != nil {
				answered++
			}
		}
		cancel()
		if !c.SessionCurrent(binding) {
			interrupted = true
			break
		}
	}
	row = s.feedDataHealth(quoteSourceID, s.orderNow())
	switch row.State {
	case "limited":
		result.Limited = 1
	case "unavailable":
		result.Failed = 1
	case "unknown":
		result.Deferred = 1
	}
	// The check judges its own probes as well as the rolled-up row: a feed
	// that answered none of the reference instruments failed, whatever an
	// older line's answer leaves on the row. A session change mid-check
	// defers it instead.
	switch {
	case interrupted:
		result.Limited, result.Failed, result.Deferred = 0, 0, 1
	case attempts > 0 && answered == 0 && result.Deferred == 0:
		result.Limited, result.Failed = 0, 1
	}

	result.CompletedAt = s.orderNow()
	return result
}

// brokerReadyForChecks reports a connected, set-up broker session that a
// source check may read through.
func (s *Server) brokerReadyForChecks() bool {
	c := s.gatewayConnector()
	return c != nil && c.IsReady() && !c.BackendLink().Down && s.postConnectSetupDone.Load()
}

// historyProbe is the chart the check reads to prove the history service:
// a week of SPY daily closes, which answers on a shut market as well as an
// open one. It is a market reference, never a holding.
var historyProbe = rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "1M"}

const historyProbeTimeout = 8 * time.Second

// checkHistorySource proves the history service when no chart read has done
// so within its observation window: after a start or reconnect, and after a
// quiet stretch with no chart open. Without it the source reads "Not
// checked" until a chart happens to be read, and status shows attention
// with nothing wrong (owner instruction 2026-11-21 14:05 CET). A read that
// fails or returns nothing is not repeated within the five-minute
// observation window, so a broken service is asked at most that often.
func (s *Server) checkHistorySource(ctx context.Context, fetch func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error)) {
	now := s.orderNow()
	if s.feedDataHealth(historySourceID, now).State != "unknown" {
		return
	}
	s.dataHealth.mu.Lock()
	recent := !s.dataHealth.historyProbedAt.IsZero() && now.Sub(s.dataHealth.historyProbedAt) < quoteHealthObservationValidity
	if !recent {
		s.dataHealth.historyProbedAt = now
	}
	s.dataHealth.mu.Unlock()
	if recent {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, historyProbeTimeout)
	defer cancel()
	_, _ = fetch(readCtx, historyProbe, 7, now) // the read records its own observation
}

func (s *Server) attachQuoteAccess(row *rpc.DataSourceHealth, contract rpc.ContractParams, c *ibkr.Connector) {
	if c == nil {
		return
	}
	route, _, _, err := normaliseStockQuoteContract(contract)
	if err != nil {
		return
	}
	key := ibkr.MarketDataKeyForContract(route)
	for _, access := range c.MarketDataAbsences() {
		if access.Key != key {
			continue
		}
		row.Access = &rpc.DataAccessObservation{Code: access.Code, Reason: rpc.MarketDataAccessReason(access.Code), ObservedAt: access.ObservedAt, RetryAt: access.RetryAt}
		row.NextAttempt = access.RetryAt
		row.Action = "Canary retries live access automatically when due"
	}
}
