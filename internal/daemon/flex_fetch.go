package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Daily IBKR Flex statement ingestion (internal-docs/design/post-trade-truth.md).
// Read-only toward the broker. The Flex token is read from its own 0600
// file at fetch time and must never appear in any error, log line, journal,
// or redirect. IBKR's documented GET contract necessarily places it in the
// encrypted request URL, so the client refuses redirects and never reports
// request URLs in errors.

const (
	flexSendRequestURL    = "https://ndcdyn.interactivebrokers.com/AccountManagement/FlexWebService/SendRequest"
	flexGetStatementURL   = "https://ndcdyn.interactivebrokers.com/AccountManagement/FlexWebService/GetStatement"
	flexUserAgent         = "canary-flex-reconciliation/1"
	flexStatementsDir     = "statements"
	flexFetchStateVersion = 2
	flexFetchStateKind    = "flex_fetch"
	flexFetchProjecting   = "projecting"
	flexScheduleZone      = "Europe/Berlin"
	flexReportingZone     = "America/New_York"
	// IBKR says securities statements are available around midnight
	flexMorningHour   = 6
	flexMorningMinute = 30
	flexPollInterval  = 10 * time.Second
	flexPollAttempts  = 30
	flexHTTPTimeout   = 30 * time.Second
	// One SendRequest plus every documented GetStatement attempt may each
	// consume the HTTP timeout. Keep the outer budget larger than that exact
	flexFetchTimeout     = (flexPollAttempts+1)*flexHTTPTimeout + (flexPollAttempts-1)*flexPollInterval + time.Minute
	flexRetryAfterFail   = 30 * time.Minute
	flexManualRetryFloor = time.Minute
	flexCheckInterval    = 5 * time.Minute
)

type flexFetchStateV1 struct {
	Version            int       `json:"version"`
	Stage              string    `json:"stage,omitempty"`
	LastAttempt        time.Time `json:"last_attempt,omitzero"`
	LastSuccess        time.Time `json:"last_success,omitzero"`
	LastReason         string    `json:"last_reason,omitempty"`
	LastRetryable      bool      `json:"last_retryable,omitempty"`
	ExpectedCoverageTo time.Time `json:"expected_coverage_to,omitzero"`
	CoverageTo         time.Time `json:"coverage_to,omitzero"`
	NextAttempt        time.Time `json:"next_attempt,omitzero"`
}

type flexFetchStateV2 struct {
	Version          int       `json:"version"`
	QueryFingerprint string    `json:"query_fingerprint,omitempty"`
	Stage            string    `json:"stage,omitempty"`
	LastAttempt      time.Time `json:"last_attempt,omitzero"`
	LastSuccess      time.Time `json:"last_success,omitzero"`
	LastReason       string    `json:"last_reason,omitempty"`
	LastBrokerCode   string    `json:"last_broker_code,omitempty"`
	LastRetryable    bool      `json:"last_retryable,omitempty"`
	// TargetDate is the latest completed New York reporting date whose one
	// daily broker check is being attempted. CoverageTo is deliberately
	// separate because IBKR may return a shorter range while generation runs.
	TargetDate  time.Time `json:"target_date,omitzero"`
	CoverageTo  time.Time `json:"coverage_to,omitzero"`
	NextAttempt time.Time `json:"next_attempt,omitzero"`
}

type flexFetchState struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	state    flexFetchStateV2
	busy     bool
	done     chan struct{}
	cancel   context.CancelFunc
	stopping bool
	wg       sync.WaitGroup
}

type flexFetchOutcome struct {
	Path          string
	CoverageFrom  time.Time
	CoverageTo    time.Time
	WhenGenerated time.Time
}

type flexFetchFailure struct {
	reason     string
	brokerCode string // exact four ASCII digits from a verified service envelope
	retryable  bool
	detail     string // local log only; must already be redacted
}

func (e *flexFetchFailure) Error() string {
	if e == nil || e.detail == "" {
		return "Flex report check failed"
	}
	return e.detail
}

type flexServiceEnvelope struct {
	XMLName       xml.Name `xml:"FlexStatementResponse"`
	Status        string   `xml:"Status"`
	ReferenceCode string   `xml:"ReferenceCode"`
	URL           string   `xml:"Url"`
	ErrorCode     string   `xml:"ErrorCode"`
	ErrorMessage  string   `xml:"ErrorMessage"`
}

func flexStatementsDirPath() (string, error) {
	return defaultTradingStatePath(flexStatementsDir)
}

