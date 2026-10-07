package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Read-only bond support (internal-docs/design/cash-sweep.md, Phase B):
// contract details for BILL and BOND lines by ISIN, CUSIP or contract id, a
// short-lived quote per line under the daemon's market-data budget, the
// bill/bond classification of held BILL and BOND rows, and canary market
// --type BILL|BOND. Nothing here builds or sends an order.

// cashSweepBonds holds the daemon's bond directory and US bill universe.
// The zero value is ready; both are built on first use.
type cashSweepBonds struct {
	mu       sync.Mutex
	dir      *bondDirectory
	universe *billUniverse
	// source replaces the bill source the proposal engine reads; nil reads
	// the daemon's own (tests set it).
	source cashSweepBillSource
	// evidence holds the issuer sources a user bond buy is admitted by
	// (bond_evidence.go).
	evidence *bondEvidenceSources
}

const (
	// A bond line's identity does not change during a day.
	bondDetailsTTL = 24 * time.Hour
	// A lookup the gateway answered with "no such line" is retried after
	// this, so an identifier IBKR does not list is not asked every cycle; any
	// other failure (no gateway, a timeout, a session change) is retried
	// after bondDetailsTransientRetry.
	bondDetailsRetry          = 10 * time.Minute
	bondDetailsTransientRetry = 30 * time.Second
	bondDetailsWait           = 5 * time.Second
	// The market check waits this long for a lookup that may ask two
	// security types; a longer one answers on the next run.
	bondMarketLookupWait = 8 * time.Second
	// A quote is reused for a minute: the proposal cadence is 30 seconds and
	// a bill's price does not need a line per cycle.
	bondQuoteTTL      = time.Minute
	bondQuoteErrorTTL = 30 * time.Second
	bondQuoteTimeout  = 3 * time.Second
	// Positions wait this long for a cold classification; the lookup goes on
	// and the next read finds it.
	bondPositionWait = 1500 * time.Millisecond
)

// bondDirectory caches bond lines by request and recent quotes by contract
// id. Lookups for one request share one gateway call; fetch returns the
// lookup's trace (nil when nothing was asked), kept with its answer.
type bondDirectory struct {
	mu       sync.Mutex
	lines    map[string]bondLinesEntry
	quotes   map[int]bondQuoteEntry
	inflight map[string]chan struct{}
	fetch    func(context.Context, ibkrlib.BondContractRequest) ([]ibkrlib.BondContractDetails, *ibkrlib.BondLookupTrace, error)
	quote    func(context.Context, ibkrlib.BondContractDetails) (rpc.BondQuote, error)
	now      func() time.Time
}

type bondLinesEntry struct {
	lines []ibkrlib.BondContractDetails
	trace *ibkrlib.BondLookupTrace
	err   error
	at    time.Time
}

type bondQuoteEntry struct {
	quote rpc.BondQuote
	err   error
	at    time.Time
}

// errBondLookupPending means the lookup is still running; the next read
// finds its answer.
var errBondLookupPending = errors.New("bond contract lookup still running")

func (d *bondDirectory) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// bondRequestKey keys a lookup by what it asks: the types in order, the
// identifier or contract id, and the currency.
func bondRequestKey(r ibkrlib.BondContractRequest) string {
	secTypes, _ := r.AskedSecTypes()
	asked := strings.Join(secTypes, "+") + "|"
	if r.ConID > 0 {
		return asked + "CONID:" + strconv.Itoa(r.ConID) + "|" + normCcy(r.Currency)
	}
	return asked + strings.ToUpper(strings.TrimSpace(r.IDType)) + ":" + strings.ToUpper(strings.TrimSpace(r.ID)) + "|" + normCcy(r.Currency)
}

// lookup returns the lines a request names, from the cache when it is fresh.
// A cold lookup runs detached from the caller, who waits at most wait for it.
func (d *bondDirectory) lookup(ctx context.Context, r ibkrlib.BondContractRequest, wait time.Duration) ([]ibkrlib.BondContractDetails, error) {
	e, err := d.lookupEntry(ctx, r, wait)
	return e.lines, err
}

