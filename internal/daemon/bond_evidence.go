package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/osauer/canary/v2/internal/publichttp"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Bond issuer evidence (internal-docs/design/bond-orders.md). A bond buy the
// owner names by identifier is admitted only when one source names that exact
// identifier as a government or investment-grade bond with a fixed or zero
// coupon (owner decisions B1–B4, 2026-10-06): TreasuryDirect for US
// Treasuries, the ECB's list of eligible marketable assets for any bond on
// it, and OpenFIGI, checked against IBKR's own description of the line, for
// UK gilts and Government of Canada bonds. The evidence decides admission
// and supplies maturity and coupon; it never fills the contract id, grid or
// hours the broker owns.

// Evidence classes and sources as the draft records them.
const (
	bondEvidenceGovernment      = "government"
	bondEvidenceInvestmentGrade = "investment_grade"

	bondEvidenceTreasuryDirect = "treasurydirect"
	bondEvidenceECB            = "ecb_eligible_assets"
	bondEvidenceOpenFIGI       = "openfigi"
)

// bondEvidence is one source's word on one identifier.
type bondEvidence struct {
	Class    string
	Source   string
	Issuer   string
	Currency string
	Maturity time.Time
	// Coupon is the annual coupon in percent of face; zero for a bill.
	Coupon float64
	// AsOf is when the source's data was read (TreasuryDirect, OpenFIGI) or
	// published (the ECB's file date).
	AsOf time.Time
}

var (
	treasuryDirectSearchURL = "https://www.treasurydirect.gov/TA_WS/securities/search?format=json&cusip="
	// ecbEligibleAssetsURL takes the publication date as YYMMDD.
	ecbEligibleAssetsURL = "https://www.ecb.europa.eu/paym/coll/assets/html/dla/ea_MID/ea_csv_%s.csv.gz"
	openFIGIMappingURL   = "https://api.openfigi.com/v3/mapping"

	bondEvidenceHTTPClient = &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || req.URL.Scheme != "https" || len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("redirect to %s refused", req.URL.Redacted())
		}
		return nil
	}}
)

const (
	// A source's answer for one identifier serves a day; a failed read is
	// asked again after bondDetailsTransientRetry.
	bondEvidenceTTL = 24 * time.Hour
	// The ECB publishes its list each business day. A list read within
	// ecbListRefresh serves without a new read; one published more than
	// ecbListMaxAge ago (a long weekend, a missed day) no longer serves.
	ecbListRefresh   = 12 * time.Hour
	ecbListMaxAge    = 4 * 24 * time.Hour
	ecbListRetry     = 15 * time.Minute
	ecbListLookback  = 7
	ecbListMaxBody   = 8 << 20
	ecbListMaxPlain  = 64 << 20
	bondEvidenceBody = 1 << 20
)

// bondEvidenceSources caches each source's answers. The zero value is ready;
// the fetch fields replace the network in tests.
type bondEvidenceSources struct {
	mu       sync.Mutex
	treasury map[string]bondEvidenceCached[[]treasuryDirectSecurity]
	figi     map[string]bondEvidenceCached[[]openFIGIRecord]
	ecb      *ecbEligibleList
	ecbErr   error
	ecbTried time.Time
	ecbLoad  chan struct{}
	// factorPriced remembers every contract IBKR has flagged with notice
	// 2130, so a lookup the notice does not accompany still refuses it.
	factorPriced map[int]bool

	fetchTreasury func(context.Context, string) ([]treasuryDirectSecurity, error)
	fetchFIGI     func(context.Context, string) ([]openFIGIRecord, error)
	fetchECB      func(context.Context, time.Time) (*ecbEligibleList, error)
}

type bondEvidenceCached[T any] struct {
	value T
	err   error
	at    time.Time
}

func (c bondEvidenceCached[T]) fresh(now time.Time) bool {
	if c.at.IsZero() {
		return false
	}
	if c.err != nil {
		return now.Sub(c.at) < bondDetailsTransientRetry
	}
	return now.Sub(c.at) < bondEvidenceTTL
}

