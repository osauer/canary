//go:build trading

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func apiBoundaryDispatch(t *testing.T, srv *Server, method, params string) rpc.Response {
	t.Helper()
	var out bytes.Buffer
	if srv.logger == nil {
		srv.logger = NewLogger(&bytes.Buffer{}, "error")
	}
	srv.dispatch(t.Context(), &rpc.Request{ID: "boundary-review", Method: method, Params: json.RawMessage(params)}, json.NewEncoder(&out), bufio.NewReader(strings.NewReader("")))
	var response rpc.Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v", out.String(), err)
	}
	if response.ID != "boundary-review" {
		t.Fatalf("response lost request identity: %+v", response)
	}
	return response
}

func TestAPIBoundaryMalformedPreviewsDoNotReachWhatIf(t *testing.T) {
	cases := []struct{ name, params string }{
		{"missing", `{}`},
		{"null", `null`},
		{"array", `[]`},
		{"string", `"not an object"`},
		{"missing_quantity", `{"action":"sell","contract":{"symbol":"SYNTHB"}}`},
		{"negative_quantity", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":-1}`},
		{"fractional_quantity", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":0.5}`},
		{"overflow_quantity", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":9223372036854775808}`},
		{"missing_contract", `{"action":"sell","quantity":1}`},
		{"invalid_action", `{"action":"short","contract":{"symbol":"SYNTHB"},"quantity":1}`},
		{"invalid_expiry", `{"action":"sell","contract":{"symbol":"SYNTHB","sec_type":"OPT","expiry":"20260230","right":"C","strike":100},"quantity":1}`},
		{"missing_right", `{"action":"sell","contract":{"symbol":"SYNTHB","sec_type":"OPT","expiry":"20260619","strike":100},"quantity":1}`},
		{"negative_strike", `{"action":"sell","contract":{"symbol":"SYNTHB","sec_type":"OPT","expiry":"20260619","right":"C","strike":-1},"quantity":1}`},
		{"zero_limit", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":1,"limit_price":0}`},
		{"negative_limit", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":1,"limit_price":-1}`},
		{"nonfinite_limit", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":1,"limit_price":NaN}`},
		{"overflow_limit", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":1,"limit_price":1e309}`},
		{"unsupported_order_type", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":1,"order_type":"MKT"}`},
		{"invalid_tif", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":1,"limit_price":100,"tif":"FOREVER"}`},
		{"invalid_trigger", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":1,"order_type":"TRAIL","trigger_method":6}`},
		{"caller_allowance", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":1,"strategy":"allowance-limit","allowance":{"pct":0}}`},
		{"caller_bounded", `{"action":"sell","contract":{"symbol":"SYNTHB"},"quantity":1,"strategy":"bounded-limit","bounded":{"concession":0.01,"worst_price":99,"max_spread_pct_of_mid":1}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
			srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
			srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)
			whatIfCalls := 0
			srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				whatIfCalls++
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
			}
			response := apiBoundaryDispatch(t, srv, rpc.MethodOrderPreview, tc.params)
			if response.Ok || response.Error == nil || response.Error.Code != rpc.CodeBadRequest {
				t.Fatalf("invalid preview response = %+v, want bad_request", response)
			}
			if whatIfCalls != 0 {
				t.Fatalf("invalid preview made %d WhatIf calls", whatIfCalls)
			}
			events, err := srv.orderJournal.LoadEvents(0)
			if err != nil || len(events) != 0 {
				t.Fatalf("invalid preview journal = %+v, err=%v", events, err)
			}
		})
	}
}

func TestAPIBoundaryRejectsTokenDriftBeforeBrokerMutation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*orderPreviewTokenPayload)
	}{
		{"account", func(p *orderPreviewTokenPayload) { p.Account = "DU7654321" }},
		{"mode", func(p *orderPreviewTokenPayload) { p.Mode = rpc.AccountModeLive }},
		{"endpoint", func(p *orderPreviewTokenPayload) { p.Endpoint = "127.0.0.1:7497" }},
		{"client", func(p *orderPreviewTokenPayload) { p.ClientID++ }},
		{"expired", func(p *orderPreviewTokenPayload) { p.ExpiresAt = p.IssuedAt.Add(-time.Second) }},
		{"modify_scope", func(p *orderPreviewTokenPayload) { p.Scope = rpc.OrderTokenScopeModify }},
		{"exercise_scope", func(p *orderPreviewTokenPayload) { p.Scope = rpc.OrderTokenScopeExercise }},
		{"unavailable_whatif", func(p *orderPreviewTokenPayload) {
			p.WhatIf = previewWhatIfUnavailable()
			p.WhatIfStatus = p.WhatIf.Status
		}},
		{"rejected_whatif", func(p *orderPreviewTokenPayload) {
			p.WhatIf.Status = rpc.OrderWhatIfStatusRejected
			p.WhatIfStatus = p.WhatIf.Status
		}},
		{"additional_submit_gate", func(p *orderPreviewTokenPayload) { p.WhatIf.RequiredForSubmit = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
			calls := 0
			srv.orderReserveBrokerID = func(context.Context) (int, error) { calls++; return 1001, nil }
			srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error { calls++; return nil }
			token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true})
			if _, err := srv.verifyPreviewTokenForPlace(token); err != nil {
				t.Fatalf("valid token precondition: %v", err)
			}
			if auth, _, err := srv.authorizeBrokerWriteTransaction("", false); !auth.Allowed || err != nil {
				t.Fatalf("write authority precondition: allowed=%v err=%v blockers=%v", auth.Allowed, err, auth.Blockers)
			}
			payload, err := srv.orderTokens.verify(token)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(&payload)
			token, _, _, err = srv.orderTokens.mint(payload)
			if err != nil {
				t.Fatal(err)
			}
			params, err := json.Marshal(rpc.OrderPlaceParams{PreviewToken: token})
			if err != nil {
				t.Fatal(err)
			}
			response := apiBoundaryDispatch(t, srv, rpc.MethodOrderPlace, string(params))
			if response.Ok || response.Error == nil || response.Error.Code != rpc.CodeTradingDisabled {
				t.Fatalf("drifted token response = %+v, want trading_disabled", response)
			}
			if calls != 0 {
				t.Fatalf("drifted token made %d broker mutation calls", calls)
			}
			events, err := srv.orderJournal.LoadEvents(0)
			if err != nil || len(events) != 0 {
				t.Fatalf("rejected place journal = %+v, err=%v", events, err)
			}
		})
	}
}

func TestAPIBoundaryInvalidHistoryIsBadRequest(t *testing.T) {
	for _, params := range []string{
		`{"contract":{"symbol":"SYNTH","sec_type":"OPT","expiry":"20261218","right":"C","strike":100},"range":"1D"}`,
		`{"contract":{"symbol":"SYNTH","sec_type":"BAG"},"range":"1D"}`,
		`{"contract":{"symbol":"MES","sec_type":"FUT","exchange":"CME","currency":"USD"},"range":"1D"}`,
	} {
		srv := newTestServer(t)
		response := apiBoundaryDispatch(t, srv, rpc.MethodMarketHistory, params)
		if response.Ok || response.Error == nil || response.Error.Code != rpc.CodeBadRequest {
			t.Errorf("invalid history response = %+v, want bad_request", response)
		}
	}
}
