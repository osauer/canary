package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

// All prices and volumes here are invented. The dates only select official
// US sessions from the embedded calendar.

type readMarket struct {
	t        *testing.T
	sessions []marketcal.Session
	session  marketcal.Session
	prior    marketcal.Session
	priors   []marketcal.Session
	src      marketReadSources
}

// newReadMarket sets up a regular session (2026-09-23) whose prior session
// ranged 90 to 100 and closed at 95.
func newReadMarket(t *testing.T) *readMarket {
	t.Helper()
	sessions, err := tapeArchiveCalendar(time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC), 50)
	if err != nil {
		t.Fatal(err)
	}
	m := &readMarket{t: t, sessions: sessions}
	for i, s := range sessions {
		if s.Date == "2026-09-23" {
			m.session, m.prior, m.priors = s, sessions[i-1], sessions[:i]
		}
	}
	if m.session.Date == "" {
		t.Fatal("fixture session missing from the calendar")
	}
	priorDay, _ := time.Parse(time.DateOnly, m.prior.Date)
	m.src.SPXDaily = &rpc.MarketHistoryResult{Contract: marketReadSPX, Interval: "1 day", RegularHoursOnly: true,
		Points: []rpc.MarketHistoryPoint{{At: priorDay, Value: 95, Open: new(92.0), High: new(100.0), Low: new(90.0)}}}
	return m
}

// at is the end of the session's nth bar (1-based) plus a minute.
func (m *readMarket) at(bars int) time.Time {
	return m.session.Open.Add(time.Duration(bars)*marketReadBar + time.Minute)
}

func intraday(symbol string, fetched time.Time, points []rpc.MarketHistoryPoint) *rpc.MarketHistoryResult {
	return &rpc.MarketHistoryResult{Contract: rpc.ContractParams{Symbol: symbol}, Interval: "5 mins", AsOf: fetched, Points: points}
}

// index sets the session's SPX five-minute closes, fetched at fetched.
func (m *readMarket) index(fetched time.Time, closes ...float64) {
	var points []rpc.MarketHistoryPoint
	for i, c := range closes {
		points = append(points, rpc.MarketHistoryPoint{At: m.session.Open.Add(time.Duration(i) * marketReadBar), Value: c})
	}
	m.src.SPXIntraday = intraday("SPX", fetched, points)
}

// volume records priorSessions sessions of `usual` shares per bucket and the
// session's own buckets.
func (m *readMarket) volume(fetched time.Time, priorSessions int, usual int64, today ...int64) {
	var points []rpc.MarketHistoryPoint
	for _, prior := range m.priors[len(m.priors)-priorSessions:] {
		for i := range 78 {
			points = append(points, rpc.MarketHistoryPoint{At: prior.Open.Add(time.Duration(i) * marketReadBar), Value: 500, Volume: new(usual)})
		}
	}
	for i, v := range today {
		points = append(points, rpc.MarketHistoryPoint{At: m.session.Open.Add(time.Duration(i) * marketReadBar), Value: 500, Volume: new(v)})
	}
	m.src.SPYIntraday = intraday("SPY", fetched, points)
}

func (m *readMarket) vix(fetched time.Time, prior float64, closes ...float64) {
	var points []rpc.MarketHistoryPoint
	for i, c := range closes {
		points = append(points, rpc.MarketHistoryPoint{At: m.session.Open.Add(time.Duration(i) * marketReadBar), Value: c})
	}
	m.src.VIXIntraday = intraday("VIX", fetched, points)
	m.src.VIXQuote = &broker.BriefQuote{Close: prior}
}

// leader adds a company whose prior session closed at 100 with a low of 98.
func (m *readMarket) leader(symbol string, open, last *float64) {
	priorDay, _ := time.Parse(time.DateOnly, m.prior.Date)
	q := &broker.BriefQuote{Close: 100}
	if open != nil {
		q.Open = *open
	}
	if last != nil {
		q.Last = *last
	}
	m.src.Leaders = append(m.src.Leaders, marketReadLeaderSource{Symbol: symbol, Quote: q,
		Daily: &rpc.MarketHistoryResult{Contract: rpc.ContractParams{Symbol: symbol}, Interval: "1 day", RegularHoursOnly: true, Points: []rpc.MarketHistoryPoint{{At: priorDay, Value: 100, Low: new(98.0)}}}})
}

func (m *readMarket) read(now time.Time) *rpc.MarketReadResult {
	m.t.Helper()
	out, err := assembleMarketRead(now, m.sessions, m.src)
	if err != nil {
		m.t.Fatal(err)
	}
	return out
}

