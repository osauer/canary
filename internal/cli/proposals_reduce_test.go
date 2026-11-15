package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

func runReduce(t *testing.T, args ...string) (*preparedProposalCLIConn, string, int) {
	t.Helper()
	var out bytes.Buffer
	c := &preparedProposalCLIConn{}
	env := &Env{Conn: c, Stdout: &out, Stderr: &out, Origin: rpc.OrderOriginAgent}
	exit := Run(t.Context(), env, "proposals", append([]string{"reduce"}, args...))
	return c, out.String(), exit
}

// A tokened submit sends the token, the reference and the origin and nothing
// about the holding; the token is the order.
func TestReduceCLITokenSubmitCarriesOnlyTheToken(t *testing.T) {
	c, out, exit := runReduce(t, "--submit", "--preview-token", "ibkrp4.synthetic.token", "--request-ref", "ref-1", "--json")
	if exit != 0 || c.calls != 1 || c.method != rpc.MethodTradeProposalsReduceSubmit {
		t.Fatalf("exit=%d calls=%d method=%s output=%s", exit, c.calls, c.method, out)
	}
	p, ok := c.params.(rpc.TradeProposalReduceParams)
	if !ok || p.PreviewToken != "ibkrp4.synthetic.token" || p.RequestRef != "ref-1" || p.Origin != rpc.OrderOriginAgent || p.Percent != 0 || p.Symbol != "" || p.ConID != 0 || p.AdaptivePriority != "" {
		t.Fatalf("params = %+v, want the token, the reference and the origin alone", c.params)
	}
}

func TestReduceCLIPreviewCarriesTheAdaptivePriority(t *testing.T) {
	c, out, exit := runReduce(t, "SYNA", "--percent", "25", "--adaptive-priority", "Patient", "--json")
	if exit != 0 || c.method != rpc.MethodTradeProposalsReducePreview {
		t.Fatalf("exit=%d method=%s output=%s", exit, c.method, out)
	}
	p, ok := c.params.(rpc.TradeProposalReduceParams)
	if !ok || p.Symbol != "SYNA" || p.Percent != 25 || p.AdaptivePriority != "Patient" || p.PreviewToken != "" || p.Origin != "" {
		t.Fatalf("params = %+v, want a Patient preview of SYNA at 25%%", c.params)
	}
}

// A basket submit sends one token per leg; the percent is the preview's.
func TestReduceCLIPortfolioSubmitCarriesOneTokenPerLeg(t *testing.T) {
	c, out, exit := runReduce(t, "--portfolio", "--submit", "--preview-token", "ibkrp4.leg.one", "--preview-token", "ibkrp4.leg.two", "--request-ref", "basket-1", "--json")
	if exit != 0 || c.method != rpc.MethodTradeProposalsReducePortfolioSubmit {
		t.Fatalf("exit=%d method=%s output=%s", exit, c.method, out)
	}
	p, ok := c.params.(rpc.TradeProposalReducePortfolioParams)
	if !ok || len(p.PreviewTokens) != 2 || p.PreviewTokens[0] != "ibkrp4.leg.one" || p.PreviewTokens[1] != "ibkrp4.leg.two" || p.RequestRef != "basket-1" || p.Origin != rpc.OrderOriginAgent || p.Percent != 0 {
		t.Fatalf("params = %+v, want both tokens and the reference", c.params)
	}
}

// Mixed inputs are refused before any RPC: a token names the whole order.
func TestReduceCLIRefusesAmbiguousTokenInputsBeforeRPC(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"token without submit", []string{"--preview-token", "ibkrp4.x"}, "go with --submit"},
		{"request-ref without submit", []string{"SYNA", "--percent", "50", "--request-ref", "r"}, "go with --submit"},
		{"token with a symbol", []string{"SYNA", "--submit", "--preview-token", "ibkrp4.x"}, "drop SYMBOL"},
		{"token with a percent", []string{"--submit", "--percent", "50", "--preview-token", "ibkrp4.x"}, "drop SYMBOL"},
		{"token with a priority", []string{"--submit", "--adaptive-priority", "Urgent", "--preview-token", "ibkrp4.x"}, "drop SYMBOL"},
		{"two tokens for one holding", []string{"--submit", "--preview-token", "ibkrp4.x", "--preview-token", "ibkrp4.y"}, "one holding takes one"},
		{"unknown priority", []string{"SYNA", "--percent", "50", "--adaptive-priority", "fast"}, "Normal, Patient, Urgent or none"},
		{"no holding and no token", []string{"--percent", "50"}, "provide a SYMBOL or --con-id"},
		{"no percent and no token", []string{"SYNA"}, "--percent must be between"},
	} {
		c, out, exit := runReduce(t, tc.args...)
		if exit == 0 || c.calls != 0 || !strings.Contains(out, tc.want) {
			t.Errorf("%s: exit=%d calls=%d output=%q, want a refusal saying %q", tc.name, exit, c.calls, out, tc.want)
		}
	}
}
