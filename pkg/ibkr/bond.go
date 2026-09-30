package ibkr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Bond support: contract details for secType BILL or BOND by ISIN, CUSIP or
// contract id, and the yield ticks a bond quote carries. Canary's cash sweep
// (internal-docs/design/cash-sweep.md, Phase B) resolves and quotes
// government bills with these; bond_order.go builds the one order shape it
// sends.

// Bond identifier types a contract-details request can name.
const (
	BondIdentifierISIN  = "ISIN"
	BondIdentifierCUSIP = "CUSIP"
)

// IBKR security types of a government bill or bond line. The TWS API lists
// Treasury bills as BILL and notes and bonds as BOND; a US bill asked as
// BOND finds no line (live read 2026-09-30).
const (
	SecTypeBill = "BILL"
	SecTypeBond = "BOND"
)

// IsBillOrBond reports whether secType is BILL or BOND, whatever its case
// and surrounding space: the one test every bill path uses.
func IsBillOrBond(secType string) bool {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case SecTypeBill, SecTypeBond:
		return true
	}
	return false
}

// BillOrBondSecType is BILL for a BILL security type and BOND for anything
// else: the wire type a bill-or-bond contract carries.
func BillOrBondSecType(secType string) string {
	if strings.EqualFold(strings.TrimSpace(secType), SecTypeBill) {
		return SecTypeBill
	}
	return SecTypeBond
}

// BondContractDetails is one bill or bond line: a decoded bondContractData
// frame (message 18), or an ordinary contractData frame (message 10) that
// answered a bond request, whose bond-only fields (coupon, issue date, the
// cusip field, bond and coupon types) stay empty. Identity fields come from
// the fixed frame prefix; the size rules, the security-identifier list, the
// long name and the hours are kept only when the whole frame decoded in the
// negotiated layout (Complete), so a gateway that adds or moves a trailing
// field can never shift a size rule into place. IBKR may leave the
// maturity, the currency or the identifiers empty (bond data licensing);
// such a line is kept, and the paths that need them refuse it.
type BondContractDetails struct {
	ReqID   int
	ConID   int
	Symbol  string
	SecType string
	// CUSIPField is the frame's cusip field as sent. IBKR may carry another
	// identifier there for bonds outside the US; ISIN and CUSIP check shape.
	CUSIPField string
	Coupon     float64
	// Maturity is YYYYMMDD; IssueDate is as sent (YYYYMMDD when present).
	Maturity   string
	IssueDate  string
	BondType   string
	CouponType string
	DescAppend string
	Exchange   string
	Currency   string
	MarketName string
	// TradingClass and MinTick are the venue's; MinTick is in the quote's
	// price convention.
	TradingClass string
	MinTick      float64
	// The remaining fields are set only on a Complete frame.
	ValidExchanges string
	LongName       string
	TimeZoneID     string
	TradingHours   string
	LiquidHours    string
	SecIDs         map[string]string
	MinSize        float64
	SizeIncrement  float64
	Complete       bool
}

// ISIN is the contract's ISIN when the broker named one: the secIdList
// entry, else a cusip field that has an ISIN's shape and check digit.
func (d BondContractDetails) ISIN() string {
	if v := strings.ToUpper(strings.TrimSpace(d.SecIDs[BondIdentifierISIN])); ValidISIN(v) {
		return v
	}
	if v := strings.ToUpper(strings.TrimSpace(d.CUSIPField)); ValidISIN(v) {
		return v
	}
	return ""
}

// CUSIP is the contract's CUSIP when the broker named one: the secIdList
// entry, else a nine-character cusip field, else the CUSIP inside a US or
// CA ISIN.
func (d BondContractDetails) CUSIP() string {
	if v := strings.ToUpper(strings.TrimSpace(d.SecIDs[BondIdentifierCUSIP])); ValidCUSIP(v) {
		return v
	}
	if v := strings.ToUpper(strings.TrimSpace(d.CUSIPField)); ValidCUSIP(v) {
		return v
	}
	if isin := d.ISIN(); strings.HasPrefix(isin, "US") || strings.HasPrefix(isin, "CA") {
		return isin[2:11]
	}
	return ""
}

