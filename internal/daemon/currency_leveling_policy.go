package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// currencyLevelingWrittenDefaults are the values the template and policy
// ensure write into the file: off by default, the owner switches their own
// file on (owner decisions 2026-10-05 21:28 CEST and 2026-10-06 06:10 CEST);
// band 10,000, cushion 250, limit within 2 bp of the mid (owner decisions
// 2026-10-06 05:47–05:50 CEST); a conversion pays back within 30 days (owner
// decision 2026-10-06 07:11 CEST, "all six confirmed"). They are never read
// at runtime: a number missing from the file holds leveling at
// needs_your_number.
var currencyLevelingWrittenDefaults = []cashSweepWrittenDefault{
	{"enabled", "false"},
	{"trigger_base", "10000.0"},
	{"cushion_base", "250.0"},
	{"max_slippage_bp", "2.0"},
	{"payback_days", "30"},
}

// currencyLevelingDefaultPaybackDays is the payback_days policy ensure
// writes.
const currencyLevelingDefaultPaybackDays = 30

// currencyLevelingMaxPaybackDays bounds payback_days: a conversion that
// needs more than a year to pay for itself is not a repayment.
const currencyLevelingMaxPaybackDays = 365

// defaultCurrencyLevelingPolicy is the table the template writes, as the
// embedded default carries it so a fresh file reads as Canary's defaults.
// It is off, so its numbers decide nothing.
func defaultCurrencyLevelingPolicy() *protectionCurrencyLevelingPolicy {
	return &protectionCurrencyLevelingPolicy{Enabled: false, TriggerBase: new(10000.0), CushionBase: new(250.0), MaxSlippageBP: new(2.0), PaybackDays: new(currencyLevelingDefaultPaybackDays)}
}

// enabled reports whether the table is present and switched on.
func (p *protectionCurrencyLevelingPolicy) enabled() bool {
	return p != nil && p.Enabled
}

// currencyLevelingNumber is one number the file must carry.
type currencyLevelingNumber struct {
	key string
	v   *float64
}

// numbers lists the file's decimal numbers by key, in the order the template
// writes them; payback_days, a whole number, is handled beside them.
func (p *protectionCurrencyLevelingPolicy) numbers() []currencyLevelingNumber {
	return []currencyLevelingNumber{{"trigger_base", p.TriggerBase}, {"cushion_base", p.CushionBase}, {"max_slippage_bp", p.MaxSlippageBP}}
}

// missingNumbers names the keys an enabled bucket still needs from the
// owner; every currency holds until they are written.
func (p *protectionCurrencyLevelingPolicy) missingNumbers() []string {
	if !p.enabled() {
		return nil
	}
	var out []string
	for _, f := range p.numbers() {
		if f.v == nil {
			out = append(out, f.key)
		}
	}
	if p.PaybackDays == nil {
		out = append(out, "payback_days")
	}
	return out
}

// deliberateCarry reports whether the owner keeps ccy negative on purpose.
func (p *protectionCurrencyLevelingPolicy) deliberateCarry(ccy string) bool {
	return p != nil && p.Currency[ccy].DeliberateCarry
}

// currencyLevelingMaxSlippageBP bounds max_slippage_bp: a limit further than
// 1% from the mid is no longer a bounded conversion, whatever the file says.
const currencyLevelingMaxSlippageBP = 100.0

// validateCurrencyLevelingPolicy checks the table when it is present. A
// written value must be well formed even while disabled, so a bad number
// cannot sit dormant behind an enabled/version flip; an unwritten number is
// not an error but needs_your_number.
func validateCurrencyLevelingPolicy(prefix string, p *protectionCurrencyLevelingPolicy) error {
	if p == nil {
		return nil
	}
	for _, f := range p.numbers() {
		if f.v == nil {
			continue
		}
		v := *f.v
		switch {
		case !finiteProtectionOptionPolicyValue(v) || v < 0:
			return fmt.Errorf("%s.%s must be finite and nonnegative", prefix, f.key)
		case (f.key == "trigger_base" || f.key == "max_slippage_bp") && v == 0:
			return fmt.Errorf("%s.%s must be positive", prefix, f.key)
		case f.key == "max_slippage_bp" && v > currencyLevelingMaxSlippageBP:
			return fmt.Errorf("%s.max_slippage_bp must be at most %g basis points", prefix, currencyLevelingMaxSlippageBP)
		}
	}
	if d := p.PaybackDays; d != nil && (*d < 1 || *d > currencyLevelingMaxPaybackDays) {
		return fmt.Errorf("%s.payback_days must be from 1 to %d days", prefix, currencyLevelingMaxPaybackDays)
	}
	for _, ccy := range slices.Sorted(maps.Keys(p.Currency)) {
		if len(ccy) != 3 || strings.IndexFunc(ccy, func(r rune) bool { return r < 'A' || r > 'Z' }) >= 0 {
			return fmt.Errorf("%s.currency.%s: the table name must be a three-letter ISO currency code in capitals", prefix, ccy)
		}
	}
	return nil
}

