package ibkr

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

// Bond frames: decoding the frames IBKR answers a bill or bond
// contract-details request with, and the per-request wire record that says
// which frames arrived and why each became a line or did not
// (internal-docs/design/cash-sweep.md, F4).
//
// IBKR answers a bond request with bondContractData (message 18) and notes
// that bond market-data licensing leaves only a few fields of a bond
// description populated (the minimum tick, the exchange, the short name).
// A frame is therefore a line when its identity decodes: the request id,
// the typed fields up to the minimum tick and a positive contract id. An
// empty maturity, currency or identifier is a gap the line carries and the
// wire record names, never a reason to drop it: the sweep refuses a line
// without a maturity, and an order needs the size rules of a whole frame.
// An ordinary contractData frame (message 10) that answers the request is a
// line too, with the bond-only fields empty.

// Frame kinds a bond lookup records.
const (
	BondFrameBondContractData = "bondContractData"
	BondFrameContractData     = "contractData"
	BondFrameContractDataEnd  = "contractDataEnd"
	BondFrameError            = "error"
	BondFrameOther            = "other"
)

// BondLookupFrame is one inbound frame that named a bond request's id,
// reduced to identifiers: the message, its raw field count, and for a
// contract frame the contract id, security type, identifiers, exchange,
// currency and maturity as sent (by their position in the layout, whether
// or not the frame decoded). An error frame carries IBKR's code. Line says
// the frame decoded into a line, Complete that the whole frame did; Note
// says why a contract frame is not a line, or what the line lacks.
type BondLookupFrame struct {
	MessageID   int
	Kind        string
	Fields      int
	Layout      string
	ConID       string
	SecType     string
	Symbol      string
	CUSIP       string
	LocalSymbol string
	Exchange    string
	Currency    string
	Maturity    string
	Code        int
	Line        bool
	Complete    bool
	Note        string
}

// String is the frame as a log line shows it.
func (f BondLookupFrame) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "msg %d %s, %d fields", f.MessageID, f.Kind, f.Fields)
	for _, kv := range []struct{ k, v string }{{"layout", f.Layout}, {"con_id", f.ConID}, {"sec_type", f.SecType}, {"symbol", f.Symbol},
		{"cusip", f.CUSIP}, {"local_symbol", f.LocalSymbol}, {"exchange", f.Exchange}, {"currency", f.Currency}, {"maturity", f.Maturity}} {
		if kv.v != "" {
			fmt.Fprintf(&b, ", %s %q", kv.k, kv.v)
		}
	}
	if f.Code != 0 {
		fmt.Fprintf(&b, ", code %d", f.Code)
	}
	if f.contract() {
		switch {
		case f.Line && f.Complete:
			b.WriteString(": a line, whole frame decoded")
		case f.Line:
			b.WriteString(": a line")
		default:
			b.WriteString(": not a line")
		}
	}
	if f.Note != "" {
		b.WriteString(": " + f.Note)
	}
	return b.String()
}

// contract reports whether the frame is contract data (message 10 or 18).
func (f BondLookupFrame) contract() bool {
	return f.Kind == BondFrameBondContractData || f.Kind == BondFrameContractData
}

// bondFrameLayout is the layout a contract frame is decoded with: the
// negotiated server version's, per IBKR's EDecoder (API 10.37), where a
// server before 164 sends a message version after the message id. A frame
// that names the request where the other reading expects it is decoded
// with the other reading and says so.
type bondFrameLayout struct {
	serverVersion int
	versionField  bool
}

func bondFrameLayoutFor(serverVersion int) bondFrameLayout {
	return bondFrameLayout{serverVersion: serverVersion, versionField: serverVersion < minServerVerSizeRules}
}

// String names the layout: the server version, and a version-field
// reading other than the version's own.
func (l bondFrameLayout) String() string {
	name := "server " + strconv.Itoa(l.serverVersion)
	switch {
	case l.versionField == (l.serverVersion < minServerVerSizeRules):
		return name
	case l.versionField:
		return name + ", read with a message version"
	}
	return name + ", read without a message version"
}

// reqIDIndex is the field that carries the request id.
func (l bondFrameLayout) reqIDIndex() int {
	if l.versionField {
		return 2
	}
	return 1
}

