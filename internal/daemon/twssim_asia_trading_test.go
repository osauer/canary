//go:build trading

package daemon

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// The Hong Kong listing of the default book: SYNH on SEHK in HKD, board lot
// 100, Hong Kong grid; the account holds no HKD.
var synhAdd = rpc.AddParams{Contract: rpc.ContractParams{Symbol: "SYNH", Currency: "HKD", Exchange: "SEHK"}, LimitPrice: 82.60, Quantity: 100}

// hkOpen installs an open Hong Kong session in the daemon's calendar; the
// simulator's starts open unless an Options lists it closed.
func hkOpen(t *testing.T, open bool) {
	t.Helper()
	t.Cleanup(marketcal.ClearSimulatedSessions)
	w := marketcal.Window{}
	if open {
		now := time.Now()
		w = marketcal.Window{Open: now.Add(-time.Hour), Close: now.Add(5 * time.Hour)}
	}
	marketcal.SimulateSession(marketcal.MarketHKHKEX, w)
}

// borrowOn writes buy_stock_on_borrowed_currency = true into the synthetic
// owner's protection policy, for this stack only.
func borrowOn(t *testing.T) func(string) {
	return func(dir string) {
		path := filepath.Join(dir, "protection-policy.toml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := withPolicyKeys(string(raw), [][]string{{"cash.sweep", "buy_stock_on_borrowed_currency = true"}})
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func planCode(p rpc.AddPlanResult, code string) (string, bool) {
	for _, w := range append(slices.Clone(p.Warnings), p.Blockers...) {
		if w.Code == code {
			return w.Message, true
		}
	}
	return "", false
}

// H1. Without the key, an HKD add on an account that never held HKD is
// refused for the missing native cash, as before.
func TestSimulatedTWSHongKongAddWithoutHKDIsRefused(t *testing.T) {
	hkOpen(t, true)
	d := startSimDesk(t, twssim.Options{}, true)
	plan := d.plan(synhAdd)
	if len(plan.Blockers) == 0 || !strings.Contains(plan.Blockers[0].Message, "native settled cash is unavailable for HKD") {
		t.Fatalf("plan without the key: %+v", plan.Blockers)
	}
	if len(d.allPlaces()) != 0 {
		t.Fatal("a refused plan reached the broker")
	}
}

// H2-H3. With the key, 100 SYNH borrow HKD against the USD cash: the plan
// warns with the amount, the protected pair goes to SEHK without Adaptive,
// the fill leaves the HKD ledger negative, and a second HKD add and a USD
// add on that book are still sized, bounded by total cash.
func TestSimulatedTWSHongKongAddOnBorrowedHKD(t *testing.T) {
	hkOpen(t, true)
	d := startSimDeskPolicies(t, twssim.Options{}, true, borrowOn(t))
	plan := d.plan(synhAdd)
	msg, ok := planCode(plan, "add_borrowed_currency")
	if len(plan.Blockers) > 0 || plan.Quantity != 100 || !ok || plan.Cash == nil || !plan.Cash.BorrowAllowed || plan.Cash.Borrowed <= 8260 {
		t.Fatalf("plan: quantity %d blockers %+v warnings %+v cash %+v", plan.Quantity, plan.Blockers, plan.Warnings, plan.Cash)
	}
	if !strings.HasPrefix(msg, "This purchase borrows 8,2") || !strings.Contains(msg, " HKD; your cash in other currencies covers it.") {
		t.Fatalf("warning %q", msg)
	}
	p := synhAdd
	p.StopPrice = 78
	placed := d.placeProtected(p)
	sent := d.realPlaces()
	if len(sent) != 2 {
		t.Fatalf("frames %+v", sent)
	}
	parent, child := sent[0], sent[1]
	if parent.Exchange != "SEHK" || parent.AlgoStrategy != "" || parent.Quantity != 100 || parent.LimitPrice != 82.6 || parent.Currency != "HKD" || parent.Transmit ||
		child.Exchange != "SEHK" || child.ParentID != parent.OrderID || child.OrderType != "STP" || child.AuxPrice != 78 || child.Quantity != 100 || !child.Transmit {
		t.Fatalf("pair frames:\nparent %+v\nchild %+v", parent, child)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
	if pos, ok := d.sim.Book().Position(900701); !ok || pos.Quantity != 100 {
		t.Fatalf("broker position %+v", pos)
	}
	if hkd := d.sim.Book().Cash("HKD"); hkd >= -8255 {
		t.Fatalf("broker HKD cash %v, want the debit", hkd)
	}
	// The daemon reads the debit from the ledger: the next HKD plan starts
	// from negative cash and is still bounded by the account's total.
	var again rpc.AddPlanResult
	d.waitFor("the ledger's HKD debit", 15*time.Second, func() bool {
		again = d.plan(synhAdd)
		return again.Cash != nil && again.Cash.Available < -8255
	})
	if len(again.Blockers) > 0 || again.Quantity != 100 || again.Cash.Spendable >= plan.Cash.Spendable {
		t.Fatalf("second HKD add: %+v cash %+v (first spendable %v)", again.Blockers, again.Cash, plan.Cash.Spendable)
	}
	usd := d.plan(synaAdd)
	if len(usd.Blockers) > 0 || usd.Quantity != 10 || usd.Cash == nil || usd.Cash.BorrowAllowed {
		t.Fatalf("USD add beside the HKD debit: %+v cash %+v", usd.Blockers, usd.Cash)
	}
	t.Logf("first HKD spendable %.2f, second %.2f, USD spendable %.2f; broker HKD %.2f", plan.Cash.Spendable, again.Cash.Spendable, usd.Cash.Spendable, d.sim.Book().Cash("HKD"))
}

// H4. Other cash short of the reserve: the borrowing add is held on cash.
func TestSimulatedTWSHongKongBorrowHeldWhenOtherCashIsShort(t *testing.T) {
	hkOpen(t, true)
	book := twssim.Default()
	book.SetCash("USD", 3000)
	d := startSimDeskPolicies(t, twssim.Options{Book: book}, true, borrowOn(t))
	plan := d.plan(synhAdd)
	if len(plan.Blockers) == 0 || plan.Quantity != 0 || plan.Binding != "cash" {
		t.Fatalf("plan: binding %q blockers %+v cash %+v", plan.Binding, plan.Blockers, plan.Cash)
	}
	if _, ok := planCode(plan, "add_borrowed_currency"); ok {
		t.Fatal("a held plan carries the borrowing warning")
	}
	t.Logf("held: %s", plan.Blockers[0].Message)
}

// H5. A closed Hong Kong session: the add is sent and rests at the broker,
// as a US add does, and fills at the open.
func TestSimulatedTWSHongKongAddWhileClosedRests(t *testing.T) {
	hkOpen(t, false)
	d := startSimDeskPolicies(t, twssim.Options{ClosedMarkets: []string{twssim.MarketHKHKEX}}, true, borrowOn(t))
	preview, rerr := d.preview(synhAdd)
	if rerr != nil {
		t.Fatalf("preview while closed: %+v", rerr)
	}
	placed, rerr := d.place(preview.PreviewToken)
	if rerr != nil || !placed.Accepted {
		t.Fatalf("place while closed: %+v", rerr)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "PreSubmitted" })
	hkOpen(t, true)
	d.sim.SetSession(twssim.MarketHKHKEX, true)
	v := d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
	if v.Filled != 100 || len(d.realPlaces()) != 1 || math.Abs(d.sim.Book().Cash("HKD")) < 8000 {
		t.Fatalf("after the open: %s, %d frames, HKD %v", orderLine(v), len(d.realPlaces()), d.sim.Book().Cash("HKD"))
	}
}

// H6. Search and lot rules need the add-search branch (add.search, the
// add_lot_size blocker); the simulator already answers both.
func TestSimulatedTWSHongKongSearchAndLot(t *testing.T) {
	requireSimTier(t)
	t.Skip("needs branch add-search (add.search and add_lot_size); the simulator side is in pkg/ibkr/twssim")
}