// MaturityDate parses Maturity; ok is false when the frame carried none.
func (d BondContractDetails) MaturityDate() (time.Time, bool) {
	return parseBondDate(d.Maturity)
}

// IssueDateValue parses IssueDate; ok is false when the frame carried none.
func (d BondContractDetails) IssueDateValue() (time.Time, bool) {
	return parseBondDate(d.IssueDate)
}

func parseBondDate(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) != 8 {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102", raw)
	return t, err == nil
}

// ValidISIN reports whether s is an ISIN: two letters, nine alphanumerics
// and a check digit that satisfies the Luhn test over the letters' values.
func ValidISIN(s string) bool {
	if len(s) != 12 {
		return false
	}
	var digits strings.Builder
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if i == 11 {
				return false
			}
			digits.WriteString(strconv.Itoa(int(r-'A') + 10))
		case r >= '0' && r <= '9':
			if i < 2 {
				return false
			}
			digits.WriteRune(r)
		default:
			return false
		}
	}
	return luhnValid(digits.String())
}

// ValidCUSIP reports whether s is a CUSIP: eight alphanumerics and the
// standard modulus-10 check digit.
func ValidCUSIP(s string) bool {
	if len(s) != 9 {
		return false
	}
	sum := 0
	for i := range 8 {
		var v int
		switch r := s[i]; {
		case r >= '0' && r <= '9':
			v = int(r - '0')
		case r >= 'A' && r <= 'Z':
			v = int(r-'A') + 10
		case r == '*':
			v = 36
		case r == '@':
			v = 37
		case r == '#':
			v = 38
		default:
			return false
		}
		if i%2 == 1 {
			v *= 2
		}
		sum += v/10 + v%10
	}
	check := s[8]
	return check >= '0' && check <= '9' && int(check-'0') == (10-sum%10)%10
}

func luhnValid(digits string) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		v := int(digits[i] - '0')
		if double {
			v *= 2
			if v > 9 {
				v -= 9
			}
		}
		sum += v
		double = !double
	}
	return len(digits) > 0 && sum%10 == 0
}

// minServerVerBondTradingHours is the server version from which a bond
// frame carries its time zone and trading hours.
const minServerVerBondTradingHours = 188

// bondWireDate keeps the date of a "YYYYMMDD[ HH:MM:SS[ TZ]]",
// "YYYYMMDD-HH:MM:SS" or "YYYY-MM-DD" wire value as YYYYMMDD; any other
// value keeps its first word, which reads as no date.
func bondWireDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, ' '); i >= 0 {
		raw = raw[:i]
	}
	if t, err := time.Parse(time.DateOnly, raw); err == nil {
		return t.Format("20060102")
	}
	if i := strings.IndexByte(raw, '-'); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

