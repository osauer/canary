// Command refresh-fund-sectors rewrites internal/daemon/fund_sectors_data.go
// with SPY's GICS sector split. It downloads State Street's daily SPY
// holdings workbook, sums each holding's weight under the sector the
// embedded S&P 500 table (internal/breadth/spx) gives its ticker, and stamps
// the result with the workbook's own holdings date. The workbook's Sector
// column is empty, so the sector comes from the spx table.
//
// Invoked by `make refresh-fund-sectors`. QQQ and IWM stay hand-maintained in
// internal/daemon/portfolio_sector.go; see the comment there for why.
//
// The output is written only after every guard passes; a failed run exits
// non-zero and leaves the file untouched.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
)

const (
	holdingsURL = "https://www.ssga.com/us/en/intermediary/library-content/products/fund-data/etfs/us/holdings-daily-us-en-spy.xlsx"
	outputPath  = "internal/daemon/fund_sectors_data.go"
	source      = "State Street SPY daily holdings weighted by Wikipedia GICS sectors"

	// State Street serves the workbook to browser-like clients; Go's default
	// User-Agent has not been tried against it and need not be.
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
)

// Parse-breakage guards from issue #45, not owner policy: SPY holds about
// 500 S&P names and its non-cash weight sums to roughly 99.7-99.9, so fewer
// resolved tickers or a smaller resolved sum means the workbook layout or the
// spx sector table has drifted and the split would be quietly wrong.
const (
	minResolved  = 480
	minSectorSum = 99.0
)

type holding struct {
	Ticker string
	Weight float64
}

type split struct {
	Weights    map[string]float64 // raw summed weight per sector
	Resolved   int
	Sum        float64 // raw weight of resolved holdings
	Unresolved []holding
}

func main() {
	dry := flag.Bool("dry-run", false, "print the split without rewriting "+outputPath)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := fetch(ctx, holdingsURL)
	if err != nil {
		log.Fatalf("fetch %s: %v", holdingsURL, err)
	}
	holdings, asOf, err := parseWorkbook(data)
	if err != nil {
		log.Fatalf("parse SPY workbook: %v", err)
	}
	s := aggregate(holdings, spx.SectorOf)
	for _, h := range s.Unresolved {
		fmt.Fprintf(os.Stderr, "unresolved: %-10s %.6f\n", h.Ticker, h.Weight)
	}
	fmt.Fprintf(os.Stderr, "SPY as of %s: %d of %d holdings resolved, sector weights sum %.2f\n",
		asOf.Format(time.DateOnly), s.Resolved, len(holdings), s.Sum)
	if err := checkCoverage(s.Resolved, s.Sum); err != nil {
		log.Fatalf("%v; %s left untouched", err, outputPath)
	}
	src, err := render(asOf, s.Weights)
	if err != nil {
		log.Fatalf("render %s: %v", outputPath, err)
	}
	if *dry {
		os.Stdout.Write(src)
		return
	}
	if current, err := os.ReadFile(outputPath); err == nil && bytes.Equal(current, src) {
		fmt.Fprintf(os.Stderr, "%s unchanged\n", outputPath)
		return
	}
	if err := os.WriteFile(outputPath, src, 0o644); err != nil {
		log.Fatalf("write %s: %v", outputPath, err)
	}
	fmt.Fprintf(os.Stderr, "Wrote %d sectors to %s\n", len(s.Weights), outputPath)
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	// A bot wall answers 200 with HTML; say so instead of failing in unzip.
	if !bytes.HasPrefix(data, []byte("PK")) {
		return nil, fmt.Errorf("response is not an xlsx workbook (%d bytes, Content-Type %q)", len(data), resp.Header.Get("Content-Type"))
	}
	return data, nil
}

