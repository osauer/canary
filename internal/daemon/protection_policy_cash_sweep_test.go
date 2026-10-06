package daemon

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

const cashSweepPolicyHead = `
kind = "canary.protection_policy"
schema_version = 1
policy_id = "protection-mvp"
policy_version = 7
[authority]
close_reduce_only = true
auto_submit = false
`

// The sweep carries no default that acts: the embedded default has no
// cash_sweep table, `canary policy default protection` shows it only as a
// commented placeholder, a nil table leaves the fingerprint alone, and a
// written table without enabled is present, disabled and in shadow.
func TestCashSweepAbsentFromDefaultAndDisabledByDefault(t *testing.T) {
	if defaultProtectionPolicy().Cash.Sweep != nil {
		t.Fatal("embedded default carries a cash_sweep table")
	}
	raw, err := DefaultPolicyTOML("protection")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.Contains(line, "cash_sweep") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Fatalf("canary policy default protection writes the sweep live: %q", line)
		}
	}
	for _, want := range []string{"# [cash.sweep]", "# [cash.sweep.currency.EUR]", "# max_order_notional = 0.0", "# max_maturity_days = 182", "# min_maturity_days = 28"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("the template does not show %q:\n%s", want, raw)
		}
	}
	parsed, _, err := parseProtectionPolicy(raw)
	if err != nil || parsed.Cash.Sweep != nil {
		t.Fatalf("template parses to sweep %+v, err %v", parsed.Cash.Sweep, err)
	}
	before := fingerprintProtectionPolicy(defaultProtectionPolicy())
	withNil := defaultProtectionPolicy()
	withNil.Cash.Sweep = nil
	if fingerprintProtectionPolicy(withNil).Key != before.Key {
		t.Fatal("nil cash_sweep changed the protection policy fingerprint")
	}

	p, _, err := parseProtectionPolicy([]byte(cashSweepPolicyHead + "[cash.sweep]\nmax_order_notional = 20000\n"))
	if err != nil {
		t.Fatalf("table without enabled must load: %v", err)
	}
	bucket := p.Cash.Sweep
	if bucket == nil || bucket.enabled() || bucket.effectiveMode() != rpc.CashSweepModeShadow || bucket.missingNumbers() != nil {
		t.Fatalf("written table = %+v; want present, disabled, shadow", bucket)
	}
	if fingerprintProtectionPolicy(p).Key == before.Key {
		t.Fatal("a written cash_sweep table must enter the fingerprint")
	}
	var nilBucket *protectionCashSweepPolicy
	if nilBucket.enabled() || nilBucket.effectiveMode() != rpc.CashSweepModeShadow || nilBucket.missingNumbers() != nil {
		t.Fatal("a nil sweep must read disabled and shadow")
	}
}

// The compiled declarations follow S1, S3, O2 and O3.
func TestCashSweepCompiledDefaultsPerCurrency(t *testing.T) {
	for ccy, want := range map[string]struct {
		instruments []string
		fallback    string
		maxDays     int
	}{
		"USD": {[]string{"us_tbill"}, "", 91},
		"EUR": {[]string{"de_bubill", "fr_btf"}, "etf", 182},
		"GBP": {[]string{"uk_tbill"}, "", 91},
		"CAD": {[]string{"ca_tbill"}, "", 91},
		"CHF": {[]string{"none"}, "", 91},
		"JPY": {[]string{"none"}, "", 91},
		"SEK": {[]string{"none"}, "", 91},
	} {
		c := defaultCashSweepCurrency(ccy)
		if !slices.Equal(c.Instruments, want.instruments) || c.Fallback != want.fallback || c.MaxMaturityDays != want.maxDays ||
			c.KeepCash != nil || c.MinTranche != nil || c.MinMaturityDays != 28 || c.LadderRungs != 4 {
			t.Fatalf("%s default = %+v", ccy, c)
		}
		if err := validateCashSweepCurrency("cash_sweep.currency."+ccy, ccy, c); err != nil {
			t.Fatalf("%s default does not validate: %v", ccy, err)
		}
	}
	// EUR names its fallback ETF's symbol and exchange as the owner's numbers
	// (O3); every other default needs nothing.
	if got := defaultCashSweepCurrency("EUR").missingNumbers(); !slices.Equal(got, []string{"etf_symbol", "etf_exchange"}) {
		t.Fatalf("EUR missing = %v", got)
	}
	if got := defaultCashSweepCurrency("USD").missingNumbers(); got != nil {
		t.Fatalf("USD missing = %v", got)
	}
	// An enabled sweep needs every sizing number from the file (owner
	// decision 2026-10-05 18:35 CEST); compiled values are never read.
	enabled := &protectionCashSweepPolicy{Enabled: true}
	if got := enabled.missingNumbers(); !slices.Equal(got, []string{"max_order_notional", "max_order_pct_nlv", "min_order_notional", "reserve_floor_base", "reserve_pct_nlv", "order_step_base", "no_buy_while_borrowed"}) {
		t.Fatalf("enabled sweep missing = %v", got)
	}
}