func (st *flexFetchState) bindCore(ctx context.Context, core *corestore.Store) error {
	if st == nil || core == nil {
		return fmt.Errorf("flex fetch state SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, flexFetchStateKind)
	if err != nil {
		return fmt.Errorf("load Flex fetch state: %w", err)
	}
	state := flexFetchStateV2{Version: flexFetchStateVersion}
	migrated := false
	if ok {
		var header struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(doc.JSON, &header); err != nil {
			return fmt.Errorf("decode Flex fetch state: %w", err)
		}
		switch header.Version {
		case 1:
			var legacy flexFetchStateV1
			if err := json.Unmarshal(doc.JSON, &legacy); err != nil {
				return fmt.Errorf("decode Flex fetch state v1: %w", err)
			}
			state = flexFetchStateV2{
				Version: flexFetchStateVersion, Stage: legacy.Stage,
				LastAttempt: legacy.LastAttempt, LastSuccess: legacy.LastSuccess,
				LastReason: legacy.LastReason, LastRetryable: legacy.LastRetryable,
				CoverageTo: legacy.CoverageTo, NextAttempt: legacy.NextAttempt,
			}
			targetSource := legacy.LastAttempt
			if targetSource.IsZero() {
				targetSource = legacy.LastSuccess
			}
			if !targetSource.IsZero() {
				state.TargetDate, _ = flexDailyWindow(targetSource)
			}
			migrated = true
		case flexFetchStateVersion:
			if err := json.Unmarshal(doc.JSON, &state); err != nil {
				return fmt.Errorf("decode Flex fetch state v2: %w", err)
			}
		default:
			return fmt.Errorf("decode Flex fetch state: unsupported version %d", header.Version)
		}
	} else {
		raw, _ := json.Marshal(state)
		doc, err = core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: flexFetchStateKind, JSON: raw,
		})
		if err != nil {
			return fmt.Errorf("initialize Flex fetch state: %w", err)
		}
	}
	if normalized := normalizeFlexBrokerCode(state.LastBrokerCode); normalized != state.LastBrokerCode {
		state.LastBrokerCode = normalized
		migrated = true
	}
	if state.QueryFingerprint != "" && !validFlexQueryFingerprint(state.QueryFingerprint) {
		return fmt.Errorf("decode Flex fetch state: invalid query fingerprint")
	}
	// A daemon that stopped mid-request cannot still be checking after a
	// restart. Recover it as an automatic retry without trusting the former
	// process's unfinished stage.
	recoveredProjecting := state.Stage == flexFetchProjecting
	recoveredInterrupted := state.Stage == rpc.ReconReportStateChecking || recoveredProjecting
	if recoveredInterrupted {
		state.Stage = rpc.ReconReportStateRetryScheduled
		state.LastReason = rpc.ReconReportReasonNetworkUnavailable
		state.LastBrokerCode = ""
		if recoveredProjecting || retainedFlexEvidenceSince(state.LastAttempt) {
			state.LastReason = rpc.ReconReportReasonProjectionFailed
		}
		state.LastRetryable = true
		state.NextAttempt = state.LastAttempt.Add(flexRetryAfterFail)
		if state.LastAttempt.IsZero() {
			state.NextAttempt = time.Now().UTC()
		}
	}
	st.mu.Lock()
	st.core, st.revision, st.state = core, doc.Revision, state
	st.busy, st.done, st.cancel, st.stopping = false, nil, nil, false
	if recoveredInterrupted || migrated {
		if err := st.persistLocked(ctx); err != nil {
			st.mu.Unlock()
			return fmt.Errorf("persist migrated Flex fetch state: %w", err)
		}
	}
	st.mu.Unlock()
	return nil
}

func (st *flexFetchState) persistLocked(ctx context.Context) error {
	if st.core == nil {
		return fmt.Errorf("flex fetch state authority is unavailable")
	}
	st.state.Version = flexFetchStateVersion
	st.state.LastBrokerCode = normalizeFlexBrokerCode(st.state.LastBrokerCode)
	raw, err := json.Marshal(st.state)
	if err != nil {
		return err
	}
	saved, err := st.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: flexFetchStateKind,
		ExpectedRevision: st.revision, JSON: raw,
	})
	if err != nil {
		return err
	}
	st.revision = saved.Revision
	return nil
}

func (st *flexFetchState) stopAndWait() {
	if st == nil {
		return
	}
	st.mu.Lock()
	st.stopping = true
	cancel := st.cancel
	st.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	st.wg.Wait()
}

func (st *flexFetchState) isBusy() bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.busy
}

