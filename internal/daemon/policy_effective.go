package daemon

import (
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The effective policy view (`canary policy show --explain`) prints every
// key that governs behaviour with its value in force, where the value comes
// from and what it means. Meanings come from the generated help tables
// (policy_help_generated.go, built from the struct doc comments that also
// write docs/docs/reference/config.md), so no description is duplicated by
// hand. The view is read-only.

// policyEffectiveInputs is what one view is assembled from: the policies in
// force (the daemon's managers, or the files when the CLI reads them), the
// keys each file sets, the trading gates of config.toml and the runtime
// settings view.
type policyEffectiveInputs struct {
	origin       string
	files        []rpc.PolicyFileStatus
	limits       []risk.ConstitutionLimit
	rulebook     risk.RulebookPolicy
	protection   *protectionPolicy
	opportunity  *opportunityPolicy
	trading      config.Trading
	settings     *rpc.PlatformSettings
	definedByKey map[string]map[string]bool
	// notes carries per-file read errors when the CLI reads the files.
	notes map[string][]string
}

// policyEffectiveView assembles the view from the policies in force.
func (s *Server) policyEffectiveView(files []rpc.PolicyFileStatus, limits []risk.ConstitutionLimit) *rpc.PolicyEffectiveView {
	if s == nil {
		return nil
	}
	in := policyEffectiveInputs{origin: "daemon", files: files, limits: limits}
	in.rulebook, _ = s.activeRulebookPolicy()
	if s.protectionPolicies != nil {
		p, _ := s.protectionPolicies.Active()
		in.protection = &p
	}
	if s.opportunityPolicies != nil {
		p, _ := s.opportunityPolicies.Active()
		in.opportunity = &p
	}
	if s.cfg != nil {
		in.trading = s.cfg.Trading
	}
	health := s.statusHealthSnapshot()
	settings := s.platformSettingsSnapshot(&platformSettingsObserved{
		DataQuality:     health.DataQuality,
		MarketDataReady: platformHealthMarketDataReady(health),
		ObservedAt:      s.orderNow(),
	})
	in.settings = &settings
	in.definedByKey = definedKeysForFiles(files)
	return buildPolicyEffectiveView(in)
}

// PolicyEffectiveFromFiles assembles the view by reading the policy files
// the daemon reported, for a daemon that predates the view. Runtime values
// still come from the daemon's settings view; config.toml supplies the
// file values a runtime override replaces.
func PolicyEffectiveFromFiles(configPath string, files []rpc.PolicyFileStatus, limits []risk.ConstitutionLimit, settings *rpc.PlatformSettings) *rpc.PolicyEffectiveView {
	set, _ := PolicyFileSetFromConfigFile(configPath)
	pathFor := func(policy, fallback string) string {
		for _, f := range files {
			if f.Policy == policy && f.Path != "" {
				return expandUserPath(f.Path)
			}
		}
		return fallback
	}
	in := policyEffectiveInputs{origin: "files", files: files, limits: limits, settings: settings,
		rulebook: risk.DefaultRulebookPolicy(), notes: map[string][]string{}}
	unreadable := func(policy string, err error) {
		in.notes[policy] = append(in.notes[policy], "cannot be read ("+err.Error()+"); Canary's defaults are shown")
	}
	if read, err := loadRulebookPolicyFile(pathFor(PolicyFileRulebook, set.Rulebook)); err == nil {
		in.rulebook = read.policy
	} else {
		unreadable(PolicyFileRulebook, err)
	}
	protection := defaultProtectionPolicy()
	if data, err := os.ReadFile(pathFor(PolicyFileProtection, set.Protection)); err == nil {
		if p, _, err := parseProtectionPolicy(data); err == nil {
			protection = p
		} else {
			unreadable(PolicyFileProtection, err)
		}
	}
	in.protection = &protection
	opportunity := defaultOpportunityPolicy()
	if data, err := os.ReadFile(pathFor(PolicyFileOpportunity, set.Opportunity)); err == nil {
		if p, err := parseOpportunityPolicy(data); err == nil {
			opportunity = p
		} else {
			unreadable(PolicyFileOpportunity, err)
		}
	}
	in.opportunity = &opportunity
	if cfg, err := config.Load(configPath); err == nil {
		in.trading = cfg.Trading
	}
	if len(files) == 0 {
		files = []rpc.PolicyFileStatus{
			{Policy: PolicyFileRulebook, Path: set.Rulebook},
			{Policy: PolicyFileProtection, Path: set.Protection},
			{Policy: PolicyFileOpportunity, Path: set.Opportunity},
			{Policy: PolicyFileConstitution, Path: set.Constitution},
		}
		in.files = files
	}
	in.definedByKey = definedKeysForFiles(files)
	return buildPolicyEffectiveView(in)
}

// definedKeysForFiles reads which dotted keys each policy file sets.
func definedKeysForFiles(files []rpc.PolicyFileStatus) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(expandUserPath(f.Path))
		if err != nil {
			continue
		}
		out[f.Policy] = definedTOMLKeys(data)
	}
	return out
}