// bondFrameLayoutNaming is the layout a frame that names reqID is decoded
// with: the negotiated one when the frame names the request where it
// expects, else the other version-field reading when the frame names it
// there. ok is false for a frame of another request.
func bondFrameLayoutNaming(fields []string, reqID int, negotiated bondFrameLayout) (bondFrameLayout, bool) {
	id := strconv.Itoa(reqID)
	if strings.TrimSpace(safeGet(fields, negotiated.reqIDIndex())) == id {
		return negotiated, true
	}
	other := negotiated
	other.versionField = !negotiated.versionField
	if strings.TrimSpace(safeGet(fields, other.reqIDIndex())) == id {
		return other, true
	}
	return negotiated, false
}

// errBondFrameOtherRequest marks a contract frame that answers another
// request.
var errBondFrameOtherRequest = errors.New("the frame answers another request")

// wireReader reads a frame field by field and keeps the first failure,
// naming the field and its position.
type wireReader struct {
	fields []string
	idx    int
	err    error
}

func (r *wireReader) next(name string) (string, bool) {
	if r.err != nil {
		return "", false
	}
	if r.idx >= len(r.fields) {
		r.err = fmt.Errorf("the frame ends before the %s (%d fields)", name, len(r.fields))
		return "", false
	}
	v := r.fields[r.idx]
	r.idx++
	return v, true
}

func (r *wireReader) str(name string) string {
	v, _ := r.next(name)
	return strings.TrimSpace(v)
}

// integer reads an integer field; an empty field reads as zero.
func (r *wireReader) integer(name string) int {
	raw, ok := r.next(name)
	v := strings.TrimSpace(raw)
	if !ok || v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.err = fmt.Errorf("the %s %q is not an integer (field %d)", name, wireValue(raw), r.idx-1)
		return 0
	}
	return n
}

// number reads a finite numeric field; an empty field reads as zero.
func (r *wireReader) number(name string) float64 {
	raw, ok := r.next(name)
	v := strings.TrimSpace(raw)
	if !ok || v == "" {
		return 0
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		r.err = fmt.Errorf("the %s %q is not a finite number (field %d)", name, wireValue(raw), r.idx-1)
		return 0
	}
	return n
}

// flag reads a 0/1 field, as IBKR sends booleans; empty reads false.
func (r *wireReader) flag(name string) {
	raw, ok := r.next(name)
	switch strings.TrimSpace(raw) {
	case "", "0", "1":
		return
	}
	if ok {
		r.err = fmt.Errorf("the %s %q is not 0 or 1 (field %d)", name, wireValue(raw), r.idx-1)
	}
}

// tagValues reads a counted tag/value list; entries with an empty tag or
// value are skipped.
func (r *wireReader) tagValues(name string) map[string]string {
	count := r.integer(name + " count")
	if r.err != nil {
		return nil
	}
	if left := len(r.fields) - r.idx; count < 0 || count > left/2 {
		r.err = fmt.Errorf("the %s count %d does not fit the %d fields left", name, count, left)
		return nil
	}
	var out map[string]string
	for range count {
		tag, value := strings.ToUpper(r.str(name+" tag")), r.str(name+" value")
		if tag != "" && value != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[tag] = value
		}
	}
	return out
}

// finish reports whether the whole frame was read: every field, or all
// but the one empty field IBKR's closing separator leaves.
func (r *wireReader) finish(layout bondFrameLayout) error {
	if r.err != nil {
		return r.err
	}
	rest := len(r.fields) - r.idx
	if rest == 0 || (rest == 1 && strings.TrimSpace(r.fields[r.idx]) == "") {
		return nil
	}
	return fmt.Errorf("%d fields past the %d the %s layout reads", rest, r.idx, layout)
}

// bondLineIdentity refuses a frame whose identity did not decode: a field
// up to the minimum tick that ended the frame or had the wrong type, no
// positive contract id, or a currency that is not a three-letter code.
func bondLineIdentity(d BondContractDetails, err error) error {
	switch {
	case err != nil:
		return err
	case d.ConID <= 0:
		return errors.New("the frame carries no positive contract id")
	case d.Currency != "" && !bondCurrencyCode(d.Currency):
		return fmt.Errorf("the currency %q is not a three-letter code", wireValue(d.Currency))
	}
	return nil
}

// bondLineGaps names what a decoded line lacks.
func bondLineGaps(d BondContractDetails, rawMaturity string) []string {
	var gaps []string
	if _, ok := d.MaturityDate(); !ok {
		if strings.TrimSpace(rawMaturity) == "" {
			gaps = append(gaps, "no maturity")
		} else {
			gaps = append(gaps, fmt.Sprintf("maturity %q unreadable", wireValue(rawMaturity)))
		}
	}
	if d.Currency == "" {
		gaps = append(gaps, "no currency (the request's stands in)")
	}
	return gaps
}

