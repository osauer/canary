package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestAcceptedStatementWarmReadsBoundAllocatedBytes(t *testing.T) {
	s, scope, now := flexCashSourceTestServer(t)
	raw := syntheticFlexCashSourceXML(scope.Account, "20260930", "20260930", "20261001;003000", "11000")
	raw = []byte(strings.Replace(string(raw), "</FlexStatement>", "<!--"+strings.Repeat("x", 2<<20)+"--></FlexStatement>", 1))
	acceptSyntheticFlexCash(t, s, raw)
	for _, name := range []string{"FX", "cash"} {
		t.Run(name, func(t *testing.T) {
			read := func() {
				if name == "FX" {
					rows, err := s.fxStatements(t.Context())
					if err != nil || len(rows) != 1 {
						t.Fatalf("FX evidence: rows=%d err=%v", len(rows), err)
					}
				} else {
					result, err := s.flexSettledCashBaseline(scope, now)
					if err != nil || result.Currencies["EUR"].EndingSettledCash == nil || *result.Currencies["EUR"].EndingSettledCash != 11000 {
						t.Fatalf("cash evidence: %v", err)
					}
				}
			}
			read()
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					read()
				}
			})
			t.Logf("warm %s: %d bytes/op for %d source bytes", name, result.AllocedBytesPerOp(), len(raw))
			if result.AllocedBytesPerOp() >= int64(len(raw)/4) {
				t.Fatal("warm accepted read allocates full statement XML")
			}
		})
	}
}

func TestDataHealthPageSizingAvoidsRepeatedFullEncoding(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	result := rpc.DataHealthResult{Sources: []rpc.DataSourceHealth{}, Complete: true}
	for i := range 64 {
		row := rpc.DataSourceHealth{ID: fmt.Sprintf("synthetic:%02d", i), Name: "Synthetic provider", State: "unknown"}
		for j := range 64 {
			row.History = append(row.History, rpc.DataHealthTransition{At: now.Add(-time.Duration(j) * time.Minute), State: "unavailable", Reason: strings.Repeat("x", 64)})
		}
		result.Sources = append(result.Sources, row)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	measured := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			page, err := fitDataHealthPage(result)
			if err != nil || len(page.Sources) == 0 || page.NextOffset == nil {
				b.Fatalf("page: %v", err)
			}
		}
	})
	t.Logf("page sizing: %d bytes/op for %d encoded input bytes", measured.AllocedBytesPerOp(), len(raw))
	if measured.AllocedBytesPerOp() > int64(len(raw)*12) {
		t.Fatal("page sizing repeatedly encodes the full shrinking catalogue")
	}
}

func TestDataHealthPageSizingKeepsLargestFittingPrefix(t *testing.T) {
	for _, count := range []int{0, 1, 2, 8, 64} {
		input := rpc.DataHealthResult{Offset: 998, Complete: true}
		for i := range count {
			input.Sources = append(input.Sources, rpc.DataSourceHealth{ID: fmt.Sprint(i), Name: strings.Repeat("x", 1024*(i%4+1))})
		}
		want := input
		for {
			raw, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) <= dataHealthPageBytes {
				break
			}
			want.Sources = want.Sources[:len(want.Sources)-1]
			next := want.Offset + len(want.Sources)
			want.NextOffset, want.Complete = &next, false
		}
		got, err := fitDataHealthPage(input)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if string(a) != string(b) {
			t.Fatalf("count %d: page changed from the largest fitting prefix", count)
		}
	}
	oversize := rpc.DataHealthResult{Sources: []rpc.DataSourceHealth{{Name: strings.Repeat("x", dataHealthPageBytes)}}}
	if _, err := fitDataHealthPage(oversize); err == nil {
		t.Fatal("oversize source silently omitted")
	}
}