// runFlexFetchLoop checks a once-per-Berlin-day morning schedule. Durable attempt
func (s *Server) runFlexFetchLoop(ctx context.Context) {
	if s == nil || s.cfg == nil || !s.cfg.Flex.Enabled {
		return
	}
	t := time.NewTicker(flexCheckInterval)
	defer t.Stop()
	for {
		s.maybeFetchFlex(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) maybeFetchFlex(ctx context.Context) {
	now := time.Now()
	if s != nil && s.now != nil {
		now = s.now()
	}
	status := s.flexFetchStatusAt(now)
	if status.State == rpc.ReconReportStateDue {
		s.startFlexFetch(ctx, false)
	}
}

// kickFlexFetch requests one user-initiated check. It remains single-flight
// and enforces a short local cooldown independently of IBKR's pacing limit.
func (s *Server) kickFlexFetch(ctx context.Context) bool {
	return s.startFlexFetch(ctx, true)
}

// maybeFetchFlexForLatch schedules bounded statement rechecks while a
// drawdown latch waits for the report covering its day. IBKR Flex is daily
// statement truth, not intraday cash-flow telemetry, so attempts back off —
// half-hourly at first, then two-hourly, then six-hourly — until coverage
// reaches the latch day and the provisional latch can be decided. Failure
// and pacing schedules recorded by the fetch state (IBKR error 1018 among
// them) are honored, and startFlexFetch keeps the manual-retry floor.
func (s *Server) maybeFetchFlexForLatch(ctx context.Context, latchedAt time.Time) bool {
	if s == nil || latchedAt.IsZero() {
		return false
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	status := s.flexFetchStatusAt(now)
	if !status.CoverageTo.IsZero() {
		coverageDay := status.CoverageTo.In(time.Local).Format(time.DateOnly)
		latchDay := latchedAt.In(time.Local).Format(time.DateOnly)
		if coverageDay >= latchDay {
			return false
		}
	}
	if status.Busy || (!status.NextAttempt.IsZero() && now.Before(status.NextAttempt)) {
		return false
	}
	if !status.LastAttempt.IsZero() && !status.LastAttempt.Before(latchedAt.UTC()) &&
		now.Before(status.LastAttempt.Add(latchFlexRecheckInterval(now.Sub(latchedAt)))) {
		return false
	}
	return s.startFlexFetch(ctx, true)
}

// latchFlexRecheckInterval widens the post-latch recheck spacing as the
// latch ages; the ordinary morning schedule keeps running regardless.
func latchFlexRecheckInterval(age time.Duration) time.Duration {
	switch {
	case age <= 2*time.Hour:
		return 30 * time.Minute
	case age <= 24*time.Hour:
		return 2 * time.Hour
	default:
		return 6 * time.Hour
	}
}

func (s *Server) startFlexFetch(ctx context.Context, manual bool) bool {
	if s == nil || s.cfg == nil || !s.cfg.Flex.Enabled || strings.TrimSpace(s.cfg.Flex.QueryID) == "" {
		return false
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	targetDate, firstAttempt := flexDailyWindow(now)
	if now.Before(firstAttempt) {
		return false
	}
	parent := context.WithoutCancel(ctx)
	s.mu.Lock()
	if s.serverCtx != nil {
		parent = s.serverCtx
	}
	s.mu.Unlock()
	operationCtx, operationCancel := context.WithTimeout(parent, flexFetchTimeout)
	selection := s.flexEvidenceSelection()
	st := &s.flexFetch
	st.mu.Lock()
	if st.stopping || st.busy {
		st.mu.Unlock()
		operationCancel()
		return false
	}
	if !flexFetchStateMatchesSelection(st.state, selection) {
		st.state = flexFetchStateV2{Version: flexFetchStateVersion, QueryFingerprint: selection.ActiveQueryFingerprint}
	}
	if manual && !st.state.LastAttempt.IsZero() && now.Sub(st.state.LastAttempt) < flexManualRetryFloor {
		st.mu.Unlock()
		operationCancel()
		return false
	}
	failedTarget := st.state.TargetDate
	st.busy = true
	st.done = make(chan struct{})
	st.cancel = operationCancel
	st.state.Stage = rpc.ReconReportStateChecking
	st.state.QueryFingerprint = selection.ActiveQueryFingerprint
	st.state.LastAttempt = now.UTC()
	st.state.TargetDate = targetDate
	st.state.LastBrokerCode = ""
	st.state.NextAttempt = time.Time{}
	if err := st.persistLocked(context.WithoutCancel(ctx)); err != nil {
		st.state.Stage = rpc.ReconReportStateUnavailable
		st.state.LastReason = rpc.ReconReportReasonAuthorityUnavailable
		st.state.LastBrokerCode = ""
		st.state.LastRetryable = false
		st.state.NextAttempt = time.Time{}
		st.busy = false
		close(st.done)
		st.done = nil
		st.cancel = nil
		st.mu.Unlock()
		operationCancel()
		s.warnf("Flex fetch state start failed: %v", err)
		return false
	}
	st.wg.Add(1)
	st.mu.Unlock()
	go s.runFlexFetch(operationCtx, operationCancel, targetDate, failedTarget)
	return true
}

func (s *Server) runFlexFetch(ctx context.Context, operationCancel context.CancelFunc, targetDate, failedTarget time.Time) {
	st := &s.flexFetch
	defer st.wg.Done()
	defer operationCancel()

	coverage, _, evidenceOK := s.latestFlexEvidence(ctx)
	var outcome flexFetchOutcome
	var err error
	// Projection failures are retried locally from retained broker evidence on
	// the same daily target; do not redownload a report that was already saved.
	st.mu.Lock()
	localRetry := st.state.LastReason == rpc.ReconReportReasonProjectionFailed &&
		failedTarget.Equal(targetDate) && evidenceOK
	st.mu.Unlock()
	if !localRetry {
		if s.flexFetchOnceFn != nil {
			outcome, err = s.flexFetchOnceFn(ctx, targetDate)
		} else {
			outcome, err = s.fetchFlexOnce(ctx, targetDate)
		}
		if err == nil {
			coverage = outcome.CoverageTo
			s.infof("Flex statement ingested: %s", filepath.Base(outcome.Path))
			st.mu.Lock()
			st.state.Stage = flexFetchProjecting
			st.state.CoverageTo = coverage.UTC()
			persistErr := st.persistLocked(context.WithoutCancel(ctx))
			st.mu.Unlock()
			if persistErr != nil {
				err = &flexFetchFailure{reason: rpc.ReconReportReasonAuthorityUnavailable, detail: "Flex fetch progress could not be retained"}
			}
		}
	}
	if err == nil {
		projectionCtx, projectionCancel := context.WithTimeout(ctx, flexHTTPTimeout)
		if s.flexProjectionFn != nil {
			err = s.flexProjectionFn(projectionCtx)
		} else {
			err = s.refreshStatementProjection(projectionCtx)
		}
		projectionCancel()
		if err != nil {
			err = &flexFetchFailure{reason: rpc.ReconReportReasonProjectionFailed, retryable: true, detail: "statement projection refresh failed"}
		}
	}
	if err == nil {
		s.evaluateRiskPolicyV3Reconciliation()
		s.kickEdgeRebuild()
	}

	finished := time.Now()
	if s.now != nil {
		finished = s.now()
	}
	st.mu.Lock()
	if err != nil {
		reason, retryable := flexFailureStatus(err)
		st.state.Stage = rpc.ReconReportStateRetryScheduled
		if !retryable {
			st.state.Stage = rpc.ReconReportStateActionRequired
		}
		st.state.LastReason, st.state.LastBrokerCode, st.state.LastRetryable = reason, flexFailureBrokerCode(err), retryable
		if retryable {
			st.state.NextAttempt = finished.UTC().Add(flexRetryAfterFail)
		} else {
			st.state.NextAttempt = time.Time{}
		}
		s.infof("Flex report check failed: %s", reason)
	} else {
		st.state.Stage = rpc.ReconReportStateCurrent
		st.state.LastReason, st.state.LastBrokerCode, st.state.LastRetryable = "", "", false
		st.state.LastSuccess = finished.UTC()
		st.state.CoverageTo = coverage.UTC()
		st.state.NextAttempt = time.Time{}
	}
	if persistErr := st.persistLocked(context.Background()); persistErr != nil {
		st.state.Stage = rpc.ReconReportStateUnavailable
		st.state.LastReason = rpc.ReconReportReasonAuthorityUnavailable
		st.state.LastBrokerCode = ""
		st.state.LastRetryable = false
		st.state.NextAttempt = time.Time{}
		s.warnf("Flex fetch state completion failed: %v", persistErr)
	}
	st.busy = false
	st.cancel = nil
	if st.done != nil {
		close(st.done)
		st.done = nil
	}
	st.mu.Unlock()
}

// fetchFlexOnce runs the two-step Flex protocol: SendRequest returns a
// reference code, GetStatement is polled until the report is generated.
// The saved raw file is validated through the parser before retention so a
// service envelope can never sit in the statements dir pretending to be a
// week with no activity.
func (s *Server) fetchFlexOnce(ctx context.Context, target time.Time) (flexFetchOutcome, error) {
	to := dateOnlyUTC(target)
	if to.IsZero() {
		to = latestCompletedFlexDate(time.Now())
		if s != nil && s.now != nil {
			to = latestCompletedFlexDate(s.now())
		}
	}
	return s.fetchFlexDateRange(ctx, to.AddDate(0, 0, -(edgeDailyLookbackDays-1)), to)
}

// fetchFlexDateRange requests one explicit inclusive window. The caller owns
// scheduling; this method owns the single broker lane and the fixed endpoint.
func (s *Server) fetchFlexDateRange(ctx context.Context, from, to time.Time) (flexFetchOutcome, error) {
	return s.fetchFlexDateRangeWithPollAttempts(ctx, from, to, flexPollAttempts)
}

func (s *Server) fetchFlexDateRangeWithPollAttempts(ctx context.Context, from, to time.Time, pollAttempts int) (flexFetchOutcome, error) {
	from, to = dateOnlyUTC(from), dateOnlyUTC(to)
	if from.IsZero() || to.IsZero() || from.After(to) || int(to.Sub(from)/(24*time.Hour))+1 > 365 || pollAttempts < 1 {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonQueryInvalid, detail: "Flex date range is invalid"}
	}
	s.flexBrokerMu.Lock()
	defer s.flexBrokerMu.Unlock()
	cfg := s.cfg.Flex
	queryID := strings.TrimSpace(cfg.QueryID)
	if queryID == "" {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonQueryMissing, detail: "Flex query id is not configured"}
	}
	var (
		raw []byte
		err error
	)
	if s.flexRawDateRangeLockedFn != nil {
		raw, err = s.flexRawDateRangeLockedFn(ctx, from, to, pollAttempts, queryID, cfg.TokenPath)
	} else {
		raw, err = fetchFlexRawDateRangeWithCredentialsLocked(ctx, from, to, pollAttempts, queryID, cfg.TokenPath)
	}
	if err != nil {
		return flexFetchOutcome{}, err
	}
	if s.flexRetainRawFn != nil {
		return s.flexRetainRawFn(raw)
	}
	return s.retainFlexStatementForQuery(ctx, raw, flexQueryFingerprint(queryID))
}

// fetchFlexRawDateRangeWithCredentialsLocked performs only the broker
// protocol and returns one complete report in memory. The caller must hold
// flexBrokerMu through its consumer-specific parse/retain step so active and
// candidate work cannot interleave on IBKR's shared pacing lane.
func fetchFlexRawDateRangeWithCredentialsLocked(ctx context.Context, from, to time.Time, pollAttempts int, queryID, tokenPath string) ([]byte, error) {
	tokenBytes, err := os.ReadFile(expandUserPath(tokenPath))
	if err != nil {
		return nil, &flexFetchFailure{reason: rpc.ReconReportReasonTokenMissing, detail: "Flex token is unavailable"}
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return nil, &flexFetchFailure{reason: rpc.ReconReportReasonTokenMissing, detail: "Flex token is unavailable"}
	}
	client := &http.Client{Timeout: flexHTTPTimeout}

	env, err := flexServiceCall(ctx, client, flexSendRequestURL, url.Values{
		"t": {token}, "q": {queryID}, "v": {"3"},
		"fd": {from.Format("20060102")}, "td": {to.Format("20060102")},
	})
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(env.Status, "Success") || strings.TrimSpace(env.ReferenceCode) == "" {
		return nil, flexEnvelopeFailure(env.ErrorCode)
	}

	var raw []byte
	for attempt := range pollAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, &flexFetchFailure{reason: rpc.ReconReportReasonNetworkUnavailable, retryable: true, detail: "Flex report check timed out"}
			case <-time.After(flexPollInterval):
			}
		}
		// The response URL is legacy and untrusted. Always use the fixed IBKR
		// endpoint rather than following a broker-authored arbitrary host.
		body, err := flexRawCall(ctx, client, flexGetStatementURL, url.Values{"t": {token}, "q": {env.ReferenceCode}, "v": {"3"}})
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(body), "<FlexStatementResponse") {
			var progress flexServiceEnvelope
			if xml.Unmarshal(body, &progress) == nil && progress.ErrorCode == "1019" {
				continue // statement generation in progress
			}
			var code string
			if xml.Unmarshal(body, &progress) == nil {
				code = progress.ErrorCode
			}
			return nil, flexEnvelopeFailure(code)
		}
		raw = body
		break
	}
	if raw == nil {
		return nil, &flexFetchFailure{reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "Flex report is still being generated"}
	}
	return raw, nil
}

