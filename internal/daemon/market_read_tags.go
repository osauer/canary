package daemon

import (
	"math"
	"slices"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The levels market.read draws its tags at. They are heuristics of the read,
// returned with every result, and no decision reads them.
const (
	marketReadBarMinutes          = 5
	marketReadHoldBars            = 2
	marketReadStaleAfterMinutes   = 15
	marketReadBaselineMinSessions = 3
	marketReadContinuationRelVol  = 1.2
	marketReadThinRelVol          = 0.8
	marketReadLookbackMinutes     = 30
	marketReadFadingWindowVolume  = 0.7
	marketReadFadingRelVol        = 1.0
	marketReadSellingWindowVolume = 1.3
	marketReadVIXLine             = 25.0
	marketReadVIXRisingPct        = 5.0
	// marketReadVIXBand is how far beyond the line two closes must be for VIX
	// to cross back once a side holds. Replay of 2026-11-18, VIX between
	// 23.6 and 25.2 all session, swapped fear_easing and stress_elevated
	// three times without it.
	marketReadVIXBand         = 0.5
	marketReadGapMinPct       = 0.25
	marketReadGapHoldFraction = 0.5
	// marketReadGapAdvanceFraction: a session that opened beyond the prior
	// range advances only once it has moved this fraction of its opening gap
	// further; until then holding there is the gap holding.
	marketReadGapAdvanceFraction = 0.5
	marketReadLeadersMinGapped   = 3
	marketReadLeadersHoldShare   = 0.70
	marketReadLeadersFadeShare   = 0.40
)

const marketReadBar = marketReadBarMinutes * time.Minute

func marketReadThresholds() rpc.MarketReadThresholds {
	return rpc.MarketReadThresholds{
		Heuristic: true, BarMinutes: marketReadBarMinutes, HoldBars: marketReadHoldBars,
		StaleAfterMinutes: marketReadStaleAfterMinutes, BaselineMinSessions: marketReadBaselineMinSessions,
		ContinuationRelativeVolume: marketReadContinuationRelVol, ThinRelativeVolume: marketReadThinRelVol,
		LookbackMinutes: marketReadLookbackMinutes, FadingWindowVolume: marketReadFadingWindowVolume,
		FadingRelativeVolume: marketReadFadingRelVol, SellingWindowVolume: marketReadSellingWindowVolume,
		VIXLine: marketReadVIXLine, VIXRisingPct: marketReadVIXRisingPct, VIXBand: marketReadVIXBand,
		GapMinPct: marketReadGapMinPct, GapHoldFraction: marketReadGapHoldFraction, GapAdvanceFraction: marketReadGapAdvanceFraction,
		LeadersMinGapped: marketReadLeadersMinGapped, LeadersHoldShare: marketReadLeadersHoldShare, LeadersFadeShare: marketReadLeadersFadeShare,
	}
}

// marketReadSessions picks the session a read describes and the one before
// it from the official calendar: today's session (state pre, open or closed by
// the clock), or on a day without one the last that traded, read as closed.
func marketReadSessions(sessions []marketcal.Session, now time.Time) (session, prior marketcal.Session, state string, ok bool) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return session, prior, "", false
	}
	today := now.In(loc).Format(time.DateOnly)
	at := -1
	for i, candidate := range sessions {
		if candidate.Date == today {
			at = i
			break
		}
		if !candidate.Close.After(now) {
			at = i
		}
	}
	if at < 1 {
		return session, prior, "", false
	}
	session, prior = sessions[at], sessions[at-1]
	switch {
	case session.Date == today && now.Before(session.Open):
		state = rpc.MarketReadPre
	case session.Date == today && now.Before(session.Close):
		state = rpc.MarketReadOpen
	default:
		state = rpc.MarketReadClosed
	}
	return session, prior, state, true
}

// marketReadRegular reports whether a bar starting at at lies inside the
// session's regular hours.
func marketReadRegular(session marketcal.Session, at time.Time) bool {
	return !at.Before(session.Open) && at.Before(session.Close)
}

