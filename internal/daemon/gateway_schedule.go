package daemon

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/marketcal"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The schedule holds only time intervals, never positions or account identity.
// One worker compiles embedded calendars; readers do bounded comparisons only.
type gatewayScheduleView struct {
	from, until, evidenceUntil time.Time
	unknown                    bool
	windows                    []marketcal.Window
}

func (v *gatewayScheduleView) required(from, until time.Time) bool {
	if v == nil || v.unknown || until.Before(from) || from.Before(v.from) || !until.Before(v.until) || !until.Before(v.evidenceUntil) {
		return true
	}
	for _, w := range v.windows {
		if until.Before(w.Open) {
			break
		}
		if from.Before(w.Close) {
			return true
		}
	}
	return false
}

type gatewaySchedule struct {
	view atomic.Pointer[gatewayScheduleView]
	wg   sync.WaitGroup
}

func (s *Server) gatewayLogRequired(from, until time.Time) bool {
	if s.cfg == nil || s.cfg.Daemon.LogCalendarMode != "scheduled" {
		return true
	}
	return s.gatewaySchedule.view.Load().required(from, until)
}

func compileGatewaySchedule(now time.Time, markets []marketcal.Market, before, after time.Duration, evidenceUntil time.Time, unknown bool) *gatewayScheduleView {
	v := &gatewayScheduleView{from: now.Add(-12 * time.Hour), until: now.Add(12 * time.Hour), evidenceUntil: evidenceUntil, unknown: unknown || len(markets) == 0}
	if v.unknown {
		return v
	}
	cal := marketcal.New()
	for _, market := range markets {
		res, err := cal.Query(marketcal.Query{Market: market, At: now.Add(-24 * time.Hour), Days: 4})
		if err != nil {
			v.unknown = true
			return v
		}
		for _, session := range res.Sessions {
			if session.State == marketcal.StateUnknown {
				v.unknown = true
				return v
			}
			if session.Open.IsZero() || session.Close.IsZero() {
				continue
			}
			// Keep lunch and auctions/preparation within one conservative duty span.
			v.windows = append(v.windows, marketcal.Window{Open: session.Open.Add(-before), Close: session.Close.Add(after)})
		}
	}
	slices.SortFunc(v.windows, func(a, b marketcal.Window) int { return a.Open.Compare(b.Open) })
	merged := v.windows[:0]
	for _, w := range v.windows {
		if len(merged) > 0 && !w.Open.After(merged[len(merged)-1].Close) {
			if w.Close.After(merged[len(merged)-1].Close) {
				merged[len(merged)-1].Close = w.Close
			}
		} else {
			merged = append(merged, w)
		}
	}
	v.windows = merged
	return v
}

// loggingPositionMarket deliberately has no currency, symbol or SMART fallback.
// Options and other products may have global sessions not modeled by cash calendars.
func loggingPositionMarket(c ibkrlib.Contract) (marketcal.Market, bool) {
	if c.SecType != "STK" {
		return "", false
	}
	venue := strings.ToUpper(strings.TrimSpace(c.PrimaryExch))
	if venue == "" {
		venue = strings.ToUpper(strings.TrimSpace(c.Exchange))
	}
	switch venue {
	case "NYSE", "NASDAQ", "ISLAND", "ARCA", "AMEX", "BATS", "IEX", "EDGEA", "EDGX":
		return marketcal.MarketUSEquity, true
	case "IBIS", "IBIS2", "XETRA":
		return marketcal.MarketDEXetra, true
	case "LSE", "LSEETF":
		return marketcal.MarketUKLSE, true
	case "TSEJ":
		return marketcal.MarketJPTSE, true
	case "SEHK":
		return marketcal.MarketHKHKEX, true
	default:
		return "", false
	}
}

// gatewayScopeEvidence retains same-scope outage context but never grants a
// successor connection the predecessor's completed-inventory evidence.
type gatewayScopeEvidence struct {
	connector *ibkrlib.Connector
	session   ibkrlib.ConnectorSessionBinding
	until     time.Time
}

