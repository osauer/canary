package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// historyFakeConn answers regime.history / rules.history with canned
// results and records the decoded params, keeping renderer tests
// transport-free (briefFakeConn pattern).
type historyFakeConn struct {
	method       string
	regimeParams rpc.RegimeHistoryParams
	rulesParams  rpc.RulesHistoryParams
	regime       rpc.RegimeHistoryResult
	rules        rpc.RulesHistoryResult
}

func (c *historyFakeConn) Call(_ context.Context, method string, params, out any) error {
	c.method = method
	raw, _ := json.Marshal(params)
	var result any
	switch method {
	case rpc.MethodRegimeHistory:
		_ = json.Unmarshal(raw, &c.regimeParams)
		result = c.regime
	case rpc.MethodRulesHistory:
		_ = json.Unmarshal(raw, &c.rulesParams)
		result = c.rules
	default:
		result = struct{}{}
	}
	buf, _ := json.Marshal(result)
	return json.Unmarshal(buf, out)
}

func (*historyFakeConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func regimeHistoryFixture() rpc.RegimeHistoryResult {
	return rpc.RegimeHistoryResult{
		AsOf:  time.Date(2026, 7, 20, 14, 30, 0, 0, time.UTC),
		Since: time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 7, 20, 14, 30, 0, 0, time.UTC),
		Entries: []rpc.RegimeHistoryEntry{
			{
				At:                 time.Date(2026, 7, 20, 14, 2, 0, 0, time.UTC),
				Stage:              "calm",
				Severity:           "none",
				Verdict:            "Stable tape",
				ClusterYellow:      1,
				ClusterEligibleRed: 0,
			},
			{
				At:            time.Date(2026, 7, 19, 9, 30, 0, 0, time.UTC),
				Stage:         "early_warning",
				Severity:      "watch",
				Verdict:       "Stress signal present",
				ClusterRed:    2,
				ClusterYellow: 1,
			},
		},
		Count:      2,
		TotalCount: 312,
		Limit:      2,
		Truncated:  true,
		Index: rpc.HistoryIndexHealth{
			LastIngestAt:  time.Date(2026, 7, 20, 14, 2, 0, 0, time.UTC),
			IngestedBytes: 1000,
			JournalBytes:  1000,
		},
	}
}

