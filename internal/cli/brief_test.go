package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			LastSession:   rpc.BriefLastSessionRow{BriefRowState: rpc.BriefRowState{Status: "unavailable", Detail: "not captured for 2026-07-17"}, SessionDate: "2026-07-17"},
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
	for _, want := range []string{"Review  (since the last close)", "Ready  (today)", "session P&L", "by underlying", "proposals", "capital events", "gateway unavailable", "nil greeks excluded", "no delta baseline yet", "attention", "tier block · enforcement shadow", "2 offered · 1 acted", "last session close", "2026-07-17 · not captured"} {
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

// ---- narrative render -------------------------------------------------
//
// internal/daemon/brief_narrative.go owns every word of the brief's prose.
// The fixtures below copy sentences from that composer's own table cases
// (internal/daemon/brief_narrative_test.go) so the CLI can pin only what the
// CLI owns: the projection of typed runs onto a terminal — wrapping, tints,
// the sign-off line, the degraded footer, and the fallback boundary. A
// composer copy edit may make a golden read oddly; it can never make the
// projection wrong, and TestRenderBriefNarrativeIsPureProjection is the
// assertion that survives such an edit.

func briefText(text string) rpc.BriefRun { return rpc.BriefRun{Text: text} }
func briefFig(text string) rpc.BriefRun {
	return rpc.BriefRun{Text: text, Role: rpc.BriefRunRoleFigure}
}
func briefWatch(text string) rpc.BriefRun {
	return rpc.BriefRun{Text: text, Role: rpc.BriefRunRoleWatch}
}
func briefAct(text string) rpc.BriefRun { return rpc.BriefRun{Text: text, Role: rpc.BriefRunRoleAct} }

func briefPara(runs ...rpc.BriefRun) rpc.BriefParagraph { return rpc.BriefParagraph{Runs: runs} }

// briefNarrativeCase is one payload state: the composed prose plus the rows it
// was composed from, because the sign-off line and the degraded footer read
// the rows, not the prose.
type briefNarrativeCase struct {
	name string
	res  rpc.BriefResult
}

func briefQuietCase() briefNarrativeCase {
	return briefNarrativeCase{name: "quiet", res: rpc.BriefResult{
		Narrative: &rpc.BriefNarrative{
			Lead: []rpc.BriefRun{briefText("Stress reads stand down at observe severity. Regime stage quiet, verdict Normal regime. Nothing across Review and Ready needs a decision.")},
			Review: []rpc.BriefParagraph{briefPara(
				briefText("Daily P/L stands at "), briefFig("EUR +2,340"),
				briefText(" on equity of "), briefFig("EUR 1,250,000"),
				briefText(". By name in EUR: AAPL "), briefFig("+900.00"),
				briefText(", NVDA "), briefFig("+800.40"),
				briefText(", SPY "), briefFig("+639.60"),
				briefText(", and 2 other names at "), briefFig("-120.00"), briefText("."),
			)},
			Ready: []rpc.BriefParagraph{
				briefPara(briefText("Breadth has "), briefFig("62.0%"), briefText(" above the 50-DMA and "),
					briefFig("58.0%"), briefText(" above the 200-DMA, net new highs "), briefFig("+1.4%"), briefText(".")),
				briefPara(briefText("Process folds clean: policy pins match, cadence artefacts are declared with 1 of 2 complete, the monthly pulse is not due, and no held-name events.")),
			},
			Coda: []rpc.BriefRun{briefText("Nothing owed before the bell.")},
		},
	}}
}

func briefWatchCase() briefNarrativeCase {
	return briefNarrativeCase{name: "watch", res: rpc.BriefResult{
		Narrative: &rpc.BriefNarrative{
			Lead: []rpc.BriefRun{
				briefText("Stress reads "), briefWatch("reduce risk at watch severity"),
				briefText(". 3 rows need a decision: "), briefWatch("overrides"), briefText(", "),
				briefWatch("held-name earnings"), briefText(", "), briefWatch("protection proposals"), briefText("."),
			},
			Review: []rpc.BriefParagraph{briefPara(
				briefWatch("1 active override widens policy controls: hedge_coverage."),
				briefText(" No rule transitions."),
			)},
			Ready: []rpc.BriefParagraph{
				briefPara(briefWatch("2 held names carry earnings context: AAPL, NVDA."),
					briefText(" The remaining held-name event source is clean.")),
				briefPara(briefWatch("2 protection proposals are ready to act"), briefText(", with 1 more blocked.")),
			},
			Coda: []rpc.BriefRun{
				briefText("Owed before the bell: "), briefWatch("overrides"), briefText(", "),
				briefWatch("held-name earnings"), briefText(", "), briefWatch("protection proposals"),
				briefText(". Everything else holds."),
			},
		},
	}}
}

func briefActCase() briefNarrativeCase {
	return briefNarrativeCase{name: "act", res: rpc.BriefResult{
		Narrative: &rpc.BriefNarrative{
			Lead: []rpc.BriefRun{
				briefText("Stress reads "), briefAct("de-risk at act severity"),
				briefText(". 4 rows need a decision: "), briefAct("rules delta"), briefText(", "),
				briefAct("capital events"), briefText(", "), briefAct("capital"), briefText(", "),
				briefAct("drawdown latch"), briefText(". Regime stage quiet, verdict Normal regime."),
			},
			Review: []rpc.BriefParagraph{
				briefPara(briefAct("1 rule worsened to act since the last stamped brief: hedge coverage.")),
				briefPara(briefAct("The drawdown latch engaged this episode and remains open until a human reset."),
					briefText(" It engaged at 30.4% consumed.")),
			},
			Ready: []rpc.BriefParagraph{
				briefPara(briefAct("Capital sits in the block tier"), briefText(" under shadow enforcement with "),
					briefFig("118.0%"), briefText(" of the drawdown budget consumed. The drawdown latch is engaged, 2 days old, and remains so until a human reset.")),
				briefPara(briefText("No protection proposals are staged.")),
			},
			Coda: []rpc.BriefRun{
				briefText("Owed before the bell: "), briefAct("rules delta"), briefText(", "),
				briefAct("capital events"), briefText(", "), briefAct("capital"), briefText(", "),
				briefAct("drawdown latch"), briefText(". Everything else holds."),
			},
		},
	}}
}

// briefDegradedCase is the disclosure case: the prose names each unread input
// and the rows carry the reason the prose never states. The row states are the
// composer test's own degraded payload.
func briefDegradedCase() briefNarrativeCase {
	unavailable := func(detail string) rpc.BriefRowState {
		return rpc.BriefRowState{Status: rpc.BriefStatusUnavailable, Detail: detail}
	}
	return briefNarrativeCase{name: "degraded", res: rpc.BriefResult{
		Review: rpc.BriefReviewSection{
			SessionPnL:    rpc.BriefAccountRow{BriefRowState: unavailable("account summary unavailable: broker down")},
			Attribution:   rpc.BriefMoversRow{BriefRowState: unavailable("positions unavailable")},
			Proposals:     rpc.BriefProposalsRow{BriefRowState: unavailable("proposal outcome journal is unavailable")},
			WorkingOrders: rpc.BriefCountRow{BriefRowState: unavailable("open-orders journal unavailable")},
		},
		Ready: rpc.BriefReadySection{
			Regime:        rpc.BriefRegimeRow{BriefRowState: unavailable("regime snapshot unavailable")},
			Breadth:       rpc.BriefBreadthRow{BriefRowState: unavailable("breadth snapshot unavailable")},
			Gamma:         rpc.BriefGammaRow{BriefRowState: rpc.BriefRowState{Status: rpc.BriefStatusDegraded, Detail: "dealer gamma source is stale"}},
			Capital:       rpc.BriefCapitalRow{BriefRowState: unavailable("risk constitution absent")},
			PremiumAtRisk: rpc.BriefMoneyCoverageRow{BriefRowState: unavailable("positions unavailable")},
			HedgeCost:     rpc.BriefMoneyCoverageRow{BriefRowState: unavailable("positions unavailable")},
			Proposals:     rpc.BriefReadyProposalsRow{BriefRowState: unavailable("protection proposal snapshot is unavailable")},
		},
		Narrative: &rpc.BriefNarrative{
			Lead: []rpc.BriefRun{briefText("The regime read is unavailable. Nothing across Review and Ready needs a decision. 12 inputs could not be read and are named below: session P/L, attribution, proposals, working orders, regime, breadth, dealer gamma, held-name events, capital, premium at risk, hedge cost, protection proposals.")},
			Review: []rpc.BriefParagraph{
				briefPara(briefText("Account P/L is unavailable, so the session cannot be summarized. Per-name attribution is unavailable.")),
				briefPara(briefText("The proposal outcome journal is unavailable. The open-orders journal is unavailable.")),
			},
			Ready: []rpc.BriefParagraph{
				briefPara(briefText("Breadth is unavailable, so participation cannot be stated. Dealer gamma is degraded, so the spot-to-zero-gamma relationship cannot be stated.")),
				briefPara(briefText("The risk constitution is absent, so capital controls are unapproved and the drawdown budget cannot be stated. Premium at risk is unavailable. Hedge cost is unavailable.")),
			},
			Coda: []rpc.BriefRun{briefText("Nothing on the desk needs a decision, but 12 inputs could not be read: unknown is not clean.")},
		},
	}}
}

func briefNarrativeCases() []briefNarrativeCase {
	return []briefNarrativeCase{briefQuietCase(), briefWatchCase(), briefActCase(), briefDegradedCase()}
}

// renderBriefNarrativeCase renders one state at an explicit measure. Width is
// never read from the environment here, so a wide terminal cannot reflow a
// golden.
func renderBriefNarrativeCase(tc briefNarrativeCase, color bool, width int) string {
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: color}
	renderBriefNarrative(env, servedBriefNarrative(tc.res.Narrative), tc.res, width)
	return stdout.String()
}