// A written currency table keeps what the owner wrote and takes Canary's
// instrument and ladder defaults for the keys it leaves out; keep_cash comes
// from the file only (the currency's own, else the bucket's); tax_reviewed_at
// reads a TOML date or a quoted one to the same value.
func TestCashSweepWrittenCurrencyTableFillsDefaults(t *testing.T) {
	p, _, err := parseProtectionPolicy([]byte(cashSweepPolicyHead + `
[cash.sweep]
enabled = true
mode = "Active"
max_order_notional = 25000
tax_reviewed_at = 2026-09-30

[cash.sweep.currency.EUR]
keep_cash = 8000
etf_symbol = "BBB"
etf_exchange = "IBIS"

[cash.sweep.currency.USD]
fallback = "none"
ladder_rungs = 2
`))
	if err != nil {
		t.Fatal(err)
	}
	bucket := p.Cash.Sweep
	if bucket.effectiveMode() != rpc.CashSweepModeActive || bucket.TaxReviewedAt != "2026-09-30" || bucket.MaxOrderNotional != 25000 {
		t.Fatalf("bucket = %+v", bucket)
	}
	eur := bucket.currency("EUR")
	if eur.KeepCash == nil || *eur.KeepCash != 8000 || !slices.Equal(eur.Instruments, []string{"de_bubill", "fr_btf"}) || eur.Fallback != "etf" || eur.MaxMaturityDays != 182 ||
		eur.MinMaturityDays != 28 || eur.MinTranche != nil || eur.LadderRungs != 4 || eur.missingNumbers() != nil {
		t.Fatalf("EUR = %+v", eur)
	}
	usd := bucket.currency("USD")
	if usd.Fallback != "none" || usd.LadderRungs != 2 || usd.KeepCash != nil || !slices.Equal(usd.Instruments, []string{"us_tbill"}) {
		t.Fatalf("USD = %+v", usd)
	}
	if keep, ok := bucket.keepCash("USD"); ok || keep != 0 {
		t.Fatalf("USD keep_cash without a written value = %v, %v; want none", keep, ok)
	}
	if keep, ok := bucket.keepCash("EUR"); !ok || keep != 8000 {
		t.Fatalf("EUR keep_cash = %v, %v", keep, ok)
	}
	if chf := bucket.currency("CHF"); !slices.Equal(chf.Instruments, []string{"none"}) {
		t.Fatalf("CHF without a table = %+v", chf)
	}

	quoted, _, err := parseProtectionPolicy([]byte(cashSweepPolicyHead + "[cash.sweep]\ntax_reviewed_at = \"2026-09-30\"\n"))
	if err != nil || quoted.Cash.Sweep.TaxReviewedAt != "2026-09-30" {
		t.Fatalf("quoted date = %+v, err %v", quoted.Cash.Sweep, err)
	}
	if fingerprintProtectionPolicy(quoted).Key == "" {
		t.Fatal("no fingerprint")
	}
	for _, bad := range []string{"tax_reviewed_at = 2026-09-30T10:00:00Z", "tax_reviewed_at = \"30.09.2026\"", "tax_reviewed_at = 20260930"} {
		if _, _, err := parseProtectionPolicy([]byte(cashSweepPolicyHead + "[cash.sweep]\n" + bad + "\n")); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	// A misspelt key is refused like any other protection key.
	if _, _, err := parseProtectionPolicy([]byte(cashSweepPolicyHead + "[cash.sweep.currency.USD]\nkeep_cahs = 1\n")); err == nil || !strings.Contains(err.Error(), "keep_cahs") {
		t.Fatalf("misspelt key: %v", err)
	}
}

// Validation refuses anything outside the closed vocabulary or its currency,
// and a malformed written value even while the sweep is disabled.
func TestCashSweepValidation(t *testing.T) {
	base := func() protectionPolicy {
		p := defaultProtectionPolicy()
		p.Cash.Sweep = &protectionCashSweepPolicy{Enabled: true, MaxOrderNotional: 25000, Currency: map[string]protectionCashSweepCurrency{
			"USD": defaultCashSweepCurrency("USD"), "EUR": defaultCashSweepCurrency("EUR"),
		}}
		return p
	}
	if err := validateProtectionPolicy(base()); err != nil {
		t.Fatalf("well-formed sweep rejected: %v", err)
	}
	for name, tc := range map[string]struct {
		change func(*protectionCashSweepPolicy)
		want   string
	}{
		"mode":                {func(b *protectionCashSweepPolicy) { b.Mode = "hard" }, "mode"},
		"notional negative":   {func(b *protectionCashSweepPolicy) { b.MaxOrderNotional = -1 }, "max_order_notional"},
		"notional NaN":        {func(b *protectionCashSweepPolicy) { b.MaxOrderNotional = math.NaN() }, "max_order_notional"},
		"tax date":            {func(b *protectionCashSweepPolicy) { b.TaxReviewedAt = "soon" }, "tax_reviewed_at"},
		"lower-case currency": {func(b *protectionCashSweepPolicy) { b.Currency["usd"] = defaultCashSweepCurrency("USD") }, "ISO currency code"},
		"bill of another ccy": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "EUR", func(c *protectionCashSweepCurrency) { c.Instruments = []string{"us_tbill"} })
		}, "not an instrument for EUR"},
		"unknown instrument": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "USD", func(c *protectionCashSweepCurrency) { c.Instruments = []string{"money_market"} })
		}, "not an instrument for USD"},
		"conversion": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "CHF", func(c *protectionCashSweepCurrency) { c.Instruments = []string{"us_tbill"} })
		}, "not an instrument for CHF"},
		"duplicate": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "EUR", func(c *protectionCashSweepCurrency) { c.Instruments = []string{"fr_btf", "fr_btf"} })
		}, "twice"},
		"none with others": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "USD", func(c *protectionCashSweepCurrency) { c.Instruments = []string{"us_tbill", "none"} })
		}, "none stands alone"},
		"no instruments": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "USD", func(c *protectionCashSweepCurrency) { c.Instruments = nil })
		}, "at least one"},
		"bad fallback": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "EUR", func(c *protectionCashSweepCurrency) { c.Fallback = "fr_btf" })
		}, "fallback"},
		"fallback repeats etf": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "EUR", func(c *protectionCashSweepCurrency) { c.Instruments = []string{"etf"} })
		}, "repeats"},
		"fallback from none": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "EUR", func(c *protectionCashSweepCurrency) { c.Instruments = []string{"none"} })
		}, "fall back from"},
		"symbol without etf": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "USD", func(c *protectionCashSweepCurrency) { c.ETFSymbol = "AAA" })
		}, "neither an instrument nor the fallback"},
		"symbol free text": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "EUR", func(c *protectionCashSweepCurrency) { c.ETFSymbol = "buy all now" })
		}, "etf_symbol"},
		"keep_cash negative": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "USD", func(c *protectionCashSweepCurrency) { c.KeepCash = new(-1.0) })
		}, "keep_cash"},
		"tranche zero": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "USD", func(c *protectionCashSweepCurrency) { c.MinTranche = new(0.0) })
		}, "min_tranche"},
		"min maturity zero": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "USD", func(c *protectionCashSweepCurrency) { c.MinMaturityDays = 0 })
		}, "maturities"},
		"max above ceiling": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "USD", func(c *protectionCashSweepCurrency) { c.MaxMaturityDays = 398 })
		}, "397"},
		"min above max": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "USD", func(c *protectionCashSweepCurrency) { c.MinMaturityDays = 92 })
		}, "maturities"},
		"no rungs": {func(b *protectionCashSweepPolicy) {
			setSweepCcy(b, "USD", func(c *protectionCashSweepCurrency) { c.LadderRungs = 0 })
		}, "ladder_rungs"},
	} {
		t.Run(name, func(t *testing.T) {
			p := base()
			tc.change(p.Cash.Sweep)
			err := validateProtectionPolicy(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
	// Disabled: a malformed written value is still refused.
	disabled := base()
	disabled.Cash.Sweep.Enabled = false
	setSweepCcy(disabled.Cash.Sweep, "USD", func(c *protectionCashSweepCurrency) { c.MaxMaturityDays = 500 })
	if err := validateProtectionPolicy(disabled); err == nil {
		t.Fatal("malformed maturity accepted while disabled")
	}
	// The ETF may be a primary instrument of any currency.
	etf := base()
	setSweepCcy(etf.Cash.Sweep, "CHF", func(c *protectionCashSweepCurrency) {
		c.Instruments, c.ETFSymbol, c.ETFExchange = []string{"etf"}, "AAA", "EBS"
	})
	if err := validateProtectionPolicy(etf); err != nil {
		t.Fatalf("a CHF ETF declaration rejected: %v", err)
	}
	// close_reduce_only stays mandatory: the sweep is an exception, not a
	// relaxation of the authority block.
	open := base()
	open.Authority.CloseReduceOnly = false
	if err := validateProtectionPolicy(open); err == nil {
		t.Fatal("close_reduce_only = false accepted beside the sweep")
	}
}

func setSweepCcy(b *protectionCashSweepPolicy, ccy string, change func(*protectionCashSweepCurrency)) {
	c := b.currency(ccy)
	change(&c)
	if b.Currency == nil {
		b.Currency = map[string]protectionCashSweepCurrency{}
	}
	b.Currency[ccy] = c
}

// canary policy status names what an enabled sweep still needs; an absent or
// disabled sweep asks for nothing.
func TestCashSweepPolicyStatusNeedsYourNumber(t *testing.T) {
	if got := cashSweepNeedsYourNumber(nil); got != nil {
		t.Fatalf("absent sweep asks for %v", got)
	}
	if got := cashSweepNeedsYourNumber(&protectionCashSweepPolicy{}); got != nil {
		t.Fatalf("disabled sweep asks for %v", got)
	}
	got := strings.Join(cashSweepNeedsYourNumber(&protectionCashSweepPolicy{Enabled: true, Mode: "active"}), "\n")
	for _, want := range []string{"max_order_notional", "reserve_pct_nlv", "keep_cash", "EUR fallback ETF needs etf_symbol, etf_exchange", "bills still plan", "tax_reviewed_at"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	eur := defaultCashSweepCurrency("EUR")
	eur.ETFSymbol, eur.ETFExchange = "BBB", "IBIS"
	written := &protectionCashSweepPolicy{Enabled: true, MaxOrderNotional: 1, TaxReviewedAt: "2026-09-30", Currency: map[string]protectionCashSweepCurrency{"EUR": eur},
		MaxOrderPctNLV: new(10.0), MinOrderNotional: new(1.0), ReserveFloorBase: new(0.0), ReservePctNLV: new(10.0), OrderStepBase: new(1000.0), KeepCash: new(5000.0), NoBuyWhileBorrowed: new(true)}
	if got := cashSweepNeedsYourNumber(written); got != nil {
		t.Fatalf("a complete sweep asks for %v", got)
	}
}
