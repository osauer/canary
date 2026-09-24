package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func (s *Server) observedDataHealth(id string, now time.Time) (rpc.DataSourceHealth, bool) {
	s.dataHealth.mu.Lock()
	o, ok := s.dataHealth.observations[id]
	s.dataHealth.mu.Unlock()
	if !ok {
		return rpc.DataSourceHealth{}, false
	}
	row := o.row
	if o.broker && (o.connector == nil || !o.connector.SessionCurrent(o.binding) || o.connector.BackendLink().Down) {
		row.State, row.Receiving = "unknown", "Previous session observation"
		row.Detail = "Current availability is unverified after a broker session change. Previous observations are diagnostic history."
		row.ProblemIDs = nil
		row.Action = "Awaiting current-session producer check"
		row.Availability, row.Usability = "unknown", ""
		row.UsabilityReason = ""
	} else if !row.ValidUntil.IsZero() && !now.Before(row.ValidUntil) {
		row.State, row.Receiving = "unknown", "Last observation expired"
		row.Detail = "No current producer observation. Historical data mode and failure do not establish current availability."
		row.ProblemIDs = nil
		row.Availability, row.Usability = "unknown", ""
		row.UsabilityReason = ""
	}
	var truncated bool
	row.History, truncated = retainDataHealthHistory(slices.Clone(row.History), now)
	row.HistoryTruncated = row.HistoryTruncated || truncated
	return row, true
}