// briefSpan is one run of terminal text under a single SGR code.
type briefSpan struct{ code, text string }

// briefLineSpans splits one rendered line into spans and reports whether the
// line ended with an SGR still open, which would leak the attribute past the
// newline.
func briefLineSpans(line string) ([]briefSpan, bool) {
	var spans []briefSpan
	code, text := "", strings.Builder{}
	flush := func() {
		if text.Len() > 0 {
			spans = append(spans, briefSpan{code: code, text: text.String()})
			text.Reset()
		}
	}
	for i := 0; i < len(line); {
		if line[i] != '\x1b' {
			text.WriteByte(line[i])
			i++
			continue
		}
		end := strings.IndexByte(line[i:], 'm')
		if end < 0 {
			text.WriteByte(line[i])
			i++
			continue
		}
		flush()
		if code = line[i : i+end+1]; code == ansiReset {
			code = ""
		}
		i += end + 1
	}
	flush()
	return spans, code != ""
}

// briefCodeText reconstructs, per SGR code, the text the render placed under
// it, undoing the wrap: a span that ends a line and a span of the same code
// opening the next line were one composed run before wrapping.
func briefCodeText(output string) map[string]string {
	out := map[string]string{}
	last, have := "", false
	for line := range strings.SplitSeq(strings.TrimSuffix(output, "\n"), "\n") {
		spans, _ := briefLineSpans(strings.TrimPrefix(line, briefProseIndent))
		for i, span := range spans {
			sep := "\n"
			switch {
			case out[span.code] == "":
				sep = ""
			case i == 0 && have && span.code == last:
				sep = " "
			}
			out[span.code] += sep + span.text
		}
		last, have = "", false
		if len(spans) > 0 {
			last, have = spans[len(spans)-1].code, true
		}
	}
	return out
}

