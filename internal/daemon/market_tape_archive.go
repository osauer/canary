package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	tapeArchiveScope = "market/tape"
	tapeArchiveKind  = "market_tape.archive.v1"
	tapeSessionKind  = "market_tape.session.v1"
)

// Each date has a bounded current projection plus append-only observations in
// daemon.db. No rolling cache or JSON sidecar is the archive authority.
type tapeArchiveDay struct {
	Fingerprint string                       `json:"fingerprint"`
	First       rpc.MarketTapeCapture        `json:"first"`
	Latest      rpc.MarketTapeCapture        `json:"latest"`
	SPX         *rpc.MarketTapeArchivedClose `json:"spx,omitempty"`
	QQQ         *rpc.MarketTapeArchivedClose `json:"qqq,omitempty"`
}

func tapeDayScope(date string) string { return tapeArchiveScope + "/" + date }

func readTapeArchiveStatus(ctx context.Context, store *corestore.Store) (rpc.MarketTapeArchiveStatus, error) {
	if store == nil {
		return rpc.MarketTapeArchiveStatus{Status: "unavailable"}, errors.New("market tape archive unavailable")
	}
	doc, exists, err := store.GetStateDocument(ctx, tapeArchiveScope, tapeArchiveKind)
	if err != nil {
		return rpc.MarketTapeArchiveStatus{}, err
	}
	if !exists {
		return rpc.MarketTapeArchiveStatus{Status: "not_started"}, nil
	}
	var status rpc.MarketTapeArchiveStatus
	if err := json.Unmarshal(doc.JSON, &status); err != nil {
		return status, err
	}
	return status, nil
}

func updateTapeArchiveStatus(ctx context.Context, store *corestore.Store, now time.Time, stored bool) (rpc.MarketTapeArchiveStatus, error) {
	if store == nil {
		return rpc.MarketTapeArchiveStatus{}, errors.New("market tape archive unavailable")
	}
	for range 4 {
		doc, exists, err := store.GetStateDocument(ctx, tapeArchiveScope, tapeArchiveKind)
		if err != nil {
			return rpc.MarketTapeArchiveStatus{}, err
		}
		status := rpc.MarketTapeArchiveStatus{StartedAt: now, Status: "collecting"}
		if exists {
			if err := json.Unmarshal(doc.JSON, &status); err != nil {
				return status, err
			}
			if !stored {
				return status, nil
			}
		}
		if stored && now.After(status.LastStoredAt) {
			status.LastStoredAt, status.Status = now, "collecting"
		}
		data, err := json.Marshal(status)
		if err != nil {
			return status, err
		}
		_, err = store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: tapeArchiveScope, Kind: tapeArchiveKind, ExpectedRevision: doc.Revision, JSON: data})
		if errors.Is(err, corestore.ErrRevisionConflict) {
			continue
		}
		return status, err
	}
	return rpc.MarketTapeArchiveStatus{}, errors.New("market tape archive status changed concurrently")
}

// Window-relative prices depend on the caller's selected display range, not on
// the day measured. Producer recomputation clocks are retained in the payload,
// but alone do not justify another measurement revision.
func tapeArchiveCapture(row rpc.MarketTapeSession, sources []rpc.MarketTapeSource, now time.Time) (rpc.MarketTapeCapture, string, error) {
	data, err := json.Marshal(rpc.MarketTapeCapture{CapturedAt: now, Session: row, Sources: sources})
	if err != nil {
		return rpc.MarketTapeCapture{}, "", err
	}
	var capture rpc.MarketTapeCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		return capture, "", err
	}
	if capture.Session.SPX != nil {
		capture.Session.SPX.WindowChangePct = nil
	}
	if capture.Session.QQQ != nil {
		capture.Session.QQQ.WindowChangePct = nil
	}
	for _, p := range []*rpc.MarketTapePrice{capture.Session.SPY, capture.Session.VIX} {
		if p != nil {
			p.WindowChangePct = nil
		}
	}
	if b := capture.Session.Leaders; b != nil {
		if b.Price != nil {
			b.Price.WindowChangePct = nil
		}
		for i := range b.Members {
			if b.Members[i].Price != nil {
				b.Members[i].Price.WindowChangePct = nil
			}
		}
	}
	canonical := capture.Session
	if canonical.Breadth != nil && canonical.Breadth.Participation != nil {
		breadth, participation := *canonical.Breadth, *canonical.Breadth.Participation
		participation.RecordedAt, participation.InputObservedAt = time.Time{}, time.Time{}
		breadth.Participation, canonical.Breadth = &participation, &breadth
	}
	data, err = json.Marshal(canonical)
	if err != nil {
		return capture, "", err
	}
	digest := sha256.Sum256(data)
	return capture, hex.EncodeToString(digest[:]), nil
}

