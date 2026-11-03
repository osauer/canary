package ibkr

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseOrderWhatIfErrorAndNoticeKeepCodeAndText(t *testing.T) {
	reqID, code, msg, ok := parseOrderWhatIfError([]string{"4", "2", "77", "201", "Order rejected - reason:Insufficient margin"})
	if !ok || reqID != 77 || code != 201 || msg != "Order rejected - reason:Insufficient margin" {
		t.Fatalf("error frame: %d %d %q %t", reqID, code, msg, ok)
	}
	if _, _, _, ok := parseOrderWhatIfError([]string{"4", "2", "77", "0", "x"}); ok {
		t.Fatal("code 0 accepted")
	}
	if _, _, _, ok := parseOrderWhatIfError([]string{"4", "2", "77"}); ok {
		t.Fatal("short frame accepted")
	}
	notice := timestampedSystemNotice(77, time.Now(), 10349, "Order rejected")
	reqID, code, msg, adv, ok := parseOrderWhatIfSystemNotice(notice)
	if !ok || reqID != 77 || code != 10349 || msg != "Order rejected" || adv != "" {
		t.Fatalf("notice: %d %d %q %q %t", reqID, code, msg, adv, ok)
	}
	if _, _, _, _, ok := parseOrderWhatIfSystemNotice(timestampedSystemNotice(-1, time.Now(), 2104, "farm ok")); ok {
		t.Fatal("farm notice taken as an order answer")
	}
	if _, _, _, _, ok := parseOrderWhatIfSystemNotice([]string{"204"}); ok {
		t.Fatal("short notice accepted")
	}
	if _, _, _, _, ok := parseOrderWhatIfSystemNotice([]string{"204", "\xff"}); ok {
		t.Fatal("malformed notice accepted")
	}
	got := orderWhatIfErrorMessage(201, "  Insufficient margin ", ` {"x":1} `)
	if !strings.Contains(got, "code 201: Insufficient margin") || !strings.HasSuffix(got, `advanced_reject_json={"x":1}`) {
		t.Fatalf("message = %q", got)
	}
	if got := orderWhatIfErrorMessage(201, "", ""); !strings.HasSuffix(got, "error code 201") {
		t.Fatalf("bare message = %q", got)
	}
	for _, code := range []int{2104, 2106, 2107, 2119, 2158, 2169, 2150} {
		if !orderWhatIfInformationalError(code) {
			t.Fatalf("code %d not informational", code)
		}
	}
	if orderWhatIfInformationalError(201) || orderWhatIfInformationalError(2200) {
		t.Fatal("rejection taken as informational")
	}
}

func TestOrderWhatIfStateDetection(t *testing.T) {
	for _, s := range []string{"Pending", "PendingSubmit", "ApiSent", "ApiPending", "PreSubmitted", "Submitted", "Inactive", "Cancelled", "ApiCancelled", "Rejected", " submitted "} {
		if !orderWhatIfStateStatus(s) {
			t.Errorf("%q not a state status", s)
		}
	}
	if orderWhatIfStateStatus("Filled") || orderWhatIfStateStatus("") {
		t.Error("non-state status accepted")
	}
	for _, s := range []string{"Inactive", "Cancelled", "ApiCancelled", "Rejected"} {
		if !orderWhatIfRejectedStatus(s) {
			t.Errorf("%q not rejected", s)
		}
	}
	if orderWhatIfRejectedStatus("Submitted") {
		t.Error("submitted taken as rejected")
	}
	if idx := findOrderWhatIfStateIndex([]string{"5", "1", "77", "0", "1", "Nope", "1", "PreSubmitted"}); idx != 6 {
		t.Fatalf("state index = %d", idx)
	}
	if idx := findOrderWhatIfStateIndex([]string{"5", "0", "Submitted"}); idx != -1 {
		t.Fatalf("state index without the flag = %d", idx)
	}
	for raw, want := range map[string]bool{"1": true, "true": true, "YES": true, "0": false, "": false, "no": false} {
		if parseIBKRBool(raw) != want {
			t.Errorf("parseIBKRBool(%q) = %t", raw, !want)
		}
	}
	for _, raw := range []string{"", "N/A", "NaN", "Inf", "x", "1.7976931348623157e308"} {
		if parseIBKRFloatPtr(raw) != nil {
			t.Errorf("parseIBKRFloatPtr(%q) accepted", raw)
		}
	}
	if v := parseIBKRFloatPtr(" 1,234.5 "); v == nil || *v != 1234.5 {
		t.Fatalf("grouped number = %v", v)
	}
}

