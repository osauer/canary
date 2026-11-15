package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// The account view prints the broker's rate per currency and side, a real
// zero as 0.00%, and an unavailable side with its reason; it prints nothing
// while no row carries rates.
func TestRenderAccountCashInterestRates(t *testing.T) {
	a := &rpc.AccountResult{BaseCurrency: "EUR",
		BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", CashCcy: -30000, ExchangeRate: 1},
		CurrencyExposure:   []rpc.CurrencyExposure{{Currency: "USD", NetLiquidationCcy: 52000, CashCcy: 52000, ExchangeRate: 0.85, NetLiquidationBase: 44200}}}
	var buf bytes.Buffer
	env := &Env{Stdout: &buf, Stderr: &buf}
	renderCashInterestTo(env, &buf, a)
	if buf.Len() != 0 {
		t.Fatalf("rates printed without evidence:\n%s", buf.String())
	}
	a.BaseCurrencyLedger.Interest = &rpc.CashInterestRates{Source: rpc.CashInterestSourceFlexTiers,
		Credit: rpc.CashInterestSide{State: rpc.CashInterestStateUnavailable, Reason: "no EUR credit row in the broker's statements: the account held no EUR cash on a statement day"},
		Debit:  rpc.CashInterestSide{State: rpc.CashInterestStateAvailable, Rate: new(0.03482), Tier: "1", ValueDate: "2026-10-09"}}
	a.CurrencyExposure[0].Interest = &rpc.CashInterestRates{Source: rpc.CashInterestSourceFlexTiers,
		Credit: rpc.CashInterestSide{State: rpc.CashInterestStateAvailable, Rate: new(0.0), Tier: "1", ValueDate: "2026-10-09"},
		Debit:  rpc.CashInterestSide{State: rpc.CashInterestStateUnavailable, Reason: "the latest USD debit row is dated 2026-10-02, 5 business days ago, so the live rate is unknown", LastValueDate: "2026-10-02"}}
	renderCashInterestTo(env, &buf, a)
	out := buf.String()
	for _, want := range []string{
		"Cash interest  (broker rates from the Flex statement's Interest Details (Tiers))",
		"EUR    credit unavailable (no EUR credit row in the broker's statements: the account held no EUR cash on a statement day) · debit 3.48% a year (tier 1, value date 2026-10-09)",
		"USD    credit 0.00% a year (tier 1, value date 2026-10-09) · debit unavailable (the latest USD debit row is dated 2026-10-02, 5 business days ago, so the live rate is unknown)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "EUR ") > strings.Index(out, "USD ") {
		t.Fatalf("the base currency is not first:\n%s", out)
	}
}