// briefRunTexts is every run text in reading order, which is exactly what the
// renderer may print and nothing more.
func briefRunTexts(narrative *rpc.BriefNarrative) []string {
	view := servedBriefNarrative(narrative)
	paragraphs := [][]rpc.BriefRun{view.lead}
	paragraphs = append(paragraphs, view.review...)
	paragraphs = append(paragraphs, view.ready...)
	paragraphs = append(paragraphs, view.coda)
	out := make([]string, 0, len(paragraphs))
	for _, runs := range paragraphs {
		var text strings.Builder
		for _, run := range runs {
			text.WriteString(run.Text)
		}
		if text.Len() > 0 {
			out = append(out, text.String())
		}
	}
	return out
}

// TestRenderBriefNarrativeIsPureProjection is the assertion that outlives any
// composer copy edit: unwrapped, the rendered prose is the composed text
// exactly — no word added, none dropped, none reordered.
func TestRenderBriefNarrativeIsPureProjection(t *testing.T) {
	t.Parallel()
	for _, tc := range briefNarrativeCases() {
		for _, width := range []int{40, 60, 80, 200} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, width), func(t *testing.T) {
				t.Parallel()
				view := servedBriefNarrative(tc.res.Narrative)
				paragraphs := [][]rpc.BriefRun{view.lead}
				paragraphs = append(paragraphs, view.review...)
				paragraphs = append(paragraphs, view.ready...)
				paragraphs = append(paragraphs, view.coda)
				for i, runs := range paragraphs {
					var stdout bytes.Buffer
					env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
					briefProseParagraph(env, runs, width)
					got := strings.Join(strings.Fields(stdout.String()), " ")
					var text strings.Builder
					for _, run := range runs {
						text.WriteString(run.Text)
					}
					want := strings.Join(strings.Fields(text.String()), " ")
					if got != want {
						t.Fatalf("paragraph %d is not a pure projection\n got: %s\nwant: %s", i, got, want)
					}
				}
			})
		}
	}
}

