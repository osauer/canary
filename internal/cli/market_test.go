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
	for _, args := range [][]string{{"tape", "--sessions", "5", "--json"}, {"--json", "tape", "--sessions=5"}, {"tape", "--sessions", "5"}, {"tape"}, {"tape", "--explain", "--json"}} {
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
		} else if !strings.Contains(out.String(), "—") || !strings.Contains(out.String(), "0.00×") || !strings.Contains(out.String(), "no forecast") || !strings.Contains(out.String(), "Shared descriptive reading") {
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
		for _, want := range []string{"canary market tape —", "Usage: canary market tape", "default 5, range 5–60", "--explain", "--json", "Examples:", "no forecast"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("help lost %q", want)
			}
		}
	}
}

func TestMarketTapeTextPreservesEvidenceAndBoundsUntrustedProse(t *testing.T) {
	result := rpc.MarketTapeResult{LatestSession: "2026-09-23", CoverageStatus: "partial", Sessions: []rpc.MarketTapeSession{{
		Date: "2026-09-23", SPX: &rpc.MarketTapePrice{Close: 100, ChangePct: new(-0.0007)}, QQQ: &rpc.MarketTapePrice{Close: 100, ChangePct: new(0.0)},
		Reading: &rpc.MarketTapeReading{Headline: strings.Repeat("Observed evidence ", 15) + "\x1b[2J", Evidence: []rpc.MarketTapeEvidence{{Key: "giveback", Label: "Recent gain", Value: "2026-09-21: +2%; 50% given back\x1b[2J", Meaning: "Detailed technical meaning"}}, Limits: []string{"Unknown original availability"}},
	}}, Sources: []rpc.MarketTapeSource{{Key: "breadth", Status: "partial", Detail: "Source detail with control text \x1b[2J"}}}
	for _, explain := range []bool{false, true} {
		var out bytes.Buffer
		renderMarketTape(&Env{Stdout: &out}, &result, explain)
		text := out.String()
		if strings.Contains(text, "-0.00") || strings.Contains(text, "\x1b") || !strings.Contains(text, "0.00%") || !strings.Contains(text, "—") || !strings.Contains(text, "Stocks rising/falling that day: unavailable") || !strings.Contains(text, "50% given back") {
			t.Fatalf("zero, missing or safe text changed: %q", text)
		}
		if strings.Contains(text, "What this tells you") != explain || strings.Contains(text, "Detailed technical meaning") || strings.Contains(text, "Source detail") {
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

func TestMarketTapeCompactOutputKeepsCoverageAndFailedRefreshVisible(t *testing.T) {
	result := rpc.MarketTapeResult{LatestSession: "2026-09-23", Sessions: []rpc.MarketTapeSession{{
		Date: "2026-09-23", Breadth: &rpc.MarketTapeBreadth{PctAbove50DMA: new(0.0), Coverage50: 90, MemberCount: 100, Participation: &rpc.BreadthParticipation{}},
	}}, Sources: []rpc.MarketTapeSource{
		{Key: "spx", Status: "partial", MissingSessions: 1, CoveredThrough: "2026-09-22", Cache: &rpc.MarketHistoryCache{RefreshFailed: true}},
		{Key: "qqq", Status: "unavailable"},
	}}
	var out bytes.Buffer
	renderMarketTape(&Env{Stdout: &out}, &result, false)
	for _, want := range []string{"0.00%*", "cannot be compared", "90 of 100 stocks", "Stocks rising/falling that day: unavailable", "missing days: 1", "last available 2026-09-22", "refresh failed; showing saved history", "QQQ prices/volume unavailable"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("compact view hid %q: %s", want, &out)
		}
	}
	// A measured zero is different from a default zero with no covered stocks.
	result.Sessions[0].Breadth.Participation = &rpc.BreadthParticipation{CoverageAD: 90, Declining: 89, Unchanged: 1}
	out.Reset()
	renderMarketTape(&Env{Stdout: &out}, &result, false)
	if !strings.Contains(out.String(), "0 stocks rose / 89 fell / 1 unchanged (90 of 100 covered)") {
		t.Fatalf("measured zero hidden: %s", &out)
	}
}
