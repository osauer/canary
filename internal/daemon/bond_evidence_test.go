package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// syntheticISIN completes prefix (11 characters) with its check digit.
func syntheticISIN(t *testing.T, prefix string) string {
	t.Helper()
	for d := range 10 {
		if id := prefix + strconv.Itoa(d); ibkrlib.ValidISIN(id) {
			return id
		}
	}
	t.Fatalf("no check digit completes %s", prefix)
	return ""
}

// syntheticCUSIP completes prefix (8 characters) with its check digit.
func syntheticCUSIP(t *testing.T, prefix string) string {
	t.Helper()
	for d := range 10 {
		if id := prefix + strconv.Itoa(d); ibkrlib.ValidCUSIP(id) {
			return id
		}
	}
	t.Fatalf("no check digit completes %s", prefix)
	return ""
}

var evidenceNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func TestTreasuryBondEvidenceAdmitsOnlyNominalTreasuries(t *testing.T) {
	cusip := syntheticCUSIP(t, "91282CZZ")
	note := treasuryDirectSecurity{CUSIP: cusip, SecurityType: "Note", Type: "Note", MaturityDate: "2033-09-30T00:00:00", InterestRate: "5.000000", TIPS: "No", FloatingRate: "No"}
	ev, err := treasuryBondEvidence(cusip, []treasuryDirectSecurity{note, note}, evidenceNow)
	if err != nil || ev.Class != bondEvidenceGovernment || ev.Source != bondEvidenceTreasuryDirect || ev.Currency != "USD" || ev.Coupon != 5 ||
		ev.Maturity.Format(time.DateOnly) != "2033-09-30" {
		t.Fatalf("note evidence = %+v, %v", ev, err)
	}
	bill := treasuryDirectSecurity{CUSIP: cusip, SecurityType: "Bill", Type: "Bill", MaturityDate: "2027-01-05T00:00:00"}
	if ev, err := treasuryBondEvidence(cusip, []treasuryDirectSecurity{bill}, evidenceNow); err != nil || ev.Coupon != 0 {
		t.Fatalf("bill evidence = %+v, %v", ev, err)
	}
	refused := map[string]treasuryDirectSecurity{
		"TIPS":      {CUSIP: cusip, SecurityType: "Note", Type: "TIPS", TIPS: "Yes", MaturityDate: "2031-01-15T00:00:00", InterestRate: "1.5"},
		"FRN":       {CUSIP: cusip, SecurityType: "Note", Type: "FRN", FloatingRate: "Yes", MaturityDate: "2028-01-31T00:00:00"},
		"type":      {CUSIP: cusip, SecurityType: "Strip", MaturityDate: "2030-01-31T00:00:00"},
		"undate":    {CUSIP: cusip, SecurityType: "Note", MaturityDate: ""},
		"no coupon": {CUSIP: cusip, SecurityType: "Note", Type: "Note", MaturityDate: "2033-09-30T00:00:00", InterestRate: ""},
	}
	for name, r := range refused {
		if _, err := treasuryBondEvidence(cusip, []treasuryDirectSecurity{r}, evidenceNow); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
	if _, err := treasuryBondEvidence(cusip, nil, evidenceNow); err == nil || !strings.Contains(err.Error(), "lists no Treasury security") {
		t.Fatalf("unknown CUSIP: %v", err)
	}
	reopened := note
	reopened.MaturityDate = "2034-09-30T00:00:00"
	if _, err := treasuryBondEvidence(cusip, []treasuryDirectSecurity{note, reopened}, evidenceNow); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("disagreeing records: %v", err)
	}
}