// whatIfMarginTail is the orderState tail of a legacy openOrder frame as
// EDecoder reads it: before/change/after margin triples from server 142,
// commission fields, then from server 195 the margin currency, nine
// outside-RTH figures, suggested size, reject reason and allocations, and
// the warning text last.
func whatIfMarginTail(serverVersion int, rejectReason string, allocations int) []string {
	var tail []string
	if serverVersion >= 142 {
		tail = append(tail, "1000", "900", "50000", "100", "90", "-100")
	}
	tail = append(tail, "1100", "990", "49900", "1.5", "1", "2", "USD")
	if serverVersion >= 195 {
		tail = append(tail, "EUR")
		for range 9 {
			tail = append(tail, "0")
		}
		tail = append(tail, "7", rejectReason, strconv.Itoa(allocations))
		for range allocations * 7 {
			tail = append(tail, "a")
		}
	}
	return append(tail, "warning text")
}

func TestParseOrderWhatIfMarginFollowsServerVersionLayout(t *testing.T) {
	for _, version := range []int{141, 142, 195, maxClientVersion} {
		margin, reject := parseOrderWhatIfMargin(whatIfMarginTail(version, "", 0), 0, version)
		if version >= 142 {
			if margin.InitialMarginBefore == nil || *margin.InitialMarginBefore != 1000 || *margin.MaintenanceMarginBefore != 900 || *margin.EquityWithLoanBefore != 50000 {
				t.Fatalf("server %d: before figures %+v", version, margin)
			}
		} else if margin.InitialMarginBefore != nil {
			t.Fatalf("server %d has no before figures: %+v", version, margin)
		}
		if margin.InitialMarginAfter == nil || *margin.InitialMarginAfter != 1100 || *margin.MaintenanceMarginAfter != 990 || *margin.EquityWithLoanAfter != 49900 ||
			*margin.Commission != 1.5 || *margin.MinCommission != 1 || *margin.MaxCommission != 2 || margin.CommissionCurrency != "USD" {
			t.Fatalf("server %d: after figures %+v", version, margin)
		}
		if version >= 195 && margin.Currency != "EUR" || version < 195 && margin.Currency != "" {
			t.Fatalf("server %d: currency %q", version, margin.Currency)
		}
		if margin.WarningText != "warning text" || reject != "" {
			t.Fatalf("server %d: warning %q reject %q", version, margin.WarningText, reject)
		}
	}
	margin, reject := parseOrderWhatIfMargin(whatIfMarginTail(203, "margin check failed", 2), 0, 203)
	if reject != "margin check failed" || margin.WarningText != "warning text" {
		t.Fatalf("allocations skipped wrongly: reject %q warning %q", reject, margin.WarningText)
	}
}

