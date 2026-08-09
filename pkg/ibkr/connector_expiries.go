package ibkr

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// optionExpiryFetch coalesces the per-exchange msg-75 frames that IBKR emits
// Two views are maintained side-by-side. The legacy `strikes` map dedupes
type optionExpiryFetch struct {
	mu          sync.Mutex
	expirations map[string]struct{}                        // YYYYMMDD set, deduped across exchanges and classes
	strikes     map[string]map[float64]struct{}            // YYYYMMDD -> set of strikes (legacy, any class)
	classed     map[string]map[string]map[float64]struct{} // YYYYMMDD -> tradingClass -> set of strikes
	done        chan struct{}
}

func newOptionExpiryFetch() *optionExpiryFetch {
	return &optionExpiryFetch{
		expirations: make(map[string]struct{}),
		strikes:     make(map[string]map[float64]struct{}),
		classed:     make(map[string]map[string]map[float64]struct{}),
		done:        make(chan struct{}),
	}
}

// ExpiryClassedStrikes contains the sorted, deduplicated strike grid for one
// trading class on an expiry date. TradingClass preserves the broker's class
type ExpiryClassedStrikes struct {
	TradingClass string    `json:"trading_class"`
	Strikes      []float64 `json:"strikes"`
}

// fetchOptionExpiriesData runs one reqSecDefOptParams round trip and returns
func (c *Connector) fetchOptionExpiriesData(symbol string, timeout time.Duration) ([]string, map[string][]float64, map[string][]ExpiryClassedStrikes, error) {
	if !c.IsReady() {
		return nil, nil, nil, ErrIBKRUnavailable
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, nil, nil, fmt.Errorf("FetchOptionExpiries: symbol required")
	}
	if _, inactive := c.inactiveReason(symbol); inactive {
		return nil, nil, nil, ErrSymbolInactive
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return nil, nil, nil, ErrIBKRUnavailable
	}

	// Resolve the underlying conID via the existing contract cache. The chain
	detail, err := c.ensureContractDetails(symbol, 5*time.Second)
	if err != nil || detail == nil || detail.ConID == 0 {
		// Fall back to the late-arrival grace window historical uses.
		grace := contractDetailsLateGrace
		if half := timeout / 2; half > 0 && half < grace {
			grace = half
		}
		late := c.awaitContractDetail(symbol, grace)
		if late == nil || late.ConID == 0 {
			if err == nil {
				err = fmt.Errorf("contract details unresolved for %s", symbol)
			}
			return nil, nil, nil, err
		}
		detail = late
	}
	secType, _, _, _ := classifySymbol(symbol)
	if secType == "" {
		secType = "STK"
	}

	fetch := newOptionExpiryFetch()
	var (
		registeredReqID    int
		dataHandlerID      uint64
		endHandlerID       uint64
		registeredHandlers bool
	)

	// Register handlers BEFORE the request goes on the wire, but key on the
	beforeSend := func(reqID int) {
		registeredReqID = reqID
		dataHandlerID = conn.RegisterHandler(msgSecurityDefinitionOptionalParameter, func(fields []string) {
			c.handleSecDefOptParam(reqID, fetch, fields)
		})
		endHandlerID = conn.RegisterHandler(msgSecurityDefinitionOptionalParameterEnd, func(fields []string) {
			c.handleSecDefOptParamEnd(reqID, fetch, fields)
		})
		registeredHandlers = true
	}

	_, err = conn.RequestSecDefOptParams(symbol, "", secType, detail.ConID, beforeSend)
	if err != nil {
		if registeredHandlers {
			conn.UnregisterHandler(msgSecurityDefinitionOptionalParameter, dataHandlerID)
			conn.UnregisterHandler(msgSecurityDefinitionOptionalParameterEnd, endHandlerID)
		}
		return nil, nil, nil, fmt.Errorf("reqSecDefOptParams: %w", err)
	}
	defer func() {
		conn.UnregisterHandler(msgSecurityDefinitionOptionalParameter, dataHandlerID)
		conn.UnregisterHandler(msgSecurityDefinitionOptionalParameterEnd, endHandlerID)
	}()
	_ = registeredReqID

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Wait for end marker or timeout. On timeout we still return whatever we
	// the listing UX, and the IBKR-spec end marker is best-effort during
	timedOut := false
	select {
	case <-fetch.done:
	case <-timer.C:
		timedOut = true
	}

	expiries, strikes, classed := fetch.snapshot()
	if len(expiries) == 0 && timedOut {
		return nil, nil, nil, fmt.Errorf("option expiries timeout for %s after %s", symbol, timeout)
	}
	return expiries, strikes, classed, nil
}