// noteFactorPriced remembers the factor-priced lines among lines.
func (src *bondEvidenceSources) noteFactorPriced(lines []ibkrlib.BondContractDetails) {
	if src == nil {
		return
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	for _, line := range lines {
		if line.FactorPriced && line.ConID > 0 {
			if src.factorPriced == nil {
				src.factorPriced = map[int]bool{}
			}
			src.factorPriced[line.ConID] = true
		}
	}
}

// factorPricedConID reports whether IBKR has ever flagged the contract.
func (src *bondEvidenceSources) factorPricedConID(conID int) bool {
	if src == nil {
		return false
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	return src.factorPriced[conID]
}

// bondEvidenceSupport returns the server's sources, built on first use.
func (s *Server) bondEvidenceSupport() *bondEvidenceSources {
	b := s.bondSupport()
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.evidence == nil {
		b.evidence = &bondEvidenceSources{}
	}
	return b.evidence
}

// bondBuyEvidence finds the evidence that admits a buy of line, or says why
// there is none, keyed by the identifier the owner named. A US Treasury
// CUSIP asks TreasuryDirect; any other line is looked up on the ECB's list; a
// UK or Canadian ISIN the list does not carry asks OpenFIGI and checks its
// answer against IBKR's description.
func (src *bondEvidenceSources) bondBuyEvidence(ctx context.Context, line ibkrlib.BondContractDetails, requested string, now time.Time) (bondEvidence, error) {
	requested = strings.ToUpper(strings.TrimSpace(requested))
	isin, cusip := "", ""
	switch {
	case ibkrlib.ValidISIN(requested):
		isin = requested
		if strings.HasPrefix(isin, "US") || strings.HasPrefix(isin, "CA") {
			cusip = isin[2:11]
		}
	case ibkrlib.ValidCUSIP(requested):
		// The line's ISIN may only extend a CUSIP request; the identity check
		// refuses a line whose US or Canadian ISIN names another CUSIP.
		cusip, isin = requested, line.ISIN()
	default:
		return bondEvidence{}, fmt.Errorf("%q is not a valid ISIN or CUSIP", requested)
	}
	if strings.HasPrefix(cusip, "912") {
		records, at, err := src.treasurySecurities(ctx, cusip, now)
		if err != nil {
			return bondEvidence{}, fmt.Errorf("TreasuryDirect could not be read for %s: %v", cusip, err)
		}
		return treasuryBondEvidence(cusip, records, at)
	}
	if isin == "" {
		return bondEvidence{}, fmt.Errorf("IBKR names no ISIN for contract %d, so no source can vouch for it", line.ConID)
	}
	var ecbReason string
	list, err := src.ecbList(ctx, now)
	switch {
	case err != nil:
		ecbReason = "the ECB's list of eligible assets could not be read: " + err.Error()
	default:
		if asset, ok := list.Assets[isin]; ok {
			return ecbBondEvidence(asset, list.Published)
		}
		ecbReason = fmt.Sprintf("%s is not on the ECB's list of eligible assets of %s", isin, list.Published.Format(time.DateOnly))
	}
	if strings.HasPrefix(isin, "GB") || strings.HasPrefix(isin, "CA") {
		records, at, err := src.figiRecords(ctx, isin, now)
		if err != nil {
			return bondEvidence{}, fmt.Errorf("%s, and OpenFIGI could not be read: %v", ecbReason, err)
		}
		return openFIGIBondEvidence(isin, records, line, at)
	}
	return bondEvidence{}, fmt.Errorf("%s; only US Treasuries, UK and Canadian government bonds and ECB-listed bonds can be bought", ecbReason)
}

// ---- TreasuryDirect -------------------------------------------------------

// treasuryDirectSecurity is one auction record of a Treasury security.
type treasuryDirectSecurity struct {
	CUSIP          string `json:"cusip"`
	SecurityType   string `json:"securityType"`
	Type           string `json:"type"`
	MaturityDate   string `json:"maturityDate"`
	InterestRate   string `json:"interestRate"`
	TIPS           string `json:"tips"`
	FloatingRate   string `json:"floatingRate"`
	InflationIndex string `json:"inflationIndexSecurity"`
}

// treasurySecurities returns TreasuryDirect's records of cusip and when they
// were read.
func (src *bondEvidenceSources) treasurySecurities(ctx context.Context, cusip string, now time.Time) ([]treasuryDirectSecurity, time.Time, error) {
	src.mu.Lock()
	if c, ok := src.treasury[cusip]; ok && c.fresh(now) {
		src.mu.Unlock()
		return c.value, c.at, c.err
	}
	fetch := src.fetchTreasury
	src.mu.Unlock()
	if fetch == nil {
		fetch = fetchTreasuryDirectSecurity
	}
	records, err := fetch(ctx, cusip)
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.treasury == nil {
		src.treasury = map[string]bondEvidenceCached[[]treasuryDirectSecurity]{}
	}
	src.treasury[cusip] = bondEvidenceCached[[]treasuryDirectSecurity]{value: records, err: err, at: now}
	return records, now, err
}

func fetchTreasuryDirectSecurity(ctx context.Context, cusip string) ([]treasuryDirectSecurity, error) {
	body, err := bondEvidenceGet(ctx, treasuryDirectSearchURL+cusip, bondEvidenceBody)
	if err != nil {
		return nil, err
	}
	var records []treasuryDirectSecurity
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("decode TreasuryDirect search: %w", err)
	}
	return records, nil
}

// treasuryBondEvidence admits a nominal Treasury bill, note or bond:
// TreasuryDirect's records of the CUSIP (one per auction, reopenings
// included) must agree on its maturity and coupon, and none may be a TIPS or
// a floating-rate note.
func treasuryBondEvidence(cusip string, records []treasuryDirectSecurity, asOf time.Time) (bondEvidence, error) {
	if len(records) == 0 {
		return bondEvidence{}, fmt.Errorf("TreasuryDirect lists no Treasury security with CUSIP %s", cusip)
	}
	ev := bondEvidence{Class: bondEvidenceGovernment, Source: bondEvidenceTreasuryDirect, Issuer: "United States Treasury", Currency: "USD", AsOf: asOf.UTC()}
	for i, r := range records {
		if !strings.EqualFold(strings.TrimSpace(r.CUSIP), cusip) {
			return bondEvidence{}, fmt.Errorf("TreasuryDirect answered %s with a record for %q", cusip, r.CUSIP)
		}
		kind := strings.TrimSpace(r.SecurityType)
		switch {
		case yes(r.TIPS) || yes(r.InflationIndex) || strings.EqualFold(strings.TrimSpace(r.Type), "TIPS"):
			return bondEvidence{}, fmt.Errorf("%s is an inflation-protected security (TIPS); Canary buys only nominal Treasuries", cusip)
		case yes(r.FloatingRate) || strings.EqualFold(strings.TrimSpace(r.Type), "FRN"):
			return bondEvidence{}, fmt.Errorf("%s is a floating-rate note; Canary buys only fixed or zero coupons", cusip)
		case !strings.EqualFold(kind, "Bill") && !strings.EqualFold(kind, "Note") && !strings.EqualFold(kind, "Bond"):
			return bondEvidence{}, fmt.Errorf("TreasuryDirect lists %s as %q, not a bill, note or bond", cusip, kind)
		}
		maturity, ok := treasuryDirectDate(r.MaturityDate)
		if !ok {
			return bondEvidence{}, fmt.Errorf("TreasuryDirect names no maturity for %s", cusip)
		}
		coupon := 0.0
		if raw := strings.TrimSpace(r.InterestRate); raw == "" && !strings.EqualFold(kind, "Bill") {
			return bondEvidence{}, fmt.Errorf("TreasuryDirect names no coupon for %s, a %s not yet auctioned", cusip, strings.ToLower(kind))
		} else if raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil || v < 0 || v > 100 {
				return bondEvidence{}, fmt.Errorf("TreasuryDirect's coupon %q for %s does not read", raw, cusip)
			}
			coupon = v
		}
		if i > 0 && (!maturity.Equal(ev.Maturity) || coupon != ev.Coupon) {
			return bondEvidence{}, fmt.Errorf("TreasuryDirect's records for %s disagree on maturity or coupon", cusip)
		}
		ev.Maturity, ev.Coupon = maturity, coupon
	}
	return ev, nil
}