// writeCurrencyLevelingTemplate appends the table as Canary writes it: off,
// with the numbers policy ensure writes.
func writeCurrencyLevelingTemplate(b *strings.Builder) {
	b.WriteString(`
# Currency leveling: a currency whose trade-date cash is negative is a margin
# loan IBKR never repays on its own. Once enabled, leveling repays each loan
# beyond the band back to between zero and the cushion, never more, from the
# currencies whose cash earns least, and only from a currency that earns less
# than the loan costs, at the rates in the broker's daily statements. A
# conversion must earn back its worst-case cost within payback_days. A loan
# repaid from several currencies is one approval. Each conversion is a CASH
# LMT DAY order on IDEALPRO, priced from a live bid and ask; one loan's
# conversions together are held to the order cap in force; never
# pre-authorised. Every number is read from this file only: a missing one
# holds leveling at needs_your_number.
[buckets.currency_leveling]
`)
	for _, d := range currencyLevelingWrittenDefaults {
		fmt.Fprintf(b, "%s = %s\n", d.key, d.value)
	}
	b.WriteString(`# A currency you keep negative on purpose, for example a funding leg:
# [buckets.currency_leveling.currency.USD]
# deliberate_carry = true
`)
}

// materialiseCurrencyLeveling writes the table into an owner file: the
// whole table, off, when the file has none; into an existing plain
// [buckets.currency_leveling] section only the keys it leaves out (an
// unwritten enabled already reads false, so writing it switches nothing on).
func materialiseCurrencyLeveling(doc *tomlDoc, md toml.MetaData, release string) (changes, notes []string) {
	const table = "buckets.currency_leveling"
	comment := "  # written by Canary " + release + "; currency leveling reads it from this file only"
	if !md.IsDefined("buckets", "currency_leveling") {
		lines := []string{"# Currency leveling, written off by Canary " + release + ": set enabled = true to start it."}
		for _, d := range currencyLevelingWrittenDefaults {
			lines = append(lines, d.key+" = "+d.value)
		}
		doc.insert(table, lines)
		return []string{"added [buckets.currency_leveling] with enabled = false"}, nil
	}
	var missing []cashSweepWrittenDefault
	for _, d := range currencyLevelingWrittenDefaults {
		if !md.IsDefined("buckets", "currency_leveling", d.key) {
			missing = append(missing, d)
		}
	}
	switch {
	case len(missing) == 0:
		return nil, nil
	case doc.headerLine(table) < 0:
		return nil, []string{"[buckets.currency_leveling] is not a plain section, so its missing keys were not written; leveling holds until you write them"}
	}
	for _, d := range missing {
		doc.insert(table, []string{d.key + " = " + d.value + comment})
		changes = append(changes, fmt.Sprintf("added buckets.currency_leveling.%s = %s", d.key, d.value))
	}
	return changes, nil
}

// currencyLevelingMaterialisationPreserves reports whether after differs
// from before only by what materialiseCurrencyLeveling writes: the whole
// default table where there was none, or the written defaults for keys
// before left out, every other value kept.
func currencyLevelingMaterialisationPreserves(before, after *protectionCurrencyLevelingPolicy) bool {
	if after == nil {
		return before == nil
	}
	want := defaultCurrencyLevelingPolicy()
	if before == nil {
		return !after.Enabled && len(after.Currency) == 0 && after.PaybackDays != nil && *after.PaybackDays == *want.PaybackDays &&
			slices.EqualFunc(after.numbers(), want.numbers(), func(a, b currencyLevelingNumber) bool { return a.v != nil && b.v != nil && *a.v == *b.v })
	}
	if after.Enabled != before.Enabled || !maps.Equal(after.Currency, before.Currency) {
		return false
	}
	switch {
	case after.PaybackDays == nil:
		return false
	case before.PaybackDays != nil && *after.PaybackDays != *before.PaybackDays:
		return false
	case before.PaybackDays == nil && *after.PaybackDays != *want.PaybackDays:
		return false
	}
	for i, b := range before.numbers() {
		a := after.numbers()[i]
		switch {
		case a.v == nil:
			return false
		case b.v != nil && *a.v != *b.v:
			return false
		case b.v == nil && *a.v != *want.numbers()[i].v:
			return false
		}
	}
	return true
}

// currencyLevelingNeedsYourNumber lists, for canary policy status, what an
// enabled table still needs from the owner. An absent or disabled table asks
// for nothing.
func currencyLevelingNeedsYourNumber(p *protectionCurrencyLevelingPolicy) []string {
	if missing := p.missingNumbers(); len(missing) > 0 {
		return []string{"currency leveling: holds until you write " + strings.Join(missing, ", ") + " in [buckets.currency_leveling]"}
	}
	return nil
}

// currencyLevelingDisplayCurrencies are printed by policy show even when the
// file has no table for them, so the deliberate_carry key stays visible.
var currencyLevelingDisplayCurrencies = []string{"EUR", "USD"}

// currencyLevelingNumberKey reports whether key is one of the numbers read
// from the file only.
func currencyLevelingNumberKey(key string) bool {
	return key == "payback_days" || slices.ContainsFunc((&protectionCurrencyLevelingPolicy{}).numbers(), func(n currencyLevelingNumber) bool { return n.key == key })
}
