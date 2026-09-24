package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestMarketTapeHistoryRoutingAndHelp(t *testing.T) {
	var out bytes.Buffer
	conn := &tapeCLIConn{}
	if code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &out}, "market", []string{"tape", "--history", "--before", "2026-09-21", "--json"}); code != 0 || !conn.params.History || conn.params.Before != "2026-09-21" {
		t.Fatalf("history routing: %d %s", code, &out)
	}
	out.Reset()
	if code := Run(t.Context(), &Env{Stdout: &out, Stderr: &out}, "market", []string{"tape", "--help"}); code != 0 || !strings.Contains(out.String(), "--history") || !strings.Contains(out.String(), "--before") {
		t.Fatalf("history help missing: %d %s", code, &out)
	}
	out.Reset()
	if code := Run(t.Context(), &Env{Stdout: &out, Stderr: &out}, "market", []string{"tape", "--before", "2026-09-21"}); code == 0 || !strings.Contains(out.String(), "requires history") {
		t.Fatalf("before silently ignored: %d %s", code, &out)
	}
}

func TestMarketTapeHistoryTextIsCompactAndHonestAboutTiming(t *testing.T) {
	result := rpc.MarketTapeResult{Archive: &rpc.MarketTapeArchiveStatus{Status: "collection_failed"}, History: &rpc.MarketTapeHistory{NextBefore: "2026-09-18", Rows: []rpc.MarketTapeHistoryRow{{
		Date: "2026-09-18\x1b[2J", Latest: &rpc.MarketTapeCapture{Timing: "reconstructed", Session: rpc.MarketTapeSession{SPX: &rpc.MarketTapePrice{ChangePct: new(-0.001)}, Breadth: &rpc.MarketTapeBreadth{PctAbove50DMA: new(0.0)}}},
		FollowUps: []rpc.MarketTapeFollowUp{{Sessions: 1, Status: "available", SPXChangePct: new(0.0)}, {Sessions: 3, Status: "pending"}},
	}, {Date: "2026-09-21", FollowUps: []rpc.MarketTapeFollowUp{{Sessions: 1, Status: "unavailable"}, {Sessions: 3, Status: "pending"}}}}}}
	for _, explain := range []bool{false, true} {
		var out bytes.Buffer
		renderMarketTapeHistory(&Env{Stdout: &out}, &result, explain)
		text := out.String()
		for _, want := range []string{"what happened next", "no forecast", "2026-09-18", "0.00%", "waiting", "—", "Reconstructed later", "Latest collection failed", "--before 2026-09-18"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q in %s", want, text)
			}
		}
		if strings.Contains(text, "\x1b") || strings.Contains(text, "-0.00") {
			t.Fatal("untrusted control characters or spurious negative zero")
		}
		if strings.Contains(text, "scorecard") != explain {
			t.Fatal("explanation disclosure lost")
		}
		for line := range strings.SplitSeq(text, "\n") {
			if visibleLen(line) > 80 {
				t.Fatalf("line too wide: %s", line)
			}
		}
	}
}