func yes(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "yes") }

// ---- ECB eligible marketable assets ---------------------------------------

// ecbEligibleList is one day's list, by ISIN.
type ecbEligibleList struct {
	Published time.Time
	FetchedAt time.Time
	Assets    map[string]ecbEligibleAsset
}

// ecbEligibleAsset keeps the columns admission reads.
type ecbEligibleAsset struct {
	ISIN             string
	Type             string
	Denomination     string
	CouponDefinition string
	IssuerName       string
	IssuerGroup      string
	Maturity         time.Time
	Coupon           float64
	// Conflicting is set when the file lists the ISIN twice with different
	// terms; such an asset is never admitted.
	Conflicting bool
}

// ECB code lists (Eligible Assets Dictionary, read 2026-10-06).
var (
	ecbAdmittedTypes = map[string]string{
		"AT01": "bond", "AT02": "medium-term note", "AT03": "bill",
		"AT10": "EEA covered bond", "AT13": "non-EEA G10 covered bond",
	}
	ecbRefusedTypes = map[string]string{
		"AT11": "an asset-backed security", "AT12": "a multi-cédula",
	}
	ecbAdmittedCoupons = map[string]bool{"CD1": true, "CD4": true} // zero, fixed
)

const ecbIssuerGroupCentralGovernment = "IG2"

