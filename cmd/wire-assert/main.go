// Command wire-assert reads a JSONL wire log (as emitted by
// IBKR_WIRE_LOG_PATH)
// and evaluates one named invariant against the frames produced after a
// given byte offset. Exit 0 = pass; non-zero = fail with a one-screen
// failure report on stderr.
// The script that drives this binary (scripts/wire-smoke.sh) captures
// the wire log's size before invoking a CLI command, then passes that
// size as --since-offset so the check only sees frames produced by THIS
// Adding a new invariant: add a case to dispatch(), implement a
//
//	account-summary        — reqAccountSummary OUT + accountSummary IN
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// WireFrame mirrors pkg/ibkr.WireFrame's JSON shape. Kept as a separate
type WireFrame struct {
	Seq       uint64    `json:"seq"`
	When      time.Time `json:"ts"`
	Direction string    `json:"direction"` // "OUT" | "IN"
	MsgID     int       `json:"msg_id"`
	MsgName   string    `json:"msg_name"`
	ReqID     string    `json:"req_id,omitempty"`
	Symbol    string    `json:"symbol,omitempty"`
	Fields    []string  `json:"fields"`
}

// CheckResult describes a single invariant's verdict. On failure the
// Print method emits a one-screen report — exactly what a future
// maintainer needs to see to diagnose the wire-level regression
type CheckResult struct {
	OK         bool
	Name       string
	Expected   string
	Observed   string
	Hypothesis string // one-line guess at the failure cause
	Highlight  []WireFrame
}

// Print writes a bounded failure report to standard error.
func (r CheckResult) Print(jsonlPath string, total int) {
	fmt.Fprintf(os.Stderr, "wire-assert: FAIL [%s]\n\n", r.Name)
	fmt.Fprintf(os.Stderr, "Expected:\n  %s\n\n", r.Expected)
	fmt.Fprintf(os.Stderr, "Observed:\n  %s\n\n", r.Observed)
	if len(r.Highlight) > 0 {
		fmt.Fprintf(os.Stderr, "Relevant frames (from %s, %d total in window):\n", jsonlPath, total)
		for _, f := range r.Highlight {
			fmt.Fprintf(os.Stderr, "  %s #%-4d %s msg=%-3d %-22s reqID=%-3s symbol=%-10s fields=%v\n",
				f.When.Format("15:04:05.000"), f.Seq, f.Direction, f.MsgID, f.MsgName, f.ReqID, f.Symbol, truncateFields(f.Fields, 8))
		}
		fmt.Fprintln(os.Stderr)
	}
	if r.Hypothesis != "" {
		fmt.Fprintf(os.Stderr, "Hypothesis: %s\n", r.Hypothesis)
	}
}

func truncateFields(f []string, max int) []string {
	if len(f) <= max {
		return f
	}
	return append(append([]string{}, f[:max]...), "…")
}

// readFrames parses the JSONL log from the given byte offset. The
func readFrames(path string, sinceOffset int64) ([]WireFrame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	if sinceOffset > 0 {
		if _, err := f.Seek(sinceOffset-1, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek %d: %w", sinceOffset-1, err)
		}
		prev := []byte{0}
		if _, err := f.Read(prev); err != nil {
			return nil, fmt.Errorf("read byte before offset %d: %w", sinceOffset, err)
		}
		dropPartial := prev[0] != '\n'

		if _, err := f.Seek(sinceOffset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek %d: %w", sinceOffset, err)
		}
		br := bufio.NewReader(f)
		if dropPartial {
			if _, err := br.ReadBytes('\n'); err != nil && err != io.EOF {
				return nil, fmt.Errorf("skip partial line at offset %d: %w", sinceOffset, err)
			}
		}
		return parseFrames(br)
	}
	return parseFrames(bufio.NewReader(f))
}