func (e *gatewayScopeEvidence) observeConnector(c *ibkrlib.Connector) {
	if c != nil && c != e.connector {
		e.connector = c
		e.session = ibkrlib.ConnectorSessionBinding{}
		e.until = time.Time{}
	}
}
func (e *gatewayScopeEvidence) observeSession(session ibkrlib.ConnectorSessionBinding) {
	if session != e.session {
		e.session = session
		e.until = time.Time{}
	}
}
func (s *Server) publishGatewaySchedule(c *ibkrlib.Connector, epoch uint64, view *gatewayScheduleView) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connector != c || s.connectorEpoch != epoch {
		return false
	}
	s.gatewaySchedule.view.Store(view)
	return true
}

func (s *Server) startGatewaySchedule(ctx context.Context) {
	s.gatewaySchedule.wg.Go(func() {
		var cfg config.Daemon
		if s.cfg != nil {
			cfg = s.cfg.Daemon
		}
		markets := cfg.GatewayLogMarkets()
		always := len(markets) == 0
		if always {
			return
		}
		// The daemon's automatic gamma/breadth work requires both US calendars.
		for _, m := range []marketcal.Market{marketcal.MarketUSEquity, marketcal.MarketUSOptions} {
			if !slices.Contains(markets, m) {
				markets = append(markets, m)
			}
		}
		before, after := cfg.GatewayLogPadding()
		if cfg.LogCalendarMode != "scheduled" {
			// Conservative mode preserves incident warnings. Its baseline still
			// promotes repeated backend losses during Asian/other duty windows.
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for {
				now := s.gatewayLogClock()
				s.gatewaySchedule.view.Store(compileGatewaySchedule(now, markets, before, after, now.Add(12*time.Hour), false))
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}
		unsupported := false
		var evidence gatewayScopeEvidence
		var compiled *gatewayScheduleView
		var compiledAt time.Time
		previousCount, previousUnsupported := -1, false
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			now := s.gatewayLogClock()
			s.mu.Lock()
			c, epoch := s.connector, s.connectorEpoch
			s.mu.Unlock()
			evidence.observeConnector(c)
			if c != nil && !c.BackendLink().Down {
				if session, ok := c.CaptureSession(); ok {
					evidence.observeSession(session)
					if p, ok := c.CapturePortfolioProjectionForSession(session); ok {
						h := p.Health
						if !h.InitialCompletedAt.IsZero() && h.Account != "" && h.Account == c.AccountID() && h.ScopeConflictAt.IsZero() && h.InvalidPayloadAt.IsZero() {
							observed := h.LastUpdateAt
							if observed.Before(h.InitialCompletedAt) {
								observed = h.InitialCompletedAt
							}
							evidence.until = time.Time{}
							if !observed.After(now) {
								evidence.until = observed.Add(24 * time.Hour)
							}
							for _, row := range p.Positions {
								if row == nil {
									unsupported = true
									continue
								}
								if row.Position == 0 {
									continue
								}
								m, ok := loggingPositionMarket(row.Contract)
								if !ok {
									unsupported = true
								} else if !slices.Contains(markets, m) {
									markets = append(markets, m)
								}
							}
						} else {
							evidence.until = time.Time{}
						}
					}
				}
			}
			// Reuse existing API-order evidence. It can widen scope but cannot prove
			// manual TWS orders absent. Never request orders solely for diagnostics.
			s.protectionOrderSnapshotMu.Lock()
			for _, order := range s.protectionOrderSnapshotCache.snapshot.Orders {
				if order.WhatIf {
					continue
				}
				m, ok := loggingPositionMarket(ibkrlib.Contract{SecType: order.SecType, Exchange: order.Exchange})
				if order.OutsideRth || !ok {
					unsupported = true
				} else if !slices.Contains(markets, m) {
					markets = append(markets, m)
				}
			}
			s.protectionOrderSnapshotMu.Unlock()
			if compiled == nil || now.Before(compiledAt) || now.Sub(compiledAt) >= time.Hour || previousCount != len(markets) || previousUnsupported != unsupported {
				compiled = compileGatewaySchedule(now, markets, before, after, evidence.until, unsupported)
				compiledAt, previousCount, previousUnsupported = now, len(markets), unsupported
			}
			view := *compiled
			view.evidenceUntil = evidence.until
			s.publishGatewaySchedule(c, epoch, &view)
			if c != nil {
				c.CheckBackendLogRelevance(now)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}
