package daemon

import (
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/publichttp"
)

type regimeSeriesPoint struct {
	Date  time.Time
	Value float64
}

var regimeHTTPClient = &http.Client{Timeout: 10 * time.Second}

// Treasury's monthly XML can take about 20 seconds to respond even when its
// published data is current. Keep that bounded allowance specific to this feed;
// the caller's deadline and the overall Regime refresh deadline still apply.
var regimeTreasuryHTTPClient = &http.Client{Timeout: 25 * time.Second}

func fetchFREDSeries(ctx context.Context, seriesID string) ([]regimeSeriesPoint, error) {
	u := "https://fred.stlouisfed.org/graph/fredgraph.csv?id=" + url.QueryEscape(seriesID)
	return fetchCSVSeries(ctx, u, seriesID, "2006-01-02")
}

func fetchOfficialRegimeSeries(ctx context.Context, seriesID string) ([]regimeSeriesPoint, error) {
	switch seriesID {
	case fredSeriesCP3M:
		return fetchFedCommercialPaper90AAFinancial(ctx)
	case fredSeriesTBill3M:
		return fetchTreasury13WeekBill(ctx)
	default:
		return fetchFREDSeries(ctx, seriesID)
	}
}

func fetchCBOEVVIXSeries(ctx context.Context) ([]regimeSeriesPoint, error) {
	const u = "https://cdn.cboe.com/api/global/us_indices/daily_prices/VVIX_History.csv"
	return fetchCSVSeries(ctx, u, "VVIX", "01/02/2006")
}

// Cboe publishes VIX3M's official daily close at the same path as VVIX's, as
// DATE,OPEN,HIGH,LOW,CLOSE. It is the daemon's second, broker-independent VIX3M
// observation: unlike a gateway quote the value carries a real session date.
var cboeVIX3MHistoryURL = "https://cdn.cboe.com/api/global/us_indices/daily_prices/VIX3M_History.csv"

func fetchCBOEVIX3MSeries(ctx context.Context) ([]regimeSeriesPoint, error) {
	return fetchCSVSeries(ctx, cboeVIX3MHistoryURL, "CLOSE", "01/02/2006")
}

var fedCommercialPaperRatesURL = "https://www.federalreserve.gov/datadownload/Output.aspx?rel=CP&series=593ce926936cbd64b3c79b960a792b85&lastobs=270&from=&to=&filetype=csv&label=include&layout=seriescolumn&type=package"

var treasuryBillRatesXMLURL = func(month string) string {
	return "https://home.treasury.gov/resource-center/data-chart-center/interest-rates/pages/xml?data=daily_treasury_bill_rates&field_tdr_date_value_month=" + url.QueryEscape(month)
}

func fetchFedCommercialPaper90AAFinancial(ctx context.Context) ([]regimeSeriesPoint, error) {
	return fetchFedDDPSeries(ctx, fedCommercialPaperRatesURL, "RIFSPPFAAD90_N.B")
}

func fetchFedDDPSeries(ctx context.Context, endpoint, valueColumn string) ([]regimeSeriesPoint, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	publichttp.SetUserAgent(req)
	resp, err := regimeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %s", endpoint, resp.Status)
	}
	points, err := parseFedDDPSeriesCSV(resp.Body, valueColumn)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", valueColumn, err)
	}
	return points, nil
}

