package twssim

import (
	"strings"
	"testing"
)

func fieldsOf(body []byte) []string {
	return strings.Split(strings.TrimSuffix(string(body), "\x00"), "\x00")
}

// A Hong Kong listing answers in its own facts: valid exchanges without
// SMART, the board lot in the size fields, the Hong Kong grid and time zone;
// a symbol search returns it beside its NYSE ADR; an order off the lot is
// rejected on WhatIf and on place.
func TestHongKongListingSearchAndLot(t *testing.T) {
	s := Start(t, Options{})
	cl := dialClient(t, s, 15)
	cl.send(textMessage(reqMatchingSymbols, "5", "synh"))
	_, body := cl.until(msgSymbolSamples)
	f := fieldsOf(body)
	if f[0] != "5" || f[1] != "2" || f[2] != "900701" || f[5] != "SEHK" || f[6] != "HKD" || f[7] != "0" || f[8] != "SYNTHETIC H HOLDINGS LTD" ||
		f[10] != "900702" || f[13] != "NYSE" || f[14] != "USD" {
		t.Fatalf("symbol samples %q", f)
	}
	cl.send(textMessage(reqMatchingSymbols, "6", "SYNTHETIC H HOLD"))
	if _, body := cl.until(msgSymbolSamples); fieldsOf(body)[1] != "2" {
		t.Fatalf("name search %q", fieldsOf(body))
	}
	cl.send(textMessage(reqContractDetails, "8", "7", "900701", "SYNH", "STK", "", "0", "", "", "SEHK", "", "HKD"))
	_, body = cl.until(msgContractDetails)
	d := strings.Join(fieldsOf(body), "|")
	for _, want := range []string{"|SEHK|HKD|9901|", "|0.05|", "|SEHK|1|0|SYNTHETIC H HOLDINGS LTD|SEHK|", "|Asia/Hong_Kong|", "|4|", "|COMMON|100|100|100|0"} {
		if !strings.Contains(d, want) {
			t.Fatalf("contract details lack %q: %s", want, d)
		}
	}
	cl.send(textMessage(reqMarketRule, "4"))
	if _, body := cl.until(msgMarketRule); !strings.HasPrefix(strings.Join(fieldsOf(body), "|"), "4|11|0|0.001|0.25|0.005") {
		t.Fatalf("market rule %q", fieldsOf(body))
	}
	for _, whatIf := range []bool{true, false} {
		var contract protoWriter
		contract = contract.varint(1, 900701).str(2, "SYNH").str(3, "STK").str(8, "SEHK").str(10, "HKD")
		var order protoWriter
		order = order.varint(1, 7).str(5, "BUY").str(6, "50").str(8, "LMT").double(9, 82.6).str(11, "DAY").str(12, DefaultAccount).boolean(65, whatIf).boolean(66, true)
		var b protoWriter
		b = b.varint(1, 40).bytes(2, contract).bytes(3, order)
		cl.send(protoMessage(placeOrder, b))
		_, body := cl.until(msgError + protoOffset)
		if e := string(body); !strings.Contains(e, "board lot of 100 shares for SYNH") {
			t.Fatalf("odd lot (WhatIf %v): %s", whatIf, e)
		}
	}
	if len(s.Book().Orders()) != 0 {
		t.Fatalf("an odd lot left an order: %+v", s.Book().Orders())
	}
}