// definedTOMLKeys flattens a TOML document to the dotted paths it sets,
// tables included. An unparsable file sets nothing.
func definedTOMLKeys(data []byte) map[string]bool {
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return map[string]bool{}
	}
	out := map[string]bool{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			path := joinPolicyPath(prefix, k)
			out[path] = true
			if child, ok := v.(map[string]any); ok {
				walk(path, child)
			}
		}
	}
	walk("", raw)
	return out
}

func buildPolicyEffectiveView(in policyEffectiveInputs) *rpc.PolicyEffectiveView {
	view := &rpc.PolicyEffectiveView{Origin: in.origin}
	view.Sections = append(view.Sections, constitutionSection(in))
	view.Sections = append(view.Sections, rulebookSection(in))
	if in.protection != nil {
		view.Sections = append(view.Sections, protectionSection(in))
	}
	if in.opportunity != nil {
		view.Sections = append(view.Sections, opportunitySection(in))
	}
	view.Sections = append(view.Sections, tradingSection(in), runtimeSection(in))
	return view
}

// sectionFor heads a section with its file's reported status.
func sectionFor(in policyEffectiveInputs, policy, id, title string) rpc.PolicyEffectiveSection {
	sec := rpc.PolicyEffectiveSection{ID: id, Title: title, Notes: in.notes[policy]}
	for _, f := range in.files {
		if f.Policy != policy {
			continue
		}
		sec.Path, sec.Status, sec.Review = f.Path, f.Status, f.Review
		if f.PolicyID != "" {
			sec.Identity = f.PolicyID
			if f.PolicyVersion != "" {
				sec.Identity += " v" + f.PolicyVersion
			}
		}
	}
	return sec
}

func constitutionSection(in policyEffectiveInputs) rpc.PolicyEffectiveSection {
	sec := sectionFor(in, PolicyFileConstitution, rpc.PolicySectionConstitution, "Risk constitution")
	groups := map[string]*rpc.PolicyEffectiveGroup{}
	var order []string
	for _, l := range in.limits {
		id, _, _ := strings.Cut(l.Key, ".")
		if !strings.Contains(l.Key, ".") {
			id = ""
		}
		g, ok := groups[id]
		if !ok {
			g = &rpc.PolicyEffectiveGroup{ID: id, Title: tableTitle(id)}
			groups[id] = g
			order = append(order, id)
		}
		g.Rows = append(g.Rows, rpc.PolicyEffectiveRow{Key: l.Key, Value: l.Value, Source: l.Source,
			Enforcement: l.Enforcement, Meaning: sentence(l.Meaning)})
	}
	for _, id := range order {
		sec.Groups = append(sec.Groups, *groups[id])
	}
	return sec
}

// rulebookGroups orders the Rulebook's flat keys into readable groups; a
// key that matches no prefix lands in "Other limits". Only the grouping is
// written here: values and meanings come from the policy and its help.
var rulebookGroups = []struct {
	id, title string
	prefixes  []string
}{
	{"", "File", []string{"kind", "schema_version", "policy_id", "policy_version"}},
	{"issuer", "Issuer concentration (rules 1, 2, 16, 17, 18)", []string{"single_name_", "takeover_gap_pct", "hedge_min_days", "exit_participation_pct", "illiquid_", "delta_swing_", "budget_watch_pct", "issuer_groups", "cluster"}},
	{"options", "Options and hedges", []string{"option_line_", "hedge_line_", "hedge_symbols", "overhedge_multiple", "runway_", "exit_watch_loss_pct", "exit_act_loss_pct", "short_put_"}},
	{"tape", "Earnings and daily tape", []string{"earnings_", "red_on_green_", "winner_trim_"}},
	{"account", "Account and data", []string{"fx_exposure_", "margin_headroom_", "greeks_gap_", "regime_stage_max_age_minutes"}},
	{"modes", "Rule modes", []string{"modes."}},
}

