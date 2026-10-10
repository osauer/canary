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

// Order readers reuse only the pure fold of the append-only order journal.
// Every read checks storage authority; scope and DAY expiry are applied anew.
// Execution and reconciliation continue to use their existing fenced reads.
type orderReadCache struct {
	mu         sync.Mutex
	store      *corestore.Store
	head       corestore.AuthorityHead
	projection *orderReadProjection
}

type orderReadProjection struct {
	views       []rpc.OrderView
	eventsByKey map[string][]rpc.OrderEvent
	lastEvent   string
}

func (s *Server) loadOrderReadProjection() (*orderReadProjection, error) {
	if s == nil || s.orderJournal == nil {
		return nil, fmt.Errorf("%w: order journal is unavailable", ErrTradingDisabled)
	}
	c := &s.orderReads
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx := context.Background()
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(orderJournalHeadSettle)
		}
		store, err := s.orderJournal.coreStore()
		if err != nil {
			return nil, err
		}
		before, err := store.AuthorityHead(ctx)
		if err != nil {
			return nil, err
		}
		reuse := false
		if c.store == store {
			reuse, err = orderReadHistoryUnchanged(ctx, store, c.head, before)
			if err != nil {
				return nil, err
			}
		}
		if !reuse {
			events, err := s.orderJournal.LoadEvents(0)
			if err != nil {
				return nil, err
			}
			after, err := store.AuthorityHead(ctx)
			if err != nil {
				return nil, err
			}
			current, err := s.orderJournal.coreStore()
			if err != nil {
				return nil, err
			}
			stable, err := orderReadHistoryUnchanged(ctx, store, before, after)
			if err != nil {
				return nil, err
			}
			if current != store || !stable {
				continue
			}
			before = after
			c.store = store
			c.projection = &orderReadProjection{views: buildOrderViews(events), eventsByKey: buildOrderEventsByKey(events)}
			if len(events) > 0 {
				last := events[len(events)-1]
				if !last.At.IsZero() {
					c.projection.lastEvent = fmt.Sprintf("%s %s at %s", last.Type, orderJournalEventLabel(last), last.At.Format(time.RFC3339))
				}
			}
		}
		c.head = before
		return c.projection, nil
	}
	return nil, errOrderJournalHeadUnstable
}

func orderReadHistoryUnchanged(ctx context.Context, store *corestore.Store, before, after corestore.AuthorityHead) (bool, error) {
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

// Returned views and trailing-stop terms belong to the caller. The retained
// fold and its event map are immutable, including across broker reconciliation.
func (p *orderReadProjection) viewsAt(now time.Time) []rpc.OrderView {
	views := slices.Clone(p.views)
	for i := range views {
		views[i].Trail = cloneTrailSpec(views[i].Trail)
	}
	inferDayOrderExpiry(views, p.eventsByKey, now)
	return views
}

func (s *Server) orderHealthSnapshot() ([]rpc.OrderView, string, error) {
	projection, err := s.loadOrderReadProjection()
	if err != nil {
		return nil, "", err
	}
	return projection.viewsAt(s.orderNow()), projection.lastEvent, nil
}