func parseFrames(br *bufio.Reader) ([]WireFrame, error) {
	var out []WireFrame
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var frame WireFrame
			if err := json.Unmarshal(line, &frame); err == nil {
				out = append(out, frame)
			}
			// Malformed line: drop silently. The wire interceptor
			// indicate disk corruption, not a real wire event.
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func main() {
	var (
		jsonlPath  = flag.String("jsonl", "", "path to wire JSONL log")
		sinceOff   = flag.Int64("since-offset", 0, "skip bytes before this offset")
		check      = flag.String("check", "", "check name (quote-spy, chain-iv-source, …)")
		loose      = flag.Bool("loose", false, "loosen budgets when gateway is in frozen/off-hours mode")
		envPath    = flag.String("envelope-path", "", "path to a JSON file holding the command's response envelope")
		listChecks = flag.Bool("list", false, "print the catalogue of supported checks and exit")
	)
	flag.Parse()

	if *listChecks {
		for _, c := range catalogue() {
			fmt.Printf("%-24s %s\n", c.name, c.summary)
		}
		return
	}

	if *jsonlPath == "" || *check == "" {
		fmt.Fprintln(os.Stderr, "usage: wire-assert --jsonl PATH --check NAME [--since-offset N] [--loose] [--envelope-path PATH]")
		fmt.Fprintln(os.Stderr, "       wire-assert --list")
		os.Exit(2)
	}

	frames, err := readFrames(*jsonlPath, *sinceOff)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wire-assert: read frames: %v\n", err)
		os.Exit(2)
	}

	// Auxiliary input: the JSON response the CLI printed. Typed daemon
	// evidence that wire frames do not carry.
	var envBytes []byte
	if *envPath != "" {
		envBytes, err = os.ReadFile(*envPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wire-assert: read envelope %s: %v\n", *envPath, err)
			os.Exit(2)
		}
	}

	result := dispatch(*check, checkInputs{
		Frames:   frames,
		Loose:    *loose,
		Envelope: envBytes,
	})
	if result.OK {
		return
	}
	result.Print(*jsonlPath, len(frames))
	os.Exit(1)
}

// ---- catalogue ------------------------------------------------------------

// checkInputs aggregates everything a check function may need. Passing one
type checkInputs struct {
	Frames   []WireFrame
	Loose    bool
	Envelope []byte // raw JSON response; nil when --envelope-path wasn't passed
}

type checkEntry struct {
	name    string
	summary string
	fn      func(in checkInputs) CheckResult
}

func catalogue() []checkEntry {
	return []checkEntry{
		{"status-handshake", "after canary status: at least one MarketDataType notice inbound", checkStatusHandshake},
		{"quote-spy", "after canary quote SPY: reqMktData OUT + tickPrice IN within budget", checkQuoteSPY},
		{"account-summary", "after canary account: reqAccountSummary OUT + acctValue/accountSummary IN", checkAccountSummary},
		{"chain-iv-source", "after canary chain SPY --width 5: ≥1 OPTION_COMPUTATION (msg 21) with non-NaN IV, or per-leg IV in the chain response when no new subscribe was needed", checkChainIVSource},
		{"regime-subs", "after canary regime: MarketDataType notice for each of VIX/VIX3M/HYG/SPY/USDJPY", checkRegimeSubs},
		{"gamma-no-wait-envelope", "after canary gamma --no-wait: valid cold/computing/ready/error envelope", checkGammaNoWaitEnvelope},
	}
}

func dispatch(name string, in checkInputs) CheckResult {
	for _, c := range catalogue() {
		if c.name == name {
			r := c.fn(in)
			r.Name = name
			return r
		}
	}
	return CheckResult{
		Name:     name,
		Expected: "a known check name",
		Observed: fmt.Sprintf("unknown check %q", name),
	}
}

// ---- checks ---------------------------------------------------------------

func checkStatusHandshake(in checkInputs) CheckResult {
	frames := in.Frames
	// Status itself doesn't issue new subscribes (it reads daemon
	// boot sequence must have produced wire activity: the connection
	var sawFarmNotice bool
	var sawSetType bool
	for _, f := range frames {
		if f.Direction == "OUT" && f.MsgID == 59 {
			sawSetType = true
		}
		if f.Direction == "IN" && f.MsgID == 204 {
			// msg 204 is a system notification with a protobuf-encoded
			// payload; the wire interceptor captures the raw blob in
			for _, fld := range f.Fields {
				if strings.Contains(fld, "farm connection is OK") {
					sawFarmNotice = true
					break
				}
			}
		}
	}
	if !sawSetType {
		return CheckResult{
			Expected:   "SetMarketDataType OUT (msg 59) during daemon boot",
			Observed:   "no msg 59 outbound",
			Hypothesis: "daemon never sent SetMarketDataType — boot may have failed before market-data init",
		}
	}
	if !sawFarmNotice {
		return CheckResult{
			Expected:   "system-notification IN (msg 204) with code 2104/2106/2158 confirming a market-data farm is connected",
			Observed:   "no farm-connected notice",
			Hypothesis: "gateway accepted the connection but no data farm is up; check IBKR Gateway login + entitlement status",
		}
	}
	return CheckResult{OK: true}
}

