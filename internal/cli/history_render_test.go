package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestHistoryRenderersDescribeDirectAuthority(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	legacyHealth := rpc.HistoryIndexHealth{
		LastIngestAt:  now.Add(-time.Hour),
		IngestedBytes: 100,
		JournalBytes:  200,
	}
	tests := []struct {
		name string
		want string
		run  func(*Env, *bytes.Buffer)
	}{
		{
			name: "rules history",
			want: "source: daemon.db · direct history read",
			run: func(env *Env, out *bytes.Buffer) {
				renderRulesHistoryText(env, out, &rpc.RulesHistoryResult{
					Since: now.Add(-24 * time.Hour), Until: now, Index: legacyHealth,
				})
			},
		},
		{
			name: "reconciliation equity",
			want: "source: daemon.db · retained statement projection and declared capital events",
			run: func(env *Env, out *bytes.Buffer) {
				renderReconEquityText(env, out, &rpc.ReconEquityResult{
					Since: now.Add(-24 * time.Hour), Until: now,
					Index: legacyHealth, Statements: legacyHealth,
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			test.run(&Env{}, &out)
			got := out.String()
			if !strings.Contains(got, test.want) {
				t.Fatalf("output missing %q:\n%s", test.want, got)
			}
			for _, retired := range []string{"fully ingested", "catching up"} {
				if strings.Contains(got, retired) {
					t.Fatalf("output still claims retired index state %q:\n%s", retired, got)
				}
			}
		})
	}
}
