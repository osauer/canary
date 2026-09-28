package cli

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestFormatStalledAnswerPath(t *testing.T) {
	since := time.Date(2026, 9, 28, 4, 31, 0, 0, time.UTC)
	due := since.Add(10 * time.Minute)
	lane := rpc.ConnectionAnswerPath{Lane: rpc.AnswerPathLanePrimary, State: rpc.AnswerPathStalled, StalledSince: since, RedialDue: due}
	want := "primary connection answers no history since " + since.Local().Format("15:04") + " · redial at " + due.Local().Format("15:04")
	if got := formatStalledAnswerPath(lane); got != want {
		t.Fatalf("stalled line = %q, want %q", got, want)
	}
	lane.RedialDue, lane.RedialedAt = time.Time{}, due
	want = "primary connection answers no history since " + since.Local().Format("15:04") + " · redialled at " + due.Local().Format("15:04") + "; restart the Gateway"
	if got := formatStalledAnswerPath(lane); got != want {
		t.Fatalf("stalled line after a redial = %q, want %q", got, want)
	}
}