// lookupEntry is lookup with the answer's trace and time; the entry is
// zero while the lookup is still running.
func (d *bondDirectory) lookupEntry(ctx context.Context, r ibkrlib.BondContractRequest, wait time.Duration) (bondLinesEntry, error) {
	key := bondRequestKey(r)
	d.mu.Lock()
	if d.lines == nil {
		d.lines, d.inflight = map[string]bondLinesEntry{}, map[string]chan struct{}{}
	}
	if e, ok := d.lines[key]; ok {
		if d.clock().Sub(e.at) < bondLinesTTL(e.err) {
			d.mu.Unlock()
			e.lines = slices.Clone(e.lines)
			return e, e.err
		}
	}
	done, running := d.inflight[key]
	if !running {
		done = make(chan struct{})
		d.inflight[key] = done
		fetch := d.fetch
		// Each security type asked gets its own budget.
		budget := bondDetailsWait * time.Duration(max(len(r.SecTypes), 1))
		go func() {
			fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
			defer cancel()
			var lines []ibkrlib.BondContractDetails
			var trace *ibkrlib.BondLookupTrace
			err := errors.New("no bond contract source is attached")
			if fetch != nil {
				lines, trace, err = fetch(fetchCtx, r)
			}
			d.mu.Lock()
			d.lines[key] = bondLinesEntry{lines: lines, trace: trace, err: err, at: d.clock()}
			delete(d.inflight, key)
			d.mu.Unlock()
			close(done)
		}()
	}
	d.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		return bondLinesEntry{}, errBondLookupPending
	case <-ctx.Done():
		return bondLinesEntry{}, ctx.Err()
	}
	d.mu.Lock()
	e := d.lines[key]
	d.mu.Unlock()
	e.lines = slices.Clone(e.lines)
	return e, e.err
}

// bondLinesTTL is how long a lookup's answer is reused.
func bondLinesTTL(err error) time.Duration {
	switch {
	case err == nil:
		return bondDetailsTTL
	case errors.Is(err, ibkrlib.ErrContractNoDefinition), errors.Is(err, ibkrlib.ErrBondContractNotFound):
		return bondDetailsRetry
	}
	return bondDetailsTransientRetry
}

// quoteFor reads one quote for a line, reusing a recent one.
func (d *bondDirectory) quoteFor(ctx context.Context, line ibkrlib.BondContractDetails) (rpc.BondQuote, error) {
	d.mu.Lock()
	if d.quotes == nil {
		d.quotes = map[int]bondQuoteEntry{}
	}
	if e, ok := d.quotes[line.ConID]; ok {
		ttl := bondQuoteTTL
		if e.err != nil {
			ttl = bondQuoteErrorTTL
		}
		if d.clock().Sub(e.at) < ttl {
			d.mu.Unlock()
			return *rpc.CloneBondQuote(&e.quote), e.err
		}
	}
	quote := d.quote
	d.mu.Unlock()
	if quote == nil {
		return rpc.BondQuote{}, errors.New("no bond quote source is attached")
	}
	q, err := quote(ctx, line)
	d.mu.Lock()
	d.quotes[line.ConID] = bondQuoteEntry{quote: *rpc.CloneBondQuote(&q), err: err, at: d.clock()}
	d.mu.Unlock()
	return q, err
}

// bondSupport returns the server's bond support, building the directory on
// first use.
func (s *Server) bondSupport() *cashSweepBonds {
	if s == nil {
		return nil
	}
	b := &s.cashSweepB
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dir == nil {
		b.dir = &bondDirectory{
			fetch: func(ctx context.Context, r ibkrlib.BondContractRequest) ([]ibkrlib.BondContractDetails, *ibkrlib.BondLookupTrace, error) {
				c := s.gatewayConnector()
				if c == nil {
					return nil, nil, ibkrlib.ErrIBKRUnavailable
				}
				lines, trace, err := c.BondContractDetailsTraced(ctx, r, bondDetailsWait)
				s.bondEvidenceSupport().noteFactorPriced(lines)
				return lines, trace, err
			},
			quote: func(ctx context.Context, line ibkrlib.BondContractDetails) (rpc.BondQuote, error) {
				return s.quoteBondLine(ctx, line, bondQuoteTimeout)
			},
		}
	}
	return b
}