// decodeBondContractData decodes one bondContractData frame in layout
// (IBKR's EDecoder.processBondContractDataMsg). The error says why the
// frame is not a line; the notes say what the line lacks. The size rules,
// secIdList, long name and hours are kept only when the whole frame
// decodes, so a field IBKR adds or moves can never shift into a size rule.
func decodeBondContractData(fields []string, reqID int, layout bondFrameLayout) (BondContractDetails, []string, error) {
	r := &wireReader{fields: fields}
	if id := r.integer("message id"); r.err != nil || id != msgBondContractData {
		return BondContractDetails{}, nil, fmt.Errorf("the message id %q is not bondContractData", wireValue(safeGet(fields, 0)))
	}
	version := 6
	if layout.versionField {
		if version = r.integer("message version"); r.err == nil && version < 1 {
			r.err = fmt.Errorf("the message version %d is not positive", version)
		}
	}
	d := BondContractDetails{ReqID: -1}
	if version >= 3 {
		d.ReqID = r.integer("request id")
	}
	if r.err != nil {
		return BondContractDetails{}, nil, r.err
	}
	if reqID != 0 && d.ReqID != reqID {
		return BondContractDetails{}, nil, errBondFrameOtherRequest
	}
	d.Symbol = r.str("symbol")
	d.SecType = strings.ToUpper(r.str("security type"))
	d.CUSIPField = r.str("cusip")
	d.Coupon = r.number("coupon")
	maturity := r.str("maturity")
	d.Maturity = bondWireDate(maturity)
	d.IssueDate = bondWireDate(r.str("issue date"))
	r.str("ratings")
	d.BondType = r.str("bond type")
	d.CouponType = r.str("coupon type")
	r.flag("convertible flag")
	r.flag("callable flag")
	r.flag("putable flag")
	d.DescAppend = r.str("description")
	d.Exchange = strings.ToUpper(r.str("exchange"))
	d.Currency = strings.ToUpper(r.str("currency"))
	d.MarketName = r.str("market name")
	d.TradingClass = r.str("trading class")
	d.ConID = r.integer("contract id")
	d.MinTick = r.number("minimum tick")
	if err := bondLineIdentity(d, r.err); err != nil {
		return BondContractDetails{}, nil, err
	}
	notes := bondLineGaps(d, maturity)

	tail := d
	sv := layout.serverVersion
	if sv >= minServerVerMdSizeMultiplier && sv < minServerVerSizeRules {
		r.integer("md size multiplier")
	}
	r.str("order types")
	tail.ValidExchanges = r.str("valid exchanges")
	r.str("next option date")
	r.str("next option type")
	r.flag("next option partial flag")
	r.str("notes")
	if version >= 4 {
		tail.LongName = r.str("long name")
	}
	if sv >= minServerVerBondTradingHours {
		tail.TimeZoneID = r.str("time zone")
		tail.TradingHours = r.str("trading hours")
		tail.LiquidHours = r.str("liquid hours")
	}
	if version >= 6 {
		r.str("ev rule")
		r.number("ev multiplier")
	}
	if version >= 5 {
		tail.SecIDs = r.tagValues("secIdList")
	}
	if sv >= minServerVerAggGroup {
		r.integer("agg group")
	}
	if sv >= minServerVerMarketRules {
		r.str("market rule ids")
	}
	if sv >= minServerVerSizeRules {
		tail.MinSize = r.number("minimum size")
		tail.SizeIncrement = r.number("size increment")
		r.number("suggested size increment")
	}
	if err := r.finish(layout); err != nil {
		return d, append(notes, "size rules, identifiers and hours not kept: "+err.Error()), nil
	}
	tail.Complete = true
	return tail, notes, nil
}