// collectDataHealth reads producer caches only. It never calls a quote,
// calendar fetcher, market-event snapshot, or other acquiring RPC handler.
func (s *Server) collectDataHealth(now time.Time) ([]rpc.DataSourceHealth, string) {
	rows := make([]rpc.DataSourceHealth, 0, 40)
	scope := "unknown"
	c := s.gatewayConnector()
	connected := c != nil && c.IsReady() && !c.BackendLink().Down
	if connected {
		scope = "current"
	}
	connection := unknownDataSource("broker_connection", "Broker connection", "IBKR", "connection", "Broker-supplied data")
	connection.CheckedAt, connection.ValidUntil = now, now.Add(dataHealthObservationValidity)
	if connected {
		connection.State, connection.Receiving = "current", "Connected"
		connection.Availability = "available"
	} else {
		connection.State, connection.Receiving = "unavailable", "Broker unavailable"
		connection.Availability = "unavailable"
		connection.ProblemIDs = []string{"broker_connection"}
		connection.Action = "Canary reconnects automatically when the broker API is available"
	}
	rows = append(rows, connection)
	s.dataHealth.mu.Lock()
	historyUnavailable := s.dataHealth.historyUnavailable
	s.dataHealth.mu.Unlock()
	if historyUnavailable {
		row := unknownDataSource("diagnostic_history", "Source history", "Canary", "diagnostics", "Health chronology")
		row.Detail = "Diagnostic history could not be retained; current producer observations remain separate."
		rows = append(rows, row)
	}
	for id, name := range map[string]string{"account": "Account fields", "positions": "Position and valuation data"} {
		row, ok := s.observedDataHealth(id, now)
		if !ok {
			row = unknownDataSource(id, name, "IBKR", "account_data", "Portfolio display and assessments")
		}
		if row.Name == "" {
			row.Name, row.Provider, row.Kind = name, "IBKR", "account_data"
		}
		row.Required = true
		if !connected {
			row.DerivedFrom = []string{"broker_connection"}
		}
		rows = append(rows, row)
	}
	for _, id := range []string{quoteSourceID, historySourceID} {
		row := s.feedDataHealth(id, now)
		if !connected {
			row.State, row.Receiving = "unknown", "Broker connection unavailable"
			row.Availability = "unknown"
			row.ProblemIDs = nil
			row.DerivedFrom = []string{"broker_connection"}
		}
		rows = append(rows, row)
	}
	macroFailures := s.macroSourceFailures()
	for _, source := range s.handleMacroSnapshot().Sources {
		row := unknownDataSource("macro:"+source.ID, source.Name, "Official public source", "public_source", "Macro calendar and publications")
		row.CheckedAt, row.ReceivedAt, row.ValidUntil, row.NextAttempt = source.LastAttempt, source.LastSuccess, source.ValidUntil, source.NextAttempt
		row.LastSuccess = source.LastSuccess
		row.Availability = source.Availability
		row.Action = "Canary refreshes this source automatically"
		streak := ""
		switch {
		case source.LastAttempt.IsZero():
		case source.Availability != "available":
			row.State, row.Receiving = "unavailable", "Latest refresh failed"
			if !source.LastSuccess.IsZero() {
				row.State, row.Receiving = "limited", "Prior data · refresh failed"
			}
			row.ProblemIDs = []string{row.ID}
			// The cause is read separately from the snapshot; its onset must
			// match this streak or it belongs to a refresh that has since run.
			if failure, ok := macroFailures[source.ID]; ok && failure.FailedAt.Equal(source.FirstFailure) {
				row.Failure = &failure
				if !failure.Retryable {
					row.Action = "Canary keeps retrying, but this rejection needs a publisher or Canary change to clear"
				}
			}
			if source.ConsecutiveFailures == 1 {
				streak = "1 failed refresh"
			} else if source.ConsecutiveFailures > 1 {
				streak = fmt.Sprintf("%d consecutive failed refreshes", source.ConsecutiveFailures)
			}
		case source.Stale || source.ValidUntil.IsZero() || !now.Before(source.ValidUntil):
			row.State, row.Receiving, row.ProblemIDs = "limited", "Retained data · stale", []string{row.ID}
		default:
			row.State, row.Receiving = "current", "Current retained source"
		}
		// Both values are producer-authored public-source diagnostics. Bound
		// display copy without promoting provider prose into a typed cause.
		detail := source.Detail
		if len(detail) > 512 {
			detail = detail[:512]
		}
		row.Detail = strings.Join(compactNonEmptyStrings(detail, streak, source.Coverage), " · ")
		rows = append(rows, row)
	}
	rows = append(rows, s.calendarDataHealth(now)...)
	rows = append(rows, s.regimeDataHealth(now)...)
	for _, spec := range []struct{ id, name, provider string }{
		{"reg_sho_threshold", "Reg SHO threshold list", "Nasdaq"},
		{"trading_halts", "Trading halts", "Nasdaq"},
		{"borrow_fee", "Borrow fees", "IBKR"},
		{"borrow_inventory", "Shortable inventory", "IBKR"},
	} {
		id := "events:" + spec.id
		row, ok := s.observedDataHealth(id, now)
		if !ok {
			row = unknownDataSource(id, spec.name, spec.provider, "market_events", "Market-event assessment")
		}
		if row.Name == "" {
			row.Required = true
		}
		row.Name, row.Provider, row.Kind = spec.name, spec.provider, "market_events"
		rows = append(rows, row)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if report, err := s.handleReportingStatus(ctx); err == nil {
		row := unknownDataSource("reporting", "Broker statements", "IBKR Flex", "reporting", "Reconciliation", "Performance review")
		row.CheckedAt, row.ReceivedAt, row.SourceAt = report.Broker.LastAttempt, report.Broker.LastSuccess, report.Evidence.CoverageTo
		row.SourceTimeKind, row.NextAttempt, row.Action, row.Detail = "coverage_end", report.Broker.NextAttempt, report.Action, report.Reason
		switch report.State {
		case rpc.ReportingStateCurrent:
			row.State, row.Receiving = "current", "Current"
		case rpc.ReportingStateUnavailable, rpc.ReportingStateActionRequired:
			row.State, row.Receiving = "unavailable", "Action required"
		default:
			row.State, row.Receiving = "limited", "Coverage incomplete"
		}
		if row.State != "current" {
			row.ProblemIDs = []string{row.ID}
		}
		rows = append(rows, row)
	}
	// These owners expose retained snapshots rather than acquisition callbacks.
	// Record only their diagnostic state here; a report never requests a fetch.
	for i := range rows {
		row := &rows[i]
		if row.ID != "broker_connection" && row.ID != "reporting" && row.ID != "regime_publication" && !strings.HasPrefix(row.ID, "macro:") && !strings.HasPrefix(row.ID, "regime:") && !strings.HasPrefix(row.ID, "calendar:") {
			continue
		}
		if row.CheckedAt.IsZero() {
			row.CheckedAt = now
		}
		s.recordDataHealth(*row, nil, ibkr.ConnectorSessionBinding{}, false)
		s.dataHealth.mu.Lock()
		history := s.dataHealth.observations[row.ID].row
		row.FirstObserved, row.LastSuccess = history.FirstObserved, history.LastSuccess
		row.History, row.HistoryTruncated = slices.Clone(history.History), history.HistoryTruncated
		s.dataHealth.mu.Unlock()
	}
	return rows, scope
}

func (s *Server) regimeDataHealth(now time.Time) []rpc.DataSourceHealth {
	type indicator struct {
		id, name, provider string
		meta               rpc.RegimeIndicatorMeta
		status             string
		cadence            string
		gammaQuality       *rpc.GammaSignalQuality
	}
	specs := []indicator{
		{id: "vix_term", name: "Volatility term-structure data", provider: "IBKR"}, {id: "vvix", name: "Volatility-of-volatility daily series", provider: "Cboe"},
		{id: "hyg_spy", name: "Credit/equity comparison data", provider: "IBKR"}, {id: "credit", name: "Credit spreads", provider: "FRED"},
		{id: "funding", name: "Funding rates", provider: "Federal Reserve / New York Fed"}, {id: "fx", name: "Foreign-exchange data", provider: "IBKR"},
		{id: "gamma", name: "Option prices, Greeks and open interest", provider: "IBKR · Canary gamma"}, {id: "breadth", name: "S&P 500 membership and breadth", provider: "Canary breadth"},
	}
	publication := unknownDataSource("regime_publication", "Regime publication", "Canary", "analytics_publication", "Regime assessment")
	publicationCurrent := false
	if s.regimeSnapshots != nil {
		if view, err := s.regimeSnapshots.current(); err == nil && view.Snapshot != nil {
			publication.CheckedAt = now
			publication.ValidUntil = now.Add(dataHealthObservationValidity)
			publication.State, publication.Receiving = "current", "Published"
			publicationCurrent = view.Health.Status == rpc.RegimeAuthorityFresh && view.Health.FailureCode == rpc.RegimeAuthorityFailureNone
			if view.Health.LastSuccessAt != nil {
				publication.ReceivedAt, publication.LastSuccess = *view.Health.LastSuccessAt, *view.Health.LastSuccessAt
			}
			if view.Health.Status != rpc.RegimeAuthorityFresh || view.Health.FailureCode != rpc.RegimeAuthorityFailureNone {
				publication.State, publication.Receiving = "limited", "Prior publication · refresh outstanding"
				publication.Detail = string(view.Health.FailureCode)
				publication.ProblemIDs = []string{publication.ID}
			}
			publication.Action = "Managed by Canary's regime publication owner"
			r := view.Snapshot
			if r.GammaZero.Envelope.Result != nil {
				specs[6].gammaQuality = r.GammaZero.Envelope.Result.Quality
			}
			metas := []rpc.RegimeIndicatorMeta{r.VIXTermStructure.RegimeIndicatorMeta, r.VolOfVol.RegimeIndicatorMeta, r.HYGSPYDivergence.RegimeIndicatorMeta, r.CreditSpreads.RegimeIndicatorMeta, r.FundingStress.RegimeIndicatorMeta, r.USDJPY.RegimeIndicatorMeta, r.GammaZero.RegimeIndicatorMeta, r.Breadth.RegimeIndicatorMeta}
			statuses := []string{r.VIXTermStructure.Status, r.VolOfVol.Status, r.HYGSPYDivergence.Status, r.CreditSpreads.Status, r.FundingStress.Status, r.USDJPY.Status, r.GammaZero.Status, r.Breadth.Status}
			for i := range specs {
				specs[i].meta, specs[i].status = metas[i], statuses[i]
			}
			// Reuse the producer's pure schedule classifiers at read time. A
			// cached not_due verdict cannot remain valid after its venue opens.
			at := nyTime(now)
			specs[0].cadence = vixTermCadenceClass(r, at)
			specs[1].cadence = volOfVolCadenceClass(r, at)
			specs[5].cadence = usdJpyCadenceClass(r, at)
			specs[6].cadence = gammaCadenceClass(r, at)
			specs[7].cadence = breadthCadenceClass(r, at)
		}
	}
	rows := []rpc.DataSourceHealth{publication}
	for _, spec := range specs {
		row := unknownDataSource("regime:"+spec.id, spec.name, spec.provider, "analytics_input", "Regime assessment")
		m := spec.meta
		if m.AsOf != nil {
			row.SourceAt, row.SourceDate, row.Receiving = m.AsOf.Time, m.AsOf.Date, m.AsOf.Label
			row.SourceTimeKind = "producer_observation"
		}
		if m.Freshness != nil {
			class := m.Freshness.Class
			if spec.cadence != "" {
				class = spec.cadence
			}
			switch class {
			case rpc.RegimeFreshnessFresh:
				row.State = "current"
				if row.SourceAt.IsZero() || row.SourceAt.After(now) || m.Freshness.MaxAgeSeconds <= 0 {
					row.State = "unknown"
				} else if now.Sub(row.SourceAt) > time.Duration(m.Freshness.MaxAgeSeconds)*time.Second {
					row.State = "limited"
					row.Receiving = "Stale producer observation"
				}
			case rpc.RegimeFreshnessNotDue:
				row.State, row.Receiving = "not_due", "As scheduled"
				row.CadenceState = "not_due"
				if m.Freshness.NextDueAt != nil && now.Before(*m.Freshness.NextDueAt) {
					row.NextAttempt = *m.Freshness.NextDueAt
				}
			case rpc.RegimeFreshnessStale, rpc.RegimeFreshnessOverdue, rpc.RegimeFreshnessPending:
				row.State = "limited"
				if class == rpc.RegimeFreshnessOverdue {
					row.CadenceState = "overdue"
				}
				if class == rpc.RegimeFreshnessPending {
					row.CadenceState = "pending"
				}
			}
		}
		if spec.status == "unavailable" || spec.status == "error" {
			row.State, row.Receiving = "unavailable", "Unavailable"
			row.Availability = "unavailable"
		}
		if spec.id == "gamma" {
			row.Usability = "unknown"
			if q := spec.gammaQuality; q != nil {
				switch q.Rankability {
				case rpc.GammaRankabilityRankable:
					if publicationCurrent && row.State == "current" {
						row.Usability = "usable"
					}
				case rpc.GammaRankabilityContextOnly:
					row.Usability = "limited"
				case rpc.GammaRankabilityBlocked, rpc.GammaRankabilityUnavailable:
					row.Usability = "blocked"
				}
				if row.Usability == "blocked" || row.Usability == "limited" {
					row.UsabilityReason = "Gamma quality gates permit context only"
					if row.Usability == "blocked" {
						row.UsabilityReason = "Gamma quality gates block analytical use"
					}
					if row.State != "unavailable" {
						row.State = "limited"
					}
					row.Receiving = row.UsabilityReason
				}
			}
		}
		if row.State == "limited" || row.State == "unavailable" {
			row.ProblemIDs = []string{row.ID}
		}
		row.Action = "Managed by the existing analytics producer"
		rows = append(rows, row)
	}
	return rows
}

// Pages are immutable for their short retention window. Concurrent refreshes
// cannot join source rows from different revisions in a consumer's report.
func (s *Server) handleDataHealth(p rpc.DataHealthParams) (rpc.DataHealthResult, error) {
	now := s.orderNow()
	state := &s.dataHealth
	state.mu.Lock()
	var full rpc.DataHealthResult
	if p.Revision != "" {
		full = state.reports[p.Revision]
		if now.Sub(full.AsOf) > 2*time.Minute {
			delete(state.reports, p.Revision)
			full = rpc.DataHealthResult{}
		}
	} else if now.Sub(state.latest.AsOf) < 5*time.Second {
		full = state.latest
	}
	state.mu.Unlock()
	if full.Revision == "" {
		if p.Revision != "" {
			return rpc.DataHealthResult{}, errBadRequest("health revision expired; restart pagination")
		}
		rows, scope := s.collectDataHealth(now)
		var err error
		full, err = finalizeDataHealth(rows, scope, now, rpc.DataHealthParams{})
		if err != nil {
			return full, err
		}
		full.Sources, full.Offset, full.NextOffset, full.Complete = rows, 0, nil, true
		state.mu.Lock()
		if state.reports == nil {
			state.reports = map[string]rpc.DataHealthResult{}
		}
		for id, r := range state.reports {
			if now.Sub(r.AsOf) > 2*time.Minute {
				delete(state.reports, id)
			}
		}
		// A burst of readers cannot retain an unbounded number of snapshots.
		if len(state.reports) >= 32 {
			oldestID := ""
			for id, r := range state.reports {
				if oldestID == "" || r.AsOf.Before(state.reports[oldestID].AsOf) {
					oldestID = id
				}
			}
			delete(state.reports, oldestID)
		}
		check := state.checkResult
		if !check.StartedAt.IsZero() {
			full.Check = &check
		}
		state.reports[full.Revision], state.latest = full, full
		state.mu.Unlock()
	}
	page, err := finalizeDataHealth(slices.Clone(full.Sources), full.ScopeState, full.AsOf, p)
	if err != nil {
		return page, err
	}
	page.Check = full.Check
	return fitDataHealthPage(page)
}

func (s *Server) observeEventHealth(result rpc.MarketEventsResult, connector *ibkr.Connector, binding ibkr.ConnectorSessionBinding) {
	for _, health := range result.SourceHealth {
		row := projectSourceHealth("events:"+health.Source, strings.ReplaceAll(health.Source, "_", " "), "Canary market events", "market_events", health, result.AsOf, "Market-event assessment")
		for i := range row.DerivedFrom {
			row.DerivedFrom[i] = "events:" + row.DerivedFrom[i]
		}
		if health.MaxAgeSeconds > 0 {
			row.ValidUntil = result.AsOf.Add(time.Duration(health.MaxAgeSeconds) * time.Second)
			if health.Source == "borrow_inventory" && !health.AsOf.IsZero() {
				row.ValidUntil = health.AsOf.Add(time.Duration(health.MaxAgeSeconds) * time.Second)
			}
		}
		if health.Source == "borrow_inventory" {
			if !connector.SessionCurrent(binding) || connector.BackendLink().Down {
				row.State, row.Availability, row.Receiving = "unknown", "unknown", "Previous session observation"
				row.ProblemIDs = nil
			}
			s.recordDataHealth(row, connector, binding, true)
		} else {
			s.recordDataHealth(row, nil, ibkr.ConnectorSessionBinding{}, false)
		}
	}
}

func dataHealthParams(req *rpc.Request) (rpc.DataHealthParams, error) {
	var p rpc.DataHealthParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return p, errBadRequest("invalid health parameters")
		}
	}
	return p, nil
}

