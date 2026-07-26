package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestPolicyV3FlowAndReconcileEvidenceRendering(t *testing.T) {
	declared, statement := 1005.0, 1000.0
	c := rpc.CapitalStateReport{
		DeclaredCumFlowsBase: &declared, StatementCumFlowsBase: &statement,
		FlowSource: rpc.CapitalFlowSourceStatement, LastReconcileReportID: "recon-clean",
		LastReconcileSource: rpc.ReconcileSourceAutomatic,
	}
	flows := capitalFlowComparison(c, "EUR")
	if !strings.Contains(flows, "declared") || !strings.Contains(flows, "statement-authoritative") || !strings.Contains(flows, "using statement") {
		t.Fatalf("flow comparison = %q", flows)
	}
	if got := reconcileEvidenceDetail(c); got != " (report recon-clean, automatic)" {
		t.Fatalf("automatic evidence = %q", got)
	}
	c.LastReconcileSource = rpc.ReconcileSourceHuman
	if got := reconcileEvidenceDetail(c); got != " (report recon-clean, human sign-off)" {
		t.Fatalf("human evidence = %q", got)
	}
	if got := ledgerNeverVerifiedMessage(c); !strings.Contains(got, "qualifying clean report extends automatically") {
		t.Fatalf("v3 missing evidence = %q", got)
	}
	if got := ledgerNeverVerifiedMessage(rpc.CapitalStateReport{}); got != "  ledger check        never verified against broker statements — run `canary recon`, then sign off the report it prints" {
		t.Fatalf("v2 missing evidence changed = %q", got)
	}
}

func TestReconConfirmedAndAutoExtendRendering(t *testing.T) {
	amount := -925.5
	row := rpc.ReconException{ValueDate: time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC), Type: "Deposits/Withdrawals", AmountBase: &amount, Description: "confirmed fixture"}
	line := formatReconDisclosedFlow(row)
	for _, want := range []string{"2026-07-18", "Deposits/Withdrawals", "-925.50", "confirmed fixture"} {
		if !strings.Contains(line, want) {
			t.Fatalf("confirmed line %q missing %q", line, want)
		}
	}
	res := rpc.ReconResult{ReportID: "recon-current", LastAutoExtendReportID: "recon-current", LastAutoExtendedAt: time.Date(2026, 7, 18, 12, 0, 0, 0, time.Local)}
	if got := formatReconAutoExtend(res); !strings.Contains(got, "recon-current") || !strings.Contains(got, "current report") {
		t.Fatalf("auto extend line = %q", got)
	}
	if got := reconCleanEvidenceMessage(res); !strings.Contains(got, "has extended") {
		t.Fatalf("current clean evidence = %q", got)
	}
	res.LastAutoExtendReportID = "recon-previous"
	if got := reconCleanEvidenceMessage(res); !strings.Contains(got, "has not been recorded") {
		t.Fatalf("unrecorded clean evidence = %q", got)
	}
}
