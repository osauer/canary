package ibkr

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// reqMatchingSymbols (81) asks the broker for listings whose symbol or name
// matches a pattern; the answer is symbolSamples (79). The request carries
// the request id and the pattern, nothing else, from server version 108.
const (
	reqMatchingSymbols             = 81
	minServerVerReqMatchingSymbols = 108
	minServerVerBondIssuerID       = 176
	symbolSearchSpacing            = time.Second
)

// ErrSymbolSearchBusy refuses a search while another runs or within IBKR's
// pacing interval of the last one; the caller tries again in a moment.
var ErrSymbolSearchBusy = errors.New("a listing search is already running; try again in a moment")

// SymbolSample is one listing in the broker's answer to a symbol search.
type SymbolSample struct {
	ConID              int
	Symbol             string
	SecType            string
	PrimaryExch        string
	Currency           string
	DerivativeSecTypes []string
	Description        string
	IssuerID           string
}

// symbolSearchPacing serialises searches on one connector: one at a time,
// at most one a second.
type symbolSearchPacing struct {
	mu   sync.Mutex
	busy bool
	last time.Time
}

func (p *symbolSearchPacing) acquire(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy || (!p.last.IsZero() && now.Sub(p.last) < symbolSearchSpacing) {
		return false
	}
	p.busy, p.last = true, now
	return true
}

func (p *symbolSearchPacing) release(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.busy, p.last = false, now
}

// decodeSymbolSamples reads symbolSamples in the official field order:
// request id, count, then per listing conId, symbol, secType, primary
// exchange, currency, the count of derivative security types and those
// types, and from server version 176 description and issuer id.
func decodeSymbolSamples(fields []string, reqID, serverVersion int) ([]SymbolSample, error) {
	invalid := fmt.Errorf("invalid symbol search answer")
	if len(fields) < 3 || fields[0] != strconv.Itoa(msgSymbolSamples) || fields[1] != strconv.Itoa(reqID) {
		return nil, invalid
	}
	n, err := strconv.Atoi(fields[2])
	if err != nil || n < 0 || n > 1000 {
		return nil, invalid
	}
	i := 3
	next := func() (string, bool) {
		if i >= len(fields) {
			return "", false
		}
		i++
		return fields[i-1], true
	}
	out := make([]SymbolSample, 0, n)
	for range n {
		var s SymbolSample
		var raw string
		var ok bool
		if raw, ok = next(); !ok {
			return nil, invalid
		}
		if s.ConID, err = strconv.Atoi(raw); err != nil {
			return nil, invalid
		}
		for _, dst := range []*string{&s.Symbol, &s.SecType, &s.PrimaryExch, &s.Currency} {
			if *dst, ok = next(); !ok {
				return nil, invalid
			}
		}
		if raw, ok = next(); !ok {
			return nil, invalid
		}
		count, err := strconv.Atoi(raw)
		if err != nil || count < 0 || count > 64 {
			return nil, invalid
		}
		for range count {
			if raw, ok = next(); !ok {
				return nil, invalid
			}
			s.DerivativeSecTypes = append(s.DerivativeSecTypes, raw)
		}
		if serverVersion >= minServerVerBondIssuerID {
			if s.Description, ok = next(); !ok {
				return nil, invalid
			}
			if s.IssuerID, ok = next(); !ok {
				return nil, invalid
			}
		}
		out = append(out, s)
	}
	// The socket decoder keeps the final NUL as one empty framing field.
	if i == len(fields)-1 && fields[i] == "" {
		i++
	}
	if i != len(fields) {
		return nil, invalid
	}
	return out, nil
}

// MatchingSymbols asks the broker which listings match pattern, on the exact
// session binding names, within ctx and at most ten seconds. Searches are
// serialised and paced; a search that cannot start now is refused with
// ErrSymbolSearchBusy rather than queued.
func (c *Connector) MatchingSymbols(ctx context.Context, binding ConnectorSessionBinding, pattern string) ([]SymbolSample, error) {
	if !c.SessionCurrent(binding) || binding.connection == nil {
		return nil, fmt.Errorf("exact broker session required")
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, fmt.Errorf("search pattern required")
	}
	if err := ensureASCII("search pattern", pattern); err != nil {
		return nil, err
	}
	conn := binding.connection
	serverVersion := conn.ServerVersion()
	if serverVersion < minServerVerReqMatchingSymbols {
		return nil, fmt.Errorf("the broker's API version does not support listing search")
	}
	if !c.symbolSearch.acquire(time.Now()) {
		return nil, ErrSymbolSearchBusy
	}
	defer func() { c.symbolSearch.release(time.Now()) }()
	reqID, err := conn.nextRequestID()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type result struct {
		rows []SymbolSample
		err  error
	}
	done := make(chan result, 1)
	handler := conn.RegisterHandlerAtEpoch(msgSymbolSamples, func(fields []string, epoch uint64) {
		if epoch != binding.epoch || len(fields) < 2 || fields[1] != strconv.Itoa(reqID) {
			return
		}
		rows, err := decodeSymbolSamples(fields, reqID, serverVersion)
		select {
		case done <- result{rows, err}:
		default:
		}
	})
	defer conn.UnregisterHandler(msgSymbolSamples, handler)
	if err := conn.sendMessageWithTypeContextForEpoch(ctx, conn.encodeMsg(reqMatchingSymbols, reqID, pattern), RequestTypeGeneral, binding.epoch, true); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-done:
		if !c.SessionCurrent(binding) {
			return nil, fmt.Errorf("broker session changed during listing search")
		}
		return out.rows, out.err
	}
}

// OrderHomeExchange is the exchange a stock order in currency goes to when
// SMART does not reach its listings (orderHomeExchanges), or "" when SMART
// does.
func OrderHomeExchange(currency string) string {
	return orderHomeExchanges[strings.ToUpper(strings.TrimSpace(currency))]
}
