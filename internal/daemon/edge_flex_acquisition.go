package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	edgeFlexAcquisitionStateKind = "edge_flex_acquisition"
	edgeFlexAcquisitionVersion   = 1
	edgeFlexChunkCount           = 4
	edgeFlexPace                 = time.Minute
	edgeFlexRetryAfter           = 30 * time.Minute
	// Initial execution-detail ranges are materially heavier than the daily
	// 35-day refresh. Keep the same ten-second cadence but allow 15 minutes
	// before falling back to the durable retry schedule.
	edgeFlexPollAttempts = 90
)

type edgeFlexAcquisition struct {
	Version              int       `json:"version"`
	ScopeFingerprint     string    `json:"scope_fingerprint"`
	QueryFingerprint     string    `json:"query_fingerprint,omitempty"`
	AnchorDate           time.Time `json:"anchor_date,omitzero"`
	NextChunk            int       `json:"next_chunk"`
	NextAttempt          time.Time `json:"next_attempt,omitzero"`
	LastReason           string    `json:"last_reason,omitempty"`
	LastFullRevalidation time.Time `json:"last_full_revalidation,omitzero"`
}

type edgeFlexAcquisitionProgress struct {
	Pending              bool
	State                string
	Reason               string
	LastFullRevalidation time.Time
}

// advanceEdgeFlexAcquisition performs at most one broker request. Four
// durable date chunks cover the inclusive trailing 365 days; completion of a
// chunk is committed before the next one is scheduled, so a daemon restart
// resumes rather than restarts the backfill.
func (s *Server) advanceEdgeFlexAcquisition(ctx context.Context, scopeFingerprint string) (edgeFlexAcquisitionProgress, error) {
	progress := edgeFlexAcquisitionProgress{State: rpc.EdgeStateBackfilling, Reason: "statement_backfill_pending"}
	state, err := s.loadEdgeFlexAcquisition(ctx)
	if err != nil {
		return progress, err
	}
	now := s.edgeNow()
	queryFingerprint := ""
	if s.cfg != nil {
		queryFingerprint = flexQueryFingerprint(s.cfg.Flex.QueryID)
	}
	if state.ScopeFingerprint != scopeFingerprint || state.QueryFingerprint != queryFingerprint {
		state = edgeFlexAcquisition{
			Version:          edgeFlexAcquisitionVersion,
			ScopeFingerprint: scopeFingerprint,
			QueryFingerprint: queryFingerprint,
		}
	}
	progress.LastFullRevalidation = state.LastFullRevalidation
	fullDue := state.LastFullRevalidation.IsZero() || now.Sub(state.LastFullRevalidation) >= edgeFullRevalidateAfter
	if !fullDue {
		return progress, nil
	}
	progress.Pending = true
	if state.AnchorDate.IsZero() {
		state.AnchorDate = latestCompletedFlexDate(now)
		state.NextChunk = 0
		state.NextAttempt = time.Time{}
		state.LastReason = ""
		if err := s.saveEdgeFlexAcquisition(ctx, state); err != nil {
			return progress, err
		}
	}
	if !state.NextAttempt.IsZero() && now.Before(state.NextAttempt) {
		progress.Reason = firstNonEmptyEdgeReason(state.LastReason, "statement_backfill_paced")
		s.scheduleEdgeRebuildAfter(state.NextAttempt.Sub(now))
		return progress, nil
	}
	if s.flexFetch.isBusy() {
		progress.Reason = "statement_fetch_busy"
		s.scheduleEdgeRebuildAfter(edgeFlexPace)
		return progress, nil
	}
	ranges := edgeFlexRanges(state.AnchorDate)
	if state.NextChunk < 0 || state.NextChunk >= len(ranges) {
		return progress, fmt.Errorf("invalid Edge Flex acquisition chunk %d", state.NextChunk)
	}
	current := ranges[state.NextChunk]
	var outcome flexFetchOutcome
	if s.edgeFlexFetchRangeFn != nil {
		outcome, err = s.edgeFlexFetchRangeFn(ctx, current.From, current.To)
	} else {
		outcome, err = s.fetchFlexDateRangeWithPollAttempts(ctx, current.From, current.To, edgeFlexPollAttempts)
	}
	if err == nil && !flexOutcomeCoversRange(outcome, current) {
		err = &flexFetchFailure{reason: rpc.ReconReportReasonReportNotReady, retryable: true, detail: "IBKR Flex response did not prove the requested date range"}
	}
	if err == nil {
		projectionCtx, cancel := context.WithTimeout(ctx, flexHTTPTimeout)
		if s.flexProjectionFn != nil {
			err = s.flexProjectionFn(projectionCtx)
		} else {
			err = s.refreshStatementProjection(projectionCtx)
		}
		cancel()
		if err != nil {
			err = &flexFetchFailure{reason: rpc.ReconReportReasonProjectionFailed, retryable: true, detail: "Edge statement projection refresh failed"}
		}
	}
	if err != nil {
		reason, retryable := flexFailureStatus(err)
		state.LastReason = reason
		state.NextAttempt = time.Time{}
		if retryable {
			state.NextAttempt = now.Add(edgeFlexRetryAfter)
			progress.State = rpc.EdgeStateBackfilling
			s.scheduleEdgeRebuildAfter(edgeFlexRetryAfter)
		} else {
			progress.State = rpc.EdgeStateActionRequired
		}
		progress.Reason = reason
		if saveErr := s.saveEdgeFlexAcquisition(context.WithoutCancel(ctx), state); saveErr != nil {
			return progress, saveErr
		}
		return progress, nil
	}

	state.NextChunk++
	state.LastReason = ""
	if state.NextChunk < edgeFlexChunkCount {
		state.NextAttempt = now.Add(edgeFlexPace)
		progress.Reason = "statement_backfill_paced"
		if err := s.saveEdgeFlexAcquisition(context.WithoutCancel(ctx), state); err != nil {
			return progress, err
		}
		s.scheduleEdgeRebuildAfter(edgeFlexPace)
		return progress, nil
	}
	state.LastFullRevalidation = now
	state.AnchorDate = time.Time{}
	state.NextChunk = 0
	state.NextAttempt = time.Time{}
	progress.Pending = false
	progress.Reason = ""
	progress.LastFullRevalidation = now
	if err := s.saveEdgeFlexAcquisition(context.WithoutCancel(ctx), state); err != nil {
		return progress, err
	}
	return progress, nil
}