// regimeSetColumns are the Rulebook regime-conditional sets, as group columns.
var regimeSetColumns = []struct{ column, field string }{
	{"calm", "regime_calm"},
	{"early_warning", "regime_early_warning"},
	{"confirmed", "regime_confirmed"},
}

func rulebookSection(in policyEffectiveInputs) rpc.PolicyEffectiveSection {
	sec := sectionFor(in, PolicyFileRulebook, rpc.PolicySectionRulebook, "Rulebook")
	defined := in.definedByKey[PolicyFileRulebook]
	w := newPolicyWalker(rulebookPolicyHelp, defined)
	skip := map[string]bool{}
	for _, set := range regimeSetColumns {
		skip[set.field] = true
	}
	w.skip = skip
	w.walk(reflect.ValueOf(in.rulebook), "", "")
	var flat []rpc.PolicyEffectiveRow
	for _, g := range w.groups {
		flat = append(flat, g.Rows...)
	}
	placed := map[string]bool{}
	for _, spec := range rulebookGroups {
		g := rpc.PolicyEffectiveGroup{ID: spec.id, Title: spec.title}
		for _, row := range flat {
			if placed[row.Key] {
				continue
			}
			for _, p := range spec.prefixes {
				if strings.HasPrefix(row.Key, p) {
					g.Rows = append(g.Rows, row)
					placed[row.Key] = true
					break
				}
			}
		}
		if len(g.Rows) > 0 {
			sec.Groups = append(sec.Groups, g)
		}
		if spec.id == "account" {
			sec.Groups = append(sec.Groups, rulebookRegimeGroup(in.rulebook, defined))
		}
	}
	other := rpc.PolicyEffectiveGroup{ID: "other", Title: "Other limits"}
	for _, row := range flat {
		if !placed[row.Key] {
			other.Rows = append(other.Rows, row)
		}
	}
	if len(other.Rows) > 0 {
		sec.Groups = append(sec.Groups, other)
	}
	return sec
}

// rulebookRegimeGroup prints rules 3, 4, 12 and 15 once per key with one
// value per regime set.
func rulebookRegimeGroup(p risk.RulebookPolicy, defined map[string]bool) rpc.PolicyEffectiveGroup {
	g := rpc.PolicyEffectiveGroup{ID: "regime", Title: "Regime sets (rules 3, 4, 12, 15)"}
	for _, set := range regimeSetColumns {
		g.Columns = append(g.Columns, set.column)
	}
	sets := []reflect.Value{reflect.ValueOf(p.RegimeCalm), reflect.ValueOf(p.RegimeEarlyWarning), reflect.ValueOf(p.RegimeConfirmed)}
	t := reflect.TypeFor[risk.RegimeThresholds]()
	for i := range t.NumField() {
		name := tomlName(t.Field(i))
		if name == "" {
			continue
		}
		row := rpc.PolicyEffectiveRow{Key: "regime_*." + name}
		var inFile, absent int
		for si, set := range regimeSetColumns {
			row.Values = append(row.Values, policyValueText(name, sets[si].Field(i)))
			if defined[set.field+"."+name] {
				inFile++
			} else {
				absent++
			}
		}
		switch {
		case absent == 0:
			row.Source = rpc.PolicySourceFile
		case inFile == 0:
			row.Source = rpc.PolicySourceDefault
		default:
			row.Source = rpc.PolicySourceFile + "+" + rpc.PolicySourceDefault
		}
		row.Value = strings.Join(row.Values, " / ")
		row.Meaning = sentence(rulebookPolicyHelp["regime_calm."+name])
		g.Rows = append(g.Rows, row)
	}
	return g
}

// cashSweepDefaultCurrencies are the currencies with a compiled sweep
// declaration, printed even when the file has no table for them.
var cashSweepDefaultCurrencies = []string{"USD", "EUR", "GBP", "CAD"}

// cashSweepMachineKeys are the currency keys Canary maintains (the bill
// settlement route) until a file sets them.
var cashSweepMachineKeys = map[string]bool{"settlement_days": true, "settlement_exchange": true, "settlement_valid_through": true}