func TestRunRegimeHistoryForwardsParamsAndRendersTable(t *testing.T) {
	t.Parallel()
	conn := &historyFakeConn{regime: regimeHistoryFixture()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	code := Run(context.Background(), env, "regime", []string{
		"history", "--since", "2026-07-14", "--until", "2026-07-20", "--stage", "calm", "--limit", "2",
	})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if conn.method != rpc.MethodRegimeHistory {
		t.Fatalf("method = %q, want %q", conn.method, rpc.MethodRegimeHistory)
	}
	p := conn.regimeParams
	if p.Since != "2026-07-14" || p.Until != "2026-07-20" || p.Stage != "calm" || p.Limit != 2 {
		t.Fatalf("params = %+v, want flags forwarded", p)
	}
	out := stdout.String()
	for _, want := range []string{
		"Regime history  2026-07-14 → 2026-07-20 UTC  2 of 312 rows (truncated; raise --limit)",
		"AT (UTC)",
		"R/Y(elig)",
		"2026-07-20 14:02  calm",
		"2/1(0)",
		"Stress signal present",
		"index: through 2026-07-20 14:02Z · journal fully ingested",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunRegimeHistoryJSONPassThrough(t *testing.T) {
	t.Parallel()
	conn := &historyFakeConn{regime: regimeHistoryFixture()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(context.Background(), env, "regime", []string{"history", "--json"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	var res rpc.RegimeHistoryResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("stdout is not the result envelope: %v\n%s", err, stdout.String())
	}
	if res.TotalCount != 312 || len(res.Entries) != 2 || !res.Truncated || res.Index.IngestedBytes != 1000 {
		t.Fatalf("envelope did not pass through verbatim: %+v", res)
	}
}

func TestRegimeHistoryBacklogFooter(t *testing.T) {
	t.Parallel()
	res := regimeHistoryFixture()
	res.Index.JournalBytes = res.Index.IngestedBytes + 2048
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderRegimeHistoryText(env, &stdout, &res)
	if !strings.Contains(stdout.String(), "index catching up: 2048 bytes behind (rows may be missing)") {
		t.Fatalf("backlog footer missing:\n%s", stdout.String())
	}
}

func TestRunRegimeHistoryRejectsTrailingArgs(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	if code := runRegimeHistory(context.Background(), env, []string{"history", "extra"}); code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "canary regime history") {
		t.Fatalf("stderr missing usage: %s", stderr.String())
	}
}

func TestRunRulesHistoryForwardsParamsAndRendersTable(t *testing.T) {
	t.Parallel()
	conn := &historyFakeConn{rules: rpc.RulesHistoryResult{
		AsOf:  time.Date(2026, 7, 20, 14, 30, 0, 0, time.UTC),
		Since: time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 7, 20, 14, 30, 0, 0, time.UTC),
		Entries: []rpc.RuleTransitionEntry{
			{
				At:            time.Date(2026, 7, 20, 14, 2, 0, 0, time.UTC),
				Rule:          "single_name_exposure",
				Status:        "act",
				Was:           "watch",
				Evidence:      "synthetic renderer evidence",
				PolicyID:      "rulebook-v1",
				PolicyVersion: 1,
			},
			{
				At:            time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC),
				Rule:          "cash_sell_only",
				Status:        "watch",
				PolicyID:      "rulebook-v1",
				PolicyVersion: 1,
			},
		},
		Count:      2,
		TotalCount: 2,
		Limit:      50,
		Index: rpc.HistoryIndexHealth{
			LastIngestAt:  time.Date(2026, 7, 20, 14, 2, 0, 0, time.UTC),
			IngestedBytes: 500,
			JournalBytes:  500,
		},
	}}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	code := Run(context.Background(), env, "rules", []string{"history", "--rule", "single_name_exposure", "--limit", "10"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if conn.method != rpc.MethodRulesHistory {
		t.Fatalf("method = %q, want %q", conn.method, rpc.MethodRulesHistory)
	}
	if conn.rulesParams.Rule != "single_name_exposure" || conn.rulesParams.Limit != 10 {
		t.Fatalf("params = %+v, want rule/limit forwarded", conn.rulesParams)
	}
	out := stdout.String()
	for _, want := range []string{
		"Rules history  2026-07-14 → 2026-07-20 UTC  2 of 2 rows",
		"WAS→STATUS",
		"watch→act",
		"single_name_exposure",
		"synthetic renderer evidence",
		"index: through 2026-07-20 14:02Z · journal fully ingested",
		"policy rulebook-v1 v1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	// A first observation (empty was) renders as the bare status.
	if !strings.Contains(out, "cash_sell_only") || strings.Contains(out, "→watch\n") {
		t.Fatalf("first-observation row rendering wrong:\n%s", out)
	}
}

func TestRulesHistoryPolicyFooterOmittedWhenMixed(t *testing.T) {
	t.Parallel()
	res := rpc.RulesHistoryResult{
		Entries: []rpc.RuleTransitionEntry{
			{Rule: "a", Status: "act", PolicyID: "rulebook-v1", PolicyVersion: 1},
			{Rule: "b", Status: "act", PolicyID: "rulebook-v2", PolicyVersion: 2},
		},
		Count: 2, TotalCount: 2,
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderRulesHistoryText(env, &stdout, &res)
	if strings.Contains(stdout.String(), "policy rulebook") {
		t.Fatalf("mixed policies must not render a uniform policy footer:\n%s", stdout.String())
	}
}

func TestRunRulesHistoryRejectsBadFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	// Through Run so flag hoisting applies, as in real invocations.
	if code := Run(context.Background(), env, "rules", []string{"history", "--nope"}); code != 2 {
		t.Fatalf("exit=%d, want 2 (flag parse error)", code)
	}
}
