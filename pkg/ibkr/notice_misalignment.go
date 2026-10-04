package ibkr

import (
	"regexp"
	"strconv"
	"strings"
)

// A request frame the gateway reads shifted by one byte or one field puts the
// wrong value into every later slot: an exchange name loses its first byte
// and lands where a number was expected, and the gateway answers with a
// request-parse error that quotes what it saw — "MART" for SMART, "BOE" for
// CBOE, or a NumberFormatException naming a word. That echo, and only that
// echo, is the parser-misalignment signature. (Until 2026-10-04 any notice
// whose text merely contained those letters was flagged, so an ordinary
// "HMDS query returned no data: AAPL@SMART Trades" logged at ERROR.)

// parseFaultCodes are the gateway's "could not read or validate the request"
// answers; a shifted frame surfaces through one of them.
var parseFaultCodes = map[int]bool{320: true, 321: true, 322: true, 323: true}

// misalignmentVenues are routing venues whose first-byte-shifted spelling is
// the recognisable fragment. Fragments shorter than three letters, or equal
// to another venue's name, are not distinctive and are skipped.
var misalignmentVenues = []string{
	"SMART", "CBOE", "CBOE2", "NASDAQ", "NASDAQOM", "NASDAQBX", "NYSE", "NYSENAT", "ARCA", "ARCAEDGE", "AMEX",
	"ISLAND", "IDEALPRO", "GLOBEX", "NYMEX", "COMEX", "CBOT", "ECBOT", "ICEUS", "NYBOT", "IBIS", "IBIS2", "XETRA",
	"SEHK", "TSE", "TSEJ", "BATS", "EDGEA", "EDGX", "PAXOS", "HKFE", "PHLX", "MIAX", "GEMINI", "MERCURY", "PEARL",
	"EMERALD", "MEMX", "LTSE", "OVERNIGHT", "DRCTEDGE", "PINK", "OTCLNK", "VALUE", "FUNDSERV", "LSEETF", "EUREX",
	"BVME", "SNFE", "TRADEWEB", "BONDDESK", "ICEEU", "ICEEUSOFT", "ENDEX", "MONEP", "MEFFRV", "IDEM", "OMS",
}

var shiftedVenueFragments = func() map[string]bool {
	venues := map[string]bool{}
	for _, v := range misalignmentVenues {
		venues[v] = true
	}
	out := map[string]bool{}
	for _, v := range misalignmentVenues {
		fragment := v[1:]
		if len(fragment) < 3 || venues[fragment] {
			continue
		}
		out[fragment] = true
	}
	return out
}()

var (
	noticeTokenPattern = regexp.MustCompile(`[A-Z0-9.]+`)
	// "For input string: "X"" is Java's NumberFormatException detail; a
	// non-numeric X at the gateway means text landed in a numeric slot.
	numberFormatInput = regexp.MustCompile(`FOR INPUT STRING: "([^"]*)"`)
)

// noticeSignalsParserMisalignment reports whether a broker notice is the
// gateway describing a request frame it read misaligned. Only a parse-fault
// code or an explicit parse-exception text qualifies, and then only when the
// text carries a shifted venue fragment as a token of its own or a
// non-numeric value in a NumberFormatException. Ordinary notices that mention
// SMART or CBOE in passing are not misalignments.
func noticeSignalsParserMisalignment(code int, message string) bool {
	upper := strings.ToUpper(message)
	parseFault := parseFaultCodes[code] ||
		strings.Contains(upper, "NUMBERFORMATEXCEPTION") ||
		strings.Contains(upper, "UNABLE TO PARSE") ||
		strings.Contains(upper, "ERROR READING REQUEST")
	if !parseFault {
		return false
	}
	if m := numberFormatInput.FindStringSubmatch(upper); m != nil {
		if _, err := strconv.ParseFloat(strings.TrimSpace(m[1]), 64); err != nil {
			return true
		}
	}
	for _, token := range noticeTokenPattern.FindAllString(upper, -1) {
		if shiftedVenueFragments[strings.Trim(token, ".")] {
			return true
		}
	}
	return false
}
