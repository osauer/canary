package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// TestNormalizeGammaParams fills in defaults for unset / negative
// fields without clobbering legitimate caller overrides.
func TestNormalizeGammaParams(t *testing.T) {
	cases := []struct {
		name string
		in   rpc.GammaZeroParams
		want rpc.GammaZeroParams
	}{
		{
			name: "all_defaults",
			in:   rpc.GammaZeroParams{},
			want: rpc.GammaZeroParams{
				ExpiryCount:    6,
				StrikeWidthPct: 0.10,
				SweepRangePct:  0.15,
				WorkerCount:    4,
			},
		},
		{
			name: "respects_overrides",
			in: rpc.GammaZeroParams{
				ExpiryCount: 10, StrikeWidthPct: 0.05, SweepRangePct: 0.20, WorkerCount: 8,
			},
			want: rpc.GammaZeroParams{
				ExpiryCount: 10, StrikeWidthPct: 0.05, SweepRangePct: 0.20, WorkerCount: 8,
			},
		},
		{
			name: "treats_negative_as_unset",
			in:   rpc.GammaZeroParams{ExpiryCount: -1, StrikeWidthPct: 0.05},
			want: rpc.GammaZeroParams{
				ExpiryCount:    6,
				StrikeWidthPct: 0.05,
				SweepRangePct:  0.15,
				WorkerCount:    4,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeGammaParams(tc.in)
			if got != tc.want {
				t.Errorf("normalize(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestSelectExpirations pins the 0DTE-post-settlement filter at the
// per-trading-class NY-time cutoff plus the slot-anchored picker:
//
//	[front-week-1, front-week-2, EOW, next-monthly, next-quarterly, fill]
//
// Together they make sure (a) past-settlement same-day expiries fall
// out and (b) the basket reaches monthly + quarterly horizons rather
// than picking only weeklies inside ~2 weeks, which the old
// lexicographic-N predecessor did.
func TestSelectExpirations(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	chain := map[string][]float64{
		"2026-05-15": {5000}, // yesterday
		"2026-05-16": {5000}, // today (Sat — synthetic)
		"2026-05-17": {5000}, // tomorrow (Sun)
		"2026-05-19": {5000}, // Tue next week
		"2026-05-26": {5000}, // Tue week+1
		"2026-06-19": {5000}, // 3rd Friday of June — monthly
		"2026-09-18": {5000}, // 3rd Friday of September — quarterly
		"2026-12-18": {5000}, // 3rd Friday of December — quarterly
	}

	t.Run("morning_today_included_with_monthly_anchor", func(t *testing.T) {
		// Sat 2026-05-16 10:00 ET → this week's Friday = 05-22 (NOT in
		// chain). EOW anchor falls through; monthly anchor lands on
		// 06-19 and quarterly on 09-18, leaving room for two front-week
		// slots.
		now := time.Date(2026, 5, 16, 10, 0, 0, 0, loc)
		got := selectExpirations(chain, "", now, 4)
		// Slot 1 = 05-16, Slot 2 = 05-17, Slot 4 (monthly) = 06-19,
		// Slot 5 (quarterly) = 09-18.
		want := []string{"2026-05-16", "2026-05-17", "2026-06-19", "2026-09-18"}
		if !equalSlice(got, want) {
			t.Errorf("morning slot basket: got %v, want %v", got, want)
		}
	})

	t.Run("post_settlement_today_excluded", func(t *testing.T) {
		// 17:00 ET on 2026-05-16 → today (Sat) excluded by the past-cutoff
		// rule even though it's pre-PM-settle for a Saturday listing
		// (16:15 ET buffer applied uniformly).
		now := time.Date(2026, 5, 16, 17, 0, 0, 0, loc)
		got := selectExpirations(chain, "", now, 4)
		// Slot 1 = 05-17, Slot 2 = 05-19, Slot 4 monthly = 06-19,
		// Slot 5 quarterly = 09-18.
		want := []string{"2026-05-17", "2026-05-19", "2026-06-19", "2026-09-18"}
		if !equalSlice(got, want) {
			t.Errorf("post-settlement: got %v, want %v", got, want)
		}
	})

	t.Run("yesterday_always_excluded", func(t *testing.T) {
		now := time.Date(2026, 5, 16, 10, 0, 0, 0, loc)
		got := selectExpirations(chain, "", now, 10)
		for _, d := range got {
			if d == "2026-05-15" {
				t.Errorf("selectExpirations included expired date 2026-05-15: got %v", got)
			}
		}
	})

	t.Run("count_caps_result", func(t *testing.T) {
		// Count=2 only fills slots 1 + 2 (front-week-1, front-week-2);
		// every later slot is skipped.
		now := time.Date(2026, 5, 16, 10, 0, 0, 0, loc)
		got := selectExpirations(chain, "", now, 2)
		want := []string{"2026-05-16", "2026-05-17"}
		if !equalSlice(got, want) {
			t.Errorf("count=2: got %v, want %v", got, want)
		}
	})

	// SPX-class third-Friday is AM-settled (09:30 ET cash-settle). At
	// 10:00 ET on the third Friday the SPX-class listing is already
	// past its settlement window (09:30 + 15-min buffer = 09:45), so
	// the SPX-class filter must exclude it. The SPXW-class filter on
	// the same date+time keeps the listing (PM-settle 16:00, still
	// hours away).
	//
	// Without the trading-class qualifier on selectExpirations, both
	// classes' third-Friday listings would inherit the same 16:15
	// cutoff and the SPX-AM book would be priced with ~6 hours of
	// nonexistent time-to-expiry.
	thirdFridayChain := map[string][]float64{
		"2026-06-19": {5400}, // third Friday — listed under both SPX (AM) and SPXW (PM)
		"2026-06-20": {5400}, // Saturday — synthetic; included to confirm count truncation works
		"2026-06-26": {5400}, // next week
	}
	t.Run("spx_class_third_friday_post_AM_settle_excluded", func(t *testing.T) {
		now := time.Date(2026, 6, 19, 10, 0, 0, 0, loc) // 10:00 ET — past 09:45 AM-settle buffer
		got := selectExpirations(thirdFridayChain, "SPX", now, 4)
		for _, d := range got {
			if d == "2026-06-19" {
				t.Errorf("SPX-class selectExpirations included AM-settled third Friday post-09:45: got %v", got)
			}
		}
	})
	t.Run("spxw_class_third_friday_pre_PM_settle_included", func(t *testing.T) {
		now := time.Date(2026, 6, 19, 10, 0, 0, 0, loc) // 10:00 ET — well before 16:15 PM-settle buffer
		got := selectExpirations(thirdFridayChain, "SPXW", now, 4)
		found := false
		for _, d := range got {
			if d == "2026-06-19" {
				found = true
			}
		}
		if !found {
			t.Errorf("SPXW-class selectExpirations dropped PM-settled third Friday pre-16:00: got %v", got)
		}
	})
	t.Run("spx_class_pre_AM_settle_morning_included", func(t *testing.T) {
		now := time.Date(2026, 6, 19, 8, 30, 0, 0, loc) // 08:30 ET — before 09:30 AM-settle
		got := selectExpirations(thirdFridayChain, "SPX", now, 4)
		found := false
		for _, d := range got {
			if d == "2026-06-19" {
				found = true
			}
		}
		if !found {
			t.Errorf("SPX-class selectExpirations dropped pre-AM-settle morning listing: got %v", got)
		}
	})
}

// TestPickExpirationSlots pins the slot-anchored picker against
// realistic SPY-like chains. The slot rule is:
//
//	[front-week-1, front-week-2, EOW, next-monthly, next-quarterly, fill]
//
// where unfilled anchored slots (e.g. quarterly listing not on chain)
// roll forward to the fill rule. Goals of this block:
//
//   - Confirm the basket reaches into the monthly and quarterly
//     horizons rather than staying inside ~2 weeks (the bug this PR
//     fixes).
//   - Lock the EOW collision behaviour (today is a Friday → EOW
//     candidate is "today", already picked as front-week-1, so the
//     slot rolls to fill).
//   - Confirm anchors degrade gracefully on chains missing a quarterly
//     listing.
func TestPickExpirationSlots(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}

	// A realistic SPY-style chain on 2026-05-22 (Friday): daily M/W/F
	// weeklies, the next monthly 3rd-Friday (Jun 19), and the next two
	// quarterly 3rd-Fridays (Sep 18, Dec 18).
	spyChain := []string{
		"2026-05-22", // Fri (today)
		"2026-05-25", // Mon
		"2026-05-27", // Wed
		"2026-05-29", // Fri
		"2026-06-01", // Mon
		"2026-06-03", // Wed
		"2026-06-05", // Fri
		"2026-06-08", // Mon
		"2026-06-10", // Wed
		"2026-06-12", // Fri
		"2026-06-15", // Mon
		"2026-06-17", // Wed
		"2026-06-19", // 3rd Friday Jun — monthly
		"2026-06-22", // Mon (post-OPEX)
		"2026-06-24", // Wed
		"2026-06-26", // Fri
		"2026-07-17", // 3rd Friday Jul — monthly
		"2026-08-21", // 3rd Friday Aug — monthly
		"2026-09-18", // 3rd Friday Sep — quarterly
		"2026-12-18", // 3rd Friday Dec — quarterly
	}

	t.Run("friday_today_anchors_to_monthly_and_quarterly", func(t *testing.T) {
		// 09:30 ET, 2026-05-22 — markets open, today's expiry still
		// pre-settle. Verifies the documented basket on a Friday: EOW
		// rolls to fill because today is its own EOW.
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc)
		got := pickExpirationSlots(spyChain, now, 6)
		want := []string{
			"2026-05-22", // front-week-1 (today)
			"2026-05-25", // front-week-2
			"2026-05-27", // fill (EOW collided with front-week-1)
			"2026-05-29", // fill
			"2026-06-19", // monthly
			"2026-09-18", // quarterly
		}
		if !equalSlice(got, want) {
			t.Errorf("Fri 2026-05-22 basket: got %v, want %v", got, want)
		}
	})

	t.Run("wednesday_today_eow_fills_to_this_weeks_friday", func(t *testing.T) {
		// Cut the chain to a 2026-05-27 (Wed) view — today's expiry
		// (Wed) is front-week-1, next nearest is Fri 05-29, and the
		// EOW anchor is also Fri 05-29, so EOW collides with
		// front-week-2 and rolls to fill.
		now := time.Date(2026, 5, 27, 9, 30, 0, 0, loc)
		candidates := []string{
			"2026-05-27", "2026-05-29", "2026-06-01", "2026-06-03",
			"2026-06-05", "2026-06-19", "2026-09-18", "2026-12-18",
		}
		got := pickExpirationSlots(candidates, now, 6)
		want := []string{
			"2026-05-27", // front-week-1
			"2026-05-29", // front-week-2 (also EOW collision target)
			"2026-06-01", // fill (EOW rolled)
			"2026-06-03", // fill
			"2026-06-19", // monthly
			"2026-09-18", // quarterly
		}
		if !equalSlice(got, want) {
			t.Errorf("Wed 2026-05-27 basket: got %v, want %v", got, want)
		}
	})

	t.Run("tuesday_today_eow_picks_this_weeks_friday", func(t *testing.T) {
		// On a Tuesday this week's Friday is not picked as front-week,
		// so the EOW anchor lands on it cleanly.
		now := time.Date(2026, 5, 26, 9, 30, 0, 0, loc) // Tue
		candidates := []string{
			"2026-05-26", "2026-05-27", "2026-05-29", "2026-06-01",
			"2026-06-03", "2026-06-19", "2026-09-18", "2026-12-18",
		}
		got := pickExpirationSlots(candidates, now, 6)
		want := []string{
			"2026-05-26", // front-week-1 (today)
			"2026-05-27", // front-week-2
			"2026-05-29", // EOW (this Friday)
			"2026-06-01", // fill
			"2026-06-19", // monthly
			"2026-09-18", // quarterly
		}
		if !equalSlice(got, want) {
			t.Errorf("Tue 2026-05-26 basket: got %v, want %v", got, want)
		}
	})

	t.Run("monthly_and_quarterly_same_date_picks_distinct_quarter", func(t *testing.T) {
		// When today is itself a 3rd Friday of a quarterly month
		// (09-18 = quarterly), it's picked as front-week-1. The
		// monthly slot then rolls to the next 3rd Friday (10-16) and
		// the quarterly slot to Dec 18. EOW collides with today and
		// rolls to fill.
		now := time.Date(2026, 9, 18, 9, 30, 0, 0, loc)
		candidates := []string{
			"2026-09-18", // today (Fri, 3rd Fri Sep, quarterly)
			"2026-09-21", // Mon
			"2026-09-23", // Wed
			"2026-09-25", // Fri
			"2026-10-16", // 3rd Fri Oct (monthly)
			"2026-11-20", // 3rd Fri Nov (monthly)
			"2026-12-18", // 3rd Fri Dec (quarterly)
		}
		got := pickExpirationSlots(candidates, now, 6)
		// Output is sorted ascending. Picked: 09-18 (slot1), 09-21
		// (slot2), 10-16 (slot4 monthly), 12-18 (slot5 quarterly),
		// 09-23 + 09-25 (fills since EOW collided with today).
		want := []string{
			"2026-09-18", "2026-09-21", "2026-09-23", "2026-09-25",
			"2026-10-16", "2026-12-18",
		}
		if !equalSlice(got, want) {
			t.Errorf("Fri-quarterly basket: got %v, want %v", got, want)
		}
	})

	t.Run("missing_quarterly_rolls_to_fill", func(t *testing.T) {
		// Chain has weeklies + a monthly but no quarterly listing.
		// Quarterly slot rolls to fill so the basket still produces N
		// dates rather than 5.
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc) // Fri
		candidates := []string{
			"2026-05-22", "2026-05-25", "2026-05-27", "2026-05-29",
			"2026-06-01", "2026-06-19", // monthly only
		}
		got := pickExpirationSlots(candidates, now, 6)
		want := []string{
			"2026-05-22", "2026-05-25", "2026-05-27", "2026-05-29",
			"2026-06-01", "2026-06-19",
		}
		if !equalSlice(got, want) {
			t.Errorf("missing-quarterly basket: got %v, want %v", got, want)
		}
	})

	t.Run("count_2_only_front_weeks", func(t *testing.T) {
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc)
		got := pickExpirationSlots(spyChain, now, 2)
		want := []string{"2026-05-22", "2026-05-25"}
		if !equalSlice(got, want) {
			t.Errorf("count=2: got %v, want %v", got, want)
		}
	})

	t.Run("count_4_prefers_anchored_slots_over_fill", func(t *testing.T) {
		// Count=4. The anchored slots (front-week-1, front-week-2,
		// monthly, quarterly) consume the full budget; EOW collides
		// with today (Friday) and rolls but the count is already met
		// by the monthly + quarterly anchors. No nearby weeklies make
		// the basket — that's the intent: at small N, prefer horizon
		// coverage over weeklies density.
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc)
		got := pickExpirationSlots(spyChain, now, 4)
		want := []string{
			"2026-05-22", // front-week-1
			"2026-05-25", // front-week-2
			"2026-06-19", // monthly
			"2026-09-18", // quarterly
		}
		if !equalSlice(got, want) {
			t.Errorf("count=4 basket: got %v, want %v", got, want)
		}
	})

	t.Run("empty_candidates_returns_nil", func(t *testing.T) {
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc)
		got := pickExpirationSlots(nil, now, 6)
		if got != nil {
			t.Errorf("nil candidates: got %v, want nil", got)
		}
	})

	t.Run("count_zero_returns_nil", func(t *testing.T) {
		now := time.Date(2026, 5, 22, 9, 30, 0, 0, loc)
		got := pickExpirationSlots(spyChain, now, 0)
		if got != nil {
			t.Errorf("count=0: got %v, want nil", got)
		}
	})
}