func protectionSection(in policyEffectiveInputs) rpc.PolicyEffectiveSection {
	sec := sectionFor(in, PolicyFileProtection, rpc.PolicySectionProtection, "Protection policy")
	defined := in.definedByKey[PolicyFileProtection]
	p := *in.protection

	// Absent optional buckets print at their defaults so every key stays
	// visible; the notes say what holds until the owner writes the table.
	budget := protectionBudgetPolicy{}
	if p.Buckets.BudgetReduction != nil {
		budget = *p.Buckets.BudgetReduction
	}
	sweep := protectionCashSweepPolicy{}
	if p.Buckets.CashSweep != nil {
		sweep = *p.Buckets.CashSweep
	}
	currencies := map[string]protectionCashSweepCurrency{}
	maps.Copy(currencies, sweep.Currency)
	for _, ccy := range cashSweepDefaultCurrencies {
		if _, ok := currencies[ccy]; !ok {
			currencies[ccy] = defaultCashSweepCurrency(ccy)
		}
	}
	sweep.Currency = currencies
	p.Buckets.BudgetReduction, p.Buckets.CashSweep = &budget, &sweep

	w := newPolicyWalker(protectionPolicyHelp, defined)
	w.walk(reflect.ValueOf(p), "", "")

	// Effective resolution of keys whose empty value means a default.
	resolved := map[string]string{
		"authority.veto_window":                shortDuration(p.Authority.vetoWindow()),
		"buckets.trailing_stop.tif":            p.Buckets.TrailingStop.effectiveTIF(),
		"buckets.trailing_stop.options.tif":    p.Buckets.TrailingStop.Options.effectiveTIF(),
		"buckets.budget_reduction.mode":        in.protection.Buckets.BudgetReduction.effectiveMode(),
		"buckets.budget_reduction.basis":       in.protection.Buckets.BudgetReduction.basis(),
		"buckets.cash_sweep.mode":              in.protection.Buckets.CashSweep.effectiveMode(),
		"buckets.cash_sweep.currency_priority": nonEmptyString(sweep.CurrencyPriority, "none (policy order)"),
	}
	// Under basis = rulebook the governor's caps are the Rulebook's levels,
	// which Canary applies in place of the two percentages.
	if in.protection.Buckets.BudgetReduction.basis() == rpc.BudgetBasisRulebook {
		resolved["buckets.budget_reduction.premium_at_risk_pct_of_risk_capital"] = "rule 3 budget (Rulebook)"
		resolved["buckets.budget_reduction.per_line_pct_of_risk_capital"] = formatPolicyNumber(in.rulebook.OptionLineActPct) + "% NLV (Rulebook)"
	}
	// A cap nobody has written reads as missing, not as zero.
	for key, value := range map[string]float64{
		"buckets.budget_reduction.premium_at_risk_pct_of_risk_capital": budget.PremiumAtRiskPctOfRiskCapital,
		"buckets.budget_reduction.per_line_pct_of_risk_capital":        budget.PerLinePctOfRiskCapital,
		"buckets.budget_reduction.max_order_notional":                  budget.MaxOrderNotional,
		"buckets.cash_sweep.max_order_notional":                        sweep.MaxOrderNotional,
	} {
		if _, set := resolved[key]; !set && value == 0 {
			resolved[key] = "—"
		}
	}
	machine := map[string]bool{
		"buckets.budget_reduction.premium_at_risk_pct_of_risk_capital": in.protection.Buckets.BudgetReduction.basis() == rpc.BudgetBasisRulebook,
		"buckets.budget_reduction.per_line_pct_of_risk_capital":        in.protection.Buckets.BudgetReduction.basis() == rpc.BudgetBasisRulebook,
	}
	needs := map[string]bool{}
	if len(p.Authority.PreAuthorised) == 0 {
		needs["authority.pre_authorised"] = true
	}
	for _, k := range in.protection.Buckets.BudgetReduction.missingNumbers() {
		needs["buckets.budget_reduction."+k] = true
	}
	for _, k := range in.protection.Buckets.CashSweep.missingNumbers() {
		needs["buckets.cash_sweep."+k] = true
	}
	if in.protection.Buckets.CashSweep.enabled() {
		if sweep.TaxReviewedAt == "" {
			needs["buckets.cash_sweep.tax_reviewed_at"] = true
		}
		for ccy, c := range currencies {
			for _, k := range c.missingNumbers() {
				needs["buckets.cash_sweep.currency."+ccy+"."+k] = true
			}
		}
	}
	for gi := range w.groups {
		g := &w.groups[gi]
		for ri := range g.Rows {
			row := &g.Rows[ri]
			if v, ok := resolved[row.Key]; ok && !defined[row.Key] {
				row.Value = v
			}
			if machine[row.Key] && !defined[row.Key] {
				row.Source = rpc.PolicySourceMachine
			}
			leaf := row.Key[strings.LastIndex(row.Key, ".")+1:]
			if strings.HasPrefix(g.ID, "buckets.cash_sweep.currency.") && cashSweepMachineKeys[leaf] && !defined[row.Key] {
				row.Value, row.Source = "Canary's maintained route", rpc.PolicySourceMachine
			}
			if needs[row.Key] {
				row.Source = rpc.PolicySourceNeedsYourNumber
			}
			// The sweep reads its sizing numbers from the file only (reserve
			// design, 2026-10-05): an unwritten one is not written, never a
			// default, and min_tranche is retired in favour of
			// min_order_notional. Instruments, maturities and the ladder keep
			// their compiled per-currency declaration.
			if strings.HasPrefix(g.ID, "buckets.cash_sweep") && !defined[row.Key] {
				switch {
				case leaf == "min_tranche":
					row.Source = rpc.PolicySourceRetired
				case cashSweepSizingKeys[leaf] && row.Source == rpc.PolicySourceDefault:
					row.Source = rpc.PolicySourceNotWritten
				}
			}
		}
		switch {
		case g.ID == "buckets.budget_reduction" && in.protection.Buckets.BudgetReduction == nil:
			g.Notes = append(g.Notes, "not in the file: the premium budget governor is off until you write [buckets.budget_reduction] with your caps")
		case g.ID == "buckets.cash_sweep" && in.protection.Buckets.CashSweep == nil:
			g.Notes = append(g.Notes, "not in the file: the cash sweep is off until you write [buckets.cash_sweep]")
		case strings.HasPrefix(g.ID, "buckets.cash_sweep.currency.") && !defined[g.ID]:
			g.Notes = append(g.Notes, "no table in the file: Canary's compiled declaration for this currency applies")
		}
	}
	sec.Groups = w.groups
	return sec
}

