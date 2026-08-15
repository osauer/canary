package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRenderOrderPreviewShowsTokenAndSubmitEligibility(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}

	renderOrderPreviewText(env, &rpc.OrderPreviewResult{
		PreviewToken:          "ibkrp1.body.sig",
		PreviewTokenID:        "tok_123",
		PreviewTokenExpiresAt: time.Date(2026, 5, 30, 9, 30, 0, 0, time.UTC),
		TokenMinted:           true,
		SubmitEligible:        false,
		Executable:            false,
		Mode:                  "paper",
		Account:               "DU1234567",
		Endpoint:              "127.0.0.1:4002",
		ClientID:              31,
		Draft: rpc.OrderDraft{
			Action:     rpc.OrderActionBuy,
			Contract:   rpc.ContractParams{Symbol: "AAPL"},
			Quantity:   10,
			OrderType:  rpc.OrderTypeLMT,
			LimitPrice: 100.12,
			TIF:        rpc.OrderTIFDay,
			Strategy:   rpc.OrderStrategyPatientLimit,
		},
		Quote: rpc.OrderQuoteSnapshot{
			Symbol:       "AAPL",
			DataType:     rpc.MarketDataLive,
			QuoteQuality: "firm",
		},
		Position:         rpc.OrderPositionImpact{Before: 0, After: 10, Effect: rpc.OrderPositionEffectOpen},
		Notional:         1001.20,
		NotionalCurrency: "USD",
		NotionalBase:     921.10,
		BaseCurrency:     "EUR",
		WhatIf: rpc.OrderWhatIfResult{
			Status:            rpc.OrderWhatIfStatusUnavailable,
			RequiredForSubmit: true,
		},
	})

	got := stdout.String()
	for _, want := range []string{
		"Token minted   true",
		"Submit eligible false",
		"WhatIf         unavailable (required=true)",
		"Token ID       tok_123",
		"Notional       1001.20 USD",
		"Notional (base) 921.10 EUR",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("order preview missing %q:\n%s", want, got)
		}
	}
}

func TestPreviewCLIOrderTypeAndTrailDraftSummary(t *testing.T) {
	t.Parallel()
	got, err := previewCLIOrderType("", true, true)
	if err != nil {
		t.Fatalf("previewCLIOrderType: %v", err)
	}
	if got != rpc.OrderTypeTRAILLIMIT {
		t.Fatalf("default trail-limit order type = %q, want TRAIL LIMIT", got)
	}
	got, err = previewCLIOrderType("trail-limit", false, false)
	if err != nil {
		t.Fatalf("previewCLIOrderType trail-limit: %v", err)
	}
	if got != rpc.OrderTypeTRAILLIMIT {
		t.Fatalf("normalized trail-limit order type = %q, want TRAIL LIMIT", got)
	}
	if _, err := previewCLIOrderType("LMT", true, false); err == nil || !strings.Contains(err.Error(), "cannot include trail") {
		t.Fatalf("previewCLIOrderType LMT+trail err = %v, want contradiction", err)
	}

	pct, offset := 2.0, 0.05
	summary := formatOrderDraftSummary(rpc.OrderDraft{
		Action:    rpc.OrderActionSell,
		Contract:  rpc.ContractParams{Symbol: "SPY"},
		Quantity:  10,
		OrderType: rpc.OrderTypeTRAILLIMIT,
		TIF:       rpc.OrderTIFDay,
		Trail: &rpc.OrderTrailSpec{
			TrailingPercent:  &pct,
			InitialStopPrice: 98,
			LimitOffset:      &offset,
		},
	})
	for _, want := range []string{"SELL 10 SPY TRAIL LIMIT", "trail 2%", "stop 98.0000", "limit_offset 0.0500"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("draft summary %q missing %q", summary, want)
		}
	}
}

func TestHoistFlagsKeepsReplaceOrderValue(t *testing.T) {
	t.Parallel()
	got := hoistFlags([]string{"preview", "--replace-order", "6", "--market", "de", "buy", "MBG", "1"})
	want := []string{"--replace-order", "6", "--market", "de", "preview", "buy", "MBG", "1"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("hoistFlags = %#v, want %#v", got, want)
	}
}

func TestRunOrderPreviewForwardsTriggerAndRouteFields(t *testing.T) {
	t.Parallel()
	conn, calls := startOrderPreviewFakeConn(t, rpc.OrderPreviewResult{PreviewTokenID: "tok", TokenMinted: true})
	defer conn.Close()

	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	code := Run(context.Background(), env, "order", []string{
		"preview", "sell", "AAPL", "1",
		"--order-type", "TRAIL",
		"--trail-percent", "2",
		"--trigger-method", "4",
		"--exchange", "SMART",
		"--primary", "NASDAQ",
		"--currency", "USD",
		"--replace-order", "ibkr-123",
		"--json",
	})
	if code != 0 {
		t.Fatalf("order preview exit=%d stderr=%s", code, stderr.String())
	}
	call := <-calls
	if call.method != rpc.MethodOrderPreview {
		t.Fatalf("method = %s, want %s", call.method, rpc.MethodOrderPreview)
	}
	p := call.params
	if p.TriggerMethod != 4 || p.ReplaceID != "ibkr-123" {
		t.Fatalf("params trigger/replace = %d/%q, want 4/ibkr-123", p.TriggerMethod, p.ReplaceID)
	}
	if p.Contract.Exchange != "SMART" || p.Contract.PrimaryExch != "NASDAQ" || p.Contract.Currency != "USD" {
		t.Fatalf("contract route = %+v, want SMART/NASDAQ/USD", p.Contract)
	}
}

func TestRunOrderPreviewRejectsInvalidTriggerMethodBeforeDial(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := runOrderPreview(context.Background(), env, []string{"--trigger-method", "9", "buy", "AAPL", "1"})
	if code != 1 {
		t.Fatalf("order preview exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "trigger method") {
		t.Fatalf("stderr missing trigger method error: %s", stderr.String())
	}
}

type orderPreviewCall struct {
	method string
	params rpc.OrderPreviewParams
}

func startOrderPreviewFakeConn(t *testing.T, result rpc.OrderPreviewResult) (*dial.Conn, <-chan orderPreviewCall) {
	t.Helper()
	socketPath := filepath.Join("/tmp", fmt.Sprintf("ibkr-cli-order-preview-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen fake daemon: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	calls := make(chan orderPreviewCall, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var req rpc.Request
		if err := json.NewDecoder(c).Decode(&req); err != nil {
			return
		}
		var params rpc.OrderPreviewParams
		_ = json.Unmarshal(req.Params, &params)
		calls <- orderPreviewCall{method: req.Method, params: params}
		raw, _ := json.Marshal(result)
		_ = json.NewEncoder(c).Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw})
	}()

	conn, err := dial.Connect(socketPath)
	if err != nil {
		t.Fatalf("connect fake daemon: %v", err)
	}
	return conn, calls
}