func authoritativeHealthVerdict(h *rpc.HealthResult) rpc.HealthVerdict {
	if h.GatewayPhase == rpc.GatewayPhaseConnecting || h.GatewayPhase == rpc.GatewayPhaseAPINotReady {
		return rpc.HealthVerdict{State: "STARTING", Reason: "Broker API handshake is incomplete"}
	}
	if h.GatewayPhase == rpc.GatewayPhasePortRejecting {
		return rpc.HealthVerdict{State: "OFFLINE", Reason: "Broker API port accepts connections and drops them before the handshake"}
	}
	if !h.Connected || h.GatewayPhase == rpc.GatewayPhaseBackendLinkDown {
		return rpc.HealthVerdict{State: "OFFLINE", Reason: "Broker data connection unavailable"}
	}
	if len(h.DataFarms) > 0 {
		return rpc.HealthVerdict{State: "ATTENTION", Reason: "Broker data farm issue"}
	}
	for _, sub := range h.Subsystems {
		if sub.Status == "unavailable" || sub.Status == "error" || sub.Status == "degraded" {
			return rpc.HealthVerdict{State: "ATTENTION", Reason: sub.Name + " requires attention"}
		}
	}
	if h.GatewayTLS != h.NegotiatedTLS {
		return rpc.HealthVerdict{State: "ATTENTION", Reason: "Broker TLS differs from the configured connection"}
	}
	if h.Members.Source != "" && h.Members.RefreshState != "" && h.Members.RefreshState != "healthy" {
		return rpc.HealthVerdict{State: "ATTENTION", Reason: "S&P 500 membership refresh requires attention"}
	}
	if h.Trading.Mode != "" && h.Trading.Mode != config.TradingModeDisabled && h.Trading.Blocked {
		return rpc.HealthVerdict{State: "ATTENTION", Reason: "Trading is blocked by the existing local gates"}
	}
	if h.DataHealth == nil {
		return rpc.HealthVerdict{State: "ATTENTION", Reason: "Data health is unverified"}
	}
	if h.DataHealth.Summary.State != "current" {
		reason := h.DataHealth.Summary.Label
		if len(h.DataHealth.Concerns) > 0 {
			reason = h.DataHealth.Concerns[0].Label + " · " + reason
		}
		return rpc.HealthVerdict{State: "ATTENTION", Reason: reason}
	}
	return rpc.HealthVerdict{State: "READY", Reason: h.DataHealth.Summary.Label}
}