func (s *Server) bondDirectory() *bondDirectory {
	b := s.bondSupport()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dir
}

// quoteBondLine reads one quote for a bond line through the subscription
// manager: the line is held only while this read waits, like any snapshot
// quote, and released after. Prices are per 100 of face.
func (s *Server) quoteBondLine(ctx context.Context, line ibkrlib.BondContractDetails, timeout time.Duration) (rpc.BondQuote, error) {
	q := rpc.BondQuote{PriceConvention: rpc.BondPriceConventionPer100}
	c := s.gatewayConnector()
	if c == nil || s.subs == nil {
		return q, ibkrlib.ErrIBKRUnavailable
	}
	contract := ibkrlib.Contract{ConID: line.ConID, Symbol: nonEmptyString(line.Symbol, nonEmptyString(line.CUSIP(), line.ISIN())),
		SecType: ibkrlib.BillOrBondSecType(line.SecType), Exchange: "SMART", Currency: line.Currency}
	key, release, err := s.subs.HoldContract(ctx, contract)
	if err != nil {
		return q, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		release(cleanup)
	}()
	pollErr := pollMarketData(ctx, c, key, time.Now().Add(timeout), func(d *ibkrlib.MarketData) bool {
		return d.Bid > 0 || d.Ask > 0
	})
	if d := c.MarketDataSnapshot()[key]; d != nil {
		fillBondQuote(&q, d)
	}
	q.AsOf = time.Now().UTC()
	if pollErr != nil && !errors.Is(pollErr, context.DeadlineExceeded) && !q.HasPrice() {
		return q, pollErr
	}
	return q, nil
}

// fillBondQuote copies a market-data observation into a bond quote and
// judges freshness: only a live bid or ask is fresh.
func fillBondQuote(q *rpc.BondQuote, d *ibkrlib.MarketData) {
	q.Bid, q.Ask, q.Last, q.Close = ptrIfPos(d.Bid), ptrIfPos(d.Ask), ptrIfPos(d.Last), ptrIfPos(d.Close)
	q.BidYield, q.AskYield, q.LastYield = cloneFloat64Ptr(d.BidYield), cloneFloat64Ptr(d.AskYield), cloneFloat64Ptr(d.LastYield)
	q.DataType = bondFeedName(d.FeedType)
	twoSided := q.Bid != nil || q.Ask != nil
	switch {
	case twoSided && d.FeedType == 1:
		q.Fresh = true
	case twoSided:
		q.StaleReason = "the bid or ask is " + q.DataType + ", not live"
	case q.Last != nil:
		q.StaleReason = "no bid or ask; only a last trade"
	case q.Close != nil:
		q.StaleReason = "no bid or ask; only the previous close"
	default:
		q.StaleReason = "no price arrived"
	}
}

func bondFeedName(feed int) string {
	switch feed {
	case 1:
		return "live"
	case 2:
		return "frozen"
	case 3:
		return "delayed"
	case 4:
		return "delayed_frozen"
	}
	return "unknown"
}

// bondClassOf is the line's class: a bill is zero-coupon with at most 397
// days from issue to maturity, or, without an issue date, a US Treasury bill
// CUSIP. Every other line is a bond.
func bondClassOf(line ibkrlib.BondContractDetails) string {
	if line.Coupon != 0 {
		return rpc.BondClassBond
	}
	maturity, okMaturity := line.MaturityDate()
	issue, okIssue := line.IssueDateValue()
	if okMaturity && okIssue {
		if days := int(math.Round(maturity.Sub(issue).Hours() / 24)); days > 0 && days <= cashSweepMaturityCeilingDays {
			return rpc.BondClassBill
		}
		return rpc.BondClassBond
	}
	if usTreasuryBillCUSIP(line.CUSIP()) {
		return rpc.BondClassBill
	}
	return rpc.BondClassBond
}