func flexOutcomeCoversRange(outcome flexFetchOutcome, requested edgeFlexRange) bool {
	from := dateOnlyUTC(outcome.CoverageFrom)
	to := dateOnlyUTC(outcome.CoverageTo)
	return !from.IsZero() && !to.IsZero() && !from.After(dateOnlyUTC(requested.From)) && !to.Before(dateOnlyUTC(requested.To))
}

type edgeFlexRange struct {
	From time.Time
	To   time.Time
}

func edgeFlexRanges(anchor time.Time) []edgeFlexRange {
	anchor = dateOnlyUTC(anchor)
	start := anchor.AddDate(0, 0, -364)
	sizes := [...]int{92, 91, 91, 91}
	ranges := make([]edgeFlexRange, 0, len(sizes))
	cursor := start
	for _, size := range sizes {
		to := cursor.AddDate(0, 0, size-1)
		ranges = append(ranges, edgeFlexRange{From: cursor, To: to})
		cursor = to.AddDate(0, 0, 1)
	}
	return ranges
}

func (s *Server) loadEdgeFlexAcquisition(ctx context.Context) (edgeFlexAcquisition, error) {
	out := edgeFlexAcquisition{Version: edgeFlexAcquisitionVersion}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, edgeFlexAcquisitionStateKind)
	if err != nil || !ok {
		return out, err
	}
	if err := json.Unmarshal(doc.JSON, &out); err != nil {
		return out, fmt.Errorf("decode Edge Flex acquisition: %w", err)
	}
	if out.Version != edgeFlexAcquisitionVersion {
		return out, fmt.Errorf("unsupported Edge Flex acquisition version")
	}
	return out, nil
}

func (s *Server) saveEdgeFlexAcquisition(ctx context.Context, state edgeFlexAcquisition) error {
	state.Version = edgeFlexAcquisitionVersion
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.replaceEdgeStateDocument(ctx, edgeFlexAcquisitionStateKind, raw)
}

func (s *Server) scheduleEdgeRebuildAfter(delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	if s.edgeScheduleRebuildFn != nil {
		s.edgeScheduleRebuildFn(delay)
		return
	}
	s.mu.Lock()
	ctx := s.serverCtx
	s.mu.Unlock()
	if ctx == nil {
		return
	}
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			s.kickEdgeRebuild()
		}
	}()
}

func firstNonEmptyEdgeReason(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
