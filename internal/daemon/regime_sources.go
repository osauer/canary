package daemon

import (
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type regimeSeriesPoint struct {
	Date  time.Time
	Value float64
}

var regimeHTTPClient = &http.Client{Timeout: 10 * time.Second}

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
	req.Header.Set("User-Agent", "Go-http-client/1.1")
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
	now := time.Now().UTC()
	// The previous AND current month merge into one series. The funding
	// row's five-publication replay walks the sparse CP leg across month
	// starts (CP prints carry ND gaps), and a current-month-only bill file
	// left the join without an at-or-before print for the first days of
	// every month — the v2 rise read nil exactly then.
	months := []string{now.AddDate(0, -1, 0).Format("200601"), now.Format("200601")}
	merged := []regimeSeriesPoint{}
	var lastErr error
	for _, month := range months {
		points, err := fetchTreasury13WeekBillMonth(ctx, month)
		if err != nil {
			lastErr = err
			continue
		}
		merged = append(merged, points...)
	}
	if len(merged) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("treasury 13-week bill XML contained no usable observations")
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Date.Before(merged[j].Date) })
	merged = slices.CompactFunc(merged, func(a, b regimeSeriesPoint) bool { return a.Date.Equal(b.Date) })
	return merged, nil
}

func fetchTreasury13WeekBillMonth(ctx context.Context, month string) ([]regimeSeriesPoint, error) {
	endpoint := treasuryBillRatesXMLURL(month)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	resp, err := regimeHTTPClient.Do(req)
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
		return nil, fmt.Errorf("treasury XML contained no usable 13-week bill observations")
	}
	return out, nil
}

func fetchCSVSeries(ctx context.Context, endpoint, valueColumn, dateLayout string) ([]regimeSeriesPoint, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	// FRED's Akamai edge has been observed resetting streams for a custom
	// product UA while accepting Go/curl defaults. Use Go's conventional UA
	// so official daily rows do not flap unavailable solely from edge policy.
	req.Header.Set("User-Agent", "Go-http-client/1.1")
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
