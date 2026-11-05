package rpc

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestInstrumentIDRoundTripsAndIsAbsentWhenUnknown(t *testing.T) {
	id := InstrumentID{Broker: InstrumentBrokerOpenD, ID: "HK.00700", Market: "hk", Venue: "SEHK", Currency: "HKD"}
	for name, row := range map[string]struct{ with, without any }{
		"contract": {&ContractParams{Symbol: "00700", Instrument: id}, &ContractParams{Symbol: "00700"}},
		"position": {&PositionView{Symbol: "00700", Instrument: id}, &PositionView{Symbol: "00700"}},
		"quote":    {&Quote{Symbol: "00700", Instrument: id}, &Quote{Symbol: "00700"}},
		"order":    {&OrderView{Symbol: "00700", Instrument: id}, &OrderView{Symbol: "00700"}},
		"watch":    {&WatchlistContract{Symbol: "00700", Instrument: id}, &WatchlistContract{Symbol: "00700"}},
	} {
		raw, err := json.Marshal(row.with)
		if err != nil || !strings.Contains(string(raw), `"instrument":{"broker":"opend","id":"HK.00700"`) {
			t.Fatalf("%s: %s %v", name, raw, err)
		}
		back := reflect.New(reflect.TypeOf(row.with).Elem()).Interface()
		if err := json.Unmarshal(raw, back); err != nil || !reflect.DeepEqual(back, row.with) {
			t.Fatalf("%s: %+v %v", name, back, err)
		}
		bare, err := json.Marshal(row.without)
		if err != nil || strings.Contains(string(bare), "instrument") {
			t.Fatalf("%s without an identity: %s %v", name, bare, err)
		}
		// An older reader's request or row, without the field, decodes to no identity.
		old := reflect.New(reflect.TypeOf(row.without).Elem()).Interface()
		if err := json.Unmarshal(bare, old); err != nil || !reflect.DeepEqual(old, row.without) {
			t.Fatalf("%s from an older peer: %+v %v", name, old, err)
		}
	}
}

// ContractParams keys two maps in the daemon and is compared with ==. Two
// requests naming one instrument decode separately and must still be one key,
// which a pointer field would break.
func TestContractParamsStaysOneMapKeyWithAnIdentity(t *testing.T) {
	raw := []byte(`{"symbol":"AAPL","instrument":{"broker":"ibkr","id":"265598","venue":"NASDAQ"}}`)
	var a, b ContractParams
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	if a != b || len(map[ContractParams]bool{a: true, b: true}) != 1 {
		t.Fatalf("one instrument made two keys: %+v %+v", a, b)
	}
}

func TestOneStockJoinsAcrossBrokers(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		ibkrSymbol, ibkrPrimary string
		opendSymbol, opendVenue string
		wantKey                 string
	}{
		{"Hong Kong drops leading zeros", "700", "SEHK", "00700", "SEHK", "SEHK:700"},
		{"a US name on the same exchange", "AAPL", "NASDAQ", "AAPL", "NASDAQ", "NASDAQ:AAPL"},
		{"IBKR's name for Nasdaq", "AAPL", "ISLAND", "AAPL", "NASDAQ", "NASDAQ:AAPL"},
		{"IBKR's name for Tokyo", "7203", "TSEJ", "7203", "TSE", "TSE:7203"},
	} {
		ibkr := NewInstrumentID(InstrumentBrokerIBKR, "1", ListingVenue(tc.ibkrPrimary, "SMART"), "")
		opend := NewInstrumentID(InstrumentBrokerOpenD, "x", tc.opendVenue, "")
		if got := ibkr.JoinKey(tc.ibkrSymbol); got != tc.wantKey || got != opend.JoinKey(tc.opendSymbol) {
			t.Errorf("%s: ibkr %q opend %q want %q", tc.name, got, opend.JoinKey(tc.opendSymbol), tc.wantKey)
		}
	}
	if (InstrumentID{Venue: "NYSE"}).JoinKey("AAPL") == (InstrumentID{Venue: "NASDAQ"}).JoinKey("AAPL") {
		t.Fatal("two listings joined")
	}
}

func TestAnIdentityAlwaysCarriesAListingVenue(t *testing.T) {
	if got := ListingVenue("", "SMART"); got != "" {
		t.Fatalf("a smart route is no listing: %q", got)
	}
	if got := ListingVenue("IBIS", "SMART"); got != "XETRA" {
		t.Fatalf("Xetra: %q", got)
	}
	if got := NewInstrumentID(InstrumentBrokerIBKR, "265598", "", "USD"); got != (InstrumentID{}) {
		t.Fatalf("no venue: %+v", got)
	}
	if got := NewInstrumentID(InstrumentBrokerOpenD, "", "SEHK", "HKD"); got != (InstrumentID{}) {
		t.Fatalf("no id: %+v", got)
	}
	if got := NewInstrumentID(InstrumentBrokerOpenD, "HK.00700", "SEHK", "HKD"); got.Market != "hk" {
		t.Fatalf("market: %+v", got)
	}
}

// A list read and written back must not store the identities it was given.
func TestWatchlistDropsAnIdentityOnTheWayIn(t *testing.T) {
	in := []WatchlistContract{{Symbol: "AAPL", Instrument: InstrumentID{Broker: InstrumentBrokerOpenD, ID: "US.AAPL", Venue: "NASDAQ"}}}
	got, err := NormalizeWatchlistSymbols(in)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := NormalizeWatchlistSymbols(got)
	if err != nil || got[0].Instrument != (InstrumentID{}) || !slices.Equal(got, stored) {
		t.Fatalf("%+v %+v %v", got, stored, err)
	}
}