// FetchOptionExpiries returns a newly allocated, sorted, deduplicated list of
func (c *Connector) FetchOptionExpiries(symbol string, timeout time.Duration) ([]string, error) {
	expiries, _, _, err := c.fetchOptionExpiriesData(symbol, timeout)
	if err != nil {
		return nil, err
	}
	return expiries, nil
}

// FetchOptionExpiryStrikes returns newly allocated, sorted, deduplicated strike
func (c *Connector) FetchOptionExpiryStrikes(symbol string, timeout time.Duration) (map[string][]float64, error) {
	_, strikes, _, err := c.fetchOptionExpiriesData(symbol, timeout)
	if err != nil {
		return nil, err
	}
	return strikes, nil
}

// FetchOptionExpiryStrikesClassed returns newly allocated strike grids grouped
// first by YYYY-MM-DD expiry and then by broker trading class. Both class entries
func (c *Connector) FetchOptionExpiryStrikesClassed(symbol string, timeout time.Duration) (map[string][]ExpiryClassedStrikes, error) {
	_, _, classed, err := c.fetchOptionExpiriesData(symbol, timeout)
	if err != nil {
		return nil, err
	}
	return classed, nil
}

// handleSecDefOptParam decodes one msg-75 frame and merges its expirations
// and strikes into the shared fetch state. Per the IBKR Python ibapi
// reference (decoder.processSecurityDefinitionOptionParameterMsg), the wire
// Note: there is no version field — msg 78 was added after IBKR moved to the
// BOTH the legacy `strikes` map (deduped across classes, for back-compat) and
func (c *Connector) handleSecDefOptParam(expectedReqID int, fetch *optionExpiryFetch, fields []string) {
	if len(fields) < 7 {
		return
	}
	// fields[0] = msgID
	rid, err := strconv.Atoi(fields[1])
	if err != nil || rid != expectedReqID {
		return
	}
	// fields[2] = exchange (we keep it implicit — dedupe across all exchanges)
	tradingClass := strings.TrimSpace(fields[4])
	idx := 6
	expCount, err := strconv.Atoi(fields[idx])
	if err != nil || expCount < 0 {
		return
	}
	idx++
	if idx+expCount > len(fields) {
		return
	}
	expirations := fields[idx : idx+expCount]
	idx += expCount

	if idx >= len(fields) {
		return
	}
	strikeCount, err := strconv.Atoi(fields[idx])
	if err != nil || strikeCount < 0 {
		return
	}
	idx++
	if idx+strikeCount > len(fields) {
		return
	}
	strikeStrings := fields[idx : idx+strikeCount]

	parsedStrikes := make([]float64, 0, strikeCount)
	for _, s := range strikeStrings {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		parsedStrikes = append(parsedStrikes, v)
	}

	fetch.mu.Lock()
	defer fetch.mu.Unlock()
	for _, exp := range expirations {
		exp = strings.TrimSpace(exp)
		if exp == "" {
			continue
		}
		fetch.expirations[exp] = struct{}{}
		// Legacy: deduped across all classes — back-compat for SPY callers.
		set, ok := fetch.strikes[exp]
		if !ok {
			set = make(map[float64]struct{})
			fetch.strikes[exp] = set
		}
		for _, k := range parsedStrikes {
			set[k] = struct{}{}
		}
		// Classed: keyed by tradingClass so SPX vs SPXW stay separated.
		// Empty tradingClass (unexpected — IBKR always fills it in
		byClass, ok := fetch.classed[exp]
		if !ok {
			byClass = make(map[string]map[float64]struct{})
			fetch.classed[exp] = byClass
		}
		classSet, ok := byClass[tradingClass]
		if !ok {
			classSet = make(map[float64]struct{})
			byClass[tradingClass] = classSet
		}
		for _, k := range parsedStrikes {
			classSet[k] = struct{}{}
		}
	}
}

