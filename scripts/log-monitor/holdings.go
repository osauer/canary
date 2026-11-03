package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/redact"
)

// holdingsSource says where the current-holdings list came from, so a report
// states whether contract names could be checked against it.
const (
	holdingsFromCache   = "cache"
	holdingsUnavailable = "unavailable"
)

// xdgDir resolves an XDG base directory as the account-data gate does.
func xdgDir(env, home string, fallback ...string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	return filepath.Join(append([]string{home}, fallback...)...)
}

// holdings is the current-holdings list a report must never name, with
// where it came from. When no list could be read, contract names are
// withheld rather than checked.
type holdings struct {
	source string
	list   redact.Holdings
}

func (h holdings) available() bool { return h.source != holdingsUnavailable }

// held is set once by main before classification; tests set it directly.
var held = holdings{source: holdingsUnavailable}

// loadHoldings reads the account-data gate's private copy of the daemon's
// positions, refreshed whenever the gate runs. The monitor never opens
// daemon.db itself: an outside SQLite opener resets a live daemon's shared
// memory if the daemon's lock on it is lost (crash of 2026-10-29). Without
// the copy the list is unavailable.
func loadHoldings(cache string) holdings {
	if tickers, ok := cacheHoldings(cache); ok {
		return newHoldings(holdingsFromCache, tickers)
	}
	return holdings{source: holdingsUnavailable}
}

func cacheHoldings(path string) ([]string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var tickers []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); !strings.HasPrefix(line, "#") {
			tickers = append(tickers, line)
		}
	}
	return tickers, scanner.Err() == nil && len(tickers) > 0
}

func newHoldings(source string, raw []string) holdings {
	return holdings{source: source, list: redact.NewHoldings(raw)}
}

// noticeContract matches the contract tag the connector writes on a broker
// notice it can attribute: "System notice reqID=N (SYMBOL SECTYPE) code=".
var noticeContract = regexp.MustCompile(`System notice reqID=\d+ \(([^()]+)\) code=`)

// noticeContractLabel names the contract a broker notice is about: a
// holding as [holding], every contract as [contract] when no holdings list
// was read, and "untagged" when the connector could not attribute it.
func noticeContractLabel(line string) string {
	m := noticeContract.FindStringSubmatch(line)
	if m == nil {
		return "untagged"
	}
	label := strings.TrimSpace(m[1])
	symbol, secType := label, ""
	if i := strings.LastIndexByte(label, ' '); i > 0 {
		symbol, secType = label[:i], label[i+1:]
	}
	root, _, _ := strings.Cut(symbol, " ")
	switch {
	case !held.available():
		symbol = "[contract]"
	case held.list.Holds(root) && !slices.Contains(redact.UnmaskedTickers, root):
		symbol = "[holding]"
	}
	return strings.TrimSpace(symbol + " " + secType)
}