func briefWords1Line(text string) string { return strings.Join(strings.Fields(text), " ") }

// TestRenderBriefNarrativeTints pins the terminal register: watch is amber,
// act is red, figures and body prose keep the terminal's own ink, and no
// escape survives a line break. Every paragraph is checked whole and in both
// directions, so a tint that leaks one word past a run boundary fails here.
func TestRenderBriefNarrativeTints(t *testing.T) {
	t.Parallel()
	for _, tc := range briefNarrativeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			view := servedBriefNarrative(tc.res.Narrative)
			paragraphs := [][]rpc.BriefRun{view.lead}
			paragraphs = append(paragraphs, view.review...)
			paragraphs = append(paragraphs, view.ready...)
			paragraphs = append(paragraphs, view.coda)
			for i, runs := range paragraphs {
				var stdout bytes.Buffer
				env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
				briefProseParagraph(env, runs, 80)
				// Runs that share a code and sit next to each other render as
				// one span — figures carry no code of their own, so a figure
				// inside plain prose must not open one.
				want, previous := map[string][]string{}, "\x00"
				for _, run := range runs {
					code := briefRunSGR(run.Role)
					if code == previous {
						want[code][len(want[code])-1] += run.Text
					} else {
						want[code] = append(want[code], run.Text)
					}
					previous = code
				}
				got := briefCodeText(stdout.String())
				for code, texts := range want {
					text := strings.Join(texts, " ")
					if briefWords1Line(got[code]) != briefWords1Line(text) {
						t.Errorf("paragraph %d, code %q\n got: %s\nwant: %s", i, code, got[code], text)
					}
				}
				for code := range got {
					if _, ok := want[code]; !ok {
						t.Errorf("paragraph %d carries an unexpected SGR code %q: %s", i, code, got[code])
					}
				}
			}
			// Headers, the sign-off line and the disclosure footer are the
			// CLI's own chrome and carry no tint at all.
			output := renderBriefNarrativeCase(tc, true, 80)
			for line := range strings.SplitSeq(strings.TrimSuffix(output, "\n"), "\n") {
				if _, open := briefLineSpans(line); open {
					t.Errorf("line leaks an open SGR past the newline: %q", line)
				}
			}
		})
	}
}

// TestRenderBriefNarrativeChromeIsNeverTinted keeps the CLI's own lines out of
// the composed register: only run roles may colour anything.
func TestRenderBriefNarrativeChromeIsNeverTinted(t *testing.T) {
	t.Parallel()
	tc := briefDegradedCase()
	tc.res.Review.OneTap.Signable = true
	output := renderBriefNarrativeCase(tc, true, 80)
	prose, footer, split := strings.Cut(output, "\nDegraded inputs\n")
	if !split {
		t.Fatal("a degraded payload must disclose its rows")
	}
	if strings.Contains(footer, "\x1b") {
		t.Errorf("the disclosure footer carries an escape:\n%s", footer)
	}
	for line := range strings.SplitSeq(prose, "\n") {
		chrome := strings.HasPrefix(line, "Review  (") || strings.HasPrefix(line, "Ready  (") ||
			strings.HasPrefix(line, briefProseIndent+"one-tap sign-off:")
		if chrome && strings.Contains(line, "\x1b") {
			t.Errorf("chrome line carries an escape: %q", line)
		}
	}
}

// TestRenderBriefNarrativeWrapsToWidth pins the measure: every rendered line
// fits it in both colour modes, and only a token longer than the whole measure
// is ever split.
func TestRenderBriefNarrativeWrapsToWidth(t *testing.T) {
	t.Parallel()
	// The sign-off line is the longest fixed string the render carries, so a
	// signable state joins the table: it must wrap like everything else.
	signable := briefQuietCase()
	signable.name, signable.res.Review.OneTap.Signable = "quiet-signable", true
	for _, tc := range append(briefNarrativeCases(), signable) {
		for _, width := range []int{60, 80, 100} {
			for _, color := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/color=%t", tc.name, width, color), func(t *testing.T) {
					t.Parallel()
					output := renderBriefNarrativeCase(tc, color, width)
					for line := range strings.SplitSeq(strings.TrimSuffix(output, "\n"), "\n") {
						if visibleLen(line) > width {
							t.Errorf("line of %d cells exceeds the %d-column measure: %q", visibleLen(line), width, line)
						}
					}
					// No composed token is long enough to split at these
					// measures, so every word survives whole.
					plain := strings.Join(strings.Fields(stripBriefANSI(output)), " ")
					for _, paragraph := range briefRunTexts(tc.res.Narrative) {
						for word := range strings.FieldsSeq(paragraph) {
							if !strings.Contains(plain, word) {
								t.Errorf("wrapping split the token %q", word)
							}
						}
					}
				})
			}
		}
	}
}