func tagOf(r *rpc.MarketReadResult, scope string) *rpc.MarketReadTag {
	for i := range r.Tags {
		if r.Tags[i].Scope == scope {
			return &r.Tags[i]
		}
	}
	return nil
}

func codeOf(r *rpc.MarketReadResult, scope string) string {
	if tag := tagOf(r, scope); tag != nil {
		return tag.Code
	}
	return ""
}

func TestMarketReadIndexLevelHoldsOnlyOnTwoCompletedCloses(t *testing.T) {
	m := newReadMarket(t)
	now := m.at(3)
	m.index(now, 95, 99, 101) // one close above the prior high
	if got := codeOf(m.read(now), "index"); got != "inside_prior_range" {
		t.Fatalf("one close above the prior high: %q, want inside_prior_range", got)
	}
	now = m.at(4)
	m.index(now, 95, 99, 101, 102)
	r := m.read(now)
	tag := tagOf(r, "index")
	if tag == nil || tag.Code != "above_prior_high" || tag.Tone != "neutral" {
		t.Fatalf("two closes above with no volume: %+v", tag)
	}
	if want := m.session.Open.Add(4 * marketReadBar); !tag.Since.Equal(want) {
		t.Errorf("since %s, want the close of the second bar above, %s", tag.Since, want)
	}
	if tag.Evidence["prior_high"] != 100 || tag.Evidence["close"] != 102 {
		t.Errorf("evidence: %v", tag.Evidence)
	}
	// A dip back inside breaks the hold at once.
	m.index(m.at(5), 95, 99, 101, 102, 99.5)
	if got := codeOf(m.read(m.at(5)), "index"); got != "inside_prior_range" {
		t.Errorf("back inside the range: %q", got)
	}
	// A bar still forming when the series was read draws no tag.
	m.index(m.at(4).Add(-2*time.Minute), 95, 99, 101, 102)
	if r := m.read(m.at(4)); len(r.Index.Bars) != 3 || codeOf(r, "index") != "inside_prior_range" {
		t.Errorf("forming bar counted: %d bars, %q", len(r.Index.Bars), codeOf(r, "index"))
	}
	m.index(m.at(4), 95, 89, 88, 87)
	if got := tagOf(m.read(m.at(4)), "index"); got == nil || got.Code != "bounce_failed" || got.Tone != "negative" {
		t.Errorf("held below the prior low: %+v", got)
	}
}

func TestMarketReadBreakoutReadsVolumeAgainstTheSameTimeOfDay(t *testing.T) {
	for _, tc := range []struct {
		today int64
		code  string
		tone  string
	}{{130, "continuation", "positive"}, {70, "breakout_thin_volume", "caution"}, {100, "above_prior_high", "neutral"}} {
		m := newReadMarket(t)
		now := m.at(3)
		m.index(now, 101, 102, 103)
		m.volume(now, 3, 100, tc.today, tc.today, tc.today)
		r := m.read(now)
		if tag := tagOf(r, "index"); tag == nil || tag.Code != tc.code || tag.Tone != tc.tone {
			t.Errorf("today %d per bucket against 100: %+v, want %s", tc.today, tag, tc.code)
		}
		v := r.Index.Volume
		if v == nil || v.Relative == nil || v.BaselineSessions != 3 || v.Cumulative != 3*tc.today || *v.BaselineCumulative != 300 {
			t.Errorf("volume: %+v", v)
		}
	}
}

func TestMarketReadVolumeBaselineNeedsThreePriorSessions(t *testing.T) {
	m := newReadMarket(t)
	now := m.at(3)
	m.index(now, 101, 102, 103)
	m.volume(now, 2, 100, 150, 150, 150)
	r := m.read(now)
	v := r.Index.Volume
	if v == nil || v.Relative != nil || v.BaselineCumulative != nil || v.RelativeUnavailable == "" || v.BaselineSessions != 2 {
		t.Fatalf("two prior sessions: %+v", v)
	}
	if got := codeOf(r, "index"); got != "above_prior_high" {
		t.Errorf("breakout without a baseline: %q, want above_prior_high", got)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), `"relative":`) || !strings.Contains(string(raw), `"relative_unavailable":`) {
		t.Errorf("missing relative volume must be absent with a reason: %s", raw)
	}
}

