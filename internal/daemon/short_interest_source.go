package daemon

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const shortInterestIndexURL = "https://www.finra.org/finra-data/browse-catalog/equity-short-interest/files"
const shortInterestMaxBytes = 16 << 20
const shortInterestMaxRows = 30000

type shortInterestPublication struct {
	URL            string                 `json:"url"`
	SettlementDate string                 `json:"settlement_date"`
	FetchedAt      time.Time              `json:"fetched_at"`
	Skipped        int                    `json:"skipped"`
	Rows           []rpc.ShortInterestRow `json:"rows"`
}

var shortInterestLink = regexp.MustCompile(`https://cdn\.finra\.org/equity/otcmarket/biweekly/shrt([0-9]{8})\.csv`)

func shortInterestLatestURL(body []byte, now time.Time) (string, string, error) {
	latest, url := "", ""
	for _, match := range shortInterestLink.FindAllStringSubmatch(string(body), -1) {
		day, err := time.Parse("20060102", match[1])
		if err != nil || day.After(now) || now.Sub(day) > 60*24*time.Hour {
			continue
		}
		if match[1] > latest {
			latest, url = match[1], match[0]
		}
	}
	if url == "" {
		return "", "", errors.New("no recent FINRA publication link")
	}
	day, _ := time.Parse("20060102", latest)
	return url, day.Format(time.DateOnly), nil
}

func fetchShortInterest(ctx context.Context, now time.Time) (shortInterestPublication, error) {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 2 || req.URL.Scheme != "https" || (req.URL.Host != "www.finra.org" && req.URL.Host != "cdn.finra.org") {
			return errors.New("unexpected FINRA redirect")
		}
		return nil
	}}
	read := func(url string, limit int64) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Canary research (public FINRA short-interest files)")
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("FINRA HTTP %d", res.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(b)) > limit {
			return nil, errors.New("FINRA response exceeds bound")
		}
		return b, nil
	}
	index, err := read(shortInterestIndexURL, 2<<20)
	if err != nil {
		return shortInterestPublication{}, err
	}
	url, date, err := shortInterestLatestURL(index, now)
	if err != nil {
		return shortInterestPublication{}, err
	}
	body, err := read(url, shortInterestMaxBytes)
	if err != nil {
		return shortInterestPublication{}, err
	}
	rows, skipped, err := parseShortInterest(string(body), date)
	if err != nil {
		return shortInterestPublication{}, err
	}
	return shortInterestPublication{URL: url, SettlementDate: date, FetchedAt: now, Rows: rows, Skipped: skipped}, nil
}

func parseShortInterest(body, date string) ([]rpc.ShortInterestRow, int, error) {
	if len(body) > shortInterestMaxBytes {
		return nil, 0, errors.New("FINRA file exceeds bound")
	}
	reader := csv.NewReader(strings.NewReader(body))
	reader.Comma = '|'
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil {
		return nil, 0, err
	}
	fields := map[string]int{}
	for i, k := range header {
		if _, exists := fields[k]; exists {
			return nil, 0, errors.New("duplicate FINRA column")
		}
		fields[k] = i
	}
	for _, key := range []string{"symbolCode", "issueName", "marketClassCode", "currentShortPositionQuantity", "previousShortPositionQuantity", "averageDailyVolumeQuantity", "daysToCoverQuantity", "stockSplitFlag", "revisionFlag", "settlementDate"} {
		if _, ok := fields[key]; !ok {
			return nil, 0, fmt.Errorf("missing FINRA column %s", key)
		}
	}
	rows := []rpc.ShortInterestRow{}
	seen := map[string]bool{}
	skipped := 0
	for {
		record, e := reader.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, skipped, e
		}
		if len(rows)+skipped >= shortInterestMaxRows {
			return nil, skipped, errors.New("FINRA row bound exceeded")
		}
		get := func(k string) string { return strings.TrimSpace(record[fields[k]]) }
		symbol := get("symbolCode")
		normalized, e := rpc.NormalizeLendingRateSymbols([]string{symbol})
		if e != nil || len(normalized) != 1 || normalized[0] != symbol {
			skipped++
			continue
		}
		if seen[symbol] {
			return nil, skipped, errors.New("duplicate FINRA symbol")
		}
		seen[symbol] = true
		if get("settlementDate") != date {
			return nil, skipped, errors.New("mixed FINRA settlement dates")
		}
		integer := func(k string) (int64, error) {
			n, e := strconv.ParseInt(get(k), 10, 64)
			if e == nil && n < 0 {
				e = errors.New("negative FINRA quantity")
			}
			return n, e
		}
		current, e1 := integer("currentShortPositionQuantity")
		previous, e2 := integer("previousShortPositionQuantity")
		volume, e3 := integer("averageDailyVolumeQuantity")
		if e1 != nil || e2 != nil || e3 != nil {
			skipped++
			continue
		}
		market := get("marketClassCode")
		if !slices.Contains([]string{"OTC", "NNM", "NYSE", "ARCA", "SC", "BZX", "AMEX"}, market) {
			skipped++
			continue
		}
		split, revision := get("stockSplitFlag"), get("revisionFlag")
		if (split != "" && split != "S") || (revision != "" && revision != "R") {
			skipped++
			continue
		}
		row := rpc.ShortInterestRow{Symbol: symbol, Name: get("issueName"), Market: market, ShortInterestShares: current, PreviousShortInterestShares: previous, AverageDailyVolume: volume, SettlementDate: date, Split: split == "S", Revised: revision == "R"}
		if len(row.Name) > 512 {
			skipped++
			continue
		}
		if volume > 0 {
			days, e := strconv.ParseFloat(get("daysToCoverQuantity"), 64)
			// FINRA floors a positive ratio at 1.00. Preserve their published value.
			if e != nil || math.IsNaN(days) || math.IsInf(days, 0) || days < 1 {
				skipped++
				continue
			}
			row.DaysToCover = new(days)
		}
		if previous > 0 && !row.Split {
			row.ChangePct = new((float64(current)/float64(previous) - 1) * 100)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 || skipped > max(10, len(rows)/100) {
		return nil, skipped, errors.New("FINRA publication empty or excessively malformed")
	}
	return rows, skipped, nil
}
