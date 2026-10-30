// Command account-data prints the owner's current holdings and live money
// figures for the account-data gate (scripts/check-no-account-data.sh). It
// asks the running daemon over its socket and never starts one: a commit hook
// must not open a broker session. Nor does it open daemon.db, which only the
// daemon may: an outside SQLite opener resets a live daemon's shared memory
// if the daemon's lock on it is lost (crash of 2026-10-29).
//
// Output, one row per line:
//
//	holdings|available|DAY   then held|TICKER rows
//	holdings|unavailable|WHY
//	money|available          then nlv|VALUE|DAY and cash|VALUE| rows
//	money|paper|             the connected account is not the live book
//	money|unavailable|WHY
//
// It exits 0 when the daemon answered and 2 when no daemon is reachable.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

const exitNoDaemon = 2

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	os.Exit(run(ctx, dial.DefaultSocketPath(), os.Stdout, os.Stderr))
}

func run(ctx context.Context, socket string, out, errOut io.Writer) int {
	conn, err := dial.Connect(socket)
	if err != nil {
		fmt.Fprintf(errOut, "account-data: no daemon reachable: %v\n", err)
		return exitNoDaemon
	}
	defer conn.Close()

	var positions rpc.PositionsResult
	if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &positions); err != nil {
		fmt.Fprintf(out, "holdings|unavailable|%s\n", oneLine(err.Error()))
	} else if why := unavailable(positions.Authority); why != "" {
		fmt.Fprintf(out, "holdings|unavailable|%s\n", why)
	} else if positions.Authority.Scope.AccountMode != rpc.AccountModeLive {
		// A paper or unidentified book cannot replace the saved live denylist
		// or prove that the live account is flat. Keep the gate's fallback.
		fmt.Fprintln(out, "holdings|unavailable|the selected account is not live")
	} else {
		fmt.Fprintf(out, "holdings|available|%s\n", day(positions.AsOf))
		for _, p := range append(positions.Stocks, positions.Options...) {
			for _, s := range []string{p.Symbol, p.LocalSymbol} {
				// An OCC leg ("ZQXOPT1 260918C00005000") and an IBKR class
				// suffix ("BRK B") keep their root.
				if root, _, _ := strings.Cut(strings.TrimSpace(s), " "); root != "" {
					fmt.Fprintf(out, "held|%s\n", root)
				}
			}
		}
	}

	var account rpc.AccountResult
	if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &account); err != nil {
		fmt.Fprintf(out, "money|unavailable|%s\n", oneLine(err.Error()))
		return 0
	}
	if why := unavailable(account.Authority); why != "" {
		fmt.Fprintf(out, "money|unavailable|%s\n", why)
		return 0
	}
	if account.Authority.Scope.AccountMode != "live" {
		fmt.Fprintln(out, "money|paper|")
		return 0
	}
	fmt.Fprintln(out, "money|available")
	nlv := "missing"
	if account.Authority.Fields != nil && account.Authority.Fields.NetLiquidation {
		nlv = number(account.NetLiquidation)
	}
	fmt.Fprintf(out, "nlv|%s|%s\n", nlv, day(account.AsOf))
	ledger := account.CurrencyExposure
	if account.BaseCurrencyLedger != nil {
		ledger = append(ledger, *account.BaseCurrencyLedger)
	}
	for _, c := range ledger {
		if c.CashObserved {
			fmt.Fprintf(out, "cash|%s|\n", number(c.CashCcy))
		}
		if c.SettledCashCcy != nil {
			fmt.Fprintf(out, "cash|%s|\n", number(*c.SettledCashCcy))
		}
	}
	return 0
}

// unavailable says why a read cannot stand as account truth, or "" when it
// can. An empty answer from a disconnected broker proves nothing.
func unavailable(a *rpc.AccountDataAuthority) string {
	switch {
	case a == nil:
		return "the daemon sent no account authority"
	case a.Availability != rpc.AccountDataAvailable:
		return oneLine(nonEmpty(string(a.Reason), "the broker data is unavailable"))
	}
	return ""
}

func day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
}

func number(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func oneLine(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "|", "/") }

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
