package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu"
)

// opendRegimeCodes are the names the regime and the brief read every minute, after
// the book's. An input Futu does not list is simply not streamed.
var opendRegimeCodes = []string{"US.SPY", "US.QQQ", "US.HYG"}

// opendLineBudget is which names hold a streaming line. There is no number here to
// configure: the book comes first and the regime after it, recomputed from the
// current book on every refresh, and OpenD's own quota says how far that reaches.
type opendLineBudget struct {
	mu     sync.Mutex
	wanted map[string]bool
	// reason is why the last refresh could not give a wanted name its line.
	reason string
	// quotes and snapshots count the last positions read's marks.
	quotes, snapshots int
	// refusedMarkets are the markets that read was refused for want of a right.
	refusedMarkets []string
}

// allowed reports whether an ad hoc stream may take a line for the code.
func (b *opendLineBudget) allowed(code string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wanted[code]
}

func (b *opendLineBudget) record(wanted []string, reason string) {
	set := make(map[string]bool, len(wanted))
	for _, code := range wanted {
		set[code] = true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.wanted, b.reason = set, reason
}

// recordRefusedRights keeps the markets a mark read was refused for, by right.
func (b *opendLineBudget) recordRefusedRights(err error) {
	var markets []string
	for _, one := range refusedQuoteRights(err) {
		markets = append(markets, one.Market)
	}
	slices.Sort(markets)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refusedMarkets = slices.Compact(markets)
}

// refusedQuoteRights is every missing right an error carries; a read of several
// markets joins one refusal per market.
func refusedQuoteRights(err error) []*futu.QuoteRightError {
	if one, ok := err.(*futu.QuoteRightError); ok {
		return []*futu.QuoteRightError{one}
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return nil
	}
	var out []*futu.QuoteRightError
	for _, each := range joined.Unwrap() {
		out = append(out, refusedQuoteRights(each)...)
	}
	return out
}

func (b *opendLineBudget) recordMarks(quotes, snapshots int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.quotes, b.snapshots = quotes, snapshots
}

// orderedLineWants is the book first, the regime after it, each name once.
func orderedLineWants(held []string) []string {
	out := make([]string, 0, len(held)+len(opendRegimeCodes))
	for _, code := range append(slices.Clone(held), opendRegimeCodes...) {
		if !slices.Contains(out, code) {
			out = append(out, code)
		}
	}
	return out
}

// refreshOpenDLines moves the session's lines toward the book. OpenD's quota says
// how many lines it can hold, counting what this session already has, since another
// client on the same OpenD spends the same quota. A line outside the reach is
// released, and one still inside its minimum hold waits for the next refresh, so a
// new holding always takes a line within a minute of arriving. A refused subscribe
// is the name's snapshot delivery, not a failure.
func (s *Server) refreshOpenDLines(ctx context.Context, held []string) {
	conn := s.opend.Connector()
	load, err := conn.RefreshLoad(ctx)
	if err != nil {
		return
	}
	lines := conn.Lines()
	capacity := len(lines) + load.SubscriptionsRemaining
	wants := orderedLineWants(held)
	reach := wants[:min(len(wants), max(capacity, 0))]
	for code := range lines {
		if !slices.Contains(reach, code) {
			if err := conn.Unsubscribe(ctx, code); err != nil && !errors.Is(err, futu.ErrSubscriptionHeld) {
				s.logger.Debugf("opend: release %s: %v", code, err)
			}
		}
	}
	reason := ""
	if len(reach) < len(wants) {
		reason = "subscription quota"
	}
	for _, code := range reach {
		if _, ok := lines[code]; ok {
			continue
		}
		if _, err := conn.Subscribe(ctx, code); err != nil {
			if errors.Is(err, futu.ErrQuotaExhausted) {
				reason = "subscription quota"
				break
			}
			if !errors.Is(err, futu.ErrPaced) {
				s.logger.Debugf("opend: line for %s: %v", code, err)
			}
		}
	}
	s.opendBudget.record(reach, reason)
}

// opendMarks prices held names: a streaming line where one exists, else one
// snapshot batch. The second return is the codes that came from the batch.
func (s *Server) opendMarks(ctx context.Context, codes []string) (map[string]*broker.Quote, map[string]bool) {
	marks := s.opend.Snapshot()
	var missing []string
	for _, code := range codes {
		if marks[code] == nil || marks[code].Last <= 0 {
			missing = append(missing, code)
		}
	}
	fromBatch := map[string]bool{}
	if len(missing) == 0 {
		s.opendBudget.recordRefusedRights(nil)
		return marks, fromBatch
	}
	batch, err := s.opend.ReadSnapshots(ctx, missing)
	if err != nil {
		s.logger.Debugf("opend: snapshot batch: %v", err)
	}
	s.opendBudget.recordRefusedRights(err)
	for code, quote := range batch {
		marks[code] = quote
		fromBatch[code] = true
	}
	return marks, fromBatch
}

// opendQuoteSentence is the brief's line on the split, empty when every quote
// streams and nothing was refused.
func (s *Server) opendQuoteSentence() string {
	load := s.opend.Connector().Load()
	s.opendBudget.mu.Lock()
	quotes, snapshots := s.opendBudget.quotes, s.opendBudget.snapshots
	refused := s.opendBudget.refusedMarkets
	s.opendBudget.mu.Unlock()
	sentence := ""
	if snapshots > 0 && quotes > 0 {
		sentence = fmt.Sprintf("%d of %d quotes are snapshots", snapshots, quotes)
		if load.SubscriptionQuota > 0 {
			sentence += fmt.Sprintf("; %d of %d subscriptions in use", load.SubscriptionsUsed, load.SubscriptionQuota)
		}
	}
	if len(refused) > 0 {
		missing := fmt.Sprintf("this OpenD login has no %[1]s quote right, so %[1]s holdings show the price OpenD reports with the holding, not a market quote", strings.Join(refused, ", "))
		if sentence != "" {
			return sentence + "; " + missing
		}
		return missing
	}
	return sentence
}

// quoteDeliveryOf is the wire's delivery word for a seam quote.
func quoteDeliveryOf(q *broker.Quote) string {
	if q == nil {
		return ""
	}
	return q.Delivery
}

// errStreamingNotAvailable answers a stream request for a name without a line.
var errStreamingNotAvailable = &rpc.Error{Code: rpc.CodeNotApplicable, Message: "OpenD streams the held and regime names; read this name as one quote"}