func bondCurrencyCode(ccy string) bool {
	if len(ccy) != 3 {
		return false
	}
	for _, r := range ccy {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// BondContractRequest names one bond: an identifier (ISIN or CUSIP) or a
// contract id, and the currency it trades in. Exchange defaults to SMART.
// SecTypes are the security types asked, in order (BILL, BOND); none asks
// BOND.
type BondContractRequest struct {
	IDType   string
	ID       string
	ConID    int
	Currency string
	Exchange string
	SecTypes []string
}

// AskedSecTypes are the request's security types, upper-cased, each once,
// in order; none reads as BOND. Anything but BILL or BOND is refused.
func (r BondContractRequest) AskedSecTypes() ([]string, error) {
	var out []string
	for _, raw := range r.SecTypes {
		secType := strings.ToUpper(strings.TrimSpace(raw))
		if !IsBillOrBond(secType) {
			return nil, fmt.Errorf("bond contract request security type %q is neither BILL nor BOND", raw)
		}
		if !slices.Contains(out, secType) {
			out = append(out, secType)
		}
	}
	if len(out) == 0 {
		out = []string{SecTypeBond}
	}
	return out, nil
}

// bondWireForm is one way a reqContractDetails request names the bond.
type bondWireForm struct {
	label    string
	contract Contract
}

// attempt is the form's lookup attempt before the gateway answers: the
// request as sent.
func (f bondWireForm) attempt() BondLookupAttempt {
	c := f.contract
	return BondLookupAttempt{Form: f.label, SecType: c.SecType, Symbol: c.Symbol, SecIDType: c.SecIDType, SecID: c.SecID,
		ConID: c.ConID, Exchange: c.Exchange, Currency: c.Currency}
}

// wireForms are the reqContractDetails contracts for the request, in the
// order they are asked: every form of the first security type, then every
// form of the next. A contract id is asked once per type. An identifier is
// asked first the way IBKR documents a bond, as the contract's symbol (the
// type, SMART, the currency, nothing else), then by secIdType/secId, then
// by symbol with no exchange, each only when the one before found no line.
// The live reads of 2026-09-30 found the symbol form ending without a line
// and the secIdType form answered with code 200 for outstanding US bills
// asked as BOND and as BILL (F3, F4).
func (r BondContractRequest) wireForms() ([]bondWireForm, error) {
	secTypes, err := r.AskedSecTypes()
	if err != nil {
		return nil, err
	}
	base := Contract{Exchange: strings.ToUpper(strings.TrimSpace(r.Exchange)), Currency: strings.ToUpper(strings.TrimSpace(r.Currency))}
	if !bondCurrencyCode(base.Currency) {
		return nil, fmt.Errorf("bond contract request needs a three-letter currency")
	}
	var forms []bondWireForm
	if r.ConID > 0 {
		for _, secType := range secTypes {
			byConID := base
			byConID.SecType, byConID.ConID = secType, r.ConID
			forms = append(forms, bondWireForm{label: secType + " by contract id", contract: byConID})
		}
		return forms, nil
	}
	if base.Exchange == "" {
		base.Exchange = "SMART"
	}
	idType, id := strings.ToUpper(strings.TrimSpace(r.IDType)), strings.ToUpper(strings.TrimSpace(r.ID))
	if (idType != BondIdentifierISIN || !ValidISIN(id)) && (idType != BondIdentifierCUSIP || !ValidCUSIP(id)) {
		return nil, fmt.Errorf("bond contract request needs a valid ISIN, CUSIP or contract id")
	}
	for _, secType := range secTypes {
		bySymbol, bySecID := base, base
		bySymbol.SecType, bySecID.SecType = secType, secType
		bySymbol.Symbol = id
		bySecID.SecIDType, bySecID.SecID = idType, id
		noExchange := bySymbol
		noExchange.Exchange = ""
		forms = append(forms, bondWireForm{label: secType + " by symbol", contract: bySymbol},
			bondWireForm{label: secType + " by secIdType " + idType, contract: bySecID},
			bondWireForm{label: secType + " by symbol, no exchange", contract: noExchange})
	}
	return forms, nil
}

// description names the request in a lookup error: the types asked, in
// order, and the identifier.
func (r BondContractRequest) description() string {
	ccy := strings.ToUpper(strings.TrimSpace(r.Currency))
	asked := "BOND"
	if secTypes, err := r.AskedSecTypes(); err == nil {
		asked = strings.Join(secTypes, " then ")
	}
	if r.ConID > 0 {
		return fmt.Sprintf("%s contract id %d in %s", asked, r.ConID, ccy)
	}
	exchange := strings.ToUpper(strings.TrimSpace(r.Exchange))
	if exchange == "" {
		exchange = "SMART"
	}
	return fmt.Sprintf("%s %s %s on %s in %s", asked, strings.ToUpper(strings.TrimSpace(r.IDType)), strings.ToUpper(strings.TrimSpace(r.ID)), exchange, ccy)
}

// linesNaming drops the lines that name a different identifier of the
// request's type: asked by symbol, the gateway matches free text, so a line
// must not contradict the identifier it was asked for. A line that names
// none (an ordinary contractData frame) is kept.
func (r BondContractRequest) linesNaming(lines []BondContractDetails) []BondContractDetails {
	id := strings.ToUpper(strings.TrimSpace(r.ID))
	if r.ConID > 0 || id == "" {
		return lines
	}
	return slices.DeleteFunc(lines, func(d BondContractDetails) bool {
		var named string
		switch strings.ToUpper(strings.TrimSpace(r.IDType)) {
		case BondIdentifierISIN:
			named = d.ISIN()
		case BondIdentifierCUSIP:
			named = d.CUSIP()
		}
		return named != "" && named != id
	})
}

// ErrBondContractNotFound means the gateway finished the request without a
// bond line: the identifier names nothing IBKR lists in that currency.
var ErrBondContractNotFound = errors.New("no bond contract line for the request")

// ContractDetailsRejection is the gateway's rejection of one bond
// contract-details request: IBKR's code and its own text, on one line. It
// is ErrContractNoDefinition for IBKR's code-200 no-definition verdict.
type ContractDetailsRejection struct {
	Code    int
	Message string
}

// Error names IBKR's code and its text.
func (e *ContractDetailsRejection) Error() string {
	return fmt.Sprintf("IBKR %d \"%s\"", e.Code, e.Message)
}

// Is reports IBKR's definitive missing-contract verdict as
// ErrContractNoDefinition, as the other contract-details paths classify it.
func (e *ContractDetailsRejection) Is(target error) bool {
	return target == ErrContractNoDefinition && e.Code == 200 && strings.Contains(strings.ToUpper(e.Message), "NO SECURITY DEFINITION")
}

// brokerNoticeLine keeps a gateway notice's text readable on one line:
// control characters and runs of white space become one space, and the text
// is cut at 200 characters.
func brokerNoticeLine(message string) string {
	line := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)), " ")
	if runes := []rune(line); len(runes) > 200 {
		line = string(runes[:200]) + "…"
	}
	return line
}

