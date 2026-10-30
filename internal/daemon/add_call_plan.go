package daemon

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func (s *Server) handleAddCallPlan(ctx context.Context, req *rpc.Request) (*rpc.AddCallPlanResult, error) {
	var p rpc.AddCallPlanParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	c := p.Contract
	date, err := time.Parse("20060102", c.Expiry)
	if err != nil || !date.After(time.Now().Add(7*24*time.Hour)) || c.ConID <= 0 || c.SecType != "OPT" || c.Currency != "USD" || c.Exchange != "SMART" || (c.Right != "C" && c.Right != "P") || c.Multiplier != 100 || c.TradingClass == "" || !positiveFinite(p.Budget) || p.Budget > 1e9 || !positiveFinite(p.LimitPrice) {
		return nil, errBadRequest("Choose an exact long call or put, entry limit and positive budget")
	}
	side := s.addSide(c.Right)
	windows, hoursOK := s.addCallHours(ctx, c)
	if !addCallSessionLive(windows, hoursOK, s.orderNow()) {
		return nil, errBadRequest("Options session must be open to plan a " + side.noun())
	}
	// A put is sized within the shares it may still protect, so the plan
	// never offers more than the protective put gate accepts.
	optionCap, room := s.addOptionCap(), -1
	if side.right == "P" {
		cover := s.readAddPutCover(ctx, c)
		if cover == nil {
			return nil, errBadRequest("The held shares and working orders cannot be read to size a protective put")
		}
		if cover.RoomContracts == 0 {
			return nil, errBadRequest("No held shares are left for a protective put to cover")
		}
		room = cover.RoomContracts
	}
	plan, err := planAddQuantity(ctx, p, optionCap, room, s.previewOrder)
	if err != nil {
		return nil, err
	}
	// A preview can straddle the close. A completed plan must still belong
	// to the open options session; broker data_type alone is not authority.
	if !addCallSessionLive(windows, hoursOK, s.orderNow()) {
		return nil, errBadRequest("Options session must be open to plan a " + side.noun())
	}
	return plan, nil
}

// addCallHours reads the call's class hours. Without them the class closes
// with the stock market, the earlier close, so a failed read never extends it.
func (s *Server) addCallHours(ctx context.Context, call rpc.ContractParams) ([]ibkr.TradingWindow, bool) {
	c := s.gatewayConnector()
	if c == nil {
		return nil, false
	}
	binding, ok := c.CaptureSession()
	if !ok {
		return nil, false
	}
	exact, err := c.ResolveOrderContractForSession(ctx, binding, *previewIBKRContract(call), 5*time.Second)
	if err != nil || exact.Contract.ConID != call.ConID {
		return nil, false
	}
	return exact.SessionWindows()
}

func addCallSessionLive(windows []ibkr.TradingWindow, hoursOK bool, now time.Time) bool {
	return addClassOptionsCalendar(addOptionsCalendar(now), windows, hoursOK, now).mode == "live"
}

// addOptionCap is the option contracts one order may carry under the order
// limits in force, or 0 when they cannot be read (the preview still judges).
func (s *Server) addOptionCap() int {
	if limits := s.orderLimitsInForce(""); limits.Complete && limits.MaxOptionContracts > 0 {
		return limits.MaxOptionContracts
	}
	return 0
}

// Every candidate, including a rounded-up lot, receives its own exact broker
// preview. Rounding is an explicit draft preference, never spending authority.
// A known option cap holds the size, rounding included: a plan never sizes
// into a refusal (owner review 2026-10-23 16:50 CEST).
func planCallQuantity(ctx context.Context, p rpc.AddCallPlanParams, maxContracts int, previewOrder func(context.Context, rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error)) (*rpc.AddCallPlanResult, error) {
	c := p.Contract
	n := int(math.Min(1000000, math.Floor(p.Budget/(p.LimitPrice*100))))
	held := maxContracts > 0 && n > maxContracts
	if held {
		n = maxContracts
	}
	rounded := false
	for attempt := 0; attempt < 4 && n > 0; attempt++ {
		preview, err := previewOrder(ctx, rpc.OrderPreviewParams{Action: rpc.OrderActionBuy, Contract: c, Quantity: n, OrderType: "LMT", LimitPrice: new(p.LimitPrice), Strategy: "explicit-limit", TIF: "DAY", AdaptivePriority: "Normal"})
		if err != nil {
			return nil, err
		}
		fee, ok := cashSweepFeeUpper(preview, c.Currency)
		if !ok {
			return nil, fmt.Errorf("an accepted broker fee bound is required to size this call")
		}
		d := preview.Draft
		if d.Action != rpc.OrderActionBuy || d.OrderType != "LMT" || d.Contract.SecType != "OPT" || d.Contract.Right != c.Right || d.Contract.Multiplier != 100 || d.Contract.Currency != c.Currency || d.Contract.Exchange != c.Exchange || d.Contract.ConID != c.ConID || d.Contract.Symbol != c.Symbol || d.Contract.Strike != c.Strike || d.Contract.Expiry != c.Expiry || d.Contract.TradingClass != c.TradingClass || d.Quantity != n || d.LimitPrice != p.LimitPrice {
			return nil, fmt.Errorf("option identity or terms changed during sizing")
		}
		debit := float64(n)*p.LimitPrice*100 + fee
		if !positiveFinite(debit) {
			return nil, fmt.Errorf("call debit is not finite and positive")
		}
		if debit <= p.Budget || rounded {
			if p.RoundSize && !rounded {
				next := roundCallQuantity(n)
				if maxContracts > 0 {
					next = min(next, maxContracts)
				}
				if next != n {
					n = next
					rounded = true
					continue
				}
			}
			out := &rpc.AddCallPlanResult{RoundSize: p.RoundSize, OverBudget: debit > p.Budget, Contract: d.Contract, LimitPrice: p.LimitPrice, Budget: p.Budget, Quantity: n, FeeBound: fee, Debit: debit, AsOf: time.Now()}
			if held && n == maxContracts {
				out.HeldToCap, out.OptionCap = true, maxContracts
			}
			return out, nil
		}
		next := int(math.Floor((p.Budget - fee) / (p.LimitPrice * 100)))
		n = min(n-1, next)
	}
	return nil, fmt.Errorf("no checked call quantity fits this budget")
}

// planAddQuantity sizes under the option cap and, for a put, the cover room
// (-1 for none), whichever is smaller, and says which held the size.
func planAddQuantity(ctx context.Context, p rpc.AddCallPlanParams, optionCap, room int, previewOrder func(context.Context, rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error)) (*rpc.AddCallPlanResult, error) {
	sizeCap := optionCap
	if room > 0 && (sizeCap == 0 || room < sizeCap) {
		sizeCap = room
	}
	plan, err := planCallQuantity(ctx, p, sizeCap, previewOrder)
	if err != nil || !plan.HeldToCap || sizeCap != room {
		return plan, err
	}
	plan.CoverCapped = true
	if sizeCap != optionCap {
		plan.HeldToCap, plan.OptionCap = false, 0
	}
	return plan, nil
}

func roundCallQuantity(n int) int {
	if n < 3 {
		return n
	}
	return ((n + 2) / 5) * 5
}
