package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	purgeBookKind          = "ibkr.purge_book"
	purgeBookSchemaVersion = "purge-book-v1"
	activePurgeBookID      = "active"

	purgeBookStatusDraft = "draft"

	purgeOriginalSideLong  = "LONG"
	purgeOriginalSideShort = "SHORT"

	purgeLegStatusDraft      = "draft"
	purgeLegStatusPriced     = "priced"
	purgeLegStatusStale      = "stale"
	purgeLegStatusUnpriced   = "unpriced"
	purgeLegStatusFractional = "fractional"

	purgeRestoreKind          = "ibkr.purge_restore_plan"
	purgeRestoreSchemaVersion = "purge-restore-v1"
)

var purgeBookDefaultDir = defaultPurgeBookDir

type purgeBook struct {
	Kind             string          `json:"kind"`
	SchemaVersion    string          `json:"schema_version"`
	PurgeID          string          `json:"purge_id"`
	Status           string          `json:"status"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	AccountID        string          `json:"account_id,omitempty"`
	BaseCurrency     string          `json:"base_currency,omitempty"`
	Source           string          `json:"source"`
	SourceAsOf       time.Time       `json:"source_as_of,omitzero"`
	PositionCount    int             `json:"position_count"`
	BookPath         string          `json:"book_path,omitempty"`
	Legs             []purgeBookLeg  `json:"legs"`
	Totals           purgeBookTotals `json:"totals"`
	Warnings         []string        `json:"warnings,omitempty"`
	NotExecution     string          `json:"not_execution"`
	RestoreCommand   string          `json:"restore_command,omitempty"`
	MonitorCommand   string          `json:"monitor_command,omitempty"`
	ExecutionCommand string          `json:"execution_command,omitempty"`
}

type purgeBookLeg struct {
	LegID                  string             `json:"leg_id"`
	Symbol                 string             `json:"symbol"`
	SecType                string             `json:"sec_type"`
	Contract               rpc.ContractParams `json:"contract"`
	OriginalSide           string             `json:"original_side"`
	OriginalQuantity       float64            `json:"original_quantity"`
	PurgeAction            string             `json:"purge_action"`
	RestoreAction          string             `json:"restore_action"`
	Quantity               float64            `json:"quantity"`
	Multiplier             int                `json:"multiplier"`
	Currency               string             `json:"currency,omitempty"`
	ExitPrice              float64            `json:"exit_price"`
	ExitPriceSource        string             `json:"exit_price_source"`
	ExitValue              float64            `json:"exit_value"`
	CurrentPrice           *float64           `json:"current_price,omitempty"`
	CurrentPriceSource     string             `json:"current_price_source,omitempty"`
	CurrentRestoreValue    *float64           `json:"current_restore_value,omitempty"`
	ShadowSaved            *float64           `json:"shadow_saved,omitempty"`
	ShadowSavedPctExit     *float64           `json:"shadow_saved_pct_exit,omitempty"`
	LowRestoreValue        *float64           `json:"low_restore_value,omitempty"`
	HighRestoreValue       *float64           `json:"high_restore_value,omitempty"`
	LowPrice               *float64           `json:"low_price,omitempty"`
	HighPrice              *float64           `json:"high_price,omitempty"`
	LastQuoteAt            time.Time          `json:"last_quote_at,omitzero"`
	DataType               string             `json:"data_type,omitempty"`
	QuoteQuality           string             `json:"quote_quality,omitempty"`
	Status                 string             `json:"status"`
	Warnings               []string           `json:"warnings,omitempty"`
	Estimated              bool               `json:"estimated"`
	OriginalMarketValueCCY float64            `json:"original_market_value_ccy,omitempty"`
}

type purgeBookTotals struct {
	ExitValue           float64  `json:"exit_value"`
	CurrentRestoreValue *float64 `json:"current_restore_value,omitempty"`
	ShadowSaved         *float64 `json:"shadow_saved,omitempty"`
	ShadowSavedPctExit  *float64 `json:"shadow_saved_pct_exit,omitempty"`
	PricedLegs          int      `json:"priced_legs"`
	UnpricedLegs        int      `json:"unpriced_legs"`
	LongLegs            int      `json:"long_legs"`
	ShortLegs           int      `json:"short_legs"`
}

type purgeRestorePlan struct {
	Kind             string             `json:"kind"`
	SchemaVersion    string             `json:"schema_version"`
	PurgeID          string             `json:"purge_id"`
	AsOf             time.Time          `json:"as_of"`
	AccountID        string             `json:"account_id,omitempty"`
	BaseCurrency     string             `json:"base_currency,omitempty"`
	Scale            float64            `json:"scale"`
	Only             []string           `json:"only,omitempty"`
	Legs             []purgeRestoreLeg  `json:"legs"`
	Totals           purgeRestoreTotals `json:"totals"`
	Warnings         []string           `json:"warnings,omitempty"`
	NotExecution     string             `json:"not_execution"`
	PreviewAvailable bool               `json:"preview_available"`
	Recorded         bool               `json:"recorded"`
}

type purgeRestoreLeg struct {
	LegID               string             `json:"leg_id"`
	Symbol              string             `json:"symbol"`
	SecType             string             `json:"sec_type"`
	Contract            rpc.ContractParams `json:"contract"`
	Action              string             `json:"action"`
	Quantity            float64            `json:"quantity"`
	MaxQuantity         float64            `json:"max_quantity"`
	ReferencePrice      *float64           `json:"reference_price,omitempty"`
	EstimatedValue      *float64           `json:"estimated_value,omitempty"`
	ShadowSavedAfterLeg *float64           `json:"shadow_saved_after_leg,omitempty"`
	Status              string             `json:"status"`
	Warnings            []string           `json:"warnings,omitempty"`
}

type purgeRestoreTotals struct {
	SelectedLegs    int      `json:"selected_legs"`
	EstimatedValue  *float64 `json:"estimated_value,omitempty"`
	ShadowSavedUsed *float64 `json:"shadow_saved_used,omitempty"`
}

type purgeRPCConn interface {
	Call(context.Context, string, any, any) error
}

func runPurge(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 {
		return fail(env, "purge: usage is `canary purge SYMBOL` or `canary purge --all`")
	}
	// "help" must never reach the ticker path: a bare word is otherwise treated
	// as a symbol and `canary purge help` would close a real position.
	switch args[0] {
	case "help", "--help", "-h":
		return fail(env, "purge: usage is `canary purge SYMBOL|'*' [--bypass-preview=true] [--wait 2s] [--json]`, `canary purge --all [...]`, `canary purge restore SYMBOL|'*' [--scale 0.5] [--execute]`, `canary purge status [PURGE_ID]`, `canary purge monitor [PURGE_ID]`, or `canary purge dry-run`")
	}
	subIdx := purgeSubcommandIndex(args)
	if subIdx < 0 {
		return runPurgeTicker(ctx, env, args)
	}
	sub := args[subIdx]
	args = append(append([]string{}, args[:subIdx]...), args[subIdx+1:]...)
	switch sub {
	case "dry-run":
		return runPurgeDryRun(ctx, env, args)
	case "status":
		return runPurgeStatus(ctx, env, args, false)
	case "monitor":
		return runPurgeStatus(ctx, env, args, true)
	case "restore":
		return runPurgeRestore(ctx, env, args)
	case "execute":
		return runPurgeExecute(ctx, env, args)
	default:
		return fail(env, "purge: unknown subcommand %q (try dry-run, status, monitor, restore, or execute)", sub)
	}
}

func purgeSubcommandIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			name := strings.TrimLeft(arg, "-")
			if before, _, ok := strings.Cut(name, "="); ok {
				name = before
			}
			if isValueFlag(name) && !strings.Contains(arg, "=") {
				i++
			}
			continue
		}
		switch arg {
		case "dry-run", "status", "monitor", "restore", "execute":
			return i
		}
		return -1
	}
	return -1
}

func runPurgeTicker(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "purge")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	all := fs.Bool("all", false, "target all open positions")
	wait := fs.Duration("wait", 2*time.Second, "wait briefly for immediate broker lifecycle callbacks after sending")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	target, err := purgeTargetArg(*all, fs.Args(), "canary purge SYMBOL|'*' [--wait 2s] [--json] or canary purge --all [--wait 2s] [--json]")
	if err != nil {
		return fail(env, "purge: %v", err)
	}
	return runPurgeDirect(ctx, env, "purge", target, *wait, *jsonOut)
}

func runPurgeDryRun(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "purge")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	save := fs.Bool("save", false, "merge the preview into the active purge book")
	timeout := fs.Duration("timeout", 5*time.Second, "quote refresh timeout for the initial shadow snapshot")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return failUnexpectedArgs(env, fs)
	}
	purgeProgress(env, *jsonOut, "purge dry-run: loading current positions")
	var pos rpc.PositionsResult
	if err := env.Conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &pos); err != nil {
		return fail(env, "purge dry-run: positions: %v", err)
	}
	now := time.Now()
	book := buildPurgeBookFromPositions(pos, now)
	purgeProgress(env, *jsonOut, "purge dry-run: refreshing quotes for %d leg(s)", len(book.Legs))
	if err := refreshPurgeBookQuotes(ctx, env.Conn, &book, *timeout); err != nil {
		book.Warnings = append(book.Warnings, "quote refresh failed: "+err.Error())
	}
	if *save {
		if len(book.Legs) == 0 {
			return fail(env, "purge dry-run: no open positions to save; purge book unchanged")
		}
		active, err := loadOrNewActivePurgeBook(now)
		if err != nil {
			return fail(env, "purge dry-run: load active purge book: %v", err)
		}
		if err := mergePurgeBook(&active, book, now); err != nil {
			return fail(env, "purge dry-run: %v", err)
		}
		purgeProgress(env, *jsonOut, "purge dry-run: refreshing active purge book quotes for %d leg(s)", len(active.Legs))
		if err := refreshPurgeBookQuotes(ctx, env.Conn, &active, *timeout); err != nil {
			active.Warnings = appendUniqueString(active.Warnings, "quote refresh failed: "+err.Error())
		}
		purgeProgress(env, *jsonOut, "purge dry-run: saving active purge book")
		path, err := savePurgeBook(&active)
		if err != nil {
			return fail(env, "purge dry-run: save active purge book: %v", err)
		}
		active.BookPath = path
		book = active
	}
	if *jsonOut {
		return printJSON(env, book)
	}
	renderPurgeBookText(env, env.Stdout, &book)
	return 0
}

func runPurgeStatus(ctx context.Context, env *Env, args []string, monitor bool) int {
	fs := flagSet(env, "purge")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	watch := fs.Bool("watch", false, "refresh repeatedly until Ctrl-C")
	rate := fs.Duration("rate", time.Second, "refresh interval for --watch")
	account := fs.String("account", "", "filter to one account")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 1 {
		if monitor {
			return fail(env, "purge monitor: usage is `canary purge monitor [PURGE_ID]`")
		}
		return fail(env, "purge status: usage is `canary purge status [PURGE_ID]`")
	}
	purgeID := activePurgeBookID
	if fs.NArg() == 1 {
		purgeID = fs.Arg(0)
	}
	render := func(out io.Writer) int {
		params := rpc.PurgeStatusParams{
			Account: strings.TrimSpace(*account),
			Limit:   50,
		}
		if purgeID != activePurgeBookID {
			params.PurgeID = purgeID
		}
		var res rpc.PurgeStatusResult
		if err := env.Conn.Call(ctx, rpc.MethodPurgeStatus, params, &res); err != nil {
			return fail(env, "purge %s: %v", statusVerb(monitor), err)
		}
		if *jsonOut {
			return printJSONTo(env, out, res)
		}
		renderPurgeStatusText(env, out, &res)
		return 0
	}
	if *watch {
		if *jsonOut {
			return fail(env, "purge monitor: --watch and --json are mutually exclusive")
		}
		return runWatch(ctx, env, *rate, "purge "+purgeID, render)
	}
	return render(env.Stdout)
}

func statusVerb(monitor bool) string {
	if monitor {
		return "monitor"
	}
	return "status"
}

func runPurgeRestore(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "purge")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	all := fs.Bool("all", false, "target every restore leg in the active purge book")
	scale := fs.Float64("scale", 1, "quantity scale to restore, 0.0-1.0")
	record := fs.Bool("record", false, "deprecated; restore quantities are adjusted only from daemon fill evidence")
	execute := fs.Bool("execute", false, "submit restore orders after a fresh daemon WhatIf preflight")
	wait := fs.Duration("wait", 2*time.Second, "wait briefly for immediate broker lifecycle callbacks after sending")
	timeout := fs.Duration("timeout", 5*time.Second, "per-leg quote/WhatIf preflight timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *scale <= 0 || *scale > 1 || math.IsNaN(*scale) || math.IsInf(*scale, 0) {
		return fail(env, "purge restore: --scale must be greater than 0 and at most 1")
	}
	if *record {
		return fail(env, "purge restore: --record is disabled; daemon ledger quantities change only after broker fills or safe reconciliation")
	}
	target, err := purgeTargetArg(*all, fs.Args(), "canary purge restore SYMBOL|'*' [--scale 0.5] [--execute] [--json] or canary purge restore --all [--scale 0.5] [--execute] [--json]")
	if err != nil {
		return fail(env, "purge restore: %v", err)
	}
	verb := "previewing daemon restore plan"
	method := rpc.MethodPurgeRestorePreview
	if *execute {
		verb = "executing daemon restore plan"
		method = rpc.MethodPurgeRestoreExecute
	}
	purgeProgress(env, *jsonOut, "purge restore %s: %s", target.label(), verb)
	params := rpc.PurgeRestoreParams{
		All:       target.All,
		Symbols:   target.onlySymbols(),
		Scale:     *scale,
		WaitMs:    int(wait.Milliseconds()),
		TimeoutMs: int(timeout.Milliseconds()),
		Origin:    env.Origin,
	}
	if len(params.Symbols) == 0 {
		params.All = true
	}
	var res rpc.PurgeRestoreResult
	if err := env.Conn.Call(ctx, method, params, &res); err != nil {
		return fail(env, "purge restore %s: %v", target.label(), err)
	}
	if *jsonOut {
		_ = printJSON(env, res)
		if purgeRestoreResultOK(res, *execute) {
			return 0
		}
		return 1
	}
	renderPurgeRestoreResultText(env, env.Stdout, &res)
	if purgeRestoreResultOK(res, *execute) {
		return 0
	}
	return 1
}

func purgeRestoreResultOK(res rpc.PurgeRestoreResult, execute bool) bool {
	if execute {
		return res.Status == "submitted" || res.Status == "flat"
	}
	return res.Status == "preview" || res.Status == "flat"
}

func runPurgeExecute(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "purge")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	all := fs.Bool("all", false, "target all open positions")
	wait := fs.Duration("wait", 2*time.Second, "wait briefly for immediate broker lifecycle callbacks after sending")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	target, err := purgeOptionalTargetArg(*all, fs.Args(), "canary purge execute [SYMBOL|'*'|--all] [--wait 2s] [--json]")
	if err != nil {
		return fail(env, "purge execute: %v", err)
	}
	return runPurgeDirect(ctx, env, "purge execute", target, *wait, *jsonOut)
}

func runPurgeDirect(ctx context.Context, env *Env, verb string, target purgeTarget, wait time.Duration, jsonOut bool) int {
	purgeProgress(env, jsonOut, "%s %s: refreshing current positions and submitting close orders", verb, target.label())
	params := rpc.PurgeExecuteParams{
		PurgeID: purgeBookID(time.Now()),
		All:     target.All,
		Symbols: target.onlySymbols(),
		WaitMs:  int(wait.Milliseconds()),
		Origin:  env.Origin,
	}
	if len(params.Symbols) == 0 {
		params.All = true
	}
	var result rpc.PurgeExecuteResult
	if err := env.Conn.Call(ctx, rpc.MethodPurgeExecute, params, &result); err != nil {
		return fail(env, "%s %s: %v", verb, target.label(), err)
	}
	if jsonOut {
		_ = printJSON(env, result)
		if purgeExecuteResultOK(result) {
			return 0
		}
		return 1
	}
	renderPurgeExecuteResultText(env, env.Stdout, &result)
	if purgeExecuteResultOK(result) {
		return 0
	}
	return 1
}

func purgeExecuteResultOK(result rpc.PurgeExecuteResult) bool {
	return result.Status == "submitted" || result.Status == "flat"
}

type purgeTarget struct {
	Symbol string
	All    bool
}

func purgeTargetArg(all bool, args []string, usage string) (purgeTarget, error) {
	if all {
		if len(args) != 0 {
			return purgeTarget{}, fmt.Errorf("--all cannot be combined with a ticker")
		}
		return purgeTarget{All: true}, nil
	}
	if len(args) != 1 {
		return purgeTarget{}, purgeTargetUsageError(args, usage)
	}
	raw := strings.TrimSpace(args[0])
	if raw == "*" {
		return purgeTarget{All: true}, nil
	}
	symbols := splitSymbols(raw)
	if len(symbols) != 1 {
		return purgeTarget{}, fmt.Errorf("ticker must be one underlying symbol")
	}
	return purgeTarget{Symbol: symbols[0]}, nil
}

func purgeOptionalTargetArg(all bool, args []string, usage string) (purgeTarget, error) {
	if all {
		if len(args) != 0 {
			return purgeTarget{}, fmt.Errorf("--all cannot be combined with a ticker")
		}
		return purgeTarget{All: true}, nil
	}
	if len(args) == 0 {
		return purgeTarget{All: true}, nil
	}
	return purgeTargetArg(false, args, usage)
}

func purgeTargetUsageError(args []string, usage string) error {
	if len(args) > 1 {
		return fmt.Errorf("expected one ticker (got %d args); if you typed an unquoted *, your shell expanded it before canary saw it; use `canary purge --all` or quote it as `canary purge '*'`; usage is `%s`", len(args), usage)
	}
	return fmt.Errorf("usage is `%s`", usage)
}

func (t purgeTarget) label() string {
	if t.All {
		return "*"
	}
	return t.Symbol
}

func (t purgeTarget) onlySymbols() []string {
	if t.All {
		return nil
	}
	return []string{t.Symbol}
}

func buildPurgeBookFromPositions(pos rpc.PositionsResult, now time.Time) purgeBook {
	if now.IsZero() {
		now = time.Now()
	}
	book := purgeBook{
		Kind:             purgeBookKind,
		SchemaVersion:    purgeBookSchemaVersion,
		PurgeID:          purgeBookID(now),
		Status:           purgeBookStatusDraft,
		CreatedAt:        now,
		UpdatedAt:        now,
		AccountID:        pos.AccountID,
		Source:           "positions.snapshot",
		SourceAsOf:       pos.AsOf,
		PositionCount:    len(pos.Stocks) + len(pos.Options),
		NotExecution:     "Dry-run review only; no broker order has been placed, modified, cancelled, or transmitted.",
		ExecutionCommand: "canary purge --all",
	}
	if pos.Portfolio != nil {
		book.BaseCurrency = pos.Portfolio.BaseCurrency
	}
	rows := make([]rpc.PositionView, 0, len(pos.Stocks)+len(pos.Options))
	rows = append(rows, pos.Stocks...)
	rows = append(rows, pos.Options...)
	slices.SortStableFunc(rows, func(a, b rpc.PositionView) int {
		if c := strings.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		if c := strings.Compare(a.SecType, b.SecType); c != 0 {
			return c
		}
		if c := strings.Compare(a.Expiry, b.Expiry); c != 0 {
			return c
		}
		if a.Strike < b.Strike {
			return -1
		}
		if a.Strike > b.Strike {
			return 1
		}
		return strings.Compare(a.Right, b.Right)
	})
	for i, p := range rows {
		if p.Quantity == 0 {
			continue
		}
		leg := purgeBookLegFromPosition(i+1, p)
		book.Legs = append(book.Legs, leg)
	}
	book.RestoreCommand = "canary purge restore SYMBOL"
	book.MonitorCommand = "canary purge monitor"
	recomputePurgeBookTotals(&book)
	if len(book.Legs) == 0 {
		book.Warnings = append(book.Warnings, "no open positions were available to purge")
	}
	return book
}

func newActivePurgeBook(now time.Time) purgeBook {
	if now.IsZero() {
		now = time.Now()
	}
	book := purgeBook{
		Kind:             purgeBookKind,
		SchemaVersion:    purgeBookSchemaVersion,
		PurgeID:          activePurgeBookID,
		Status:           purgeBookStatusDraft,
		CreatedAt:        now,
		UpdatedAt:        now,
		Source:           "active.purge_book",
		NotExecution:     "Saved restore review only; no broker order has been placed, modified, cancelled, or transmitted.",
		RestoreCommand:   "canary purge restore SYMBOL",
		MonitorCommand:   "canary purge monitor",
		ExecutionCommand: "canary purge --all",
	}
	return book
}

func loadOrNewActivePurgeBook(now time.Time) (purgeBook, error) {
	book, found, err := loadActivePurgeBook()
	if err != nil {
		return purgeBook{}, err
	}
	if !found {
		return newActivePurgeBook(now), nil
	}
	prepareActivePurgeBook(&book, now)
	return book, nil
}

func prepareActivePurgeBook(book *purgeBook, now time.Time) {
	if book == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	book.Kind = purgeBookKind
	book.SchemaVersion = purgeBookSchemaVersion
	book.PurgeID = activePurgeBookID
	book.Status = purgeBookStatusDraft
	if book.CreatedAt.IsZero() {
		book.CreatedAt = now
	}
	book.UpdatedAt = now
	if strings.TrimSpace(book.Source) == "" {
		book.Source = "active.purge_book"
	}
	book.PositionCount = len(book.Legs)
	book.NotExecution = "Saved restore review only; no broker order has been placed, modified, cancelled, or transmitted."
	book.RestoreCommand = "canary purge restore SYMBOL"
	book.MonitorCommand = "canary purge monitor"
	book.ExecutionCommand = "canary purge --all"
	sortPurgeBookLegs(book.Legs)
	recomputePurgeBookTotals(book)
}

func mergePurgeBook(dst *purgeBook, incoming purgeBook, now time.Time) error {
	if dst == nil {
		return fmt.Errorf("active purge book is nil")
	}
	prepareActivePurgeBook(dst, now)
	if dst.AccountID != "" && incoming.AccountID != "" && dst.AccountID != incoming.AccountID {
		return fmt.Errorf("active purge book account %s does not match positions account %s", dst.AccountID, incoming.AccountID)
	}
	if dst.BaseCurrency != "" && incoming.BaseCurrency != "" && dst.BaseCurrency != incoming.BaseCurrency {
		return fmt.Errorf("active purge book base currency %s does not match positions base currency %s", dst.BaseCurrency, incoming.BaseCurrency)
	}
	if dst.AccountID == "" {
		dst.AccountID = incoming.AccountID
	}
	if dst.BaseCurrency == "" {
		dst.BaseCurrency = incoming.BaseCurrency
	}
	if incoming.SourceAsOf.After(dst.SourceAsOf) {
		dst.SourceAsOf = incoming.SourceAsOf
	}

	byInstrument := make(map[string]int, len(dst.Legs))
	for i, leg := range dst.Legs {
		byInstrument[purgeLegInstrumentKey(leg)] = i
	}
	for _, leg := range incoming.Legs {
		if leg.Quantity <= 0 {
			continue
		}
		key := purgeLegInstrumentKey(leg)
		if i, ok := byInstrument[key]; ok {
			if err := mergePurgeLeg(&dst.Legs[i], leg); err != nil {
				return err
			}
			continue
		}
		resetPurgeLegQuote(&leg)
		dst.Legs = append(dst.Legs, leg)
		byInstrument[key] = len(dst.Legs) - 1
	}
	prepareActivePurgeBook(dst, now)
	return nil
}

func mergePurgeLeg(dst *purgeBookLeg, src purgeBookLeg) error {
	if dst == nil {
		return fmt.Errorf("purge leg is nil")
	}
	if dst.OriginalSide != src.OriginalSide || dst.PurgeAction != src.PurgeAction || dst.RestoreAction != src.RestoreAction {
		return fmt.Errorf("active purge book already tracks %s as %s; restore or record that leg before adding the opposite side", purgeLegLabel(*dst), dst.OriginalSide)
	}
	oldQty := dst.Quantity
	newQty := src.Quantity
	if newQty <= 0 {
		return nil
	}
	multiplier := max(max(dst.Multiplier, src.Multiplier), 1)
	if dst.Multiplier != src.Multiplier {
		dst.Warnings = appendUniqueString(dst.Warnings, "refreshed position reported a different multiplier")
	}
	if math.Abs(oldQty-newQty) > 1e-9 {
		dst.Warnings = appendUniqueString(dst.Warnings, "quantity reconciled from current positions")
	}
	if dst.ExitPrice <= 0 && src.ExitPrice > 0 {
		dst.ExitPrice = src.ExitPrice
		dst.ExitPriceSource = src.ExitPriceSource
	}
	if dst.ExitPriceSource == "" {
		dst.ExitPriceSource = src.ExitPriceSource
	}
	dst.Symbol = src.Symbol
	dst.SecType = src.SecType
	dst.Contract = src.Contract
	dst.Currency = src.Currency
	dst.OriginalQuantity = src.OriginalQuantity
	dst.Quantity = newQty
	dst.Multiplier = multiplier
	if dst.ExitPrice > 0 {
		dst.ExitValue = dst.ExitPrice * dst.Quantity * float64(multiplier)
	} else {
		dst.ExitValue = 0
	}
	dst.OriginalMarketValueCCY = src.OriginalMarketValueCCY
	for _, warning := range src.Warnings {
		dst.Warnings = appendUniqueString(dst.Warnings, warning)
	}
	if oldQty > 0 && math.Abs(oldQty-newQty) > 1e-9 {
		ratio := newQty / oldQty
		scaleFloatPtr(dst.CurrentRestoreValue, ratio)
		scaleFloatPtr(dst.ShadowSaved, ratio)
		scaleFloatPtr(dst.LowRestoreValue, ratio)
		scaleFloatPtr(dst.HighRestoreValue, ratio)
		if dst.ShadowSaved != nil && dst.ExitValue > 0 {
			pct := *dst.ShadowSaved / dst.ExitValue * 100
			dst.ShadowSavedPctExit = &pct
		}
	}
	if src.Status == purgeLegStatusFractional {
		dst.Status = purgeLegStatusFractional
	} else if dst.ExitPrice <= 0 {
		dst.Status = purgeLegStatusUnpriced
	} else if dst.CurrentPrice != nil {
		dst.Status = purgeLegStatusPriced
	} else {
		dst.Status = purgeLegStatusDraft
	}
	return nil
}

func recordPurgeRestorePlan(book *purgeBook, plan purgeRestorePlan, now time.Time) error {
	if book == nil {
		return fmt.Errorf("active purge book is nil")
	}
	for _, restoreLeg := range plan.Legs {
		if restoreLeg.Quantity <= 0 {
			continue
		}
		idx := slices.IndexFunc(book.Legs, func(leg purgeBookLeg) bool {
			return leg.LegID == restoreLeg.LegID
		})
		if idx < 0 {
			return fmt.Errorf("restore leg %s is not in the active purge book", restoreLeg.LegID)
		}
		reducePurgeLeg(&book.Legs[idx], restoreLeg.Quantity)
	}
	book.Legs = slices.DeleteFunc(book.Legs, func(leg purgeBookLeg) bool {
		return leg.Quantity <= 1e-9
	})
	prepareActivePurgeBook(book, now)
	return nil
}

func reducePurgeLeg(leg *purgeBookLeg, qty float64) {
	if leg == nil || qty <= 0 || leg.Quantity <= 0 {
		return
	}
	oldQty := leg.Quantity
	if qty >= oldQty-1e-9 {
		leg.Quantity = 0
		leg.OriginalQuantity = 0
		leg.ExitValue = 0
		leg.OriginalMarketValueCCY = 0
		zeroPurgeLegValues(leg)
		return
	}
	remaining := oldQty - qty
	ratio := remaining / oldQty
	leg.Quantity = remaining
	if leg.OriginalQuantity < 0 {
		leg.OriginalQuantity = -remaining
	} else {
		leg.OriginalQuantity = remaining
	}
	leg.ExitValue *= ratio
	leg.OriginalMarketValueCCY *= ratio
	scaleFloatPtr(leg.CurrentRestoreValue, ratio)
	scaleFloatPtr(leg.ShadowSaved, ratio)
	scaleFloatPtr(leg.LowRestoreValue, ratio)
	scaleFloatPtr(leg.HighRestoreValue, ratio)
	if leg.ExitValue > 0 {
		leg.ExitPrice = leg.ExitValue / (remaining * float64(max(leg.Multiplier, 1)))
	}
	if leg.ShadowSaved != nil && leg.ExitValue > 0 {
		pct := *leg.ShadowSaved / leg.ExitValue * 100
		leg.ShadowSavedPctExit = &pct
	}
}

func resetPurgeLegQuote(leg *purgeBookLeg) {
	if leg == nil {
		return
	}
	leg.CurrentPrice = nil
	leg.CurrentPriceSource = ""
	leg.CurrentRestoreValue = nil
	leg.ShadowSaved = nil
	leg.ShadowSavedPctExit = nil
	leg.LowRestoreValue = nil
	leg.HighRestoreValue = nil
	leg.LowPrice = nil
	leg.HighPrice = nil
	leg.LastQuoteAt = time.Time{}
	leg.DataType = ""
	leg.QuoteQuality = ""
}

func zeroPurgeLegValues(leg *purgeBookLeg) {
	if leg == nil {
		return
	}
	if leg.CurrentRestoreValue != nil {
		*leg.CurrentRestoreValue = 0
	}
	if leg.ShadowSaved != nil {
		*leg.ShadowSaved = 0
	}
	if leg.ShadowSavedPctExit != nil {
		*leg.ShadowSavedPctExit = 0
	}
	if leg.LowRestoreValue != nil {
		*leg.LowRestoreValue = 0
	}
	if leg.HighRestoreValue != nil {
		*leg.HighRestoreValue = 0
	}
}

func scaleFloatPtr(v *float64, ratio float64) {
	if v != nil {
		*v *= ratio
	}
}

func purgeBookLegFromPosition(idx int, p rpc.PositionView) purgeBookLeg {
	side := purgeOriginalSideLong
	purgeAction := rpc.OrderActionSell
	restoreAction := rpc.OrderActionBuy
	if p.Quantity < 0 {
		side = purgeOriginalSideShort
		purgeAction = rpc.OrderActionBuy
		restoreAction = rpc.OrderActionSell
	}
	multiplier := max(p.Multiplier, 1)
	qty := math.Abs(p.Quantity)
	exit, source, warnings := purgeExitPrice(p, purgeAction)
	exitValue := exit * qty * float64(multiplier)
	status := purgeLegStatusDraft
	if exit <= 0 {
		status = purgeLegStatusUnpriced
	}
	if math.Trunc(qty) != qty {
		status = purgeLegStatusFractional
		warnings = append(warnings, "fractional quantity cannot use the current integer order path")
	}
	return purgeBookLeg{
		LegID:                  purgeLegID(idx, p),
		Symbol:                 strings.ToUpper(strings.TrimSpace(p.Symbol)),
		SecType:                p.SecType,
		Contract:               purgeContractFromPosition(p),
		OriginalSide:           side,
		OriginalQuantity:       p.Quantity,
		PurgeAction:            purgeAction,
		RestoreAction:          restoreAction,
		Quantity:               qty,
		Multiplier:             multiplier,
		Currency:               strings.ToUpper(strings.TrimSpace(p.Currency)),
		ExitPrice:              exit,
		ExitPriceSource:        source,
		ExitValue:              exitValue,
		Status:                 status,
		Warnings:               warnings,
		Estimated:              true,
		OriginalMarketValueCCY: p.MarketValue,
	}
}

func purgeContractFromPosition(p rpc.PositionView) rpc.ContractParams {
	secType := "STK"
	if strings.EqualFold(p.SecType, rpc.SecTypeOption) {
		secType = "OPT"
	}
	c := rpc.ContractParams{
		ConID:        p.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(p.Symbol)),
		SecType:      secType,
		Exchange:     strings.TrimSpace(p.Exchange),
		Currency:     strings.ToUpper(strings.TrimSpace(p.Currency)),
		LocalSymbol:  strings.TrimSpace(p.LocalSymbol),
		TradingClass: strings.TrimSpace(p.TradingClass),
		Expiry:       strings.TrimSpace(p.Expiry),
		Strike:       p.Strike,
		Right:        strings.ToUpper(strings.TrimSpace(p.Right)),
		Multiplier:   max(p.Multiplier, 1),
	}
	if c.Exchange == "" {
		c.Exchange = "SMART"
	}
	if c.Currency == "" {
		c.Currency = "USD"
	}
	normalisePurgePositionStockRoute(&c)
	return c
}

func normalisePurgePositionStockRoute(c *rpc.ContractParams) {
	if c == nil {
		return
	}
	switch strings.ToUpper(strings.TrimSpace(c.SecType)) {
	case "STK", "ETF":
	default:
		return
	}
	exchange := strings.ToUpper(strings.TrimSpace(c.Exchange))
	primary := strings.ToUpper(strings.TrimSpace(c.PrimaryExch))
	if primary == "" && exchange != "" && exchange != "SMART" {
		c.PrimaryExch = exchange
	}
	if exchange == "" || exchange != "SMART" {
		c.Exchange = "SMART"
	}
}

func purgeExitPrice(p rpc.PositionView, action string) (float64, string, []string) {
	warnings := []string{}
	if strings.EqualFold(p.SecType, rpc.SecTypeOption) {
		switch action {
		case rpc.OrderActionSell:
			if validPricePtr(p.OptionBid) {
				return *p.OptionBid, "option_bid", warnings
			}
			warnings = append(warnings, "missing option bid; using mark estimate")
		case rpc.OrderActionBuy:
			if validPricePtr(p.OptionAsk) {
				return *p.OptionAsk, "option_ask", warnings
			}
			warnings = append(warnings, "missing option ask; using mark estimate")
		}
	}
	if validPricePtr(p.QuotePrice) {
		return *p.QuotePrice, "quote_price", warnings
	}
	if p.Mark > 0 {
		return p.Mark, "position_mark", warnings
	}
	if p.ValuationMark > 0 {
		return p.ValuationMark, "valuation_mark", warnings
	}
	warnings = append(warnings, "no usable exit price estimate")
	return 0, "unavailable", warnings
}

func refreshPurgeBookQuotes(ctx context.Context, conn purgeRPCConn, book *purgeBook, timeout time.Duration) error {
	if book == nil || conn == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	var firstErr error
	for i := range book.Legs {
		leg := &book.Legs[i]
		var q rpc.Quote
		params := rpc.QuoteSnapshotParams{
			Contract:  leg.Contract,
			TimeoutMs: int(timeout.Milliseconds()),
		}
		if leg.Contract.SecType == "STK" {
			params.IncludeLiquidity = true
		}
		if err := conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
			leg.Status = purgeLegStatusStale
			leg.Warnings = appendUniqueString(leg.Warnings, "quote refresh failed: "+err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		applyQuoteToPurgeLeg(leg, q)
	}
	book.UpdatedAt = time.Now()
	recomputePurgeBookTotals(book)
	return firstErr
}

func applyQuoteToPurgeLeg(leg *purgeBookLeg, q rpc.Quote) {
	price, source := purgeRestorePrice(q, leg.RestoreAction)
	if price == nil || *price <= 0 {
		leg.Status = purgeLegStatusUnpriced
		leg.CurrentPrice = nil
		leg.CurrentRestoreValue = nil
		leg.ShadowSaved = nil
		leg.ShadowSavedPctExit = nil
		leg.Warnings = appendUniqueString(leg.Warnings, "no usable restore price from quote")
		return
	}
	v := *price
	restoreValue := v * leg.Quantity * float64(max(leg.Multiplier, 1))
	signedQty := leg.OriginalQuantity
	shadowSaved := (leg.ExitPrice - v) * signedQty * float64(max(leg.Multiplier, 1))
	var pct *float64
	if leg.ExitValue > 0 {
		p := shadowSaved / leg.ExitValue * 100
		pct = &p
	}
	leg.CurrentPrice = &v
	leg.CurrentPriceSource = source
	leg.CurrentRestoreValue = &restoreValue
	leg.ShadowSaved = &shadowSaved
	leg.ShadowSavedPctExit = pct
	if leg.LowRestoreValue == nil || restoreValue < *leg.LowRestoreValue {
		leg.LowRestoreValue = &restoreValue
		leg.LowPrice = &v
	}
	if leg.HighRestoreValue == nil || restoreValue > *leg.HighRestoreValue {
		leg.HighRestoreValue = &restoreValue
		leg.HighPrice = &v
	}
	leg.LastQuoteAt = q.AsOf
	if leg.LastQuoteAt.IsZero() {
		leg.LastQuoteAt = q.PriceAt
	}
	leg.DataType = q.DataType
	leg.QuoteQuality = q.QuoteQuality
	if leg.Status != purgeLegStatusFractional {
		leg.Status = purgeLegStatusPriced
	}
}

func purgeRestorePrice(q rpc.Quote, action string) (*float64, string) {
	switch action {
	case rpc.OrderActionBuy:
		if validPricePtr(q.Ask) {
			return q.Ask, "ask"
		}
	case rpc.OrderActionSell:
		if validPricePtr(q.Bid) {
			return q.Bid, "bid"
		}
	}
	if validPricePtr(q.QuotePrice) {
		return q.QuotePrice, "quote_price"
	}
	if validPricePtr(q.Price) {
		return q.Price, "price"
	}
	if validPricePtr(q.Mark) {
		return q.Mark, "mark"
	}
	if validPricePtr(q.Last) {
		return q.Last, "last"
	}
	if validPricePtr(q.PrevClose) {
		return q.PrevClose, "prev_close"
	}
	return nil, "unavailable"
}

func recomputePurgeBookTotals(book *purgeBook) {
	if book == nil {
		return
	}
	var totals purgeBookTotals
	var restoreValue, saved float64
	var priced bool
	for _, leg := range book.Legs {
		totals.ExitValue += leg.ExitValue
		if leg.OriginalQuantity >= 0 {
			totals.LongLegs++
		} else {
			totals.ShortLegs++
		}
		if leg.CurrentRestoreValue != nil && leg.ShadowSaved != nil {
			totals.PricedLegs++
			restoreValue += *leg.CurrentRestoreValue
			saved += *leg.ShadowSaved
			priced = true
		} else {
			totals.UnpricedLegs++
		}
	}
	if priced {
		totals.CurrentRestoreValue = &restoreValue
		totals.ShadowSaved = &saved
		if totals.ExitValue > 0 {
			pct := saved / totals.ExitValue * 100
			totals.ShadowSavedPctExit = &pct
		}
	}
	book.Totals = totals
}

func buildPurgeRestorePlan(book purgeBook, scale float64, only []string, now time.Time) purgeRestorePlan {
	if now.IsZero() {
		now = time.Now()
	}
	onlySet := map[string]bool{}
	for _, sym := range only {
		onlySet[strings.ToUpper(strings.TrimSpace(sym))] = true
	}
	plan := purgeRestorePlan{
		Kind:             purgeRestoreKind,
		SchemaVersion:    purgeRestoreSchemaVersion,
		PurgeID:          book.PurgeID,
		AsOf:             now,
		AccountID:        book.AccountID,
		BaseCurrency:     book.BaseCurrency,
		Scale:            scale,
		Only:             only,
		NotExecution:     "Restore review only; no broker order has been placed, modified, cancelled, previewed, or transmitted.",
		PreviewAvailable: false,
	}
	for _, leg := range book.Legs {
		if len(onlySet) > 0 && !onlySet[strings.ToUpper(leg.Symbol)] {
			continue
		}
		qty := leg.Quantity * scale
		if qty <= 0 {
			continue
		}
		rleg := purgeRestoreLeg{
			LegID:          leg.LegID,
			Symbol:         leg.Symbol,
			SecType:        leg.SecType,
			Contract:       leg.Contract,
			Action:         leg.RestoreAction,
			Quantity:       qty,
			MaxQuantity:    leg.Quantity,
			ReferencePrice: leg.CurrentPrice,
			Status:         leg.Status,
			Warnings:       append([]string(nil), leg.Warnings...),
		}
		plan.Totals.SelectedLegs++
		if leg.CurrentPrice != nil {
			value := *leg.CurrentPrice * qty * float64(max(leg.Multiplier, 1))
			rleg.EstimatedValue = &value
			if plan.Totals.EstimatedValue == nil {
				v := 0.0
				plan.Totals.EstimatedValue = &v
			}
			*plan.Totals.EstimatedValue += value
		}
		if leg.ShadowSaved != nil {
			used := *leg.ShadowSaved * scale
			rleg.ShadowSavedAfterLeg = &used
			if plan.Totals.ShadowSavedUsed == nil {
				v := 0.0
				plan.Totals.ShadowSavedUsed = &v
			}
			*plan.Totals.ShadowSavedUsed += used
		}
		if leg.Status == purgeLegStatusFractional {
			rleg.Warnings = appendUniqueString(rleg.Warnings, "review only: fractional restore quantity is not supported by the current order preview path")
		}
		if strings.EqualFold(leg.Contract.SecType, "OPT") {
			rleg.Warnings = appendUniqueString(rleg.Warnings, "review only: option restore preview is not enabled yet")
		}
		plan.Legs = append(plan.Legs, rleg)
	}
	if len(plan.Legs) == 0 {
		plan.Warnings = append(plan.Warnings, "no restore legs selected")
	}
	return plan
}

func sortPurgeBookLegs(legs []purgeBookLeg) {
	slices.SortStableFunc(legs, func(a, b purgeBookLeg) int {
		if c := strings.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		if c := strings.Compare(a.Contract.SecType, b.Contract.SecType); c != 0 {
			return c
		}
		if c := strings.Compare(a.Contract.Expiry, b.Contract.Expiry); c != 0 {
			return c
		}
		if a.Contract.Strike < b.Contract.Strike {
			return -1
		}
		if a.Contract.Strike > b.Contract.Strike {
			return 1
		}
		if c := strings.Compare(a.Contract.Right, b.Contract.Right); c != 0 {
			return c
		}
		return strings.Compare(a.LegID, b.LegID)
	})
}

func renderPurgeStatusText(env *Env, out io.Writer, res *rpc.PurgeStatusResult) {
	fmt.Fprintln(out)
	if res == nil {
		fmt.Fprintln(out, "No purge status")
		return
	}
	fmt.Fprintf(out, "Canary Purge Status  %s\n", env.statusBadge(statusConcern{Text: purgeStatusBadgeText(res.Status), Level: purgeStatusConcernLevel(res.Status)}))
	if res.PurgeID != "" {
		statusRow(env, out, "Purge", res.PurgeID)
	} else {
		statusRow(env, out, "Purge", "all tracked purge orders")
	}
	if res.Account != "" {
		statusRow(env, out, "Account", res.Account)
	}
	statusRow(env, out, "Orders", fmt.Sprintf("%d total / %d open / %d filled / %d cancelled / %d attention",
		res.TotalOrders, res.OpenOrders, res.FilledOrders, res.CancelledOrders, res.AttentionOrders))
	statusRow(env, out, "Book", fmt.Sprintf("%d active / %d restored / %s remaining",
		res.Totals.ActiveRows, res.Totals.RestoredRows, formatPurgeQuantity(res.Totals.RemainingQuantity)))
	if res.Totals.PurgeValue != 0 {
		statusRow(env, out, "Purge value", formatMoneyBare(res.Totals.PurgeValue))
	}
	if res.Totals.RestoreValue != 0 {
		statusRow(env, out, "Restore value", formatMoneyBare(res.Totals.RestoreValue))
	}
	if res.Totals.ShadowPnL != 0 {
		statusRow(env, out, "Shadow P/L", env.colorBySign(res.Totals.ShadowPnL, formatMoneyBare(res.Totals.ShadowPnL), signPnL))
	}
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	if len(res.Rows) > 0 {
		fmt.Fprintln(out)
		cols := []positionTableColumn{
			{header: "LEG", align: positionAlignLeft},
			{header: "SIDE", align: positionAlignLeft},
			{header: "PURGED", align: positionAlignRight},
			{header: "RESTORED", align: positionAlignRight},
			{header: "REMAIN", align: positionAlignRight},
			{header: "P/L", align: positionAlignRight},
			{header: "STATE", align: positionAlignLeft},
		}
		rows := make([][]string, 0, len(res.Rows))
		for _, row := range res.Rows {
			rows = append(rows, []string{
				purgeLedgerRowLabel(row),
				row.OriginalSide,
				formatPurgeQuantity(row.PurgedQuantity),
				formatPurgeQuantity(row.RestoredQuantity),
				formatPurgeQuantity(row.RemainingQuantity),
				env.colorBySign(row.ShadowPnL, formatMoneyBare(row.ShadowPnL), signPnL),
				row.Status,
			})
		}
		renderPositionTable(env, out, cols, rows)
	}
	if len(res.Orders) > 0 {
		fmt.Fprintln(out)
		cols := []positionTableColumn{
			{header: "PURGE", align: positionAlignLeft},
			{header: "ORDER", align: positionAlignLeft},
			{header: "LEG", align: positionAlignLeft},
			{header: "ACTION", align: positionAlignLeft},
			{header: "QTY", align: positionAlignRight},
			{header: "FILLED", align: positionAlignRight},
			{header: "STATE", align: positionAlignLeft},
			{header: "UPDATED", align: positionAlignLeft},
		}
		rows := make([][]string, 0, len(res.Orders))
		for _, order := range res.Orders {
			state := order.LifecycleStatus
			if order.SendState != "" && order.SendState != "send_attempted" {
				state += "/" + order.SendState
			}
			rows = append(rows, []string{
				nonEmpty(order.PurgeID, "unknown"),
				purgeOrderIDLabel(order),
				purgeOrderLegLabel(order),
				order.Action,
				formatPurgeQuantity(order.Quantity),
				formatPurgeQuantity(order.Filled),
				state,
				formatOrderTime(order.UpdatedAt),
			})
		}
		renderPositionTable(env, out, cols, rows)
	}
	fmt.Fprintln(out)
}

func purgeLedgerRowLabel(row rpc.PurgeLedgerRow) string {
	return purgeLegLabel(purgeBookLeg{Symbol: row.Symbol, Contract: row.Contract})
}

func purgeOrderIDLabel(order rpc.OrderView) string {
	if strings.TrimSpace(order.OrderRef) != "" {
		return order.OrderRef
	}
	if order.ReservedOrderID != 0 {
		return strconv.Itoa(order.ReservedOrderID)
	}
	return "unknown"
}

func purgeStatusConcernLevel(status string) statusConcernLevel {
	switch status {
	case "attention":
		return statusConcernWarn
	case "open":
		return statusConcernNotice
	default:
		return statusConcernNone
	}
}

func purgeStatusBadgeText(status string) string {
	switch status {
	case "no_orders":
		return "NO ORDERS"
	default:
		return strings.ToUpper(strings.ReplaceAll(status, "_", " "))
	}
}

func purgeOrderLegLabel(order rpc.OrderView) string {
	return purgeLegLabel(purgeBookLeg{
		Symbol: order.Symbol,
		Contract: rpc.ContractParams{
			Symbol:       order.Symbol,
			SecType:      order.SecType,
			Expiry:       order.Expiry,
			Strike:       order.Strike,
			Right:        order.Right,
			LocalSymbol:  order.LocalSymbol,
			TradingClass: order.TradingClass,
		},
	})
}

func renderPurgeBookText(env *Env, out io.Writer, book *purgeBook) {
	fmt.Fprintln(out)
	if book == nil {
		fmt.Fprintln(out, "No purge book")
		return
	}
	fmt.Fprintf(out, "Canary Purge Review  %s\n", env.statusBadge(statusConcern{Text: "REVIEW", Level: statusConcernNotice}))
	statusRow(env, out, "Review", book.PurgeID)
	if book.AccountID != "" {
		statusRow(env, out, "Account", book.AccountID)
	}
	statusRow(env, out, "Exit value", formatMoneyCcy(book.Totals.ExitValue, book.BaseCurrency))
	if book.Totals.CurrentRestoreValue != nil {
		statusRow(env, out, "Restore value", formatMoneyCcy(*book.Totals.CurrentRestoreValue, book.BaseCurrency))
	}
	if book.Totals.ShadowSaved != nil {
		statusRow(env, out, "Shadow saved", env.colorBySign(*book.Totals.ShadowSaved, formatMoneyCcy(*book.Totals.ShadowSaved, book.BaseCurrency), signPnL))
	}
	if book.Totals.ShadowSavedPctExit != nil {
		statusRow(env, out, "Saved % exit", env.formatChangePct(book.Totals.ShadowSavedPctExit, 8))
	}
	statusRow(env, out, "Legs", fmt.Sprintf("%d priced / %d total", book.Totals.PricedLegs, len(book.Legs)))
	if book.BookPath != "" {
		statusRow(env, out, "Book file", book.BookPath)
	}
	if book.NotExecution != "" {
		statusRow(env, out, "Boundary", book.NotExecution)
	}
	if len(book.Legs) > 0 {
		fmt.Fprintln(out)
		renderPurgeLegTable(env, out, book)
	}
	if len(book.Warnings) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Warnings:")
		for _, w := range book.Warnings {
			fmt.Fprintf(out, "  - %s\n", w)
		}
	}
	fmt.Fprintln(out)
}

func renderPurgeLegTable(env *Env, out io.Writer, book *purgeBook) {
	cols := []positionTableColumn{
		{header: "LEG", align: positionAlignLeft},
		{header: "SIDE", align: positionAlignLeft},
		{header: "QTY", align: positionAlignRight},
		{header: "EXIT", align: positionAlignRight},
		{header: "NOW", align: positionAlignRight},
		{header: "SAVED", align: positionAlignRight},
		{header: "LOW", align: positionAlignRight},
		{header: "STATE", align: positionAlignLeft},
	}
	rows := make([][]string, 0, len(book.Legs))
	for _, leg := range book.Legs {
		saved := "—"
		if leg.ShadowSaved != nil {
			saved = env.colorBySign(*leg.ShadowSaved, formatMoneyBare(*leg.ShadowSaved), signPnL)
		}
		low := "—"
		if leg.LowPrice != nil {
			low = fmt.Sprintf("%.2f", *leg.LowPrice)
		}
		rows = append(rows, []string{
			purgeLegLabel(leg),
			leg.OriginalSide,
			formatPurgeQuantity(leg.Quantity),
			formatPositionPrice(leg.ExitPrice),
			formatPositionPricePtr(leg.CurrentPrice),
			saved,
			low,
			purgeLegState(leg),
		})
	}
	renderPositionTable(env, out, cols, rows)
}

func renderPurgeRestoreResultText(env *Env, out io.Writer, res *rpc.PurgeRestoreResult) {
	fmt.Fprintln(out)
	if res == nil {
		fmt.Fprintln(out, "No restore result")
		return
	}
	badge := statusConcern{Text: strings.ToUpper(res.Status), Level: statusConcernNotice}
	switch res.Status {
	case "preview", "submitted", "flat":
		badge.Level = statusConcernNone
	case "blocked", "partial":
		badge.Level = statusConcernWarn
	default:
		badge.Level = statusConcernBad
	}
	fmt.Fprintf(out, "Canary Purge Restore  %s\n", env.statusBadge(badge))
	statusRow(env, out, "Purge", nonEmpty(res.PurgeID, "active"))
	if res.Account != "" {
		statusRow(env, out, "Account", res.Account)
	}
	if res.Endpoint != "" {
		statusRow(env, out, "Endpoint", fmt.Sprintf("%s client %d", res.Endpoint, res.ClientID))
	}
	statusRow(env, out, "Scale", fmt.Sprintf("%.0f%%", res.Scale*100))
	statusRow(env, out, "Selected", fmt.Sprintf("%d leg(s)", res.SelectedLegs))
	if res.SubmittedLegs > 0 {
		statusRow(env, out, "Submitted", fmt.Sprintf("%d order(s)", res.SubmittedLegs))
	}
	if res.EstimatedValue != 0 {
		statusRow(env, out, "Restore value", formatMoneyBare(res.EstimatedValue))
	}
	if res.ShadowPnL != 0 {
		statusRow(env, out, "Shadow P/L", env.colorBySign(res.ShadowPnL, formatMoneyBare(res.ShadowPnL), signPnL))
	}
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	if len(res.Legs) > 0 {
		fmt.Fprintln(out)
		cols := []positionTableColumn{
			{header: "LEG", align: positionAlignLeft},
			{header: "ACTION", align: positionAlignLeft},
			{header: "QTY", align: positionAlignRight},
			{header: "LIMIT", align: positionAlignRight},
			{header: "VALUE", align: positionAlignRight},
			{header: "WHATIF", align: positionAlignLeft},
			{header: "STATE", align: positionAlignLeft},
		}
		rows := make([][]string, 0, len(res.Legs))
		for _, leg := range res.Legs {
			rows = append(rows, []string{
				purgeRPCRestoreLegLabel(leg),
				leg.Action,
				formatPurgeQuantity(float64(leg.Quantity)),
				formatPositionPrice(leg.LimitPrice),
				formatMoneyBare(leg.EstimatedValue),
				nonEmpty(leg.WhatIf.Status, "—"),
				leg.Status,
			})
		}
		renderPositionTable(env, out, cols, rows)
	}
	if len(res.Orders) > 0 {
		fmt.Fprintln(out)
		cols := []positionTableColumn{
			{header: "LEG", align: positionAlignLeft},
			{header: "ACTION", align: positionAlignLeft},
			{header: "QTY", align: positionAlignRight},
			{header: "LIMIT", align: positionAlignRight},
			{header: "ORDER", align: positionAlignLeft},
			{header: "STATE", align: positionAlignLeft},
		}
		rows := make([][]string, 0, len(res.Orders))
		for _, order := range res.Orders {
			state := order.LifecycleStatus
			if state == "" {
				state = order.SendState
			}
			rows = append(rows, []string{
				purgeExecuteOrderLabel(order),
				order.Action,
				formatPurgeQuantity(float64(order.Quantity)),
				formatPositionPrice(order.LimitPrice),
				order.OrderRef,
				state,
			})
		}
		renderPositionTable(env, out, cols, rows)
	}
	if len(res.Skipped) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Skipped:")
		for _, leg := range res.Skipped {
			fmt.Fprintf(out, "  - %s: %s\n", purgeExecuteSkippedLabel(leg), leg.Reason)
		}
	}
	if len(res.Blockers) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Blockers:")
		for _, blocker := range res.Blockers {
			fmt.Fprintf(out, "  - %s: %s\n", blocker.Code, blocker.Message)
		}
	}
	if len(res.Warnings) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Warnings:")
		for _, w := range res.Warnings {
			fmt.Fprintf(out, "  - %s\n", w)
		}
	}
	fmt.Fprintln(out)
}

func purgeRPCRestoreLegLabel(leg rpc.PurgeRestoreLeg) string {
	return purgeLegLabel(purgeBookLeg{Symbol: leg.Symbol, Contract: leg.Contract})
}

func renderPurgeExecuteResultText(env *Env, out io.Writer, result *rpc.PurgeExecuteResult) {
	fmt.Fprintln(out)
	if result == nil {
		fmt.Fprintln(out, "No purge execution result")
		return
	}
	badge := statusConcern{Text: strings.ToUpper(result.Status), Level: statusConcernNone}
	switch result.Status {
	case "submitted", "flat":
		badge.Level = statusConcernNone
	case "partial", "blocked":
		badge.Level = statusConcernWarn
	default:
		badge.Level = statusConcernBad
	}
	fmt.Fprintf(out, "Canary Purge  %s\n", env.statusBadge(badge))
	statusRow(env, out, "Purge", result.PurgeID)
	if result.Account != "" {
		statusRow(env, out, "Account", result.Account)
	}
	if result.Endpoint != "" {
		statusRow(env, out, "Endpoint", result.Endpoint)
	}
	statusRow(env, out, "Selected", fmt.Sprintf("%d leg(s)", result.SelectedLegs))
	statusRow(env, out, "Submitted", fmt.Sprintf("%d order(s)", result.SubmittedLegs))
	statusRow(env, out, "Skipped", fmt.Sprintf("%d leg(s)", result.SkippedLegs))
	if result.ErrorLegs > 0 {
		statusRow(env, out, "Errors", fmt.Sprintf("%d leg(s)", result.ErrorLegs))
	}
	if result.Message != "" {
		statusRow(env, out, "Message", result.Message)
	}
	if result.MonitorCommand != "" {
		statusRow(env, out, "Monitor", result.MonitorCommand)
	}
	if result.RestoreReviewCommand != "" {
		statusRow(env, out, "Restore review", result.RestoreReviewCommand)
	}
	if len(result.Orders) > 0 {
		fmt.Fprintln(out)
		cols := []positionTableColumn{
			{header: "LEG", align: positionAlignLeft},
			{header: "ACTION", align: positionAlignLeft},
			{header: "QTY", align: positionAlignRight},
			{header: "LIMIT", align: positionAlignRight},
			{header: "ORDER", align: positionAlignLeft},
			{header: "STATE", align: positionAlignLeft},
		}
		rows := make([][]string, 0, len(result.Orders))
		for _, order := range result.Orders {
			state := order.LifecycleStatus
			if state == "" {
				state = order.SendState
			}
			rows = append(rows, []string{
				purgeExecuteOrderLabel(order),
				order.Action,
				formatPurgeQuantity(float64(order.Quantity)),
				formatPositionPrice(order.LimitPrice),
				order.OrderRef,
				state,
			})
		}
		renderPositionTable(env, out, cols, rows)
	}
	if len(result.Skipped) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Skipped:")
		for _, leg := range result.Skipped {
			fmt.Fprintf(out, "  - %s: %s\n", purgeExecuteSkippedLabel(leg), leg.Reason)
		}
	}
	if len(result.Blockers) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Blockers:")
		for _, blocker := range result.Blockers {
			fmt.Fprintf(out, "  - %s: %s\n", blocker.Code, blocker.Message)
		}
	}
	if len(result.Warnings) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Warnings:")
		for _, w := range result.Warnings {
			fmt.Fprintf(out, "  - %s\n", w)
		}
	}
	fmt.Fprintln(out)
}

func purgeExecuteOrderLabel(order rpc.PurgeExecuteOrder) string {
	return purgeLegLabel(purgeBookLeg{Symbol: order.Symbol, Contract: order.Contract})
}

func purgeExecuteSkippedLabel(leg rpc.PurgeExecuteSkippedLeg) string {
	return purgeLegLabel(purgeBookLeg{Symbol: leg.Symbol, Contract: leg.Contract})
}

func loadPurgeBook(id string) (purgeBook, error) {
	id, err := cleanPurgeID(id)
	if err != nil {
		return purgeBook{}, err
	}
	path, err := purgeBookPath(id)
	if err != nil {
		return purgeBook{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return purgeBook{}, fmt.Errorf("read purge book %s: %w", id, err)
	}
	var book purgeBook
	if err := json.Unmarshal(raw, &book); err != nil {
		return purgeBook{}, fmt.Errorf("decode purge book %s: %w", id, err)
	}
	if book.Kind != purgeBookKind || book.SchemaVersion != purgeBookSchemaVersion {
		return purgeBook{}, fmt.Errorf("purge book %s is %q/%q, want %q/%q", id, book.Kind, book.SchemaVersion, purgeBookKind, purgeBookSchemaVersion)
	}
	book.BookPath = path
	return book, nil
}

func loadActivePurgeBook() (purgeBook, bool, error) {
	path, err := purgeBookPath(activePurgeBookID)
	if err != nil {
		return purgeBook{}, false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return purgeBook{}, false, nil
		}
		return purgeBook{}, false, fmt.Errorf("read active purge book: %w", err)
	}
	var book purgeBook
	if err := json.Unmarshal(raw, &book); err != nil {
		return purgeBook{}, false, fmt.Errorf("decode active purge book: %w", err)
	}
	if book.Kind != purgeBookKind || book.SchemaVersion != purgeBookSchemaVersion {
		return purgeBook{}, false, fmt.Errorf("active purge book is %q/%q, want %q/%q", book.Kind, book.SchemaVersion, purgeBookKind, purgeBookSchemaVersion)
	}
	book.BookPath = path
	return book, true, nil
}

func savePurgeBook(book *purgeBook) (string, error) {
	if book == nil {
		return "", fmt.Errorf("purge book is nil")
	}
	if strings.TrimSpace(book.PurgeID) == "" {
		book.PurgeID = purgeBookID(time.Now())
	}
	id, err := cleanPurgeID(book.PurgeID)
	if err != nil {
		return "", err
	}
	book.PurgeID = id
	book.Kind = purgeBookKind
	book.SchemaVersion = purgeBookSchemaVersion
	book.UpdatedAt = time.Now()
	path, err := purgeBookPath(id)
	if err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(book, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal purge book: %w", err)
	}
	raw = append(raw, '\n')
	if err := writePrivateAtomic(path, raw); err != nil {
		return "", err
	}
	return path, nil
}

func purgeBookPath(id string) (string, error) {
	dir, err := purgeBookDefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

func defaultPurgeBookDir() (string, error) {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "purges"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "state", "ibkr", "purges"), nil
}

func writePrivateAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

func purgeBookID(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	return "purge_" + now.Format("20060102_150405")
}

func cleanPurgeID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("purge id is required")
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return "", fmt.Errorf("invalid purge id %q", id)
	}
	return id, nil
}

func purgeLegID(idx int, p rpc.PositionView) string {
	_ = idx
	key := purgeContractInstrumentKey(purgeContractFromPosition(p))
	sum := sha256.Sum256([]byte(key))
	return "leg_" + hex.EncodeToString(sum[:])[:12]
}

func purgeLegInstrumentKey(leg purgeBookLeg) string {
	return purgeContractInstrumentKey(leg.Contract)
}

func purgeContractInstrumentKey(c rpc.ContractParams) string {
	parts := []string{
		strings.ToUpper(strings.TrimSpace(c.Symbol)),
		strings.ToUpper(strings.TrimSpace(c.SecType)),
		strconv.Itoa(c.ConID),
		strings.ToUpper(strings.TrimSpace(c.Exchange)),
		strings.ToUpper(strings.TrimSpace(c.PrimaryExch)),
		strings.ToUpper(strings.TrimSpace(c.Currency)),
		strings.ToUpper(strings.TrimSpace(c.LocalSymbol)),
		strings.ToUpper(strings.TrimSpace(c.TradingClass)),
		strings.TrimSpace(c.Expiry),
		strconv.FormatFloat(c.Strike, 'f', 4, 64),
		strings.ToUpper(strings.TrimSpace(c.Right)),
		strconv.Itoa(c.Multiplier),
	}
	return strings.Join(parts, "|")
}

func purgeLegLabel(leg purgeBookLeg) string {
	if strings.EqualFold(leg.Contract.SecType, "OPT") {
		expiry := leg.Contract.Expiry
		if len(expiry) == 8 {
			expiry = expiry[2:]
		}
		return fmt.Sprintf("%s %s %s %.2f", leg.Symbol, expiry, leg.Contract.Right, leg.Contract.Strike)
	}
	return leg.Symbol
}

func purgeLegState(leg purgeBookLeg) string {
	state := leg.Status
	if leg.QuoteQuality != "" {
		state += "/" + leg.QuoteQuality
	}
	if leg.DataType != "" && !rpc.IsLiveDataType(leg.DataType) {
		state += "/" + leg.DataType
	}
	if len(leg.Warnings) > 0 {
		state += " warn"
	}
	return state
}

func formatPurgeQuantity(q float64) string {
	if math.Trunc(q) == q {
		return fmt.Sprintf("%.0f", q)
	}
	return strconv.FormatFloat(q, 'f', 4, 64)
}

func validPricePtr(v *float64) bool {
	return v != nil && *v > 0 && !math.IsNaN(*v) && !math.IsInf(*v, 0)
}

func purgeProgress(env *Env, jsonOut bool, format string, args ...any) {
	if jsonOut || env == nil || env.Stderr == nil {
		return
	}
	fmt.Fprintf(env.Stderr, "canary: "+format+"...\n", args...)
}