// Bond lookup attempt outcomes.
const (
	// BondAttemptLine: the form found the line the lookup returns.
	BondAttemptLine = "line"
	// BondAttemptNoLine: the search ended without a line the request
	// keeps.
	BondAttemptNoLine = "no_line"
	// BondAttemptRejected: IBKR answered with an error code.
	BondAttemptRejected = "rejected"
	// BondAttemptFailed: no answer (a timeout, a changed session); the
	// lookup ends here.
	BondAttemptFailed = "failed"
)

// BondLookupAttempt is one request form a bond lookup asked, the request as
// sent (request id, security type, symbol or secIdType/secId or contract
// id, exchange, currency) and the gateway's answer: the outcome, IBKR's
// code and text, or Code 0 with a note when the search ended without a
// line. Lines counts the lines the form answered that the request kept;
// Frames are the frames that named the request id, in arrival order, and
// FramesOmitted counts those past the record's cap.
type BondLookupAttempt struct {
	Form          string
	ReqID         int
	SecType       string
	Symbol        string
	SecIDType     string
	SecID         string
	ConID         int
	Exchange      string
	Currency      string
	Outcome       string
	Code          int
	Message       string
	Lines         int
	Frames        []BondLookupFrame
	FramesOmitted int
}

// String is the form and the gateway's answer, as a gap line shows it.
func (a BondLookupAttempt) String() string {
	if a.Code != 0 {
		return fmt.Sprintf("%s: IBKR %d \"%s\"", a.Form, a.Code, a.Message)
	}
	return a.Form + ": " + a.Message
}

// BondLookupTrace is how a lookup asked: the request, the server version
// the frames were decoded for, and every attempt in order, the one that
// found the line included.
type BondLookupTrace struct {
	Request       string
	ServerVersion int
	Attempts      []BondLookupAttempt
}

// BondLookupError is a lookup no request form found a line with. It matches
// ErrContractNoDefinition or ErrBondContractNotFound through its attempts'
// answers, so the callers' classification holds.
type BondLookupError struct {
	Request  string
	Attempts []BondLookupAttempt
}

// Error names the request and every form's answer.
func (e *BondLookupError) Error() string {
	return "no bond line for " + e.Request + " (" + e.Answers() + ")"
}

// Answers is every attempt's form and the gateway's answer, in order.
func (e *BondLookupError) Answers() string {
	parts := make([]string, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, "; ")
}

// Unwrap is each attempt's answer as an error: a rejection, or
// ErrBondContractNotFound for a search that ended without a line.
func (e *BondLookupError) Unwrap() []error {
	out := make([]error, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		if a.Code != 0 {
			out = append(out, &ContractDetailsRejection{Code: a.Code, Message: a.Message})
		} else {
			out = append(out, ErrBondContractNotFound)
		}
	}
	return out
}