// decodeContractDataLine decodes an ordinary contractData frame (message
// 10) that answered a bill or bond request into a line (IBKR's
// EDecoder.processContractDataMsg). The bond-only fields (coupon, issue
// date, the cusip field, bond and coupon types) stay empty; the maturity is
// the lastTradeDateOrContractMonth, else the lastTradeDate, else, on a
// whole frame, the real expiration date. The rest follows
// decodeBondContractData.
func decodeContractDataLine(fields []string, reqID int, layout bondFrameLayout) (BondContractDetails, []string, error) {
	r := &wireReader{fields: fields}
	if id := r.integer("message id"); r.err != nil || id != msgContractData {
		return BondContractDetails{}, nil, fmt.Errorf("the message id %q is not contractData", wireValue(safeGet(fields, 0)))
	}
	version := 8
	if layout.versionField {
		if version = r.integer("message version"); r.err == nil && version < 1 {
			r.err = fmt.Errorf("the message version %d is not positive", version)
		}
	}
	d := BondContractDetails{ReqID: -1}
	if version >= 3 {
		d.ReqID = r.integer("request id")
	}
	if r.err != nil {
		return BondContractDetails{}, nil, r.err
	}
	if reqID != 0 && d.ReqID != reqID {
		return BondContractDetails{}, nil, errBondFrameOtherRequest
	}
	sv := layout.serverVersion
	d.Symbol = r.str("symbol")
	d.SecType = strings.ToUpper(r.str("security type"))
	maturity := r.str("last trade date or contract month")
	if sv >= minServerVerLastTradeDate {
		if lastTradeDate := r.str("last trade date"); maturity == "" {
			maturity = lastTradeDate
		}
	}
	r.number("strike")
	r.str("right")
	d.Exchange = strings.ToUpper(r.str("exchange"))
	d.Currency = strings.ToUpper(r.str("currency"))
	r.str("local symbol")
	d.MarketName = r.str("market name")
	d.TradingClass = r.str("trading class")
	d.ConID = r.integer("contract id")
	d.MinTick = r.number("minimum tick")
	d.Maturity = bondWireDate(maturity)
	if err := bondLineIdentity(d, r.err); err != nil {
		return BondContractDetails{}, nil, err
	}

	tail := d
	realExpiration := ""
	if sv >= minServerVerMdSizeMultiplier && sv < minServerVerSizeRules {
		r.integer("md size multiplier")
	}
	r.str("multiplier")
	r.str("order types")
	tail.ValidExchanges = r.str("valid exchanges")
	if version >= 2 {
		r.integer("price magnifier")
	}
	if version >= 4 {
		r.integer("underlying contract id")
	}
	if version >= 5 {
		tail.LongName = r.str("long name")
		r.str("primary exchange")
	}
	if version >= 6 {
		for _, name := range []string{"contract month", "industry", "category", "subcategory"} {
			r.str(name)
		}
		tail.TimeZoneID = r.str("time zone")
		tail.TradingHours = r.str("trading hours")
		tail.LiquidHours = r.str("liquid hours")
	}
	if version >= 8 {
		r.str("ev rule")
		r.number("ev multiplier")
	}
	if version >= 7 {
		tail.SecIDs = r.tagValues("secIdList")
	}
	if sv >= minServerVerAggGroup {
		r.integer("agg group")
	}
	if sv >= minServerVerUnderlyingInfo {
		r.str("underlying symbol")
		r.str("underlying security type")
	}
	if sv >= minServerVerMarketRules {
		r.str("market rule ids")
	}
	if sv >= minServerVerRealExpiration {
		realExpiration = r.str("real expiration date")
	}
	if sv >= minServerVerStockType {
		r.str("stock type")
	}
	if sv >= minServerVerFractionalSize && sv < minServerVerSizeRules {
		r.number("size minimum tick")
	}
	if sv >= minServerVerSizeRules {
		tail.MinSize = r.number("minimum size")
		tail.SizeIncrement = r.number("size increment")
		r.number("suggested size increment")
	}
	if sv >= minServerVerFundDataFields && d.SecType == "FUND" {
		for range 7 {
			r.str("fund field")
		}
		for range 3 {
			r.flag("fund closed flag")
		}
		for range 7 {
			r.str("fund field")
		}
	}
	if sv >= minServerVerIneligibility {
		r.tagValues("ineligibility reasons")
	}
	tailErr := r.finish(layout)
	if tailErr == nil {
		if _, ok := tail.MaturityDate(); !ok && realExpiration != "" {
			tail.Maturity, maturity = bondWireDate(realExpiration), realExpiration
		}
		tail.Complete = true
		d = tail
	}
	notes := append(bondLineGaps(d, maturity), "an ordinary contractData frame: coupon, issue date and cusip field not sent")
	if tailErr != nil {
		notes = append(notes, "size rules, identifiers and hours not kept: "+tailErr.Error())
	}
	return d, notes, nil
}

