package daemon

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Health readers reuse only the pure fold of the append-only order journal.
// Every read checks storage authority; scope and DAY expiry are applied anew.
// Execution and reconciliation continue to use their existing fenced reads.
type orderHealthCache struct {
	mu          sync.Mutex
	store       *corestore.Store
	head        corestore.AuthorityHead
	views       []rpc.OrderView
	eventsByKey map[string][]rpc.OrderEvent
	lastEvent   string
}

func (s *Server) orderHealthSnapshot() ([]rpc.OrderView, string, error) {
	if s == nil || s.orderJournal == nil {
		return nil, "", fmt.Errorf("%w: order journal is unavailable", ErrTradingDisabled)
	}
	c := &s.orderHealth
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx := context.Background()
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(orderJournalHeadSettle)
		}
		store, err := s.orderJournal.coreStore()
		if err != nil {
			return nil, "", err
		}
		before, err := store.AuthorityHead(ctx)
		if err != nil {
			return nil, "", err
		}
		reuse := false
		if c.store == store {
			reuse, err = healthOrderHistoryUnchanged(ctx, store, c.head, before)
			if err != nil {
				return nil, "", err
			}
		}
		if !reuse {
			events, err := s.orderJournal.LoadEvents(0)
			if err != nil {
				return nil, "", err
			}
			after, err := store.AuthorityHead(ctx)
			if err != nil {
				return nil, "", err
			}
			current, err := s.orderJournal.coreStore()
			if err != nil {
				return nil, "", err
			}
			stable, err := healthOrderHistoryUnchanged(ctx, store, before, after)
			if err != nil {
				return nil, "", err
			}
			if current != store || !stable {
				continue
			}
			before = after
			c.store = store
			c.views = buildOrderViews(events)
			c.eventsByKey = buildOrderEventsByKey(events)
			c.lastEvent = ""
			if len(events) > 0 {
				last := events[len(events)-1]
				if !last.At.IsZero() {
					c.lastEvent = fmt.Sprintf("%s %s at %s", last.Type, orderJournalEventLabel(last), last.At.Format(time.RFC3339))
				}
			}
		}
		c.head = before
		views := slices.Clone(c.views)
		inferDayOrderExpiry(views, c.eventsByKey, s.orderNow())
		return views, c.lastEvent, nil
	}
	return nil, "", errOrderJournalHeadUnstable
}

func healthOrderHistoryUnchanged(ctx context.Context, store *corestore.Store, before, after corestore.AuthorityHead) (bool, error) {
	if before.AuthorityEpoch != after.AuthorityEpoch || after.LastEventSeq < before.LastEventSeq {
		return false, nil
	}
	if before.LastEventSeq == after.LastEventSeq {
		return true, nil
	}
	// Other observation producers share the global head. An indexed one-row
	// probe avoids replaying orders when only those producers changed it.
	newOrders, err := store.LoadOrderEvents(ctx, corestore.OrderQuery{AfterEventSeq: before.LastEventSeq, Limit: 1})
	return len(newOrders) == 0, err
}