func opportunitySection(in policyEffectiveInputs) rpc.PolicyEffectiveSection {
	sec := sectionFor(in, PolicyFileOpportunity, rpc.PolicySectionOpportunity, "Opportunity policy")
	w := newPolicyWalker(opportunityPolicyHelp, in.definedByKey[PolicyFileOpportunity])
	w.walk(reflect.ValueOf(*in.opportunity), "", "")
	sec.Groups = w.groups
	return sec
}

// tradingSection prints the [trading] order-entry gates of config.toml with
// the value in force: a runtime override from `canary settings set` names
// the config.toml value it replaces.
func tradingSection(in policyEffectiveInputs) rpc.PolicyEffectiveSection {
	sec := rpc.PolicyEffectiveSection{ID: rpc.PolicySectionTrading, Title: "Trading gates", Path: "config.toml"}
	file := in.trading.WithDefaults()
	g := rpc.PolicyEffectiveGroup{ID: "trading", Title: "[trading]"}
	type leaf struct {
		key, fileValue string
		active         *settingsLeaf
	}
	var st *rpc.PlatformTradingSettings
	if in.settings != nil {
		st = &in.settings.Trading
	}
	pick := func(f func(*rpc.PlatformTradingSettings) settingsLeaf) *settingsLeaf {
		if st == nil {
			return nil
		}
		l := f(st)
		return &l
	}
	rows := []leaf{
		{"mode", file.Mode, pick(func(t *rpc.PlatformTradingSettings) settingsLeaf { return leafOf(t.Mode) })},
		{"max_notional", formatPolicyNumber(file.MaxNotional), pick(func(t *rpc.PlatformTradingSettings) settingsLeaf { return leafOf(t.Limits.MaxNotional) })},
		{"max_option_contracts", strconv.Itoa(file.MaxOptionContracts), pick(func(t *rpc.PlatformTradingSettings) settingsLeaf { return leafOf(t.Limits.MaxOptionContracts) })},
		{"allow_stock_short", strconv.FormatBool(file.AllowStockShort), pick(func(t *rpc.PlatformTradingSettings) settingsLeaf { return leafOf(t.Limits.AllowStockShort) })},
		{"allow_option_sell_to_open", strconv.FormatBool(file.AllowOptionSellToOpen), pick(func(t *rpc.PlatformTradingSettings) settingsLeaf { return leafOf(t.Limits.AllowOptionSellToOpen) })},
	}
	for _, r := range rows {
		row := rpc.PolicyEffectiveRow{Key: "trading." + r.key, Value: r.fileValue, Source: "config",
			Meaning: sentence(tradingConfigHelp["trading."+r.key])}
		if r.active != nil {
			row.Value, row.Source = r.active.value, r.active.source
			if r.active.source == rpc.PolicySourceRuntime && r.active.value != r.fileValue {
				row.FileValue = r.fileValue
			}
		}
		g.Rows = append(g.Rows, row)
	}
	if st != nil {
		freeze := leafOf(st.Freeze)
		g.Rows = append(g.Rows, rpc.PolicyEffectiveRow{Key: "trading.freeze", Value: freeze.value, Source: freeze.source,
			Meaning: settingsKeyDoc("trading.freeze", freeze.reason)})
	} else {
		sec.Notes = append(sec.Notes, "runtime settings unavailable: values are config.toml's")
	}
	sec.Groups = []rpc.PolicyEffectiveGroup{g}
	return sec
}