// bondContractView is the wire view of a line; today dates days to maturity.
func bondContractView(line ibkrlib.BondContractDetails, today time.Time) rpc.BondContract {
	out := rpc.BondContract{ConID: line.ConID, Symbol: line.Symbol, SecType: line.SecType, ISIN: line.ISIN(), CUSIP: line.CUSIP(), Issuer: nonEmptyString(line.LongName, line.DescAppend), Ratings: strings.TrimSpace(line.Ratings),
		TradingClass: strings.TrimSpace(line.TradingClass), MarketName: strings.TrimSpace(line.MarketName),
		Class: bondClassOf(line), Currency: line.Currency, Exchange: line.Exchange, PriceConvention: rpc.BondPriceConventionPer100}
	if maturity, ok := line.MaturityDate(); ok {
		out.Maturity = maturity.Format(time.DateOnly)
		out.DaysToMaturity = new(cashSweepDaysLeft(today, maturity))
	}
	if issue, ok := line.IssueDateValue(); ok {
		out.IssueDate = issue.Format(time.DateOnly)
	}
	out.Coupon = new(line.Coupon)
	if line.Complete {
		out.MinSize, out.SizeIncrement = ptrIfPos(line.MinSize), ptrIfPos(line.SizeIncrement)
	}
	out.MinTick = ptrIfPos(line.MinTick)
	if instrument := bondLineInstrument(line); instrument != "" {
		out.QuantityUnit = cashSweepInstrumentConventions[instrument].QuantityUnit
	}
	return out
}

// bondLineInstrument names the vocabulary bill a line is, or "".
func bondLineInstrument(line ibkrlib.BondContractDetails) string {
	return cashSweepHeldBillInstrument(rpc.PositionBond{Class: bondClassOf(line), ISIN: line.ISIN(), CUSIP: line.CUSIP()}, line.Currency)
}

// bondLineFor picks the one BILL or BOND line of ccy among a lookup's lines
// (and, with conID, the line carrying it). Duplicates of one contract id
// count once; two contract ids are ambiguous.
func bondLineFor(lines []ibkrlib.BondContractDetails, ccy string, conID int) (ibkrlib.BondContractDetails, int, error) {
	seen := map[int]ibkrlib.BondContractDetails{}
	for _, line := range lines {
		if line.ConID <= 0 || (conID > 0 && line.ConID != conID) || normCcy(line.Currency) != normCcy(ccy) ||
			(line.SecType != "" && !ibkrlib.IsBillOrBond(line.SecType)) {
			continue
		}
		if prior, ok := seen[line.ConID]; !ok || (!prior.Complete && line.Complete) {
			seen[line.ConID] = line
		}
	}
	switch len(seen) {
	case 0:
		return ibkrlib.BondContractDetails{}, 0, fmt.Errorf("no BILL or BOND line in %s", normCcy(ccy))
	case 1:
		for _, line := range seen {
			return line, 1, nil
		}
	}
	return ibkrlib.BondContractDetails{}, len(seen), fmt.Errorf("%d BILL or BOND lines in %s; the identifier is ambiguous", len(seen), normCcy(ccy))
}