func parseFedDDPSeriesCSV(r io.Reader, valueColumn string) ([]regimeSeriesPoint, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	dateIdx := -1
	valueIdx := -1
	var out []regimeSeriesPoint
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read CSV row: %w", err)
		}
		for i, h := range rec {
			h = strings.TrimPrefix(strings.TrimSpace(h), "\ufeff")
			switch {
			case strings.EqualFold(h, "Time Period"):
				dateIdx = i
			case strings.EqualFold(h, valueColumn):
				valueIdx = i
			}
		}
		if dateIdx < 0 || valueIdx < 0 {
			continue
		}
		if len(rec) <= dateIdx || len(rec) <= valueIdx {
			continue
		}
		rawDate := strings.TrimSpace(rec[dateIdx])
		rawValue := strings.TrimSpace(rec[valueIdx])
		if rawDate == "" || rawValue == "" || rawValue == "." || strings.EqualFold(rawValue, "ND") || strings.EqualFold(rawValue, "NA") || strings.EqualFold(rawValue, "N/A") {
			continue
		}
		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			continue
		}
		date, err := time.Parse("2006-01-02", rawDate)
		if err != nil {
			continue
		}
		out = append(out, regimeSeriesPoint{Date: date, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	if len(out) == 0 {
		return nil, fmt.Errorf("CSV contained no usable %s observations", valueColumn)
	}
	return out, nil
}

func fetchTreasury13WeekBill(ctx context.Context) ([]regimeSeriesPoint, error) {
	// The previous AND current month merge into one series. The funding
	// row's five-publication replay walks the sparse CP leg across month
	// starts (CP prints carry ND gaps), and a current-month-only bill file
	// left the join without an at-or-before print for the first days of
	// every month — the v2 rise read nil exactly then.
	//
	// The merge is all-or-error: a month that fails to fetch fails the whole
	// read. A shorter merged series is indistinguishable from a complete one
	// downstream, and the series cache would hold it as a fresh success for
	// its full TTL; failing instead lets the cache serve its last complete
	// entry. The months fetch concurrently so a slow response for one cannot
	// starve the other inside the caller's shared budget.
	months := treasuryBillMonths(time.Now())
	type monthResult struct {
		points []regimeSeriesPoint
		err    error
	}
	ch := make(chan monthResult, len(months))
	for _, month := range months {
		go func() {
			points, err := fetchTreasury13WeekBillMonth(ctx, month)
			if err != nil {
				err = fmt.Errorf("month %s: %w", month, err)
			}
			ch <- monthResult{points: points, err: err}
		}()
	}
	merged := []regimeSeriesPoint{}
	var errs []error
	for range months {
		res := <-ch
		if res.err != nil {
			errs = append(errs, res.err)
			continue
		}
		merged = append(merged, res.points...)
	}
	// Treasury publishes the current month's file before its first print
	// lands: on day one it is a well-formed feed with no observations, and
	// stays so until the first business day's ~16:00 ET publication. That is
	// not a truncated fetch — the series genuinely ends with the previous
	// month's last print — so an empty current month merges as empty as long
	// as the previous month fetched. (2026-10-01 logged the all-or-error
	// failure 146 times, once per regime read, serving the cached series it
	// could have refreshed.) A fetch failure, or an empty previous month,
	// still fails the whole read.
	if len(errs) == 1 && len(merged) > 0 && errors.Is(errs[0], errTreasuryMonthEmpty) && strings.Contains(errs[0].Error(), "month "+months[1]) {
		errs = nil
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Date.Before(merged[j].Date) })
	merged = slices.CompactFunc(merged, func(a, b regimeSeriesPoint) bool { return a.Date.Equal(b.Date) })
	return merged, nil
}

func treasuryBillMonths(now time.Time) [2]string {
	now = now.UTC()
	// Subtract from day one: March 31 minus one month normalizes to March 3,
	// which otherwise fetches March twice and silently loses February.
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return [2]string{start.AddDate(0, -1, 0).Format("200601"), start.Format("200601")}
}

func fetchTreasury13WeekBillMonth(ctx context.Context, month string) ([]regimeSeriesPoint, error) {
	endpoint := treasuryBillRatesXMLURL(month)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	publichttp.SetUserAgent(req)
	resp, err := regimeTreasuryHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %s", endpoint, resp.Status)
	}
	points, err := parseTreasury13WeekBillXML(resp.Body)
	if err != nil {
		return nil, err
	}
	return points, nil
}

type treasuryBillFeed struct {
	Entries []treasuryBillEntry `xml:"entry"`
}

type treasuryBillEntry struct {
	Content treasuryBillContent `xml:"content"`
}

type treasuryBillContent struct {
	Properties treasuryBillProperties `xml:"properties"`
}

type treasuryBillProperties struct {
	IndexDate   string `xml:"INDEX_DATE"`
	Bank13Weeks string `xml:"ROUND_B1_CLOSE_13WK_2"`
}

