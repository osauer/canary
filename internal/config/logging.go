package config

import (
	"fmt"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
)

// GatewayLogMarkets resolves the configured operating baseline. Empty means
// always required; it never means no markets. Automatic duties still widen it.
func (d Daemon) GatewayLogMarkets() []marketcal.Market {
	if len(d.LogMarkets) == 1 && d.LogMarkets[0] == "always" {
		return nil
	}
	if d.LogMarkets == nil {
		return marketcal.AllMarkets()
	}
	out := make([]marketcal.Market, 0, len(d.LogMarkets))
	for _, m := range d.LogMarkets {
		out = append(out, marketcal.Market(m))
	}
	return out
}

// GatewayLogPadding includes preparation and post-close duties in diagnostics.
func (d Daemon) GatewayLogPadding() (time.Duration, time.Duration) {
	before, after := 360, 240
	if d.LogBeforeOpenMinutes != nil {
		before = *d.LogBeforeOpenMinutes
	}
	if d.LogAfterCloseMinutes != nil {
		after = *d.LogAfterCloseMinutes
	}
	return time.Duration(before) * time.Minute, time.Duration(after) * time.Minute
}

func (d Daemon) validateLogging() error {
	if d.LogCalendarMode != "" && d.LogCalendarMode != "conservative" && d.LogCalendarMode != "scheduled" {
		return fmt.Errorf("daemon.log_calendar_mode must be conservative or scheduled")
	}
	if d.LogMarkets != nil && len(d.LogMarkets) == 0 {
		return fmt.Errorf("daemon.log_markets must not be empty; use [\"always\"] for continuous warnings")
	}
	seen := map[string]bool{}
	for _, m := range d.LogMarkets {
		if seen[m] || !(slices.Contains(marketcal.AllMarkets(), marketcal.Market(m)) || m == "always" && len(d.LogMarkets) == 1) {
			return fmt.Errorf("invalid or duplicate daemon.log_markets value %q", m)
		}
		seen[m] = true
	}
	for _, n := range []*int{d.LogBeforeOpenMinutes, d.LogAfterCloseMinutes} {
		if n != nil && (*n < 0 || *n > 720) {
			return fmt.Errorf("daemon logging preparation/post-close minutes must be between 0 and 720")
		}
	}
	return nil
}