// classifyBondPositions classifies every held BILL or BOND row by its
// contract id, asked as the row's own type first. A row whose details
// cannot be read yet is unresolved and says why.
func (s *Server) classifyBondPositions(ctx context.Context, rows []rpc.PositionView, now time.Time) []rpc.PositionBond {
	var out []rpc.PositionBond
	for _, row := range rows {
		if !ibkrlib.IsBillOrBond(row.SecType) || row.Quantity == 0 {
			continue
		}
		b := rpc.PositionBond{ConID: row.ConID, Symbol: row.Symbol, Currency: normCcy(row.Currency), Class: rpc.BondClassUnresolved,
			Quantity: row.Quantity, Mark: row.Mark, MarketValue: row.MarketValue}
		switch {
		case row.ConID <= 0:
			b.Reason = "the position carries no contract id"
		case b.Currency == "":
			b.Reason = "the position carries no currency"
		default:
			lines, err := s.bondDirectory().lookup(ctx, ibkrlib.BondContractRequest{ConID: row.ConID, Currency: b.Currency,
				SecTypes: cashSweepHeldSecTypes(row.SecType, "")}, bondPositionWait)
			if err == nil {
				var line ibkrlib.BondContractDetails
				if line, _, err = bondLineFor(lines, b.Currency, row.ConID); err == nil {
					view := bondContractView(line, cashSweepDay(now))
					b.Class, b.ISIN, b.CUSIP, b.Issuer = view.Class, view.ISIN, view.CUSIP, view.Issuer
					b.Maturity, b.DaysToMaturity, b.Coupon = view.Maturity, view.DaysToMaturity, view.Coupon
					if view.Class == rpc.BondClassBill {
						b.Maturity, b.MaturitySource, b.MaturitySourceAsOf, err = cashSweepHeldMaturity(serverBillSource{s}, lines, line, now)
						if err == nil {
							date, _ := time.Parse(time.DateOnly, b.Maturity)
							b.DaysToMaturity = new(cashSweepDaysLeft(cashSweepDay(now), date))
						} else {
							b.Class, b.DaysToMaturity = rpc.BondClassUnresolved, nil
						}
					}
				}
			}
			if b.Currency == "EUR" && row.SecType == ibkrlib.SecTypeBill && b.ISIN == "" {
				if candidate, bindingErr := s.heldGermanBinding(ctx, row, now); bindingErr == nil {
					b.Class, b.ISIN, b.Maturity = rpc.BondClassBill, candidate.id, candidate.maturity.Format(time.DateOnly)
					b.ResolutionSource, b.MaturitySource, b.MaturitySourceAsOf = candidate.resolutionSource, candidate.maturitySource, candidate.publicFetchedAt
					b.DaysToMaturity = new(candidate.days)
					err = nil
				} else {
					b.Class, b.Maturity, b.DaysToMaturity = rpc.BondClassUnresolved, "", nil
					err = bindingErr
				}
			}
			if err != nil {
				b.Reason = "contract details unavailable: " + bondLookupReason(err)
			}
		}
		out = append(out, b)
	}
	return out
}

// bondLookupReason words a lookup failure. A contract search that found no
// line carries the request and each form's answer with IBKR's own code and
// text, so the next attempt is diagnosable; other failures carry no broker
// free text.
func bondLookupReason(err error) string {
	if lookupErr, ok := errors.AsType[*ibkrlib.BondLookupError](err); ok {
		verdict := "IBKR refused the request"
		if errors.Is(err, ibkrlib.ErrContractNoDefinition) || errors.Is(err, ibkrlib.ErrBondContractNotFound) {
			verdict = "IBKR lists no such bond line"
		}
		return fmt.Sprintf("%s (%s; %s)", verdict, lookupErr.Request, lookupErr.Answers())
	}
	switch {
	case errors.Is(err, errBondLookupPending):
		return "the lookup is still running"
	case errors.Is(err, ibkrlib.ErrIBKRUnavailable):
		return "the gateway is not connected"
	case errors.Is(err, ibkrlib.ErrContractNoDefinition), errors.Is(err, ibkrlib.ErrBondContractNotFound):
		return "IBKR lists no such bond line"
	case errors.Is(err, context.DeadlineExceeded):
		return "the gateway did not answer in time"
	}
	if reqErr, ok := errors.AsType[*ibkrlib.ContractDetailsRequestError](err); ok {
		return fmt.Sprintf("IBKR refused the request (code %d)", reqErr.Code)
	}
	return err.Error()
}

