package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// holdingsSource says where the current-holdings list came from, so a report
// states whether contract names could be checked against it.
const (
	holdingsFromCache   = "cache"
	holdingsUnavailable = "unavailable"
)

// unmaskedTickers are single letters and Canary's own market instruments and
// vocabulary, the gate's unchecked list: the product names them regardless
// of holdings, so naming them reveals nothing.
var unmaskedTickers = strings.Fields(`VIX VVIX VIX3M SPX NDX RUT DJI DJX DXY SPY QQQ IWM DIA GLD TLT HYG SMH XLK XLF XLI XLE XLV
XLY XLP XLU XLB XLRE IBIT IBKR CBOE CME ICE OPT CASH LMT DAY DTE DD ET PATH KEY CI ON AI UI`)

var tickerPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:[.][A-Z0-9]+)?$`)

// xdgDir resolves an XDG base directory as the account-data gate does.
func xdgDir(env, home string, fallback ...string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	return filepath.Join(append([]string{home}, fallback...)...)
}

// holdings is the current-holdings list a report must never name. When no
// list could be read, contract names are withheld rather than checked.
type holdings struct {
	source  string
	tickers map[string]bool
	// pattern matches the holdings masked in free text; nil when none are.
	pattern *regexp.Regexp
}

func (h holdings) available() bool { return h.source != holdingsUnavailable }

// held is set once by main before classification; tests set it directly.
var held = holdings{source: holdingsUnavailable}

// loadHoldings reads the holdings file the daemon writes for the
// account-data gate (internal/daemon/account_data_files.go): the latest Flex
// position report per account. The monitor never opens daemon.db itself: an
// outside SQLite opener resets a live daemon's shared memory if the daemon's
// lock on it is lost (crash of 2026-10-29). Without the file the list is
// unavailable.
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
	h := holdings{source: source, tickers: map[string]bool{}}
	var words []string
	for _, t := range raw {
		// An IBKR class suffix ("BRK B") keeps its root.
		root, _, _ := strings.Cut(strings.ToUpper(strings.TrimSpace(t)), " ")
		if !tickerPattern.MatchString(root) || h.tickers[root] {
			continue
		}
		h.tickers[root] = true
		if len(root) > 1 && !slices.Contains(unmaskedTickers, root) {
			words = append(words, regexp.QuoteMeta(root))
		}
	}
	if len(words) == 0 {
		return h
	}
	// Longest first so a ticker never masks only the prefix of a longer one.
	slices.SortFunc(words, func(a, b string) int { return len(b) - len(a) })
	h.pattern = regexp.MustCompile(`\b(?:` + strings.Join(words, "|") + `)\b`)
	return h
}

// mask replaces every current holding named in message.
func (h holdings) mask(message string) string {
	if h.pattern == nil {
		return message
	}
	return h.pattern.ReplaceAllString(message, "[holding]")
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
	case held.tickers[root] && !slices.Contains(unmaskedTickers, root):
		symbol = "[holding]"
	}
	return strings.TrimSpace(symbol + " " + secType)
}