// runtimeSkip leaves out what is identity or observation rather than a
// setting (the account, gateway endpoint and client id, market-data
// quality), the retired history shape, and the trading keys the trading
// section already prints.
var runtimeSkip = map[string]bool{
	"kind": true, "as_of": true, "market_data": true, "history": true,
	"trading.account": true, "trading.endpoint": true, "trading.client_id": true,
	"trading.mode": true, "trading.freeze": true, "trading.limits": true,
	"build.experimental_trading_note": true,
}

func runtimeSection(in policyEffectiveInputs) rpc.PolicyEffectiveSection {
	sec := rpc.PolicyEffectiveSection{ID: rpc.PolicySectionRuntime, Title: "Runtime settings", Path: "canary settings"}
	if in.settings == nil {
		sec.Notes = append(sec.Notes, "runtime settings unavailable")
		return sec
	}
	groups := map[string]*rpc.PolicyEffectiveGroup{}
	var order []string
	var walk func(v reflect.Value, path string)
	walk = func(v reflect.Value, path string) {
		if runtimeSkip[path] {
			return
		}
		if l, ok := settingsLeafOf(v); ok {
			id := path
			if i := strings.LastIndex(path, "."); i >= 0 {
				id = path[:i]
			}
			g, seen := groups[id]
			if !seen {
				g = &rpc.PolicyEffectiveGroup{ID: id, Title: id}
				groups[id] = g
				order = append(order, id)
			}
			g.Rows = append(g.Rows, rpc.PolicyEffectiveRow{Key: path, Value: l.value, Source: l.source,
				Meaning: settingsKeyDoc(path, l.reason)})
			return
		}
		if v.Kind() != reflect.Struct || v.Type() == reflect.TypeFor[time.Time]() {
			return
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || name == "" || name == "-" {
				continue
			}
			walk(v.Field(i), joinPolicyPath(path, name))
		}
	}
	walk(reflect.ValueOf(*in.settings), "")
	for _, id := range order {
		sec.Groups = append(sec.Groups, *groups[id])
	}
	return sec
}

// settingsLeaf is one Settings* value from the settings view.
type settingsLeaf struct{ value, source, reason string }

func leafOf(v any) settingsLeaf {
	l, _ := settingsLeafOf(reflect.ValueOf(v))
	return l
}

// settingsLeafOf recognises the settings view's annotated values: a struct
// with Value, Source and Access fields.
func settingsLeafOf(v reflect.Value) (settingsLeaf, bool) {
	if v.Kind() != reflect.Struct {
		return settingsLeaf{}, false
	}
	value, source, access := v.FieldByName("Value"), v.FieldByName("Source"), v.FieldByName("Access")
	if !value.IsValid() || !source.IsValid() || !access.IsValid() {
		return settingsLeaf{}, false
	}
	l := settingsLeaf{source: source.String()}
	if r := v.FieldByName("Reason"); r.IsValid() {
		l.reason = r.String()
	}
	switch value.Kind() {
	case reflect.Map:
		if value.Len() == 0 {
			l.value = "none"
			break
		}
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(value.Interface().(map[string]string))) {
			parts = append(parts, k+" "+value.MapIndex(reflect.ValueOf(k)).String())
		}
		l.value = strings.Join(parts, ", ")
	case reflect.Float64:
		l.value = formatPolicyNumber(value.Float())
	default:
		l.value = fmt.Sprint(value.Interface())
	}
	if l.value == "" {
		l.value = "—"
	}
	return l, true
}

