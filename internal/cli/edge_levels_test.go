package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestRenderEdgeLevelsNamesEveryVersionAndWhatIsPending(t *testing.T) {
	means := func() []rpc.EdgeLevelsResultMean {
		var out []rpc.EdgeLevelsResultMean
		for _, v := range rpc.EdgeLevelsVersions {
			out = append(out, rpc.EdgeLevelsResultMean{Version: v, CloseMeanPct: -1.25, CloseDays: 4, CloseBeatsHeld: 1})
		}
		return out
	}
	look := rpc.EdgeLevelsLook{LookMinutes: 60, Split: "2026-05-01", Sample: rpc.EdgeLevelsSample{StockDays: 4, Sessions: 4, Names: 1, Rows: map[string]int{rpc.EdgeLevelsRowLosing: 4}, Excluded: map[string]int{"first_hour_incomplete": 1}},
		Counts: []rpc.EdgeLevelsTally{{Reading: rpc.EdgeLevelsReadingPriorLow, Half: rpc.EdgeLevelsHalfAll, Days: 4, Cuts: 2}},
		Days:   []rpc.EdgeLevelsDay{{Symbol: "TESTA", Session: "2026-03-02", Contracts: 2, Row: rpc.EdgeLevelsRowLosing, Look: rpc.EdgeLevelsDraft{Results: []rpc.EdgeLevelsDayResult{{Version: rpc.EdgeLevelsVersionHeld, ClosePct: 1.5}}}}}}
	for _, half := range []string{rpc.EdgeLevelsHalfAll, rpc.EdgeLevelsHalfFirst, rpc.EdgeLevelsHalfSecond} {
		look.Summary = append(look.Summary, rpc.EdgeLevelsSum{Reading: rpc.EdgeLevelsReadingPriorLow, Row: rpc.EdgeLevelsRowLosing, Half: half, Size: rpc.EdgeLevelsSizeHalves, StockDays: 4, Results: means()})
	}
	r := rpc.EdgeLevelsResult{SchemaVersion: rpc.EdgeLevelsSchemaVersion, State: rpc.EdgeStateCurrent, NotExecution: true, From: "2026-03-02", To: "2026-06-30", Pending: 2, Incomplete: true, Looks: []rpc.EdgeLevelsLook{look}}
	var out bytes.Buffer
	renderEdgeLevels(&Env{Stdout: &out}, r, true)
	text := out.String()
	for _, want := range []string{"2 stock-days are still being read", "Look 60 minutes after the open", "Under the prior low at the look", "Held", "Reduced at once", "The rule", "With its buy back", "Cut with the index", "1/4", "Cuts 2", "first hour incomplete 1", "TESTA", "modelled"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output lacks %q:\n%s", want, text)
		}
	}
}
