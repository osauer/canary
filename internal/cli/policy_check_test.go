package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Synthetic config, protection policy and constitution: a sweep cap above
// the order cap in force.
const policyCheckCLIProtection = `kind = "canary.protection_policy"
schema_version = 1
policy_id = "protection-test"
policy_version = 1

[authority]
close_reduce_only = true
auto_submit = false

[cash.sweep]
enabled = true
mode = "shadow"
max_order_notional = 8000.0
`

// policyCheckCLIConstitution carries [order_limits] with the floor filled in:
// 5% of NLV between the floor and a 100,000 ceiling.
const policyCheckCLIConstitution = `kind = "canary.risk_policy"
schema_version = 2
policy_id = "constitution-test"
policy_version = 1

[capital]
base_currency = "EUR"

[order_limits]
max_order_floor_base = %s
max_order_pct_nlv = 5.0
max_order_ceiling_base = 100000.0
max_option_contracts = 5
allow_stock_short = false
allow_option_sell_to_open = false
max_bond_maturity_years = 30
`

func writePolicyCheckHome(t *testing.T, floor string) string {
	t.Helper()
	home := isolatePolicyHome(t)
	dir := filepath.Join(home, ".config", "ibkr")
	if err := os.MkdirAll(filepath.Join(dir, "policies"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[trading]\nmode = \"paper\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policies", "risk-policy.toml"), fmt.Appendf(nil, policyCheckCLIConstitution, floor), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policies", "protection-policy.toml"), []byte(policyCheckCLIProtection), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// `canary policy check --offline` runs without a daemon, reads the files
// only, and exits 1 only for an error finding.
func TestPolicyCheckOfflineExitsOnErrorsOnly(t *testing.T) {
	if !PolicyLocalSubcommand([]string{"check", "--offline"}) || PolicyLocalSubcommand([]string{"check"}) {
		t.Fatal("check --offline must run without the daemon and check alone must not")
	}
	cfg := writePolicyCheckHome(t, "5000.0")
	var out, errb bytes.Buffer
	code := RunPolicyLocal(context.Background(), &Env{Stdout: &out, Stderr: &errb}, []string{"check", "--offline", "--config", cfg})
	if code != 1 || !strings.Contains(out.String(), "ERROR cap_above_trading_max") || !strings.Contains(out.String(), "every check against the live book: --offline") {
		t.Fatalf("exit %d stderr %q:\n%s", code, errb.String(), out.String())
	}

	cfg = writePolicyCheckHome(t, "9000.0")
	out.Reset()
	code = RunPolicyLocal(context.Background(), &Env{Stdout: &out, Stderr: &errb}, []string{"check", "--offline", "--json", "--config", cfg})
	var report rpc.PolicyCheckReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if code != 0 || report.Errors != 0 || report.Book != nil {
		t.Fatalf("exit %d report %+v", code, report)
	}
}

// policyCheckConn answers the read-only calls the live check makes.
type policyCheckConn struct {
	methods []string
	failAcc bool
}

func (c *policyCheckConn) Call(_ context.Context, method string, _, out any) error {
	c.methods = append(c.methods, method)
	switch method {
	case rpc.MethodAccountSummary:
		if c.failAcc {
			return errors.New("gateway unavailable")
		}
		*out.(*rpc.AccountResult) = rpc.AccountResult{BaseCurrency: "EUR", NetLiquidation: 120000}
	case rpc.MethodPositionsList:
		*out.(*rpc.PositionsResult) = rpc.PositionsResult{}
	case rpc.MethodRiskPolicySnapshot:
		*out.(*rpc.RiskPolicyResult) = rpc.RiskPolicyResult{Files: []rpc.PolicyFileStatus{{Policy: "protection", Status: "drift"}}}
	default:
		return errors.New("unexpected method " + method)
	}
	return nil
}

func (c *policyCheckConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return errors.New("no stream")
}

// With a daemon the check reads the book, sizes the order cap in force from
// its NLV (5% of 120,000 is 6,000 EUR, above the 5,000 floor) and reads the
// managers' drift; it never calls a write method.
func TestPolicyCheckReadsTheLiveBook(t *testing.T) {
	cfg := writePolicyCheckHome(t, "5000.0")
	conn := &policyCheckConn{}
	var out bytes.Buffer
	code := runPolicy(context.Background(), &Env{Stdout: &out, Stderr: &out, Conn: conn}, []string{"check", "--config", cfg})
	text := out.String()
	for _, want := range []string{"live book    NLV 120,000 EUR", "6,000 EUR (5% of NLV)", "version_not_bumped", "ERROR cap_above_trading_max"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output lacks %q:\n%s", want, text)
		}
	}
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	for _, m := range conn.methods {
		switch m {
		case rpc.MethodAccountSummary, rpc.MethodPositionsList, rpc.MethodRiskPolicySnapshot:
		default:
			t.Fatalf("unexpected call %s", m)
		}
	}

	conn = &policyCheckConn{failAcc: true}
	out.Reset()
	runPolicy(context.Background(), &Env{Stdout: &out, Stderr: &out, Conn: conn}, []string{"check", "--config", cfg})
	if !strings.Contains(out.String(), "the account read failed (gateway unavailable)") {
		t.Fatalf("a failed account read is not named:\n%s", out.String())
	}
}