func dateOnlyUTC(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

// retainFlexStatement accepts every complete, typed broker report, even when
// its coverage date or generation timestamp has not advanced. IBKR activity
// IBKR keeps the same generation timestamp. Exact bytes reuse retained
// query result. Strictly older broker generations remain rejected.
func retainFlexStatement(raw []byte) (flexFetchOutcome, error) {
	return retainFlexStatementSelected(context.Background(), raw, flexEvidenceSelection{IncludeAll: true})
}

func (s *Server) retainFlexStatementForQuery(ctx context.Context, raw []byte, queryFingerprint string) (flexFetchOutcome, error) {
	selection := s.flexEvidenceSelection()
	if !validFlexQueryFingerprint(queryFingerprint) || selection.ActiveQueryFingerprint != queryFingerprint {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonAuthorityUnavailable, retryable: true, detail: "Flex query generation changed during retention"}
	}
	return retainFlexStatementSelected(ctx, raw, selection)
}

func retainFlexStatementSelected(ctx context.Context, raw []byte, selection flexEvidenceSelection) (flexFetchOutcome, error) {
	statements, err := flexstmt.Parse(raw)
	if err != nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonReportInvalid, retryable: true, detail: "Flex report did not match the expected format"}
	}
	var coverageFrom, coverageTo, generated time.Time
	for _, statement := range statements {
		// Use the intersection across returned account statements. It is the
		// range the response proves for every included account, rather than a
		// min/max envelope that could hide a gap.
		if coverageFrom.IsZero() || statement.FromDate.After(coverageFrom) {
			coverageFrom = statement.FromDate
		}
		if coverageTo.IsZero() || statement.ToDate.Before(coverageTo) {
			coverageTo = statement.ToDate
		}
		if statement.WhenGenerated.After(generated) {
			generated = statement.WhenGenerated
		}
	}
	if coverageFrom.IsZero() || coverageTo.IsZero() || coverageFrom.After(coverageTo) || generated.IsZero() {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonReportInvalid, retryable: true, detail: "Flex report did not carry a coverage date"}
	}
	_, latestGenerated, evidenceOK := latestFlexEvidenceSelected(ctx, selection)
	if evidenceOK && generated.Before(latestGenerated) {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "IBKR returned an older report generation"}
	}
	dir, err := flexStatementsDirPath()
	if err != nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonStorageFailed, retryable: true, detail: "Flex report storage is unavailable"}
	}
	if retainedPath, duplicate, err := findRetainedFlexReportSelected(dir, raw, selection); err != nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonStorageFailed, retryable: true, detail: "retained Flex reports could not be verified"}
	} else if duplicate {
		return flexFetchOutcome{Path: retainedPath, CoverageFrom: coverageFrom, CoverageTo: coverageTo, WhenGenerated: generated}, nil
	}
	digest := sha256.Sum256(raw)
	prefix := "flex-"
	if validFlexQueryFingerprint(selection.ActiveQueryFingerprint) {
		prefix += selection.ActiveQueryFingerprint + "-"
	}
	path := filepath.Join(dir, fmt.Sprintf("%s%s-%x.xml", prefix, time.Now().UTC().Format("20060102-150405.000000000"), digest[:6]))
	if err := writePrivateStateAtomic(path, raw); err != nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonStorageFailed, retryable: true, detail: "Flex report could not be retained"}
	}
	return flexFetchOutcome{Path: path, CoverageFrom: coverageFrom, CoverageTo: coverageTo, WhenGenerated: generated}, nil
}

