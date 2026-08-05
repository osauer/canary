package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runAccount(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "account")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	watch := fs.Bool("watch", false, "re-poll on a fixed interval; in-place redraw on a TTY")
	rate := fs.Duration("rate", time.Second, "poll interval for --watch")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	fetchAndRender := func(out io.Writer) int {
		var res rpc.AccountResult
		if err := env.Conn.Call(ctx, rpc.MethodAccountSummary, nil, &res); err != nil {
			return fail(env, "account: %v", err)
		}
		if *jsonOut {
			return printJSONTo(env, out, res)
		}
		return renderAccountTextTo(env, out, &res)
	}

	if *watch {
		if *jsonOut {
			return fail(env, "account: --watch and --json are mutually exclusive")
		}
		return runWatch(ctx, env, *rate, "account", fetchAndRender)
	}
	return fetchAndRender(env.Stdout)
}

// renderAccountText is preserved for tests and preview that pass an Env
// holding the destination writer. The new entry point with an explicit
// writer is renderAccountTextTo; this thin wrapper keeps existing callers
// working unchanged.
func renderAccountText(env *Env, a *rpc.AccountResult) int {
	return renderAccountTextTo(env, env.Stdout, a)
}

// renderAccountTextTo writes the account snapshot to out. The block is
// laid out so the two questions a trader asks first — "what's it worth?"
// and "how am I doing today?" — land on the first two lines, with the
// rest of the snapshot reading top-to-bottom in priority order.
func renderAccountTextTo(env *Env, out io.Writer, a *rpc.AccountResult) int {
	fmt.Fprintln(out)
	fields, typedFields := accountResultFields(a)
	base := strings.TrimSpace(a.BaseCurrency)
	if typedFields && !fields.BaseCurrency {
		base = ""
	}
	accountID := a.AccountID
	accountMode := ""
	if a.Authority != nil {
		if a.Authority.Scope.AccountID != "" {
			accountID = a.Authority.Scope.AccountID
		}
		accountMode = a.Authority.Scope.AccountMode
	}
	header := fmt.Sprintf("Account  %s · base=%s", nonEmpty(accountID, "—"), nonEmpty(base, "—"))
	if accountMode != "" {
		header += " · " + accountMode
	}
	if a.AccountType != "" {
		header += " · type=" + env.dim(a.AccountType)
	}
	header += env.suffixBadge(a.DataType)
	fmt.Fprintln(out, header)
	fmt.Fprintln(out, env.rule(44))
	// Width 14 fits the widest typical NLV with a thousands-grouped
	// currency symbol ("€ 992,841.68" is 12; -€ 999,999.99 is 13).
	const valW = 14
	fieldAvailable := func(available bool) bool {
		return !typedFields || (a.Authority.Availability == rpc.AccountDataAvailable && fields.BaseCurrency && available)
	}
	money := func(value float64, available bool) string {
		return env.formatAccountMoneyRight(value, base, valW, fieldAvailable(available), typedFields)
	}
	signedMoney := func(value float64, available bool) string {
		return env.formatAccountSignedMoneyRight(value, base, valW, fieldAvailable(available), typedFields)
	}
	writeAccountAuthorityNotice(env, out, a.Authority)

	// Hero block: net liquidation + today's delta. These are the two
	// numbers a trader scans for first; everything below is supporting
	// detail. Daily P&L sits here even when nil so the line is present
	// from call one — the lazy reqPnL subscription kicks in on the
	// first call and lands a value on the next refresh.
	fmt.Fprintf(out, "  Net liquidation         %s\n",
		env.bold(money(a.NetLiquidation, fields.NetLiquidation)))
	writeDailyPnLLine(env, out, a, base, valW, fieldAvailable(fields.DailyPnL), typedFields)

	fmt.Fprintln(out, env.dim("  Balances"))
	fmt.Fprintf(out, "    Buying power            %s\n", money(a.BuyingPower, fields.BuyingPower))
	fmt.Fprintf(out, "    Available funds         %s\n", money(a.AvailableFunds, fields.AvailableFunds))
	fmt.Fprintf(out, "    Excess liquidity        %s\n", money(a.ExcessLiquidity, fields.ExcessLiquidity))
	fmt.Fprintf(out, "    Total cash              %s\n", money(a.TotalCash, fields.TotalCash))
	fmt.Fprintf(out, "    Gross position value    %s\n", money(a.GrossPositionValue, fields.GrossPositionValue))
	fmt.Fprintf(out, "    Cushion                 %s\n", env.formatAccountCushion(a.Cushion, valW, !typedFields || (a.Authority.Availability == rpc.AccountDataAvailable && fields.Cushion), typedFields))

	// Session P&L: inception-to-now Unrealized (vs cost basis on every
	// open position) plus today's Realized closed-trade total. Sign-
	// coloured both directions. Different time horizon than the Daily
	// P&L line above (start-of-trading-day delta from reqPnL).
	fmt.Fprintln(out, env.dim("  Session P&L"))
	fmt.Fprintf(out, "    Unrealized (open)       %s\n", signedMoney(a.UnrealizedPnL, fields.UnrealizedPnL))
	fmt.Fprintf(out, "    Realized (today)        %s\n", signedMoney(a.RealizedPnL, fields.RealizedPnL))

	fmt.Fprintln(out, env.dim("  Margin"))
	fmt.Fprintf(out, "    Initial                 %s\n", money(a.InitialMargin, fields.InitialMargin))
	fmt.Fprintf(out, "    Maintenance             %s\n", money(a.MaintenanceMargin, fields.MaintenanceMargin))

	// Look-ahead block: only when at least one value lands. Cash accounts
	// (and stub gateways pre-handshake) leave the whole quad zero, which
	// would otherwise render as four em-dashes adding noise.
	hasLookAhead := a.LookAheadInitMargin != 0 || a.LookAheadMaintMargin != 0 || a.LookAheadAvailable != 0 || a.LookAheadExcess != 0
	if typedFields {
		hasLookAhead = fields.LookAheadInitMargin || fields.LookAheadMaintMargin || fields.LookAheadAvailable || fields.LookAheadExcess
	}
	if hasLookAhead {
		fmt.Fprintln(out, env.dim("  Look-ahead margin"))
		fmt.Fprintf(out, "    Initial                 %s\n", money(a.LookAheadInitMargin, fields.LookAheadInitMargin))
		fmt.Fprintf(out, "    Maintenance             %s\n", money(a.LookAheadMaintMargin, fields.LookAheadMaintMargin))
		fmt.Fprintf(out, "    Available funds         %s\n", money(a.LookAheadAvailable, fields.LookAheadAvailable))
		fmt.Fprintf(out, "    Excess liquidity        %s\n", money(a.LookAheadExcess, fields.LookAheadExcess))
	}

	// reqPnL-stream totals sit at the bottom because the headline figure is
	// already on the hero row. These are the account's TOTAL unrealized /
	// realized P&L from the same msg 94 frame as the Daily P&L headline
	// (inception-to-now), NOT a breakdown of the daily figure — they do not
	// sum to it. They measure the same quantity as the Session P&L block
	// above but come off the reqPnL feed rather than account-updates, so the
	// two can legitimately differ. Optional — older gateways emit only the
	// bare daily figure.
	if a.PnLUnrealizedTotal != nil || a.PnLRealizedTotal != nil {
		fmt.Fprintln(out, env.dim("  Total P&L (reqPnL stream)"))
		if a.PnLUnrealizedTotal != nil {
			fmt.Fprintf(out, "    Unrealized              %s\n",
				env.formatAccountPnLPtrRight(a.PnLUnrealizedTotal, base, valW, fieldAvailable(fields.PnLUnrealizedTotal), typedFields))
		}
		if a.PnLRealizedTotal != nil {
			fmt.Fprintf(out, "    Realized                %s\n",
				env.formatAccountPnLPtrRight(a.PnLRealizedTotal, base, valW, fieldAvailable(fields.PnLRealizedTotal), typedFields))
		}
	}

	renderCurrencyExposureTo(env, out, a)
	asOf := a.AsOf
	if a.Authority != nil && !a.Authority.AsOf.IsZero() {
		asOf = a.Authority.AsOf
	}
	fmt.Fprintf(out, "  as of %s\n", formatTimeShort(asOf))
	return 0
}