// bondLookupMiss records a form's answer on its attempt when it is the
// gateway's own "no line" (a rejection or an empty search), which the next
// form may still answer; ok is false for any other failure (a timeout, a
// changed session), which ends the lookup.
func bondLookupMiss(attempt BondLookupAttempt, err error) (BondLookupAttempt, bool) {
	if rejection, ok := errors.AsType[*ContractDetailsRejection](err); ok {
		attempt.Outcome, attempt.Code, attempt.Message = BondAttemptRejected, rejection.Code, rejection.Message
		return attempt, true
	}
	if errors.Is(err, ErrBondContractNotFound) {
		note := "the search ended without a line"
		if msg := err.Error(); msg != ErrBondContractNotFound.Error() {
			note = strings.TrimPrefix(msg, ErrBondContractNotFound.Error()+": ")
		}
		attempt.Outcome, attempt.Message = BondAttemptNoLine, note
		return attempt, true
	}
	attempt.Outcome, attempt.Message = BondAttemptFailed, brokerNoticeLine(err.Error())
	return attempt, false
}

// BondContractDetails asks the connected gateway for the bond lines a
// request names and returns every decoded line. It is a read: one
// reqContractDetails per request form on the current socket, each answered
// by bondContractData or contractData frames and the end marker. A form the
// gateway answers without a line moves to the next; when none finds one the
// error is a *BondLookupError carrying each answer (ErrContractNoDefinition
// for code 200).
func (c *Connector) BondContractDetails(ctx context.Context, request BondContractRequest, timeout time.Duration) ([]BondContractDetails, error) {
	lines, _, err := c.BondContractDetailsTraced(ctx, request, timeout)
	return lines, err
}

// BondContractDetailsTraced is BondContractDetails with the lookup's trace:
// every form asked and the frames IBKR answered each with, reduced to
// identifiers. The trace is nil only when no form was asked.
func (c *Connector) BondContractDetailsTraced(ctx context.Context, request BondContractRequest, timeout time.Duration) ([]BondContractDetails, *BondLookupTrace, error) {
	binding, ok := c.CaptureSession()
	if !ok {
		return nil, nil, ErrIBKRUnavailable
	}
	return c.bondContractDetails(ctx, binding, request, timeout)
}

// BondContractDetailsForSession is BondContractDetails on the exact socket
// generation binding names: an order preview reads its line's size, price
// and session rules from the same session it prices and previews on.
func (c *Connector) BondContractDetailsForSession(ctx context.Context, binding ConnectorSessionBinding, request BondContractRequest, timeout time.Duration) ([]BondContractDetails, error) {
	if c == nil || !c.SessionCurrent(binding) {
		return nil, fmt.Errorf("broker session changed before bond contract details")
	}
	lines, _, err := c.bondContractDetails(ctx, binding, request, timeout)
	return lines, err
}