func stripBriefANSI(s string) string {
	var out strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		spans, _ := briefLineSpans(line)
		for _, span := range spans {
			out.WriteString(span.text)
		}
		out.WriteString("\n")
	}
	return out.String()
}

// TestRenderBriefNarrativeOverlongTokenSplits covers the measure's escape
// hatch: a token wider than the whole line is split rather than allowed to
// overrun it, and the split keeps each half's tint.
func TestRenderBriefNarrativeOverlongTokenSplits(t *testing.T) {
	t.Parallel()
	runs := []rpc.BriefRun{briefText("head "), briefAct(strings.Repeat("x", 30)), briefText("tail")}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
	briefProseParagraph(env, runs, 20)
	for line := range strings.SplitSeq(strings.TrimSuffix(stdout.String(), "\n"), "\n") {
		if visibleLen(line) > 20 {
			t.Errorf("line of %d cells exceeds 20: %q", visibleLen(line), line)
		}
		if _, open := briefLineSpans(line); open {
			t.Errorf("split token leaks an open SGR: %q", line)
		}
	}
	if got := strings.Join(strings.Fields(stripBriefANSI(stdout.String())), ""); got != "headxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxtail" {
		t.Fatalf("split lost or duplicated text: %s", got)
	}
}

// TestRenderBriefFallsBackToRows pins the version-skew boundary and mirrors
// servedNarrative in web/app/brief.js: an older daemon serves no narrative, a
// narrative whose movements did not compose is not served, and a coda alone is
// not a briefing. Each falls back to the row render, reasons and all.
func TestRenderBriefFallsBackToRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		narrative *rpc.BriefNarrative
	}{
		{name: "absent"},
		{name: "empty", narrative: &rpc.BriefNarrative{}},
		{name: "empty runs and paragraphs", narrative: &rpc.BriefNarrative{
			Lead:   []rpc.BriefRun{{Text: ""}},
			Review: []rpc.BriefParagraph{{Runs: []rpc.BriefRun{{Text: ""}}}},
			Ready:  []rpc.BriefParagraph{{}},
		}},
		{name: "coda only", narrative: &rpc.BriefNarrative{Coda: []rpc.BriefRun{briefText("Nothing owed before the bell.")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if view := servedBriefNarrative(tc.narrative); view != nil {
				t.Fatalf("narrative must not count as served: %+v", view)
			}
			var stdout bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
			res := rpc.BriefResult{AsOf: time.Date(2026, 7, 30, 8, 0, 0, 0, time.Local), Narrative: tc.narrative}
			res.Review.SessionPnL.BriefRowState = rpc.BriefRowState{Status: rpc.BriefStatusUnavailable, Detail: "account summary unavailable: broker down"}
			renderBrief(env, res)
			got := stdout.String()
			for _, want := range []string{"Review  (since the last close)", "Ready  (today)", "session P&L", "account summary unavailable: broker down", "drawdown latch"} {
				if !strings.Contains(got, want) {
					t.Fatalf("row fallback missing %q:\n%s", want, got)
				}
			}
			if strings.Contains(got, "Degraded inputs") {
				t.Fatalf("the row render discloses each row on its own line and needs no footer:\n%s", got)
			}
		})
	}
}

// TestRenderBriefServesNarrativeInsteadOfRows is the other side of the
// fallback: a served narrative replaces the table outright, so the row labels
// are gone and the header line is unchanged.
func TestRenderBriefServesNarrativeInsteadOfRows(t *testing.T) {
	// renderBrief is the one entry point that resolves the measure from the
	// environment, so the measure is pinned here rather than inherited.
	t.Setenv("COLUMNS", "80")
	tc := briefQuietCase()
	tc.res.AsOf = time.Date(2026, 7, 30, 8, 0, 0, 0, time.Local)
	tc.res.BriefFingerprint = "sha256:abcdef0123456789"
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderBrief(env, tc.res)
	got := stdout.String()
	if !strings.HasPrefix(got, "Daily brief — 2026-07-30 08:00 ") {
		t.Fatalf("the header line must not change:\n%s", got)
	}
	if !strings.Contains(got, shortFingerprint(tc.res.BriefFingerprint)) {
		t.Fatalf("the header must keep the fingerprint:\n%s", got)
	}
	for _, absent := range []string{"session P&L", "by underlying", "capital events", "hedge cost / day", "artefact "} {
		if strings.Contains(got, absent) {
			t.Fatalf("prose replaces the row table, but %q survived:\n%s", absent, got)
		}
	}
	if !strings.Contains(got, "Daily P/L stands at EUR +2,340") {
		t.Fatalf("prose missing:\n%s", got)
	}
}