func accountResultFields(a *rpc.AccountResult) (rpc.AccountFieldAvailability, bool) {
	if a == nil || a.Authority == nil || a.Authority.Fields == nil {
		return rpc.AccountFieldAvailability{}, false
	}
	return *a.Authority.Fields, true
}

func writeAccountAuthorityNotice(env *Env, out io.Writer, authority *rpc.AccountDataAuthority) {
	if authority == nil || (authority.Availability == rpc.AccountDataAvailable && authority.Freshness == rpc.AccountDataFreshnessCurrent) {
		return
	}
	message := accountAuthorityReason(authority.Reason)
	if message == "" {
		message = "account data is not current"
	}
	fmt.Fprintf(out, "  %s\n", env.yellow("Data note: "+message))
}

func accountAuthorityReason(reason rpc.AccountDataReason) string {
	switch reason {
	case rpc.AccountDataReasonUnstampedCache:
		return "showing cached account values; their observation time is unknown"
	case rpc.AccountDataReasonScopeUnresolved:
		return "one account has not been selected"
	case rpc.AccountDataReasonScopeConflict:
		return "the broker data belongs to a different account"
	case rpc.AccountDataReasonAccountUnbound:
		return "the broker stream has not identified its account"
	case rpc.AccountDataReasonAccountMismatch:
		return "the broker stream account does not match the selected account"
	case rpc.AccountDataReasonUnprimed:
		return "the initial portfolio download is not complete"
	case rpc.AccountDataReasonInvalidPayload:
		return "the broker sent an invalid account payload"
	case rpc.AccountDataReasonClockInvalid:
		return "the observation time is invalid"
	case rpc.AccountDataReasonReceiptStale:
		return "the last complete portfolio receipt is stale"
	case rpc.AccountDataReasonSessionChanged:
		return "the broker session changed during the read"
	default:
		return ""
	}
}

