package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// IBKR's account rows answer ExchangeRate 1 for every currency of this account
// on $LEDGER:ALL, $LEDGER:<CCY> and the account-updates stream alike (read off
// the wire 2026-11-06), so they cannot say what rate a foreign slice of the
// account value stands on. The evidence is then the source order caps already
// rest on: a live two-sided quote of the pair from the current broker session,
// kept with its own observation time. It is observed at the sample, never
// applied to an earlier one, and it is independent of the account value, so a
// residual between that value and its currency slices stays visible.
const (
	// reconFXQuoteMaxAge bounds the distance between a quote and the sample it
	// values. An evidence window, not a policy limit: five minutes of drift on
	// a whole book is far below the divergence the comparison tests.
	reconFXQuoteMaxAge = 5 * time.Minute
	// reconFXQuoteRetry spaces the attempts per pair, so one failed read leaves
	// the previous quote inside reconFXQuoteMaxAge.
	reconFXQuoteRetry = 2 * time.Minute
)

type reconFXQuote struct {
	Rate   float64 // base units per unit of the currency
	At     time.Time
	Source string
}

// validAt refuses a quote that says nothing a broker row would not (a unit
// rate) or that is too far from the sample in either direction: the read that
// takes the quote finishes just after the sample's own clock.
func (q reconFXQuote) validAt(asOf time.Time) bool {
	if !positiveFinite(q.Rate) || q.Rate == 1 || q.At.IsZero() || q.Source == "" {
		return false
	}
	return q.At.Sub(asOf).Abs() <= reconFXQuoteMaxAge
}

type reconFXFetch func(ctx context.Context, currency, base string) (reconFXQuote, error)

// reconFXQuoteCache holds the latest quote per pair between attempts, so the
// frequent account reads share one market-data request every reconFXQuoteRetry.
type reconFXQuoteCache struct {
	mu        sync.Mutex
	latest    map[string]reconFXQuote
	attempted map[string]time.Time
}

// quotes returns the held quotes for the currencies the account rows leave
// without a usable rate. A currency with a real broker rate takes no request.
func (c *reconFXQuoteCache) quotes(ctx context.Context, raw *ibkrlib.RawAccountSummary, now time.Time, fetch reconFXFetch) map[string]reconFXQuote {
	if raw == nil || !raw.BaseCurrencyProvenance.Proven() {
		return nil
	}
	base := normCcy(raw.BaseCurrency)
	out := map[string]reconFXQuote{}
	for currency, row := range raw.CurrencyLedger {
		if currency == base || (row.ExchangeRateObserved && row.ExchangeRate != 1) {
			continue
		}
		key := fxPairKey(base, currency)
		c.mu.Lock()
		due := now.Sub(c.attempted[key]) >= reconFXQuoteRetry
		if due {
			if c.attempted == nil {
				c.attempted, c.latest = map[string]time.Time{}, map[string]reconFXQuote{}
			}
			c.attempted[key] = now
		}
		c.mu.Unlock()
		if due {
			if quote, err := fetch(ctx, currency, base); err == nil {
				c.mu.Lock()
				c.latest[key] = quote
				c.mu.Unlock()
			}
		}
		c.mu.Lock()
		if quote, ok := c.latest[key]; ok {
			out[currency] = quote
		}
		c.mu.Unlock()
	}
	return out
}

// fetchReconFXQuote reads one live quote on the current broker session, within
// the budget the display repair has on the same account read.
func (s *Server) fetchReconFXQuote(ctx context.Context, currency, base string) (reconFXQuote, error) {
	if s.reconFXQuoteForTest != nil {
		return s.reconFXQuoteForTest(ctx, currency, base)
	}
	s.mu.Lock()
	connector := s.connector
	s.mu.Unlock()
	if connector == nil || !connector.IsReady() {
		// An evidence read never asks for a reconnect, as the preview path would.
		return reconFXQuote{}, fmt.Errorf("broker session not ready for a %s/%s quote", base, currency)
	}
	authority, err := s.captureOrderPreviewBrokerAuthority()
	if err != nil {
		return reconFXQuote{}, err
	}
	if authority == nil {
		return reconFXQuote{}, fmt.Errorf("no broker session for a %s/%s quote", base, currency)
	}
	quoteCtx, cancel := context.WithTimeout(ctx, fxRepairQuoteBudget)
	defer cancel()
	fx, err := s.liveExactSessionFX(quoteCtx, authority, currency, base)
	if err != nil {
		return reconFXQuote{}, err
	}
	return reconFXQuote{Rate: fx.BasePerContract, At: fx.EvidenceAt, Source: fx.Source}, nil
}