// TestRenderBriefNarrativeDisclosesDegradedRows is the disclosure contract
// under prose: the narrative names an unread input but never carries the row's
// reason, so each degraded or unavailable row states its own beneath the text.
// A brief whose inputs all read prints no footer at all.
func TestRenderBriefNarrativeDisclosesDegradedRows(t *testing.T) {
	t.Parallel()
	got := renderBriefNarrativeCase(briefDegradedCase(), false, 80)
	_, footer, split := strings.Cut(got, "\nDegraded inputs\n")
	if !split {
		t.Fatalf("degraded rows must be disclosed:\n%s", got)
	}
	for _, want := range []string{
		"  session P&L: account summary unavailable: broker down",
		"  by underlying: positions unavailable",
		"  proposals: proposal outcome journal is unavailable",
		"  working orders: open-orders journal unavailable",
		"  regime: regime snapshot unavailable",
		"  breadth: breadth snapshot unavailable",
		"  dealer gamma: dealer gamma source is stale",
		"  capital: risk constitution absent",
		"  premium at risk: positions unavailable",
		"  hedge cost / day: positions unavailable",
		"  protection proposals: protection proposal snapshot is unavailable",
	} {
		if !strings.Contains(footer, want+"\n") {
			t.Errorf("disclosure missing %q:\n%s", want, footer)
		}
	}
	// Rows that read are not listed, and neither are the ok rows of a
	// degraded payload.
	for _, absent := range []string{"reconcile:", "policy drift:", "stress:", "session:"} {
		if strings.Contains(footer, absent) {
			t.Errorf("only unread rows are disclosed, but %q was listed:\n%s", absent, footer)
		}
	}
	for _, tc := range []briefNarrativeCase{briefQuietCase(), briefWatchCase(), briefActCase()} {
		if out := renderBriefNarrativeCase(tc, false, 80); strings.Contains(out, "Degraded inputs") {
			t.Errorf("%s has no unread input and must print no footer:\n%s", tc.name, out)
		}
	}
}

// TestRenderBriefNarrativeSignoffLine keeps the one line the prose would
// otherwise drop: the row view is the only place the CLI names the sign-off
// command, and it renders in the row view's own words.
func TestRenderBriefNarrativeSignoffLine(t *testing.T) {
	t.Parallel()
	want := "  one-tap sign-off: " + briefOneTapSignable
	tc := briefQuietCase()
	if got := renderBriefNarrativeCase(tc, false, 80); strings.Contains(got, "one-tap sign-off") {
		t.Fatalf("an unsignable report offers no sign-off line:\n%s", got)
	}
	tc.res.Review.OneTap.Signable = true
	got := renderBriefNarrativeCase(tc, false, 80)
	if strings.Count(got, want+"\n") != 1 {
		t.Fatalf("the sign-off line must render exactly once, verbatim:\n%s", got)
	}
	review, ready, _ := strings.Cut(got, "\nReady  (today)\n")
	if !strings.Contains(review, want) || strings.Contains(ready, want) {
		t.Fatalf("the sign-off line belongs to Review:\n%s", got)
	}
}

// TestRenderBriefNarrativeSanitizesControlCharacters is escape-injection
// hardening. The daemon composes run text from broker-sourced symbols and
// row details from broker error strings; this renderer is the one path that
// gives text an ANSI meaning, so control bytes never reach the terminal.
func TestRenderBriefNarrativeSanitizesControlCharacters(t *testing.T) {
	t.Parallel()
	res := rpc.BriefResult{
		Review: rpc.BriefReviewSection{SessionPnL: rpc.BriefAccountRow{BriefRowState: rpc.BriefRowState{
			Status: rpc.BriefStatusUnavailable, Detail: "account unavailable: \x1b[2Jwiped\x07",
		}}},
		Narrative: &rpc.BriefNarrative{
			Lead:   []rpc.BriefRun{briefText("Held name \x1b[31mACME\x1b[0m\r carries earnings context.")},
			Review: []rpc.BriefParagraph{briefPara(briefText("Per-name attribution is unavailable.\x0b"))},
			Ready:  []rpc.BriefParagraph{briefPara(briefText("Breadth is unavailable.\u009b31m"))},
		},
	}
	got := renderBriefNarrativeCase(briefNarrativeCase{res: res}, false, 80)
	if strings.ContainsAny(got, "\x1b\r\x07\x0b\u009b") {
		t.Fatalf("control bytes reached the terminal: %q", got)
	}
	for _, want := range []string{"Held name [31mACME[0m carries earnings context.", "account unavailable: [2Jwiped"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sanitized text missing %q:\n%s", want, got)
		}
	}
}