// writeDailyPnLLine prints the hero-row Daily P&L line. When the gateway
// hasn't delivered a frame yet, the line still renders with a dim em-dash
// + "(subscribing — value lands on next call)" hint so a first-time user
// sees the field exists. The first call starts the lazy reqPnL subscription;
// a later call can carry the observed value.
func writeDailyPnLLine(env *Env, out io.Writer, a *rpc.AccountResult, base string, w int, available, typed bool) {
	if a.DailyPnL == nil {
		fmt.Fprintf(out, "  Daily P&L               %s  %s\n",
			env.dim(padDash(w)),
			env.dim("(subscribing — value lands on next call)"))
		return
	}
	fmt.Fprintf(out, "  Daily P&L               %s\n",
		env.formatAccountPnLPtrRight(a.DailyPnL, base, w, available, typed))
}

func (e *Env) formatAccountMoneyRight(v float64, ccy string, w int, available, typed bool) string {
	if strings.TrimSpace(ccy) == "" || (typed && !available) {
		return e.dim(padDash(w))
	}
	if !typed {
		return e.formatMoneyNegCcyRight(v, ccy, w)
	}
	s := formatMoneyCcyForPnL(v, ccy)
	if pad := w - len(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return e.colorBySign(v, s, signNeg)
}

func (e *Env) formatAccountSignedMoneyRight(v float64, ccy string, w int, available, typed bool) string {
	if strings.TrimSpace(ccy) == "" || (typed && !available) {
		return e.dim(padDash(w))
	}
	if !typed {
		return e.formatSignedMoneyRight(v, ccy, w)
	}
	s := formatMoneyCcyForPnL(v, ccy)
	if v > 0 {
		s = "+" + s
	}
	if pad := w - len(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return e.colorBySign(v, s, signPnL)
}

func (e *Env) formatAccountPnLPtrRight(v *float64, ccy string, w int, available, typed bool) string {
	if v == nil || strings.TrimSpace(ccy) == "" || (typed && !available) {
		return e.dim(padDash(w))
	}
	return e.formatPnLCcyRight(*v, ccy, w)
}

func (e *Env) formatAccountCushion(v float64, w int, available, typed bool) string {
	if typed && !available {
		return e.dim(padDash(w))
	}
	if !typed || v != 0 {
		return e.formatCushion(v, w)
	}
	s := padLeftVisible("0.00 ⚠", w)
	return e.red(s)
}

// formatCushion renders the margin cushion as a 2-decimal ratio with a
// severity badge. Banding follows IBKR's convention: < 0.10 is the
// margin-warning zone (red ⚠), 0.10–0.30 is yellow (watch), > 0.30 is
// comfortable (green ✓). Zero renders as the dim em-dash placeholder so
// pre-handshake state stays distinct from "real cushion of exactly 0",
// which would itself land in the red zone.
func (e *Env) formatCushion(v float64, w int) string {
	if v == 0 {
		return e.dim(padDash(w))
	}
	s := fmt.Sprintf("%.2f", v)
	switch {
	case v < 0.10:
		s += " ⚠"
		s = padLeftVisible(s, w)
		return e.red(s)
	case v < 0.30:
		s = padLeftVisible(s, w)
		return e.yellow(s)
	default:
		s += " ✓"
		s = padLeftVisible(s, w)
		return e.green(s)
	}
}

// formatSignedMoneyRight renders v as money with the supplied currency
// prefix, right-aligned to w visible cells, and colours it green/red/dim
// by sign (positive green, negative red, zero dim) using the signPnL
// rule shared by P&L renderers. Positive values explicitly carry a leading
// "+" so a trader scanning the column never has to deduce direction from
// colour alone (colour-blindness, dim terminals, NO_COLOR).
func (e *Env) formatSignedMoneyRight(v float64, ccy string, w int) string {
	if v == 0 {
		return e.dim(padDash(w))
	}
	s := formatMoneyCcy(v, ccy)
	if v > 0 {
		s = "+" + s
	}
	if pad := w - len(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return e.colorBySign(v, s, signPnL)
}

// renderCurrencyExposureTo prints one row per non-base currency holding.
// Empty when the account is single-currency or the daemon hasn't
// received the $LEDGER:ALL response yet (pre-handshake).
func renderCurrencyExposureTo(env *Env, out io.Writer, a *rpc.AccountResult) {
	if len(a.CurrencyExposure) == 0 {
		return
	}
	base := strings.TrimSpace(a.BaseCurrency)
	if base == "" {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Currency exposure  (base=%s)\n", base)
	fmt.Fprintln(out, env.rule(60))
	fmt.Fprintf(out, "  %-4s   %16s   %12s   %16s\n",
		"CCY", "NET LIQ (CCY)", "FX→BASE", "NET LIQ (BASE)")
	for _, ex := range a.CurrencyExposure {
		fmt.Fprintf(out, "  %-4s   %16s   %12s   %16s\n",
			ex.Currency,
			formatMoneyCcy(ex.NetLiquidationCcy, ex.Currency),
			fmtFX(ex.ExchangeRate),
			formatMoneyCcy(ex.NetLiquidationBase, base))
	}
}

// fmtFX renders an exchange rate to 4 decimals. Em-dash when zero — the
// gateway never sends a real FX of zero, but the field IS zero on
// pre-handshake rows we filter out elsewhere, defense-in-depth.
func fmtFX(v float64) string {
	if v <= 0 {
		return padDash(12)
	}
	return fmt.Sprintf("%12.4f", v)
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
