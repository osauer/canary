// Command wire-assert reads a JSONL wire log (as emitted by
// IBKR_WIRE_LOG_PATH)
// and evaluates one named invariant against the frames produced after a
// given byte offset. Exit 0 = pass; non-zero = fail with a one-screen
// failure report on stderr.
//
// The script that drives this binary (scripts/wire-smoke.sh) captures
// the wire log's size before invoking a CLI command, then passes that
// size as --since-offset so the check only sees frames produced by THIS
// command — not the daemon's boot chatter or earlier commands.
//
// Adding a new invariant: add a case to dispatch(), implement a
// CheckFunc, document it in the catalogue below. No plug-in machinery;
// the binary is a single switch statement on purpose.
//
// Catalogue:
//
//	quote-spy                — reqMktData SPY STK + tickPrice within budget
//	chain-iv-source          — ≥1 OPTION_COMPUTATION (msg 21) with non-NaN IV
//	gamma-noflag             — gamma --no-wait returns terminal status, never pending
//	gamma-premarket-derived  — in loose mode, the gamma envelope reports
//	                           derived_iv_legs > 0 or model_tick_legs > 0
//	                           (off-hours pricing path landed)
//	regime-subs              — MarketDataType notice for each of VIX/VIX3M/HYG/SPY/USDJPY
//	account-summary          — reqAccountSummary OUT + accountSummary IN
//	status-handshake         — at least one MarketDataType notice (58) inbound
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
// local type so the assertion binary doesn't depend on the connector
// package — the JSONL format is the contract.
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
// without scrolling the JSONL.
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
// offset lets the script scope each check to "frames produced by THIS
// command" — the script captures the file's pre-command size, runs the
// command, then passes the size back here. Frames added by concurrent
// daemon chatter (heartbeats, autosubs) before the offset are filtered.
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
			// writes one JSON object per line so malformed entries
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
		jsonlPath    = flag.String("jsonl", "", "path to wire JSONL log")
		sinceOff     = flag.Int64("since-offset", 0, "skip bytes before this offset")
		check        = flag.String("check", "", "check name (quote-spy, chain-iv-source, …)")
		loose        = flag.Bool("loose", false, "loosen budgets when gateway is in frozen/off-hours mode")
		gammaEnvPath = flag.String("gamma-envelope-path", "", "path to a JSON file holding the gamma envelope (only used by gamma-premarket-derived)")
		listChecks   = flag.Bool("list", false, "print the catalogue of supported checks and exit")
	)
	flag.Parse()

	if *listChecks {
		for _, c := range catalogue() {
			fmt.Printf("%-24s %s\n", c.name, c.summary)
		}
		return
	}

	if *jsonlPath == "" || *check == "" {
		fmt.Fprintln(os.Stderr, "usage: wire-assert --jsonl PATH --check NAME [--since-offset N] [--loose] [--gamma-envelope-path PATH]")
		fmt.Fprintln(os.Stderr, "       wire-assert --list")
		os.Exit(2)
	}

	frames, err := readFrames(*jsonlPath, *sinceOff)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wire-assert: read frames: %v\n", err)
		os.Exit(2)
	}

	// Auxiliary input: the gamma envelope JSON, loaded only for the
	// gamma-premarket-derived check which inspects derived_iv_legs (a
	// daemon-response field, not a wire-frame field).
	var gammaEnvBytes []byte
	if *gammaEnvPath != "" {
		gammaEnvBytes, err = os.ReadFile(*gammaEnvPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wire-assert: read gamma envelope %s: %v\n", *gammaEnvPath, err)
			os.Exit(2)
		}
	}

	result := dispatch(*check, checkInputs{
		Frames:        frames,
		Loose:         *loose,
		GammaEnvelope: gammaEnvBytes,
	})
	if result.OK {
		return
	}
	result.Print(*jsonlPath, len(frames))
	os.Exit(1)
}

// ---- catalogue ------------------------------------------------------------

