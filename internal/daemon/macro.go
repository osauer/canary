package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/osauer/canary/v2/internal/macrosource"
	"github.com/osauer/canary/v2/internal/rpc"
)

const macroStateKind = "macro_public_sources_v1"

// macroRecord is one source's persisted evidence. Failure is the typed cause of
// the current failure streak, dated at its first failure; records written
// before causes were kept may have a streak without one.
type macroRecord struct {
	Source  rpc.MacroSource    `json:"source"`
	Batch   macrosource.Batch  `json:"batch"`
	Failure *rpc.SourceFailure `json:"failure,omitempty"`
}
type macroFetcher interface {
	Fetch(context.Context, macrosource.Spec, time.Time, macrosource.Batch) (macrosource.Batch, error)
}
type macroCache struct {
	mu      sync.RWMutex
	records map[string]macroRecord
	client  macroFetcher
}

func coldMacroRecord(spec macrosource.Spec) macroRecord {
	return macroRecord{Source: rpc.MacroSource{ID: spec.ID, Name: spec.Name, URL: spec.URL, Kind: spec.Kind, Availability: "unavailable", Stale: true, Coverage: spec.Coverage, Detail: "Waiting for first source read"}}
}

func (s *Server) loadMacroSources() *macroCache {
	c := &macroCache{records: map[string]macroRecord{}, client: macrosource.NewClient()}
	for _, spec := range macrosource.Specs() {
		row := coldMacroRecord(spec)
		raw, ok, err := loadMarketState(s.coreStore, "public-macro:"+spec.ID, macroStateKind)
		if err != nil {
			row.Source.Detail = "Public source persistence unavailable"
		} else if ok {
			var saved macroRecord
			if json.Unmarshal(raw, &saved) == nil && validateMacroEnvelope(spec, saved, s.orderNow()) == nil {
				row = saved
			} else {
				row.Source.Detail = "Saved public source record is invalid"
			}
		}
		c.records[spec.ID] = row
	}
	return c
}

