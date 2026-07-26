package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

type rulesFakeConn struct {
	result rpc.RulesResult
}

func (c *rulesFakeConn) Call(_ context.Context, method string, _ any, out any) error {
	if method != rpc.MethodRulesSnapshot {
		return nil
	}
	raw, _ := json.Marshal(c.result)
	return json.Unmarshal(raw, out)
}

func (*rulesFakeConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestRunRulesRendersTerminalContractAsExemptNotPass(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn := &rulesFakeConn{result: rpc.RulesResult{
		AsOf: now, Enabled: true, Status: "ok", PolicyID: "rulebook-v2", PolicyVersion: 2,
		Rules: []risk.RuleRow{{
			ID: risk.RuleCatalystCoverage, Number: 6, Title: "Option outlives its catalyst",
			Status: risk.RuleStatusNotEvaluated, Reason: risk.EarningsReasonTerminalNonReporting,
			Evidence: "1 exact terminal/non-reporting contract has no future issuer earnings catalyst.",
			Exempt:   []risk.RuleOffender{{Symbol: "EXAMPLEQ", Note: "exact contract is verified terminal/non-reporting"}},
		}},
		Ranked:       []int{0},
		BreachCounts: map[string]int{risk.RuleStatusNotEvaluated: 1},
		Earnings: []rpc.EarningsInfo{{
			Symbol: "EXAMPLEQ", Source: "verified_terminal", Status: rpc.EarningsStatusTerminalNonReporting,
			Terminal: &rpc.EarningsTerminalInfo{RevalidateAfter: now.AddDate(1, 0, 0)},
		}},
	}}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(context.Background(), env, "rules", nil); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"--     6 catalyst_coverage",
		"exempt: EXAMPLEQ — exact contract is verified terminal/non-reporting",
		"1 not evaluated",
		"Earnings not applicable: EXAMPLEQ (terminal/non-reporting; review by 2027-07-21)",
		"exact-contract evidence and provenance are available in --json",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"All 1 rules pass", "Earnings unresolved:"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("output unexpectedly contains %q:\n%s", forbidden, out)
		}
	}
}

func TestRunRulesJSONPreservesHeadlineTier(t *testing.T) {
	observed, threshold := 15.5, 15.0
	conn := &rulesFakeConn{result: rpc.RulesResult{
		AsOf: time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC), Enabled: true, Status: "ok",
		PolicyID: "rulebook-v2", PolicyVersion: 2,
		Rules: []risk.RuleRow{{
			ID: risk.RuleOptionLinePremium, Number: 2, Title: "Single option line premium",
			Status: risk.RuleStatusWatch, Unit: "% NLV", Observed: &observed, Threshold: &threshold,
			Evidence: "A hedge line drives the 15%/25% tier.",
		}},
		Ranked:       []int{0},
		BreachCounts: map[string]int{risk.RuleStatusWatch: 1},
	}}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(context.Background(), env, "rules", []string{"--json"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}

	var got rpc.RulesResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if len(got.Rules) != 1 || got.Rules[0].Observed == nil || *got.Rules[0].Observed != observed ||
		got.Rules[0].Threshold == nil || *got.Rules[0].Threshold != threshold {
		t.Fatalf("JSON lost headline tier: %+v", got.Rules)
	}
}

func TestRunRulesRendersBrokerNonIssuerSeparatelyFromTerminalAndUnresolved(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	conn := &rulesFakeConn{result: rpc.RulesResult{
		AsOf: now, Enabled: true, Status: "ok", PolicyID: "rulebook-v2", PolicyVersion: 2,
		Rules: []risk.RuleRow{{
			ID: risk.RuleCatalystCoverage, Number: 6, Title: "Option outlives its catalyst",
			Status: risk.RuleStatusNotEvaluated, Reason: risk.EarningsReasonBrokerNonIssuer,
			Evidence: "exact broker identity exempts this security",
			Exempt:   []risk.RuleOffender{{Symbol: "SYNTH1", Note: "exact broker identity proves a nonissuer security"}},
		}},
		Ranked:       []int{0},
		BreachCounts: map[string]int{risk.RuleStatusNotEvaluated: 1},
		InputHealth: []rpc.SourceHealth{{
			Source: "earnings", Status: rpc.SourceStatusOK,
			Notes: []string{"retained broker identity issue: code=contract_unavailable stage=wsh_contract_resolve retry=scheduled"},
		}},
		Earnings: []rpc.EarningsInfo{{
			Symbol: "SYNTH1", Source: "broker_identity", Status: rpc.EarningsStatusNotApplicable,
			Identity: &rpc.EarningsIdentityInfo{Outcome: "not_applicable", NotApplicable: true, AttemptedAt: now},
		}},
	}}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(context.Background(), env, "rules", nil); code != 0 {
		t.Fatal("rules renderer failed")
	}
	out := stdout.String()
	if !strings.Contains(out, "Issuer earnings not applicable: SYNTH1 (broker-proven nonissuer)") ||
		!strings.Contains(out, "input earnings  ok (informational)") || !strings.Contains(out, "contract_unavailable") ||
		strings.Contains(out, "Earnings unresolved:") || strings.Contains(out, "terminal/non-reporting") {
		t.Fatal("broker nonissuer was not rendered as its distinct resolved class")
	}
}

func TestRunRulesRendersOnlyExactWSHEntitlementNotice(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	makeResult := func(provider, code, stage string, retryable bool) rpc.RulesResult {
		return rpc.RulesResult{AsOf: now, Enabled: true, Status: "ok", PolicyID: "rulebook-v2", PolicyVersion: 2,
			Earnings: []rpc.EarningsInfo{{Symbol: "SYNTH1", Source: "unknown", Status: rpc.EarningsStatusNoDatePublished,
				Providers: []rpc.EarningsProviderInfo{{Provider: provider, LastFailure: &rpc.SourceFailure{Code: code, Stage: stage, Retryable: retryable}}},
			}},
		}
	}
	for _, tc := range []struct {
		name string
		res  rpc.RulesResult
		want bool
	}{
		{"exact metadata", makeResult("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHMetadata, false), true},
		{"exact event", makeResult("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHEvent, false), true},
		{"wrong provider", makeResult("nasdaq", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHMetadata, false), false},
		{"wrong code", makeResult("ibkr_wsh", rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageWSHMetadata, false), false},
		{"wrong stage", makeResult("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHContractResolve, false), false},
		{"retryable", makeResult("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHMetadata, true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &rulesFakeConn{result: tc.res}
			var stdout, stderr bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
			if code := Run(context.Background(), env, "rules", nil); code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, stderr.String())
			}
			out := stdout.String()
			got := strings.Contains(out, "Wall Street Horizon earnings feed")
			if got != tc.want {
				t.Fatalf("notice=%t want=%t output=%s", got, tc.want, out)
			}
			if strings.Contains(out, "ibkr_wsh") || strings.Contains(out, "invalid_payload") {
				t.Fatalf("renderer exposed typed provider internals: %s", out)
			}
		})
	}
}