func checkQuoteSPY(in checkInputs) CheckResult {
	frames := in.Frames
	// Expected outbound: reqMktData (msg 1) with SecType=STK and
	var outFound bool
	var inTickPrice []WireFrame
	wantTickTypes := map[string]bool{"1": true, "2": true, "4": true}
	if in.Loose {
		wantTickTypes["9"] = true
		wantTickTypes["37"] = true
	}

	for _, f := range frames {
		if f.Direction == "OUT" && f.MsgID == 1 && f.MsgName == "reqMktData" {
			// reqMktData fields layout: [1, 11, reqID, conID, symbol, secType, …]
			if len(f.Fields) >= 6 && strings.EqualFold(f.Fields[4], "SPY") && strings.EqualFold(f.Fields[5], "STK") {
				outFound = true
			}
		}
		if f.Direction == "IN" && f.MsgID == 1 {
			// tickPrice fields: [1, version, reqID, tickType, price, size, …]
			if len(f.Fields) >= 5 && wantTickTypes[f.Fields[3]] {
				price, err := strconv.ParseFloat(f.Fields[4], 64)
				if err == nil && price > 0 {
					inTickPrice = append(inTickPrice, f)
				}
			}
		}
	}

	if !outFound {
		return CheckResult{
			Expected:   "reqMktData OUT with Symbol=SPY SecType=STK",
			Observed:   "no matching reqMktData OUT frame",
			Hypothesis: "the quote command may have used a cached subscription or failed before the wire send",
		}
	}
	if len(inTickPrice) == 0 {
		var sawClose, sawMark bool
		for _, f := range frames {
			if f.Direction != "IN" || f.MsgID != 1 || len(f.Fields) < 5 {
				continue
			}
			switch f.Fields[3] {
			case "9":
				sawClose = true
			case "37":
				sawMark = true
			}
		}
		expected := "≥1 inbound tickPrice (msg 1) with tickType ∈ {1,2,4} and price > 0 within the command's lifetime"
		hyp := "gateway may have downgraded to type=2/3 and is only sending tickType=9 (close); or SPY entitlement is missing"
		if in.Loose {
			expected = "≥1 inbound tickPrice (msg 1) with tickType ∈ {1,2,4,9,37} and price > 0 within the command's lifetime"
			hyp = "gateway sent only non-price frames in loose/frozen mode; check quote fallback handling or entitlement"
		}
		if !sawClose && !sawMark {
			hyp = "gateway sent neither current tickPrice (1/2/4) nor fallback mark/close (37/9) — connection or entitlement issue"
		}
		return CheckResult{
			Expected:   expected,
			Observed:   "0 such tickPrice frames",
			Hypothesis: hyp,
		}
	}
	return CheckResult{OK: true}
}

func checkAccountSummary(in checkInputs) CheckResult {
	frames := in.Frames
	// reqAccountSummary is msg 62 outbound. accountSummary inbound
	// is msg 63. acctValue (legacy account update) is msg 6.
	var outFound, inFound bool
	for _, f := range frames {
		if f.Direction == "OUT" && f.MsgID == 62 {
			outFound = true
		}
		if f.Direction == "IN" && (f.MsgID == 63 || f.MsgID == 6) {
			inFound = true
		}
	}
	if !outFound {
		return CheckResult{
			Expected: "reqAccountSummary OUT (msg 62) after canary account",
			Observed: "no msg 62 OUT frame",
		}
	}
	if !inFound {
		return CheckResult{
			Expected:   "accountSummary IN (msg 63) or acctValue IN (msg 6) after canary account",
			Observed:   "no msg 63 or msg 6 IN frame",
			Hypothesis: "gateway may not be entitled for account data on this client ID",
		}
	}
	return CheckResult{OK: true}
}