// handleSecDefOptParamEnd closes the fetch's done channel exactly once.
// IBKR sends one msg-76 per request; we tolerate a duplicate as a no-op.
func (c *Connector) handleSecDefOptParamEnd(expectedReqID int, fetch *optionExpiryFetch, fields []string) {
	if len(fields) < 2 {
		return
	}
	rid, err := strconv.Atoi(fields[1])
	if err != nil || rid != expectedReqID {
		return
	}
	fetch.mu.Lock()
	defer fetch.mu.Unlock()
	select {
	case <-fetch.done:
		return
	default:
		close(fetch.done)
	}
}

// snapshot returns the deduped, normalised expiry list (YYYY-MM-DD,
// ascending), the per-expiry sorted strike list (legacy, merged across
func (f *optionExpiryFetch) snapshot() ([]string, map[string][]float64, map[string][]ExpiryClassedStrikes) {
	f.mu.Lock()
	defer f.mu.Unlock()

	expiries := make([]string, 0, len(f.expirations))
	for raw := range f.expirations {
		if normalised, ok := normaliseExpiry8(raw); ok {
			expiries = append(expiries, normalised)
		}
	}
	slices.Sort(expiries)

	strikes := make(map[string][]float64, len(f.strikes))
	for raw, set := range f.strikes {
		normalised, ok := normaliseExpiry8(raw)
		if !ok {
			continue
		}
		out := make([]float64, 0, len(set))
		for k := range set {
			out = append(out, k)
		}
		slices.Sort(out)
		// Multiple raw expiries from different exchanges can normalise to the
		if existing, ok := strikes[normalised]; ok {
			merged := append(existing, out...)
			slices.Sort(merged)
			strikes[normalised] = dedupeFloats(merged)
		} else {
			strikes[normalised] = out
		}
	}

	classed := make(map[string][]ExpiryClassedStrikes, len(f.classed))
	for raw, byClass := range f.classed {
		normalised, ok := normaliseExpiry8(raw)
		if !ok {
			continue
		}
		// Stable class ordering so the gamma compute's two-pass prewarm
		classNames := make([]string, 0, len(byClass))
		for cls := range byClass {
			classNames = append(classNames, cls)
		}
		slices.Sort(classNames)
		for _, cls := range classNames {
			set := byClass[cls]
			out := make([]float64, 0, len(set))
			for k := range set {
				out = append(out, k)
			}
			slices.Sort(out)
			classed[normalised] = append(classed[normalised], ExpiryClassedStrikes{
				TradingClass: cls,
				Strikes:      out,
			})
		}
	}
	return expiries, strikes, classed
}

// normaliseExpiry8 converts IBKR's YYYYMMDD wire form into the YYYY-MM-DD
func normaliseExpiry8(raw string) (string, bool) {
	if len(raw) != 8 {
		return "", false
	}
	for i := range 8 {
		if raw[i] < '0' || raw[i] > '9' {
			return "", false
		}
	}
	return raw[:4] + "-" + raw[4:6] + "-" + raw[6:], true
}

func dedupeFloats(in []float64) []float64 {
	if len(in) <= 1 {
		return in
	}
	out := in[:1]
	for i := 1; i < len(in); i++ {
		if in[i] != out[len(out)-1] {
			out = append(out, in[i])
		}
	}
	return out
}
