package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type marketReadConn struct{ params rpc.MarketHistoryParams }

func (c *marketReadConn) Call(_ context.Context, _ string, p, out any) error {
	c.params = p.(rpc.MarketHistoryParams)
	return errors.New("synthetic stop")
}

type tapeCLIConn struct {
	marketReadConn
	method string
	params rpc.MarketTapeParams
}

func (c *tapeCLIConn) Call(_ context.Context, method string, p, out any) error {
	c.method, c.params = method, p.(rpc.MarketTapeParams)
	*out.(*rpc.MarketTapeResult) = rpc.MarketTapeResult{LatestSession: "2026-09-23", CoverageStatus: "partial", NotPredictive: true, Sessions: []rpc.MarketTapeSession{{Reading: &rpc.MarketTapeReading{Headline: "Shared descriptive reading", Summary: "Missing data remains unknown"}, Date: "2026-09-23", QQQ: &rpc.MarketTapePrice{Close: 100, ChangePct: new(0.0), RelativeVolume20: new(0.0)}}}}
	return nil
}

func TestMarketTapeCLIHoistsFlagsAndPreservesMissingVersusZero(t *testing.T) {
	for _, args := range [][]string{{"tape", "--sessions", "5", "--json"}, {"--json", "tape", "--sessions=5"}, {"tape", "--sessions", "5"}} {
		var out bytes.Buffer
		conn := &tapeCLIConn{}
		if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", args); code != 0 || conn.method != rpc.MethodMarketTape || conn.params.Sessions != 5 {
			t.Fatalf("tape routing: code=%d args=%v output=%s", code, args, &out)
		}
		if strings.Contains(strings.Join(args, " "), "--json") {
			var got rpc.MarketTapeResult
			if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Sessions[0].SPX != nil || *got.Sessions[0].QQQ.ChangePct != 0 {
				t.Fatal("JSON evidence altered")
			}
		} else if !strings.Contains(out.String(), "—") || !strings.Contains(out.String(), "0.00%") || !strings.Contains(out.String(), "no forecast") || !strings.Contains(out.String(), "Shared descriptive reading") {
			t.Fatal("missing and zero are not distinguished")
		}
	}
}
func (*marketReadConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return errors.New("unexpected stream")
}
func TestMarketRangeHoistingCannotReplaceRangeWithJSONFlag(t *testing.T) {
	c := &marketReadConn{}
	var out bytes.Buffer
	Run(t.Context(), &Env{Conn: c, Stdout: &out, Stderr: &out}, "market", []string{"--symbol", "SYNTH", "--range", "1D", "--json"})
	if c.params.Range != "1D" || c.params.Contract.Symbol != "SYNTH" {
		t.Fatal("chart identity/range changed by CLI flag hoisting")
	}
}

func TestMarketTapeHelpNeedsNoDaemonAndShowsDefaults(t *testing.T) {
	for _, args := range [][]string{{"tape", "--help"}, {"--help", "tape"}} {
		var out, diagnostic bytes.Buffer
		if code := Run(t.Context(), &Env{Stdout: &out, Stderr: &diagnostic}, "market", args); code != 0 || diagnostic.Len() != 0 {
			t.Fatalf("help requires a connection or failed: %d %s", code, &diagnostic)
		}
		for _, want := range []string{"canary market tape —", "Usage: canary market tape", "default 20, range 5–60", "--explain", "--json", "Examples:", "no forecast"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("help lost %q", want)
			}
		}
	}
}

func TestMarketTapeTextPreservesEvidenceAndBoundsUntrustedProse(t *testing.T) {
	result := rpc.MarketTapeResult{LatestSession: "2026-09-23", CoverageStatus: "partial", Sessions: []rpc.MarketTapeSession{{
		Date: "2026-09-23", SPX: &rpc.MarketTapePrice{Close: 100, ChangePct: new(-0.0007)}, QQQ: &rpc.MarketTapePrice{Close: 100, ChangePct: new(0.0)},
		Reading: &rpc.MarketTapeReading{Headline: "Price held", Summary: strings.Repeat("Observed evidence ", 15), Evidence: []rpc.MarketTapeEvidence{{Key: "advance_decline", Label: "Daily participation", Value: "Not collected", Meaning: "A missing measurement is not zero.\x1b[2J"}}, Limits: []string{"Unknown original availability"}},
	}}, Sources: []rpc.MarketTapeSource{{Key: "breadth", Status: "partial", Detail: "Source detail with control text \x1b[2J"}}}
	for _, explain := range []bool{false, true} {
		var out bytes.Buffer
		renderMarketTape(&Env{Stdout: &out}, &result, explain)
		text := out.String()
		if strings.Contains(text, "-0.00") || strings.Contains(text, "\x1b") || !strings.Contains(text, "0.00%") || !strings.Contains(text, "—") || !strings.Contains(text, "Not collected") {
			t.Fatalf("zero, missing or safe text changed: %q", text)
		}
		if strings.Contains(text, "A missing measurement is not zero") != explain || strings.Contains(text, "Source detail") != explain {
			t.Fatal("explanation disclosure lost")
		}
		for line := range strings.SplitSeq(text, "\n") {
			if visibleLen(line) > 80 {
				t.Fatalf("line exceeds shared 80-column measure: %q", line)
			}
		}
	}
	var colored bytes.Buffer
	renderMarketTape(&Env{Stdout: &colored, Color: true}, &result, false)
	if !strings.Contains(colored.String(), ansiBold+"Market tape") {
		t.Fatal("standard terminal styling lost")
	}
	if *result.Sessions[0].SPX.ChangePct != -0.0007 {
		t.Fatal("text rounding mutated JSON evidence")
	}
}