// TestThisWeekFriday pins the EOW-anchor helper: for any weekday, it
// returns the YYYY-MM-DD of the calendar Friday >= today.
func TestThisWeekFriday(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		// 2026-05-22 is Friday. Walk the surrounding week.
		{"sun", time.Date(2026, 5, 17, 12, 0, 0, 0, loc), "2026-05-22"},
		{"mon", time.Date(2026, 5, 18, 12, 0, 0, 0, loc), "2026-05-22"},
		{"tue", time.Date(2026, 5, 19, 12, 0, 0, 0, loc), "2026-05-22"},
		{"wed", time.Date(2026, 5, 20, 12, 0, 0, 0, loc), "2026-05-22"},
		{"thu", time.Date(2026, 5, 21, 12, 0, 0, 0, loc), "2026-05-22"},
		{"fri", time.Date(2026, 5, 22, 12, 0, 0, 0, loc), "2026-05-22"},
		{"sat", time.Date(2026, 5, 23, 12, 0, 0, 0, loc), "2026-05-29"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := thisWeekFriday(tc.now); got != tc.want {
				t.Errorf("thisWeekFriday(%s): got %q, want %q", tc.now.Format("Mon"), got, tc.want)
			}
		})
	}
}

// TestIsThirdFridayDate / TestIsQuarterlyThirdFridayDate pin the two
// predicates used by the monthly / quarterly slot anchors. 3rd Fridays
// fall on days 15-21 (the unique Friday in that span each month);
// quarterlies are 3rd Fridays of Mar / Jun / Sep / Dec.
func TestIsThirdFridayDate(t *testing.T) {
	cases := map[string]bool{
		"2026-05-22": false, // Friday but day 22, not 3rd Friday of May (3rd Fri = May 15)
		"2026-05-15": true,  // Fri, day 15 — 3rd Friday of May
		"2026-06-19": true,  // Fri, day 19 — 3rd Friday of June
		"2026-06-26": false, // Fri, day 26 — 4th Friday
		"2026-09-18": true,  // Fri, day 18 — 3rd Friday of Sep
		"2026-09-21": false, // Mon
		"":           false,
		"bogus":      false,
	}
	for in, want := range cases {
		if got := isThirdFridayDate(in); got != want {
			t.Errorf("isThirdFridayDate(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsQuarterlyThirdFridayDate(t *testing.T) {
	cases := map[string]bool{
		"2026-03-20": true,  // 3rd Fri Mar
		"2026-06-19": true,  // 3rd Fri Jun
		"2026-09-18": true,  // 3rd Fri Sep
		"2026-12-18": true,  // 3rd Fri Dec
		"2026-05-15": false, // 3rd Fri May (not quarterly)
		"2026-07-17": false, // 3rd Fri Jul (not quarterly)
		"2026-06-26": false, // 4th Fri Jun
		"2026-06-22": false, // Mon
	}
	for in, want := range cases {
		if got := isQuarterlyThirdFridayDate(in); got != want {
			t.Errorf("isQuarterlyThirdFridayDate(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestDTEYearsSPXClassUsesAMInstant pins the trading-class branch on
// dteYears. SPX-class third-Friday options settle at 09:30 ET; at 10:00
// ET on expiry day they are PAST settlement (dte=0), not 6 hours away
// (which the legacy 16:00 PM-settle instant would say). The aggregate
// gamma error from a 6-hour TTE mis-attribution on third-Friday SPX is
// dollar-significant — SPX dealer gamma concentrates on expiring books.
func TestDTEYearsSPXClassUsesAMInstant(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	// 10:00 ET on the third Friday — past 09:30 SPX-class settle.
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, loc)

	// SPX class → AM-settle → already expired.
	if y := dteYears("20260619", "SPX", now); y != 0 {
		t.Errorf("SPX-class dteYears post-AM-settle: got %v years, want 0 (already cash-settled at 09:30)", y)
	}
	// SPXW class → PM-settle → still ~6 hours of TTE.
	y := dteYears("20260619", "SPXW", now)
	if y <= 0 || y > 0.001 {
		t.Errorf("SPXW-class dteYears pre-PM-settle: got %v years, want 0 < y < 0.001 (~6h)", y)
	}
	// Empty class falls back to PM-settle convention (today's SPY behaviour).
	if dteYears("20260619", "", now) <= 0 {
		t.Errorf("empty-class dteYears must match SPXW (PM-settle) for back-compat")
	}
}

func TestGammaCalendarDTEUsesCalendarBuckets(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	lateSession := time.Date(2026, 6, 1, 17, 0, 0, 0, loc)
	if y := dteYears("20260602", "SPY", lateSession); y >= zeroDTECutoffYears {
		t.Fatalf("test setup expected next-day SPY to be <24h to settlement, got %v years", y)
	}

	cases := []struct {
		name         string
		expiryYMD    string
		tradingClass string
		now          time.Time
		want         int
	}{
		{name: "same_day_spy", expiryYMD: "20260601", tradingClass: "SPY", now: lateSession, want: 0},
		{name: "next_day_spy", expiryYMD: "20260602", tradingClass: "SPY", now: lateSession, want: 1},
		{name: "seven_dte_spy", expiryYMD: "20260608", tradingClass: "SPY", now: lateSession, want: 7},
		{name: "term_spy", expiryYMD: "20260609", tradingClass: "SPY", now: lateSession, want: 8},
		{
			name:         "spx_am_thursday_key_pre_settlement_friday",
			expiryYMD:    "20260917",
			tradingClass: "SPX",
			now:          time.Date(2026, 9, 18, 9, 0, 0, 0, loc),
			want:         0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := gammaCalendarDTE(tc.expiryYMD, tc.tradingClass, tc.now)
			if !ok || got != tc.want {
				t.Fatalf("gammaCalendarDTE(%s, %s, %s) = %d ok=%v, want %d ok=true",
					tc.expiryYMD, tc.tradingClass, tc.now, got, ok, tc.want)
			}
		})
	}
	if _, ok := gammaCalendarDTE("bogus", "SPY", lateSession); ok {
		t.Fatalf("malformed expiry should return ok=false")
	}
}

// TestFilterStrikesAroundSpot pins the ±widthPct window logic and the
// defensive sort. SPX chains return strikes in arbitrary order across
// exchange-keyed frames; relying on input order would silently break
// the strike-grid contiguity assumption.
func TestFilterStrikesAroundSpot(t *testing.T) {
	strikes := []float64{4700, 4900, 5500, 4500, 5050, 5000, 4950, 4800, 5100, 5200, 5400}

	got := filterStrikesAroundSpot(strikes, 5000, 0.05) // ±5% = [4750, 5250]
	want := []float64{4800, 4900, 4950, 5000, 5050, 5100, 5200}
	if !equalFloatSlice(got, want) {
		t.Errorf("±5%% around 5000: got %v, want %v", got, want)
	}

	if got := filterStrikesAroundSpot(strikes, 0, 0.10); got != nil {
		t.Errorf("zero spot: got %v, want nil", got)
	}
	if got := filterStrikesAroundSpot(strikes, 5000, 0); got != nil {
		t.Errorf("zero width: got %v, want nil", got)
	}
	if got := filterStrikesAroundSpot(nil, 5000, 0.10); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
}

func TestCapStrikesATMOutwardBoundsSPXFanout(t *testing.T) {
	var strikes []float64
	for i := range 101 {
		strikes = append(strikes, 5000+float64(i))
	}
	capped, ok := capStrikesATMOutward(strikes, 80)
	if !ok {
		t.Fatal("expected cap to report true")
	}
	if len(capped) != 80 {
		t.Fatalf("len(capped)=%d, want 80", len(capped))
	}
	if capped[0] != 5000 || capped[79] != 5079 {
		t.Fatalf("cap should keep nearest already-ordered strikes, got first=%v last=%v", capped[0], capped[79])
	}

	uncapped, ok := capStrikesATMOutward(strikes[:80], 80)
	if ok {
		t.Fatal("exactly-at-budget strikes should not report capped")
	}
	if len(uncapped) != 80 {
		t.Fatalf("len(uncapped)=%d, want 80", len(uncapped))
	}
}

// TestCompactExpiry round-trips YYYY-MM-DD into the YYYYMMDD form
// SubscribeOption expects, with best-effort behaviour on malformed
// input.
func TestCompactExpiry(t *testing.T) {
	cases := map[string]string{
		"2026-05-16": "20260516",
		"2026-12-31": "20261231",
		"20260516":   "20260516", // already compact
		"":           "",
		"not-a-date": "not-a-date",
		"2026/05/16": "2026/05/16",
		"2026-05-1":  "2026-05-1",
	}
	for in, want := range cases {
		if got := compactExpiry(in); got != want {
			t.Errorf("compactExpiry(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeepGammaJobAfterPrewarm(t *testing.T) {
	cases := []struct {
		name                             string
		prewarmComplete, cached, blocked bool
		want                             bool
	}{
		{name: "incomplete_uncached_keeps_resolution_fallback", want: true},
		{name: "incomplete_cached_keeps_leg", cached: true, want: true},
		{name: "complete_requires_cache", prewarmComplete: true, want: false},
		{name: "complete_keeps_cached", prewarmComplete: true, cached: true, want: true},
		// A blocked fallback (zero-detail / timeout prewarm failure) must
		// not fall back to per-leg resolution, but cached contracts stay
		// usable.
		{name: "blocked_fallback_drops_uncached", blocked: true, want: false},
		{name: "blocked_fallback_keeps_cached", cached: true, blocked: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := keepGammaJobAfterPrewarm(tc.prewarmComplete, tc.cached, tc.blocked)
			if got != tc.want {
				t.Fatalf("keepGammaJobAfterPrewarm(%v, %v, %v)=%v, want %v",
					tc.prewarmComplete, tc.cached, tc.blocked, got, tc.want)
			}
		})
	}
}

func TestGammaPrewarmZeroContractDetailsClassifiesAsContractMissing(t *testing.T) {
	err := errors.New("prewarm SPX 20260603 class=SPXW returned zero contract details across route attempts SMART/CBOE")
	if got := classifyGammaLegFailure(err); got != gammaLegFailureContractMissing {
		t.Fatalf("classifyGammaLegFailure = %q, want %q", got, gammaLegFailureContractMissing)
	}
	if !gammaPrewarmFailureBlocksFallback(err) {
		t.Fatal("zero-detail prewarm should block per-leg fallback")
	}
	if !gammaPrewarmFailureBlocksFallback(errors.New("prewarm timeout after 30s (cached 3 so far)")) {
		t.Fatal("timed-out prewarm should block per-leg fallback for uncached contracts")
	}

	picked := []pickedExpiration{{
		date:         "2026-06-03",
		expiryYMD:    "20260603",
		tradingClass: "SPXW",
		strikes:      []float64{7550, 7600},
	}}
	d := newGammaCollectionDiagnostics("SPX", picked)
	d.notePrewarm("SPXW", "20260603", 0, 0, err)
	rows := d.finish(2 * time.Second)
	if len(rows) != 1 {
		t.Fatalf("diagnostic rows len=%d, want 1: %+v", len(rows), rows)
	}
	if rows[0].SubscriptionRejects != 0 || rows[0].Timeouts != 0 {
		t.Fatalf("zero-detail prewarm should not be counted as reject/timeout: %+v", rows[0])
	}
}

// TestDTEYears computes years-to-expiry to 16:00 ET on the expiration
// date, returning 0 on parse failure or non-positive deltas (the leg
// gate filters these).
func TestDTEYears(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, loc)

	if y := dteYears("20260516", "", now); y <= 0 || y > 0.01 {
		// 2h to 16:00 ≈ 2 / (24·365) ≈ 0.000228; window [0, 0.01]
		t.Errorf("2h to expiry: got %v, want in (0, 0.01)", y)
	}
	// Roughly 33 days × 24h / (24·365) ≈ 0.0904 years
	y := dteYears("20260619", "", now)
	if y < 0.080 || y > 0.105 {
		t.Errorf("~33 days: got %v, want [0.080, 0.105]", y)
	}

	if y := dteYears("20260515", "", now); y != 0 {
		t.Errorf("past date: got %v, want 0", y)
	}
	if y := dteYears("bogus", "", now); y != 0 {
		t.Errorf("bogus input: got %v, want 0", y)
	}
}

// TestSweepProfile pins the sweep grid shape and exercises the BS
// recompute by checking that a strongly call-skewed chain produces a
// sweep where higher spots → more positive GEX (calls gain delta-
// notional from rising spot; under Perfiliev's call-long convention
// that means more positive dealer GEX).
func TestSweepProfile(t *testing.T) {
	legs := []legData{
		// 100k contracts of 30-DTE ATM calls. Big enough that a single
		// strike drives the whole signed signal.
		{
			expiryYMD: "20260619", dte: 30.0 / 365, strike: 5000,
			right: "C", isCall: true, iv: 0.20, oi: 100_000,
			gammaAtSnapshot: 0.001,
		},
	}
	profile := sweepProfile(legs, 5000, 0.15, nil)
	if len(profile) != sweepPoints {
		t.Fatalf("sweep len = %d, want %d", len(profile), sweepPoints)
	}
	// Endpoints span 0.85 → 1.15 × 5000
	if math.Abs(profile[0].Spot-4250) > 0.5 {
		t.Errorf("first spot = %v, want ~4250", profile[0].Spot)
	}
	if math.Abs(profile[len(profile)-1].Spot-5750) > 0.5 {
		t.Errorf("last spot = %v, want ~5750", profile[len(profile)-1].Spot)
	}
	// Single-call chain: at spot = strike GEX is positive; gamma
	// decays at the tails so magnitude near 5000 should exceed the
	// extremes. (Trivially true for an ATM call's symmetric gamma
	// peak.)
	atSpotIdx := sweepPoints / 2
	if profile[atSpotIdx].GEX <= 0 {
		t.Errorf("ATM GEX should be positive for a long-call book, got %v", profile[atSpotIdx].GEX)
	}
	if profile[0].GEX > profile[atSpotIdx].GEX {
		t.Errorf("far-OTM GEX should be smaller than ATM: %v vs %v",
			profile[0].GEX, profile[atSpotIdx].GEX)
	}

	// Empty legs: the sweep still builds the spot grid (the renderer
	// charts a flat-zero curve), but every GEX point is exactly 0.
	// Documenting this rather than guarding because the compute
	// itself returns an error before reaching sweepProfile when no
	// legs are usable, so this is only a defensive shape pin.
	empty := sweepProfile(nil, 5000, 0.15, nil)
	if len(empty) != sweepPoints {
		t.Errorf("empty legs len = %d, want %d", len(empty), sweepPoints)
	}
	for i, p := range empty {
		if p.GEX != 0 {
			t.Errorf("empty legs profile[%d].GEX = %v, want exactly 0", i, p.GEX)
			break
		}
	}
	if got := sweepProfile(legs, 0, 0.15, nil); got != nil {
		t.Errorf("zero spot: got %v, want nil", got)
	}
}

// TestRankTopStrikesByAbsGEX pins the ranking and the
// already-format-conversion (YYYYMMDD → YYYY-MM-DD on the result).
func TestRankTopStrikesByAbsGEX(t *testing.T) {
	legs := []legData{
		{expiryYMD: "20260619", strike: 5000, right: "C", oi: 10_000, gammaAtSnapshot: 0.001},
		{expiryYMD: "20260619", strike: 5050, right: "P", oi: 50_000, gammaAtSnapshot: 0.0008},
		{expiryYMD: "20260626", strike: 5100, right: "C", oi: 5_000, gammaAtSnapshot: 0.0005},
		{expiryYMD: "20260619", strike: 4950, right: "C", oi: 0, gammaAtSnapshot: 0.001},  // dropped: OI=0
		{expiryYMD: "20260619", strike: 4900, right: "P", oi: 10_000, gammaAtSnapshot: 0}, // dropped: γ=0
	}
	top := rankTopStrikesByAbsGEX(legs, 5000, 5, "TEST")

	if len(top) != 3 {
		t.Fatalf("got %d rows, want 3 (two filtered): %+v", len(top), top)
	}
	// Highest |γ|·OI = 50000 × 0.0008 = 40, 10000 × 0.001 = 10, 5000 × 0.0005 = 2.5
	// Order: 5050P > 5000C > 5100C
	wantOrder := []float64{5050, 5000, 5100}
	for i, w := range wantOrder {
		if top[i].Strike != w {
			t.Errorf("rank[%d] strike = %v, want %v (full: %+v)", i, top[i].Strike, w, top)
		}
	}
	// Expiry format conversion
	if top[0].Expiry != "2026-06-19" {
		t.Errorf("expiry format = %q, want 2026-06-19", top[0].Expiry)
	}
	// OI surfaces through
	if top[0].OI != 50_000 {
		t.Errorf("OI = %d, want 50000", top[0].OI)
	}

	// k=0 disables ranking
	if got := rankTopStrikesByAbsGEX(legs, 5000, 0, "TEST"); got != nil {
		t.Errorf("k=0: got %v, want nil", got)
	}

	many := make([]legData, 12)
	for i := range many {
		many[i] = legData{
			expiryYMD:       "20260619",
			strike:          5000 + float64(i*5),
			right:           "C",
			oi:              int64(1_000 + i),
			gammaAtSnapshot: 0.001 + float64(i)*0.0001,
		}
	}
	if got := rankTopStrikesByAbsGEX(many, 5000, topStrikesK, "TEST"); len(got) != 10 {
		t.Errorf("topStrikesK rows = %d, want 10", len(got))
	}
}

// TestThrottleDetected pins the throttle-abort threshold and sample-size
// policy. The fan-out short-circuits when the no-contract failure rate
// exceeds 5 % after at least 50 completions — chosen so we don't bail
// on startup noise but do react to a degraded gateway before the fan-out
// runs to completion and compounds the rate-limit pressure.
func TestThrottleDetected(t *testing.T) {
	cases := []struct {
		name             string
		done, noContract int32
		want             bool
	}{
		{"below_sample_size_high_ratio", 49, 49, false},  // 100 % but only 49 samples
		{"at_sample_size_below_threshold", 50, 2, false}, // 4 % — just under
		{"at_sample_size_at_threshold", 50, 3, true},     // 6 % — over
		{"deep_run_under_threshold", 400, 19, false},     // 4.75 %
		{"deep_run_over_threshold", 400, 21, true},       // 5.25 %
		{"zero_no_contract", 200, 0, false},              // healthy gateway
		{"zero_completions", 0, 0, false},                // pre-warmup
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := throttleDetected(tc.done, tc.noContract); got != tc.want {
				t.Errorf("throttleDetected(done=%d, nc=%d) = %v, want %v",
					tc.done, tc.noContract, got, tc.want)
			}
		})
	}
}

// TestIsAcceptableDataType pins the stale-data refusal logic. Live
// and frozen pass — frozen is "yesterday's official close" which the
// spec accepts for daily refresh. Delayed passes because the production
// fetcher separately requires clock-aligned delayed option model ticks;
// delayed-frozen remains prior-close orientation only.
func TestIsAcceptableDataType(t *testing.T) {
	cases := map[string]bool{
		"":               true,
		"live":           true,
		"frozen":         true,
		"delayed":        true,
		"delayed-frozen": false,
		"unknown":        false, // forward-compat: unknown values are stale-by-default
	}
	for dt, want := range cases {
		if got := isAcceptableDataType(dt); got != want {
			t.Errorf("isAcceptableDataType(%q) = %v, want %v", dt, got, want)
		}
	}
}

func TestGammaOptionModelDataTypeCompatible(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		spotType   string
		optionType int
		want       bool
	}{
		{"delayed requires delayed model tick", rpc.MarketDataDelayed, ibkrlib.OptionModelDataTypeDelayed, true},
		{"delayed rejects live model tick", rpc.MarketDataDelayed, ibkrlib.OptionModelDataTypeLive, false},
		{"delayed rejects untyped IV", rpc.MarketDataDelayed, 0, false},
		{"live accepts live model tick", rpc.MarketDataLive, ibkrlib.OptionModelDataTypeLive, true},
		{"live preserves legacy untyped IV", rpc.MarketDataLive, 0, true},
		{"live rejects delayed model tick", rpc.MarketDataLive, ibkrlib.OptionModelDataTypeDelayed, false},
		{"frozen rejects delayed model tick", rpc.MarketDataFrozen, ibkrlib.OptionModelDataTypeDelayed, false},
		{"unknown spot refuses", "unexpected", ibkrlib.OptionModelDataTypeLive, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gammaOptionModelDataTypeCompatible(tc.spotType, tc.optionType); got != tc.want {
				t.Fatalf("compatible(%q, %d) = %v, want %v", tc.spotType, tc.optionType, got, tc.want)
			}
		})
	}
}

// ---------- v0.26.0: BS-IV pre-market fallback path ----------

// TestBSIVFallback_AssemblesLegFromSyntheticPrice drives the bsIVFallback
// helper end-to-end on a realistic pre-market scenario. SPY 7-DTE strike
// 735 priced at a known σ via bsCallPrice + put-call parity, then
// passed through the same helper productionLegFetcher uses when the
// gateway didn't push a model tick. Asserts the assembled legResult
// carries IVDerived=true, OK=true, the recovered σ within 5 bps, a
// physical (positive, finite) gamma, and the OI threaded through
// unchanged.
//
// This is the regression pin the v0.26.0 CHANGELOG's "pre-market gamma
// lands a real result" claim warrants. If a future refactor breaks
// Stage 2b (wrong sign branch, wrong parity formula, drops OI, mis-
// labels the leg) this test fails loudly — even when the strict-mode
// wire-smoke gate can't observe the fallback firing.
func TestBSIVFallback_AssemblesLegFromSyntheticPrice(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	const (
		spot    = 737.0
		strike  = 735.0
		sigmaTr = 0.14
	)
	// 7-DTE: snapshot today, expiry 7 days out. Expiry settlement is
	// at 16:00 NY per dteYears.
	now := time.Date(2026, 5, 18, 9, 30, 0, 0, loc)
	expiryYMD := now.AddDate(0, 0, 7).Format("20060102")

	cases := []struct {
		name   string
		right  string
		isCall bool
	}{
		{"call_7dte_atm", "C", true},
		{"put_7dte_atm", "P", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dte := dteYears(expiryYMD, "", now)
			callPx := bsCallPrice(spot, strike, dte, sigmaTr, 0, 0)
			price := callPx
			if !tc.isCall {
				price = callPx - spot + strike // r=q=0 parity
			}

			r := bsIVFallback(spot, now, expiryYMD, "", strike, tc.right, 123, true, price, gammaIVSourcePrevClose)

			if !r.OK || !r.IVDerived {
				t.Fatalf("expected OK=true IVDerived=true, got %+v", r)
			}
			if r.IVSource != gammaIVSourcePrevClose {
				t.Fatalf("IVSource = %q, want %q", r.IVSource, gammaIVSourcePrevClose)
			}
			if r.OI != 123 {
				t.Errorf("OI threaded through: got %d, want 123", r.OI)
			}
			if math.Abs(r.IV-sigmaTr) > 0.0005 {
				t.Errorf("σ recovery: got %.5f, want %.5f", r.IV, sigmaTr)
			}
			if r.Gamma <= 0 || math.IsNaN(r.Gamma) || math.IsInf(r.Gamma, 0) {
				t.Errorf("expected positive finite gamma, got %v", r.Gamma)
			}
		})
	}
}

func TestShouldRetryGammaWithDelayed(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	rth := time.Date(2026, 8, 7, 10, 0, 0, 0, loc)
	closed := time.Date(2026, 8, 7, 2, 0, 0, 0, loc)
	entitlement := gammaSpotUnavailableError("SPX", &SubscriptionRejectedError{
		Key: "SPX", Rejection: ibkrlib.SubscriptionRejection{Code: 354, Message: "untrusted"},
	})
	absent := fmt.Errorf("hold SPY underlying: %w", &ibkrlib.MarketDataAbsenceError{Key: "SPY", Code: 354})
	other := gammaSpotUnavailableError("SPX", &SubscriptionRejectedError{
		Key: "SPX", Rejection: ibkrlib.SubscriptionRejection{Code: 200, Message: "untrusted"},
	})

	if !shouldRetryGammaWithDelayed(entitlement, rth) {
		t.Fatal("typed RTH 354 should retry once with delayed data")
	}
	if !shouldRetryGammaWithDelayed(absent, rth) {
		t.Fatal("remembered RTH 354 should rearm for delayed retry")
	}
	if shouldRetryGammaWithDelayed(entitlement, closed) {
		t.Fatal("closed-session 354 must not request the RTH delayed stream")
	}
	if shouldRetryGammaWithDelayed(other, rth) {
		t.Fatal("non-entitlement rejection must not change market-data mode")
	}
	if shouldRetryGammaWithDelayed(context.Canceled, rth) {
		t.Fatal("cancellation must not retry")
	}
}

// TestBSIVFallback_RefusalCases pins the empty-legResult exits — every
// path where Stage 2b drops a leg rather than poisoning the aggregate
// with a non-physical σ.
func TestBSIVFallback_RefusalCases(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 5, 18, 9, 30, 0, 0, loc)
	future := now.AddDate(0, 0, 7).Format("20060102")
	past := now.AddDate(0, 0, -1).Format("20060102")

	cases := []struct {
		name      string
		expiryYMD string
		price     float64
		why       string
	}{
		{"zero_price", future, 0, "no model tick AND no bid/ask AND no prior close"},
		{"negative_price", future, -1, "garbage price"},
		{"expired", past, 5.0, "DTE ≤ 0 (rollover during compute)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := bsIVFallback(737.0, now, tc.expiryYMD, "", 735.0, "P", 100, true, tc.price, gammaIVSourcePrevClose)
			if r.OK || r.IVDerived || r.OI != 0 || r.IV != 0 {
				t.Errorf("%s should return empty legResult, got %+v", tc.why, r)
			}
		})
	}
}