// parseWorkbook reads the holdings table and the "As of DD-Mon-YYYY" date
// from the first worksheet. The table starts at the row holding both a
// "Ticker" and a "Weight" header and ends at the first row without a ticker.
func parseWorkbook(data []byte) ([]holding, time.Time, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, time.Time{}, err
	}
	var strs []string
	if raw, err := readZipFile(zr, "xl/sharedStrings.xml"); err == nil {
		if strs, err = parseSharedStrings(raw); err != nil {
			return nil, time.Time{}, fmt.Errorf("sharedStrings: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, err
	}
	raw, err := readZipFile(zr, "xl/worksheets/sheet1.xml")
	if err != nil {
		return nil, time.Time{}, err
	}
	var ws struct {
		Rows []struct {
			Cells []struct {
				Ref    string `xml:"r,attr"`
				Type   string `xml:"t,attr"`
				Value  string `xml:"v"`
				Inline string `xml:"is>t"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(raw, &ws); err != nil {
		return nil, time.Time{}, fmt.Errorf("sheet1: %w", err)
	}

	var asOf time.Time
	var holdings []holding
	tickerCol, weightCol := "", ""
	for _, row := range ws.Rows {
		cells := map[string]string{}
		for _, c := range row.Cells {
			v := c.Value
			switch c.Type {
			case "s":
				i, err := strconv.Atoi(v)
				if err != nil || i < 0 || i >= len(strs) {
					return nil, time.Time{}, fmt.Errorf("cell %s: bad shared string index %q", c.Ref, v)
				}
				v = strs[i]
			case "inlineStr":
				v = c.Inline
			}
			cells[column(c.Ref)] = strings.TrimSpace(v)
		}
		if tickerCol == "" {
			for col, v := range cells {
				if date, ok := strings.CutPrefix(v, "As of "); ok && asOf.IsZero() {
					if asOf, err = time.Parse("02-Jan-2006", date); err != nil {
						return nil, time.Time{}, fmt.Errorf("cell %s: holdings date %q: %w", col, v, err)
					}
				}
				switch v {
				case "Ticker":
					tickerCol = col
				case "Weight":
					weightCol = col
				}
			}
			if tickerCol != "" && weightCol == "" || tickerCol == "" && weightCol != "" {
				return nil, time.Time{}, errors.New("header row has only one of Ticker and Weight")
			}
			continue
		}
		ticker := cells[tickerCol]
		if ticker == "" {
			break
		}
		w, err := strconv.ParseFloat(cells[weightCol], 64)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("%s weight %q: %w", ticker, cells[weightCol], err)
		}
		holdings = append(holdings, holding{Ticker: ticker, Weight: w})
	}
	switch {
	case tickerCol == "":
		return nil, time.Time{}, errors.New("no Ticker/Weight header row")
	case asOf.IsZero():
		return nil, time.Time{}, errors.New(`no "As of" holdings date above the header`)
	}
	return holdings, asOf, nil
}

func readZipFile(zr *zip.Reader, name string) ([]byte, error) {
	f, err := zr.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// parseSharedStrings returns each <si> as the text of its <t>, or of its
// rich-text runs joined; phonetic runs (<rPh>) are not part of the value.
func parseSharedStrings(raw []byte) ([]string, error) {
	var sst struct {
		Items []struct {
			T    string `xml:"t"`
			Runs []struct {
				T string `xml:"t"`
			} `xml:"r"`
		} `xml:"si"`
	}
	if err := xml.Unmarshal(raw, &sst); err != nil {
		return nil, err
	}
	out := make([]string, len(sst.Items))
	for i, si := range sst.Items {
		var b strings.Builder
		b.WriteString(si.T)
		for _, r := range si.Runs {
			b.WriteString(r.T)
		}
		out[i] = b.String()
	}
	return out, nil
}

// column returns the letters of a cell reference such as "AB12".
func column(ref string) string {
	return strings.TrimRight(ref, "0123456789")
}

// aggregate sums holding weights per sector. Holdings sectorOf cannot place
// (cash, rights, a name newer than the sector table) are returned unresolved.
func aggregate(holdings []holding, sectorOf func(string) (string, bool)) split {
	s := split{Weights: map[string]float64{}}
	for _, h := range holdings {
		sector, ok := sectorOf(h.Ticker)
		if !ok {
			s.Unresolved = append(s.Unresolved, h)
			continue
		}
		s.Weights[sector] += h.Weight
		s.Resolved++
		s.Sum += h.Weight
	}
	return s
}

func checkCoverage(resolved int, sum float64) error {
	if resolved < minResolved {
		return fmt.Errorf("only %d SPY tickers resolved to a sector, want at least %d", resolved, minResolved)
	}
	if sum < minSectorSum {
		return fmt.Errorf("resolved sector weights sum to %.2f, want at least %.0f", sum, minSectorSum)
	}
	return nil
}

// render formats the split as gofmt-clean Go source with sorted keys and
// weights rounded to two decimals, so identical inputs give identical bytes.
func render(asOf time.Time, weights map[string]float64) ([]byte, error) {
	var b strings.Builder
	b.WriteString(`// Code generated by scripts/refresh-fund-sectors. DO NOT EDIT.
//
// To refresh: ` + "`make refresh-fund-sectors`" + `.

package daemon

// spyLookThrough is SPY's GICS sector split: each holding in State Street's
// daily workbook, summed under the sector the embedded S&P 500 table gives
// its ticker. AsOf is the workbook's holdings date.
var spyLookThrough = fundSectorWeights{
`)
	fmt.Fprintf(&b, "AsOf: %q,\nSource: %q,\nWeights: map[string]float64{\n", asOf.Format(time.DateOnly), source)
	for _, sector := range slices.Sorted(maps.Keys(weights)) {
		fmt.Fprintf(&b, "%q: %s,\n", sector, strconv.FormatFloat(math.Round(weights[sector]*100)/100, 'f', 2, 64))
	}
	b.WriteString("},\n}\n")
	return format.Source([]byte(b.String()))
}
