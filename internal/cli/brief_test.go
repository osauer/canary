package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRenderBriefTwoMovementsAndDegradation(t *testing.T) {
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := rpc.BriefResult{
		AsOf: time.Date(2026, 7, 18, 8, 0, 0, 0, time.Local), BriefFingerprint: "sha256:abcdef",
		Review: rpc.BriefReviewSection{
			SessionPnL:    rpc.BriefAccountRow{BriefRowState: rpc.BriefRowState{Status: "unavailable", Detail: "account down"}},
			Attribution:   rpc.BriefMoversRow{BriefRowState: rpc.BriefRowState{Status: "unavailable", Detail: "positions down"}},
			RulesDelta:    rpc.BriefRulesDeltaRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "no delta baseline yet"}},
			Proposals:     rpc.BriefProposalsRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "no proposals"}, Offered: 2, Acted: 1},
			Overrides:     rpc.BriefOverridesRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "none"}},
			CapitalEvents: rpc.BriefCapitalEventsRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "no capital events"}},
			Reconcile:     rpc.BriefReconcileRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "never"}},
			AutoExtend:    rpc.BriefAutoExtendRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "none"}},
			OneTap:        rpc.BriefOneTapRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "blocked"}},
			WorkingOrders: rpc.BriefCountRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "journal"}},
		},
		Ready: rpc.BriefReadySection{
			Regime:        rpc.BriefRegimeRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "gateway unavailable"}},
			Breadth:       rpc.BriefBreadthRow{BriefRowState: rpc.BriefRowState{Status: "unavailable", Detail: "cold"}},
			Gamma:         rpc.BriefGammaRow{BriefRowState: rpc.BriefRowState{Status: "unavailable", Detail: "cold"}},
			Stress:        rpc.BriefStressRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "partial"}},
			Session:       rpc.BriefSessionRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "official"}},
			Capital:       rpc.BriefCapitalRow{BriefRowState: rpc.BriefRowState{Status: "attention", Detail: "block tier breached"}, Tier: "block", Enforcement: "shadow"},
			Latch:         rpc.BriefLatchRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "open"}},
			PremiumAtRisk: rpc.BriefMoneyCoverageRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "nil values excluded"}},
			HedgeCost:     rpc.BriefMoneyCoverageRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "nil greeks excluded"}},
			PolicyDrift:   rpc.BriefPolicyDriftRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "match"}},
			Artefacts:     rpc.BriefArtefactsRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "declared"}},
		},
	}
	renderBrief(env, res)
	got := stdout.String()
	for _, want := range []string{"Review  (last completed session)", "Ready  (today)", "session P&L", "by underlying", "proposals", "capital events", "gateway unavailable", "nil greeks excluded", "no delta baseline yet", "attention", "tier block · enforcement shadow", "2 offered · 1 acted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("brief render missing %q:\n%s", want, got)
		}
	}
	var regimeLine string
	for line := range strings.SplitSeq(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "regime ") {
			regimeLine = line
			break
		}
	}
	if regimeLine == "" || !strings.HasSuffix(regimeLine, " —") || strings.Contains(regimeLine, "·") {
		t.Fatalf("empty regime stage and verdict must render an em dash, got %q:\n%s", regimeLine, got)
	}
}

func TestBriefHumanOriginClassification(t *testing.T) {
	if !briefHumanOrigin(rpc.OrderOriginHumanTTY) || !briefHumanOrigin(rpc.OrderOriginPairedDevice) {
		t.Fatal("human origins must be stamp-capable")
	}
	for _, origin := range []string{"", rpc.OrderOriginAgent, "unknown"} {
		if briefHumanOrigin(origin) {
			t.Fatalf("origin %q unexpectedly stamp-capable", origin)
		}
	}
}

func TestRunBriefTextHumanStampsRenderedFingerprint(t *testing.T) {
	snapshot := rpc.BriefResult{
		AsOf:             time.Date(2026, 7, 18, 8, 0, 0, 0, time.Local),
		BriefFingerprint: "sha256:rendered", StampTarget: rpc.BriefKindMorning,
	}
	conn := &briefFakeConn{snapshot: snapshot}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn, Origin: rpc.OrderOriginHumanTTY}
	if code := runBrief(context.Background(), env, nil); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := conn.calls[0]
	if got.method != rpc.MethodBriefSnapshot {
		t.Fatalf("first method=%q", got.method)
	}
	got = conn.calls[1]
	if got.method != rpc.MethodBriefAck || got.ack.Kind != rpc.BriefKindMorning ||
		got.ack.BriefFingerprint != snapshot.BriefFingerprint || got.ack.Origin != rpc.OrderOriginHumanTTY {
		t.Fatalf("ack call=%+v", got)
	}
	if !strings.Contains(stdout.String(), "stamp: morning artefact for 2026-07-18") {
		t.Fatalf("missing stamp receipt:\n%s", stdout.String())
	}
}

