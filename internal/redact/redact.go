// Package redact is Canary's one text redactor for free text that leaves the
// machine: the scheduled log monitor's samples and the `canary feedback`
// report. Neither may carry an account id, a balance, a holding, a broker
// order reference, a host name, a user name or a path under the home
// directory. One package holds the rules so the two readers cannot drift.
//
// The rules replace values with bracketed words ("[account]", "[ref]") and
// keep the surrounding text, so a redacted message still says what happened.
// A structured value the caller composed itself (a version, "API server 203")
// does not go through Text: the duration, clock and rate rules would read it
// as an observation.
package redact

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var (
	accountPattern = regexp.MustCompile(`\b(?:DU|U)\d{5,}\b`)
	sensitiveField = regexp.MustCompile(`(?i)\b(account(?:_id)?|order(?:_id|_?ref)?|preview_token|token|balance|holding|position|symbol|conid|reqid)=("[^"]*"|\S+)`)
	symbolPhrase   = regexp.MustCompile(`(?i)\bfor [A-Z][A-Z0-9.]{0,9} via\b`)
	spacePattern   = regexp.MustCompile(`\s+`)
	// Ephemeral values embedded in messages (socket addresses, event
	// timestamps) would make every occurrence a distinct signal, letting one
	// reconnect storm fill the whole bounded list with unmergeable
	// singletons. Normalize them so repeats collapse into one counted signal.
	addrPattern     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}:\d{1,5}\b`)
	inlineTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`)
	// Elapsed times, retry clocks, option expiries, rates, pids and progress
	// counters vary per attempt too: 172 gamma prewarm failures in one day
	// were 172 singletons, all cut into the suppressed summary. Codes and
	// statuses are kept, so distinct broker errors stay distinct signals.
	durationPattern = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:h|m|s|ms|µs)(?:\d+(?:\.\d+)?(?:m|s|ms|µs))*\b`)
	expiryPattern   = regexp.MustCompile(`\b20\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\b`)
	clockPattern    = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}\b`)
	ratePattern     = regexp.MustCompile(`\b\d+\.\d{3,}\b`)
	pidPattern      = regexp.MustCompile(`\bpid \d+\b`)
	counterPattern  = regexp.MustCompile(`\b([a-z_]+)=\d+(?:[./]\d+)?\b|\(cached \d+ so far\)`)
	// Broker order references also travel in free text: the WhatIf timeout
	// names "order 48213", broker notices "OrderId 48213" or "permId =…", the
	// order wire summary "orderId=…", the modify-token check "permanent ID
	// …, current order is …". A camelCase identifier ending in Order
	// ("openOrder 48213") has no word boundary before "order", so it is
	// matched on its own. The keyword and separator stay; the number does
	// not. "order 2 of 3" is a position, kept by orderRefs.
	orderRefPattern = regexp.MustCompile(`\b(?:[A-Za-z]*[a-z]Order|(?i:order|perm(?:anent)?|parent[ _]?id))(?i:[ _]?id)?(?i:\s*[=:#]\s*|\s+(?:is\s+)?#?)(\d+)\b`)
	ordinalTail     = regexp.MustCompile(`^\s+of\s+\d`)
	// A home directory on macOS or Linux with the user name it carries: the
	// backstop for another user's home or a path quoted from another
	// machine, after this machine's exact home path is masked.
	homePathPattern = regexp.MustCompile(`(?:/Users|/home)/[^/\s"']+`)
	// A money figure of any size, grouped or not, with an optional decimal
	// part beside a currency word or sign. Broker codes ("code=10167",
	// "IBKR 354") carry no currency and stay.
	currencies   = `(?:USD|EUR|GBP|CHF|JPY|CAD|AUD|HKD|SGD|CNY|CNH|KRW|[$€£¥₩])`
	moneyNumber  = `-?(?:\d{1,3}(?:[,._ ]\d{3})+|\d+)(?:[.,]\d+)?`
	moneyPattern = regexp.MustCompile(`(?i)(?:` + currencies + `\s?` + moneyNumber + `\b|\b` + moneyNumber + `\s?` + currencies + `)`)
	// A host name as a message names it: two or more dotted labels ending in
	// a common suffix; never a version ("v3.33.0"), a code or a file name.
	hostPattern = regexp.MustCompile(`\b(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\.)+(?:local|lan|home|internal|corp|com|net|org|io|dev|de|uk|ch|fr|eu|app|cloud|me|co)\b`)
	// The port after a masked host is configuration, not identity, but a
	// host with its port is an address; the address goes as one.
	hostPortPattern = regexp.MustCompile(`\[host\]:\d{1,5}\b`)
)