// bondLookupAttemptViews is the wire view of a lookup's attempts: the
// trace's, or, for a lookup that carries none, the attempts its error
// names (without frames).
func bondLookupAttemptViews(trace *ibkrlib.BondLookupTrace, err error) []rpc.BondLookupAttempt {
	var attempts []ibkrlib.BondLookupAttempt
	if trace != nil {
		attempts = trace.Attempts
	} else if lookupErr, ok := errors.AsType[*ibkrlib.BondLookupError](err); ok {
		attempts = lookupErr.Attempts
	}
	if len(attempts) == 0 {
		return nil
	}
	out := make([]rpc.BondLookupAttempt, 0, len(attempts))
	for _, a := range attempts {
		view := rpc.BondLookupAttempt{Form: a.Form, ReqID: a.ReqID, SecType: a.SecType, Symbol: a.Symbol, SecIDType: a.SecIDType, SecID: a.SecID,
			ConID: a.ConID, Exchange: a.Exchange, Currency: a.Currency, Outcome: a.Outcome, Code: a.Code, Message: a.Message, Lines: a.Lines,
			Frames: make([]rpc.BondLookupFrame, 0, len(a.Frames)), FramesOmitted: a.FramesOmitted}
		if view.Outcome == "" {
			view.Outcome = rpc.BondAttemptNoLine
			if a.Code != 0 {
				view.Outcome = rpc.BondAttemptRejected
			}
		}
		for _, f := range a.Frames {
			view.Frames = append(view.Frames, rpc.BondLookupFrame{MsgID: f.MessageID, Kind: f.Kind, Fields: f.Fields, Layout: f.Layout, ConID: f.ConID,
				SecType: f.SecType, Symbol: f.Symbol, CUSIP: f.CUSIP, LocalSymbol: f.LocalSymbol, Exchange: f.Exchange, Currency: f.Currency,
				Maturity: f.Maturity, Code: f.Code, Line: f.Line, Complete: f.Complete, Note: f.Note})
		}
		out = append(out, view)
	}
	return out
}

// bondIdentifierFor reads an identifier as an ISIN or CUSIP and resolves
// the currency: the caller's, else the identifier's own where it has one.
func bondIdentifierFor(identifier, currency string) (idType, id, ccy string, err error) {
	id = strings.ToUpper(strings.TrimSpace(identifier))
	switch {
	case ibkrlib.ValidISIN(id):
		idType = ibkrlib.BondIdentifierISIN
	case ibkrlib.ValidCUSIP(id):
		idType = ibkrlib.BondIdentifierCUSIP
	default:
		return "", "", "", fmt.Errorf("identifier %q is neither an ISIN (12 characters) nor a CUSIP (9) with a valid check digit", identifier)
	}
	if ccy = normCcy(currency); ccy != "" {
		if len(ccy) != 3 {
			return "", "", "", fmt.Errorf("currency %q is not a three-letter code", currency)
		}
		return idType, id, ccy, nil
	}
	if idType == ibkrlib.BondIdentifierCUSIP {
		return idType, id, "USD", nil
	}
	if ccy = bondISINCurrency[id[:2]]; ccy == "" {
		return "", "", "", fmt.Errorf("the currency of %s cannot be read from its issuer country; give --currency", id)
	}
	return idType, id, ccy, nil
}

// bondISINCurrency is the currency of an issuer country with one currency.
var bondISINCurrency = map[string]string{
	"US": "USD", "GB": "GBP", "CA": "CAD", "CH": "CHF", "JP": "JPY",
	"DE": "EUR", "FR": "EUR", "IT": "EUR", "ES": "EUR", "NL": "EUR", "BE": "EUR", "AT": "EUR", "FI": "EUR",
	"IE": "EUR", "PT": "EUR", "LU": "EUR", "GR": "EUR", "SK": "EUR", "SI": "EUR", "LT": "EUR", "LV": "EUR", "EE": "EUR",
}

