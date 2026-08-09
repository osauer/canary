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
// never from request URLs.

const (
	flexSendRequestURL    = "https://gdcdyn.interactivebrokers.com/Universal/servlet/FlexStatementService.SendRequest"
	flexGetStatementURL   = "https://gdcdyn.interactivebrokers.com/Universal/servlet/FlexStatementService.GetStatement"
	flexUserAgent         = "canary-flex-reconciliation/1"
	flexStatementsDir     = "statements"
	flexFetchStateVersion = 2
	flexFetchStateKind    = "flex_fetch"
	flexFetchProjecting   = "projecting"
	flexScheduleZone      = "Europe/Berlin"
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
	Version       int       `json:"version"`
	Stage         string    `json:"stage,omitempty"`
	LastAttempt   time.Time `json:"last_attempt,omitzero"`
	LastSuccess   time.Time `json:"last_success,omitzero"`
	LastReason    string    `json:"last_reason,omitempty"`
	LastRetryable bool      `json:"last_retryable,omitempty"`
	// TargetDate is the Berlin calendar day whose one daily broker check is
	// being attempted. CoverageTo is deliberately separate: IBKR activity
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
	CoverageTo    time.Time
	WhenGenerated time.Time
}

type flexFetchFailure struct {
	reason    string
	retryable bool
	detail    string // local log only; must already be redacted
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
	// A daemon that stopped mid-request cannot still be checking after a
	recoveredProjecting := state.Stage == flexFetchProjecting
	recoveredInterrupted := state.Stage == rpc.ReconReportStateChecking || recoveredProjecting
	if recoveredInterrupted {
		state.Stage = rpc.ReconReportStateRetryScheduled
		state.LastReason = rpc.ReconReportReasonNetworkUnavailable
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
	st := &s.flexFetch
	st.mu.Lock()
	if st.stopping || st.busy || (manual && !st.state.LastAttempt.IsZero() && now.Sub(st.state.LastAttempt) < flexManualRetryFloor) {
		st.mu.Unlock()
		operationCancel()
		return false
	}
	failedTarget := st.state.TargetDate
	st.busy = true
	st.done = make(chan struct{})
	st.cancel = operationCancel
	st.state.Stage = rpc.ReconReportStateChecking
	st.state.LastAttempt = now.UTC()
	st.state.TargetDate = targetDate
	st.state.NextAttempt = time.Time{}
	if err := st.persistLocked(context.WithoutCancel(ctx)); err != nil {
		st.state.Stage = rpc.ReconReportStateUnavailable
		st.state.LastReason = rpc.ReconReportReasonAuthorityUnavailable
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

	coverage, _, evidenceOK := latestFlexEvidence()
	var outcome flexFetchOutcome
	var err error
	// Projection failures are retried locally from retained broker evidence on
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
		st.state.LastReason, st.state.LastRetryable = reason, retryable
		if retryable {
			st.state.NextAttempt = finished.UTC().Add(flexRetryAfterFail)
		} else {
			st.state.NextAttempt = time.Time{}
		}
		s.infof("Flex report check failed: %s", reason)
	} else {
		st.state.Stage = rpc.ReconReportStateCurrent
		st.state.LastReason, st.state.LastRetryable = "", false
		st.state.LastSuccess = finished.UTC()
		st.state.CoverageTo = coverage.UTC()
		st.state.NextAttempt = time.Time{}
	}
	if persistErr := st.persistLocked(context.Background()); persistErr != nil {
		st.state.Stage = rpc.ReconReportStateUnavailable
		st.state.LastReason = rpc.ReconReportReasonAuthorityUnavailable
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
// service envelope can never sit in the statements dir pretending to be a
func (s *Server) fetchFlexOnce(ctx context.Context, _ time.Time) (flexFetchOutcome, error) {
	cfg := s.cfg.Flex
	queryID := strings.TrimSpace(cfg.QueryID)
	if queryID == "" {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonQueryMissing, detail: "Flex query id is not configured"}
	}
	tokenBytes, err := os.ReadFile(expandUserPath(cfg.TokenPath))
	if err != nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonTokenMissing, detail: "Flex token is unavailable"}
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonTokenMissing, detail: "Flex token is unavailable"}
	}
	client := &http.Client{Timeout: flexHTTPTimeout}

	env, err := flexServiceCall(ctx, client, flexSendRequestURL, url.Values{"t": {token}, "q": {queryID}, "v": {"3"}})
	if err != nil {
		return flexFetchOutcome{}, err
	}
	if !strings.EqualFold(env.Status, "Success") || strings.TrimSpace(env.ReferenceCode) == "" {
		return flexFetchOutcome{}, flexEnvelopeFailure(env.ErrorCode)
	}

	var raw []byte
	for attempt := range flexPollAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonNetworkUnavailable, retryable: true, detail: "Flex report check timed out"}
			case <-time.After(flexPollInterval):
			}
		}
		// The response URL is legacy and untrusted. Always use the fixed IBKR
		// endpoint rather than following a broker-authored arbitrary host.
		body, err := flexRawCall(ctx, client, flexGetStatementURL, url.Values{"t": {token}, "q": {env.ReferenceCode}, "v": {"3"}})
		if err != nil {
			return flexFetchOutcome{}, err
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
			return flexFetchOutcome{}, flexEnvelopeFailure(code)
		}
		raw = body
		break
	}
	if raw == nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "Flex report is still being generated"}
	}
	return retainFlexStatement(raw)
}

