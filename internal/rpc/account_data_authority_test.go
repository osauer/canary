package rpc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAccountAuthorityJSONEmitsFalseFieldAvailability(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)
	result := AccountResult{
		AccountID: "DU123", BaseCurrency: "USD", NetLiquidation: 0, TotalCash: 0, AsOf: now,
		Authority: &AccountDataAuthority{
			Scope:  AccountDataScope{AccountID: "DU123", AccountMode: AccountModePaper},
			Source: AccountDataSourceAccountSummaryRequest, Availability: AccountDataAvailable,
			Freshness: AccountDataFreshnessCurrent, AsOf: now,
			Fields: &AccountFieldAvailability{BaseCurrency: true, NetLiquidation: true, TotalCash: false},
		},
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	text := string(wire)
	for _, want := range []string{
		`"authority":{"scope":{"account_id":"DU123","account_mode":"paper"}`,
		`"net_liquidation":true`,
		`"total_cash":false`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("wire missing %s: %s", want, text)
		}
	}
}

func TestCompactPositionsRiskPreservesAuthority(t *testing.T) {
	t.Parallel()
	authority := &AccountDataAuthority{
		Scope:  AccountDataScope{AccountID: "DU123", AccountMode: AccountModePaper},
		Source: AccountDataSourcePortfolioStream, Availability: AccountDataUnavailable,
		Freshness: AccountDataFreshnessStale, Reason: AccountDataReasonReceiptStale,
	}
	got := CompactPositionsRisk(&PositionsResult{Authority: authority}, 5)
	if got.Authority != authority || got.Authority.Availability != AccountDataUnavailable || got.Authority.Reason != AccountDataReasonReceiptStale {
		t.Fatalf("compact positions dropped authority: %+v", got.Authority)
	}
}