// TestBriefProseWidth pins the measure resolution: the cap holds on a wide
// terminal, a narrow one keeps its own width, and a writer that is not a
// terminal falls back to the cap instead of an unbounded line.
func TestBriefProseWidth(t *testing.T) {
	for _, tc := range []struct {
		columns string
		want    int
	}{
		{columns: "", want: briefProseMeasure},
		{columns: "200", want: briefProseMeasure},
		{columns: "60", want: 60},
		{columns: "10", want: briefProseMeasure},
	} {
		t.Run("COLUMNS="+tc.columns, func(t *testing.T) {
			t.Setenv("COLUMNS", tc.columns)
			if got := briefProseWidth(&bytes.Buffer{}); got != tc.want {
				t.Fatalf("width=%d, want %d", got, tc.want)
			}
		})
	}
}

// TestRunBriefJSONCarriesNarrativeUnchanged pins the machine contract: the CLI
// re-encodes the daemon's result verbatim, so the narrative reaches --json
// exactly as composed and nothing in the text render may mutate it.
func TestRunBriefJSONCarriesNarrativeUnchanged(t *testing.T) {
	t.Parallel()
	snapshot := briefWatchCase().res
	snapshot.AsOf = time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	snapshot.BriefFingerprint = "sha256:rendered"
	conn := &briefFakeConn{snapshot: snapshot}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn, Origin: rpc.OrderOriginHumanTTY}
	if code := runBrief(context.Background(), env, []string{"--json"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	var want bytes.Buffer
	enc := json.NewEncoder(&want)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snapshot); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != want.String() {
		t.Fatalf("--json is not byte-identical to the decoded result:\n got: %s\nwant: %s", stdout.String(), want.String())
	}
	if !strings.Contains(stdout.String(), `"role": "watch"`) {
		t.Fatalf("run roles must survive --json:\n%s", stdout.String())
	}
	if len(conn.calls) != 1 || conn.calls[0].method != rpc.MethodBriefSnapshot {
		t.Fatalf("--json must never stamp: calls=%+v", conn.calls)
	}
}