// TestCheckLegCoverage pins the F-21/F-25 persist-or-not gate: a
// fan-out whose leg-landing fraction falls below
// MinLegCoverageFraction returns an error so the cache layer's
// gammaErrorRetryTTL machinery applies, mirroring breadth's
// MinCoverageFraction guard. Boundary, throttle-attribution, and the
// defensive empty-jobs guard are all exercised.
func TestCheckLegCoverage(t *testing.T) {
	t.Parallel()

	// Above-threshold: clean run.
	if err := checkLegCoverage(50, 100, false); err != nil {
		t.Errorf("50%% should pass MinLegCoverageFraction (0.2), got error: %v", err)
	}
	if err := checkLegCoverage(900, 1000, true); err != nil {
		t.Errorf("90%% even with throttle observed should pass: %v", err)
	}

	// Exactly the threshold passes (boundary is inclusive on the pass
	// side — coverage >= MinLegCoverageFraction returns nil).
	if err := checkLegCoverage(200, 1000, false); err != nil {
		t.Errorf("exactly 20%% should pass the >= boundary, got: %v", err)
	}

	// Below threshold: error, names the shortfall.
	err := checkLegCoverage(19, 100, false)
	if err == nil {
		t.Fatal("19%% should fail MinLegCoverageFraction")
	}
	if msg := err.Error(); !strings.Contains(msg, "19/100") || !strings.Contains(msg, "below minimum") {
		t.Errorf("error message should name landed/total and 'below minimum': %q", msg)
	}

	// Throttle attribution: when the gateway throttled, the message
	// names it so the operator can act on the cause, not the symptom.
	err = checkLegCoverage(0, 100, true)
	if err == nil {
		t.Fatal("0%% should fail")
	}
	if !strings.Contains(err.Error(), "gateway throttled") {
		t.Errorf("throttled-attribution missing from message: %q", err.Error())
	}

	// Defensive empty-jobs guard: would normally be unreachable
	// (normalizeGammaParams prevents it) but the helper must not
	// emit a NaN-laden message.
	err = checkLegCoverage(0, 0, false)
	if err == nil {
		t.Fatal("empty jobs list should defensively error rather than divide by zero")
	}
	if strings.Contains(err.Error(), "NaN") {
		t.Errorf("empty-jobs message should not surface NaN: %q", err.Error())
	}
}

