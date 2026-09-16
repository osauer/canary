package marketcal

import (
	"fmt"
	"strings"
	"time"
)

const (
	defaultDays = 14
	maxDays     = 400
)

// NormalizeMarket maps CLI/MCP-friendly aliases to the stable Market token.
func NormalizeMarket(v string) (Market, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "us", "usa", "us-equity", "us_equity", "equity", "equities", "stocks", "stock":
		return MarketUSEquity, true
	case "us-options", "us_options", "options", "option":
		return MarketUSOptions, true
	case "de", "de-xetra", "de_xetra", "xetra", "germany", "ibis":
		return MarketDEXetra, true
	case "uk", "uk_lse", "uk-lse", "lse", "london":
		return MarketUKLSE, true
	case "jp", "jp_tse", "jp-tse", "tse", "tsej", "tokyo":
		return MarketJPTSE, true
	case "hk", "hk_hkex", "hk-hkex", "hkex", "sehk":
		return MarketHKHKEX, true
	default:
		return "", false
	}
}

// Calendar is the official-calendar query engine.
type Calendar struct {
	clock func() time.Time
}

// New returns a Calendar using time.Now for default/as-of queries.
func New() *Calendar {
	return &Calendar{clock: time.Now}
}

// NewWithClock returns a Calendar with an injected clock for tests.
func NewWithClock(clock func() time.Time) *Calendar {
	if clock == nil {
		clock = time.Now
	}
	return &Calendar{clock: clock}
}

// Query returns current/date context plus an optional forward session list.
func (c *Calendar) Query(q Query) (Result, error) {
	spec, ok := specs[q.Market]
	if !ok {
		return Result{}, fmt.Errorf("unsupported market %q", q.Market)
	}
	loc, err := time.LoadLocation(spec.timezone)
	if err != nil {
		return Result{}, fmt.Errorf("load %s: %w", spec.timezone, err)
	}
	at, err := c.queryTime(q, loc)
	if err != nil {
		return Result{}, err
	}
	days := q.Days
	if days <= 0 {
		days = defaultDays
	}
	if days > maxDays {
		days = maxDays
	}
	session := c.sessionAt(spec, loc, at)
	sessions := make([]Session, 0, days)
	startDate := localDate(at.In(loc))
	for i := range days {
		day := startDate.AddDate(0, 0, i)
		if day.Equal(startDate) {
			sessions = append(sessions, session)
			continue
		}
		sessions = append(sessions, c.sessionForDate(spec, loc, day, time.Time{}))
	}
	return Result{
		Market:        spec.market,
		Label:         spec.label,
		Timezone:      spec.timezone,
		AsOf:          c.clock(),
		CoverageStart: spec.coverageStart,
		CoverageEnd:   spec.coverageEnd,
		Source:        spec.source,
		SourceURL:     spec.sourceURL,
		Session:       session,
		Sessions:      sessions,
	}, nil
}

// SessionAt returns one market's state at an instant.
func (c *Calendar) SessionAt(market Market, at time.Time) (Session, error) {
	res, err := c.Query(Query{Market: market, At: at, Days: 1})
	if err != nil {
		return Session{}, err
	}
	return res.Session, nil
}

func (c *Calendar) queryTime(q Query, loc *time.Location) (time.Time, error) {
	if !q.At.IsZero() {
		return q.At, nil
	}
	if strings.TrimSpace(q.Date) != "" {
		day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(q.Date), loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("date must be YYYY-MM-DD: %w", err)
		}
		return time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc), nil
	}
	return c.clock(), nil
}

func (c *Calendar) sessionAt(spec calendarSpec, loc *time.Location, at time.Time) Session {
	local := at.In(loc)
	return c.sessionForDate(spec, loc, localDate(local), local)
}

func (c *Calendar) sessionForDate(spec calendarSpec, loc *time.Location, day, at time.Time) Session {
	date := day.Format("2006-01-02")
	s := Session{
		Market:        spec.market,
		Label:         spec.label,
		Date:          date,
		Timezone:      spec.timezone,
		Source:        spec.source,
		SourceURL:     spec.sourceURL,
		CoverageStart: spec.coverageStart,
		CoverageEnd:   spec.coverageEnd,
		Notes:         spec.notes,
	}
	if date < spec.coverageStart || date > spec.coverageEnd {
		s.State = StateUnknown
		s.Reason = "outside embedded official calendar coverage"
		return s
	}
	if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		s.State = StateClosed
		s.Reason = "weekend"
		c.attachNext(spec, loc, &s, day)
		return s
	}
	if reason, ok := spec.holidays[date]; ok {
		s.State = StateHoliday
		s.Reason = reason
		c.attachNext(spec, loc, &s, day)
		return s
	}

	closeHM := spec.close
	if override, ok := spec.earlyCloses[date]; ok {
		closeHM = override.close
		s.State = StateEarlyClose
		s.Reason = override.reason
	} else {
		s.State = StateRegular
	}
	s.Open = atLocal(day, loc, spec.open)
	s.Close = atLocal(day, loc, closeHM)
	start := s.Open
	for _, pause := range spec.breaks {
		pauseStart, pauseEnd := atLocal(day, loc, pause.start), atLocal(day, loc, pause.end)
		if !pauseStart.Before(s.Close) {
			break
		}
		if start.Before(pauseStart) {
			s.Windows = append(s.Windows, Window{Open: start, Close: pauseStart})
		}
		start = pauseEnd
	}
	if start.Before(s.Close) {
		s.Windows = append(s.Windows, Window{Open: start, Close: s.Close})
	}
	for _, window := range s.Windows {
		if !at.IsZero() && !at.Before(window.Open) && at.Before(window.Close) {
			s.IsOpen = true
		}
	}
	if !s.IsOpen && !at.IsZero() {
		for _, window := range s.Windows {
			if at.Before(window.Open) {
				open, closeT := window.Open, window.Close
				s.NextOpen, s.NextClose = &open, &closeT
				break
			}
		}
		if s.NextOpen == nil {
			c.attachNext(spec, loc, &s, day)
		}
	}
	return s
}

func (c *Calendar) attachNext(spec calendarSpec, loc *time.Location, s *Session, day time.Time) {
	next := c.nextTradable(spec, loc, day.AddDate(0, 0, 1))
	if next.Open.IsZero() {
		return
	}
	open := next.Open
	closeT := next.Windows[0].Close
	s.NextOpen = &open
	s.NextClose = &closeT
}

func (c *Calendar) nextTradable(spec calendarSpec, loc *time.Location, start time.Time) Session {
	for i := 0; i <= maxDays; i++ {
		day := localDate(start.In(loc)).AddDate(0, 0, i)
		s := c.sessionForDate(spec, loc, day, time.Time{})
		if s.State == StateRegular || s.State == StateEarlyClose {
			return s
		}
		if s.State == StateUnknown {
			return s
		}
	}
	return Session{Market: spec.market, Date: start.Format("2006-01-02"), State: StateUnknown, Reason: "no open session inside lookup horizon"}
}

func localDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func atLocal(day time.Time, loc *time.Location, v hm) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), v.h, v.m, 0, 0, loc)
}
