package ibkr

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FetchSetupOptionStrikes reads actual standard-class listings for one exact
// underlying session. Unlike exploratory chains it never adopts a symbol-cache
// identity, adjusted multiplier, partial timeout, or invented strike grid.
func (c *Connector) FetchSetupOptionStrikes(ctx context.Context, binding ConnectorSessionBinding, underlying Contract, timeout time.Duration) (map[string][]ExpiryClassedStrikes, error) {
	if underlying.ConID <= 0 || underlying.SecType != "STK" || underlying.Currency != "USD" || underlying.Exchange != "SMART" || !c.SessionCurrent(binding) {
		return nil, fmt.Errorf("exact US stock broker session required")
	}
	if timeout <= 0 || timeout > 12*time.Second {
		timeout = 12 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn := binding.connection
	fetch := newOptionExpiryFetch()
	var stateMu sync.Mutex
	var failure error
	var frames, pairs int
	failed := make(chan struct{}, 1)
	var dataID, endID uint64
	_, err := conn.requestSecDefOptParamsContext(ctx, underlying.Symbol, "", "STK", underlying.ConID, func(id int) {
		dataID = conn.RegisterHandlerAtEpoch(msgSecurityDefinitionOptionalParameter, func(fields []string, epoch uint64) {
			if epoch != binding.epoch || ctx.Err() != nil || !setupOptionParameterMatches(fields, id, underlying) {
				return
			}
			stateMu.Lock()
			defer stateMu.Unlock()
			if failure != nil {
				return
			}
			n, err := setupOptionParameterSize(fields)
			if err != nil || frames >= 32 || pairs+n > 131072 {
				failure = fmt.Errorf("option listing malformed or exceeds bounded inventory")
				failed <- struct{}{}
				return
			}
			frames++
			pairs += n
			c.handleSecDefOptParam(id, fetch, fields)
		})
		endID = conn.RegisterHandlerAtEpoch(msgSecurityDefinitionOptionalParameterEnd, func(fields []string, epoch uint64) {
			if epoch == binding.epoch {
				c.handleSecDefOptParamEnd(id, fetch, fields)
			}
		})
	})
	defer conn.UnregisterHandler(msgSecurityDefinitionOptionalParameter, dataID)
	defer conn.UnregisterHandler(msgSecurityDefinitionOptionalParameterEnd, endID)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-failed:
	case <-fetch.done:
	}
	stateMu.Lock()
	listingErr := failure
	stateMu.Unlock()
	if listingErr != nil {
		return nil, listingErr
	}
	if !c.SessionCurrent(binding) {
		return nil, fmt.Errorf("broker session changed during option discovery")
	}
	_, _, classed := fetch.snapshot()
	return classed, nil
}

func setupOptionParameterMatches(fields []string, id int, underlying Contract) bool {
	if len(fields) < 6 || fields[1] != strconv.Itoa(id) || fields[2] != "SMART" || fields[3] != strconv.Itoa(underlying.ConID) || strings.TrimSpace(fields[4]) != underlying.Symbol || fields[5] != "100" {
		return false
	}
	return true
}

// Bound the collector's expiry×strike work before it allocates any map rows.
func setupOptionParameterSize(fields []string) (int, error) {
	invalid := fmt.Errorf("option parameter counts or values invalid")
	if len(fields) < 8 || len(fields) > 4096 {
		return 0, invalid
	}
	expiries, err := strconv.Atoi(fields[6])
	if err != nil || expiries < 0 || expiries > 128 || 7+expiries >= len(fields) {
		return 0, invalid
	}
	for _, date := range fields[7 : 7+expiries] {
		if d, err := time.Parse("20060102", date); err != nil || d.Format("20060102") != date {
			return 0, invalid
		}
	}
	strikes, err := strconv.Atoi(fields[7+expiries])
	end := 8 + expiries + strikes
	if err != nil || strikes < 0 || strikes > 2048 || expiries*strikes > 65536 || end > len(fields) || !(end == len(fields) || end+1 == len(fields) && fields[end] == "") {
		return 0, invalid
	}
	for _, value := range fields[8+expiries : end] {
		if n, err := strconv.ParseFloat(value, 64); err != nil || n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, invalid
		}
	}
	return expiries * strikes, nil
}

// ValidateSetupCallResolution checks the broker's exact underlying linkage and
// standard call tuple after ordinary exact-contract resolution has succeeded.
func ValidateSetupCallResolution(r ResolvedOrderContract, underlying Contract, expiry string, strike float64) error {
	c := r.Contract
	if r.UnderConID != underlying.ConID || r.UnderConID <= 0 || c.ConID <= 0 || c.Symbol != underlying.Symbol || c.SecType != "OPT" || c.Currency != "USD" || c.Exchange != "SMART" || c.Expiry != expiry || c.Right != "C" || c.Strike != strike || c.Multiplier != 100 || c.TradingClass != underlying.Symbol || strings.TrimSpace(c.LocalSymbol) == "" {
		return fmt.Errorf("resolved option is not the exact standard call for this underlying")
	}
	return nil
}
