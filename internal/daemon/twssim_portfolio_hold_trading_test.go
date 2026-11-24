//go:build trading

package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// positionsRead is one positions.list read's authority.
func (d *simDesk) positionsRead() rpc.AccountDataAuthority {
	d.t.Helper()
	pos, rerr := dispatchJSON[rpc.PositionsResult](d.t, d.srv, rpc.MethodPositionsList, rpc.PositionsListParams{})
	if rerr != nil || pos.Authority == nil {
		d.t.Fatalf("positions.list: %+v", rerr)
	}
	return *pos.Authority
}

func (d *simDesk) portfolioSubsystem() (rpc.SubsystemHealth, bool) {
	d.t.Helper()
	st, rerr := dispatchJSON[rpc.HealthResult](d.t, d.srv, rpc.MethodStatusHealth, struct{}{})
	if rerr != nil {
		d.t.Fatalf("status.health: %+v", rerr)
	}
	i := slices.IndexFunc(st.Subsystems, func(s rpc.SubsystemHealth) bool { return s.Name == "portfolio" })
	if i < 0 {
		return rpc.SubsystemHealth{}, false
	}
	return st.Subsystems[i], true
}

func bookCurrent(a rpc.AccountDataAuthority) bool {
	return a.Availability == rpc.AccountDataAvailable && a.Freshness == rpc.AccountDataFreshnessCurrent
}

// P1. 2026-11-24 12:51 CET: TWS restarts under the running daemon and, just
// logged in, answers the new session's account updates with the account
// values and an end marker but no portfolio rows; a subscribe on the live
// subscription gets nothing. The book is held, never served short, and
// status says so with its start while it lasts. Once TWS has the portfolio,
// the daemon's repair downloads it without a restart, inside about a minute.
func TestSimulatedTWSRestartFirstAnswersAnEmptyPortfolio(t *testing.T) {
	d := startSimDesk(t, twssim.Options{}, true)
	d.waitFor("a current book", 30*time.Second, func() bool { return bookCurrent(d.positionsRead()) })

	d.sim.SetPortfolioLoaded(false, false)
	d.sim.DropSessions()
	var held rpc.SubsystemHealth
	d.waitFor("the held book in status", 90*time.Second, func() bool {
		var ok bool
		held, ok = d.portfolioSubsystem()
		return ok
	})
	read := d.positionsRead()
	if bookCurrent(read) || held.Status != "degraded" || held.StartedAt.IsZero() || !read.HeldSince.Equal(held.StartedAt) {
		t.Fatalf("held book: status %+v, positions %+v", held, read)
	}

	d.sim.SetPortfolioLoaded(true, false)
	loaded := time.Now()
	d.waitFor("the book current again without a restart", 75*time.Second, func() bool { return bookCurrent(d.positionsRead()) })
	t.Logf("book current %s after TWS had it", time.Since(loaded).Round(time.Second))
	if _, ok := d.portfolioSubsystem(); ok {
		t.Fatal("status still names a held book")
	}
	if strings.Contains(d.log.String(), "has not sent the whole portfolio") {
		t.Fatal("the repair needed a redial")
	}
}