func checkChainIVSource(in checkInputs) CheckResult {
	frames := in.Frames
	loose := in.Loose
	// The invariant requires msg 21 option-computation frames rather than
	// subscription must receive a non-NaN IV value.
	// failing — model engine doesn't fire when options aren't trading.
	var anyOPTOut bool
	var anyModelTick bool
	for _, f := range frames {
		if f.Direction == "OUT" && f.MsgID == 1 && f.MsgName == "reqMktData" {
			if len(f.Fields) >= 6 && strings.EqualFold(f.Fields[5], "OPT") {
				anyOPTOut = true
			}
		}
		if f.Direction == "IN" && f.MsgID == 21 {
			// OPTION_COMPUTATION fields: [21, reqID, tickType, tickAttrib, IV, delta, …]
			if len(f.Fields) >= 5 {
				iv, err := strconv.ParseFloat(f.Fields[4], 64)
				if err == nil && iv > 0 && iv < 5 {
					anyModelTick = true
				}
			}
		}
	}
	if !anyOPTOut {
		// Zero OPT subscribes is not evidence of a broken chain on its own.
		// would answer for a chain that returned nothing. Only the chain
		return checkChainResponseIV(in)
	}
	if !anyModelTick {
		if loose {
			// Off-hours: model engine idle is expected. Pass with a soft
			return CheckResult{OK: true, Observed: "loose-mode: 0 model ticks tolerated (likely pre-market/off-hours)"}
		}
		return CheckResult{
			Expected:   "≥1 inbound OPTION_COMPUTATION (msg 21) with non-NaN IV across all OPT subscribes",
			Observed:   "0 model ticks received",
			Hypothesis: "gateway not pushing model ticks. Possible: MarketDataType setting wrong for current hours; or productionLegFetcher reverted to reading MarketData.IV (fed by generic tick 106, not delivered for OPT). See pkg/ibkr/connector.go SubscribeOption.",
		}
	}
	return CheckResult{OK: true}
}

// checkChainResponseIV adjudicates a chain read that issued no new OPT
// wire — neither sends reqMktData — and differ in the response: a leg the
func checkChainResponseIV(in checkInputs) CheckResult {
	if len(in.Envelope) == 0 {
		return CheckResult{
			Expected:   "≥1 reqMktData OUT with SecType=OPT after canary chain, or --envelope-path holding the chain response",
			Observed:   "0 OPT subscribes and no chain response to adjudicate",
			Hypothesis: "pass the chain --json output via --envelope-path so an already-subscribed board can be told apart from an unreachable one",
		}
	}
	// Only the fields this check reads; extra response fields stay
	// forward-compatible, and the status strings are matched against an
	type strike struct {
		CallIV         *float64 `json:"call_iv"`
		PutIV          *float64 `json:"put_iv"`
		CallDataStatus string   `json:"call_data_status"`
		PutDataStatus  string   `json:"put_data_status"`
	}
	var res struct {
		Strikes []strike `json:"strikes"`
	}
	if err := json.Unmarshal(in.Envelope, &res); err != nil {
		return CheckResult{
			Expected:   "JSON envelope parseable as the chain response",
			Observed:   fmt.Sprintf("unmarshal failed: %v", err),
			Hypothesis: "CLI may have emitted an error envelope rather than the chain result shape",
		}
	}
	// A leg that reached an option line, whether or not the model engine
	// the first two are the failure this check exists to catch, and the third
	reached := map[string]bool{"quoted": true, "prev_close": true, "model_only": true}
	var withIV, withData int
	for _, s := range res.Strikes {
		// Same plausibility bound as the msg-21 path above.
		if (s.CallIV != nil && *s.CallIV > 0 && *s.CallIV < 5) || (s.PutIV != nil && *s.PutIV > 0 && *s.PutIV < 5) {
			withIV++
		}
		if reached[s.CallDataStatus] || reached[s.PutDataStatus] {
			withData++
		}
	}
	total := len(res.Strikes)
	if withIV > 0 {
		return CheckResult{OK: true, Observed: fmt.Sprintf(
			"0 new OPT subscribes; chain response carries IV on %d of %d strikes (board already subscribed)", withIV, total)}
	}
	if in.Loose && withData > 0 {
		// Off-hours the model engine is idle, so priced legs without IV are
		return CheckResult{OK: true, Observed: fmt.Sprintf(
			"loose-mode: 0 new OPT subscribes and 0 IVs, but %d of %d strikes carry option data", withData, total)}
	}
	expected := "≥1 chain strike carrying an option IV when no new OPT subscribe was issued"
	if in.Loose {
		expected = "≥1 chain strike carrying an option IV or a priced leg when no new OPT subscribe was issued"
	}
	return CheckResult{
		Expected:   expected,
		Observed:   fmt.Sprintf("0 OPT subscribes, %d of %d strikes with IV, %d with option data", withIV, total, withData),
		Hypothesis: "the chain could not reach an option line at all — check the gateway's option market-data lines and contract resolution (pkg/ibkr/connector.go SubscribeOption); a full TWS restart clears an exhausted line pool",
	}
}