func findRetainedFlexReportSelected(dir string, raw []byte, selection flexEvidenceSelection) (string, bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".xml") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return "", false, fmt.Errorf("retained report is a symlink")
		}
		included, err := selection.includesRetainedFlexFile(entry.Name())
		if err != nil {
			return "", false, err
		}
		if !included {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return "", false, err
		}
		if bytes.Equal(data, raw) {
			return path, true, nil
		}
	}
	return "", false, nil
}

func retainedFlexEvidenceSince(at time.Time) bool {
	if at.IsZero() {
		return false
	}
	dir, err := flexStatementsDirPath()
	if err != nil {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".xml") || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(at.Add(-time.Second)) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if statements, err := flexstmt.Parse(data); err == nil && len(statements) > 0 {
			return true
		}
	}
	return false
}

// flexServiceCall performs one envelope-returning call. IBKR requires query
// parameters on a GET; flexRawCall refuses redirects and errors never include
// the request URL or parameters.
func flexServiceCall(ctx context.Context, client *http.Client, endpoint string, params url.Values) (*flexServiceEnvelope, error) {
	body, err := flexRawCall(ctx, client, endpoint, params)
	if err != nil {
		return nil, err
	}
	var env flexServiceEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		return nil, &flexFetchFailure{reason: rpc.ReconReportReasonResponseInvalid, retryable: true, detail: "IBKR returned an unrecognized Flex response"}
	}
	return &env, nil
}

