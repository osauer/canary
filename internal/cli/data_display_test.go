package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestDataHealthPreservesEveryExistingDetailAndPage(t *testing.T) {
	at := time.Date(2026, 10, 5, 15, 10, 0, 0, time.UTC)
	r := rpc.DataHealthResult{SchemaVersion: 1, Revision: "0123456789abcdef01234567", AsOf: at, ValidUntil: at.Add(time.Minute), ScopeState: "current", Offset: 0, NextOffset: new(2), Summary: rpc.DataHealthSummary{State: "limited", Label: "Sources need attention", Total: 3, Required: 3, Current: 1, Limited: 1, Unverified: 1}, Concerns: []rpc.DataHealthConcern{{SourceID: "later", State: "unverified", Label: "Later page has no observation"}}, Sources: []rpc.DataSourceHealth{
		{ID: "first", Name: "First source", State: "current", Receiving: "Receiving usable prices", Detail: strings.Repeat("Retained detail ", 12) + "last fact", Access: &rpc.DataAccessObservation{Reason: "Live subscription unavailable", Code: 354, RetryAt: at}},
		{ID: "second", Name: "Second source", State: "limited", Receiving: "Waiting for publisher", Cause: rpc.DataHealthCauseUpstreamPublicationPending, Detail: "Prior publication remains usable", Action: "Wait for the next publication"},
	}}
	original, _ := json.Marshal(r)
	for _, width := range []string{"40", "80"} {
		t.Run(width, func(t *testing.T) {
			t.Setenv("COLUMNS", width)
			for _, color := range []bool{false, true} {
				var out bytes.Buffer
				env := &Env{Stdout: &out, Color: color}
				renderDataHealth(env, r)
				raw := stripDisplayANSI(out.String())
				flat := strings.Join(strings.Fields(raw), " ")
				for _, want := range []string{"Required 3 · current 1 · limited 1 · unavailable 0 · unverified 1", "2026-10-05 15:10:00 UTC", "2026-10-05 15:11:00 UTC", "First source: Receiving usable prices [current]", "last fact", "Live access: Live subscription unavailable (IBKR 354)", "retry 2026-10-05 15:10 UTC", "Second source: Waiting for publisher [limited]", "Prior publication remains usable", "Wait for the next publication", "Later page has no observation", "--offset 2", "--revision 0123456789abcdef01234567"} {
					if !strings.Contains(flat, want) {
						t.Fatalf("lost %q: %s", want, flat)
					}
				}
				if strings.Index(flat, "Later page") > strings.Index(flat, "First source:") || strings.Index(flat, "First source:") > strings.Index(flat, "Second source:") {
					t.Fatal("concern priority or producer order changed")
				}
				command := strings.ReplaceAll(raw, "\\\n", "")
				if !strings.Contains(strings.Join(strings.Fields(command), " "), "canary data health --offset 2 --revision 0123456789abcdef01234567") {
					t.Fatal("next-page command no longer copyable", raw)
				}
				for line := range strings.SplitSeq(out.String(), "\n") {
					if visibleLen(line) > outputColumns(env.Stdout) {
						t.Fatalf("exceeds width %s: %q", width, line)
					}
				}
				after, _ := json.Marshal(r)
				if !bytes.Equal(original, after) {
					t.Fatal("renderer mutated evidence")
				}
			}
		})
	}
	conn := &riskReadConn{result: r}
	var out, stderr bytes.Buffer
	if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &stderr}, "data", []string{"health", "--json"}); code != 0 {
		t.Fatal(stderr.String())
	}
	var got rpc.DataHealthResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || !reflect.DeepEqual(got, r) {
		t.Fatal("JSON evidence changed", err)
	}
}

func TestDataHealthExpectedPublisherWaitDoesNotBecomeAlarm(t *testing.T) {
	var out bytes.Buffer
	r := rpc.DataHealthResult{ScopeState: "current", Summary: rpc.DataHealthSummary{State: "limited", Label: "Awaiting publisher", Required: 1, Limited: 1, ExpectedDelays: 1}, Sources: []rpc.DataSourceHealth{{Name: "Publisher", State: "limited", Cause: rpc.DataHealthCauseUpstreamPublicationPending}}}
	renderDataHealth(&Env{Stdout: &out, Color: true}, r)
	if strings.Contains(out.String(), ansiYellow) || strings.Contains(out.String(), ansiRed) || !strings.Contains(out.String(), "not counted as source problems") {
		t.Fatal(out.String())
	}
}

func TestOpportunityDisplayPreservesRiskDetailsAndExactContract(t *testing.T) {
	t.Setenv("COLUMNS", "40")
	var out bytes.Buffer
	snap := &rpc.OpportunitySnapshot{Revision: "synthetic-revision", Opportunities: []rpc.Opportunity{{Key: "synthetic-opportunity", Bucket: "exercise", Symbol: "SYNTH", Action: "EXERCISE", Quantity: 2, Contract: rpc.ContractParams{Symbol: "SYNTH", SecType: "OPT", Expiry: "20351219", Right: "C", Strike: 100}, ExpectedGain: 12.5, ExpectedGainCurrency: "USD", PositionEffect: "increase_long", Details: []string{strings.Repeat("evidence ", 20) + "final evidence"}, Blockers: []rpc.TradingBlocker{{Code: "cash_missing", Message: "Cash not verified", Action: "Refresh cash evidence"}}}}}
	renderOpportunitiesText(&Env{Stdout: &out, Color: true}, snap)
	got := strings.Join(strings.Fields(stripDisplayANSI(out.String())), " ")
	for _, want := range []string{"SYNTH 20351219 100 C", "EXERCISE 2 contracts", "blocked", "synthetic-opportunity", "12.50", "increase_long", "final evidence", "Cash not verified", "Refresh cash evidence"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	for line := range strings.SplitSeq(out.String(), "\n") {
		if visibleLen(line) > 40 {
			t.Fatal("overflow", line)
		}
	}
}

func TestTechnicalDisplayPrefersPerRowCurrency(t *testing.T) {
	var out bytes.Buffer
	renderTechnicalText(&Env{Stdout: &out}, &rpc.TechnicalResult{Currency: "EUR", Rows: []rpc.TechnicalRow{{Symbol: "SPY", Currency: "USD", Price: new(100.0), DataQuality: "ok"}, {Symbol: "USD.JPY", Currency: "JPY", Price: new(150.0), DataQuality: "ok"}}})
	text := out.String()
	if !strings.Contains(text, "100.00 USD") || !strings.Contains(text, "150.00 JPY") {
		t.Fatal("per-row currency lost", text)
	}
}