// marketReadBars keeps the session's regular-hours bars that had completed
// when the series was fetched. A bar still forming then is left out, so no
// tag rests on a close that can still move.
func marketReadBars(points []rpc.MarketHistoryPoint, session marketcal.Session, fetchedAt time.Time) []rpc.MarketReadBar {
	out := []rpc.MarketReadBar{}
	for _, p := range points {
		if !marketReadRegular(session, p.At) || p.At.Add(marketReadBar).After(fetchedAt) || !isFinitePrice(p.Value) {
			continue
		}
		bar := rpc.MarketReadBar{At: p.At, Open: p.Value, High: p.Value, Low: p.Value, Close: p.Value}
		if p.Open != nil && p.High != nil && p.Low != nil {
			bar.Open, bar.High, bar.Low = *p.Open, *p.High, *p.Low
		}
		out = append(out, bar)
	}
	return out
}

// marketReadVolume reads the volume proxy's completed buckets of the session
// against the same buckets of the prior sessions in the retained intraday
// series. A bucket is its offset from its own session's open, so a short
// session lacks only its late buckets. A bucket needs the minimum number of
// prior sessions to have a baseline, and the relative pace needs every
// completed bucket to have one.
func marketReadVolume(points []rpc.MarketHistoryPoint, session marketcal.Session, priors []marketcal.Session, fetchedAt time.Time) *rpc.MarketReadVolume {
	type tally struct {
		sum float64
		n   int
	}
	baseline := map[time.Duration]*tally{}
	withBars := 0
	for _, prior := range priors {
		seen := false
		for _, p := range points {
			if p.Volume == nil || *p.Volume < 0 || !marketReadRegular(prior, p.At) {
				continue
			}
			offset := p.At.Sub(prior.Open)
			t := baseline[offset]
			if t == nil {
				t = &tally{}
				baseline[offset] = t
			}
			t.sum += float64(*p.Volume)
			t.n++
			seen = true
		}
		if seen {
			withBars++
		}
	}
	out := &rpc.MarketReadVolume{Proxy: "SPY", Bars: []rpc.MarketReadVolumeBar{}}
	fewest := -1
	baselineCum, complete := 0.0, true
	for _, p := range points {
		if p.Volume == nil || *p.Volume < 0 || !marketReadRegular(session, p.At) || p.At.Add(marketReadBar).After(fetchedAt) {
			continue
		}
		bar := rpc.MarketReadVolumeBar{At: p.At, Volume: *p.Volume}
		out.Cumulative += *p.Volume
		n := 0
		if t := baseline[p.At.Sub(session.Open)]; t != nil {
			n = t.n
			if n >= marketReadBaselineMinSessions {
				bar.Baseline = new(t.sum / float64(t.n))
				baselineCum += *bar.Baseline
			}
		}
		if bar.Baseline == nil {
			complete = false
		}
		if fewest < 0 || n < fewest {
			fewest = n
		}
		out.Bars = append(out.Bars, bar)
	}
	out.BaselineSessions = fewest
	if fewest < 0 {
		out.BaselineSessions = withBars
	}
	switch {
	case len(out.Bars) == 0:
		out.RelativeUnavailable = "no completed regular-session bar yet"
	case !complete || out.BaselineSessions < marketReadBaselineMinSessions:
		out.RelativeUnavailable = "fewer than the minimum prior sessions recorded for today's buckets"
	case baselineCum <= 0:
		out.RelativeUnavailable = "prior sessions recorded no volume in today's buckets"
	default:
		out.BaselineCumulative = new(baselineCum)
		out.Relative = new(float64(out.Cumulative) / baselineCum)
	}
	return out
}

// marketReadLeaderState places one company against its opening gap. Falling
// under the prior session's low wins over every other state, a gap under the
// minimum is no gap, and between holding half the gap and giving it all back
// the gap still counts as held: a single read keeps no earlier state.
func marketReadLeaderState(row *rpc.MarketReadLeader) {
	switch {
	case row.PriorClose == nil || row.PriorLow == nil:
		row.StateUnavailable = "prior session daily bar unavailable"
		return
	case row.Open == nil:
		if row.StateUnavailable == "" {
			row.StateUnavailable = "session open unavailable"
		}
		return
	case row.Last == nil:
		row.StateUnavailable = "current price unavailable"
		return
	}
	prior, open, last := *row.PriorClose, *row.Open, *row.Last
	row.GapPct = tapeFinite(100 * (open/prior - 1))
	if row.GapPct == nil {
		row.StateUnavailable = "gap could not be measured"
		return
	}
	gap := *row.GapPct
	half := prior + marketReadGapHoldFraction*(open-prior)
	row.StateUnavailable = ""
	switch {
	case last < *row.PriorLow:
		row.State = rpc.LeaderBelowPriorLow
	case math.Abs(gap) < marketReadGapMinPct:
		row.State = rpc.LeaderNoGap
	case gap > 0 && last >= half:
		row.State = rpc.LeaderHoldsGapUp
	case gap > 0 && last <= prior:
		row.State = rpc.LeaderGapUpFilled
	case gap > 0:
		row.State = rpc.LeaderHoldsGapUp
	case last <= half:
		row.State = rpc.LeaderHoldsGapDown
	case last >= prior:
		row.State = rpc.LeaderGapDownReclaimed
	default:
		row.State = rpc.LeaderHoldsGapDown
	}
}