// ecbFile is a gzipped UTF-16LE tab-separated file in the ECB's layout.
func ecbFile(t *testing.T, rows ...[]string) []byte {
	t.Helper()
	header := []string{"ISIN_CODE", "OTHER_REG_NUMBER", "HAIRCUT_CATEGORY", "TYPE", "REFERENCE_MARKET", "DENOMINATION", "ISSUANCE_DATE", "MATURITY_DATE",
		"ISSUER_CSD", "COUPON_RATE (%)", "ISSUER_NAME", "ISSUER_RESIDENCE", "ISSUER_GROUP", "GUARANTOR_NAME", "GUARANTOR_RESIDENCE", "GUARANTOR_GROUP",
		"COUPON_DEFINITION", "HAIRCUT", "HAIRCUT_OWN_USE", "POTENTIALLY_OWN_USABLE_COVERED_BOND", "CLIMATE_FACTOR "}
	var text strings.Builder
	text.WriteString(strings.Join(header, "\t") + "\r\n")
	for _, r := range rows {
		text.WriteString(strings.Join(r, "\t") + "\r\n")
	}
	units := utf16.Encode([]rune(text.String()))
	raw := []byte{0xFF, 0xFE}
	for _, u := range units {
		raw = append(raw, byte(u), byte(u>>8))
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func ecbRow(isin, typ, ccy, maturity, coupon, issuer, group, couponDef string) []string {
	return []string{isin, "", "L1", typ, "RM", ccy, "01/01/2025 00:00:00", maturity + " 00:00:00", "CL", coupon, issuer, "IRXX", group, "", "", "", couponDef, "1", "", "N", "N"}
}

func TestECBEvidenceReadsTheListAndAdmitsPlainBonds(t *testing.T) {
	gov := syntheticISIN(t, "DE000SYN000")
	corp := syntheticISIN(t, "XS000SYN000")
	abs := syntheticISIN(t, "XS000SYN001")
	frn := syntheticISIN(t, "XS000SYN002")
	assets, err := parseECBEligibleAssets(ecbFile(t,
		ecbRow(gov, "AT01", "EUR", "15/02/2036", "2.5", "Central government: Synthetic Republic", "IG2", "CD4"),
		ecbRow(corp, "AT02", "EUR", "01/06/2031", "3.125", "SYNTHETIC CORP", "IG3", "CD4"),
		ecbRow(abs, "AT11", "EUR", "01/06/2040", "1", "SYNTH ABS", "IG9", "CD4"),
		ecbRow(frn, "AT01", "EUR", "01/06/2030", "0", "SYNTH BANK", "IG4", "CD2"),
		ecbRow("NOT-AN-ISIN", "AT01", "EUR", "01/06/2030", "1", "X", "IG3", "CD4"),
	))
	if err != nil || len(assets) != 4 {
		t.Fatalf("assets = %d, %v", len(assets), err)
	}
	published := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	ev, err := ecbBondEvidence(assets[gov], published)
	if err != nil || ev.Class != bondEvidenceGovernment || ev.Issuer != "Synthetic Republic" || ev.Currency != "EUR" || ev.Coupon != 2.5 || ev.Maturity.Format(time.DateOnly) != "2036-02-15" {
		t.Fatalf("government = %+v, %v", ev, err)
	}
	if ev, err := ecbBondEvidence(assets[corp], published); err != nil || ev.Class != bondEvidenceInvestmentGrade || ev.Source != bondEvidenceECB {
		t.Fatalf("corporate = %+v, %v", ev, err)
	}
	if _, err := ecbBondEvidence(assets[abs], published); err == nil || !strings.Contains(err.Error(), "asset-backed") {
		t.Fatalf("ABS: %v", err)
	}
	if _, err := ecbBondEvidence(assets[frn], published); err == nil || !strings.Contains(err.Error(), "variable coupon") {
		t.Fatalf("variable coupon: %v", err)
	}
	if _, err := parseECBEligibleAssets(ecbFile(t)); err == nil {
		t.Fatal("an empty file was read as a list")
	}
	twice, err := parseECBEligibleAssets(ecbFile(t,
		ecbRow(corp, "AT02", "EUR", "01/06/2031", "3.125", "SYNTHETIC CORP", "IG3", "CD4"),
		ecbRow(corp, "AT02", "EUR", "01/06/2031", "5.125", "SYNTHETIC CORP", "IG3", "CD4")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ecbBondEvidence(twice[corp], published); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("conflicting rows: %v", err)
	}
}

// The newest file is read past days the ECB published none (404).
func TestFetchECBEligibleAssetsLooksBackPastMissingDays(t *testing.T) {
	isin := syntheticISIN(t, "DE000SYN000")
	file := ecbFile(t, ecbRow(isin, "AT01", "EUR", "15/02/2036", "2.5", "Central government: Synthetic Republic", "IG2", "CD4"))
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if r.URL.Path != "/ea_csv_261002.csv.gz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(file)
	}))
	defer srv.Close()
	prev := ecbEligibleAssetsURL
	ecbEligibleAssetsURL = srv.URL + "/ea_csv_%s.csv.gz"
	defer func() { ecbEligibleAssetsURL = prev }()

	list, err := fetchECBEligibleAssets(context.Background(), time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC))
	if err != nil || list.Published.Format(time.DateOnly) != "2026-10-02" || len(list.Assets) != 1 || len(asked) != 3 {
		t.Fatalf("list = %+v, %v, asked %v", list, err, asked)
	}
}

