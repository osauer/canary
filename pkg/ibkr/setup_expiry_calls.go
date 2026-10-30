package ibkr

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"
)

const setupExpiryCallLimit = 2048

// FetchSetupExpiryCalls resolves one standard call expiry on the captured
// session. SecDefOptParams supplies separate expiry and strike sets, not valid
// pairs. This bounded partial-contract request supplies the actual pairs and
// their routing metadata; no partial response or shared cache is authoritative.
func (c *Connector) FetchSetupExpiryCalls(ctx context.Context, binding ConnectorSessionBinding, underlying Contract, expiry string, timeout time.Duration) ([]ResolvedOrderContract, error) {
	return c.FetchSetupExpiryOptions(ctx, binding, underlying, expiry, "C", timeout)
}

// FetchSetupExpiryOptions is FetchSetupExpiryCalls for either right, C or P.
func (c *Connector) FetchSetupExpiryOptions(ctx context.Context, binding ConnectorSessionBinding, underlying Contract, expiry, right string, timeout time.Duration) ([]ResolvedOrderContract, error) {
	date, dateErr := time.Parse("20060102", expiry)
	if right != "C" && right != "P" || underlying.ConID <= 0 || underlying.Symbol == "" || underlying.SecType != "STK" || underlying.Currency != "USD" || underlying.Exchange != "SMART" || dateErr != nil || date.Format("20060102") != expiry || !c.SessionCurrent(binding) {
		return nil, fmt.Errorf("exact US stock session and expiry required")
	}
	// Match the existing full-expiry prewarm budget. A short snapshot budget
	// can discard a valid inventory before its required end receipt arrives.
	// Caller cancellation, response count and exact-session fences still bound it.
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The stock's primary listing venue does not identify the option venue.
	// Use the same SMART route as exact call resolution; validate the returned
	// option's UnderConID rather than sending the stock's primary exchange.
	request := Contract{Symbol: underlying.Symbol, SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: expiry, Right: right, Multiplier: 100, TradingClass: underlying.Symbol}
	conn := binding.connection
	serverVersion := conn.serverVersion
	id, err := conn.reserveRequestID(nil)
	if err != nil {
		return nil, err
	}
	defer conn.discardRequestIDReservation(id)
	var mu sync.Mutex
	var failure error
	var frames int
	var finished bool
	rows := make(map[float64]ResolvedOrderContract)
	details := make(map[float64]ContractDetailsLite)
	ids := make(map[int]float64)
	done := make(chan struct{}, 1)
	signal := func() {
		select {
		case done <- struct{}{}:
		default:
		}
	}
	dataID := conn.RegisterHandlerAtEpoch(msgContractData, func(fields []string, epoch uint64) {
		if epoch != binding.epoch || ctx.Err() != nil {
			return
		}
		detail, ok := parseContractDetailsLite(fields, id, serverVersion)
		if !ok {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if failure != nil || finished {
			return
		}
		frames++
		if frames > setupExpiryCallLimit {
			failure = fmt.Errorf("expiry call inventory exceeds %d responses", setupExpiryCallLimit)
			signal()
			return
		}
		one := request
		one.Strike = detail.Strike
		resolved, err := exactOrderContract(one, []ContractDetailsLite{*detail})
		if err != nil || detail.Strike <= 0 || math.IsNaN(detail.Strike) || math.IsInf(detail.Strike, 0) || ValidateSetupOptionResolution(resolved, underlying, expiry, detail.Strike, right) != nil {
			failure = fmt.Errorf("expiry call inventory contains a mismatched contract")
			signal()
			return
		}
		prior, duplicate := details[detail.Strike]
		priorStrike, knownID := ids[detail.ConID]
		if duplicate && prior != *detail || knownID && priorStrike != detail.Strike {
			failure = fmt.Errorf("expiry call inventory contains conflicting identities")
			signal()
			return
		}
		details[detail.Strike], rows[detail.Strike], ids[detail.ConID] = *detail, resolved, detail.Strike
	})
	endID := conn.RegisterHandlerAtEpoch(msgContractDataEnd, func(fields []string, epoch uint64) {
		if epoch != binding.epoch || len(fields) < 3 || fields[2] != strconv.Itoa(id) {
			return
		}
		mu.Lock()
		finished = true
		mu.Unlock()
		signal()
	})
	defer conn.UnregisterHandler(msgContractData, dataID)
	defer conn.UnregisterHandler(msgContractDataEnd, endID)
	req, release := c.registerContractDetailsRequest(id, "")
	defer release()
	if err := conn.sendContractDetailsRequestForEpoch(ctx, request, id, binding.epoch); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-req.fail:
		return nil, err
	case <-done:
	}
	mu.Lock()
	defer mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !finished || !c.SessionCurrent(binding) {
		return nil, fmt.Errorf("broker session changed during expiry call discovery")
	}
	out := make([]ResolvedOrderContract, 0, len(rows))
	for _, row := range rows {
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b ResolvedOrderContract) int {
		if a.Contract.Strike < b.Contract.Strike {
			return -1
		}
		if a.Contract.Strike > b.Contract.Strike {
			return 1
		}
		return 0
	})
	return out, nil
}