// TestRenderBriefNarrativeGoldens pins the whole rendered block for the four
// payload states the composer's own table drives: layout, section headers,
// paragraph spacing, the wrap at the 80-column measure, the sign-off line and
// the disclosure footer. Plain text only — with Color off the render is the
// composer's sentences and nothing else, which is what makes a golden
// readable. Width is explicit, so a wide terminal cannot reflow these.
func TestRenderBriefNarrativeGoldens(t *testing.T) {
	t.Parallel()
	signable := briefQuietCase()
	signable.name = "quiet-signable"
	signable.res.Review.OneTap.Signable = true
	cases := append(briefNarrativeCases(), signable)
	goldens := map[string]string{
		"quiet": `
  Stress reads stand down at observe severity. Regime stage quiet, verdict
  Normal regime. Nothing across Review and Ready needs a decision.

Review  (since the last close)
  Daily P/L stands at EUR +2,340 on equity of EUR 1,250,000. By name in EUR:
  AAPL +900.00, NVDA +800.40, SPY +639.60, and 2 other names at -120.00.

Ready  (today)
  Breadth has 62.0% above the 50-DMA and 58.0% above the 200-DMA, net new highs
  +1.4%.

  Process folds clean: policy pins match, cadence artefacts are declared with 1
  of 2 complete, the monthly pulse is not due, and no held-name events.

  Nothing owed before the bell.
`,
		"watch": `
  Stress reads reduce risk at watch severity. 3 rows need a decision: overrides,
  held-name earnings, protection proposals.

Review  (since the last close)
  1 active override widens policy controls: hedge_coverage. No rule transitions.

Ready  (today)
  2 held names carry earnings context: AAPL, NVDA. The remaining held-name event
  source is clean.

  2 protection proposals are ready to act, with 1 more blocked.

  Owed before the bell: overrides, held-name earnings, protection proposals.
  Everything else holds.
`,
		"act": `
  Stress reads de-risk at act severity. 4 rows need a decision: rules delta,
  capital events, capital, drawdown latch. Regime stage quiet, verdict Normal
  regime.

Review  (since the last close)
  1 rule worsened to act since the last stamped brief: hedge coverage.

  The drawdown latch engaged this episode and remains open until a human reset.
  It engaged at 30.4% consumed.

Ready  (today)
  Capital sits in the block tier under shadow enforcement with 118.0% of the
  drawdown budget consumed. The drawdown latch is engaged, 2 days old, and
  remains so until a human reset.

  No protection proposals are staged.

  Owed before the bell: rules delta, capital events, capital, drawdown latch.
  Everything else holds.
`,
		"degraded": `
  The regime read is unavailable. Nothing across Review and Ready needs a
  decision. 12 inputs could not be read and are named below: session P/L,
  attribution, proposals, working orders, regime, breadth, dealer gamma,
  held-name events, capital, premium at risk, hedge cost, protection proposals.

Review  (since the last close)
  Account P/L is unavailable, so the session cannot be summarized. Per-name
  attribution is unavailable.

  The proposal outcome journal is unavailable. The open-orders journal is
  unavailable.

Ready  (today)
  Breadth is unavailable, so participation cannot be stated. Dealer gamma is
  degraded, so the spot-to-zero-gamma relationship cannot be stated.

  The risk constitution is absent, so capital controls are unapproved and the
  drawdown budget cannot be stated. Premium at risk is unavailable. Hedge cost
  is unavailable.

  Nothing on the desk needs a decision, but 12 inputs could not be read: unknown
  is not clean.

Degraded inputs
  session P&L: account summary unavailable: broker down
  by underlying: positions unavailable
  proposals: proposal outcome journal is unavailable
  working orders: open-orders journal unavailable
  regime: regime snapshot unavailable
  breadth: breadth snapshot unavailable
  dealer gamma: dealer gamma source is stale
  capital: risk constitution absent
  premium at risk: positions unavailable
  hedge cost / day: positions unavailable
  protection proposals: protection proposal snapshot is unavailable
`,
		"quiet-signable": `
  Stress reads stand down at observe severity. Regime stage quiet, verdict
  Normal regime. Nothing across Review and Ready needs a decision.

Review  (since the last close)
  Daily P/L stands at EUR +2,340 on equity of EUR 1,250,000. By name in EUR:
  AAPL +900.00, NVDA +800.40, SPY +639.60, and 2 other names at -120.00.

  one-tap sign-off: signable · canary policy capital-event reconcile

Ready  (today)
  Breadth has 62.0% above the 50-DMA and 58.0% above the 200-DMA, net new highs
  +1.4%.

  Process folds clean: policy pins match, cadence artefacts are declared with 1
  of 2 complete, the monthly pulse is not due, and no held-name events.

  Nothing owed before the bell.
`,
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderBriefNarrativeCase(tc, false, 80)
			if got != goldens[tc.name] {
				t.Fatalf("render drifted\n got:\n%s\nwant:\n%s", got, goldens[tc.name])
			}
			for line := range strings.SplitSeq(got, "\n") {
				if line != strings.TrimRight(line, " \t") {
					t.Errorf("line carries trailing whitespace: %q", line)
				}
			}
		})
	}
}

func TestBriefLastSessionValueRendersCaptureAndAbsence(t *testing.T) {
	capturedAt := time.Date(2026, 7, 31, 20, 0, 9, 0, time.UTC)
	row := rpc.BriefLastSessionRow{
		SessionDate: "2026-07-31", DailyPnLBase: new(-433.7), BaseCurrency: "EUR", CapturedAt: capturedAt,
	}
	want := "2026-07-31 " + formatMoneyCcy(-433.7, "EUR") + " · captured " + capturedAt.Local().Format("15:04:05")
	if got := briefLastSessionValue(row); got != want {
		t.Fatalf("captured value = %q, want %q", got, want)
	}
	if got := briefLastSessionValue(rpc.BriefLastSessionRow{SessionDate: "2026-07-31"}); got != "2026-07-31 · not captured" {
		t.Fatalf("absent value = %q", got)
	}
	if got := briefLastSessionValue(rpc.BriefLastSessionRow{}); got != "—" {
		t.Fatalf("unresolved value = %q", got)
	}
}
