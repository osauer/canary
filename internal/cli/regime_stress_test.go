package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

type riskReadConn struct {
	calls  []string
	result any
	err    error
}

func (c *riskReadConn) Call(_ context.Context, method string, _ any, out any) error {
	c.calls = append(c.calls, method)
	if c.err != nil {
		return c.err
	}
	raw, err := json.Marshal(c.result)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func (*riskReadConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return errors.New("unexpected stream")
}

func TestRegimeReadPreservesDetailedAuthority(t *testing.T) {
	want := rpc.RegimeSnapshotResult{AuthorityHealth: &rpc.RegimeAuthorityHealth{Status: rpc.RegimeAuthorityStale, LastSuccessAt: new(time.Date(2026, 9, 4, 20, 0, 0, 0, time.UTC)), LastSuccessAgeSeconds: new(int64(60))}, VIXTermStructure: rpc.RegimeVIXTerm{Status: "stale", Ratio: new(1.1), RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "red", Eligibility: &rpc.RegimeEligibility{Eligible: false, Reasons: []string{"not current"}}}}, FundingStress: rpc.RegimeFundingStress{Status: "unavailable"}}
	conn := &riskReadConn{result: want}
	var out, stderr bytes.Buffer
	env := &Env{Conn: conn, Stdout: &out, Stderr: &stderr}
	if code := Run(t.Context(), env, "regime", []string{"--json"}); code != 0 {
		t.Fatal(code, stderr.String())
	}
	var got rpc.RegimeSnapshotResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(conn.calls, []string{rpc.MethodRegimeSnapshot}) {
		t.Fatal("regime evidence changed or wrong RPC")
	}
	out.Reset()
	renderRegime(env, want, true)
	for _, needle := range []string{"stale", "not eligible", "not current", "Funding", "unavailable", "VVIX", "Breadth", "USD/JPY", "Gamma", "HYG/SPY", "Credit spreads"} {
		if !strings.Contains(out.String(), needle) {
			t.Fatalf("missing %q: %s", needle, out.String())
		}
	}
}

func TestRiskReadsRejectUnsupportedInputsBeforeRPC(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		args []string
	}{{"regime", []string{"history"}}, {"stress", []string{"history"}}, {"regime", []string{"--force"}}, {"regime", []string{"--profiles"}}, {"stress", []string{"--submit"}}} {
		conn := &riskReadConn{}
		var out bytes.Buffer
		if Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, tc.cmd, tc.args) == 0 || len(conn.calls) != 0 {
			t.Fatalf("unsupported input reached RPC: %+v", tc)
		}
	}
}

func TestStressReadFailsOnDaemonErrorAndSanitizesHumanEvidence(t *testing.T) {
	conn := &riskReadConn{err: errors.New("account: unavailable")}
	var out bytes.Buffer
	env := &Env{Conn: conn, Stdout: &out, Stderr: &out}
	if Run(t.Context(), env, "stress", []string{"--json"}) == 0 || !reflect.DeepEqual(conn.calls, []string{rpc.MethodStressSnapshot}) {
		t.Fatal("a failed daemon stress read was accepted")
	}
	out.Reset()
	renderStress(env, rpc.StressResult{Summary: "unavailable\x1b[2J\nforged", Rows: []rpc.StressRow{{Title: "Margin", Evidence: "unknown"}}, MarketIndicators: []rpc.StressMarketIndicator{{Name: "Funding", Status: "n/a"}}, NotExecution: "Read-only"}, true)
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "\nforged") || !strings.Contains(out.String(), "unknown") || !strings.Contains(out.String(), "Funding") {
		t.Fatal("unsafe or lost evidence", out.String())
	}
}

func TestRegimeBreadthMissingIsNotMeasuredZero(t *testing.T) {
	for _, tc := range []struct {
		status   string
		coverage int
		want     string
	}{{rpc.RegimeStatusUnavailable, 0, "unavailable above 50-day average"}, {rpc.RegimeStatusComputing, 0, "unavailable above 50-day average"}, {rpc.RegimeStatusOK, 500, "0.0% above 50-day average"}, {rpc.RegimeStatusStale, 500, "0.0% above 50-day average"}} {
		var out bytes.Buffer
		renderRegime(&Env{Stdout: &out}, rpc.RegimeSnapshotResult{Breadth: rpc.RegimeBreadth{Status: tc.status, Envelope: rpc.BreadthSPXResult{Coverage50: tc.coverage}}}, false)
		if !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%+v: %s", tc, out.String())
		}
	}
}

func TestRegimeProfilesAreExplicitAndScalarsSurvive(t *testing.T) {
	for _, profiles := range []bool{false, true} {
		want := rpc.RegimeSnapshotResult{GammaZero: rpc.RegimeGammaZero{Envelope: rpc.GammaZeroSPXResult{Result: &rpc.GammaZeroComputed{SpotUnderlying: 100, Profile: []rpc.GammaProfilePoint{{Spot: 100, GEX: 2}}}}}}
		conn := &riskReadConn{result: want}
		var out bytes.Buffer
		args := []string{"--json"}
		if profiles {
			args = append(args, "--profiles")
		}
		if Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "regime", args) != 0 {
			t.Fatal(out.String())
		}
		var got rpc.RegimeSnapshotResult
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.GammaZero.Envelope.Result.SpotUnderlying != 100 || (len(got.GammaZero.Envelope.Result.Profile) > 0) != profiles {
			t.Fatal("profile selection lost scalar or failed opt-in")
		}
	}
}