func flexRawCall(ctx context.Context, client *http.Client, endpoint string, params url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return nil, &flexFetchFailure{reason: rpc.ReconReportReasonResponseInvalid, retryable: true, detail: "Flex request could not be built"}
	}
	req.Header.Set("User-Agent", flexUserAgent)
	if client == nil {
		client = &http.Client{}
	}
	redirectSafe := *client
	redirectSafe.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := redirectSafe.Do(req)
	if err != nil {
		return nil, &flexFetchFailure{reason: rpc.ReconReportReasonNetworkUnavailable, retryable: true, detail: "IBKR Flex service could not be reached"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		reason := rpc.ReconReportReasonNetworkUnavailable
		if resp.StatusCode == http.StatusTooManyRequests {
			reason = rpc.ReconReportReasonRateLimited
		}
		return nil, &flexFetchFailure{reason: reason, retryable: true, detail: fmt.Sprintf("IBKR Flex service returned HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &flexFetchFailure{reason: rpc.ReconReportReasonNetworkUnavailable, retryable: true, detail: "IBKR Flex response could not be read"}
	}
	return body, nil
}

func flexEnvelopeFailure(code string) error {
	brokerCode := normalizeFlexBrokerCode(code)
	failure := func(reason string, retryable bool, detail string) error {
		return &flexFetchFailure{reason: reason, brokerCode: brokerCode, retryable: retryable, detail: detail}
	}
	switch brokerCode {
	case "1001", "1003", "1004", "1005", "1006", "1007", "1008":
		return failure(rpc.ReconReportReasonReportNotReady, true, "IBKR has not published the complete report yet")
	case "1009", "1019":
		return failure(rpc.ReconReportReasonServiceBusy, true, "IBKR is still preparing the report")
	case "1018":
		return failure(rpc.ReconReportReasonRateLimited, true, "IBKR Flex request limit was reached")
	case "1010":
		return failure(rpc.ReconReportReasonQueryInvalid, false, "IBKR Flex query type is no longer supported")
	case "1011":
		return failure(rpc.ReconReportReasonServiceInactive, false, "IBKR Flex Web Service is inactive")
	case "1012":
		return failure(rpc.ReconReportReasonTokenExpired, false, "IBKR Flex token has expired")
	case "1013":
		return failure(rpc.ReconReportReasonIPRestricted, false, "IBKR Flex token does not allow this network")
	case "1014", "1016":
		return failure(rpc.ReconReportReasonQueryInvalid, false, "IBKR Flex query is not valid for this account")
	case "1015":
		return failure(rpc.ReconReportReasonTokenInvalid, false, "IBKR Flex token is invalid")
	case "1017":
		return failure(rpc.ReconReportReasonResponseInvalid, true, "IBKR rejected the Flex reference code")
	case "1020":
		return failure(rpc.ReconReportReasonResponseInvalid, true, "IBKR could not validate the Flex request")
	case "1021":
		return failure(rpc.ReconReportReasonReportNotReady, true, "IBKR could not retrieve the Flex report yet")
	case "1025":
		return failure(rpc.ReconReportReasonResponseInvalid, true, "IBKR returned an undocumented Flex response")
	default:
		return failure(rpc.ReconReportReasonResponseInvalid, true, "IBKR returned an unrecognized Flex status")
	}
}

func normalizeFlexBrokerCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) != 4 {
		return ""
	}
	for i := range len(code) {
		if code[i] < '0' || code[i] > '9' {
			return ""
		}
	}
	return code
}