// A legacy openOrder answer to a WhatIf is recognized by order id, read for
// its margin tail, and classified: a rejected broker status or a reject
// reason is a rejection; another order's frame is not an answer. Servers
// below 145 (MIN_SERVER_VER_ORDER_CONTAINER) put a version field before the
// order id; later legacy servers do not, and the order id is then the first
// field, numeric like a version. The server version decides, as in EDecoder.
func TestParseOrderWhatIfOpenOrderLegacyFrame(t *testing.T) {
	frame := func(version string, orderID int, status string, tail []string) []string {
		fields := []string{strconv.Itoa(msgOpenOrder)}
		if version != "" {
			fields = append(fields, version)
		}
		fields = append(fields, strconv.Itoa(orderID), "4242", "SYNX", "STK", "", "0", "", "", "SMART", "USD", "SYNX", "NMS", "BUY", "10", "LMT", "100", "0", "DAY", "", "DU1234567", "", "0", "ref", "15", "9001", "0", "0", "0", "1", status)
		return append(fields, tail...)
	}
	for _, tc := range []struct {
		server  int
		version string
	}{{144, "45"}, {145, ""}, {200, ""}, {202, ""}} {
		result, ok := parseOrderWhatIfOpenOrder(frame(tc.version, 77, "PreSubmitted", whatIfMarginTail(tc.server, "", 0)), 77, tc.server)
		if !ok || result.Status != OrderWhatIfStatusAccepted || result.BrokerStatus != "PreSubmitted" || result.OrderID != 77 || result.Margin.InitialMarginAfter == nil || *result.Margin.InitialMarginAfter != 1100 {
			t.Fatalf("server %d accepted preview: %+v ok=%t", tc.server, result, ok)
		}
		result, ok = parseOrderWhatIfOpenOrder(frame(tc.version, 77, "Inactive", whatIfMarginTail(tc.server, "", 0)), 77, tc.server)
		if !ok || result.Status != OrderWhatIfStatusRejected || !strings.Contains(result.Message, "Inactive") {
			t.Fatalf("server %d inactive preview: %+v ok=%t", tc.server, result, ok)
		}
		if tc.server >= 195 {
			result, ok = parseOrderWhatIfOpenOrder(frame(tc.version, 77, "Submitted", whatIfMarginTail(tc.server, "not enough buying power", 0)), 77, tc.server)
			if !ok || result.Status != OrderWhatIfStatusRejected || result.Message != "not enough buying power" {
				t.Fatalf("server %d reject reason: %+v ok=%t", tc.server, result, ok)
			}
		}
		if _, ok := parseOrderWhatIfOpenOrder(frame(tc.version, 78, "Submitted", whatIfMarginTail(tc.server, "", 0)), 77, tc.server); ok {
			t.Fatalf("server %d: another order's frame taken as the answer", tc.server)
		}
		// A frame whose whatIf flag is off has no preview state.
		noState := frame(tc.version, 77, "Submitted", nil)
		noState[len(noState)-2] = "0"
		if _, ok := parseOrderWhatIfOpenOrder(noState, 77, tc.server); ok {
			t.Fatalf("server %d: frame without the whatIf flag accepted", tc.server)
		}
	}
	// The conId of another preview must never be mistaken for the order id:
	// order 4242 is not previewed here, order 77 is.
	if result, ok := parseOrderWhatIfOpenOrder(frame("", 77, "PreSubmitted", whatIfMarginTail(200, "", 0)), 4242, 200); ok {
		t.Fatalf("conId read as the order id: %+v", result)
	}
	if _, ok := parseOrderWhatIfOpenOrder([]string{"3", "77"}, 77, 203); ok {
		t.Fatal("an orderStatus frame taken as an openOrder")
	}
	if _, ok := parseOrderWhatIfOpenOrder(nil, 77, 203); ok {
		t.Fatal("empty frame accepted")
	}
}

// The protobuf openOrder summary names the preview by orderId= and whatIf=
// and carries the margin figures by name.
func TestParseOrderWhatIfOpenOrderProtoSummary(t *testing.T) {
	summary := []string{"5", "protobuf", "orderId=77", "whatIf=true", "status=PreSubmitted", "initMarginBefore=1000", "maintMarginBefore=900", "equityWithLoanBefore=50000",
		"initMarginAfter=1100", "maintMarginAfter=990", "equityWithLoanAfter=49900", "commission=1.5", "minCommission=1", "maxCommission=2", "commissionCurrency=USD", "marginCurrency=EUR", "warningText=w"}
	result, ok := parseOrderWhatIfOpenOrder(summary, 77, 203)
	if !ok || result.Status != OrderWhatIfStatusAccepted || result.Margin.Currency != "EUR" || *result.Margin.InitialMarginBefore != 1000 || *result.Margin.MaxCommission != 2 || result.Margin.WarningText != "w" {
		t.Fatalf("proto preview: %+v ok=%t", result, ok)
	}
	rejected := append(summary[:5:5], "rejectReason=insufficient")
	if result, ok := parseOrderWhatIfOpenOrder(rejected, 77, 203); !ok || result.Status != OrderWhatIfStatusRejected || result.Message != "insufficient" {
		t.Fatalf("proto rejection: %+v ok=%t", result, ok)
	}
	inactive := []string{"5", "protobuf", "orderId=77", "whatIf=true", "status=Inactive"}
	if result, ok := parseOrderWhatIfOpenOrder(inactive, 77, 203); !ok || result.Status != OrderWhatIfStatusRejected {
		t.Fatalf("proto inactive: %+v ok=%t", result, ok)
	}
	for name, fields := range map[string][]string{
		"another order":   {"5", "protobuf", "orderId=78", "whatIf=true", "status=PreSubmitted"},
		"a working order": {"5", "protobuf", "orderId=77", "whatIf=false", "status=PreSubmitted"},
		"no status":       {"5", "protobuf", "orderId=77", "whatIf=true"},
	} {
		if _, ok := parseOrderWhatIfOpenOrder(fields, 77, 203); ok {
			t.Fatalf("%s accepted", name)
		}
	}
}
