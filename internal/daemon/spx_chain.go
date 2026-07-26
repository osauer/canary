package daemon

import (
	"fmt"
	"sort"
	"strings"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// pickedExpiration carries one (date, trading-class, strikes) tuple
// the gamma compute walks to build per-leg jobs. Uniform shape across
// single-class underlyings (SPY: tradingClass == sym, one entry per
// date) and multi-class (SPX: SPX-class third-Friday + SPXW-class
// weeklies, possibly multiple entries per date).
type pickedExpiration struct {
	date         string // YYYY-MM-DD
	expiryYMD    string // YYYYMMDD
	tradingClass string
	strikes      []float64
	capTruncated bool
}

// expiryStrikesClassedFetcher is the one connector capability
// buildPickedExpirations needs; an interface so the grid-fallback path
// is testable without a live gateway.
type expiryStrikesClassedFetcher interface {
	FetchOptionExpiryStrikesClassed(symbol string, timeout time.Duration) (map[string][]ibkrlib.ExpiryClassedStrikes, error)
}

// gammaExpiriesFetchTimeout bounds the reqSecDefOptParams round trip.
// The SPX classed response (~36 dates × SPX+SPXW × thousands of
// strikes) streams in over tens of seconds under a sluggish sec-def
// farm — measured 2026-06-10 pre-market: 30.3 s end to end via `ibkr
// chain SPX`, which runs the same call under MethodChainExpiries' 50 s
// unary budget and succeeded while the gamma path's previous 30 s
// timeout expired ~0.3 s short and reported "SPX skipped" for the
// session. 45 s keeps headroom inside the compute while staying under
// the chain handler's budget; a genuinely dead farm now falls back to
// the persisted expiry grid instead of burning the wait every retry.
const gammaExpiriesFetchTimeout = 45 * time.Second

// buildPickedExpirations enumerates the expirations + strikes to compute
// over. For sym=="SPX" it walks the classed enumeration (SPX-AM monthlies
// + SPXW-PM weeklies) under per-class settlement cutoffs. For everything
// else it falls back to the existing single-class path (FetchOptionExpiry
// Strikes + selectExpirations with empty class).
//
// Surfaces the per-class budget (params.ExpiryCount) GLOBALLY across
// classes — for SPX, the 6-expiry cap covers SPX + SPXW combined, so a
// quarterly third-Friday + 5 nearest SPXW weeklies is the typical
// picked set.
//
// The classed grid comes from a live reqSecDefOptParams when possible;
// when that fails AND grids holds a recent prior fetch, the cached grid
// is used instead and the non-nil expiryGridFallbackInfo reports its
// age (selection below re-applies all date/settlement filters against
// spotAt, so a stale grid cannot resurrect a settled expiry). A
// successful live fetch is recorded into grids for future outages.
func buildPickedExpirations(c expiryStrikesClassedFetcher, sym string, spotAt time.Time, expiryCount int, grids *expiryGridStore, log gammaLogf) ([]pickedExpiration, *expiryGridFallbackInfo, error) {
	classed, err := c.FetchOptionExpiryStrikesClassed(sym, gammaExpiriesFetchTimeout)
	var fallbackInfo *expiryGridFallbackInfo
	if err != nil {
		cached, asOf, ok := grids.fallback(sym, spotAt)
		if !ok {
			return nil, nil, err
		}
		classed = cached
		fallbackInfo = &expiryGridFallbackInfo{asOf: asOf, liveErr: err}
	} else if storeErr := grids.noteFetched(sym, classed, spotAt); storeErr != nil {
		// Best-effort store; a rejected partial grid or write failure
		// must not affect the live compute using the live data.
		log.Warnf("gamma.expiries: keep cached grid: %v", storeErr)
	}
	if len(classed) == 0 {
		return nil, nil, fmt.Errorf("gateway returned no %s expirations", sym)
	}

	if sym == "SPX" {
		candidates := classedSPXCandidateSpecs(classed, spotAt)
		specs := pickSPXExpirationSlots(candidates, spotAt.In(newYorkLocation()), expiryCount)
		expiryCapTruncated := expiryCount > 0 && len(candidates) > len(specs)
		out := make([]pickedExpiration, 0, len(specs))
		for _, s := range specs {
			out = append(out, pickedExpiration{
				date:         s.Date,
				expiryYMD:    compactExpiry(s.Date),
				tradingClass: s.TradingClass,
				strikes:      s.Strikes,
				capTruncated: expiryCapTruncated,
			})
		}
		return out, fallbackInfo, nil
	}

	// SPY/equity path. Use classed secDef data here too: IBKR can list
	// sibling classes on a date, and merging them creates false jobs that
	// later fall into per-leg contract-detail resolution. That waterfall is
	// exactly what trips the gateway pacing guard. The default selector
	// mirrors `canary chain`: prefer the symbol class when present, otherwise
	// use the only/first listed class for that date.
	out := pickDefaultClassedExpirations(sym, classed, spotAt, expiryCount)
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("gateway returned no usable %s expirations", sym)
	}
	return out, fallbackInfo, nil
}