func TestMarketReadVolumeTagsHoldOverTwoBars(t *testing.T) {
	rising := []float64{95, 95.5, 96, 96.5, 97, 97.5, 98, 98.5, 99}
	falling := []float64{99, 98.5, 98, 97.5, 97, 96.5, 96, 95.5, 95}
	for _, tc := range []struct {
		name    string
		closes  []float64
		volumes []int64
		code    string
	}{
		{"rally on light volume", rising, []int64{100, 100, 50, 50, 50, 50, 50, 50, 50}, "rally_fading"},
		{"sell-off on heavy volume", falling, []int64{100, 100, 150, 150, 150, 150, 150, 150, 150}, "selling_on_volume"},
		{"rally on usual volume", rising, []int64{100, 100, 100, 100, 100, 100, 100, 100, 100}, ""},
	} {
		m := newReadMarket(t)
		now := m.at(len(tc.closes))
		m.index(now, tc.closes...)
		m.volume(now, 4, 100, tc.volumes...)
		r := m.read(now)
		tag := tagOf(r, "volume")
		if got := codeOf(r, "volume"); got != tc.code {
			t.Errorf("%s: volume tag %q, want %q", tc.name, got, tc.code)
			continue
		}
		if tag != nil && tag.Evidence["window_ratio"] == 0 {
			t.Errorf("%s: no window evidence: %v", tc.name, tag.Evidence)
		}
	}
	// The first bar the condition is met on is not enough.
	m := newReadMarket(t)
	now := m.at(8)
	m.index(now, rising[:8]...)
	m.volume(now, 4, 100, 100, 100, 100, 100, 100, 100, 100, 10)
	if got := codeOf(m.read(now), "volume"); got != "" {
		t.Errorf("one bar's light volume: %q", got)
	}
}

func TestMarketReadVIXTags(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prior  float64
		closes []float64
		code   string
	}{
		{"easing below the line", 22, []float64{21, 20.5}, "fear_easing"},
		{"below the line but up", 20, []float64{21, 20.5}, ""},
		{"above the line", 26, []float64{26.5, 26.8}, "stress_elevated"},
		{"above the line and up 5%", 25, []float64{26.5, 26.8}, "stress_rising"},
		{"one close above", 24, []float64{24.5, 25.5}, ""},
	} {
		m := newReadMarket(t)
		now := m.at(len(tc.closes))
		m.vix(now, tc.prior, tc.closes...)
		if got := codeOf(m.read(now), "vix"); got != tc.code {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.code)
		}
	}
}

func TestMarketReadLeaderStates(t *testing.T) {
	for _, tc := range []struct {
		open, last float64
		state      string
	}{
		{102, 101.5, rpc.LeaderHoldsGapUp},
		{102, 100.5, rpc.LeaderHoldsGapUp}, // between half the gap and the prior close
		{102, 100, rpc.LeaderGapUpFilled},
		{102, 97, rpc.LeaderBelowPriorLow},
		{100.1, 99, rpc.LeaderNoGap},
		{100.1, 97.5, rpc.LeaderBelowPriorLow}, // the prior low wins over no gap
		{99, 98.8, rpc.LeaderHoldsGapDown},
		{99, 100, rpc.LeaderGapDownReclaimed},
		{99, 99.7, rpc.LeaderHoldsGapDown},
	} {
		row := rpc.MarketReadLeader{PriorClose: new(100.0), PriorLow: new(98.0), Open: new(tc.open), Last: new(tc.last)}
		marketReadLeaderState(&row)
		if row.State != tc.state || row.StateUnavailable != "" {
			t.Errorf("open %v last %v: %q (%s), want %q", tc.open, tc.last, row.State, row.StateUnavailable, tc.state)
		}
	}
}

func TestMarketReadLeadersTagCountsGapsThatHold(t *testing.T) {
	open, holds, fills := new(102.0), new(101.8), new(99.5)
	m := newReadMarket(t)
	now := m.at(3)
	m.leader("AAA", open, holds)
	m.leader("BBB", open, holds)
	m.leader("CCC", open, holds)
	m.leader("DDD", nil, holds) // no open: no state, not counted
	r := m.read(now)
	if tag := tagOf(r, "leaders"); tag == nil || tag.Code != "leaders_hold" || !tag.Since.IsZero() || tag.SinceUnavailable == "" {
		t.Fatalf("three of three hold: %+v", tag)
	}
	if r.Leaders.GappedUp != 3 || r.Leaders.HoldingGapUp != 3 {
		t.Errorf("counts %d/%d", r.Leaders.HoldingGapUp, r.Leaders.GappedUp)
	}
	missing := r.Leaders.Rows[3]
	if missing.State != "" || missing.StateUnavailable == "" || missing.GapPct != nil {
		t.Errorf("leader without an open: %+v", missing)
	}

	m = newReadMarket(t)
	m.leader("AAA", open, holds)
	m.leader("BBB", open, fills)
	m.leader("CCC", open, new(97.0)) // below its prior low, still a gap-up name
	if got := codeOf(m.read(now), "leaders"); got != "leaders_fading" {
		t.Errorf("one of three holds: %q, want leaders_fading", got)
	}

	m = newReadMarket(t)
	m.leader("AAA", open, holds)
	m.leader("BBB", open, holds)
	if got := codeOf(m.read(now), "leaders"); got != "" {
		t.Errorf("two gapped up: %q, want no tag", got)
	}
}