// marketReadLeaderCounts counts the companies that gapped up by the minimum
// and have a state, whatever it is, and those of them holding the gap.
func marketReadLeaderCounts(rows []rpc.MarketReadLeader) (gappedUp, holding int) {
	for _, row := range rows {
		if row.State == "" || row.GapPct == nil || *row.GapPct < marketReadGapMinPct {
			continue
		}
		gappedUp++
		if row.State == rpc.LeaderHoldsGapUp {
			holding++
		}
	}
	return gappedUp, holding
}

// marketReadTagInputs is everything the tags read, already measured.
// IndexBars, VolumeBars and VIXBars are completed regular-session bars of the
// session, oldest first; an empty slice is a source not measured.
type marketReadTagInputs struct {
	State string
	Prior *rpc.MarketReadPrior
	// SessionOpen is the index's opening price, from the bar that opened the
	// session; nil without it.
	SessionOpen   *float64
	IndexBars     []rpc.MarketReadBar
	Volume        *rpc.MarketReadVolume
	VIXBars       []rpc.MarketReadVIXBar
	VIXPriorClose *float64
	Leaders       []rpc.MarketReadLeader
}

type marketReadTagAt struct {
	code, tone string
	evidence   map[string]float64
}

// marketReadTags names the conditions that hold at the session's latest
// completed bar, and the timeline of every bar-based condition over the
// session. While the session is open that is now; once it has closed, the
// read keeps the tags as they stood at its last bar. Before the open there are
// none. Each bar-based tag is evaluated at every completed bar; consecutive
// bars with the same code are one event, from the close of its first bar to
// the close of the bar that ended it, and a tag's since is the start of the
// event still standing at the latest bar.
func marketReadTags(in marketReadTagInputs) ([]rpc.MarketReadTag, []rpc.MarketReadTagEvent) {
	tags, events := []rpc.MarketReadTag{}, []rpc.MarketReadTagEvent{}
	if in.State != rpc.MarketReadOpen && in.State != rpc.MarketReadClosed {
		return tags, events
	}
	add := func(scope string, times []time.Time, at func(k int) *marketReadTagAt) {
		var open *rpc.MarketReadTagEvent
		var last *marketReadTagAt
		for k := 1; k <= len(times); k++ {
			closed := times[k-1].Add(marketReadBar)
			last = at(k)
			if open != nil && (last == nil || last.code != open.Code) {
				open.To = closed
				events = append(events, *open)
				open = nil
			}
			if last != nil && open == nil {
				open = &rpc.MarketReadTagEvent{Code: last.code, Scope: scope, Tone: last.tone, From: closed}
			}
		}
		if open == nil {
			return
		}
		events = append(events, *open)
		tags = append(tags, rpc.MarketReadTag{Code: last.code, Scope: scope, Tone: last.tone, Since: open.From, Evidence: last.evidence})
	}
	indexTimes := make([]time.Time, len(in.IndexBars))
	for i, b := range in.IndexBars {
		indexTimes[i] = b.At
	}
	add("index", indexTimes, func(k int) *marketReadTagAt { return marketReadIndexTagAt(in, k) })
	add("volume", indexTimes, func(k int) *marketReadTagAt {
		now := marketReadVolumeTagAt(in, k)
		if now == nil || k < 2 {
			return nil
		}
		// One bar's condition is not enough: the previous bar held it too.
		if before := marketReadVolumeTagAt(in, k-1); before == nil || before.code != now.code {
			return nil
		}
		return now
	})
	vixTimes := make([]time.Time, len(in.VIXBars))
	for i, b := range in.VIXBars {
		vixTimes[i] = b.At
	}
	add("vix", vixTimes, func(k int) *marketReadTagAt { return marketReadVIXTagAt(in, k) })
	if tag := marketReadLeadersTag(in.Leaders); tag != nil {
		tags = append(tags, *tag)
	}
	slices.SortStableFunc(events, func(a, b rpc.MarketReadTagEvent) int { return a.From.Compare(b.From) })
	return tags, events
}