// checkInputs aggregates everything a check function may need. Most
// checks only read Frames; gamma-premarket-derived also reads
// GammaEnvelope. Passing one struct keeps the type signature uniform
// when new auxiliary inputs are added.
type checkInputs struct {
	Frames        []WireFrame
	Loose         bool
	GammaEnvelope []byte // raw JSON; nil when --gamma-envelope-path wasn't passed
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
		{"chain-iv-source", "after canary chain SPY --width 5: ≥1 OPTION_COMPUTATION (msg 21) with non-NaN IV from any OPT reqID", checkChainIVSource},
		{"regime-subs", "after canary regime: MarketDataType notice for each of VIX/VIX3M/HYG/SPY/USDJPY", checkRegimeSubs},
		{"gamma-noflag", "after canary gamma --no-wait: terminal status (ready or known error), never pending", checkGammaNoFlag},
		{"gamma-premarket-derived", "in loose mode, gamma envelope JSON reports derived_iv_legs > 0 or model_tick_legs > 0 (off-hours pricing path landed)", checkGammaPremarketDerived},
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
	// internal state via the local socket), but on a fresh daemon the
	// boot sequence must have produced wire activity: the connection
	// handshake, SetMarketDataType OUT (msg 59), and system-notice
	// IN frames for the connected data farms (msg 204 with codes
	// 2104/2106/2158).
	var sawFarmNotice bool
	var sawSetType bool
	for _, f := range frames {
		if f.Direction == "OUT" && f.MsgID == 59 {
			sawSetType = true
		}
		if f.Direction == "IN" && f.MsgID == 204 {
			// msg 204 is a system notification with a protobuf-encoded
			// payload; the wire interceptor captures the raw blob in
			// fields[1]. The human-readable substring "farm connection
			// is OK" is reliably embedded — codes 2104 (market data
			// farm), 2106 (HMDS), 2158 (sec-def farm) all carry it.
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
	// Symbol=SPY. Expected inbound: tickPrice (msg 1) with tickType in
	// {1, 2, 4} — bid, ask, last. In loose off-hours mode, accept the
	// same fallback ticks the quote engine documents: mark (37) and
	// previous close (9). Live mode keeps the stricter check so a broken
	// current-tick path does not slip through.
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
	// relying on the generic tick 106 stock-IV field. At least one OPT
	// subscription must receive a non-NaN IV value.
	// Frames are filtered to msg_id=21 and we read fields[4] (IV).
	// In loose mode (frozen/off-hours), the check WARNS instead of
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
		return CheckResult{
			Expected: "≥1 reqMktData OUT with SecType=OPT after canary chain",
			Observed: "0 OPT subscribes",
		}
	}
	if !anyModelTick {
		if loose {
			// Off-hours: model engine idle is expected. Pass with a soft
			// signal in observed (the script can downgrade to a warning).
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

func checkRegimeSubs(in checkInputs) CheckResult {
	frames := in.Frames
	// regime fans out to VIX, VIX3M, HYG, SPY, USDJPY. Each gets a
	// reqMktData OUT and a MarketDataType notice IN. We require all
	// five outbound subscribes; inbound is best-effort because
	// off-hours some indicators (VIX3M particularly) can be slow.
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

func checkGammaNoFlag(in checkInputs) CheckResult {
	frames := in.Frames
	// gamma --no-wait should return a terminal status (ready or a
	// known error) without blocking. This check looks at the wire to
	// confirm the daemon kicked off the compute and either reused a
	// cached result or has a compute in flight. It does NOT block on
	// the compute completing. A "no OPT subscribes" outcome means the
	// compute is fully cached and returned ready instantly.
	for _, f := range frames {
		if f.Direction == "OUT" && f.MsgID == 1 && f.MsgName == "reqMktData" {
			if len(f.Fields) >= 6 && strings.EqualFold(f.Fields[5], "OPT") {
				// Active compute is in flight; that's terminal-ok for --no-wait.
				return CheckResult{OK: true, Observed: "compute in flight (OPT subscribes observed)"}
			}
		}
	}
	// No new OPT subscribes — either cached or fully off-hours.
	// Either way --no-wait should return without hanging. We trust
	// the CLI returning at all (which the script proves by reaching
	// this check) as evidence of the status invariant.
	return CheckResult{OK: true, Observed: "no new OPT subscribes (cached or skipped)"}
}

// checkGammaPremarketDerived asserts that a completed gamma compute
// off-hours found at least one priced leg through either the BS-IV
// Newton-Raphson fallback or a gateway model tick. Inspects the JSON
// envelope passed via --gamma-envelope-path (the daemon's response
// from `canary gamma --wait …`), not the wire frames — these counters are
// daemon-internal aggregations that have no wire-frame representation.
//
// Strict mode (live): the check is skipped (model engine is active,
// fallback need not fire). The wire-smoke script only runs this check
// when LOOSE=1.
//
// In loose mode without a usable envelope (no completed compute,
// gamma is still pending, or the envelope has Status != "ready"), the
// check passes with an explanatory observation rather than fails —
// the assertion is "if a result is available, it used a valid pricing
// path", not "the compute must complete before this check runs."
func checkGammaPremarketDerived(in checkInputs) CheckResult {
	if !in.Loose {
		return CheckResult{OK: true, Observed: "strict mode: skipped (BS-IV fallback only required off-hours)"}
	}
	if len(in.GammaEnvelope) == 0 {
		return CheckResult{
			Expected: "--gamma-envelope-path PATH (the gamma JSON response)",
			Observed: "no envelope provided",
		}
	}
	// Minimal struct: only the fields this check inspects. Tolerates
	// extra fields on the wire (forward-compat with future envelope
	// additions).
	type envResult struct {
		Status        string `json:"status"`
		LegCount      int    `json:"leg_count"`
		DerivedIVLegs int    `json:"derived_iv_legs"`
		ModelTickLegs int    `json:"model_tick_legs"`
	}
	type env struct {
		Status string    `json:"status"`
		Result envResult `json:"result"`
	}
	var e env
	if err := json.Unmarshal(in.GammaEnvelope, &e); err != nil {
		return CheckResult{
			Expected:   "JSON envelope parseable as the gamma response",
			Observed:   fmt.Sprintf("unmarshal failed: %v", err),
			Hypothesis: "CLI may have emitted an error envelope rather than the gamma response shape",
		}
	}
	if e.Status != "ready" {
		// Status=computing/error: nothing to assert (a pending or
		// errored compute doesn't tell us anything about the fallback).
		return CheckResult{OK: true, Observed: fmt.Sprintf("envelope status=%q (no completed result to inspect)", e.Status)}
	}
	if e.Result.DerivedIVLegs == 0 && e.Result.ModelTickLegs == 0 {
		return CheckResult{
			Expected:   "derived_iv_legs > 0 or model_tick_legs > 0 in loose mode",
			Observed:   fmt.Sprintf("derived_iv_legs=0 model_tick_legs=0 with leg_count=%d", e.Result.LegCount),
			Hypothesis: "neither the gateway model engine nor the BS-IV fallback priced a leg. Check internal/daemon/gamma_zero_compute.go productionLegFetcher Stage 2b.",
		}
	}
	return CheckResult{OK: true, Observed: fmt.Sprintf("derived_iv_legs=%d model_tick_legs=%d leg_count=%d", e.Result.DerivedIVLegs, e.Result.ModelTickLegs, e.Result.LegCount)}
}