// ecbBondEvidence admits an ECB-listed bond, bill, note or covered bond with
// a zero or fixed coupon. Eligibility requires at least credit quality step
// 3 (BBB- or better), which is the investment-grade evidence (B3); a central
// government issuer is a government bond.
func ecbBondEvidence(a ecbEligibleAsset, published time.Time) (bondEvidence, error) {
	if a.Conflicting {
		return bondEvidence{}, fmt.Errorf("the ECB lists %s twice with different terms", a.ISIN)
	}
	if what, ok := ecbRefusedTypes[a.Type]; ok {
		return bondEvidence{}, fmt.Errorf("the ECB lists %s as %s; Canary buys only plain bonds, notes, bills and covered bonds", a.ISIN, what)
	}
	if _, ok := ecbAdmittedTypes[a.Type]; !ok {
		return bondEvidence{}, fmt.Errorf("the ECB lists %s with asset type %q, which Canary does not buy", a.ISIN, a.Type)
	}
	if !ecbAdmittedCoupons[a.CouponDefinition] {
		return bondEvidence{}, fmt.Errorf("the ECB lists %s with a variable coupon; Canary buys only fixed or zero coupons", a.ISIN)
	}
	if a.Maturity.IsZero() {
		return bondEvidence{}, fmt.Errorf("the ECB names no maturity for %s", a.ISIN)
	}
	class := bondEvidenceInvestmentGrade
	if a.IssuerGroup == ecbIssuerGroupCentralGovernment {
		class = bondEvidenceGovernment
	}
	return bondEvidence{Class: class, Source: bondEvidenceECB, Issuer: strings.TrimPrefix(a.IssuerName, "Central government: "),
		Currency: a.Denomination, Maturity: a.Maturity, Coupon: a.Coupon, AsOf: published}, nil
}