func TestDegradedRiskReadsHaveHierarchyAndBoundedLines(t *testing.T) {
	at := time.Date(2026, 9, 5, 14, 23, 0, 0, time.Local)
	res := rpc.RegimeSnapshotResult{
		AuthorityHealth:  &rpc.RegimeAuthorityHealth{Status: rpc.RegimeAuthorityStale, LastSuccessAt: &at},
		VIXTermStructure: rpc.RegimeVIXTerm{Status: "ok", Ratio: new(0.8), RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green", AsOf: &rpc.RegimeAsOfSummary{Time: at, Source: "broker"}}},
		WarningDetails:   []rpc.RegimeWarning{{Code: "vvix_source", Message: "Separate DNS failure " + strings.Repeat("x", 200)}},
	}
	var out bytes.Buffer
	env := &Env{Stdout: &out, Stderr: &out}
	renderRegime(env, res, false)
	if strings.Contains(out.String(), "green") || !strings.Contains(out.String(), "recorded:") || strings.Contains(out.String(), "Separate DNS") {
		t.Fatal("retained evidence appears current or diagnostic leaked", out.String())
	}
	out.Reset()
	renderRegime(env, res, true)
	for _, want := range []string{"5 Sep 14:23", "Recorded band", "not a current rating", "Separate DNS", "    Observed"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	for line := range strings.SplitSeq(out.String(), "\n") {
		if visibleLen(line) > 80 {
			t.Fatalf("unwrapped line: %s", line)
		}
	}
	out.Reset()
	env.Conn = &riskReadConn{err: &rpc.Error{Code: rpc.CodeGatewayUnavailable, Message: strings.Repeat("private transport detail ", 20)}}
	if Run(t.Context(), env, "stress", nil) != 1 || !strings.Contains(out.String(), "Gateway unavailable") || strings.Contains(out.String(), "private transport") {
		t.Fatal("stress did not preserve failure with a compact explanation", out.String())
	}
	out.Reset()
	if Run(t.Context(), env, "stress", []string{"--details"}) != 1 || !strings.Contains(out.String(), "private transport") {
		t.Fatal("detail diagnostic missing")
	}
}

func TestBriefOverviewDefaultAndFullDetailRemainDistinct(t *testing.T) {
	res := rpc.BriefResult{Narrative: &rpc.BriefNarrative{
		Lead:     []rpc.BriefRun{{Text: "Full explanation"}},
		Overview: &rpc.BriefOverview{Assessment: []rpc.BriefRun{{Text: "Assessment incomplete."}}, Attention: []rpc.BriefParagraph{{Runs: []rpc.BriefRun{{Text: "Capital warning", Role: rpc.BriefRunRoleWatch}}}}, Coverage: []rpc.BriefParagraph{{Runs: []rpc.BriefRun{{Text: "Portfolio unavailable"}}}}},
	}}
	var out bytes.Buffer
	env := &Env{Conn: &riskReadConn{result: res}, Stdout: &out, Stderr: &out}
	if Run(t.Context(), env, "brief", nil) != 0 {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String(), "Full explanation") || !strings.Contains(out.String(), "Capital warning") || !strings.Contains(out.String(), "Portfolio unavailable") {
		t.Fatal(out.String())
	}
	out.Reset()
	if Run(t.Context(), env, "brief", []string{"--details"}) != 0 || !strings.Contains(out.String(), "Full explanation") {
		t.Fatal("full evidence unavailable", out.String())
	}
}

func TestRegimeMonitorCLIUsesSharedAuthorityProjection(t *testing.T) {
	want := rpc.RegimeSnapshotResult{AuthorityHealth: &rpc.RegimeAuthorityHealth{Status: rpc.RegimeAuthorityStale, LastSuccessAt: new(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)), LastSuccessAgeSeconds: new(int64(600))}, FundingStress: rpc.RegimeFundingStress{Status: "unavailable"}}
	conn := &riskReadConn{result: want}
	var out bytes.Buffer
	for _, args := range [][]string{{"--json", "--view", "bad"}, {"--json", "--view", "monitor", "--profiles"}, {"--view", "monitor"}} {
		if Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "regime", args) == 0 || len(conn.calls) != 0 {
			t.Fatal("invalid monitor request reached RPC")
		}
	}
	out.Reset()
	if Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "regime", []string{"--json", "--view", "monitor"}) != 0 {
		t.Fatal(out.String())
	}
	var got rpc.RegimeMonitorResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	expectedRaw, _ := json.Marshal(rpc.CompactRegimeMonitor(&want))
	var expected rpc.RegimeMonitorResult
	_ = json.Unmarshal(expectedRaw, &expected)
	if !reflect.DeepEqual(got, expected) {
		t.Fatal("CLI monitor invented or dropped authority")
	}
}