func (s *Server) startMacroSources(ctx context.Context) {
	c := s.loadMacroSources()
	// A custom StateDatabasePath is the existing offline/test authority seam.
	// It can serve retained fixtures but must not start external network readers.
	s.mu.Lock()
	s.macro = c
	s.mu.Unlock()
	if !s.productionStateDatabase || s.disableMacroSources {
		return
	}
	s.macroLoopWG.Go(func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			s.refreshMacroSources(ctx, c)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func (s *Server) refreshMacroSources(ctx context.Context, c *macroCache) {
	var wg sync.WaitGroup
	for _, spec := range macrosource.Specs() {
		wg.Go(func() { s.refreshMacroSource(ctx, c, spec) })
	}
	wg.Wait()
}

func (s *Server) refreshMacroSource(ctx context.Context, c *macroCache, spec macrosource.Spec) {
	if ctx.Err() != nil {
		return
	}
	at := s.orderNow().UTC()
	c.mu.RLock()
	retained := c.records[spec.ID]
	c.mu.RUnlock()
	if at.Before(retained.Source.NextAttempt) {
		return
	}
	batch, err := c.client.Fetch(ctx, spec, at, retained.Batch)
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		err = macrosource.ValidateBatch(spec, batch, at)
	}
	c.mu.RLock()
	row := c.records[spec.ID]
	c.mu.RUnlock()
	if at.Before(row.Source.NextAttempt) {
		return
	}
	failedBefore, failingSince := row.Source.ConsecutiveFailures, row.Source.FirstFailure
	row.Source.LastAttempt = at
	if err != nil {
		row.Source.Availability = "unavailable"
		row.Source.Detail = err.Error()
		if row.Source.ConsecutiveFailures == 0 {
			row.Source.FirstFailure = at
		}
		row.Source.ConsecutiveFailures++
		delay := 5 * time.Minute << min(row.Source.ConsecutiveFailures-1, 4)
		row.Source.NextAttempt = at.Add(min(delay, time.Hour))
		row.Failure = macroSourceFailure(err, row.Source.FirstFailure)
	} else {
		row.Batch = batch
		row.Source.Availability = "available"
		row.Source.Detail = ""
		row.Source.LastSuccess = at
		row.Source.ValidUntil = at.Add(spec.Freshness)
		row.Source.Stale = false
		row.Source.FirstFailure = time.Time{}
		row.Source.ConsecutiveFailures = 0
		row.Source.NextAttempt = at.Add(spec.Refresh)
		row.Source.WindowStart, row.Source.WindowEnd = batch.WindowStart, batch.WindowEnd
		row.Failure = nil
	}
	raw, encodeErr := json.Marshal(row)
	if encodeErr == nil {
		encodeErr = saveMarketDocument(ctx, s.coreStore, "public-macro:"+spec.ID, macroStateKind, raw)
	}
	if encodeErr != nil {
		c.mu.Lock()
		prior := c.records[spec.ID]
		prior.Source.LastAttempt = at
		prior.Source.Availability = "unavailable"
		prior.Source.Detail = "Public source persistence unavailable"
		c.records[spec.ID] = prior
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	c.records[spec.ID] = row
	c.mu.Unlock()
	// Log streak transitions only; repeated attempts are visible in health.
	switch {
	case err != nil && failedBefore == 0:
		s.warnf("macro source %s refresh failed (%s at %s, retryable=%t); next attempt %s: %s",
			spec.ID, row.Failure.Code, row.Failure.Stage, row.Failure.Retryable, row.Source.NextAttempt.Format(time.RFC3339), row.Source.Detail)
	case err == nil && failedBefore > 0:
		s.infof("macro source %s recovered after %d failed refreshes since %s", spec.ID, failedBefore, failingSince.Format(time.RFC3339))
	}
}

// macroSourceFailure classifies a refresh error for health reporting. A
// validation failure after a successful read is a rejected payload.
func macroSourceFailure(err error, firstFailure time.Time) *rpc.SourceFailure {
	failure := rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStagePublicSourceParse, FailedAt: firstFailure, Retryable: true}
	if typed, ok := errors.AsType[*macrosource.FetchError](err); ok {
		failure.Code, failure.Stage, failure.Retryable = typed.Code, typed.Stage, typed.Retryable
	}
	return &failure
}

// macroSourceFailures copies each failing source's typed cause for health rows.
func (s *Server) macroSourceFailures() map[string]rpc.SourceFailure {
	s.mu.Lock()
	cache := s.macro
	s.mu.Unlock()
	out := map[string]rpc.SourceFailure{}
	if cache == nil {
		return out
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	for id, record := range cache.records {
		if record.Failure != nil {
			out[id] = *record.Failure
		}
	}
	return out
}

func (s *Server) handleMacroRequest(req rpc.Request) (rpc.MacroSnapshotResult, error) {
	var in rpc.MacroSnapshotParams
	if len(req.Params) != 0 {
		if err := json.Unmarshal(req.Params, &in); err != nil {
			return rpc.MacroSnapshotResult{}, errors.New("invalid macro window parameters")
		}
	}
	if in.WindowStart == "" && in.WindowEnd == "" {
		return s.handleMacroSnapshot(), nil
	}
	start, e1 := time.Parse(time.DateOnly, in.WindowStart)
	end, e2 := time.Parse(time.DateOnly, in.WindowEnd)
	if e1 != nil || e2 != nil || end.Before(start) || end.Sub(start) > 30*24*time.Hour {
		return rpc.MacroSnapshotResult{}, errors.New("macro window requires both YYYY-MM-DD dates spanning at most 31 days")
	}
	return s.macroSnapshotWindow(in.WindowStart, in.WindowEnd), nil
}

func (s *Server) handleMacroSnapshot() rpc.MacroSnapshotResult {
	now := s.orderNow().UTC()
	start := now.Add(-24 * time.Hour).Format(time.DateOnly)
	end := now.AddDate(0, 0, 7).Format(time.DateOnly)
	return s.macroSnapshotWindow(start, end)
}

func (s *Server) macroSnapshotWindow(start, end string) rpc.MacroSnapshotResult {
	now := s.orderNow().UTC()
	out := rpc.MacroSnapshotResult{AsOf: now, WindowStart: start, WindowEnd: end, CoverageStatus: "partial", Events: []rpc.MacroEvent{}, Publications: []rpc.MacroPublication{}, Sources: []rpc.MacroSource{}}
	s.mu.Lock()
	cache := s.macro
	s.mu.Unlock()
	if cache == nil {
		for _, spec := range macrosource.Specs() {
			row := coldMacroRecord(spec)
			row.Source.Detail = "Public source collector unavailable"
			out.Sources = append(out.Sources, row.Source)
		}
		return out
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	out.CoverageStatus = "available"
	for _, spec := range macrosource.Specs() {
		record, ok := cache.records[spec.ID]
		if !ok {
			record = coldMacroRecord(spec)
		}
		source := record.Source
		source.Stale = source.LastSuccess.IsZero() || now.Before(source.LastSuccess.Add(-time.Minute)) || !now.Before(source.ValidUntil)
		if source.Availability != "available" || source.Stale || source.WindowStart != "" && (start < source.WindowStart || end > source.WindowEnd) {
			out.CoverageStatus = "partial"
		}
		out.Sources = append(out.Sources, source)
		for _, event := range record.Batch.Events {
			if event.Date >= start && event.Date <= end {
				out.Events = append(out.Events, event)
			}
		}
		for _, item := range record.Batch.Publications {
			if item.PublishedAt.IsZero() || !item.PublishedAt.Before(now.AddDate(0, 0, -7)) {
				out.Publications = append(out.Publications, item)
			}
		}
	}
	sort.Slice(out.Events, func(i, j int) bool {
		a, b := out.Events[i], out.Events[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if !a.ScheduledAt.Equal(b.ScheduledAt) {
			return a.ScheduledAt.Before(b.ScheduledAt)
		}
		return a.ID < b.ID
	})
	sort.Slice(out.Publications, func(i, j int) bool {
		a, b := out.Publications[i], out.Publications[j]
		if a.PublishedAt.Equal(b.PublishedAt) {
			return a.ID < b.ID
		}
		return a.PublishedAt.After(b.PublishedAt)
	})
	if len(out.Events) > 48 {
		out.Events = out.Events[:48]
		out.EventsTruncated = true
	}
	if len(out.Publications) > 12 {
		out.Publications = out.Publications[:12]
		out.PublicationsTruncated = true
	}
	// This public overview is bounded independently of the complete source cache.
	for {
		out.Truncated = out.EventsTruncated || out.PublicationsTruncated
		raw, _ := json.Marshal(out)
		if len(raw) <= 28<<10 {
			break
		}
		if len(out.Publications) > 0 {
			out.Publications = out.Publications[:len(out.Publications)-1]
			out.PublicationsTruncated = true
		} else if len(out.Events) > 0 {
			out.Events = out.Events[:len(out.Events)-1]
			out.EventsTruncated = true
		} else {
			break
		}
	}
	return out
}

func validateMacroEnvelope(spec macrosource.Spec, record macroRecord, now time.Time) error {
	source := record.Source
	if !utf8.ValidString(source.Detail) || len(source.Detail) > 500 {
		return errors.New("invalid public source detail")
	}
	for _, r := range source.Detail {
		if unicode.IsControl(r) {
			return errors.New("invalid public source detail")
		}
	}
	if source.Availability == "available" && source.Detail != "" {
		return errors.New("successful public source carries a failure")
	}
	if source.ID != spec.ID || source.Name != spec.Name || source.URL != spec.URL || source.Kind != spec.Kind || source.Coverage != spec.Coverage {
		return errors.New("invalid public source identity")
	}
	if source.Availability != "available" && source.Availability != "unavailable" {
		return errors.New("invalid public source availability")
	}
	if source.LastAttempt.After(now.Add(time.Minute)) || source.LastSuccess.After(source.LastAttempt) {
		return errors.New("invalid public source attempt clock")
	}
	if source.ConsecutiveFailures < 0 || source.FirstFailure.After(source.LastAttempt) || source.NextAttempt.After(source.LastAttempt.Add(max(time.Hour, spec.Refresh))) || !source.NextAttempt.IsZero() && source.NextAttempt.Before(source.LastAttempt) {
		return errors.New("invalid public source failure interval")
	}
	if (source.ConsecutiveFailures == 0) != source.FirstFailure.IsZero() || source.Availability == "available" && source.ConsecutiveFailures != 0 {
		return errors.New("invalid public source failure state")
	}
	if f := record.Failure; f != nil && (!rpc.ValidSourceFailure(f) || f.Stage != rpc.SourceFailureStagePublicSourceRequest && f.Stage != rpc.SourceFailureStagePublicSourceParse || source.ConsecutiveFailures == 0 || !f.FailedAt.Equal(source.FirstFailure)) {
		return errors.New("invalid public source failure cause")
	}
	if source.WindowStart != record.Batch.WindowStart || source.WindowEnd != record.Batch.WindowEnd {
		return errors.New("public source coverage mismatch")
	}
	if source.LastSuccess.IsZero() {
		if source.Availability != "unavailable" || !source.ValidUntil.IsZero() || len(record.Batch.Events)+len(record.Batch.Publications) != 0 {
			return errors.New("public source evidence missing")
		}
		return nil
	}
	// A record written under an earlier, shorter window stays valid but never
	// gains freshness; only a new read can extend it.
	if !source.ValidUntil.After(source.LastSuccess) || source.ValidUntil.After(source.LastSuccess.Add(spec.Freshness)) || source.Availability == "available" && !source.LastAttempt.Equal(source.LastSuccess) {
		return errors.New("invalid public source freshness interval")
	}
	if err := macrosource.ValidateBatch(spec, record.Batch, now); err != nil {
		return err
	}
	for _, event := range record.Batch.Events {
		if !event.RetrievedAt.Equal(source.LastSuccess) {
			return errors.New("calendar retrieval receipt mismatch")
		}
	}
	for _, item := range record.Batch.Publications {
		if !item.RetrievedAt.Equal(source.LastSuccess) {
			return errors.New("publication retrieval receipt mismatch")
		}
	}
	return nil
}
