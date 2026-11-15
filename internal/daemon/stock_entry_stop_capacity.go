package daemon

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/osauer/canary/v3/internal/rpc"
)

// Added-lot protection shares one inventory budget with existing protection.
// Reserve existing stops first, then older added lots. A reduction affects the
// newest lot first, independent of broker snapshot ordering. OCA siblings share
// one capacity claim; independent sales retain their existing commitment.
func attachedStopCapacity(working []brokerWorkingOrder, target brokerWorkingOrder, contract rpc.ContractParams, scope brokerStateScope, held float64) (float64, bool) {
	type claim struct {
		key        string
		stop, sale float64
		added      bool
		id         int
	}
	groups := map[string]*claim{}
	key := func(r brokerWorkingOrder) string {
		if g := strings.TrimSpace(r.Order.OcaGroup); g != "" && r.Order.OcaType == 2 && !r.Unseen {
			return "oca:" + g
		}
		return fmt.Sprintf("order:%d:%d:%s", r.Order.ClientID, r.Order.OrderID, r.Order.OrderRef)
	}
	hasAdded := false
	for _, r := range working {
		if !brokerOrderIdentity(r.Order).sameContract(contract) || !strings.EqualFold(r.Order.Action, "SELL") {
			continue
		}
		protective := protectiveStopGuardOrder(r.Order)
		if !protective && !protectiveStopGuardCanaryPlaced(r.Journal, scope) {
			continue
		}
		if r.Journal != nil && r.Journal.EntryLink != nil {
			hasAdded = true
			if r.Journal.EntryState == "held" {
				continue
			}
		}
		k := key(r)
		g := groups[k]
		if g == nil {
			g = &claim{key: k, added: true, id: r.Order.OrderID}
			groups[k] = g
		}
		q := brokerOrderRemaining(r.Order)
		if protective {
			g.stop = math.Max(g.stop, q)
			g.added = g.added && r.Journal != nil && r.Journal.EntryLink != nil
			g.id = min(g.id, r.Order.OrderID)
		} else {
			g.sale = math.Max(g.sale, q)
		}
	}
	if !hasAdded {
		return 0, false
	}
	claims := make([]*claim, 0, len(groups))
	available := held
	for _, g := range groups {
		available -= g.sale
		claims = append(claims, g)
	}
	slices.SortFunc(claims, func(a, b *claim) int {
		if a.added != b.added {
			if a.added {
				return 1
			}
			return -1
		}
		return cmp.Or(cmp.Compare(a.id, b.id), strings.Compare(a.key, b.key))
	})
	for _, g := range claims {
		extra := math.Max(0, g.stop-g.sale)
		allotted := math.Min(extra, math.Max(0, available))
		if g.key == key(target) {
			return g.sale + allotted, true
		}
		available -= allotted
	}
	return 0, true
}
