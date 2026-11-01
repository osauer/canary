package daemon

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The risk projection belongs to the full plan, while the currently offered
// order can fill only a capped installment. The owner must be able to tell
// those amounts apart even when no further eligible leg remains in the plan.
func TestArmedSelectionProjectionDistinguishesPlanFromCurrentOrders(t *testing.T) {
	for _, zeroRoom := range []bool{false, true} {
		t.Run(fmt.Sprintf("zero_room_%t", zeroRoom), func(t *testing.T) {
			pos := mandateReductionBook(mandateReductionCall("SYNA", 9001, 50, 100, 100, 20, "20270115"))
			if zeroRoom {
				put := mandateReductionCall("SPY", 9020, 1, 400, 420, 5, "20270115")
				put.Right = "P"
				pos.Options = append(pos.Options, put)
			}
			in := mandateReductionFixture(t, pos, 2, 200000, 2)
			if zeroRoom {
				book := *in.Input
				book.Capital.Room, book.Capital.BudgetAnnualSD = 0, 0
				in = mandateReductionVerdict(book, 2)
			}
			in.MaxOrderNotional = 10000
			rows, st := mandateReductionRun(t, in, pos)
			if len(rows) != 1 || len(st.Plan) != 1 || st.Rows != 1 || st.ProjectedLossBase == nil {
				t.Fatalf("one current order and one planned cut required: rows=%d status=%+v", len(rows), st)
			}
			p, plan := rows[0], st.Plan[0]
			m := p.Mandate
			afterOrder := mandateLossAfter(t, in, map[int]float64{9001: float64(p.Quantity)})
			afterPlan := mandateLossAfter(t, in, map[int]float64{9001: float64(m.Units)})
			if p.Quantity != 5 || m.Units <= p.Quantity || plan.Quantity != p.Quantity || plan.Units != m.Units ||
				plan.Rank != 1 || m.Rank != 1 || m.BatchRows != 1 || plan.Phase != m.Phase || m.Phase != rpc.MandatePhaseGap ||
				math.Abs(*st.ProjectedLossBase-afterPlan) > 1e-6 || m.LossAfterBase != *st.ProjectedLossBase || afterOrder <= afterPlan ||
				math.Abs(m.LossCutBase-(st.LossBase-afterPlan)) > 1e-6 || plan.LossCutBase != m.LossCutBase || st.TargetReached == zeroRoom {
				t.Fatalf("full plan and current installment arithmetic disagree: current=%+v plan=%+v status=%+v after order=%g after plan=%g", m, plan, st, afterOrder, afterPlan)
			}
			// These are semantic scope assertions, not exact copy/layout: both
			// displayed projections must identify the full plan and its fill condition.
			projection := regexp.MustCompile(`(?i)full plan.*` + regexp.QuoteMeta(mandateMoney(afterPlan, "EUR")) + `.*(?:if|once|after) all planned units (?:have )?fill`)
			for surface, text := range map[string]string{"status": st.Reason, "row": p.Reason} {
				if !projection.MatchString(text) {
					t.Errorf("%s projection does not bind its amount to the complete plan filling: %s", surface, text)
				}
			}
			if !strings.Contains(st.Reason, "offers 1 order") || !strings.Contains(st.Reason, "1 partial installment") {
				t.Errorf("status omits the current capped order count: %s", st.Reason)
			}
			details := strings.Join(p.Details, "\n")
			plannedCut := regexp.MustCompile(fmt.Sprintf(`(?i)planned cut 1 of 1.*(?:if|once|after) all %d planned contracts fill.*%s`, m.Units, regexp.QuoteMeta(mandateMoney(m.LossCutBase, "EUR"))))
			currentOrder := regexp.MustCompile(fmt.Sprintf(`(?i)current order.*%d of %d planned contracts.*after.*fills.*measures.*remaining`, p.Quantity, m.Units))
			if !plannedCut.MatchString(details) || !currentOrder.MatchString(details) {
				t.Errorf("details do not distinguish planned risk removal from the current order and later remeasurement: %s", details)
			}
			if zeroRoom && (!strings.Contains(st.Reason, "on this verdict") || !strings.Contains(details, "on this verdict") ||
				strings.Contains(st.Reason+details, "nothing reduces further") || strings.Contains(details, "nothing else reduces further")) {
				t.Errorf("unreached target reads as a promise to stop after this installment: status=%s details=%s", st.Reason, details)
			}

			// Filling this order leaves another capped installment, not the
			// already advertised full-plan end state. A fresh verdict carries it.
			filled := mandateReductionBook(mandateReductionCall("SYNA", 9001, 45, 100, 100, 20, "20270115"))
			if zeroRoom {
				filled.Options = append(filled.Options, pos.Options[1])
			}
			next := mandateReductionVerdict(mandateReductionRelegs(t, in, filled), 3)
			next.MaxOrderNotional, next.Previous, next.Episode = 10000, rows, st.Episode
			again, after := mandateReductionRun(t, next, filled)
			if len(again) != 1 || again[0].Quantity != 5 || again[0].Mandate.Units != m.Units-p.Quantity ||
				after.Episode != st.Episode || after.State != rpc.MandateReductionReducing {
				t.Fatalf("fresh post-fill verdict lost the planned remainder: rows=%+v status=%+v", again, after)
			}
			t.Logf("held=50, current order=%d, planned cut=%d, loss before=%.0f, after current order=%.0f, after full plan=%.0f", p.Quantity, m.Units, st.LossBase, afterOrder, afterPlan)
		})
	}
}
