package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
)

// instrumentRequestMethods take an instrument in place of con_id, on the
// top-level contract of their params. All are reads or plans. A method outside
// this list, the order paths among them, refuses one rather than ignore it: an
// identity quietly dropped there would leave the symbol to decide what is traded.
var instrumentRequestMethods = map[string]bool{
	rpc.MethodQuoteSnapshot: true,
	rpc.MethodMarketHistory: true,
	rpc.MethodAddPlan:       true,
}

// instrumentResolveTimeout bounds the broker's lookup of one identity.
const instrumentResolveTimeout = 5 * time.Second

// resolveRequestInstrument turns a request's instrument into the contract fields
// every handler already reads, so none of them handles an identity.
func (s *Server) resolveRequestInstrument(ctx context.Context, req *rpc.Request) error {
	if !bytes.Contains(req.Params, []byte(`"instrument"`)) {
		return nil
	}
	if !instrumentRequestMethods[req.Method] {
		switch req.Method {
		case rpc.MethodWatchlistReplace, rpc.MethodWatchlistAdd:
			// A list read and written back carries the rows' identities; the
			// watchlist's own normalisation drops them.
			return nil
		}
		var params any
		if json.Unmarshal(req.Params, &params) == nil && carriesInstrument(params) {
			return errBadRequest(req.Method + " takes con_id and does not take an instrument")
		}
		return nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(req.Params, &top) != nil || top["contract"] == nil {
		return nil // the handler reports its own malformed params
	}
	var contract rpc.ContractParams
	if json.Unmarshal(top["contract"], &contract) != nil || contract.Instrument == (rpc.InstrumentID{}) {
		return nil
	}
	reads := s.brokerReads()
	if reads == nil {
		return s.sessionUnavailableError()
	}
	binding, ok := reads.Capture()
	if !ok {
		return s.sessionUnavailableError()
	}
	resolved, err := resolveContractInstrument(ctx, reads, binding, reads.Kind(), contract)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(resolved)
	if err != nil {
		return err
	}
	top["contract"] = raw
	params, err := json.Marshal(top)
	if err != nil {
		return err
	}
	req.Params = params
	return nil
}

// carriesInstrument finds an identity anywhere in params. Bond and cash sweep
// rows use the same key for a string, which is no identity.
func carriesInstrument(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		if id, ok := v["instrument"].(map[string]any); ok && id["broker"] != nil && id["id"] != nil {
			return true
		}
		for _, inner := range v {
			if carriesInstrument(inner) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(v, carriesInstrument)
	}
	return false
}

// resolveContractInstrument asks the broker for the exact instrument and fills
// the contract from it. It refuses an identity of another broker and one that
// disagrees with the contract's own con_id or symbol, because either means the
// caller and the broker name different things.
func resolveContractInstrument(ctx context.Context, reads broker.Instruments, binding broker.SessionBinding, kind broker.Kind, contract rpc.ContractParams) (rpc.ContractParams, error) {
	id := contract.Instrument
	if broker.Kind(id.Broker).Normalized() != kind.Normalized() || strings.TrimSpace(id.ID) == "" {
		return contract, errBadRequest("instrument names broker " + strconv.Quote(id.Broker) + " with id " + strconv.Quote(id.ID) + "; this daemon serves " + string(kind.Normalized()))
	}
	secType := contract.SecType
	if secType == "" {
		secType = "STK"
	}
	in, err := reads.Resolve(ctx, binding, broker.Instrument{Broker: kind, ID: id.ID, Symbol: contract.Symbol, SecType: secType, Venue: contract.Exchange, Primary: contract.PrimaryExch, Currency: contract.Currency}, instrumentResolveTimeout)
	if err != nil {
		return contract, err
	}
	conID, _ := in.IDInt()
	if kind.Normalized() != broker.KindIBKR {
		conID = 0
	}
	if contract.ConID != 0 && contract.ConID != conID {
		return contract, errBadRequest("instrument " + id.ID + " disagrees with con_id " + strconv.Itoa(contract.ConID))
	}
	if contract.Symbol != "" && !rpc.SameListedSymbol(contract.Symbol, in.Symbol) {
		return contract, errBadRequest("instrument " + id.ID + " is " + in.Symbol + ", not " + contract.Symbol)
	}
	return rpc.ContractParams{
		ConID:        conID,
		Symbol:       in.Symbol,
		SecType:      in.SecType,
		Market:       contract.Market, // the venue is the route; the shared stock normaliser knows IBKR's market words only
		Exchange:     in.Venue,
		PrimaryExch:  in.Primary,
		Currency:     in.Currency,
		LocalSymbol:  in.LocalSymbol,
		TradingClass: in.TradingClass,
		Expiry:       in.Expiry,
		Strike:       in.Strike,
		Right:        in.Right,
		Multiplier:   in.Multiplier,
		MinTick:      contract.MinTick,
	}, nil
}

// instrumentIdentityTimeout bounds the one lookup of the US exchanges an answer
// has not seen; past it those rows go out without an identity.
const instrumentIdentityTimeout = 2 * time.Second

// instrumentRow is what a response row says about its instrument.
type instrumentRow struct {
	conID                                   int
	symbol, secType, exchange, primary, ccy string
	set                                     func(rpc.InstrumentID)
}

// fillInstrumentIDs fills every collected row from its broker's identity.
func (s *Server) fillInstrumentIDs(rows []instrumentRow) {
	if s.opend == nil {
		for _, row := range rows {
			if row.conID > 0 {
				row.set(rpc.NewInstrumentID(rpc.InstrumentBrokerIBKR, strconv.Itoa(row.conID), rpc.ListingVenue(row.primary, row.exchange), row.ccy))
			}
		}
		return
	}
	in := make([]broker.Instrument, len(rows))
	for i, row := range rows {
		secType := row.secType
		if secType == rpc.SecTypeStock {
			secType = "STK"
		}
		in[i] = broker.Instrument{Symbol: row.symbol, SecType: secType, Venue: row.exchange, Primary: row.primary, Currency: row.ccy}
	}
	ctx, cancel := context.WithTimeout(context.Background(), instrumentIdentityTimeout)
	defer cancel()
	for i, listed := range s.opend.Listed(ctx, in) {
		rows[i].set(rpc.NewInstrumentID(rpc.InstrumentBrokerOpenD, listed.ID, listed.Venue, listed.Currency))
	}
}

// withInstrumentIDs gives an answer's rows their identities as it leaves the
// daemon, on a copy: the daemon's own rows, the revisions and fingerprints
// computed from them and the cached results it hands out stay as they were.
func (s *Server) withInstrumentIDs(res any) any {
	var rows []instrumentRow
	addPosition := func(p *rpc.PositionView) {
		rows = append(rows, instrumentRow{conID: p.ConID, symbol: p.Symbol, secType: p.SecType, exchange: p.Exchange, primary: p.PrimaryExch, ccy: p.Currency, set: func(id rpc.InstrumentID) { p.Instrument = id }})
	}
	addOrder := func(o *rpc.OrderView) {
		rows = append(rows, instrumentRow{conID: o.ConID, symbol: o.Symbol, secType: o.SecType, exchange: o.Exchange, primary: o.PrimaryExch, ccy: o.Currency, set: func(id rpc.InstrumentID) { o.Instrument = id }})
	}
	addContract := func(c rpc.ContractParams, set func(rpc.InstrumentID)) {
		rows = append(rows, instrumentRow{conID: c.ConID, symbol: c.Symbol, secType: c.SecType, exchange: c.Exchange, primary: c.PrimaryExch, ccy: c.Currency, set: set})
	}
	var out any = res
	switch r := res.(type) {
	case *rpc.PositionsResult:
		if r == nil {
			return res
		}
		copied := *r
		copied.Stocks, copied.Options = slices.Clone(r.Stocks), slices.Clone(r.Options)
		copied.ByUnderlying = slices.Clone(r.ByUnderlying)
		for i := range copied.Stocks {
			addPosition(&copied.Stocks[i])
		}
		for i := range copied.Options {
			addPosition(&copied.Options[i])
		}
		for i := range copied.ByUnderlying {
			group := &copied.ByUnderlying[i]
			if group.Stock != nil {
				stock := *group.Stock
				group.Stock = &stock
				addPosition(group.Stock)
			}
			group.Options = slices.Clone(group.Options)
			for j := range group.Options {
				addPosition(&group.Options[j])
			}
		}
		out = &copied
	case *rpc.Quote:
		if r == nil {
			return res
		}
		copied := *r
		addContract(r.Contract, func(id rpc.InstrumentID) { copied.Instrument = id })
		out = &copied
	case *rpc.OrdersOpenResult:
		if r == nil {
			return res
		}
		copied := *r
		copied.Orders = slices.Clone(r.Orders)
		for i := range copied.Orders {
			addOrder(&copied.Orders[i])
		}
		out = &copied
	case *rpc.OrdersHistoryResult:
		if r == nil {
			return res
		}
		copied := *r
		copied.Orders = slices.Clone(r.Orders)
		for i := range copied.Orders {
			addOrder(&copied.Orders[i].Order)
		}
		out = &copied
	case *rpc.OrderStatusResult:
		if r == nil || !r.Found {
			return res
		}
		copied := *r
		addOrder(&copied.Order)
		out = &copied
	case *rpc.OpportunitySnapshot:
		if r == nil {
			return res
		}
		copied := *r
		copied.Opportunities = slices.Clone(r.Opportunities)
		for i := range copied.Opportunities {
			row := &copied.Opportunities[i]
			addContract(row.Contract, func(id rpc.InstrumentID) { row.Instrument = id })
		}
		out = &copied
	case *rpc.TradeProposalSnapshot:
		if r == nil {
			return res
		}
		copied := *r
		copied.Proposals = slices.Clone(r.Proposals)
		for i := range copied.Proposals {
			row := &copied.Proposals[i]
			addContract(row.Contract, func(id rpc.InstrumentID) { row.Instrument = id })
		}
		out = &copied
	case *rpc.Watchlist:
		if r == nil {
			return res
		}
		copied := *r
		copied.Symbols = slices.Clone(r.Symbols)
		for i := range copied.Symbols {
			row := &copied.Symbols[i]
			rows = append(rows, instrumentRow{conID: row.ConID, symbol: row.Symbol, secType: row.SecType, exchange: row.Exchange, ccy: row.Currency, set: func(id rpc.InstrumentID) { row.Instrument = id }})
		}
		out = &copied
	default:
		return res
	}
	if len(rows) > 0 {
		s.fillInstrumentIDs(rows)
	}
	return out
}