// settingsKeyDoc is the registry's description of a writable setting, else
// the settings view's own reason.
func settingsKeyDoc(key, reason string) string {
	for _, spec := range rpc.SettingsKeys() {
		if spec.Key == key {
			return sentence(spec.Doc)
		}
	}
	return sentence(reason)
}

// policyWalker turns a policy struct into groups of rows by its TOML tags.
type policyWalker struct {
	help    map[string]string
	defined map[string]bool
	skip    map[string]bool
	groups  []rpc.PolicyEffectiveGroup
	index   map[string]int
}

func newPolicyWalker(help map[string]string, defined map[string]bool) *policyWalker {
	if defined == nil {
		defined = map[string]bool{}
	}
	return &policyWalker{help: help, defined: defined, index: map[string]int{}}
}

func (w *policyWalker) group(id string) *rpc.PolicyEffectiveGroup {
	i, ok := w.index[id]
	if !ok {
		i = len(w.groups)
		w.index[id] = i
		w.groups = append(w.groups, rpc.PolicyEffectiveGroup{ID: id, Title: tableTitle(id)})
	}
	return &w.groups[i]
}

func (w *policyWalker) add(groupID string, row rpc.PolicyEffectiveRow) {
	g := w.group(groupID)
	g.Rows = append(g.Rows, row)
}

// walk visits v, a struct at TOML path; helpPath is the same path with map
// keys written as <name>, as the help tables key them.
func (w *policyWalker) walk(v reflect.Value, path, helpPath string) {
	t := v.Type()
	// Leaves come before nested tables, as TOML writes them.
	var nested []int
	for i := range t.NumField() {
		f := t.Field(i)
		name := tomlName(f)
		if !f.IsExported() || name == "" || w.skip[joinPolicyPath(path, name)] {
			continue
		}
		if isPolicyTable(f.Type) {
			nested = append(nested, i)
			continue
		}
		w.leaf(v.Field(i), path, helpPath, name)
	}
	if len(nested) > 0 && len(w.group(path).Rows) == 0 && path != "" {
		// Keep a table that has only nested tables out of the print.
		w.groups = w.groups[:len(w.groups)-1]
		delete(w.index, path)
	}
	for _, i := range nested {
		f := t.Field(i)
		name := tomlName(f)
		fv := v.Field(i)
		if fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				fv = reflect.Zero(fv.Type().Elem())
			} else {
				fv = fv.Elem()
			}
		}
		full, fullHelp := joinPolicyPath(path, name), joinPolicyPath(helpPath, name)
		if fv.Kind() == reflect.Map {
			keys := make([]string, 0, fv.Len())
			for _, k := range fv.MapKeys() {
				keys = append(keys, k.String())
			}
			slices.Sort(keys)
			for _, k := range keys {
				w.walk(fv.MapIndex(reflect.ValueOf(k)), full+"."+k, fullHelp+".<name>")
			}
			continue
		}
		w.walk(fv, full, fullHelp)
	}
}

// leaf adds one key; a map of plain values prints one row per entry.
func (w *policyWalker) leaf(v reflect.Value, path, helpPath, name string) {
	full, fullHelp := joinPolicyPath(path, name), joinPolicyPath(helpPath, name)
	meaning := sentence(w.help[fullHelp])
	source := func(key string) string {
		if w.defined[key] {
			return rpc.PolicySourceFile
		}
		return rpc.PolicySourceDefault
	}
	if v.Kind() == reflect.Map {
		if v.Len() == 0 {
			w.add(path, rpc.PolicyEffectiveRow{Key: full, Value: "none", Source: source(full), Meaning: meaning})
			return
		}
		keys := make([]string, 0, v.Len())
		for _, k := range v.MapKeys() {
			keys = append(keys, k.String())
		}
		slices.Sort(keys)
		for i, k := range keys {
			row := rpc.PolicyEffectiveRow{Key: full + "." + k, Value: policyValueText(name, v.MapIndex(reflect.ValueOf(k))), Source: source(full + "." + k)}
			if i == 0 {
				row.Meaning = meaning
			}
			w.add(path, row)
		}
		return
	}
	w.add(path, rpc.PolicyEffectiveRow{Key: full, Value: policyValueText(name, v), Source: source(full), Meaning: meaning})
}

// isPolicyTable reports whether a field type is a nested TOML table: a
// struct (not a date) or a map of structs, directly or behind a pointer.
func isPolicyTable(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Map {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct && t != reflect.TypeFor[time.Time]()
}

func tomlName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
	if name == "-" {
		return ""
	}
	return name
}

func joinPolicyPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// tableTitle renders a group id as its TOML table header.
func tableTitle(id string) string {
	if id == "" {
		return "File"
	}
	return "[" + id + "]"
}

// policyValueText renders a value with the unit its key names.
func policyValueText(key string, v reflect.Value) string {
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return "—"
		}
		v = v.Elem()
	}
	if v.Type() == reflect.TypeFor[time.Time]() {
		t := v.Interface().(time.Time)
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	switch v.Kind() {
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.String:
		if v.String() == "" {
			return "—"
		}
		return v.String()
	case reflect.Int, reflect.Int64, reflect.Int32:
		// Integers are counts and identifiers (con_id): no separators.
		return strconv.FormatInt(v.Int(), 10) + policyUnit(key)
	case reflect.Float64, reflect.Float32:
		return formatPolicyNumber(v.Float()) + policyUnit(key)
	case reflect.Slice:
		if v.Len() == 0 {
			return "none"
		}
		parts := make([]string, v.Len())
		for i := range v.Len() {
			e := v.Index(i)
			if e.Kind() == reflect.Struct {
				parts[i] = policyStructText(e)
			} else {
				parts[i] = policyValueText("", e)
			}
		}
		if v.Index(0).Kind() == reflect.Struct {
			return strings.Join(parts, "; ")
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(v.Interface())
	}
}

// policyStructText renders one array-of-tables entry as key value pairs.
func policyStructText(v reflect.Value) string {
	var parts []string
	for i := range v.NumField() {
		f := v.Type().Field(i)
		name := tomlName(f)
		if !f.IsExported() || name == "" {
			continue
		}
		parts = append(parts, name+" "+policyValueText(name, v.Field(i)))
	}
	return strings.Join(parts, ", ")
}

// policyUnit names a key's unit from its name: percentages, days, sessions,
// minutes and multiples.
func policyUnit(key string) string {
	switch {
	case strings.Contains(key, "_pct"):
		return "%"
	case strings.HasSuffix(key, "_dte"), strings.HasSuffix(key, "_days"), strings.HasSuffix(key, "days_to_exit"):
		return " days"
	case strings.HasSuffix(key, "_sessions"):
		return " sessions"
	case strings.HasSuffix(key, "_minutes"):
		return " min"
	case strings.HasSuffix(key, "_multiple"):
		return "×"
	}
	return ""
}

// formatPolicyNumber prints a number without trailing zeros and with
// thousands separators from 10,000.
func formatPolicyNumber(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	intPart, frac, hasFrac := strings.Cut(s, ".")
	neg := strings.HasPrefix(intPart, "-")
	intPart = strings.TrimPrefix(intPart, "-")
	if len(intPart) > 4 {
		var b strings.Builder
		for i, r := range intPart {
			if i > 0 && (len(intPart)-i)%3 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(r)
		}
		intPart = b.String()
	}
	if neg {
		intPart = "-" + intPart
	}
	if hasFrac {
		return intPart + "." + frac
	}
	return intPart
}

func shortDuration(d time.Duration) string {
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	if s == "" {
		return "0s"
	}
	return s
}

// sentenceOpeners are the words after a dropped "is"/"are" that start an
// English phrase rather than a value vocabulary.
var sentenceOpeners = map[string]bool{"the": true, "a": true, "an": true, "one": true, "how": true, "rule": true,
	"rule's": true, "retired": true, "unsupported": true, "legacy": true, "time-bounded": true, "a,": true}

// sentence capitalises a help fragment ("watches at or above…") so it reads
// as a sentence under its key.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	// "PolicyID is the required identity…" loses its subject to the help
	// table; under the key the verb alone reads better dropped.
	for _, verb := range []string{"is ", "are "} {
		rest, ok := strings.CutPrefix(s, verb)
		if !ok || rest == "" {
			continue
		}
		// A value vocabulary ("etf or none: …") keeps its lower case; an
		// English opening is capitalised as usual.
		word, _, _ := strings.Cut(rest, " ")
		if !sentenceOpeners[word] {
			return rest
		}
		s = rest
		break
	}
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// cashSweepSizingKeys are the sweep numbers read from the file only: a
// missing one holds the sweep instead of falling back to a compiled value.
var cashSweepSizingKeys = map[string]bool{
	"max_order_notional": true, "max_order_pct_nlv": true, "min_order_notional": true,
	"reserve_floor_base": true, "reserve_pct_nlv": true, "keep_cash": true,
}