// handleMarketBond resolves one bill or bond by identifier and reads one
// quote: a read-only check. A resolution or quote gap is data (resolved or
// quoted false with a reason), not an error.
func (s *Server) handleMarketBond(ctx context.Context, req *rpc.Request) (*rpc.MarketBondResult, error) {
	var p rpc.MarketBondParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	idType, id, ccy, err := bondIdentifierFor(p.Identifier, p.Currency)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	secTypes, note, err := marketBondSecTypes(p.SecType, idType, id)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	if s.gatewayConnector() == nil {
		return nil, s.gatewayUnavailableError()
	}
	timeout := bondQuoteTimeout
	if p.TimeoutMs > 0 {
		timeout = min(time.Duration(p.TimeoutMs)*time.Millisecond, 10*time.Second)
	}
	res := marketBondCheck(ctx, s.bondDirectory(), idType, id, ccy, secTypes, timeout, time.Now().UTC())
	res.SecTypesNote = note
	return res, nil
}

// marketBondSecTypes are the security types the market check asks, in
// order: the requested one (BOND when none), then the types of the
// vocabulary bill the identifier would be, with a note naming why.
func marketBondSecTypes(requested, idType, id string) ([]string, string, error) {
	first := strings.ToUpper(strings.TrimSpace(requested))
	if first == "" {
		first = ibkrlib.SecTypeBond
	}
	if !ibkrlib.IsBillOrBond(first) {
		return nil, "", fmt.Errorf("security type %q is neither BILL nor BOND", requested)
	}
	secTypes := []string{first}
	instrument := cashSweepIdentifierInstrument(idType, id)
	if instrument == "" {
		return secTypes, "", nil
	}
	var added []string
	for _, secType := range cashSweepInstrumentSecTypes(instrument) {
		if !slices.Contains(secTypes, secType) {
			secTypes = append(secTypes, secType)
			added = append(added, secType)
		}
	}
	if len(added) == 0 {
		return secTypes, "", nil
	}
	return secTypes, fmt.Sprintf("asked as %s, then as %s: %s %s reads as a %s bill, which Canary asks as %s", first, strings.Join(added, " and "),
		idType, id, instrument, strings.Join(cashSweepInstrumentSecTypes(instrument), " then ")), nil
}

// marketBondCheck is the handler's read: contract details, then one quote.
func marketBondCheck(ctx context.Context, dir *bondDirectory, idType, id, ccy string, secTypes []string, timeout time.Duration, now time.Time) *rpc.MarketBondResult {
	res := &rpc.MarketBondResult{Identifier: id, IdentifierType: idType, Currency: ccy, SecTypes: slices.Clone(secTypes), AsOf: now}
	entry, err := dir.lookupEntry(ctx, ibkrlib.BondContractRequest{IDType: idType, ID: id, Currency: ccy, SecTypes: secTypes}, bondMarketLookupWait)
	res.Attempts = bondLookupAttemptViews(entry.trace, err)
	res.LookupAsOf = entry.at
	if entry.trace != nil {
		res.ServerVersion = entry.trace.ServerVersion
	}
	if err != nil {
		res.Reason = "contract details: " + bondLookupReason(err)
		return res
	}
	line, n, err := bondLineFor(entry.lines, ccy, 0)
	res.Lines = n
	if err != nil {
		res.Reason = "contract details: " + err.Error()
		return res
	}
	res.Resolved = true
	view := bondContractView(line, cashSweepDay(now))
	res.Contract = &view
	res.Session = cashSweepBondSession(&line, bondLineInstrument(line), now)
	quoteCtx, cancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer cancel()
	q, err := dir.quoteFor(quoteCtx, line)
	if err != nil {
		res.Reason = "quote: " + bondLookupReason(err)
		return res
	}
	res.Quote, res.Quoted = &q, q.HasPrice()
	if !res.Quoted {
		res.Reason = "quote: " + q.StaleReason
	}
	return res
}
