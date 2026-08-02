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
			got := checkGammaNoWaitEnvelope(checkInputs{Envelope: []byte(tc.env)})
			if got.OK != tc.want {
				t.Fatalf("OK = %v, want %v (result=%+v)", got.OK, tc.want, got)
			}
		})
	}
}

func TestCheckChainIVSourceAcceptsAnAlreadySubscribedBoard(t *testing.T) {
	t.Parallel()
	optSubscribe := WireFrame{Direction: "OUT", MsgID: 1, MsgName: "reqMktData", Fields: []string{"1", "11", "7", "0", "SPY", "OPT"}}
	modelTick := WireFrame{Direction: "IN", MsgID: 21, Fields: []string{"21", "7", "13", "0", "0.18"}}
	const (
		served      = `{"strikes":[{"call_iv":0.18,"call_data_status":"quoted","put_iv":null,"put_data_status":"quoted"}]}`
		pricedNoIV  = `{"strikes":[{"call_iv":null,"call_data_status":"prev_close","put_iv":null,"put_data_status":"prev_close"}]}`
		unreachable = `{"strikes":[{"call_iv":null,"call_data_status":"subscribe_error","put_iv":null,"put_data_status":"subscribe_error"}]}`
		silent      = `{"strikes":[{"call_iv":null,"call_data_status":"no_quote","put_iv":null,"put_data_status":"no_quote"}]}`
	)
	for _, tc := range []struct {
		name   string
		frames []WireFrame
		loose  bool
		env    string
		want   bool
	}{
		{name: "subscribe and model tick", frames: []WireFrame{optSubscribe, modelTick}, want: true},
		{name: "subscribe without model tick strict", frames: []WireFrame{optSubscribe}},
		{name: "subscribe without model tick loose", frames: []WireFrame{optSubscribe}, loose: true, want: true},

		{name: "no subscribe but chain served IV", env: served, want: true},
		{name: "no subscribe and legs unreachable", env: unreachable},
		{name: "no subscribe and legs unreachable loose", env: unreachable, loose: true},
		{name: "no subscribe and legs silent", env: silent},
		{name: "no subscribe and legs silent loose", env: silent, loose: true},
		{name: "no subscribe and legs priced without IV strict", env: pricedNoIV},
		{name: "no subscribe and legs priced without IV loose", env: pricedNoIV, loose: true, want: true},

		// A gamma prewarm's own stream must not answer for a chain read that
		// reached no option line: msg 21 carries no tie to these contracts.
		{name: "stray prewarm ticks do not rescue an unreachable chain", frames: []WireFrame{modelTick}, env: unreachable},

		{name: "no subscribe and no response to adjudicate", env: ""},
		{name: "no subscribe and empty chain", env: `{"strikes":[]}`},
		{name: "no subscribe and malformed response", env: `{"strikes":`},
		{name: "no subscribe and implausible IV", env: `{"strikes":[{"call_iv":93.4,"call_data_status":"no_quote"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkChainIVSource(checkInputs{Frames: tc.frames, Loose: tc.loose, Envelope: []byte(tc.env)})
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