// ecbList returns a list young enough to serve. A read runs detached from
// ctx, so a preview that stops waiting does not cancel it; the next preview
// finds its result.
func (src *bondEvidenceSources) ecbList(ctx context.Context, now time.Time) (*ecbEligibleList, error) {
	src.mu.Lock()
	list := src.ecb
	usable := list != nil && now.Sub(list.Published) <= ecbListMaxAge
	if usable && now.Sub(list.FetchedAt) < ecbListRefresh {
		src.mu.Unlock()
		return list, nil
	}
	if src.ecbLoad == nil && (src.ecbTried.IsZero() || now.Sub(src.ecbTried) >= ecbListRetry) {
		src.ecbTried = now
		done := make(chan struct{})
		src.ecbLoad = done
		fetch := src.fetchECB
		if fetch == nil {
			fetch = fetchECBEligibleAssets
		}
		go func() {
			readCtx, cancel := context.WithTimeout(context.Background(), 2*bondEvidenceHTTPClient.Timeout)
			defer cancel()
			got, err := fetch(readCtx, now)
			src.mu.Lock()
			defer src.mu.Unlock()
			if err == nil {
				got.FetchedAt = now
				src.ecb, src.ecbErr = got, nil
			} else {
				src.ecbErr = err
			}
			src.ecbLoad = nil
			close(done)
		}()
	}
	load := src.ecbLoad
	src.mu.Unlock()
	if usable {
		return list, nil
	}
	if load != nil {
		select {
		case <-load:
		case <-ctx.Done():
			return nil, errors.New("the list is still loading; preview again in a minute")
		}
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.ecb != nil && now.Sub(src.ecb.Published) <= ecbListMaxAge {
		return src.ecb, nil
	}
	if src.ecbErr != nil {
		return nil, src.ecbErr
	}
	return nil, fmt.Errorf("no list published within the last %d days was read", int(ecbListMaxAge.Hours()/24))
}

// fetchECBEligibleAssets reads the newest daily file, looking back a week
// past weekends and holidays (a day without a file answers 404).
func fetchECBEligibleAssets(ctx context.Context, now time.Time) (*ecbEligibleList, error) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		berlin = time.UTC
	}
	day := now.In(berlin)
	for range ecbListLookback {
		published := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		body, err := bondEvidenceGet(ctx, fmt.Sprintf(ecbEligibleAssetsURL, day.Format("060102")), ecbListMaxBody)
		if errors.Is(err, errBondEvidenceNotFound) {
			day = day.AddDate(0, 0, -1)
			continue
		}
		if err != nil {
			return nil, err
		}
		assets, err := parseECBEligibleAssets(body)
		if err != nil {
			return nil, err
		}
		return &ecbEligibleList{Published: published, Assets: assets}, nil
	}
	return nil, fmt.Errorf("the ECB published no list in the last %d days", ecbListLookback)
}

// parseECBEligibleAssets reads the gzipped, tab-separated file the ECB
// publishes in UTF-16 with a byte-order mark (UTF-8 is read as well). Columns
// are found by name; a row without a valid ISIN is skipped.
func parseECBEligibleAssets(gz []byte) (map[string]ecbEligibleAsset, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("the ECB file is not gzip: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, ecbListMaxPlain+1))
	if err != nil {
		return nil, fmt.Errorf("read the ECB file: %w", err)
	}
	if len(raw) > ecbListMaxPlain {
		return nil, fmt.Errorf("the ECB file exceeds %d bytes uncompressed", ecbListMaxPlain)
	}
	text := decodeUTF16OrUTF8(raw)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		return nil, errors.New("the ECB file holds no rows")
	}
	col := map[string]int{}
	for i, name := range strings.Split(lines[0], "\t") {
		col[strings.TrimSpace(name)] = i
	}
	need := []string{"ISIN_CODE", "TYPE", "DENOMINATION", "MATURITY_DATE", "COUPON_RATE (%)", "ISSUER_NAME", "ISSUER_GROUP", "COUPON_DEFINITION"}
	for _, name := range need {
		if _, ok := col[name]; !ok {
			return nil, fmt.Errorf("the ECB file has no %s column", name)
		}
	}
	field := func(cells []string, name string) string {
		if i := col[name]; i < len(cells) {
			return strings.TrimSpace(cells[i])
		}
		return ""
	}
	out := map[string]ecbEligibleAsset{}
	for _, line := range lines[1:] {
		cells := strings.Split(line, "\t")
		isin := strings.ToUpper(field(cells, "ISIN_CODE"))
		if !ibkrlib.ValidISIN(isin) {
			continue
		}
		a := ecbEligibleAsset{ISIN: isin, Type: field(cells, "TYPE"), Denomination: strings.ToUpper(field(cells, "DENOMINATION")),
			CouponDefinition: field(cells, "COUPON_DEFINITION"), IssuerName: sourceText(field(cells, "ISSUER_NAME")), IssuerGroup: field(cells, "ISSUER_GROUP")}
		if raw := field(cells, "MATURITY_DATE"); raw != "" {
			datePart, _, _ := strings.Cut(raw, " ")
			if t, err := time.Parse("02/01/2006", datePart); err == nil {
				a.Maturity = t
			}
		}
		if v, err := strconv.ParseFloat(field(cells, "COUPON_RATE (%)"), 64); err == nil && v >= 0 && v <= 100 {
			a.Coupon = v
		} else if a.CouponDefinition != "CD1" {
			// A fixed coupon that does not read cannot bound accrued interest.
			a.CouponDefinition = ""
		}
		if prior, ok := out[isin]; ok && prior != a {
			prior.Conflicting = true
			a = prior
		}
		out[isin] = a
	}
	if len(out) == 0 {
		return nil, errors.New("the ECB file holds no asset with a valid ISIN")
	}
	return out, nil
}

