package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runSettings(ctx context.Context, env *Env, args []string) int {
	if len(args) == 1 && helpArg(args[0]) {
		printSettingsUsage(env)
		return 0
	}
	sub := "show"
	if idx := settingsSubcommandIndex(args); idx >= 0 {
		sub = args[idx]
		args = append(append([]string{}, args[:idx]...), args[idx+1:]...)
	}
	switch sub {
	case "show":
		return runSettingsShow(ctx, env, args)
	case "set":
		return runSettingsSet(ctx, env, args)
	default:
		return fail(env, "settings: unknown subcommand %q (try `canary settings show` or `canary settings set key=value`)", sub)
	}
}

func settingsSubcommandIndex(args []string) int {
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
		case "show", "set":
			return i
		default:
			return -1
		}
	}
	return -1
}

func runSettingsShow(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "settings show")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	var res rpc.PlatformSettings
	if err := env.Conn.Call(ctx, rpc.MethodSettingsGet, nil, &res); err != nil {
		return fail(env, "settings show: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderSettingsText(env, &res)
	return 0
}

func runSettingsSet(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "settings set")
	fs.Usage = func() { printSettingsSetUsage(env) }
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "settings set: usage is `canary settings set key=value`; run `canary settings set --help` for supported keys")
	}
	patch, err := settingsPatchFromAssignment(fs.Arg(0))
	if err != nil {
		return fail(env, "settings set: %v", err)
	}
	patch, err = settingsPatchWithOrigin(patch, env.Origin)
	if err != nil {
		return fail(env, "settings set: %v", err)
	}
	var res rpc.PlatformSettings
	if err := env.Conn.Call(ctx, rpc.MethodSettingsUpdate, patch, &res); err != nil {
		return fail(env, "settings set: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderSettingsText(env, &res)
	return 0
}

// settingsPatchWithOrigin stamps the request origin into the settings patch;
// the daemon pops the reserved "origin" key before validating settings keys
// and uses it to gate trading.freeze writes to a human terminal.
func settingsPatchWithOrigin(patch json.RawMessage, origin string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(patch, &obj); err != nil {
		return nil, err
	}
	rawOrigin, err := json.Marshal(origin)
	if err != nil {
		return nil, err
	}
	obj["origin"] = rawOrigin
	return json.Marshal(obj)
}

func settingsPatchFromAssignment(raw string) (json.RawMessage, error) {
	key, valueRaw, ok := strings.Cut(raw, "=")
	if !ok {
		return nil, fmt.Errorf("expected key=value")
	}
	key = strings.TrimSpace(key)
	valueRaw = strings.TrimSpace(valueRaw)
	if key == "" || valueRaw == "" {
		return nil, fmt.Errorf("expected non-empty key and value")
	}
	marshalPatch := func(path []string, value any) (json.RawMessage, error) {
		root := map[string]any{}
		cur := root
		for _, part := range path[:len(path)-1] {
			next := map[string]any{}
			cur[part] = next
			cur = next
		}
		cur[path[len(path)-1]] = value
		raw, err := json.Marshal(root)
		return json.RawMessage(raw), err
	}
	spec, mapSymbol, ok := resolveSettingsKey(key)
	if !ok {
		return nil, fmt.Errorf("unsupported setting key %q (supported: %s)", key, strings.Join(supportedSettingsKeys(), ", "))
	}
	path := strings.Split(spec.Key, ".")
	if spec.Kind == rpc.SettingsKindDateMap {
		// Date-map keys take date strings, not the bool/number grammar:
		// <key>.SYMBOL=YYYY-MM-DD[Tamc|Tbmo] upserts one symbol, =null clears
		// it, and =null on the bare key clears all. The daemon owns date
		// validation and merges per symbol.
		if mapSymbol == "" {
			if !strings.EqualFold(valueRaw, "null") {
				return nil, fmt.Errorf("%s takes only null (clear all overrides); set one symbol with %s.SYMBOL=YYYY-MM-DD, optional Tamc/Tbmo suffix", key, spec.Key)
			}
			return marshalPatch(path, nil)
		}
		var value any
		if !strings.EqualFold(valueRaw, "null") {
			value = valueRaw
		}
		return marshalPatch(append(path, mapSymbol), value)
	}
	if spec.Kind == rpc.SettingsKindDateFormat || spec.Kind == rpc.SettingsKindCashSweepPriority {
		if strings.EqualFold(valueRaw, "null") {
			return marshalPatch(path, nil)
		}
		return marshalPatch(path, strings.ToLower(strings.TrimSpace(valueRaw)))
	}
	value, err := parseSettingsValue(valueRaw)
	if err != nil {
		return nil, err
	}
	if spec.Kind == rpc.SettingsKindBool && value != nil {
		if _, isBool := value.(bool); !isBool {
			return nil, fmt.Errorf("%s must be true, false, or null", key)
		}
	}
	// Numeric kinds pass through as parsed; the daemon owns range checks.
	return marshalPatch(path, value)
}

// resolveSettingsKey matches a CLI key against the shared settings registry.
// Date-map keys additionally accept a `.SYMBOL` suffix, returned upper-cased.
func resolveSettingsKey(key string) (rpc.SettingsKeySpec, string, bool) {
	for _, spec := range rpc.SettingsKeys() {
		if spec.Key == key {
			return spec, "", true
		}
		if spec.Kind != rpc.SettingsKindDateMap {
			continue
		}
		if sym, isSub := strings.CutPrefix(key, spec.Key+"."); isSub {
			sym = strings.ToUpper(strings.TrimSpace(sym))
			if sym == "" {
				return rpc.SettingsKeySpec{}, "", false
			}
			return spec, sym, true
		}
	}
	return rpc.SettingsKeySpec{}, "", false
}

// supportedSettingsKeys renders the registry as CLI key spellings; date-map
// keys advertise the per-symbol form.
func supportedSettingsKeys() []string {
	specs := rpc.SettingsKeys()
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		if spec.Kind == rpc.SettingsKindDateMap {
			out = append(out, spec.Key+".<SYMBOL>")
			continue
		}
		out = append(out, spec.Key)
	}
	return out
}