func TestParseBondTicker(t *testing.T) {
	cases := map[string]string{
		"UKT 0.375 10/22/26":        "UKT 0.375 2026-10-22",
		"UKT 0 3/8 10/22/26":        "UKT 0.375 2026-10-22",
		"CAN 3 1/4 12/01/34":        "CAN 3.25 2034-12-01",
		"T 5 09/30/33":              "T 5 2033-09-30",
		"DBR 0 08/15/30 G":          "DBR 0 2030-08-15",
		"FRTR 3.25 05/25/45 OAT":    "FRTR 3.25 2045-05-25",
		"UKT 1 5/8 10/22/71":        "UKT 1.625 2071-10-22",
		"CTB 0 01/08/27":            "CTB 0 2027-01-08",
		"DBRI 0.1 04/15/46 I/L":     "DBRI 0.1 2046-04-15",
		"UNITED KINGDOM GILT":       "",
		"UKT 3/8":                   "",
		"UKT 0 3/9x 10/22/26":       "",
		"UKT 0 1 2 3/8 10/22/26 XX": "",
	}
	for raw, want := range cases {
		got, err := parseBondTicker(raw)
		if want == "" {
			if err == nil {
				t.Errorf("%q read as %s", raw, got)
			}
			continue
		}
		if err != nil || got.String() != want {
			t.Errorf("%q = %s, %v; want %s", raw, got, err, want)
		}
	}
}