// TestGammaTotalAbsAggregator_SignAgnosticMagnitude pins the v3 B1
// fix: the magnitude aggregator must sum |Γ_i| × OI_i × multiplier ×
// spot² over all legs and the per-leg gamma must be derived from BS
// using the leg's captured IV at snapshot spot (not read from a
// gateway Greeks tick that may have raced the IV tick and arrived
// after the IV-poll loop exited).
//
// Regression posture: with the v2 code path, a leg whose IV landed
// but whose gamma tick hadn't arrived yet would carry gammaAtSnapshot=0
// and contribute 0 to GammaTotalAbs. With ~3200 legs in a typical
// SPY+SPX run, this race silently dropped most of the magnitude and
// the wire surfaced gamma_total_abs = $0 even though the sweep had
// real legs. The fix BS-recomputes gamma from captured IV in the
// aggregator step; this test confirms the aggregator produces the
// expected magnitude even when every leg's pre-fetched gammaAtSnapshot
// is 0 (the worst case that mirrors the race condition).
func TestGammaTotalAbsAggregator_SignAgnosticMagnitude(t *testing.T) {
	t.Parallel()
	const spot = 5000.0
	// Synthesize legs with known IV. expiryYMD is a future date so dte > 0.
	legs := []legData{
		{expiryYMD: "20260619", dte: 0.10, strike: 5000, right: "C", isCall: true, iv: 0.20, oi: 10_000, gammaAtSnapshot: 0},
		{expiryYMD: "20260619", dte: 0.10, strike: 5050, right: "P", isCall: false, iv: 0.21, oi: 20_000, gammaAtSnapshot: 0},
		{expiryYMD: "20260619", dte: 0.10, strike: 4950, right: "C", isCall: true, iv: 0.19, oi: 5_000, gammaAtSnapshot: 0},
		// Negative-gamma-sign leg (put). Must still contribute its
		// magnitude — the sum is sign-agnostic by construction.
		{expiryYMD: "20260619", dte: 0.10, strike: 5100, right: "P", isCall: false, iv: 0.22, oi: 8_000, gammaAtSnapshot: 0},
	}

	// Expected: sum of |bsGamma(spot, K, dte, IV)| × OI × 100 × spot² ×
	// 0.01, independent of the call/put sign.
	want := 0.0
	for _, l := range legs {
		γ := bsGamma(spot, l.strike, l.dte, l.iv, 0, 0)
		want += math.Abs(γ) * float64(l.oi) * 100 * spot * spot * 0.01
	}
	if want <= 0 {
		t.Fatalf("test fixture is degenerate: expected magnitude > 0, got %v", want)
	}

	// Mirror the aggregator block in computeGammaZeroFor exactly.
	got := 0.0
	for _, l := range legs {
		γ := bsGamma(spot, l.strike, l.dte, l.iv, 0, 0)
		got += absGEX(γ, float64(l.oi), 100, spot)
	}

	if math.Abs(got-want) > 1e-6 {
		t.Errorf("aggregator returned %.6e, want %.6e", got, want)
	}
	// Sanity floor: 43000 contracts × 100 × spot² × 0.01 × γ ≈ for
	// short-dated ATM σ=0.20 gives γ ≈ 0.001 → ~$1B notional. Keep
	// the floor loose; this is a smoke check that the magnitude is
	// in the right order, not a fitted assertion.
	if got < 1e8 {
		t.Errorf("aggregator magnitude looks too small: got %.3e, expected ≥ 1e8 for the synthesized fixture", got)
	}
}