func flexFailureStatus(err error) (string, bool) {
	if failure, ok := err.(*flexFetchFailure); ok && failure != nil {
		return failure.reason, failure.retryable
	}
	return rpc.ReconReportReasonResponseInvalid, true
}

func flexFailureBrokerCode(err error) string {
	if failure, ok := err.(*flexFetchFailure); ok && failure != nil {
		return normalizeFlexBrokerCode(failure.brokerCode)
	}
	return ""
}

func flexDailyWindow(now time.Time) (targetDate, firstAttempt time.Time) {
	location, err := time.LoadLocation(flexScheduleZone)
	if err != nil {
		location = time.FixedZone("CET", 60*60)
	}
	local := now.In(location)
	firstAttempt = time.Date(local.Year(), local.Month(), local.Day(), flexMorningHour, flexMorningMinute, 0, 0, location)
	// The job runs every calendar day, including weekends and holidays. Flex
	// accepts a weekend range end, but code 1003 proves that it does not accept
	// the still-open New York reporting date.
	targetDate = latestCompletedFlexDate(now)
	return targetDate, firstAttempt.UTC()
}

// latestCompletedFlexDate returns the most recent calendar date whose IBKR
// reporting window has closed. IBKR documents its securities window and
// statement publication in Eastern time, so a Canary host must not derive
// this date from its own local calendar.
func latestCompletedFlexDate(now time.Time) time.Time {
	location, err := time.LoadLocation(flexReportingZone)
	if err != nil {
		location = time.FixedZone("EST", -5*60*60)
	}
	completed := now.In(location).AddDate(0, 0, -1)
	return time.Date(completed.Year(), completed.Month(), completed.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *Server) latestFlexEvidence(ctx context.Context) (coverage, generated time.Time, valid bool) {
	return latestFlexEvidenceSelected(ctx, s.flexEvidenceSelection())
}

func latestFlexEvidenceSelected(ctx context.Context, selection flexEvidenceSelection) (coverage, generated time.Time, valid bool) {
	statements, problems, err := loadRetainedFlexStatementsContextSelected(ctx, nil, selection)
	if err != nil || len(problems) > 0 || len(statements) == 0 {
		return time.Time{}, time.Time{}, false
	}
	for _, statement := range statements {
		if statement.ToDate.After(coverage) {
			coverage = statement.ToDate
		}
		if statement.WhenGenerated.After(generated) {
			generated = statement.WhenGenerated
		}
	}
	coverage, generated = coverage.UTC(), generated.UTC()
	return coverage, generated, !coverage.IsZero() && !generated.IsZero()
}

func (s *Server) flexFetchStatusAt(now time.Time) rpc.ReconFetchStatus {
	targetDate, firstAttempt := flexDailyWindow(now)
	coverage, _, evidenceOK := s.latestFlexEvidence(context.Background())
	selection := s.flexEvidenceSelection()
	status := rpc.ReconFetchStatus{
		CoverageTo: coverage, RetryAutomatic: true,
	}
	if s == nil || s.cfg == nil || !s.cfg.Flex.Enabled {
		status.State, status.Reason = rpc.ReconReportStateActionRequired, rpc.ReconReportReasonFlexDisabled
		status.RetryAutomatic = false
		status.LastError = flexReasonMessage(status.Reason)
		return status
	}
	status.Configured = strings.TrimSpace(s.cfg.Flex.QueryID) != ""
	if !status.Configured {
		status.State, status.Reason = rpc.ReconReportStateActionRequired, rpc.ReconReportReasonQueryMissing
		status.RetryAutomatic = false
		status.LastError = flexReasonMessage(status.Reason)
		return status
	}

	st := &s.flexFetch
	st.mu.Lock()
	persisted, busy := st.state, st.busy
	st.mu.Unlock()
	if !flexFetchStateMatchesSelection(persisted, selection) {
		persisted = flexFetchStateV2{Version: flexFetchStateVersion, QueryFingerprint: selection.ActiveQueryFingerprint}
		busy = false
	}
	status.LastAttempt, status.LastSuccess = persisted.LastAttempt, persisted.LastSuccess
	status.BrokerCode = normalizeFlexBrokerCode(persisted.LastBrokerCode)
	status.Busy = busy
	if busy {
		status.State = rpc.ReconReportStateChecking
		return status
	}
	manualWindowOpen := !now.Before(firstAttempt)
	canCheckNow := func() bool {
		return manualWindowOpen && (persisted.LastAttempt.IsZero() || now.Sub(persisted.LastAttempt) >= flexManualRetryFloor)
	}
	if persisted.LastReason == "" && persisted.TargetDate.Equal(targetDate) && !persisted.LastSuccess.IsZero() && evidenceOK {
		status.State = rpc.ReconReportStateCurrent
		status.CanCheckNow = canCheckNow()
		return status
	}
	if persisted.LastReason != "" {
		status.Reason = persisted.LastReason
		status.RetryAutomatic = persisted.LastRetryable
		status.LastError = flexReasonMessage(status.Reason)
		if persisted.LastReason == rpc.ReconReportReasonAuthorityUnavailable {
			status.State = rpc.ReconReportStateUnavailable
			status.RetryAutomatic = false
			return status
		}
		if !persisted.LastRetryable {
			status.State = rpc.ReconReportStateActionRequired
			status.CanCheckNow = canCheckNow()
			return status
		}
		// Retry immediately only for the same daily target. On a new
		if persisted.TargetDate.Equal(targetDate) {
			next := persisted.NextAttempt
			if next.IsZero() {
				next = persisted.LastAttempt.Add(flexRetryAfterFail)
			}
			if now.Before(next) {
				status.State, status.NextAttempt = rpc.ReconReportStateRetryScheduled, next
				status.CanCheckNow = canCheckNow()
				return status
			}
		}
	}
	if now.Before(firstAttempt) {
		status.State, status.Reason, status.NextAttempt = rpc.ReconReportStateWaiting, rpc.ReconReportReasonBeforeDailyWindow, firstAttempt
		status.RetryAutomatic = true
		status.LastError = ""
		status.CanCheckNow = false
		return status
	}
	status.State, status.Reason = rpc.ReconReportStateDue, rpc.ReconReportReasonCoveragePending
	status.CanCheckNow = canCheckNow()
	return status
}

func flexFetchStateMatchesSelection(state flexFetchStateV2, selection flexEvidenceSelection) bool {
	return state.QueryFingerprint == selection.ActiveQueryFingerprint
}

func flexReasonMessage(reason string) string {
	switch reason {
	case rpc.ReconReportReasonReportNotReady, rpc.ReconReportReasonCoveragePending:
		return "IBKR has not published the complete daily report yet"
	case rpc.ReconReportReasonServiceBusy, rpc.ReconReportReasonRateLimited:
		return "IBKR is temporarily busy"
	case rpc.ReconReportReasonNetworkUnavailable:
		return "IBKR Flex service could not be reached"
	case rpc.ReconReportReasonFlexDisabled:
		return "daily Flex report checks are disabled"
	case rpc.ReconReportReasonQueryMissing, rpc.ReconReportReasonQueryInvalid:
		return "the Flex report query needs attention"
	case rpc.ReconReportReasonTokenMissing, rpc.ReconReportReasonTokenInvalid, rpc.ReconReportReasonTokenExpired:
		return "the Flex token needs attention"
	case rpc.ReconReportReasonIPRestricted:
		return "the Flex token does not allow this network"
	case rpc.ReconReportReasonServiceInactive:
		return "IBKR Flex Web Service is inactive"
	case rpc.ReconReportReasonReportInvalid, rpc.ReconReportReasonResponseInvalid:
		return "the IBKR report response could not be verified"
	case rpc.ReconReportReasonStorageFailed, rpc.ReconReportReasonProjectionFailed, rpc.ReconReportReasonAuthorityUnavailable:
		return "the report could not be processed locally"
	default:
		return ""
	}
}

// loadRetainedFlexStatements parses every retained raw statement, newest
// file first. A file that no longer parses is reported, never skipped
func loadRetainedFlexStatements() ([]flexstmt.Statement, []string, error) {
	return loadRetainedFlexStatementsContextSelected(context.Background(), nil, flexEvidenceSelection{IncludeAll: true})
}

func (s *Server) loadActiveRetainedFlexStatementsContext(ctx context.Context, checkpoint func(string) error) ([]flexstmt.Statement, []string, error) {
	return loadRetainedFlexStatementsContextSelected(ctx, checkpoint, s.flexEvidenceSelection())
}

func loadRetainedFlexStatementsContextSelected(ctx context.Context, checkpoint func(string) error, selection flexEvidenceSelection) ([]flexstmt.Statement, []string, error) {
	check := func(stage string) error {
		if checkpoint != nil {
			return checkpoint(stage)
		}
		return ctx.Err()
	}
	if err := check("retained_statements_start"); err != nil {
		return nil, nil, err
	}
	dir, err := flexStatementsDirPath()
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if err := check("retained_statements_entries"); err != nil {
			return nil, nil, err
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("retained statement %q is not a regular non-symlink file", e.Name())
		}
		included, err := selection.includesRetainedFlexFile(e.Name())
		if err != nil {
			return nil, nil, err
		}
		if included {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // timestamped names: newest first
	var out []flexstmt.Statement
	var problems []string
	for _, name := range names {
		if err := check("retained_statement_file"); err != nil {
			return nil, nil, err
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		sts, err := flexstmt.Parse(data)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		out = append(out, sts...)
	}
	if err := check("retained_statements_complete"); err != nil {
		return nil, nil, err
	}
	return out, problems, nil
}