// marketReadHolds reports whether the last HoldBars closes up to bar k all
// satisfy beyond.
func marketReadHolds(closes func(i int) float64, k int, beyond func(float64) bool) bool {
	if k < marketReadHoldBars {
		return false
	}
	for i := k - marketReadHoldBars; i < k; i++ {
		if !beyond(closes(i)) {
			return false
		}
	}
	return true
}

// marketReadRelativeAt is the volume proxy's relative pace over the buckets
// completed by the end of index bar k, or nil without a baseline for each.
func marketReadRelativeAt(in marketReadTagInputs, k int) *float64 {
	if in.Volume == nil || in.Volume.BaselineSessions < marketReadBaselineMinSessions || k < 1 {
		return nil
	}
	end := in.IndexBars[k-1].At
	volume, baseline, buckets := 0.0, 0.0, 0
	for _, b := range in.Volume.Bars {
		if b.At.After(end) {
			continue
		}
		if b.Baseline == nil {
			return nil
		}
		volume += float64(b.Volume)
		baseline += *b.Baseline
		buckets++
	}
	if buckets == 0 || baseline <= 0 {
		return nil
	}
	return new(volume / baseline)
}

func marketReadIndexTagAt(in marketReadTagInputs, k int) *marketReadTagAt {
	if in.Prior == nil || k < 1 {
		return nil
	}
	closes := func(i int) float64 { return in.IndexBars[i].Close }
	evidence := map[string]float64{"prior_high": in.Prior.High, "prior_low": in.Prior.Low, "close": closes(k - 1)}
	relative := marketReadRelativeAt(in, k)
	if relative != nil {
		evidence["relative_volume"] = *relative
	}
	open, last := in.SessionOpen, closes(k-1)
	if open != nil {
		evidence["session_open"] = *open
	}
	switch {
	case marketReadHolds(closes, k, func(c float64) bool { return c > in.Prior.High }) &&
		open != nil && *open > in.Prior.High && last-*open < marketReadGapAdvanceFraction*(*open-in.Prior.Close):
		// The whole move came with the open; the session has not added to it.
		return &marketReadTagAt{"gap_holds", "neutral", evidence}
	case marketReadHolds(closes, k, func(c float64) bool { return c < in.Prior.Low }) &&
		open != nil && *open < in.Prior.Low && *open-last < marketReadGapAdvanceFraction*(in.Prior.Close-*open):
		return &marketReadTagAt{"gap_down_holds", "neutral", evidence}
	case marketReadHolds(closes, k, func(c float64) bool { return c > in.Prior.High }):
		switch {
		case relative != nil && *relative >= marketReadContinuationRelVol:
			return &marketReadTagAt{"continuation", "positive", evidence}
		case relative != nil && *relative < marketReadThinRelVol:
			return &marketReadTagAt{"breakout_thin_volume", "caution", evidence}
		default:
			return &marketReadTagAt{"above_prior_high", "neutral", evidence}
		}
	case marketReadHolds(closes, k, func(c float64) bool { return c < in.Prior.Low }):
		return &marketReadTagAt{"bounce_failed", "negative", evidence}
	default:
		return &marketReadTagAt{"inside_prior_range", "neutral", evidence}
	}
}