// The display subscriber already owns these lines. Capture their actual ticks
// into independent observations before UI release; this starts no subscription.
func (s *Server) observeDisplayDataHealth(snapshot ibkr.DisplaySnapshot, holds []displayHold, c *ibkr.Connector) {
	if c == nil || !c.SessionCurrent(snapshot.Session) {
		return
	}
	now := s.orderNow()
	for _, hold := range holds {
		md := snapshot.Quotes[hold.cacheKey]
		if md == nil {
			continue
		}
		q := &rpc.Quote{Contract: displayContract(hold.item.contract), AsOf: now, DataType: marketDataTypeName(md.FeedType)}
		fillQuoteMarketData(q, md)
		switch {
		case q.Last != nil:
			q.Price, q.PriceSource = q.Last, "last"
		case q.Mark != nil:
			q.Price, q.PriceSource = q.Mark, "mark"
		case q.Bid != nil && q.Ask != nil:
			q.Price, q.PriceSource = new((*q.Bid+*q.Ask)/2), "midpoint"
		case q.PrevClose != nil:
			q.Price, q.PriceSource = q.PrevClose, "prev_close"
		default:
			continue
		}
		s.observeQuoteHealth(q.Contract, q, nil, c, snapshot.Session)
	}
}

func (s *Server) calendarDataHealth(now time.Time) []rpc.DataSourceHealth {
	var rows []rpc.DataSourceHealth
	for _, market := range []string{"us", "us-options", "de", "uk", "jp", "hk"} {
		row := unknownDataSource("calendar:"+market, market+" sessions", "Official exchange calendar", "calendar", "Session timing")
		raw, _ := json.Marshal(rpc.MarketCalendarParams{Market: market, At: now, Days: 7})
		result, err := s.handleMarketCalendar(&rpc.Request{Params: raw})
		row.CheckedAt = now
		if err == nil {
			row.Name = result.Label + " sessions"
			row.Detail = "Embedded official calendar · coverage " + result.CoverageStart + " to " + result.CoverageEnd
			row.State, row.Receiving = "current", "Covered sessions"
			for _, session := range result.Sessions {
				if session.State == "unknown" {
					row.State, row.Receiving = "unknown", "Session coverage unverified"
				}
			}
		}
		row.Action = "Calendar coverage is maintained in Canary"
		rows = append(rows, row)
	}
	return rows
}