func decodeUTF16OrUTF8(raw []byte) string {
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		raw = raw[2:]
		units := make([]uint16, len(raw)/2)
		for i := range units {
			units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
		}
		return string(utf16.Decode(units))
	}
	return strings.TrimPrefix(string(raw), "\ufeff")
}

// ---- OpenFIGI ---------------------------------------------------------------

// openFIGIRecord is one listing OpenFIGI maps an ISIN to.
type openFIGIRecord struct {
	Name          string `json:"name"`
	Ticker        string `json:"ticker"`
	SecurityType  string `json:"securityType"`
	SecurityType2 string `json:"securityType2"`
	MarketSector  string `json:"marketSector"`
}

// figiRecords returns OpenFIGI's listings of isin and when they were read.
func (src *bondEvidenceSources) figiRecords(ctx context.Context, isin string, now time.Time) ([]openFIGIRecord, time.Time, error) {
	src.mu.Lock()
	if c, ok := src.figi[isin]; ok && c.fresh(now) {
		src.mu.Unlock()
		return c.value, c.at, c.err
	}
	fetch := src.fetchFIGI
	src.mu.Unlock()
	if fetch == nil {
		fetch = fetchOpenFIGI
	}
	records, err := fetch(ctx, isin)
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.figi == nil {
		src.figi = map[string]bondEvidenceCached[[]openFIGIRecord]{}
	}
	src.figi[isin] = bondEvidenceCached[[]openFIGIRecord]{value: records, err: err, at: now}
	return records, now, err
}