// marketReadVolumeTagAt compares the index over the lookback with the volume
// proxy's pace over the same buckets, at index bar k.
func marketReadVolumeTagAt(in marketReadTagInputs, k int) *marketReadTagAt {
	lookback := marketReadLookbackMinutes / marketReadBarMinutes
	if in.Volume == nil || in.Volume.BaselineSessions < marketReadBaselineMinSessions || k <= lookback {
		return nil
	}
	now, then := in.IndexBars[k-1], in.IndexBars[k-1-lookback]
	if now.At.Sub(then.At) != marketReadLookbackMinutes*time.Minute {
		return nil // a missing bar would stretch the window
	}
	volume, baseline, buckets := 0.0, 0.0, 0
	for _, b := range in.Volume.Bars {
		if !b.At.After(then.At) || b.At.After(now.At) {
			continue
		}
		if b.Baseline == nil {
			return nil
		}
		volume += float64(b.Volume)
		baseline += *b.Baseline
		buckets++
	}
	if buckets != lookback || baseline <= 0 {
		return nil
	}
	ratio := volume / baseline
	evidence := map[string]float64{"close": now.Close, "close_lookback_ago": then.Close, "window_volume": volume, "window_baseline": baseline, "window_ratio": ratio}
	relative := marketReadRelativeAt(in, k)
	if relative != nil {
		evidence["relative_volume"] = *relative
	}
	switch {
	case now.Close > then.Close && ratio < marketReadFadingWindowVolume && relative != nil && *relative < marketReadFadingRelVol:
		return &marketReadTagAt{"rally_fading", "caution", evidence}
	case now.Close < then.Close && ratio >= marketReadSellingWindowVolume:
		return &marketReadTagAt{"selling_on_volume", "negative", evidence}
	}
	return nil
}

func marketReadVIXTagAt(in marketReadTagInputs, k int) *marketReadTagAt {
	if k < 1 {
		return nil
	}
	closes := func(i int) float64 { return in.VIXBars[i].Close }
	last := closes(k - 1)
	evidence := map[string]float64{"line": marketReadVIXLine, "close": last}
	var change *float64
	if in.VIXPriorClose != nil && *in.VIXPriorClose > 0 {
		change = tapeFinite(100 * (last / *in.VIXPriorClose - 1))
		evidence["prior_close"] = *in.VIXPriorClose
		if change != nil {
			evidence["change_pct"] = *change
		}
	}
	side := marketReadVIXSide(closes, k)
	switch {
	case side > 0:
		if change != nil && *change >= marketReadVIXRisingPct {
			return &marketReadTagAt{"stress_rising", "negative", evidence}
		}
		if change != nil && *change <= -marketReadVIXRisingPct {
			// Still above the line, but falling as fast as a rising day climbs.
			return &marketReadTagAt{"stress_easing", "neutral", evidence}
		}
		return &marketReadTagAt{"stress_elevated", "caution", evidence}
	case side < 0 && change != nil && *change < 0 &&
		(*in.VIXPriorClose >= marketReadVIXLine || *change <= -marketReadVIXRisingPct):
		// Easing is news only from fear: VIX came down through the line, or
		// fell by as much as a rising day would add. A calm day drifting lower
		// is not it.
		return &marketReadTagAt{"fear_easing", "positive", evidence}
	}
	return nil
}

// marketReadVIXSide is the side of its line VIX holds at bar k: +1 above,
// -1 below, 0 neither yet. The first side needs two closes beyond the line;
// once a side holds, crossing back needs two closes beyond the line by the
// band, so VIX trading at the line does not swap sides every few bars.
func marketReadVIXSide(closes func(int) float64, k int) int {
	side := 0
	for j := 1; j <= k; j++ {
		up, down := marketReadVIXLine, marketReadVIXLine
		switch side {
		case 1:
			down -= marketReadVIXBand
		case -1:
			up += marketReadVIXBand
		}
		if marketReadHolds(closes, j, func(c float64) bool { return c > up }) {
			side = 1
		} else if marketReadHolds(closes, j, func(c float64) bool { return c < down }) {
			side = -1
		}
	}
	return side
}

// marketReadLeadersTag reads the current leader states. They come from
// current quotes, which keep no history, so the tag has no since.
func marketReadLeadersTag(rows []rpc.MarketReadLeader) *rpc.MarketReadTag {
	gapped, holding := marketReadLeaderCounts(rows)
	if gapped < marketReadLeadersMinGapped {
		return nil
	}
	share := float64(holding) / float64(gapped)
	tag := &rpc.MarketReadTag{Scope: "leaders", SinceUnavailable: "leader states are read from current quotes, which keep no history", Evidence: map[string]float64{"gapped_up": float64(gapped), "holding_gap_up": float64(holding), "holding_share": share}}
	switch {
	case share >= marketReadLeadersHoldShare:
		tag.Code, tag.Tone = "leaders_hold", "positive"
	case share <= marketReadLeadersFadeShare:
		tag.Code, tag.Tone = "leaders_fading", "negative"
	default:
		return nil
	}
	return tag
}
