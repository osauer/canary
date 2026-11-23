package main

import (
	"strconv"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// The controls an exit plan's end-to-end tests use: a session opened a few
// seconds short of the plan's wait, a stock move its options follow, a
// stock feed that stops being usable, and an option spread too wide to sell.
func TestControlsForExitPlans(t *testing.T) {
	t.Cleanup(marketcal.ClearSimulatedSessions)
	sim := twssim.Start(t, twssim.Options{Book: twssim.OptionsBook(time.Now())})
	now := time.Now()
	if _, err := control(sim, []string{"session", "us_options", "opened-ago", "1195"}, now); err != nil {
		t.Fatal(err)
	}
	s, err := marketcal.New().SessionAt(marketcal.MarketUSOptions, now)
	if err != nil || !s.IsOpen || s.Open.Sub(now.Add(-1195*time.Second)).Abs() > time.Second {
		t.Fatalf("opened-ago: %+v %v", s, err)
	}
	exps := twssim.OptionExpiries(now)
	call, _ := sim.Book().OptionConIDFor(900001, exps[3], 50, "C")
	before, _ := sim.Book().Contract(call)
	if _, err := control(sim, []string{"quote", "SYNA", "53"}, now); err != nil {
		t.Fatal(err)
	}
	after, _ := sim.Book().Contract(call)
	if after.Price <= before.Price {
		t.Fatalf("the call did not follow its stock: %g then %g", before.Price, after.Price)
	}
	for _, args := range [][]string{{"feed", "SYNA", "delayed"}, {"feed", "SYNA", "none"}, {"feed", "SYNA", "one-sided"}, {"feed", "SYNA", "live"},
		{"spread", strconv.Itoa(call), "60"}, {"spread", strconv.Itoa(call), "0"}} {
		if _, err := control(sim, args, now); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"feed", "SYNA", "sometimes"}, {"feed", "SYNQ", "none"}, {"spread", "1", "10"}, {"session", "us_options", "opened-ago", "x"}} {
		if _, err := control(sim, args, now); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}