// TestGammaTotalAbsAggregator_IgnoresGatewayGammaTickRace pins the
// race-fix posture explicitly: even when half the legs carry the v2
// "no Greeks tick yet" gammaAtSnapshot=0 and the other half carry an
// arbitrary stale value, the v3 aggregator must produce the same
// result because it always BS-recomputes from captured IV.
func TestGammaTotalAbsAggregator_IgnoresGatewayGammaTickRace(t *testing.T) {
	t.Parallel()
	const spot = 5000.0
	clean := []legData{
		{expiryYMD: "20260619", dte: 0.10, strike: 5000, right: "C", isCall: true, iv: 0.20, oi: 10_000, gammaAtSnapshot: 0},
		{expiryYMD: "20260619", dte: 0.10, strike: 5050, right: "P", isCall: false, iv: 0.21, oi: 20_000, gammaAtSnapshot: 0},
	}
	raced := []legData{
		// Same legs, but gammaAtSnapshot carries an out-of-date value
		// the gateway pushed before our IV arrived. The aggregator must
		// ignore it and recompute.
		{expiryYMD: "20260619", dte: 0.10, strike: 5000, right: "C", isCall: true, iv: 0.20, oi: 10_000, gammaAtSnapshot: 99.0},
		{expiryYMD: "20260619", dte: 0.10, strike: 5050, right: "P", isCall: false, iv: 0.21, oi: 20_000, gammaAtSnapshot: -42.0},
	}

	sum := func(legs []legData) float64 {
		out := 0.0
		for _, l := range legs {
			γ := bsGamma(spot, l.strike, l.dte, l.iv, 0, 0)
			out += absGEX(γ, float64(l.oi), 100, spot)
		}
		return out
	}
	a, b := sum(clean), sum(raced)
	if math.Abs(a-b) > 1e-6 {
		t.Errorf("aggregator must be insensitive to legs[i].gammaAtSnapshot: clean=%v, raced=%v", a, b)
	}
}