func TestOpenFIGIEvidenceNeedsAgreementWithIBKR(t *testing.T) {
	gilt := syntheticISIN(t, "GB00SYNTH00")
	canada := syntheticISIN(t, "CA135SYN000")
	giltLine := ibkrlib.BondContractDetails{ConID: 1, DescAppend: "UKT 0 3/8 10/22/26"}
	giltFIGI := openFIGIRecord{Name: "UNITED KINGDOM GILT", Ticker: "UKT 0.375 10/22/26", SecurityType: "UK GILT STOCK", SecurityType2: "Govt", MarketSector: "Govt"}

	ev, err := openFIGIBondEvidence(gilt, []openFIGIRecord{giltFIGI, giltFIGI}, giltLine, evidenceNow)
	if err != nil || ev.Class != bondEvidenceGovernment || ev.Currency != "GBP" || ev.Coupon != 0.375 || ev.Maturity.Format(time.DateOnly) != "2026-10-22" || ev.Source != bondEvidenceOpenFIGI {
		t.Fatalf("gilt = %+v, %v", ev, err)
	}
	canadaFIGI := openFIGIRecord{Name: "CANADIAN GOVERNMENT", Ticker: "CAN 3.25 12/01/34", SecurityType: "CANADIAN", MarketSector: "Govt"}
	if ev, err := openFIGIBondEvidence(canada, []openFIGIRecord{canadaFIGI}, ibkrlib.BondContractDetails{LongName: "CAN 3 1/4 12/01/34"}, evidenceNow); err != nil || ev.Currency != "CAD" {
		t.Fatalf("canada = %+v, %v", ev, err)
	}

	refusals := []struct {
		name    string
		isin    string
		records []openFIGIRecord
		line    ibkrlib.BondContractDetails
		want    string
	}{
		{"index-linked", gilt, []openFIGIRecord{{Name: "TSY 0 1/8% 2031 I/L GILT", Ticker: "UKTI 0.125 08/10/31", SecurityType: "UK GILT STOCK", MarketSector: "Govt"}}, giltLine, "index-linked"},
		{"real return", canada, []openFIGIRecord{{Name: "CANADIAN GOVERNMENT RRB", Ticker: "CANRRB 1.5 12/01/44", SecurityType: "CANADIAN", MarketSector: "Govt"}}, giltLine, "index-linked"},
		{"corporate", gilt, []openFIGIRecord{{Name: "SYNTH PLC", Ticker: "SYNPLC 4 10/22/26", SecurityType: "EURO-DOLLAR", MarketSector: "Corp"}}, giltLine, "sector"},
		{"wrong country", canada, []openFIGIRecord{giltFIGI}, giltLine, "is not from GB"},
		{"brokers maturity differs", gilt, []openFIGIRecord{giltFIGI}, ibkrlib.BondContractDetails{DescAppend: "UKT 0 3/8 10/22/27"}, "disagree"},
		{"broker silent", gilt, []openFIGIRecord{giltFIGI}, ibkrlib.BondContractDetails{}, "cannot be checked"},
		{"unknown", gilt, nil, giltLine, "does not know"},
		{"listings disagree", gilt, []openFIGIRecord{giltFIGI, {Name: "UNITED KINGDOM GILT", Ticker: "UKT 0.5 10/22/26", SecurityType: "UK GILT STOCK", MarketSector: "Govt"}}, giltLine, "disagree"},
	}
	for _, c := range refusals {
		if _, err := openFIGIBondEvidence(c.isin, c.records, c.line, evidenceNow); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// Routing: a Treasury CUSIP asks TreasuryDirect only; an ECB-listed ISIN is
// decided by the list; a UK ISIN the list lacks asks OpenFIGI, even when the
// list cannot be read; any other ISIN off the list is refused.
func TestBondBuyEvidenceRoutesEachIdentifierToItsSource(t *testing.T) {
	cusip := syntheticCUSIP(t, "91282CZZ")
	listed := syntheticISIN(t, "XS000SYN000")
	gilt := syntheticISIN(t, "GB00SYNTH00")
	other := syntheticISIN(t, "US000SYN000")
	var tdCalls, figiCalls atomic.Int32
	src := &bondEvidenceSources{
		fetchTreasury: func(_ context.Context, c string) ([]treasuryDirectSecurity, error) {
			tdCalls.Add(1)
			return []treasuryDirectSecurity{{CUSIP: c, SecurityType: "Note", MaturityDate: "2033-09-30T00:00:00", InterestRate: "5"}}, nil
		},
		fetchFIGI: func(context.Context, string) ([]openFIGIRecord, error) {
			figiCalls.Add(1)
			return []openFIGIRecord{{Name: "UNITED KINGDOM GILT", Ticker: "UKT 4 10/22/31", SecurityType: "UK GILT STOCK", MarketSector: "Govt"}}, nil
		},
		fetchECB: func(context.Context, time.Time) (*ecbEligibleList, error) {
			return &ecbEligibleList{Published: evidenceNow.Truncate(24 * time.Hour), Assets: map[string]ecbEligibleAsset{
				listed: {ISIN: listed, Type: "AT01", Denomination: "EUR", CouponDefinition: "CD4", IssuerName: "SYNTH CORP", IssuerGroup: "IG3", Maturity: evidenceNow.AddDate(5, 0, 0), Coupon: 3},
			}}, nil
		},
	}
	ctx := context.Background()
	line := func(isin, cusip, desc string) ibkrlib.BondContractDetails {
		d := ibkrlib.BondContractDetails{ConID: 7, DescAppend: desc, Complete: true, SecIDs: map[string]string{}}
		if isin != "" {
			d.SecIDs["ISIN"] = isin
		}
		if cusip != "" {
			d.SecIDs["CUSIP"] = cusip
		}
		return d
	}
	if ev, err := src.bondBuyEvidence(ctx, line("", cusip, ""), cusip, evidenceNow); err != nil || ev.Source != bondEvidenceTreasuryDirect {
		t.Fatalf("treasury = %+v, %v", ev, err)
	}
	if ev, err := src.bondBuyEvidence(ctx, line(listed, "", ""), listed, evidenceNow); err != nil || ev.Source != bondEvidenceECB || ev.Class != bondEvidenceInvestmentGrade {
		t.Fatalf("listed = %+v, %v", ev, err)
	}
	if ev, err := src.bondBuyEvidence(ctx, line(gilt, "", "UKT 4 10/22/31"), gilt, evidenceNow); err != nil || ev.Source != bondEvidenceOpenFIGI {
		t.Fatalf("gilt = %+v, %v", ev, err)
	}
	if _, err := src.bondBuyEvidence(ctx, line(other, "", ""), other, evidenceNow); err == nil || !strings.Contains(err.Error(), "not on the ECB's list") {
		t.Fatalf("unlisted corporate: %v", err)
	}
	if tdCalls.Load() != 1 || figiCalls.Load() != 1 {
		t.Fatalf("TreasuryDirect asked %d times, OpenFIGI %d; want once each", tdCalls.Load(), figiCalls.Load())
	}
	// Evidence is keyed by the identifier the owner named: an XS ISIN whose
	// line also carries a Treasury CUSIP is not vouched for by TreasuryDirect.
	if _, err := src.bondBuyEvidence(ctx, line(other, cusip, ""), other, evidenceNow); err == nil || tdCalls.Load() != 1 {
		t.Fatalf("a non-Treasury ISIN was admitted on Treasury evidence: %v (TreasuryDirect asked %d times)", err, tdCalls.Load())
	}
	// Answers are kept: the same buys ask no source again.
	_, _ = src.bondBuyEvidence(ctx, line("", cusip, ""), cusip, evidenceNow.Add(time.Hour))
	_, _ = src.bondBuyEvidence(ctx, line(gilt, "", "UKT 4 10/22/31"), gilt, evidenceNow.Add(time.Hour))
	if tdCalls.Load() != 1 || figiCalls.Load() != 1 {
		t.Fatalf("cached answers were asked again: TreasuryDirect %d, OpenFIGI %d", tdCalls.Load(), figiCalls.Load())
	}

	down := &bondEvidenceSources{
		fetchFIGI: src.fetchFIGI,
		fetchECB:  func(context.Context, time.Time) (*ecbEligibleList, error) { return nil, errors.New("HTTP 503") },
	}
	if ev, err := down.bondBuyEvidence(ctx, line(gilt, "", "UKT 4 10/22/31"), gilt, evidenceNow); err != nil || ev.Source != bondEvidenceOpenFIGI {
		t.Fatalf("gilt with the ECB down = %+v, %v", ev, err)
	}
	if _, err := down.bondBuyEvidence(ctx, line(listed, "", ""), listed, evidenceNow); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("listed bond with the ECB down: %v", err)
	}
}

// A preview that stops waiting does not cancel the ECB read; the next one
// finds the list. A list published more than four days ago no longer serves.
func TestECBListLoadsDetachedAndExpires(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	published := evidenceNow.Truncate(24 * time.Hour)
	src := &bondEvidenceSources{fetchECB: func(context.Context, time.Time) (*ecbEligibleList, error) {
		calls.Add(1)
		<-release
		return &ecbEligibleList{Published: published, Assets: map[string]ecbEligibleAsset{}}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := src.ecbList(ctx, evidenceNow); err == nil || !strings.Contains(err.Error(), "still loading") {
		t.Fatalf("first wait: %v", err)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		list, err := src.ecbList(context.Background(), evidenceNow)
		if err == nil && list.Published.Equal(published) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("list never served: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("the list was read %d times, want once", calls.Load())
	}
	src.mu.Lock()
	src.fetchECB = func(context.Context, time.Time) (*ecbEligibleList, error) { return nil, fmt.Errorf("HTTP 503") }
	src.mu.Unlock()
	if _, err := src.ecbList(context.Background(), evidenceNow.Add(5*24*time.Hour)); err == nil {
		t.Fatal("a list published five days ago still served")
	}
}