func TestRunBriefJSONAndAgentTextNeverStamp(t *testing.T) {
	snapshot := rpc.BriefResult{
		AsOf:             time.Date(2026, 7, 18, 8, 0, 0, 0, time.Local),
		BriefFingerprint: "sha256:rendered", StampTarget: rpc.BriefKindMorning,
	}
	for _, tc := range []struct {
		name   string
		args   []string
		origin string
		want   string
	}{
		{name: "json", args: []string{"--json"}, origin: rpc.OrderOriginHumanTTY, want: `"brief_fingerprint": "sha256:rendered"`},
		{name: "agent text", origin: rpc.OrderOriginAgent, want: "agent-origin render — not stamped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &briefFakeConn{snapshot: snapshot}
			var stdout, stderr bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn, Origin: tc.origin}
			if code := runBrief(context.Background(), env, tc.args); code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, stderr.String())
			}
			if len(conn.calls) != 1 || conn.calls[0].method != rpc.MethodBriefSnapshot {
				t.Fatalf("calls=%+v", conn.calls)
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Fatalf("stdout missing %q:\n%s", tc.want, stdout.String())
			}
		})
	}
}

func TestRunBriefMonthlyTargetNeverAcknowledgesFromCLI(t *testing.T) {
	snapshot := rpc.BriefResult{
		AsOf:             time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		BriefFingerprint: "sha256:monthly", StampTarget: rpc.BriefKindMonthly,
		Ready: rpc.BriefReadySection{MonthlyPulse: &rpc.BriefMonthlyPulseRow{
			Status: rpc.BriefMonthlyPulseDue, Month: "2026-08",
		}},
	}
	for _, origin := range []string{rpc.OrderOriginHumanTTY, rpc.OrderOriginAgent, rpc.OrderOriginPairedDevice} {
		conn := &briefFakeConn{snapshot: snapshot}
		var stdout, stderr bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn, Origin: origin}
		if code := runBrief(context.Background(), env, nil); code != 0 {
			t.Fatalf("origin=%s exit=%d stderr=%s", origin, code, stderr.String())
		}
		if len(conn.calls) != 1 || conn.calls[0].method != rpc.MethodBriefSnapshot {
			t.Fatalf("origin=%s calls=%+v, CLI must never complete monthly", origin, conn.calls)
		}
		want := "paired-device origin required"
		if origin == rpc.OrderOriginAgent {
			want = "agent-origin render — not stamped"
		}
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("origin=%s missing %q:\n%s", origin, want, stdout.String())
		}
	}
}

func TestRunBriefAckFailureIsLoudAndAdvisory(t *testing.T) {
	conn := &briefFakeConn{
		snapshot: rpc.BriefResult{
			AsOf:             time.Date(2026, 7, 18, 8, 0, 0, 0, time.Local),
			BriefFingerprint: "sha256:rendered", StampTarget: rpc.BriefKindMorning,
		},
		ackErr: errors.New("journal unavailable"),
	}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn, Origin: rpc.OrderOriginHumanTTY}
	if code := runBrief(context.Background(), env, nil); code != 0 {
		t.Fatalf("ack failure exit=%d, want advisory success", code)
	}
	if !strings.Contains(stderr.String(), "brief rendered but stamp failed: journal unavailable") {
		t.Fatalf("stamp failure not reported loudly: %s", stderr.String())
	}
}

type briefCLICall struct {
	method string
	ack    rpc.BriefAckParams
}

type briefFakeConn struct {
	snapshot rpc.BriefResult
	calls    []briefCLICall
	ackErr   error
}

func (c *briefFakeConn) Call(_ context.Context, method string, params, out any) error {
	call := briefCLICall{method: method}
	var result any = c.snapshot
	if method == rpc.MethodBriefAck {
		raw, _ := json.Marshal(params)
		_ = json.Unmarshal(raw, &call.ack)
		c.calls = append(c.calls, call)
		if c.ackErr != nil {
			return c.ackErr
		}
		result = rpc.BriefAckResult{OK: true, Kind: call.ack.Kind, Day: "2026-07-18"}
	}
	if method != rpc.MethodBriefAck {
		c.calls = append(c.calls, call)
	}
	raw, _ := json.Marshal(result)
	return json.Unmarshal(raw, out)
}

func (*briefFakeConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}
