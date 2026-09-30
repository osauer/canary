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

// BondContractDetails is one decoded bondContractData frame (message 18).
// Identity fields come from the fixed frame prefix; the size rules, the
// security-identifier list and the long name are kept only when the whole
// versioned frame decoded (Complete), so a gateway that adds or moves a
// trailing field can never shift a size rule into place.
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

// parseBondContractDetails decodes one bondContractData frame. The identity
// prefix (through the minimum tick) must decode with the declared wire types
// and a positive contract id, a three-letter currency and a YYYYMMDD
// maturity; otherwise the frame is refused. The rest is kept only when the
// whole frame decodes.
func parseBondContractDetails(fields []string, expectedReqID, serverVersion int) (BondContractDetails, bool) {
	cursor := contractDetailsWireCursor{fields: fields, ok: true}
	messageID, ok := cursor.integer()
	if !ok || messageID != msgBondContractData {
		return BondContractDetails{}, false
	}
	version := 6
	if serverVersion < minServerVerSizeRules {
		if version, ok = cursor.integer(); !ok || version < 1 {
			return BondContractDetails{}, false
		}
	}
	d := BondContractDetails{ReqID: -1}
	if version >= 3 {
		if d.ReqID, ok = cursor.integer(); !ok {
			return BondContractDetails{}, false
		}
	}
	if expectedReqID != 0 && d.ReqID != expectedReqID {
		return BondContractDetails{}, false
	}
	d.Symbol = strings.TrimSpace(cursor.string())
	d.SecType = strings.ToUpper(strings.TrimSpace(cursor.string()))
	d.CUSIPField = strings.TrimSpace(cursor.string())
	if d.Coupon, ok = cursor.float(); !ok {
		return BondContractDetails{}, false
	}
	d.Maturity = bondWireDate(cursor.string())
	d.IssueDate = bondWireDate(cursor.string())
	_ = cursor.string() // ratings
	d.BondType = strings.TrimSpace(cursor.string())
	d.CouponType = strings.TrimSpace(cursor.string())
	for range 3 { // convertible, callable, putable
		if !cursor.boolean() {
			return BondContractDetails{}, false
		}
	}
	d.DescAppend = strings.TrimSpace(cursor.string())
	d.Exchange = strings.ToUpper(strings.TrimSpace(cursor.string()))
	d.Currency = strings.ToUpper(strings.TrimSpace(cursor.string()))
	d.MarketName = strings.TrimSpace(cursor.string())
	d.TradingClass = strings.TrimSpace(cursor.string())
	if d.ConID, ok = cursor.integer(); !ok {
		return BondContractDetails{}, false
	}
	if d.MinTick, ok = cursor.float(); !ok {
		return BondContractDetails{}, false
	}
	if !cursor.ok || d.ConID <= 0 || !bondCurrencyCode(d.Currency) {
		return BondContractDetails{}, false
	}
	if _, ok := d.MaturityDate(); !ok {
		return BondContractDetails{}, false
	}

	tail := d
	if serverVersion >= minServerVerMdSizeMultiplier && serverVersion < minServerVerSizeRules {
		_, _ = cursor.integer() // mdSizeMultiplier, no longer used
	}
	_ = cursor.string() // orderTypes
	tail.ValidExchanges = strings.TrimSpace(cursor.string())
	_ = cursor.string()  // nextOptionDate
	_ = cursor.string()  // nextOptionType
	_ = cursor.boolean() // nextOptionPartial
	_ = cursor.string()  // notes
	if version >= 4 {
		tail.LongName = strings.TrimSpace(cursor.string())
	}
	if serverVersion >= minServerVerBondTradingHours {
		tail.TimeZoneID = strings.TrimSpace(cursor.string())
		tail.TradingHours = cursor.string()
		tail.LiquidHours = cursor.string()
	}
	if version >= 6 {
		_ = cursor.string() // evRule
		_ = cursor.number() // evMultiplier
	}
	if version >= 5 {
		count, countOK := cursor.integer()
		if !countOK || count < 0 || count > (len(fields)-cursor.idx)/2 {
			cursor.ok = false
		}
		for range max(count, 0) {
			tag := strings.ToUpper(strings.TrimSpace(cursor.string()))
			value := strings.TrimSpace(cursor.string())
			if tag != "" && value != "" && cursor.ok {
				if tail.SecIDs == nil {
					tail.SecIDs = map[string]string{}
				}
				tail.SecIDs[tag] = value
			}
		}
	}
	if serverVersion >= minServerVerAggGroup {
		_, _ = cursor.integer()
	}
	if serverVersion >= minServerVerMarketRules {
		_ = cursor.string() // marketRuleIds
	}
	if serverVersion >= minServerVerSizeRules {
		tail.MinSize, _ = cursor.float()
		tail.SizeIncrement, _ = cursor.float()
		_, _ = cursor.float() // suggestedSizeIncrement
	}
	if cursor.ok && cursor.complete() {
		tail.Complete = true
		return tail, true
	}
	return d, true
}

