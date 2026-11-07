package ibkr

import (
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// moneyFields are the account summary's pointer-typed money fields, the
// ones whose nil means "not sent" and must never read as zero.
func moneyFields(t *testing.T, v reflect.Value) map[string]reflect.Value {
	t.Helper()
	out := map[string]reflect.Value{}
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Type == reflect.TypeFor[*float64]() {
			out[f.Name] = v.Field(i)
		}
	}
	if len(out) == 0 {
		t.Fatal("no money fields found")
	}
	return out
}

func TestSummaryKeepsNilMoneyNilAndCarriesProvenanceAndAsOf(t *testing.T) {
	asOf := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	nlv := 123.5
	raw := &ibkrlib.RawAccountSummary{
		AccountID: "DU_SYNTH", AccountType: "INDIVIDUAL", NetLiquidation: &nlv,
		Currency: "USD", BaseCurrency: "EUR", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag,
		CurrencyLedger:        map[string]ibkrlib.CurrencyLedger{"EUR": {CashBalance: 10, SettledCash: 9, SettledCashObserved: true, ExchangeRate: 1}},
		SettlementObservation: &ibkrlib.AccountSettlementObservation{AsOf: asOf, Callbacks: 1, Rows: []ibkrlib.AccountSettlementRow{{Currency: "EUR", Source: "account_total", Finite: true}}},
		AsOf:                  asOf, Raw: map[string]string{"NetLiquidation": "123.5"},
	}
	got := Summary(raw)
	if got == nil || got.AccountID != "DU_SYNTH" || got.AccountType != "INDIVIDUAL" || !got.AsOf.Equal(asOf) {
		t.Fatalf("identity or as_of lost: %+v", got)
	}
	if got.NetLiquidation == nil || *got.NetLiquidation != nlv {
		t.Fatalf("net liquidation lost: %v", got.NetLiquidation)
	}
	rawMoney, gotMoney := moneyFields(t, reflect.ValueOf(*raw)), moneyFields(t, reflect.ValueOf(*got))
	if len(rawMoney) != len(gotMoney) {
		t.Fatalf("money field sets differ: raw %d seam %d", len(rawMoney), len(gotMoney))
	}
	for name, rv := range rawMoney {
		gv, ok := gotMoney[name]
		if !ok {
			t.Fatalf("seam summary has no %s", name)
		}
		if rv.IsNil() != gv.IsNil() {
			t.Errorf("%s: nil-ness changed (raw nil %v, seam nil %v)", name, rv.IsNil(), gv.IsNil())
		}
	}
	if !got.BaseCurrencyProvenance.Proven() || got.BaseCurrency != "EUR" || got.Currency != "USD" {
		t.Fatalf("currency evidence lost: %+v", got)
	}
	if got.CurrencyLedger["EUR"] != (broker.CurrencyLedger{CashBalance: 10, SettledCash: 9, SettledCashObserved: true, ExchangeRate: 1}) {
		t.Fatalf("ledger row changed: %+v", got.CurrencyLedger["EUR"])
	}
	if got.Settlement == nil || got.Settlement.Callbacks != 1 || len(got.Settlement.Rows) != 1 || got.Settlement.Rows[0] != (broker.SettlementRow{Currency: "EUR", Source: "account_total", Finite: true}) {
		t.Fatalf("settlement observation changed: %+v", got.Settlement)
	}
	got.Settlement.Rows[0].Currency = "GBP"
	if raw.SettlementObservation.Rows[0].Currency != "EUR" {
		t.Fatal("settlement rows aliased")
	}
	if got.Raw["NetLiquidation"] != "123.5" {
		t.Fatal("raw keyed values lost")
	}
	if Summary(nil) != nil || Settlement(nil) != nil || Ledger(nil) != nil {
		t.Fatal("nil did not stay nil")
	}
	for raw, want := range map[ibkrlib.AccountSummaryProvenance]broker.Provenance{
		ibkrlib.AccountSummaryProvenanceRequest:        broker.ProvenanceRequest,
		ibkrlib.AccountSummaryProvenanceCachedFallback: broker.ProvenanceCachedFallback,
	} {
		if got := Provenance(raw); got != want {
			t.Errorf("provenance %q mapped to %q, want %q", raw, got, want)
		}
	}
}