func printSettingsUsage(env *Env) {
	fmt.Fprintln(env.Stdout, "canary settings — Runtime platform preferences and observed read-only state")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Usage: canary settings show [--json]")
	fmt.Fprintln(env.Stdout, "       canary settings set <supported-key>=<value> [--json]")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Run `canary settings set --help` for supported keys.")
}

func printSettingsSetUsage(env *Env) {
	fmt.Fprintln(env.Stdout, "canary settings set — update a daemon-owned runtime setting")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Usage: canary settings set <supported-key>=<value> [--json]")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Supported keys:")
	for _, spec := range rpc.SettingsKeys() {
		display := spec.Key
		if spec.Kind == rpc.SettingsKindDateMap {
			display += ".<SYMBOL>"
		}
		fmt.Fprintf(env.Stdout, "  - %s\n      %s\n", display, spec.Doc)
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Values are true, false, null, a number, or the documented closed string set.")
	fmt.Fprintln(env.Stdout, "Date format takes us, eu, us_weekday, or eu_weekday. Earnings overrides take")
	fmt.Fprintln(env.Stdout, "YYYY-MM-DD (optional Tamc/Tbmo suffix), null to clear one symbol, or")
	fmt.Fprintln(env.Stdout, "null on the bare earnings_overrides key to clear all of them.")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "The daemon still decides writability from each field's access/source metadata.")
}

func parseSettingsValue(raw string) (any, error) {
	switch strings.ToLower(raw) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if i, err := strconv.Atoi(raw); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f, nil
	}
	return nil, fmt.Errorf("value must be true, false, null, or a number")
}