func fetchOpenFIGI(ctx context.Context, isin string) ([]openFIGIRecord, error) {
	payload, _ := json.Marshal([]map[string]string{{"idType": "ID_ISIN", "idValue": isin}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openFIGIMappingURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	publichttp.SetUserAgent(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := bondEvidenceHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	var answers []struct {
		Data    []openFIGIRecord `json:"data"`
		Error   string           `json:"error"`
		Warning string           `json:"warning"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, bondEvidenceBody)).Decode(&answers); err != nil {
		return nil, fmt.Errorf("decode OpenFIGI mapping: %w", err)
	}
	if len(answers) != 1 {
		return nil, fmt.Errorf("OpenFIGI answered %d results for one identifier", len(answers))
	}
	if e := strings.TrimSpace(answers[0].Error); e != "" && !strings.Contains(strings.ToLower(e), "no identifier found") {
		return nil, fmt.Errorf("OpenFIGI: %s", sourceText(e))
	}
	records := answers[0].Data
	for i := range records {
		r := &records[i]
		r.Name, r.Ticker, r.SecurityType, r.SecurityType2, r.MarketSector = sourceText(r.Name), sourceText(r.Ticker), sourceText(r.SecurityType), sourceText(r.SecurityType2), sourceText(r.MarketSector)
	}
	return records, nil
}

// sourceText keeps outside text printable and short before it reaches a
// decision message or a terminal: control and non-ASCII-printable runes
// become '?', and the text is cut at 80 runes.
func sourceText(raw string) string {
	out := []rune(strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, raw)))
	if len(out) > 80 {
		out = append(out[:80], '…')
	}
	return string(out)
}

// openFIGIGovernment names the OpenFIGI tickers admitted as UK or Canadian
// government debt, with their ISIN country, currency, Bloomberg security
// type and issuer (OpenFIGI reads of 2026-10-06).
var openFIGIGovernment = map[string]struct {
	country, currency, securityType, issuer string
}{
	"UKT":  {"GB", "GBP", "UK GILT STOCK", "United Kingdom"},
	"UKTB": {"GB", "GBP", "DOMESTIC", "United Kingdom"},
	"CAN":  {"CA", "CAD", "CANADIAN", "Government of Canada"},
	"CTB":  {"CA", "CAD", "CANADIAN", "Government of Canada"},
}

// openFIGIGovernmentLinked names the index-linked roots of the same issuers.
var openFIGIGovernmentLinked = map[string]bool{"UKTI": true, "CANRRB": true}

// openFIGIBondEvidence admits a UK gilt or bill, or a Government of Canada
// bond or bill: every OpenFIGI listing of the ISIN is in the government
// sector under one admitted ticker with one coupon and maturity, none is
// index-linked, and IBKR's description of the line names the same ticker,
// coupon and maturity (B4).
func openFIGIBondEvidence(isin string, records []openFIGIRecord, line ibkrlib.BondContractDetails, asOf time.Time) (bondEvidence, error) {
	if len(records) == 0 {
		return bondEvidence{}, fmt.Errorf("OpenFIGI does not know %s", isin)
	}
	var want bondTicker
	for i, r := range records {
		if strings.Contains(strings.ToUpper(r.Name+" "+r.Ticker), "I/L") {
			return bondEvidence{}, fmt.Errorf("OpenFIGI lists %s as index-linked (%s); Canary buys only nominal bonds", isin, r.Ticker)
		}
		t, err := parseBondTicker(r.Ticker)
		if err != nil {
			return bondEvidence{}, fmt.Errorf("OpenFIGI's ticker %q for %s does not read: %v", r.Ticker, isin, err)
		}
		if openFIGIGovernmentLinked[t.Root] {
			return bondEvidence{}, fmt.Errorf("OpenFIGI lists %s as index-linked (%s); Canary buys only nominal bonds", isin, r.Ticker)
		}
		gov, ok := openFIGIGovernment[t.Root]
		switch {
		case !strings.EqualFold(r.MarketSector, "Govt"):
			return bondEvidence{}, fmt.Errorf("OpenFIGI classes %s in the %q sector, not government", isin, r.MarketSector)
		case !ok:
			return bondEvidence{}, fmt.Errorf("OpenFIGI names %s as %s (%s), not a UK or Canadian government bond", isin, t.Root, r.Name)
		case !strings.HasPrefix(isin, gov.country):
			return bondEvidence{}, fmt.Errorf("OpenFIGI names %s as %s, but the ISIN is not from %s", isin, t.Root, gov.country)
		case !strings.EqualFold(r.SecurityType, gov.securityType):
			return bondEvidence{}, fmt.Errorf("OpenFIGI's security type %q for %s is not %q", r.SecurityType, isin, gov.securityType)
		case t.Root == "UKTB" && !strings.HasSuffix(strings.ToUpper(strings.TrimSpace(r.Name)), "GBP"):
			return bondEvidence{}, fmt.Errorf("OpenFIGI names %s as a UK Treasury bill not in GBP (%s)", isin, r.Name)
		case i > 0 && !t.same(want):
			return bondEvidence{}, fmt.Errorf("OpenFIGI's listings of %s disagree (%s, %s)", isin, want, t)
		}
		want = t
	}
	broker, err := ibkrDescriptionTicker(line)
	if err != nil {
		return bondEvidence{}, fmt.Errorf("IBKR's description of %s cannot be checked against OpenFIGI: %v", isin, err)
	}
	if !broker.same(want) {
		return bondEvidence{}, fmt.Errorf("IBKR describes %s as %s but OpenFIGI as %s; the sources disagree", isin, broker, want)
	}
	gov := openFIGIGovernment[want.Root]
	return bondEvidence{Class: bondEvidenceGovernment, Source: bondEvidenceOpenFIGI, Issuer: gov.issuer, Currency: gov.currency,
		Maturity: want.Maturity, Coupon: want.Coupon, AsOf: asOf.UTC()}, nil
}

// bondTicker is a Bloomberg-style bond ticker: issuer root, coupon and
// maturity, "UKT 0.375 10/22/26" or "UKT 0 3/8 10/22/26".
type bondTicker struct {
	Root     string
	Coupon   float64
	Maturity time.Time
}

func (t bondTicker) same(o bondTicker) bool {
	return t.Root == o.Root && t.Maturity.Equal(o.Maturity) && abs(t.Coupon-o.Coupon) < 1e-9
}

func (t bondTicker) String() string {
	return fmt.Sprintf("%s %s %s", t.Root, strconv.FormatFloat(t.Coupon, 'f', -1, 64), t.Maturity.Format(time.DateOnly))
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// parseBondTicker reads root, coupon (decimal, whole, "a b/c" or "b/c") and
// an MM/DD/YY maturity; words after the date are ignored. A two-digit year
// is in this century: every bond Canary can buy matures after 2026.
func parseBondTicker(raw string) (bondTicker, error) {
	words := strings.Fields(strings.ToUpper(raw))
	if len(words) < 3 {
		return bondTicker{}, errors.New("no root, coupon and maturity")
	}
	dateAt := -1
	for i := 1; i < len(words); i++ {
		if len(words[i]) == 8 && words[i][2] == '/' && words[i][5] == '/' {
			dateAt = i
			break
		}
	}
	if dateAt < 2 || dateAt > 3 {
		return bondTicker{}, errors.New("no MM/DD/YY maturity after the coupon")
	}
	maturity, err := time.Parse("01/02/06", words[dateAt])
	if err != nil {
		return bondTicker{}, fmt.Errorf("maturity %q: %v", words[dateAt], err)
	}
	if maturity.Year() < 2000 {
		maturity = maturity.AddDate(100, 0, 0)
	}
	coupon := 0.0
	for _, part := range words[1:dateAt] {
		v, err := parseCouponPart(part)
		if err != nil {
			return bondTicker{}, err
		}
		coupon += v
	}
	if coupon < 0 || coupon > 100 {
		return bondTicker{}, fmt.Errorf("coupon %v is out of range", coupon)
	}
	return bondTicker{Root: words[0], Coupon: coupon, Maturity: maturity}, nil
}

func parseCouponPart(part string) (float64, error) {
	if num, den, ok := strings.Cut(part, "/"); ok {
		n, err1 := strconv.Atoi(num)
		d, err2 := strconv.Atoi(den)
		if err1 != nil || err2 != nil || d <= 0 || n < 0 || n >= d {
			return 0, fmt.Errorf("coupon fraction %q does not read", part)
		}
		return float64(n) / float64(d), nil
	}
	v, err := strconv.ParseFloat(part, 64)
	if err != nil {
		return 0, fmt.Errorf("coupon %q does not read", part)
	}
	return v, nil
}

// ibkrDescriptionTicker reads IBKR's description of the line (its
// description field, else its long name) as a ticker.
func ibkrDescriptionTicker(line ibkrlib.BondContractDetails) (bondTicker, error) {
	var firstErr error
	for _, raw := range []string{line.DescAppend, line.LongName} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		t, err := parseBondTicker(raw)
		if err == nil {
			return t, nil
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%q: %v", raw, err)
		}
	}
	if firstErr == nil {
		firstErr = errors.New("IBKR sent no description")
	}
	return bondTicker{}, firstErr
}

// ---- HTTP -------------------------------------------------------------------

var errBondEvidenceNotFound = errors.New("not found")

// bondEvidenceGet reads one public document, without credentials or
// redirects to another host, and bounds the body it keeps.
func bondEvidenceGet(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	publichttp.SetUserAgent(req)
	resp, err := bondEvidenceHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errBondEvidenceNotFound
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("the document exceeds %d bytes", limit)
	}
	return body, nil
}