func TestLedgerAndPositionsHealthMapFieldForField(t *testing.T) {
	for _, pair := range []struct {
		name         string
		raw, neutral reflect.Type
		seamOnly     int // trailing seam fields no IBKR row has
	}{
		{"currency ledger", reflect.TypeFor[ibkrlib.CurrencyLedger](), reflect.TypeFor[broker.CurrencyLedger](), 1},
		{"positions health", reflect.TypeFor[ibkrlib.PortfolioStreamHealth](), reflect.TypeFor[broker.PositionsHealth](), 0},
	} {
		if pair.raw.NumField()+pair.seamOnly != pair.neutral.NumField() {
			t.Fatalf("%s: %d raw fields, %d seam fields", pair.name, pair.raw.NumField(), pair.neutral.NumField())
		}
		for i := range pair.raw.NumField() {
			r, n := pair.raw.Field(i), pair.neutral.Field(i)
			if r.Name != n.Name || r.Type != n.Type {
				t.Errorf("%s: field %d is %s %v raw, %s %v seam", pair.name, i, r.Name, r.Type, n.Name, n.Type)
			}
		}
	}
	at := time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC)
	h := ibkrlib.PortfolioStreamHealth{Account: "DU_SYNTH", RequestedAt: at, InitialCompletedAt: at.Add(time.Second), LastUpdateAt: at.Add(2 * time.Second), ProjectionGeneration: 7, ScopeConflictAt: at.Add(3 * time.Second), InvalidPayloadAt: at.Add(4 * time.Second), DownloadShortAt: at.Add(5 * time.Second)}
	if back := StreamHealth(PositionsHealth(h)); back != h {
		t.Fatalf("positions health round trip changed: %+v", back)
	}
}

func TestPositionMapsTheRowAndItsContract(t *testing.T) {
	at := time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC)
	rows := []*ibkrlib.RawPosition{
		nil,
		{ValuationAt: at, Account: "DU_SYNTH", Contract: daemonContracts[0], Position: 10, MarketPrice: 2, MarketValue: 20, AverageCost: 1.5, UnrealizedPNL: 5, RealizedPNL: -1},
		{Account: "DU_SYNTH", Contract: daemonContracts[2], Position: -1, MarketValue: -300},
	}
	got := Positions(rows)
	if len(got) != 2 {
		t.Fatalf("%d rows mapped, want 2 (nil skipped)", len(got))
	}
	want := broker.Position{ValuationAt: at, Account: "DU_SYNTH", Instrument: Instrument(daemonContracts[0]), Quantity: 10, MarketPrice: 2, MarketValue: 20, AverageCost: 1.5, UnrealizedPnL: 5, RealizedPnL: -1}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("stock row:\n got %+v\nwant %+v", got[0], want)
	}
	if got[1].Instrument.SecType != "OPT" || got[1].Quantity != -1 || got[1].MarketValue != -300 || got[1].Instrument.Strike != 450 {
		t.Fatalf("option row: %+v", got[1])
	}
	if id, ok := got[0].Instrument.IDInt(); !ok || id != daemonContracts[0].ConID {
		t.Fatalf("con id lost: %v %v", id, ok)
	}
}

func TestNilAdapterRefusesAccountAndPositions(t *testing.T) {
	var a *Adapter
	if _, _, err := a.Summary(t.Context(), time.Second); err == nil {
		t.Fatal("nil adapter answered a summary")
	}
	if _, _, err := a.Positions(); err == nil {
		t.Fatal("nil adapter answered positions")
	}
	if _, ok := a.Concrete(broker.SessionBinding{Kind: broker.KindIBKR, Generation: 1, Epoch: 1}); ok {
		t.Fatal("nil adapter had a concrete binding")
	}
}