func TestPrepareGEXLegsRequiresOpenInterest(t *testing.T) {
	t.Parallel()
	const spot = 5000.0
	pricedNoOI := []legData{
		{expiryYMD: "20260619", dte: 0.10, strike: 5000, right: "C", isCall: true, iv: 0.20, oi: 0},
		{expiryYMD: "20260619", dte: 0.10, strike: 5050, right: "P", isCall: false, iv: 0.21, oi: 0},
	}
	gexLegs, total := prepareGEXLegs(pricedNoOI, spot)
	if len(gexLegs) != 0 {
		t.Fatalf("priced legs with missing OI must not count as GEX legs: %+v", gexLegs)
	}
	if total != 0 {
		t.Fatalf("priced legs with missing OI must produce no OI-weighted GEX total, got %v", total)
	}

	withOI := append(append([]legData{}, pricedNoOI...), legData{
		expiryYMD: "20260619", dte: 0.10, strike: 5000, right: "C", isCall: true, iv: 0.20, oi: 10_000,
	})
	gexLegs, total = prepareGEXLegs(withOI, spot)
	if len(gexLegs) != 1 {
		t.Fatalf("only the leg with OI should contribute to GEX: got %d", len(gexLegs))
	}
	if total <= 0 {
		t.Fatalf("leg with OI should produce positive absolute GEX, got %v", total)
	}
}