func pickDefaultClassedExpirations(sym string, classed map[string][]ibkrlib.ExpiryClassedStrikes, spotAt time.Time, expiryCount int) []pickedExpiration {
	selected := make(map[string]ibkrlib.ExpiryClassedStrikes, len(classed))
	selectedStrikes := make(map[string][]float64, len(classed))
	for date, entries := range classed {
		normalised := normalisedSPXChainEntries(entries, sym)
		if len(normalised) == 0 {
			continue
		}
		entry, _, err := selectDefaultChainEntry(sym, normalised, sym, false, date)
		if err != nil {
			continue
		}
		selected[date] = entry
		selectedStrikes[date] = entry.Strikes
	}
	if len(selectedStrikes) == 0 {
		return nil
	}
	candidates := selectExpirationCandidates(selectedStrikes, "", spotAt)
	pickedDates := pickExpirationSlots(candidates, spotAt.In(newYorkLocation()), expiryCount)
	expiryCapTruncated := expiryCount > 0 && len(candidates) > len(pickedDates)
	out := make([]pickedExpiration, 0, len(pickedDates))
	for _, d := range pickedDates {
		entry, ok := selected[d]
		if !ok {
			continue
		}
		out = append(out, pickedExpiration{
			date:         d,
			expiryYMD:    compactExpiry(d),
			tradingClass: entry.TradingClass,
			strikes:      entry.Strikes,
			capTruncated: expiryCapTruncated,
		})
	}
	return out
}

// pickedDatesFromPicked extracts the deduped, sorted set of dates from a
// picked-expiration slice. SPX runs with both class enumerations on the
// same third-Friday yield one entry per (date, class); the result
// envelope's Expirations field carries the unique dates only (matches
// today's []string shape for back-compat).
func pickedDatesFromPicked(picked []pickedExpiration) []string {
	seen := make(map[string]struct{}, len(picked))
	out := make([]string, 0, len(picked))
	for _, p := range picked {
		if _, ok := seen[p.date]; ok {
			continue
		}
		seen[p.date] = struct{}{}
		out = append(out, p.date)
	}
	sort.Strings(out)
	return out
}

// spxExpirySpec is a single (date, tradingClass) pair the SPX classed
// enumeration emits. Same date listed under both SPX and SPXW
// (third-Friday quarterlies) yields two spxExpirySpec entries with
// disjoint strike grids and disjoint settlement instants.
//
// Strikes carries the per-class grid as returned by
// FetchOptionExpiryStrikesClassed — does NOT merge across classes
// because the IV surface, ConID, and settlement convention differ.
type spxExpirySpec struct {
	Date         string // YYYY-MM-DD
	TradingClass string // "SPX" | "SPXW"
	Strikes      []float64
}

// selectSPXExpirationsClassed picks up to N (date, tradingClass) pairs
// that are NOT past their class-specific settlement window. The budget is
// global across classes — each (date, class) counts as a distinct listing
// because SPX-class AM-settled contracts and SPXW-class PM-settled
// contracts have different ConIDs and settlement instants.
//
// SPX needs a class-aware slot policy instead of "nearest N": daily SPXW
// listings can otherwise consume the whole basket and miss the SPX AM
// monthly/quarterly contracts that dominate term dealer exposure. IBKR/TWS
// exposes those SPX AM contracts by the Thursday last-trade date before the
// third Friday; the anchor below targets the actual chain dates rather than
// projecting a third-Friday calendar onto IBKR's wire shape.
//
// Slots: nearest/0DTE, next nearest, this week's Friday/EOW, next SPX AM
// monthly, next SPX AM quarterly, then nearest unused fill. The returned
// slice is sorted date/class ascending for deterministic downstream fanout.
//
// classed is the per-date per-class strike grid emitted by
// FetchOptionExpiryStrikesClassed. now is the wall-clock reference
// for the settlement cutoff. count caps the returned slice.
func selectSPXExpirationsClassed(classed map[string][]ibkrlib.ExpiryClassedStrikes, now time.Time, count int) []spxExpirySpec {
	candidates := classedSPXCandidateSpecs(classed, now)
	return pickSPXExpirationSlots(candidates, now.In(newYorkLocation()), count)
}

