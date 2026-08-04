package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRenderAccountTypedFieldsDistinguishObservedZeroFromMissing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)
	result := &rpc.AccountResult{
		AccountID: "DU123", BaseCurrency: "USD", AsOf: now,
		NetLiquidation: 0, TotalCash: 0,
		Authority: &rpc.AccountDataAuthority{
			Scope:  rpc.AccountDataScope{AccountID: "DU123", AccountMode: rpc.AccountModePaper},
			Source: rpc.AccountDataSourceAccountSummaryRequest, Availability: rpc.AccountDataAvailable,
			Freshness: rpc.AccountDataFreshnessCurrent, AsOf: now,
			Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, NetLiquidation: true, TotalCash: false},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderAccountText(env, result)
	out := stdout.String()
	if !strings.Contains(out, "Net liquidation") || !strings.Contains(accountOutputLine(out, "Net liquidation"), "$ 0.00") {
		t.Fatalf("observed zero net liquidation did not render as a number:\n%s", out)
	}
	if line := accountOutputLine(out, "Total cash"); !strings.Contains(line, "—") || strings.Contains(line, "$ 0.00") {
		t.Fatalf("missing total cash did not render as unavailable: %q\n%s", line, out)
	}
}

func TestRenderAccountMissingBaseCurrencyNeverFallsBackToUSD(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)
	result := &rpc.AccountResult{
		AccountID: "DU123", NetLiquidation: 100_000, AsOf: now,
		Authority: &rpc.AccountDataAuthority{
			Scope:  rpc.AccountDataScope{AccountID: "DU123", AccountMode: rpc.AccountModePaper},
			Source: rpc.AccountDataSourceAccountSummaryRequest, Availability: rpc.AccountDataAvailable,
			Freshness: rpc.AccountDataFreshnessCurrent, AsOf: now,
			Fields: &rpc.AccountFieldAvailability{BaseCurrency: false, NetLiquidation: true},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderAccountText(env, result)
	out := stdout.String()
	if !strings.Contains(out, "base=—") {
		t.Fatalf("missing base currency not named in header:\n%s", out)
	}
	if line := accountOutputLine(out, "Net liquidation"); strings.Contains(line, "$") || !strings.Contains(line, "—") {
		t.Fatalf("missing base currency was formatted as USD: %q\n%s", line, out)
	}
}

func TestRenderAccountNamesCachedUnknownFreshness(t *testing.T) {
	t.Parallel()
	result := &rpc.AccountResult{
		AccountID: "DU123", BaseCurrency: "EUR",
		Authority: &rpc.AccountDataAuthority{
			Scope:  rpc.AccountDataScope{AccountID: "DU123", AccountMode: rpc.AccountModePaper},
			Source: rpc.AccountDataSourceAccountUpdatesCache, Availability: rpc.AccountDataAvailable,
			Freshness: rpc.AccountDataFreshnessUnknown, Reason: rpc.AccountDataReasonUnstampedCache,
			Fields: &rpc.AccountFieldAvailability{BaseCurrency: true},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderAccountText(env, result)
	if out := stdout.String(); !strings.Contains(out, "showing cached account values") || !strings.Contains(out, "observation time is unknown") {
		t.Fatalf("cached freshness note missing:\n%s", out)
	}
}

func TestRenderPositionsUnavailableEmptyDoesNotClaimNoPositions(t *testing.T) {
	t.Parallel()
	result := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{}, Options: []rpc.PositionView{},
		Authority: &rpc.AccountDataAuthority{
			Scope:  rpc.AccountDataScope{AccountID: "DU123", AccountMode: rpc.AccountModePaper},
			Source: rpc.AccountDataSourcePortfolioStream, Availability: rpc.AccountDataUnavailable,
			Freshness: rpc.AccountDataFreshnessUnknown, Reason: rpc.AccountDataReasonUnprimed,
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, result)
	out := stdout.String()
	if !strings.Contains(out, "Positions unavailable") || strings.Contains(out, "No open positions") {
		t.Fatalf("unprimed empty projection became a clean-book claim:\n%s", out)
	}
	if !strings.Contains(out, "initial portfolio download is not complete") {
		t.Fatalf("unprimed reason missing:\n%s", out)
	}
}

func TestRenderPositionsStaleEmptyDoesNotClaimNoPositions(t *testing.T) {
	t.Parallel()
	result := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{}, Options: []rpc.PositionView{},
		Authority: &rpc.AccountDataAuthority{
			Scope:  rpc.AccountDataScope{AccountID: "DU123", AccountMode: rpc.AccountModePaper},
			Source: rpc.AccountDataSourcePortfolioStream, Availability: rpc.AccountDataUnavailable,
			Freshness: rpc.AccountDataFreshnessStale, Reason: rpc.AccountDataReasonReceiptStale,
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, result)
	out := stdout.String()
	if !strings.Contains(out, "Positions unavailable") || strings.Contains(out, "No open positions") {
		t.Fatalf("stale empty projection became a clean-book claim:\n%s", out)
	}
}

func accountOutputLine(output, label string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, label) {
			return line
		}
	}
	return ""
}