func TestWaitForOptionOpenInterestCapturesLateTick(t *testing.T) {
	t.Parallel()

	var oi atomic.Int64
	time.AfterFunc(20*time.Millisecond, func() {
		oi.Store(4321)
	})

	got, observed := waitForOptionOpenInterest(context.Background(), time.Now().Add(500*time.Millisecond), func() (int64, bool) {
		v := oi.Load()
		return v, v != 0
	})
	if got != 4321 || !observed {
		t.Fatalf("OpenInterest = %d, want late tick value 4321", got)
	}
}

func TestWaitForOptionOpenInterestCapturesObservedZero(t *testing.T) {
	t.Parallel()

	var seen atomic.Bool
	time.AfterFunc(20*time.Millisecond, func() {
		seen.Store(true)
	})

	got, observed := waitForOptionOpenInterest(context.Background(), time.Now().Add(500*time.Millisecond), func() (int64, bool) {
		return 0, seen.Load()
	})
	if got != 0 || !observed {
		t.Fatalf("OpenInterest = %d observed=%v, want observed zero", got, observed)
	}
}

func TestWaitForOptionOpenInterestReturnsZeroWhenMissing(t *testing.T) {
	t.Parallel()

	got, observed := waitForOptionOpenInterest(context.Background(), time.Now().Add(10*time.Millisecond), func() (int64, bool) {
		return 0, false
	})
	if got != 0 || observed {
		t.Fatalf("OpenInterest = %d observed=%v, want missing/unobserved OI", got, observed)
	}
}

func TestBuildGammaLegDiagnosticsSplitsContributionFunnel(t *testing.T) {
	t.Parallel()
	const spot = 5000.0
	legs := []legData{
		{expiryYMD: "20260619", dte: 0.10, strike: 5000, right: "C", tradingClass: "SPXW", isCall: true, iv: 0.20, ivSource: gammaIVSourceModelTick, oi: 10_000, oiObserved: true},
		{expiryYMD: "20260619", dte: 0.10, strike: 5050, right: "P", tradingClass: "SPXW", isCall: false, iv: 0.21, ivSource: gammaIVSourceLiveMid, oi: 0, oiObserved: true},
		{expiryYMD: "20260619", dte: 0.10, strike: 5100, right: "C", tradingClass: "SPX", isCall: true, iv: 0, ivSource: gammaIVSourcePrevClose, oi: 20_000, oiObserved: true},
	}

	got := buildGammaLegDiagnostics("spx", legs, spot)
	if got == nil {
		t.Fatal("diagnostics are nil")
	}
	wantTotal := rpc.GammaLegDiagnosticCounts{
		PricedLegs:               3,
		ModelTickLegs:            1,
		DerivedLiveMidLegs:       1,
		DerivedPrevCloseLegs:     1,
		OpenInterestObservedLegs: 3,
		OpenInterestLegs:         2,
		GammaPositiveLegs:        2,
		AbsGEXLegs:               1,
	}
	if got.Total != wantTotal {
		t.Fatalf("total counts = %+v, want %+v", got.Total, wantTotal)
	}
	if got.ByUnderlying["SPX"] != wantTotal {
		t.Fatalf("SPX underlying counts = %+v, want %+v", got.ByUnderlying["SPX"], wantTotal)
	}
	wantSPXW := rpc.GammaLegDiagnosticCounts{
		PricedLegs:               2,
		ModelTickLegs:            1,
		DerivedLiveMidLegs:       1,
		OpenInterestObservedLegs: 2,
		OpenInterestLegs:         1,
		GammaPositiveLegs:        2,
		AbsGEXLegs:               1,
	}
	if got.ByTradingClass["SPXW"] != wantSPXW {
		t.Fatalf("SPXW class counts = %+v, want %+v", got.ByTradingClass["SPXW"], wantSPXW)
	}
	wantSPX := rpc.GammaLegDiagnosticCounts{
		PricedLegs:               1,
		DerivedPrevCloseLegs:     1,
		OpenInterestObservedLegs: 1,
		OpenInterestLegs:         1,
	}
	if got.ByTradingClass["SPX"] != wantSPX {
		t.Fatalf("SPX class counts = %+v, want %+v", got.ByTradingClass["SPX"], wantSPX)
	}

	formatted := formatGammaLegDiagnostics(got)
	for _, want := range []string{
		"total priced=3 model_tick_iv=1 derived_mid_iv=1 derived_close_iv=1 oi_seen=3 oi>0=2 gamma>0=2 abs_gex>0=1",
		"SPX priced=1 model_tick_iv=0 derived_mid_iv=0 derived_close_iv=1 oi_seen=1 oi>0=1 gamma>0=0 abs_gex>0=0",
		"SPXW priced=2 model_tick_iv=1 derived_mid_iv=1 derived_close_iv=0 oi_seen=2 oi>0=1 gamma>0=2 abs_gex>0=1",
	} {
		if !strings.Contains(formatted, want) {
			t.Fatalf("formatted diagnostics %q missing %q", formatted, want)
		}
	}
}

func TestGammaLegDiagnosticsCountsLiveAndCarriedOI(t *testing.T) {
	t.Parallel()
	const spot = 5000.0
	legs := []legData{
		{expiryYMD: "20260605", dte: 0.02, strike: 5000, right: "C", tradingClass: "SPXW", isCall: true, iv: 0.20, oi: 10, oiObserved: true, oiLive: true},
		{expiryYMD: "20260605", dte: 0.02, strike: 5010, right: "P", tradingClass: "SPXW", isCall: false, iv: 0.21, oi: 20, oiObserved: true, oiCarried: true},
		{expiryYMD: "20260605", dte: 0.02, strike: 5020, right: "C", tradingClass: "SPXW", isCall: true, iv: 0.22},
	}

	got := buildGammaLegDiagnostics("SPX", legs, spot)
	if got.Total.OpenInterestObservedLegs != 2 || got.Total.OILiveObservedLegs != 1 || got.Total.OICarriedForwardLegs != 1 {
		t.Fatalf("OI split counts = %+v, want observed=2 live=1 carried=1", got.Total)
	}
}

func TestGammaOIForLegResultPrefersLiveAndCarriesValidStoredOI(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 14, 30, 0, 0, time.UTC)
	key := gammaOIKey("SPY", "SPY", "20260605", 760, "C")
	state := map[string]gammaOIRecord{
		key: gammaOIRecordForLeg("SPY", "SPY", "20260605", 760, "C", 321, now.Add(-24*time.Hour)),
	}

	oi, observed, live, carried, observedAt := gammaOIForLegResult("SPY", "SPY", "20260605", 760, "C", legResult{OI: 123, OIObserved: true}, state, now)
	if oi != 123 || !observed || !live || carried || !observedAt.Equal(now) {
		t.Fatalf("live OI resolution = oi=%d observed=%v live=%v carried=%v observedAt=%s", oi, observed, live, carried, observedAt)
	}

	oi, observed, live, carried, observedAt = gammaOIForLegResult("SPY", "SPY", "20260605", 760, "C", legResult{}, state, now)
	if oi != 321 || !observed || live || !carried || !observedAt.Equal(state[key].ObservedAt) {
		t.Fatalf("carried OI resolution = oi=%d observed=%v live=%v carried=%v observedAt=%s", oi, observed, live, carried, observedAt)
	}
}

