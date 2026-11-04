package futu

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Every row names its page and the date it was read; frequency-limited
// rows carry the 30-second window; no row names the trade unlock.
func TestLimitsTableIsSourcedAndComplete(t *testing.T) {
	seen := map[uint32]bool{}
	for _, l := range Limits {
		if l.Name == "" || !strings.HasPrefix(l.Source, "https://openapi.futunn.com/futu-api-doc/en/") || l.Read == "" {
			t.Errorf("row %d lacks a name, page or read date: %+v", l.ProtoID, l)
		}
		if _, err := time.Parse("2006-01-02", l.Read); err != nil {
			t.Errorf("row %d read date %q: %v", l.ProtoID, l.Read, err)
		}
		if (l.Requests > 0) != (l.Window > 0) {
			t.Errorf("row %d: requests %d with window %s", l.ProtoID, l.Requests, l.Window)
		}
		if l.ProtoID == 2005 {
			t.Error("the table names the trade unlock")
		}
		if seen[l.ProtoID] {
			t.Errorf("row %d listed twice", l.ProtoID)
		}
		seen[l.ProtoID] = true
	}
	for _, want := range []struct {
		proto      uint32
		requests   int
		perRequest int
	}{
		{ProtoQotGetSecuritySnapshot, 60, 400},
		{ProtoQotRequestHistoryKL, 60, 1000},
		{ProtoTrdGetFunds, 10, 0},
		{ProtoTrdGetPositionList, 10, 0},
		{ProtoTrdGetMaxTrdQtys, 10, 0},
		{ProtoQotSub, 0, 0},
	} {
		l, ok := LimitFor(want.proto)
		if !ok || l.Requests != want.requests || l.PerRequest != want.perRequest {
			t.Errorf("row %d: %+v, want %d per window and %d per request", want.proto, l, want.requests, want.perRequest)
		}
	}
	if _, ok := LimitFor(2005); ok {
		t.Error("LimitFor knows the trade unlock")
	}
	if SubscriptionMinimumHold != time.Minute || HistoryQuotaWindow != 7*24*time.Hour || SubscriptionQuotaTiers[0] != 100 {
		t.Fatal("documented quota rules changed")
	}
}

// The pacer admits the documented count in a window, refuses the next with
// the wait until the oldest ages out, and admits again after it; a refusal
// spends nothing, and a protocol without a limit is never paced.
func TestPacerRefusesTheRequestAfterTheWindowIsSpentAndAdmitsAgainAfterIt(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	p := NewPacer(func() time.Time { return now })
	limit, _ := LimitFor(ProtoTrdGetFunds)
	for i := range limit.Requests {
		if err := p.Admit(ProtoTrdGetFunds); err != nil {
			t.Fatalf("request %d refused: %v", i+1, err)
		}
		now = now.Add(time.Second)
	}
	if got := p.Spent(ProtoTrdGetFunds); got != limit.Requests {
		t.Fatalf("spent %d, want %d", got, limit.Requests)
	}
	err := p.Admit(ProtoTrdGetFunds)
	var paced *PacedError
	if !errors.As(err, &paced) || !errors.Is(err, ErrPaced) {
		t.Fatalf("request %d: %v", limit.Requests+1, err)
	}
	// The oldest request was 10 seconds ago in a 30-second window.
	if paced.Limit.ProtoID != ProtoTrdGetFunds || paced.RetryAfter != 20*time.Second {
		t.Fatalf("refusal %+v", paced)
	}
	if !strings.Contains(err.Error(), "Trd_GetFunds (2101) allows 10 requests per 30s; next in 20s") {
		t.Fatalf("refusal text %q", err)
	}
	if p.Spent(ProtoTrdGetFunds) != limit.Requests {
		t.Fatal("a refusal spent a slot")
	}
	now = now.Add(paced.RetryAfter - time.Millisecond)
	if err := p.Admit(ProtoTrdGetFunds); !errors.Is(err, ErrPaced) {
		t.Fatalf("admitted a millisecond early: %v", err)
	}
	now = now.Add(time.Millisecond)
	if err := p.Admit(ProtoTrdGetFunds); err != nil {
		t.Fatalf("refused after the window opened: %v", err)
	}
	if p.Spent(ProtoTrdGetFunds) != limit.Requests {
		t.Fatalf("spent %d after one aged out and one came in", p.Spent(ProtoTrdGetFunds))
	}
	// Protocols are paced apart, and one without a limit never.
	if err := p.Admit(ProtoTrdGetPositionList); err != nil {
		t.Fatalf("positions paced by the funds window: %v", err)
	}
	for range 1000 {
		if err := p.Admit(ProtoQotGetBasicQot); err != nil {
			t.Fatalf("unlimited protocol paced: %v", err)
		}
	}
	if p.Spent(ProtoQotGetBasicQot) != 0 || p.Spent(9999) != 0 {
		t.Fatal("an unlimited or unknown protocol reports spend")
	}
	if err := NewPacer(nil).Admit(ProtoQotGetSecuritySnapshot); err != nil {
		t.Fatalf("wall-clock pacer: %v", err)
	}
}

func TestErrorRendersTheCodesAndClassifiesTheSession(t *testing.T) {
	e := &Error{ProtoID: ProtoTrdGetFunds, RetType: RetTypeFailed, ErrCode: 1234, Message: "no such account"}
	if e.Error() != `opend protocol 2101: retType -1 errCode 1234: "no such account"` || e.Session() {
		t.Fatalf("request error: %q session=%v", e, e.Session())
	}
	bare := &Error{ProtoID: ProtoInitConnect, RetType: RetTypeTimeOut}
	if bare.Error() != "opend protocol 1001: retType -100 errCode 0" || !bare.Session() {
		t.Fatalf("timeout: %q session=%v", bare, bare.Session())
	}
	for _, rt := range []int32{RetTypeDisconnect, RetTypeInvalid} {
		if !(&Error{RetType: rt}).Session() {
			t.Fatalf("retType %d is a session error", rt)
		}
	}
	for _, rt := range []int32{RetTypeSucceed, RetTypeUnknown} {
		if (&Error{RetType: rt}).Session() {
			t.Fatalf("retType %d is not a session error", rt)
		}
	}
	for _, err := range []error{ErrFrame, ErrPaced, ErrServerVersion, ErrTradingDisabled} {
		if err == nil || err.Error() == "" {
			t.Fatal("sentinel without text")
		}
	}
}