func newYorkLocation() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.UTC
	}
	return loc
}

func classedSPXCandidateSpecs(classed map[string][]ibkrlib.ExpiryClassedStrikes, now time.Time) []spxExpirySpec {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	nyNow := now.In(loc)
	today := nyNow.Format("2006-01-02")

	var candidates []spxExpirySpec
	for date, entries := range classed {
		for _, entry := range entries {
			// Class-specific settlement cutoff. SPX-class third-Friday
			// settles at 09:30 ET, but IBKR keys the standard AM monthlies
			// by their Thursday last-trade date. classSettlementInstant
			// normalises that Thursday key to Friday 09:30. SPXW and any
			// other class settle at 16:00 ET. The 15-minute buffer mirrors
			// selectExpirations' PM convention for symmetry.
			day, parseErr := time.ParseInLocation("2006-01-02", date, loc)
			if parseErr != nil {
				continue
			}
			cutoff := spxGammaDataCutoff(entry.TradingClass, day, loc)
			if spxGammaDataWindowClosed(entry.TradingClass, nyNow, cutoff) {
				continue // post usable data window for this specific class
			}
			if date < today && !strings.EqualFold(entry.TradingClass, "SPX") {
				continue
			}
			candidates = append(candidates, spxExpirySpec{
				Date:         date,
				TradingClass: entry.TradingClass,
				Strikes:      entry.Strikes,
			})
		}
	}

	// Sort: date ascending, then trading-class ascending (SPX before
	// SPXW) for ties. Stable so re-ordering doesn't churn the leg list
	// across runs.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Date != candidates[j].Date {
			return candidates[i].Date < candidates[j].Date
		}
		return candidates[i].TradingClass < candidates[j].TradingClass
	})

	return candidates
}

func spxGammaDataCutoff(tradingClass string, day time.Time, loc *time.Location) time.Time {
	settle := classSettlementInstant(tradingClass, day.Year(), day.Month(), day.Day(), loc)
	if strings.EqualFold(strings.TrimSpace(tradingClass), "SPXW") {
		// Expiring SPXW Weeklys/EOM stop trading at 16:00 ET. Non-expiring
		// SPXW series can trade later, but the same-day 0DTE bucket is no
		// longer collectable after the expiring series closes.
		return settle
	}
	return settle.Add(classSettlementBuffer)
}

func spxGammaDataWindowClosed(tradingClass string, nyNow, cutoff time.Time) bool {
	if strings.EqualFold(strings.TrimSpace(tradingClass), "SPXW") {
		return !nyNow.Before(cutoff)
	}
	return nyNow.After(cutoff)
}

func pickSPXExpirationSlots(candidates []spxExpirySpec, nyNow time.Time, count int) []spxExpirySpec {
	if count <= 0 || len(candidates) == 0 {
		return nil
	}
	used := make(map[string]struct{}, count)
	picks := make([]spxExpirySpec, 0, count)
	attempt := func(predicate func(spxExpirySpec) bool) bool {
		if len(picks) >= count {
			return false
		}
		for _, spec := range candidates {
			key := spxExpirySpecKey(spec)
			if _, ok := used[key]; ok {
				continue
			}
			if !predicate(spec) {
				continue
			}
			used[key] = struct{}{}
			picks = append(picks, spec)
			return true
		}
		return false
	}

	always := func(spxExpirySpec) bool { return true }
	attempt(always)
	attempt(always)
	thisFri := thisWeekFriday(nyNow)
	attempt(func(spec spxExpirySpec) bool {
		return spec.Date == thisFri && !strings.EqualFold(spec.TradingClass, "SPX")
	})
	attempt(func(spec spxExpirySpec) bool {
		return strings.EqualFold(spec.TradingClass, "SPX") && isSPXAMMonthlyLastTradeDate(spec.Date)
	})
	attempt(func(spec spxExpirySpec) bool {
		return strings.EqualFold(spec.TradingClass, "SPX") && isSPXAMQuarterlyLastTradeDate(spec.Date)
	})
	for _, spec := range candidates {
		if len(picks) >= count {
			break
		}
		key := spxExpirySpecKey(spec)
		if _, ok := used[key]; ok {
			continue
		}
		used[key] = struct{}{}
		picks = append(picks, spec)
	}
	sort.SliceStable(picks, func(i, j int) bool {
		if picks[i].Date != picks[j].Date {
			return picks[i].Date < picks[j].Date
		}
		return picks[i].TradingClass < picks[j].TradingClass
	})
	return picks
}

func spxExpirySpecKey(spec spxExpirySpec) string {
	return spec.Date + "|" + spec.TradingClass
}