func TestMarketReadOutsideTheSessionDrawsNoTags(t *testing.T) {
	m := newReadMarket(t)
	open := m.session.Open
	m.index(open.Add(-time.Hour), 101, 102, 103)
	m.vix(open.Add(-time.Hour), 22, 21, 20)
	m.leader("AAA", new(102.0), new(101.8))
	pre := m.read(open.Add(-time.Hour))
	if pre.Session.State != rpc.MarketReadPre || pre.Session.Date != m.session.Date || len(pre.Tags) != 0 {
		t.Fatalf("before the open: %+v, tags %+v", pre.Session, pre.Tags)
	}
	if row := pre.Leaders.Rows[0]; row.Open != nil || row.State != "" || row.StateUnavailable == "" {
		t.Errorf("a leader before the open: %+v", row)
	}
	if pre.Index.Prior == nil || pre.Index.Prior.Date != m.prior.Date {
		t.Errorf("before the open the prior session is still the last one: %+v", pre.Index.Prior)
	}

	// A Saturday reads the Friday session, closed.
	saturday := time.Date(2026, 9, 26, 15, 0, 0, 0, time.UTC)
	m.index(saturday, 101, 102, 103)
	closed := m.read(saturday)
	if closed.Session.State != rpc.MarketReadClosed || closed.Session.Date != "2026-09-25" || len(closed.Tags) != 0 {
		t.Fatalf("weekend: %+v, tags %+v", closed.Session, closed.Tags)
	}
	if closed.Index.Prior != nil || closed.Index.PriorUnavailable == "" {
		t.Errorf("Friday's prior session has no bar in the fixture, yet: %+v", closed.Index.Prior)
	}
}

func TestMarketReadStaleBarsDrawNoTags(t *testing.T) {
	m := newReadMarket(t)
	now := m.at(10)
	m.index(m.at(4), 95, 99, 101, 102)
	r := m.read(now)
	if r.Index.BarsUnavailable == "" || len(r.Index.Bars) != 0 || codeOf(r, "index") != "" {
		t.Errorf("bars read %s ago: %+v, index tag %q", now.Sub(m.at(4)), r.Index, codeOf(r, "index"))
	}
}

func TestMarketReadMissingSourcesAreAbsentWithAReason(t *testing.T) {
	m := newReadMarket(t)
	m.src.SPXDaily = nil
	r := m.read(m.at(3))
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	index := doc["index"].(map[string]any)
	for _, key := range []string{"prior", "last", "position", "volume"} {
		if _, present := index[key]; present {
			t.Errorf("index.%s present without data: %s", key, raw)
		}
		if reason, _ := index[key+"_unavailable"].(string); reason == "" {
			t.Errorf("index.%s has no reason", key)
		}
	}
	vix := doc["vix"].(map[string]any)
	for _, key := range []string{"last", "prior_close", "change_pct"} {
		if _, present := vix[key]; present {
			t.Errorf("vix.%s present without data", key)
		}
	}
	if th := doc["thresholds"].(map[string]any); th["heuristic"] != true || th["vix_line"] != 25.0 {
		t.Errorf("thresholds: %v", th)
	}
}

// Callers within the cache window share one read; a caller after it gets a
// new one. Without a broker every input is missing, and the read says so.
func TestMarketReadAnswersPollersFromOneRead(t *testing.T) {
	now := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }}
	first, err := s.handleMarketRead(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Session.State != rpc.MarketReadOpen || first.Index.PriorUnavailable == "" || len(first.Tags) != 0 {
		t.Fatalf("read without a broker: %+v", first)
	}
	now = now.Add(marketReadCacheFor - time.Second)
	if again, err := s.handleMarketRead(t.Context(), &rpc.Request{}); err != nil || again != first {
		t.Fatalf("a caller inside the window started another read: %v", err)
	}
	now = now.Add(2 * time.Second)
	if later, err := s.handleMarketRead(t.Context(), &rpc.Request{}); err != nil || later == first || !later.AsOf.Equal(now) {
		t.Fatalf("a caller after the window got the old read: %v", err)
	}
	if _, err := s.handleMarketRead(t.Context(), &rpc.Request{Params: []byte(`{"symbol":"SPY"}`)}); err == nil {
		t.Error("a caller-supplied symbol was accepted")
	}
}