func renderSettingsText(env *Env, st *rpc.PlatformSettings) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Settings  %s\n", env.statusBadge(settingsVerdict(*st)))
	fmt.Fprintln(out)
	displayRow(env, out, "Date format", nonEmpty(st.Display.DateFormat.Value, rpc.DisplayDateFormatUS))
	displayRow(env, out, "Stock protection", formatSettingsBool(env, st.Features.StockProtection.Enabled))
	displayRow(env, out, "Rulebook", formatSettingsBool(env, st.Features.Rulebook.Enabled))
	if n := len(st.Features.Rulebook.EarningsOverrides.Value); n > 0 {
		displayRow(env, out, "Earnings overrides", fmt.Sprintf("%d symbol(s)", n))
	}
	displayRow(env, out, "Trading freeze", formatSettingsBool(env, st.Trading.Freeze))
	displayRow(env, out, "Trading", nonEmpty(st.Trading.Mode.Value, "disabled"))
	displayRow(env, out, "Endpoint", nonEmpty(st.Trading.Endpoint.Value, "unknown"))
	displayRow(env, out, "Account", nonEmpty(st.Trading.Account.Value, "unknown"))
	displayRow(env, out, "MCP trading", nonEmpty(st.Trading.MCPTrading.Value, "disabled"))
	displayRow(env, out, "Build", st.Build.Channel.Value)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Order limits (risk-policy.toml [order_limits]):")
	displayRow(env, out, "Order cap in force", orderCapSettingText(st.Trading.Limits.MaxNotional))
	displayRow(env, out, "Max option qty (every order)", fmt.Sprintf("%d (%s)", st.Trading.Limits.MaxOptionContracts.Value, accessSummary(st.Trading.Limits.MaxOptionContracts.Access, st.Trading.Limits.MaxOptionContracts.Source)))
	displayRow(env, out, "Apparent exits", "same caps; SELL uses worst-case short/STO gates")
	displayRow(env, out, "Stock short", formatSettingsBool(env, st.Trading.Limits.AllowStockShort))
	displayRow(env, out, "Option STO", formatSettingsBool(env, st.Trading.Limits.AllowOptionSellToOpen))
	fmt.Fprintln(out)
	displayRow(env, out, "Market data", nonEmpty(st.MarketData.Quality.Status, "unknown")+" - "+nonEmpty(st.MarketData.Quality.Summary, "no observation"))
	for _, concern := range st.MarketData.Quality.DataQuality {
		displayLine(env, "  "+concern.Surface+" · "+concern.Status+" · "+concern.Summary, env.yellow)
	}
	if st.Build.ExperimentalTradingNote != "" {
		displayRow(env, out, "Build note", st.Build.ExperimentalTradingNote)
	}
	fmt.Fprintln(out)
}

func settingsVerdict(st rpc.PlatformSettings) statusConcern {
	if st.Trading.Freeze.Value {
		return statusConcern{Text: "FROZEN", Level: statusConcernWarn}
	}
	if !st.Features.StockProtection.Enabled.Value {
		return statusConcern{Text: "LIMITED", Level: statusConcernNotice}
	}
	if st.MarketData.Quality.Status == "degraded" {
		return statusConcern{Text: "DEGRADED", Level: statusConcernWarn}
	}
	if st.MarketData.Quality.Status == "delayed" {
		return statusConcern{Text: "DELAYED DATA", Level: statusConcernNotice}
	}
	if st.MarketData.Quality.Status != "ok" {
		return statusConcern{Text: "DATA UNKNOWN", Level: statusConcernNotice}
	}
	return statusConcern{Text: "READY", Level: statusConcernNone}
}

func formatSettingsBool(_ *Env, v rpc.SettingsBool) string {
	return fmt.Sprint(v.Value) + " (" + accessSummary(v.Access, v.Source) + ")"
}

func accessSummary(access, source string) string {
	if source == "" {
		return access
	}
	return access + "/" + source
}

// orderCapSettingText renders the order cap in force from its settings leaf:
// the reason carries the policy's own summary of how it is bound.
func orderCapSettingText(f rpc.SettingsFloat) string {
	if reason := strings.TrimSpace(f.Reason); reason != "" {
		return strings.TrimPrefix(reason, "risk-policy.toml [order_limits]: ")
	}
	return fmt.Sprintf("%.2f (%s)", f.Value, accessSummary(f.Access, f.Source))
}
