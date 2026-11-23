package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

func addSearchTestServer(t *testing.T, samples []broker.Listing, err error) *Server {
	t.Helper()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.addSearchForTest = func(context.Context, string) ([]broker.Listing, error) { return samples, err }
	return srv
}

func addSearch(t *testing.T, srv *Server, pattern string) (*rpc.AddSearchResult, error) {
	t.Helper()
	raw, _ := json.Marshal(rpc.AddSearchParams{Pattern: pattern})
	return srv.handleAddSearch(t.Context(), &rpc.Request{Params: raw})
}

// A search keeps stock listings, exact symbol matches first and otherwise in
// the broker's order, and says where Add asks about each and which calendar
// it keeps.
func TestAddSearchListsStocksWithRouteAndMarket(t *testing.T) {
	samples := []broker.Listing{
		{ID: strconv.Itoa(880010), Symbol: "SYNAX", SecType: "STK", Primary: "NYSE", Currency: "USD", Name: "SYNAX CORP"},
		{ID: strconv.Itoa(880011), Symbol: "9988", SecType: "STK", Primary: "SEHK", Currency: "HKD", Name: "SYNTHETIC GROUP HOLDINGS"},
		{ID: strconv.Itoa(880012), Symbol: "9988", SecType: "IND", Primary: "SEHK", Currency: "HKD", Name: ""},
		{ID: strconv.Itoa(880013), Symbol: "SYN9", SecType: "STK", Primary: "IBIS", Currency: "EUR", Name: "SYN NINE AG"},
	}
	res, err := addSearch(t, addSearchTestServer(t, samples, nil), "9988")
	if err != nil {
		t.Fatal(err)
	}
	if res.Query != "9988" || res.Truncated || len(res.Listings) != 3 {
		t.Fatalf("result = %+v", res)
	}
	want := []rpc.AddSearchListing{
		{Symbol: "9988", ConID: 880011, Name: "SYNTHETIC GROUP HOLDINGS", SecType: "STK", Currency: "HKD", PrimaryExchange: "SEHK", Exchange: "SEHK", Market: "hk_hkex"},
		{Symbol: "SYNAX", ConID: 880010, Name: "SYNAX CORP", SecType: "STK", Currency: "USD", PrimaryExchange: "NYSE", Exchange: "SMART", Market: "us_equity"},
		{Symbol: "SYN9", ConID: 880013, Name: "SYN NINE AG", SecType: "STK", Currency: "EUR", PrimaryExchange: "IBIS", Exchange: "SMART", Market: "de_xetra"},
	}
	for i := range want {
		if res.Listings[i] != want[i] {
			t.Errorf("listing %d = %+v, want %+v", i, res.Listings[i], want[i])
		}
	}
	raw, _ := json.Marshal(res)
	for _, key := range []string{`"as_of"`, `"query"`, `"listings"`, `"truncated"`, `"con_id"`, `"primary_exchange"`, `"market"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("JSON lacks %s: %s", key, raw)
		}
	}
}

func TestAddSearchLimitsAndRefuses(t *testing.T) {
	var many []broker.Listing
	for i := range 15 {
		many = append(many, broker.Listing{ID: strconv.Itoa(881000 + i), Symbol: fmt.Sprintf("SYN%d", i), SecType: "STK", Primary: "NASDAQ", Currency: "USD", Name: ""})
	}
	res, err := addSearch(t, addSearchTestServer(t, many, nil), "SYN")
	if err != nil || len(res.Listings) != rpc.AddSearchLimit || !res.Truncated {
		t.Fatalf("limit: %+v, %v", res, err)
	}
	if res, err := addSearch(t, addSearchTestServer(t, nil, nil), "SYNQ"); err != nil || res.Listings == nil || len(res.Listings) != 0 {
		t.Fatalf("no matches: %+v, %v", res, err)
	}
	for _, bad := range []string{"", "9988;", strings.Repeat("A", 33), "SYN/A"} {
		if _, err := addSearch(t, addSearchTestServer(t, nil, nil), bad); func() bool { code, _ := classifyError(err); return code != rpc.CodeBadRequest }() {
			t.Errorf("pattern %q: %v", bad, err)
		}
	}
	_, err = addSearch(t, addSearchTestServer(t, nil, ibkr.ErrSymbolSearchBusy), "SYNA")
	if code, msg := classifyError(err); code != rpc.CodeTimeout || !strings.Contains(msg, "try again in a moment") {
		t.Fatalf("busy search = %s %q", code, msg)
	}
}