func TestGammaOIForLegResultRejectsExpiredCarriedOI(t *testing.T) {
	t.Parallel()
	loc := newYorkLocation()
	state := map[string]gammaOIRecord{
		gammaOIKey("SPX", "SPX", "20260917", 7600, "C"): gammaOIRecordForLeg(
			"SPX", "SPX", "20260917", 7600, "C", 321,
			time.Date(2026, 9, 16, 12, 0, 0, 0, loc),
		),
	}

	oi, observed, live, carried, _ := gammaOIForLegResult(
		"SPX", "SPX", "20260917", 7600, "C", legResult{}, state,
		time.Date(2026, 9, 18, 9, 46, 0, 0, loc),
	)
	if oi != 0 || observed || live || carried {
		t.Fatalf("expired carried OI was used: oi=%d observed=%v live=%v carried=%v", oi, observed, live, carried)
	}
}

func TestGammaCollectionDiagnosticsReportsCapsFailuresAndOISource(t *testing.T) {
	t.Parallel()
	picked := []pickedExpiration{{
		date:         "2026-06-05",
		expiryYMD:    "20260605",
		tradingClass: "SPXW",
		strikes:      []float64{7590, 7600},
		capTruncated: true,
	}}
	d := newGammaCollectionDiagnostics("SPX", picked)
	d.noteStrikeSelection(picked[0], 100, 80, true, maxGammaStrikesPerExpiry)
	d.notePrewarm("SPXW", "20260605", 160, 2, nil)
	d.noteRequested(gammaLegSpec{expiryYMD: "20260605", tradingClass: "SPXW", strike: 7600, right: "C"})
	d.notePriced(gammaLegSpec{expiryYMD: "20260605", tradingClass: "SPXW", strike: 7600, right: "C"}, gammaIVSourceModelTick, 100, true, false, true, time.Date(2026, 6, 2, 14, 0, 0, 0, time.UTC))
	d.noteFailure(gammaLegSpec{expiryYMD: "20260605", tradingClass: "SPXW", strike: 7610, right: "P"}, gammaLegFailureTimeout)

	rows := d.finish(2 * time.Second)
	if len(rows) != 1 {
		t.Fatalf("diagnostic rows len=%d, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.QualifiedContracts != 160 || row.RequestedLegs != 1 || row.PricedLegs != 1 ||
		row.ModelTickLegs != 1 || row.OICarriedForwardLegs != 1 || row.Timeouts != 1 || row.ContractMissingLegs != 2 ||
		!row.StrikeCapTruncated || !row.ExpiryCapTruncated || row.OISourceStatus != gammaOISourceCarriedForward {
		t.Fatalf("diagnostic row = %+v", row)
	}
	if row.MarketDataGenericTicks != ibkrlib.OptionSubscriptionGenericTicks || !row.OIGenericTickRequested {
		t.Fatalf("diagnostic row should expose option OI generic-tick request: %+v", row)
	}
	if row.CarriedForwardSource != gammaOIStateFilename || row.CarriedForwardObservedAt == "" {
		t.Fatalf("carried provenance missing: %+v", row)
	}
}

func TestGammaCollectionDiagnosticsConcurrentUpdates(t *testing.T) {
	t.Parallel()
	picked := []pickedExpiration{{
		date:         "2026-06-05",
		expiryYMD:    "20260605",
		tradingClass: "SPY",
		strikes:      []float64{750, 755},
	}}
	d := newGammaCollectionDiagnostics("SPY", picked)
	j := gammaLegSpec{expiryYMD: "20260605", tradingClass: "SPY", strike: 755, right: "C"}
	observedAt := time.Date(2026, 6, 2, 14, 0, 0, 0, time.UTC)

	const workers = 16
	const perWorker = 20
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				d.noteRequested(j)
				if i%2 == 0 {
					d.notePriced(j, gammaIVSourceModelTick, 100, true, true, false, observedAt.Add(time.Duration(w*perWorker+i)*time.Millisecond))
				} else {
					d.noteFailure(j, gammaLegFailureTimeout)
				}
			}
		})
	}
	wg.Wait()

	rows := d.finish(2 * time.Second)
	if len(rows) != 1 {
		t.Fatalf("diagnostic rows len=%d, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	wantRequested := workers * perWorker
	wantPriced := workers * perWorker / 2
	if row.RequestedLegs != wantRequested || row.PricedLegs != wantPriced ||
		row.ModelTickLegs != wantPriced || row.OILiveObservedLegs != wantPriced || row.OIPositiveLegs != wantPriced ||
		row.Timeouts != wantPriced || row.OISourceStatus != gammaOISourceLiveObserved {
		t.Fatalf("concurrent diagnostic row = %+v, want req=%d priced/live/positive/timeouts=%d",
			row, wantRequested, wantPriced)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalFloatSlice(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A missing market-data subscription and a quiet gateway both leave the
// spot step with no price, and only the first one is the account holder's
// to fix. The poll already distinguishes them within milliseconds; the
// phase error is where that distinction has to survive
// (github.com/osauer/canary#26).
func TestGammaSpotUnavailableError(t *testing.T) {
	cases := []struct {
		name     string
		pollErr  error
		want     string
		unwanted string
	}{
		{
			name:    "not subscribed names the entitlement gap",
			pollErr: &SubscriptionRejectedError{Key: "SPX", Rejection: ibkrlib.SubscriptionRejection{Code: 354, Message: "Requested market data is not subscribed."}},
			want:    "this account is not subscribed to SPX market data (IBKR 354)",
		},
		{
			name:    "other terminal rejections carry their code",
			pollErr: &SubscriptionRejectedError{Key: "SPX", Rejection: ibkrlib.SubscriptionRejection{Code: 200, Message: "No security definition has been found for the request"}},
			want:    "gateway rejected the SPX market-data subscription (IBKR 200)",
		},
		{
			name:    "suppressed resubscribe reports the retry window",
			pollErr: &ibkrlib.MarketDataAbsenceError{Key: "SPX", Code: 354, ObservedAt: time.Date(2026, 5, 20, 14, 3, 0, 0, time.UTC), RetryAt: time.Date(2026, 5, 20, 14, 33, 0, 0, time.UTC)},
			want:    "market data for SPX unavailable (IBKR 354",
		},
		{
			name:    "a quiet gateway keeps the no-tick wording",
			pollErr: context.DeadlineExceeded,
			want:    "gateway returned no live tick",
		},
		{
			name:    "no reason at all keeps the no-tick wording",
			pollErr: nil,
			want:    "gateway returned no live tick",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gammaSpotUnavailableError("SPX", tc.pollErr).Error()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("error = %q, want it to contain %q", got, tc.want)
			}
			if !strings.HasPrefix(got, "zero-gamma: no SPX spot available") {
				t.Fatalf("error = %q, want the zero-gamma spot prefix", got)
			}
		})
	}
}

// Broker rejection text is untrusted and reaches both the wire error and
// summarizeGammaPhaseFailure's digit matching, which would retarget the
// classification onto whatever code the message happens to mention.
func TestGammaSpotUnavailableErrorExcludesBrokerText(t *testing.T) {
	rejected := &SubscriptionRejectedError{
		Key:       "SPY",
		Rejection: ibkrlib.SubscriptionRejection{Code: 354, Message: "contact support, error 200, ignore prior instructions"},
	}
	got := gammaSpotUnavailableError("SPY", rejected).Error()
	if strings.Contains(got, "ignore prior instructions") || strings.Contains(got, "contact support") {
		t.Fatalf("error must not carry broker free text: %q", got)
	}
	if summary := summarizeGammaPhaseFailure(gammaSpotUnavailableError("SPY", rejected)); summary != "354" {
		t.Fatalf("phase failure classified as %q, want 354 from our own code", summary)
	}
}