func loadTapeDay(ctx context.Context, store *corestore.Store, date string) (tapeArchiveDay, corestore.StateDocument, bool, error) {
	doc, exists, err := store.GetStateDocument(ctx, tapeDayScope(date), tapeSessionKind)
	var day tapeArchiveDay
	if err == nil && exists {
		err = json.Unmarshal(doc.JSON, &day)
	}
	if err == nil && exists && (day.First.Session.Date != date || day.Latest.Session.Date != date || day.Latest.Revision != doc.Revision) {
		err = errors.New("market tape archive identity mismatch")
	}
	return day, doc, exists, err
}

func saveTapeDay(ctx context.Context, store *corestore.Store, capture rpc.MarketTapeCapture, fingerprint string) error {
	for range 4 {
		day, doc, exists, err := loadTapeDay(ctx, store, capture.Session.Date)
		if err != nil {
			return err
		}
		if exists && (day.Fingerprint == fingerprint || capture.CapturedAt.Before(day.Latest.CapturedAt)) {
			return nil
		}
		capture.Revision = doc.Revision + 1
		if !exists {
			day.First = capture
		}
		day.Latest, day.Fingerprint = capture, fingerprint
		for _, leg := range []struct {
			price *rpc.MarketTapePrice
			saved **rpc.MarketTapeArchivedClose
		}{{capture.Session.SPX, &day.SPX}, {capture.Session.QQQ, &day.QQQ}} {
			if leg.price != nil && (*leg.saved == nil || (*leg.saved).Value != leg.price.Close) {
				*leg.saved = &rpc.MarketTapeArchivedClose{Value: leg.price.Close, Revision: capture.Revision, CapturedAt: capture.CapturedAt}
			}
		}
		payload, err := json.Marshal(capture)
		if err != nil {
			return err
		}
		projection, err := json.Marshal(day)
		if err != nil {
			return err
		}
		_, _, err = store.CompareAndSwapStateDocumentWithObservations(ctx, corestore.StateDocumentCAS{ScopeKey: tapeDayScope(capture.Session.Date), Kind: tapeSessionKind, ExpectedRevision: doc.Revision, JSON: projection}, []corestore.ObservationInput{{
			ScopeKey: tapeDayScope(capture.Session.Date), Source: "market-tape", Kind: tapeSessionKind,
			ObservedAt: capture.CapturedAt, ContentType: "application/json", Payload: payload,
			// Descriptive research records never become decision authority.
			DecisionEligible: false,
		}})
		if errors.Is(err, corestore.ErrRevisionConflict) {
			continue
		}
		return err
	}
	return errors.New("market tape archive changed concurrently")
}

func archiveMarketTape(ctx context.Context, store *corestore.Store, result *rpc.MarketTapeResult) (*rpc.MarketTapeArchiveStatus, error) {
	status, err := updateTapeArchiveStatus(ctx, store, result.AsOf, false)
	if err != nil {
		return nil, err
	}
	if len(result.Sessions) == 0 {
		return &status, nil
	}
	start, err := time.Parse("2006-01-02", result.Sessions[0].Date)
	if err != nil {
		return nil, err
	}
	calendar, err := tapeArchiveCalendar(start, 120)
	if err != nil {
		return nil, err
	}
	byDate := make(map[string]int, len(calendar))
	for i, session := range calendar {
		byDate[session.Date] = i
	}
	measured := 0
	for _, row := range result.Sessions {
		i, ok := byDate[row.Date]
		if !ok || i+1 >= len(calendar) || calendar[i].Close.Add(15*time.Minute).After(result.AsOf) {
			return nil, errors.New("market tape archive requires a completed official session")
		}
		// Do not fill an outage with empty permanent records. Missing dates are
		// still explicit in history; a partial measured row is retained.
		if row.SPX == nil && row.QQQ == nil && row.Breadth == nil && row.SPY == nil && row.VIX == nil && (row.Leaders == nil || row.Leaders.Price == nil && row.Leaders.Companies.Coverage50 == 0 && row.Leaders.Companies.CoverageAD == 0) {
			continue
		}
		capture, fingerprint, err := tapeArchiveCapture(row, result.Sources, result.AsOf)
		if err != nil {
			return nil, err
		}
		capture.Timing = "reconstructed"
		if calendar[i].Close.After(status.StartedAt) && result.AsOf.Before(calendar[i+1].Open) {
			capture.Timing = "before_next_open"
		}
		if err := saveTapeDay(ctx, store, capture, fingerprint); err != nil {
			return nil, err
		}
		measured++
	}
	if measured == 0 {
		return &status, errors.New("market tape has no measurements to archive")
	}
	status, err = updateTapeArchiveStatus(ctx, store, result.AsOf, true)
	return &status, err
}