func (c *Connector) bondContractDetails(ctx context.Context, binding ConnectorSessionBinding, request BondContractRequest, timeout time.Duration) ([]BondContractDetails, *BondLookupTrace, error) {
	forms, err := request.wireForms()
	if err != nil {
		return nil, nil, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	trace := &BondLookupTrace{Request: request.description(), ServerVersion: binding.connection.serverVersion}
	lookupErr := &BondLookupError{Request: trace.Request}
	for _, form := range forms {
		lines, attempt, err := c.bondContractDetailsOnce(ctx, binding, form, timeout)
		if err == nil {
			named := request.linesNaming(stampAsked(lines, form.contract))
			if attempt.Lines = len(named); len(named) > 0 {
				attempt.Outcome = BondAttemptLine
				trace.Attempts = append(trace.Attempts, attempt)
				return named, trace, nil
			}
			err = fmt.Errorf("%w: the %d lines IBKR answered name another %s (%s)", ErrBondContractNotFound, len(lines),
				strings.ToUpper(strings.TrimSpace(request.IDType)), strings.Join(linesNamed(lines, request.IDType), ", "))
		}
		attempt, miss := bondLookupMiss(attempt, err)
		trace.Attempts = append(trace.Attempts, attempt)
		if !miss {
			return nil, trace, err
		}
		lookupErr.Attempts = append(lookupErr.Attempts, attempt)
	}
	return nil, trace, lookupErr
}

// linesNamed is the identifier of idType each line names, for a gap line.
func linesNamed(lines []BondContractDetails, idType string) []string {
	out := make([]string, 0, len(lines))
	for _, d := range lines {
		named := d.CUSIP()
		if strings.EqualFold(strings.TrimSpace(idType), BondIdentifierISIN) {
			named = d.ISIN()
		}
		out = append(out, wireValue(named))
	}
	return out
}

// stampAsked names the type and currency a line was asked in when its
// frame carried none (bond data licensing may leave them empty), so every
// caller reads which type resolved, and in what currency, from the line
// itself. A line that names another currency keeps it.
func stampAsked(lines []BondContractDetails, asked Contract) []BondContractDetails {
	for i := range lines {
		if lines[i].SecType == "" {
			lines[i].SecType = asked.SecType
		}
		if lines[i].Currency == "" {
			lines[i].Currency = asked.Currency
		}
	}
	return lines
}

// bondContractDetailsRequestMessage is reqContractDetails for a bill or
// bond exactly as IBKR's documented bond example and its official encoder
// (API 10.37, version 8) send it: the contract id, symbol, security type,
// exchange, currency and secIdType/secId as the form names them and every
// other field empty (includeExpired false, which the encoder sends as 0).
// Unlike the discovery request it adds no SMART default and no primary
// exchange, so a form can ask with no exchange.
func (c *Connection) bondContractDetailsRequestMessage(contract Contract, reqID int) []byte {
	c.registerReqAlias(reqID, contract)
	return c.encodeMsg(reqContractData, 8, reqID, contract.ConID, contract.Symbol, contract.SecType,
		"", "", "", "", // lastTradeDateOrContractMonth, strike, right, multiplier
		contract.Exchange, "", // exchange, primaryExchange
		contract.Currency, "", "", // currency, localSymbol, tradingClass
		0,                                      // includeExpired
		contract.SecIDType, contract.SecID, "") // secIdType, secId, issuerId
}

// bondContractDetailsOnce is one reqContractDetails for one request form.
// While it is in flight every frame that names its request id is recorded
// on the attempt and logged, one line per frame: a contract frame with its
// identifiers and why it is or is not a line, the end marker, an error
// with IBKR's code, and any other message that names the id.
func (c *Connector) bondContractDetailsOnce(ctx context.Context, binding ConnectorSessionBinding, form bondWireForm, timeout time.Duration) (lines []BondContractDetails, attempt BondLookupAttempt, err error) {
	attempt = form.attempt()
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn := binding.connection
	reqID, err := conn.reserveRequestID(nil)
	if err != nil {
		return nil, attempt, err
	}
	defer conn.discardRequestIDReservation(reqID)
	attempt.ReqID = reqID
	frames := &bondFrameLog{}
	defer func() { attempt.Frames, attempt.FramesOmitted = frames.snapshot() }()
	record := func(f BondLookupFrame) {
		frames.add(f)
		if f.contract() && !f.Line {
			c.logWarn("bond lookup reqID %d (%s): %s", reqID, form.label, f)
			return
		}
		c.logInfo("bond lookup reqID %d (%s): %s", reqID, form.label, f)
	}
	detailsCh := make(chan BondContractDetails, 64)
	doneCh := make(chan struct{}, 1)
	overflowCh := make(chan struct{}, 1)
	layout := bondFrameLayoutFor(conn.serverVersion)
	deliver := func(d BondContractDetails) {
		select {
		case detailsCh <- d:
		default:
			select {
			case overflowCh <- struct{}{}:
			default:
			}
		}
	}
	// A contract frame for this request is decoded in the layout it names
	// the request in; either message is a line when its identity decodes.
	decodeFrame := func(fields []string, decode func([]string, int, bondFrameLayout) (BondContractDetails, []string, error)) {
		at, ours := bondFrameLayoutNaming(fields, reqID, layout)
		if !ours {
			return
		}
		f := bondFrameSummary(fields, at)
		d, notes, err := decode(fields, reqID, at)
		if err != nil {
			f.Note = err.Error()
		} else {
			f.Line, f.Complete, f.Note = true, d.Complete, strings.Join(notes, "; ")
		}
		record(f)
		if err == nil {
			deliver(d)
		}
	}
	bondHandlerID := conn.RegisterHandlerAtEpoch(msgBondContractData, func(fields []string, receiptEpoch uint64) {
		if receiptEpoch == binding.epoch {
			decodeFrame(fields, decodeBondContractData)
		}
	})
	dataHandlerID := conn.RegisterHandlerAtEpoch(msgContractData, func(fields []string, receiptEpoch uint64) {
		if receiptEpoch == binding.epoch {
			decodeFrame(fields, decodeContractDataLine)
		}
	})
	endHandlerID := conn.RegisterHandlerAtEpoch(msgContractDataEnd, func(fields []string, receiptEpoch uint64) {
		if receiptEpoch != binding.epoch || len(fields) < 3 {
			return
		}
		if id, _ := strconv.Atoi(strings.TrimSpace(fields[2])); id == reqID {
			record(BondLookupFrame{MessageID: msgContractDataEnd, Kind: BondFrameContractDataEnd, Fields: len(fields)})
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	})
	releaseTap := conn.tapInboundFrames(func(fields []string, receiptEpoch uint64) {
		if receiptEpoch != binding.epoch {
			return
		}
		if f, ok := bondTapFrame(fields, reqID); ok {
			record(f)
		}
	})
	defer releaseTap()
	defer conn.UnregisterHandler(msgBondContractData, bondHandlerID)
	defer conn.UnregisterHandler(msgContractData, dataHandlerID)
	defer conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
	req, releaseReq := c.registerBondContractDetailsRequest(reqID)
	defer releaseReq()

	msg := conn.bondContractDetailsRequestMessage(form.contract, reqID)
	if err := conn.sendMessageWithTypeContextForEpoch(fetchCtx, msg, RequestTypeGeneral, binding.epoch, true); err != nil {
		return nil, attempt, err
	}
	var out []BondContractDetails
	for {
		select {
		case d := <-detailsCh:
			out = append(out, d)
		case err := <-req.fail:
			return nil, attempt, err
		case <-overflowCh:
			return nil, attempt, fmt.Errorf("bond contract details overflow")
		case <-doneCh:
			for {
				select {
				case d := <-detailsCh:
					out = append(out, d)
					continue
				case <-overflowCh:
					return nil, attempt, fmt.Errorf("bond contract details overflow")
				default:
				}
				break
			}
			if !c.SessionCurrent(binding) {
				return nil, attempt, fmt.Errorf("broker session changed during bond contract details")
			}
			if len(out) == 0 {
				return nil, attempt, fmt.Errorf("%w: %s", ErrBondContractNotFound, frames.emptyNote())
			}
			return out, attempt, nil
		case <-fetchCtx.Done():
			return nil, attempt, fetchCtx.Err()
		}
	}
}

// Bond yield tick types. IBKR sends a bond's yields as price ticks: 50–52
// on the live feed, 103–105 on the delayed one. The value is passed through
// as the gateway sends it (percent, an assumption to verify post-install).
const (
	tickBidYield        = 50
	tickAskYield        = 51
	tickLastYield       = 52
	tickDelayedBidYield = 103
	tickDelayedAskYield = 104
	tickDelayedLastYld  = 105
)

// isBondYieldTick reports whether a price tick carries a yield, not a price.
func isBondYieldTick(tickType int) bool {
	switch tickType {
	case tickBidYield, tickAskYield, tickLastYield, tickDelayedBidYield, tickDelayedAskYield, tickDelayedLastYld:
		return true
	}
	return false
}

// recordBondYield stores a yield tick. -1 is the gateway's "no value"
// sentinel; anything at or below it, or not finite, is not a yield.
func (s *Subscription) recordBondYield(tickType int, value float64) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= -1 {
		return
	}
	v := value
	switch tickType {
	case tickBidYield, tickDelayedBidYield:
		s.bidYield = &v
	case tickAskYield, tickDelayedAskYield:
		s.askYield = &v
	case tickLastYield, tickDelayedLastYld:
		s.lastYield = &v
	}
}

// isBondMarketDataContract reports whether a quote request is for a bill or
// bond, which is subscribed without generic ticks: the stock tick list is
// not defined for them.
func isBondMarketDataContract(contract Contract) bool {
	return IsBillOrBond(contract.SecType)
}

func cloneYield(v *float64) *float64 {
	if v == nil {
		return nil
	}
	return new(*v)
}