// retainFlexStatement accepts every complete, typed broker report, even when
// its coverage date or generation timestamp has not advanced. IBKR activity
// IBKR keeps the same generation timestamp. Exact bytes reuse retained
// query result. Strictly older broker generations remain rejected.
func retainFlexStatement(raw []byte) (flexFetchOutcome, error) {
	statements, err := flexstmt.Parse(raw)
	if err != nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonReportInvalid, retryable: true, detail: "Flex report did not match the expected format"}
	}
	var coverage, generated time.Time
	for _, statement := range statements {
		if statement.ToDate.After(coverage) {
			coverage = statement.ToDate
		}
		if statement.WhenGenerated.After(generated) {
			generated = statement.WhenGenerated
		}
	}
	if coverage.IsZero() || generated.IsZero() {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonReportInvalid, retryable: true, detail: "Flex report did not carry a coverage date"}
	}
	_, latestGenerated, evidenceOK := latestFlexEvidence()
	if evidenceOK && generated.Before(latestGenerated) {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "IBKR returned an older report generation"}
	}
	dir, err := flexStatementsDirPath()
	if err != nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonStorageFailed, retryable: true, detail: "Flex report storage is unavailable"}
	}
	if retainedPath, duplicate, err := findRetainedFlexReport(dir, raw); err != nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonStorageFailed, retryable: true, detail: "retained Flex reports could not be verified"}
	} else if duplicate {
		return flexFetchOutcome{Path: retainedPath, CoverageTo: coverage, WhenGenerated: generated}, nil
	}
	digest := sha256.Sum256(raw)
	path := filepath.Join(dir, fmt.Sprintf("flex-%s-%x.xml", time.Now().UTC().Format("20060102-150405.000000000"), digest[:6]))
	if err := writePrivateStateAtomic(path, raw); err != nil {
		return flexFetchOutcome{}, &flexFetchFailure{reason: rpc.ReconReportReasonStorageFailed, retryable: true, detail: "Flex report could not be retained"}
	}
	return flexFetchOutcome{Path: path, CoverageTo: coverage, WhenGenerated: generated}, nil
}

func findRetainedFlexReport(dir string, raw []byte) (string, bool, error) {
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

// flexServiceCall performs one envelope-returning call. The token travels
// only in the POST form body, and errors never include the request.
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, &flexFetchFailure{reason: rpc.ReconReportReasonResponseInvalid, retryable: true, detail: "Flex request could not be built"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", flexUserAgent)
	resp, err := client.Do(req)
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
	switch strings.TrimSpace(code) {
	case "1001", "1003", "1004", "1005", "1006", "1007", "1008":
		return &flexFetchFailure{reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "IBKR has not published the complete report yet"}
	case "1009", "1019":
		return &flexFetchFailure{reason: rpc.ReconReportReasonServiceBusy, retryable: true, detail: "IBKR is still preparing the report"}
	case "1018":
		return &flexFetchFailure{reason: rpc.ReconReportReasonRateLimited, retryable: true, detail: "IBKR Flex request limit was reached"}
	case "1010":
		return &flexFetchFailure{reason: rpc.ReconReportReasonQueryInvalid, detail: "IBKR Flex query type is no longer supported"}
	case "1011":
		return &flexFetchFailure{reason: rpc.ReconReportReasonServiceInactive, detail: "IBKR Flex Web Service is inactive"}
	case "1012":
		return &flexFetchFailure{reason: rpc.ReconReportReasonTokenExpired, detail: "IBKR Flex token has expired"}
	case "1013":
		return &flexFetchFailure{reason: rpc.ReconReportReasonIPRestricted, detail: "IBKR Flex token does not allow this network"}
	case "1014", "1016":
		return &flexFetchFailure{reason: rpc.ReconReportReasonQueryInvalid, detail: "IBKR Flex query is not valid for this account"}
	case "1015":
		return &flexFetchFailure{reason: rpc.ReconReportReasonTokenInvalid, detail: "IBKR Flex token is invalid"}
	default:
		return &flexFetchFailure{reason: rpc.ReconReportReasonResponseInvalid, retryable: true, detail: "IBKR returned an unrecognized Flex status"}
	}
}

func flexFailureStatus(err error) (string, bool) {
	if failure, ok := err.(*flexFetchFailure); ok && failure != nil {
		return failure.reason, failure.retryable
	}
	return rpc.ReconReportReasonResponseInvalid, true
}

func flexDailyWindow(now time.Time) (targetDate, firstAttempt time.Time) {
	location, err := time.LoadLocation(flexScheduleZone)
	if err != nil {
		location = time.FixedZone("CET", 60*60)
	}
	local := now.In(location)
	firstAttempt = time.Date(local.Year(), local.Month(), local.Day(), flexMorningHour, flexMorningMinute, 0, 0, location)
	// The job runs every calendar day, including weekends and holidays, but
	// does not invent an expected broker coverage date. IBKR's own report says
	targetDate = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	return targetDate, firstAttempt.UTC()
}

func latestFlexEvidence() (coverage, generated time.Time, valid bool) {
	statements, problems, err := loadRetainedFlexStatements()
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
	coverage, _, evidenceOK := latestFlexEvidence()
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
	status.LastAttempt, status.LastSuccess = persisted.LastAttempt, persisted.LastSuccess
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
	return loadRetainedFlexStatementsContext(context.Background(), nil)
}

func loadRetainedFlexStatementsContext(ctx context.Context, checkpoint func(string) error) ([]flexstmt.Statement, []string, error) {
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
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".xml") {
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