func checkRegimeSubs(in checkInputs) CheckResult {
	frames := in.Frames
	// regime fans out to VIX, VIX3M, HYG, SPY, USDJPY. Each gets a
	wantSymbols := map[string]bool{
		"VIX": false, "VIX3M": false, "HYG": false, "SPY": false, "USD": false,
	}
	for _, f := range frames {
		if f.Direction == "OUT" && f.MsgID == 1 && f.MsgName == "reqMktData" {
			if len(f.Fields) >= 5 {
				sym := strings.ToUpper(f.Fields[4])
				if _, ok := wantSymbols[sym]; ok {
					wantSymbols[sym] = true
				}
			}
		}
	}
	var missing []string
	for s, found := range wantSymbols {
		if !found {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return CheckResult{
			Expected:   "reqMktData OUT for each of VIX, VIX3M, HYG, SPY, USD(JPY)",
			Observed:   fmt.Sprintf("missing: %s", strings.Join(missing, ", ")),
			Hypothesis: "regime fan-out may have aborted early; check daemon log for fetcher errors",
		}
	}
	return CheckResult{OK: true}
}

func checkGammaNoWaitEnvelope(in checkInputs) CheckResult {
	if len(in.Envelope) == 0 {
		return CheckResult{
			Expected: "--envelope-path PATH (the gamma JSON response)",
			Observed: "no envelope provided",
		}
	}

	// Minimal response shape: extra computed fields remain forward-compatible,
	// while lifecycle/result mismatches fail closed.
	type env struct {
		Status    string           `json:"status"`
		StartedAt *time.Time       `json:"started_at,omitempty"`
		Result    *json.RawMessage `json:"result,omitempty"`
		Error     string           `json:"error,omitempty"`
	}
	var e env
	if err := json.Unmarshal(in.Envelope, &e); err != nil {
		return CheckResult{
			Expected:   "JSON envelope parseable as the gamma response",
			Observed:   fmt.Sprintf("unmarshal failed: %v", err),
			Hypothesis: "CLI may have emitted an error envelope rather than the gamma response shape",
		}
	}

	switch e.Status {
	case "cold":
		if e.Result != nil {
			return gammaEnvelopeShapeFailure(e.Status, "result must be absent")
		}
	case "computing":
		if e.StartedAt == nil {
			return gammaEnvelopeShapeFailure(e.Status, "started_at must be present")
		}
		if e.Result != nil {
			return gammaEnvelopeShapeFailure(e.Status, "result must be absent")
		}
	case "ready":
		if e.Result == nil {
			return gammaEnvelopeShapeFailure(e.Status, "result must be present")
		}
	case "error":
		if strings.TrimSpace(e.Error) == "" {
			return gammaEnvelopeShapeFailure(e.Status, "classified error text must be present")
		}
		if e.Result != nil {
			return gammaEnvelopeShapeFailure(e.Status, "result must be absent")
		}
	default:
		return CheckResult{
			Expected:   "status is exactly cold, computing, ready, or error",
			Observed:   fmt.Sprintf("status=%q", e.Status),
			Hypothesis: "the gamma lifecycle contract drifted or untrusted text reached a typed status field",
		}
	}
	return CheckResult{OK: true, Observed: fmt.Sprintf("valid gamma lifecycle envelope: status=%s", e.Status)}
}

func gammaEnvelopeShapeFailure(status, detail string) CheckResult {
	return CheckResult{
		Expected:   "gamma lifecycle fields match the typed envelope contract",
		Observed:   fmt.Sprintf("status=%q: %s", status, detail),
		Hypothesis: "the daemon or CLI emitted a contradictory gamma lifecycle state",
	}
}
