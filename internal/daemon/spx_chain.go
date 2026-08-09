package daemon

import (
	"fmt"
	"sort"
	"strings"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// pickedExpiration carries one (date, trading-class, strikes) tuple
type pickedExpiration struct {
	date         string // YYYY-MM-DD
	expiryYMD    string // YYYYMMDD
	tradingClass string
	strikes      []float64
	capTruncated bool
}

// expiryStrikesClassedFetcher is the one connector capability
type expiryStrikesClassedFetcher interface {
	FetchOptionExpiryStrikesClassed(symbol string, timeout time.Duration) (map[string][]ibkrlib.ExpiryClassedStrikes, error)
}

// gammaExpiriesFetchTimeout bounds the reqSecDefOptParams round trip.
// farm — measured 2026-06-10 pre-market: 30.3 s end to end via `ibkr
const gammaExpiriesFetchTimeout = 45 * time.Second

// buildPickedExpirations enumerates the expirations + strikes to compute
// when that fails AND grids holds a recent prior fetch, the cached grid
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
	// exactly what trips the gateway pacing guard. The default selector
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
// envelope's Expirations field carries the unique dates only (matches
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
type spxExpirySpec struct {
	Date         string // YYYY-MM-DD
	TradingClass string // "SPX" | "SPXW"
	Strikes      []float64
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