// bondFrameSummary reduces a bondContractData or contractData frame to its
// identifiers by their position in layout, whether or not it decodes.
func bondFrameSummary(fields []string, layout bondFrameLayout) BondLookupFrame {
	msgID, _ := strconv.Atoi(strings.TrimSpace(safeGet(fields, 0)))
	f := BondLookupFrame{MessageID: msgID, Fields: len(fields), Layout: layout.String()}
	at := func(i int) string { return wireValue(safeGet(fields, i)) }
	off := 0
	if layout.versionField {
		off = 1
	}
	switch msgID {
	case msgBondContractData:
		f.Kind = BondFrameBondContractData
		f.Symbol, f.SecType, f.CUSIP, f.Maturity = at(2+off), at(3+off), at(4+off), at(6+off)
		f.Exchange, f.Currency, f.ConID = at(15+off), at(16+off), at(19+off)
	case msgContractData:
		f.Kind = BondFrameContractData
		f.Symbol, f.SecType, f.Maturity = at(2+off), at(3+off), at(4+off)
		if layout.serverVersion >= minServerVerLastTradeDate {
			off++ // the lastTradeDate follows the lastTradeDateOrContractMonth
		}
		f.Exchange, f.Currency, f.LocalSymbol, f.ConID = at(7+off), at(8+off), at(9+off), at(12+off)
	}
	return f
}

// bondTapFrame is the record of a frame other than contract data that
// names reqID: an error (text message 4, or the protobuf notice 204) with
// IBKR's code and text, or any other message by its id and field count.
// ok is false for a frame that does not name reqID; contract frames belong
// to the request's own handlers.
func bondTapFrame(fields []string, reqID int) (BondLookupFrame, bool) {
	msgID, err := strconv.Atoi(strings.TrimSpace(safeGet(fields, 0)))
	if err != nil {
		return BondLookupFrame{}, false
	}
	id := strconv.Itoa(reqID)
	f := BondLookupFrame{MessageID: msgID, Kind: BondFrameOther, Fields: len(fields)}
	switch msgID {
	case msgContractData, msgBondContractData, msgContractDataEnd:
		return BondLookupFrame{}, false
	case msgSystemNotification:
		note, err := parseSystemNotificationPayload([]byte(safeGet(fields, 1)))
		if err != nil || note.tickerID != int64(reqID) {
			return BondLookupFrame{}, false
		}
		f.Kind, f.Code, f.Note = BondFrameError, note.code, brokerNoticeLine(note.message)
		return f, true
	case msgErrMsg:
		// [4, reqID, code, text, …] from server 194, [4, version, reqID, code, text] before.
		for _, at := range []int{1, 2} {
			if strings.TrimSpace(safeGet(fields, at)) == id {
				code, _ := strconv.Atoi(strings.TrimSpace(safeGet(fields, at+1)))
				f.Kind, f.Code, f.Note = BondFrameError, code, brokerNoticeLine(safeGet(fields, at+2))
				return f, true
			}
		}
		return BondLookupFrame{}, false
	}
	if strings.TrimSpace(safeGet(fields, 1)) != id && strings.TrimSpace(safeGet(fields, 2)) != id {
		return BondLookupFrame{}, false
	}
	return f, true
}

// wireValue keeps a wire field readable in a log or JSON line: printable
// ASCII only, trimmed, at most 40 characters.
func wireValue(raw string) string {
	v := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, raw))
	if len(v) > 40 {
		v = v[:40] + "…"
	}
	return v
}

// bondFrameLogMax caps the frames one request form records; the rest are
// counted.
const bondFrameLogMax = 16

// bondFrameLog records the frames that name one request form's id. The
// reader goroutine adds; the lookup reads when the form ends.
type bondFrameLog struct {
	mu      sync.Mutex
	frames  []BondLookupFrame
	omitted int
}

func (l *bondFrameLog) add(f BondLookupFrame) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.frames) < bondFrameLogMax {
		l.frames = append(l.frames, f)
	} else {
		l.omitted++
	}
}

func (l *bondFrameLog) snapshot() ([]BondLookupFrame, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]BondLookupFrame(nil), l.frames...), l.omitted
}

// emptyNote says why a form that ended without a line found none: no frame
// named the request before its end marker, or the contract frames that did
// were not lines (the first one's reason).
func (l *bondFrameLog) emptyNote() string {
	frames, omitted := l.snapshot()
	refused, others, first := 0, omitted, ""
	for _, f := range frames {
		switch {
		case f.contract() && !f.Line:
			if refused++; first == "" {
				first = f.Kind + ": " + f.Note
			}
		case f.Kind != BondFrameContractDataEnd:
			others++
		}
	}
	switch {
	case refused == 1:
		return "the search ended without a line: IBKR sent 1 contract frame that did not decode (" + first + ")"
	case refused > 1:
		return fmt.Sprintf("the search ended without a line: IBKR sent %d contract frames that did not decode (the first %s)", refused, first)
	case others > 0:
		return fmt.Sprintf("the search ended without a line: %d frames of other messages named the request before its end marker", others)
	}
	return "the search ended without a line: no frame named the request before its end marker"
}