// UnmaskedTickers are single letters and Canary's own market instruments and
// vocabulary, the account-data gate's unchecked list: the product names them
// regardless of holdings, so naming them reveals nothing.
var UnmaskedTickers = strings.Fields(`VIX VVIX VIX3M SPX NDX RUT DJI DJX DXY SPY QQQ IWM DIA GLD TLT HYG SMH XLK XLF XLI XLE XLV
XLY XLP XLU XLB XLRE IBIT IBKR CBOE CME ICE OPT CASH LMT DAY DTE DD ET PATH KEY CI ON AI UI`)

var tickerPattern = regexp.MustCompile(`^[A-Z0-9]+(?:[.][A-Z0-9]+)?$`)

// Holdings is the current-holdings list a text must never name. The zero
// value masks nothing; a caller without a list says so rather than
// pretending to have checked.
type Holdings struct {
	tickers map[string]bool
	// pattern matches the holdings masked in free text; nil when none are.
	pattern *regexp.Regexp
}

// NewHoldings builds the mask from ticker roots. An IBKR class suffix
// ("BRK B") and an OCC option leg ("SYNTH 260918C00005000") keep their root.
func NewHoldings(raw []string) Holdings {
	h := Holdings{tickers: map[string]bool{}}
	var words []string
	for _, t := range raw {
		root, _, _ := strings.Cut(strings.ToUpper(strings.TrimSpace(t)), " ")
		if !tickerPattern.MatchString(root) || h.tickers[root] {
			continue
		}
		h.tickers[root] = true
		// A single letter can be ordinary prose; a single digit can be an
		// exchange's complete stock symbol and must retain the holding mask.
		if (len(root) > 1 || root[0] >= '0' && root[0] <= '9') && !slices.Contains(UnmaskedTickers, root) {
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

// Holds reports whether root is on the list, whatever the unmasked
// vocabulary says.
func (h Holdings) Holds(root string) bool { return h.tickers[root] }

// Count is the number of distinct roots on the list.
func (h Holdings) Count() int { return len(h.tickers) }

// Mask replaces every current holding named in message with "[holding]".
func (h Holdings) Mask(message string) string {
	if h.pattern == nil {
		return message
	}
	return h.pattern.ReplaceAllString(message, "[holding]")
}

// Identity is what names this machine and its user. The exact strings are
// masked before any pattern runs, so a host called "mbp" or a user called
// "oliver" is caught where no pattern could know them.
type Identity struct {
	Home     string
	User     string
	Hostname string
	// Hosts are further names to mask verbatim, such as the configured
	// gateway host.
	Hosts []string
}

// LocalIdentity reads the running process's home, user and host names.
func LocalIdentity() Identity {
	id := Identity{}
	if name, err := os.Hostname(); err == nil {
		id.Hostname = name
	}
	if home, err := os.UserHomeDir(); err == nil {
		id.Home = home
		id.User = filepath.Base(home)
	}
	if user := os.Getenv("USER"); user != "" {
		id.User = user
	}
	return id
}

// Redactor is one configured rule set. Monitor is the log monitor's: it
// redacts what a log line can carry. Outbound adds what a report that
// leaves the machine must also drop: money figures, host names, home paths
// and the machine's own identity.
type Redactor struct {
	Identity Identity
	Holdings Holdings
	outbound bool
}

// Monitor is the log monitor's rule set over the given holdings.
func Monitor(held Holdings) Redactor { return Redactor{Holdings: held} }

// Outbound is the rule set for text that leaves the machine.
func Outbound(id Identity, held Holdings) Redactor {
	return Redactor{Identity: id, Holdings: held, outbound: true}
}

// Text applies the rules to free text: account ids, sensitive key=value
// fields, symbols in connector phrases, socket addresses, timestamps, broker
// order references, durations, expiries, clocks, rates, pids, counters, the
// holdings list and, outbound, money figures, host names, home paths and the
// machine's identity; then collapses whitespace.
func (r Redactor) Text(message string) string {
	if r.outbound {
		message = maskIdentity(message, r.Identity)
	}
	message = accountPattern.ReplaceAllString(message, "[account]")
	message = sensitiveField.ReplaceAllString(message, "$1=[redacted]")
	message = symbolPhrase.ReplaceAllString(message, "for [symbol] via")
	message = addrPattern.ReplaceAllString(message, "[addr]")
	message = inlineTimestamp.ReplaceAllString(message, "[time]")
	message = orderRefs(message)
	if r.outbound {
		message = moneyPattern.ReplaceAllString(message, "[amount]")
	}
	message = durationPattern.ReplaceAllString(message, "[duration]")
	message = expiryPattern.ReplaceAllString(message, "[expiry]")
	message = clockPattern.ReplaceAllString(message, "[clock]")
	message = ratePattern.ReplaceAllString(message, "[rate]")
	message = pidPattern.ReplaceAllString(message, "pid N")
	message = counterPattern.ReplaceAllStringFunc(message, func(field string) string {
		key, _, ok := strings.Cut(field, "=")
		if !ok {
			return "(cached N so far)"
		}
		if strings.HasSuffix(key, "code") || key == "status" {
			return field
		}
		return key + "=N"
	})
	if r.outbound {
		message = hostPattern.ReplaceAllString(message, "[host]")
		message = hostPortPattern.ReplaceAllString(message, "[addr]")
		message = homePathPattern.ReplaceAllString(message, "~")
	}
	message = r.Holdings.Mask(message)
	return spacePattern.ReplaceAllString(strings.TrimSpace(message), " ")
}

// maskIdentity replaces the exact home path, host names and user name. The
// home path goes first so its user segment is gone before the bare user
// name is masked; a one-letter name is left alone, it would mask every
// word carrying that letter.
func maskIdentity(message string, id Identity) string {
	if id.Home != "" {
		message = strings.ReplaceAll(message, id.Home, "~")
	}
	for _, host := range append([]string{id.Hostname}, id.Hosts...) {
		host = strings.TrimSpace(host)
		if host == "" || host == "localhost" || host == "127.0.0.1" {
			continue
		}
		message = replaceWord(message, host, "[host]")
		if short, _, ok := strings.Cut(host, "."); ok && len(short) > 2 {
			message = replaceWord(message, short, "[host]")
		}
	}
	if len(id.User) > 1 {
		message = replaceWord(message, id.User, "[user]")
	}
	return message
}

// replaceWord replaces word wherever it stands on its own, whatever its
// case: "Oliver" in prose, "oliver" in a path, "OLIVER" in a shouted log.
func replaceWord(message, word, with string) string {
	re := regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(word) + `($|[^A-Za-z0-9_])`)
	for {
		next := re.ReplaceAllString(message, "${1}"+with+"${2}")
		if next == message {
			return message
		}
		message = next
	}
}

// orderRefs replaces the number of every broker order reference with
// [ref], keeping the words around it, except an ordinal such as "order 2 of
// 3". An ordinal has at most two digits: "order 48213 of 100 shares" names a
// broker order.
func orderRefs(message string) string {
	var out strings.Builder
	last := 0
	for _, m := range orderRefPattern.FindAllStringSubmatchIndex(message, -1) {
		if m[3]-m[2] <= 2 && ordinalTail.MatchString(message[m[1]:]) {
			continue
		}
		out.WriteString(message[last:m[2]])
		out.WriteString("[ref]")
		last = m[3]
	}
	if last == 0 {
		return message
	}
	out.WriteString(message[last:])
	return out.String()
}