func tapeArchiveCalendar(start time.Time, days int) ([]marketcal.Session, error) {
	// Noon UTC keeps an ISO date on the same New York calendar day.
	at := time.Date(start.Year(), start.Month(), start.Day(), 12, 0, 0, 0, time.UTC)
	result, err := marketcal.New().Query(marketcal.Query{Market: marketcal.MarketUSEquity, At: at, Days: days})
	if err != nil {
		return nil, err
	}
	var sessions []marketcal.Session
	for _, session := range result.Sessions {
		// The bounded lookback can extend before embedded calendar coverage.
		// Unknown edge dates are not invented sessions; callers require enough
		// known dates and leave out-of-coverage follow-ups unavailable.
		if session.State == marketcal.StateUnknown {
			continue
		}
		if !session.Close.IsZero() {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

func readMarketTapeHistory(ctx context.Context, store *corestore.Store, p rpc.MarketTapeParams, now time.Time) (*rpc.MarketTapeResult, error) {
	status, err := readTapeArchiveStatus(ctx, store)
	if err != nil {
		return nil, err
	}
	end := now.UTC()
	if p.Before != "" {
		before, err := time.Parse("2006-01-02", p.Before)
		if err != nil {
			return nil, err
		}
		if before.After(now.AddDate(0, 0, 1)) {
			return nil, errors.New("tape before cannot be in the future")
		}
		end = before
	}
	calendar, err := tapeArchiveCalendar(end.AddDate(0, 0, -180), 195)
	if err != nil {
		return nil, err
	}
	var indices []int
	for i, session := range calendar {
		if !session.Close.Add(15*time.Minute).After(now) && (p.Before == "" || session.Date < p.Before) {
			indices = append(indices, i)
		}
	}
	if len(indices) < p.Sessions {
		return nil, errors.New("market tape history calendar unavailable")
	}
	indices = indices[len(indices)-p.Sessions:]
	history := &rpc.MarketTapeHistory{Before: p.Before, NextBefore: calendar[indices[0]].Date, Rows: make([]rpc.MarketTapeHistoryRow, 0, p.Sessions)}
	loaded := make(map[string]tapeArchiveDay)
	for i := indices[0]; i <= min(indices[len(indices)-1]+5, len(calendar)-1); i++ {
		date := calendar[i].Date
		day, _, exists, err := loadTapeDay(ctx, store, date)
		if err != nil {
			return nil, err
		}
		if exists {
			loaded[date] = day
		}
	}
	covered := 0
	for _, i := range indices {
		date := calendar[i].Date
		row := rpc.MarketTapeHistoryRow{Date: date, FollowUps: make([]rpc.MarketTapeFollowUp, 0, 2)}
		anchor, exists := loaded[date]
		if exists {
			row.First, row.Latest = &anchor.First, &anchor.Latest
			covered++
		}
		for _, horizon := range []int{1, 3, 5} {
			follow := rpc.MarketTapeFollowUp{Sessions: horizon, Status: "unavailable"}
			if i+horizon < len(calendar) {
				target := calendar[i+horizon]
				follow.Date = target.Date
				if target.Close.Add(15 * time.Minute).After(now) {
					follow.Status = "pending"
				} else {
					last := loaded[target.Date]
					follow.SPXAnchor, follow.SPXEnd = anchor.SPX, last.SPX
					follow.QQQAnchor, follow.QQQEnd = anchor.QQQ, last.QQQ
					follow.SPXChangePct = tapeFollowUpChange(anchor.SPX, last.SPX)
					follow.QQQChangePct = tapeFollowUpChange(anchor.QQQ, last.QQQ)
					if follow.SPXChangePct != nil && follow.QQQChangePct != nil {
						follow.Status = "available"
					} else if follow.SPXChangePct != nil || follow.QQQChangePct != nil {
						follow.Status = "partial"
					}
				}
			}
			row.FollowUps = append(row.FollowUps, follow)
		}
		history.Rows = append(history.Rows, row)
	}
	coverage := "partial"
	if covered == len(indices) {
		coverage = "available"
	} else if covered == 0 {
		coverage = "unavailable"
	}
	return &rpc.MarketTapeResult{SchemaVersion: "market-tape-v1", AsOf: now, Timezone: "America/New_York", LatestSession: calendar[indices[len(indices)-1]].Date,
		NotPredictive: true, HistoricalAvailability: "see_capture_times", CoverageStatus: coverage, Sessions: []rpc.MarketTapeSession{}, Sources: []rpc.MarketTapeSource{}, Archive: &status, History: history,
		Notes: []string{
			"Permanent local records; collection runs while the daemon is running. First and latest versions are shown; all changed versions are retained in daemon.db.",
			"Reconstructed records describe sessions preceding collection or were captured after the next session opened; they are not evidence of a forecast made in advance.",
			"Follow-ups compare the latest available archived closes after one, three and five official trading sessions. They exclude dividends and costs and are not achievable trading returns.",
			"Corrections may change follow-ups. Each closing price names its captured revision. Pending means the target close has not settled; unavailable means a required measurement is missing.",
			fmt.Sprintf("%d of %d displayed sessions have an archived observation. No score, trading rule or predictive claim.", covered, len(indices)),
		}}, nil
}

func tapeFollowUpChange(first, last *rpc.MarketTapeArchivedClose) *float64 {
	if first == nil || last == nil || tapeFinite(first.Value) == nil || tapeFinite(last.Value) == nil || first.Value <= 0 || last.Value <= 0 {
		return nil
	}
	value := (last.Value/first.Value - 1) * 100
	return tapeFinite(value)
}