// minServerVerBondTradingHours is the server version from which a bond
// frame carries its time zone and trading hours.
const minServerVerBondTradingHours = 188

// bondWireDate keeps the date of a "YYYYMMDD[ HH:MM:SS[ TZ]]" or dashed
// wire value.
func bondWireDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexAny(raw, " -"); i >= 0 {
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

// float reads one numeric field; an empty field reads as zero.
func (c *contractDetailsWireCursor) float() (float64, bool) {
	value := strings.TrimSpace(c.string())
	if !c.ok {
		return 0, false
	}
	if value == "" {
		return 0, true
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
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

// wireForms are the reqContractDetails contracts for the request, in the
// order they are asked: every form of the first security type, then every
// form of the next. A contract id is asked once per type. An identifier is
// asked first the way IBKR documents a bond, as the contract's symbol (the
// type, SMART, the currency), and, only when that finds no line, by
// secIdType/secId. The live reads of 2026-09-30 found both forms answered
// without a line for outstanding US bills asked as BOND: IBKR lists
// Treasury bills as BILL.
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
		forms = append(forms, bondWireForm{label: secType + " by symbol", contract: bySymbol},
			bondWireForm{label: secType + " by secIdType " + idType, contract: bySecID})
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

// BondLookupAttempt is one request form a bond lookup asked and the
// gateway's answer: IBKR's code and text, or Code 0 with a note when the
// search ended without a line.
type BondLookupAttempt struct {
	Form    string
	Code    int
	Message string
}

// String is the form and the gateway's answer, as a gap line shows it.
func (a BondLookupAttempt) String() string {
	if a.Code != 0 {
		return fmt.Sprintf("%s: IBKR %d \"%s\"", a.Form, a.Code, a.Message)
	}
	return a.Form + ": " + a.Message
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

// bondLookupMiss turns a form's answer into an attempt when it is the
// gateway's own "no line" (a rejection or an empty search), which the next
// form may still answer; ok is false for any other failure (a timeout, a
// changed session), which ends the lookup.
func bondLookupMiss(form string, err error) (BondLookupAttempt, bool) {
	if rejection, ok := errors.AsType[*ContractDetailsRejection](err); ok {
		return BondLookupAttempt{Form: form, Code: rejection.Code, Message: rejection.Message}, true
	}
	if errors.Is(err, ErrBondContractNotFound) {
		note := "the search ended without a line"
		if msg := err.Error(); msg != ErrBondContractNotFound.Error() {
			note = strings.TrimPrefix(msg, ErrBondContractNotFound.Error()+": ")
		}
		return BondLookupAttempt{Form: form, Message: note}, true
	}
	return BondLookupAttempt{}, false
}

// BondContractDetails asks the connected gateway for the bond lines a
// request names and returns every decoded line. It is a read: one
// reqContractDetails per request form on the current socket, each answered
// by bondContractData frames and the end marker. A form the gateway answers
// without a line moves to the next; when none finds one the error is a
// *BondLookupError carrying each answer (ErrContractNoDefinition for code
// 200).
func (c *Connector) BondContractDetails(ctx context.Context, request BondContractRequest, timeout time.Duration) ([]BondContractDetails, error) {
	binding, ok := c.CaptureSession()
	if !ok {
		return nil, ErrIBKRUnavailable
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
	return c.bondContractDetails(ctx, binding, request, timeout)
}

func (c *Connector) bondContractDetails(ctx context.Context, binding ConnectorSessionBinding, request BondContractRequest, timeout time.Duration) ([]BondContractDetails, error) {
	forms, err := request.wireForms()
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	lookupErr := &BondLookupError{Request: request.description()}
	for _, form := range forms {
		lines, err := c.bondContractDetailsOnce(ctx, binding, form.contract, timeout)
		if err == nil {
			named := request.linesNaming(stampSecType(lines, form.contract.SecType))
			if len(named) > 0 {
				return named, nil
			}
			err = fmt.Errorf("%w: the %d lines IBKR answered name another %s", ErrBondContractNotFound, len(lines), strings.ToUpper(strings.TrimSpace(request.IDType)))
		}
		attempt, miss := bondLookupMiss(form.label, err)
		if !miss {
			return nil, err
		}
		lookupErr.Attempts = append(lookupErr.Attempts, attempt)
	}
	return nil, lookupErr
}

// stampSecType names the type a line was asked as when its frame carried
// none, so every caller reads which type resolved from the line itself.
func stampSecType(lines []BondContractDetails, asked string) []BondContractDetails {
	for i := range lines {
		if lines[i].SecType == "" {
			lines[i].SecType = asked
		}
	}
	return lines
}

// bondContractDetailsOnce is one reqContractDetails for one request form.
func (c *Connector) bondContractDetailsOnce(ctx context.Context, binding ConnectorSessionBinding, contract Contract, timeout time.Duration) ([]BondContractDetails, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn := binding.connection
	reqID, err := conn.reserveRequestID(nil)
	if err != nil {
		return nil, err
	}
	defer conn.discardRequestIDReservation(reqID)
	detailsCh := make(chan BondContractDetails, 64)
	doneCh := make(chan struct{}, 1)
	overflowCh := make(chan struct{}, 1)
	serverVersion := conn.serverVersion
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
	bondHandlerID := conn.RegisterHandlerAtEpoch(msgBondContractData, func(fields []string, receiptEpoch uint64) {
		if receiptEpoch != binding.epoch {
			return
		}
		if d, ok := parseBondContractDetails(fields, reqID, serverVersion); ok {
			deliver(d)
		}
	})
	// Some gateway builds answer a BILL or BOND request with ordinary contractData
	// frames; keep their identity so the caller can still see the line.
	dataHandlerID := conn.RegisterHandlerAtEpoch(msgContractData, func(fields []string, receiptEpoch uint64) {
		if receiptEpoch != binding.epoch {
			return
		}
		if lite, ok := parseContractDetailsLite(fields, reqID, serverVersion); ok && lite.ConID > 0 {
			deliver(BondContractDetails{ReqID: reqID, ConID: lite.ConID, Symbol: lite.Symbol, SecType: strings.ToUpper(lite.SecType),
				Maturity: bondWireDate(lite.Expiry), Exchange: strings.ToUpper(lite.Exchange), Currency: strings.ToUpper(lite.Currency),
				TradingClass: lite.TradingClass, MinTick: lite.MinTick})
		}
	})
	endHandlerID := conn.RegisterHandlerAtEpoch(msgContractDataEnd, func(fields []string, receiptEpoch uint64) {
		if receiptEpoch != binding.epoch || len(fields) < 3 {
			return
		}
		if id, _ := strconv.Atoi(strings.TrimSpace(fields[2])); id == reqID {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	})
	defer conn.UnregisterHandler(msgBondContractData, bondHandlerID)
	defer conn.UnregisterHandler(msgContractData, dataHandlerID)
	defer conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
	req, releaseReq := c.registerBondContractDetailsRequest(reqID)
	defer releaseReq()

	if err := conn.sendContractDetailsRequestForEpoch(fetchCtx, contract, reqID, binding.epoch); err != nil {
		return nil, err
	}
	var out []BondContractDetails
	for {
		select {
		case d := <-detailsCh:
			out = append(out, d)
		case err := <-req.fail:
			return nil, err
		case <-overflowCh:
			return nil, fmt.Errorf("bond contract details overflow")
		case <-doneCh:
			for {
				select {
				case d := <-detailsCh:
					out = append(out, d)
					continue
				case <-overflowCh:
					return nil, fmt.Errorf("bond contract details overflow")
				default:
				}
				break
			}
			if !c.SessionCurrent(binding) {
				return nil, fmt.Errorf("broker session changed during bond contract details")
			}
			if len(out) == 0 {
				return nil, ErrBondContractNotFound
			}
			return out, nil
		case <-fetchCtx.Done():
			return nil, fetchCtx.Err()
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
