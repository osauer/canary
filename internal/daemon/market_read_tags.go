package daemon

import (
	"math"
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
	marketReadGapMinPct           = 0.25
	marketReadGapHoldFraction     = 0.5
	marketReadLeadersMinGapped    = 3
	marketReadLeadersHoldShare    = 0.70
	marketReadLeadersFadeShare    = 0.40
)

const marketReadBar = marketReadBarMinutes * time.Minute

func marketReadThresholds() rpc.MarketReadThresholds {
	return rpc.MarketReadThresholds{
		Heuristic: true, BarMinutes: marketReadBarMinutes, HoldBars: marketReadHoldBars,
		StaleAfterMinutes: marketReadStaleAfterMinutes, BaselineMinSessions: marketReadBaselineMinSessions,
		ContinuationRelativeVolume: marketReadContinuationRelVol, ThinRelativeVolume: marketReadThinRelVol,
		LookbackMinutes: marketReadLookbackMinutes, FadingWindowVolume: marketReadFadingWindowVolume,
		FadingRelativeVolume: marketReadFadingRelVol, SellingWindowVolume: marketReadSellingWindowVolume,
		VIXLine: marketReadVIXLine, VIXRisingPct: marketReadVIXRisingPct,
		GapMinPct: marketReadGapMinPct, GapHoldFraction: marketReadGapHoldFraction,
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
	State         string
	Prior         *rpc.MarketReadPrior
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

// marketReadTags names the conditions that hold now. They are drawn only
// while the regular session is open. Each bar-based tag is evaluated at every
// completed bar, and its since is the close of the first bar of the unbroken
// run that ends at the latest one.
func marketReadTags(in marketReadTagInputs) []rpc.MarketReadTag {
	tags := []rpc.MarketReadTag{}
	if in.State != rpc.MarketReadOpen {
		return tags
	}
	add := func(scope string, times []time.Time, at func(k int) *marketReadTagAt) {
		n := len(times)
		if n == 0 {
			return
		}
		last := at(n)
		if last == nil {
			return
		}
		since := n
		for k := n - 1; k >= 1; k-- {
			if earlier := at(k); earlier == nil || earlier.code != last.code {
				break
			}
			since = k
		}
		tags = append(tags, rpc.MarketReadTag{Code: last.code, Scope: scope, Tone: last.tone, Since: times[since-1].Add(marketReadBar), Evidence: last.evidence})
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
	return tags
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
	switch {
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
	switch {
	case marketReadHolds(closes, k, func(c float64) bool { return c > marketReadVIXLine }):
		if change != nil && *change >= marketReadVIXRisingPct {
			return &marketReadTagAt{"stress_rising", "negative", evidence}
		}
		return &marketReadTagAt{"stress_elevated", "caution", evidence}
	case marketReadHolds(closes, k, func(c float64) bool { return c < marketReadVIXLine }) && change != nil && *change < 0:
		return &marketReadTagAt{"fear_easing", "positive", evidence}
	}
	return nil
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