func parseTreasury13WeekBillXML(r io.Reader) ([]regimeSeriesPoint, error) {
	var feed treasuryBillFeed
	if err := xml.NewDecoder(r).Decode(&feed); err != nil {
		return nil, fmt.Errorf("decode XML: %w", err)
	}
	var out []regimeSeriesPoint
	for _, entry := range feed.Entries {
		rawDate := strings.TrimSpace(entry.Content.Properties.IndexDate)
		rawValue := strings.TrimSpace(entry.Content.Properties.Bank13Weeks)
		if rawDate == "" || rawValue == "" || strings.EqualFold(rawValue, "N/A") {
			continue
		}
		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			continue
		}
		date, err := time.Parse("2006-01-02T15:04:05", rawDate)
		if err != nil {
			continue
		}
		out = append(out, regimeSeriesPoint{Date: date, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	if len(out) == 0 {
		return nil, errTreasuryMonthEmpty
	}
	return out, nil
}

// errTreasuryMonthEmpty is a parsed month file that carries no 13-week bill
// print yet. fetchTreasury13WeekBill tolerates it for the current month only.
var errTreasuryMonthEmpty = errors.New("treasury XML contained no usable 13-week bill observations")

func fetchCSVSeries(ctx context.Context, endpoint, valueColumn, dateLayout string) ([]regimeSeriesPoint, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	publichttp.SetUserAgent(req)
	resp, err := regimeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %s", endpoint, resp.Status)
	}

	reader := csv.NewReader(resp.Body)
	// Ragged rows are skipped per row below, as bad values and dates already
	// are — without this the default field count locks to the header and one
	// short line from a public file drops the entire series instead.
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read CSV header: %w", err)
	}
	valueIdx := -1
	dateIdx := -1
	for i, h := range header {
		h = strings.TrimPrefix(strings.TrimSpace(h), "\ufeff")
		switch {
		case strings.EqualFold(h, "observation_date") || strings.EqualFold(h, "DATE"):
			dateIdx = i
		case strings.EqualFold(h, valueColumn):
			valueIdx = i
		}
	}
	if dateIdx < 0 || valueIdx < 0 {
		return nil, fmt.Errorf("CSV missing date or %s column", valueColumn)
	}

	var out []regimeSeriesPoint
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read CSV row: %w", err)
		}
		if len(rec) <= dateIdx || len(rec) <= valueIdx {
			continue
		}
		rawValue := strings.TrimSpace(rec[valueIdx])
		if rawValue == "" || rawValue == "." {
			continue
		}
		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			continue
		}
		date, err := time.Parse(dateLayout, strings.TrimSpace(rec[dateIdx]))
		if err != nil {
			continue
		}
		out = append(out, regimeSeriesPoint{Date: date, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	if len(out) == 0 {
		return nil, fmt.Errorf("%s CSV contained no usable observations", valueColumn)
	}
	return out, nil
}

func latestSeriesPoint(points []regimeSeriesPoint) (regimeSeriesPoint, bool) {
	if len(points) == 0 {
		return regimeSeriesPoint{}, false
	}
	return points[len(points)-1], true
}

func laggedSeriesPoint(points []regimeSeriesPoint, observationsBack int) (regimeSeriesPoint, bool) {
	if observationsBack < 0 || len(points) <= observationsBack {
		return regimeSeriesPoint{}, false
	}
	return points[len(points)-1-observationsBack], true
}

// latestSeriesPointAtOrBefore finds the newest observation dated at or before
// the given day, within slackDays calendar days — the tolerance a two-legged
// join allows before the legs stop describing the same market moment.
func latestSeriesPointAtOrBefore(points []regimeSeriesPoint, day time.Time, slackDays int) (regimeSeriesPoint, bool) {
	cutoff := day.AddDate(0, 0, -slackDays)
	for _, point := range slices.Backward(points) {
		if point.Date.After(day) {
			continue
		}
		if point.Date.Before(cutoff) {
			return regimeSeriesPoint{}, false
		}
		return point, true
	}
	return regimeSeriesPoint{}, false
}

func seriesObservationAge(date time.Time, now time.Time) time.Duration {
	if date.IsZero() {
		return 0
	}
	return now.Sub(date)
}
