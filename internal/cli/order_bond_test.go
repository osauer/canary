package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// bondPreviewConn records the order preview request it receives.
type bondPreviewConn struct {
	method string
	params rpc.OrderPreviewParams
}

func (c *bondPreviewConn) Call(_ context.Context, method string, in, out any) error {
	c.method = method
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &c.params); err != nil {
		return err
	}
	res, _ := json.Marshal(rpc.OrderPreviewResult{Draft: rpc.OrderDraft{Action: rpc.OrderActionBuy}})
	return json.Unmarshal(res, out)
}

func (c *bondPreviewConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return errors.New("unexpected stream")
}

// The bond form names the bond and its face; the daemon derives the rest.
// Stock-only flags and a missing currency are refused before any call.
func TestOrderPreviewBondFormSendsIdentifierAndFace(t *testing.T) {
	run := func(args ...string) (*bondPreviewConn, int, string) {
		conn := &bondPreviewConn{}
		var stdout, stderr bytes.Buffer
		code := Run(t.Context(), &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}, "order", args)
		return conn, code, stderr.String()
	}
	conn, code, stderr := run("preview", "buy", "de000syn0000", "10,000", "--type", "bond", "--currency", "eur")
	if code != 0 || conn.method != rpc.MethodOrderPreview {
		t.Fatalf("exit %d, method %q, stderr %s", code, conn.method, stderr)
	}
	p := conn.params
	if p.Action != "BUY" || p.Quantity != 0 || p.Contract.SecType != "BOND" || p.Contract.Currency != "EUR" || p.Contract.Symbol != "" ||
		p.BondOrder == nil || p.BondOrder.Identifier != "DE000SYN0000" || p.BondOrder.Face != 10000 || p.LimitPrice != nil {
		t.Fatalf("params = %+v bond %+v", p, p.BondOrder)
	}
	for name, args := range map[string][]string{
		"a limit price": {"preview", "buy", "DE000SYN0000", "10000", "--type", "BOND", "--currency", "EUR", "--limit", "98"},
		"outside RTH":   {"preview", "buy", "DE000SYN0000", "10000", "--type", "BOND", "--currency", "EUR", "--outside-rth"},
		"no currency":   {"preview", "buy", "DE000SYN0000", "10000", "--type", "BOND"},
		"a stock type":  {"preview", "buy", "DE000SYN0000", "10000", "--type", "STK", "--currency", "EUR"},
		"no face":       {"preview", "buy", "DE000SYN0000", "--type", "BOND", "--currency", "EUR"},
		"a zero face":   {"preview", "buy", "DE000SYN0000", "0", "--type", "BOND", "--currency", "EUR"},
	} {
		conn, code, stderr := run(args...)
		if code == 0 || conn.method != "" || !strings.Contains(stderr, "order preview") {
			t.Errorf("%s: exit %d, method %q, stderr %q", name, code, conn.method, stderr)
		}
	}
}

// A limit the policy does not write reads "not set" with what it refuses,
// never 0 (desk settings review, 2026-10-07).
func TestUnsetOrderLimitsReadNotSet(t *testing.T) {
	st := goldenSettings()
	st.Trading.Limits.MaxOptionContracts = rpc.SettingsInt{Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourcePolicy, Unset: true}
	st.Trading.Limits.MaxBondMaturityYears = rpc.SettingsInt{Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourcePolicy, Unset: true}
	var out bytes.Buffer
	renderSettingsText(&Env{Stdout: &out}, &st)
	text := strings.Join(strings.Fields(out.String()), " ") // rows wrap at the terminal width
	for _, want := range []string{"Max option qty (every order) not set: order previews refused until Canary writes it", "Bond maturity, buys not set: bond buys refused until Canary writes it"} {
		if !strings.Contains(text, want) {
			t.Errorf("settings view lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "at most 0 years") || strings.Contains(text, "(every order) 0") {
		t.Errorf("an unset limit read as 0:\n%s", text)
	}
	status := rpc.TradingStatus{Mode: "paper", OrderLimits: &risk.OrderLimitsInForce{Complete: true, Summary: "12,000 EUR", MaxOptionContracts: 5, BondMaturityUnset: true}}
	out.Reset()
	renderTradingStatusText(&Env{Stdout: &out}, &status)
	if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), "Bond maturity, buys not set: bond buys refused until Canary writes it") {
		t.Errorf("trading status:\n%s", out.String())
	}
}
