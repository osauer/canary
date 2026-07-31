package main

import (
	"strings"
	"testing"
)

func TestCheckGammaNoWaitEnvelopeValidatesLifecycleShape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  string
		want bool
	}{
		{
			name: "cold",
			env:  `{"status":"cold","cold_reason_code":"outside_option_session"}`,
			want: true,
		},
		{
			name: "computing",
			env:  `{"status":"computing","started_at":"2026-07-31T08:00:00Z","progress":12}`,
			want: true,
		},
		{
			name: "ready",
			env:  `{"status":"ready","result":{"scope":"spy+spx"}}`,
			want: true,
		},
		{
			name: "classified error",
			env:  `{"status":"error","error":"market data unavailable"}`,
			want: true,
		},
		{
			name: "ready without result",
			env:  `{"status":"ready"}`,
		},
		{
			name: "computing without started_at",
			env:  `{"status":"computing"}`,
		},
		{
			name: "computing with contradictory result",
			env:  `{"status":"computing","started_at":"2026-07-31T08:00:00Z","result":{}}`,
		},
		{
			name: "cold with contradictory result",
			env:  `{"status":"cold","result":{}}`,
		},
		{
			name: "error without classification",
			env:  `{"status":"error"}`,
		},
		{
			name: "hostile unknown status",
			env:  `{"status":"ready<script>","result":{}}`,
		},
		{
			name: "malformed JSON",
			env:  `{"status":`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkGammaNoWaitEnvelope(checkInputs{GammaEnvelope: []byte(tc.env)})
			if got.OK != tc.want {
				t.Fatalf("OK = %v, want %v (result=%+v)", got.OK, tc.want, got)
			}
		})
	}
}

func TestCatalogueRetiresObsoleteGammaChecks(t *testing.T) {
	t.Parallel()
	var names []string
	for _, entry := range catalogue() {
		names = append(names, entry.name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "gamma-no-wait-envelope") {
		t.Fatalf("catalogue lacks gamma-no-wait-envelope: %s", joined)
	}
	for _, retired := range []string{"gamma-noflag", "gamma-premarket-derived"} {
		if strings.Contains(joined, retired) {
			t.Fatalf("catalogue still exposes retired check %q: %s", retired, joined)
		}
	}
}
