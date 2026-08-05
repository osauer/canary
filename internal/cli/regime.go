package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runRegime(ctx context.Context, env *Env, args []string) int {
	if slicesContains(args, "history") {
		return runRegimeHistory(ctx, env, args)
	}
	fs := flagSet(env, "regime")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON for tooling")
	explain := fs.Bool("explain", false, "show concise threshold, reading, and provenance notes")
	diagnostics := fs.Bool("diagnostics", false, "with --explain, include raw source, missing-field, and quality diagnostics")
	watch := fs.Bool("watch", false, "re-poll continuously; in-place redraw on a TTY")
	rate := fs.Duration("rate", 5*time.Minute, "poll interval for --watch (default 5m)")
	logPath := fs.String("log", "", "append snapshot to a JSONL file at <path> (one line per call)")
	view := fs.String("view", rpc.ViewDetail, "JSON response view: detail | monitor")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return failUnexpectedArgs(env, fs)
	}
	if *diagnostics && !*explain {
		return fail(env, "regime: --diagnostics requires --explain")
	}
	if *view != rpc.ViewDetail && *view != rpc.ViewMonitor {
		return fail(env, "regime: --view must be %q or %q (got %q)", rpc.ViewDetail, rpc.ViewMonitor, *view)
	}
	if *view != rpc.ViewDetail && !*jsonOut {
		return fail(env, "regime: --view requires --json")
	}

	fetchAndRender := func(out io.Writer) int {
		var res rpc.RegimeSnapshotResult
		if err := env.Conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &res); err != nil {
			return fail(env, "regime: %v", err)
		}
		rpc.StripRegimeGammaProfiles(&res)
		// Append to the JSONL log before rendering. If the write fails
		// the user still sees the snapshot — log persistence is a
		// side effect, not the primary goal. Stderr-equivalent warning
		// surfaces via env so a non-TTY consumer can still parse the
		// regime envelope cleanly.
		if *logPath != "" {
			if err := appendRegimeLog(*logPath, res); err != nil {
				_, _ = fmt.Fprintf(env.Stderr, "regime: log append failed: %v\n", err)
			}
		}
		if *jsonOut {
			if *view == rpc.ViewMonitor {
				return printJSONTo(env, out, rpc.CompactRegimeMonitor(&res))
			}
			if !*explain {
				rpc.CompactRegimeSnapshot(&res)
			} else {
				rpc.StripRegimeGammaProfiles(&res)
			}
			return printJSONTo(env, out, res)
		}
		rpc.StripRegimeGammaProfiles(&res)
		return renderRegimeTextWithOptions(env, out, &res, regimeRenderOptions{Explain: *explain, Diagnostics: *diagnostics})
	}

	if *watch {
		if *jsonOut {
			return fail(env, "regime: --watch and --json are mutually exclusive")
		}
		return runWatch(ctx, env, *rate, "regime", fetchAndRender)
	}
	// Single-call TTY path: the daemon's fan-out can sit silent for
	// 10-20 s on a cold cache while the fetchers race contract-
	// resolution against history pulls. Surface motion so the user
	// sees the command is alive instead of staring at a blank
	// terminal. Pipe / non-TTY callers and --json keep their atomic
	// stdout shape unchanged.
	if *jsonOut || !isTerminal(env.Stdout) {
		return fetchAndRender(env.Stdout)
	}
	stop := startRegimeSpinner(env)
	code := fetchAndRender(env.Stdout)
	stop()
	return code
}

// runRegimeHistory renders the daemon's derived regime-decision index
// (regime.history). Read-only: rows are journal evidence; the daemon owns
// filtering, ordering, and index-health disclosure.
func runRegimeHistory(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "regime")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	since := fs.String("since", "", "inclusive lower boundary: YYYY-MM-DD UTC day or RFC3339 timestamp")
	until := fs.String("until", "", "upper boundary: YYYY-MM-DD UTC day (whole day included) or RFC3339 timestamp")
	stage := fs.String("stage", "", "exact lifecycle stage filter (e.g. calm, early_warning)")
	limit := fs.Int("limit", 0, "max rows, newest first (default 50, max 500)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "history" {
		rest = rest[1:]
	}
	if len(rest) != 0 {
		return fail(env, "regime history: usage is `canary regime history [--since YYYY-MM-DD|RFC3339] [--until YYYY-MM-DD|RFC3339] [--stage STAGE] [--limit N] [--json]`")
	}
	params := rpc.RegimeHistoryParams{
		Since: strings.TrimSpace(*since),
		Until: strings.TrimSpace(*until),
		Stage: strings.TrimSpace(*stage),
		Limit: *limit,
	}
	var res rpc.RegimeHistoryResult
	if err := env.Conn.Call(ctx, rpc.MethodRegimeHistory, params, &res); err != nil {
		return fail(env, "regime history: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderRegimeHistoryText(env, env.Stdout, &res)
	return 0
}

// renderRegimeHistoryText prints the newest-first decision table plus the
// index-freshness footer. Verdicts are journal free text — truncated to
// the terminal, never interpreted.
func renderRegimeHistoryText(env *Env, out io.Writer, res *rpc.RegimeHistoryResult) {
	width := outputColumns(out)
	if width < 60 {
		width = 120
	}
	header := fmt.Sprintf("Regime history  %s → %s UTC  %d of %d rows",
		res.Since.UTC().Format("2006-01-02"), res.Until.UTC().Format("2006-01-02"), res.Count, res.TotalCount)
	if res.Truncated {
		header += " (truncated; raise --limit)"
	}
	fmt.Fprintln(out, header)
	if len(res.Entries) == 0 {
		fmt.Fprintln(out, "  no indexed regime decisions in this window")
	} else {
		stageW := 5
		for _, e := range res.Entries {
			stageW = min(max(stageW, len(e.Stage)), 16)
		}
		fmt.Fprintf(out, "  %s\n", env.dim(fmt.Sprintf("%-16s  %-*s  %-6s  %-9s  %s",
			"AT (UTC)", stageW, "STAGE", "SEV", "R/Y(elig)", "VERDICT")))
		verdictW := max(width-(2+16+2+stageW+2+6+2+9+2), 16)
		for _, e := range res.Entries {
			fmt.Fprintf(out, "  %-16s  %-*s  %-6s  %-9s  %s\n",
				e.At.UTC().Format("2006-01-02 15:04"),
				stageW, truncateVisible(e.Stage, stageW),
				truncateVisible(nonEmpty(e.Severity, "-"), 6),
				fmt.Sprintf("%d/%d(%d)", e.ClusterRed, e.ClusterYellow, e.ClusterEligibleRed),
				truncateVisible(e.Verdict, verdictW))
		}
	}
	renderHistoryIndexFooter(env, out, res.Index)
}

// renderHistoryIndexFooter is the shared index-health line for both
// history renderers: a fully-ingested watermark, or a loud byte-backlog
// warning — the index is never presented as silently fresh.
func renderHistoryIndexFooter(env *Env, out io.Writer, idx rpc.HistoryIndexHealth) {
	if behind := idx.JournalBytes - idx.IngestedBytes; behind > 0 {
		fmt.Fprintf(out, "  %s\n", env.yellow(fmt.Sprintf("index catching up: %d bytes behind (rows may be missing)", behind)))
		return
	}
	label := "index: journal fully ingested"
	if !idx.LastIngestAt.IsZero() {
		label = fmt.Sprintf("index: through %s · journal fully ingested", idx.LastIngestAt.UTC().Format("2006-01-02 15:04Z"))
	}
	fmt.Fprintf(out, "  %s\n", env.dim(label))
}

// appendRegimeLog writes one JSONL line to path: an object with the
// current ISO-8601 timestamp and the full regime envelope. Each line
// stands alone; a partial trailing line on crash is salvageable (drop
// the malformed line, the rest of the file parses). The wrap object's
// shape: `{"timestamp": "<RFC 3339>", "regime": {...}}`.
//
// File is opened with O_APPEND|O_CREATE|O_WRONLY at 0o644 — no atomic
// temp+rename needed for an append-only log. Concurrent writers on
// the same file may interleave lines if each line exceeds PIPE_BUF
// (typically 4 KB; a regime envelope is small enough in practice but
// not guaranteed), so don't spawn many parallel calls against the
// same path. For the daily-cadence ritual the spec recommends, that's
// not a concern.
func appendRegimeLog(path string, snap rpc.RegimeSnapshotResult) error {
	envelope := struct {
		Timestamp time.Time                `json:"timestamp"`
		Regime    rpc.RegimeSnapshotResult `json:"regime"`
	}{
		Timestamp: time.Now().UTC(),
		Regime:    snap,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	// One write of <line>\n. POSIX guarantees atomicity up to PIPE_BUF;
	// a typical regime envelope (~2-4 KB) sits well inside that for the
	// daily-cadence ritual, so we don't bother with file locking.
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// startRegimeSpinner writes a single dim status line to stderr-style
// stdout (so it never appears in captured stdout that consumers might
// parse) and animates a braille spinner until the returned stop
// function is called. The clear-line sequence is written one last time
// on stop so the regime renderer's own header lands at column 0 with
// no leftover spinner characters underneath.
//
// Lives in stdout (not stderr) because env.Stdout is what the rest of
// the CLI writes to — keeping motion on the same stream prevents a
// stderr capture (e.g. `bin/canary regime 2>/dev/null`) from swallowing
// the spinner while the final output stays on stdout. The non-TTY
// guard at the call site means a pipe never sees these escapes.
func startRegimeSpinner(env *Env) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	// Braille spinner — narrow, dense, reads as motion at 10 fps.
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	indicators := "VIX VIX3M VVIX HYG SPY OAS funding USD.JPY gamma breadth"
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-stop:
				// Clear the spinner line so the renderer's first
				// fmt.Fprintln(out) blank line lands at col 0.
				fmt.Fprint(env.Stdout, "\r\x1b[K")
				return
			case <-ticker.C:
				fmt.Fprintf(env.Stdout, "\r\x1b[K%s %s",
					env.dim("Fetching regime: "+indicators), frames[i])
				i = (i + 1) % len(frames)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// renderRegimeText is the writer-less entry point preserved for tests
// and the preview tool. The redesign reads to and writes from an
// explicit io.Writer (so --watch can buffer); this wrapper keeps the
// PreviewRenderRegime and test signature stable.
func renderRegimeText(env *Env, r *rpc.RegimeSnapshotResult) int {
	return renderRegimeTextTo(env, env.Stdout, r, false)
}

type regimeRenderOptions struct {
	Explain     bool
	Diagnostics bool
}

// renderRegimeTextTo writes the dashboard to out. Layout: header with
// timestamp; bold composite verdict + ranked-count summary; market tape;
// one-line indicator rows; optional --explain footer with the spec's prose
// per row. Pass explain=true for the verbose mode.
func renderRegimeTextTo(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult, explain bool) int {
	return renderRegimeTextWithOptions(env, out, r, regimeRenderOptions{Explain: explain})
}

func renderRegimeTextWithOptions(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult, opts regimeRenderOptions) int {
	width := outputColumns(out)
	if width == 0 {
		width = 120
	}
	return renderRegimeTextWidthWithOptions(env, out, r, opts, width)
}

func renderRegimeTextWidthWithOptions(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult, opts regimeRenderOptions, width int) int {
	if width < 40 {
		width = 80
	}
	now := r.AsOf
	rows := []regimeRow{
		rowVIXTerm(now, r.VIXTermStructure),
		rowVolOfVol(now, r.VolOfVol),
		rowHYGSPY(now, r.HYGSPYDivergence),
		rowCreditSpreads(now, r.CreditSpreads),
		rowFundingStress(now, r.FundingStress),
		rowUSDJPY(now, r.USDJPY),
		rowGamma(now, r.GammaZero),
		rowBreadth(now, r.Breadth),
	}
	decorateRegimeRowPolicy(rows, r)
	c := tallyCompositeFromSnapshot(r, rows)
	// The monitor projection is the typed cross-surface authority contract for
	// the compact readout. Build it once and let the text renderer consume its
	// whole-snapshot health, lifecycle, and canonical composite instead of
	// inferring those states from otherwise healthy-looking rows.
	monitorSnapshot := *r
	monitor := rpc.CompactRegimeMonitor(&monitorSnapshot)

	// Shared hero: only the title + timestamp live here. The regime
	// renderer keeps its own flow below so the verdict/punch-line block
	// reads first and the SPY/VIX tape lands immediately above the audit
	// rows it anchors.
	renderCommandHero(env, out,
		"Risk Regime",
		r.AsOf.Format("2006-01-02 15:04 MST"),
		"",
		"")
	fmt.Fprintln(out)

	// Hero summary: SERVED regime verdict + usable-signal coverage. The
	// daemon owns the headline words and tone (single wording table in
	// internal/rpc); the renderer-local tally only sizes the coverage
	// counts and layout. Raw band and rankability words stay out of the
	// default readout; the table below is the audit trail and --explain
	// carries the mechanics.
	fmt.Fprintf(out, "  %s %s\n", env.bold(colorRegimeVerdict(env, monitor, c, regimeServedVerdict(monitor, c))), env.dim("· "+c.evidence(monitor.AuthorityHealth)))
	fmt.Fprintln(out)
	renderRegimeSummaryLine(out, "Read:", regimeReadLine(c, monitor), width, nil)
	renderRegimeSummaryLine(out, "Input health:", regimeInputHealthLine(c, rows, monitor), width, regimeInputHealthColor(env, c, rows, monitor))
	if governed := regimeGovernorLine(r.Lifecycle.Governors); governed != "" {
		renderRegimeSummaryLine(out, "Governed:", governed, width, env.yellow)
	}
	if support := regimeSupportLine(rows); support != "" {
		renderRegimeSummaryLine(out, "Support:", support, width, nil)
	}
	if watch := regimeWatchLine(rows); watch != "" {
		renderRegimeSummaryLine(out, "Watch:", watch, width, nil)
	}
	if aside := regimeSetAsideLine(rows); aside != "" {
		renderRegimeSummaryLine(out, "Set aside:", aside, width, env.dim)
	}
	if len(monitor.DataQuality) > 0 {
		if context := regimeDataQualityContext(monitor.DataQuality); context != "" {
			renderRegimeSummaryLine(out, "Data context:", context, width, env.dim)
		}
	}
	fmt.Fprintln(out)

	if headline := renderRegimeHeadline(env, r); headline != "" {
		fmt.Fprintf(out, "  %s\n", headline)
		fmt.Fprintln(out)
	}

	// Header row + horizontal rule give the reader a key for the
	// columns. Dim-colored so the band rows stay visually dominant. The
	// rule width matches the widest row layout (glyph + name + value +
	// band + note), recomputed once here so it tracks layout changes.
	layout := regimeTableLayout(rows, width)
	fmt.Fprintln(out, renderRegimeHeader(env, layout))
	for _, row := range rows {
		for _, line := range renderRow(env, row, layout) {
			fmt.Fprintln(out, line)
		}
	}

	fmt.Fprintln(out)
	if opts.Explain {
		renderExplainBlock(env, out, r, opts.Diagnostics)
		return 0
	}
	for _, line := range wrapVisibleText("Pass --explain for thresholds and reading notes; add --diagnostics for raw source/provenance details.", max(width-2, 24)) {
		fmt.Fprintln(out, env.dim("  "+line))
	}
	return 0
}

func renderRegimeSummaryLine(out io.Writer, label, value string, width int, color func(string) string) {
	prefix := "  " + label + " "
	available := max(width-visibleLen(prefix), 24)
	lines := wrapVisibleText(value, available)
	for i, line := range lines {
		if color != nil {
			line = color(line)
		}
		if i == 0 {
			labelText := label
			if color != nil {
				labelText = color(labelText)
			}
			fmt.Fprintf(out, "  %s %s\n", labelText, line)
			continue
		}
		fmt.Fprintf(out, "%s%s\n", strings.Repeat(" ", visibleLen(prefix)), line)
	}
}

// renderRegimeHeader returns the dim column-header line displayed above
// the indicator rows. Aligned with renderRow's column widths so labels
// land directly over their cells. Reads as a key for the reader without
// needing to consult docs.
type regimeTableWidths struct {
	nameW   int
	valueW  int
	asOfW   int
	bandW   int
	whyW    int
	detailW int
	compact bool
}

func regimeTableLayout(rows []regimeRow, width int) regimeTableWidths {
	w := regimeTableWidths{nameW: 17, valueW: 30, asOfW: 12, bandW: 8}
	for _, row := range rows {
		w.nameW = min(max(w.nameW, visibleLen(row.name)), 18)
		w.valueW = max(w.valueW, visibleLen(rowDisplayValue(row)))
		w.asOfW = min(max(w.asOfW, visibleLen(regimeWhenLabel(row))), 12)
	}
	// Wide mode keeps the familiar SIGNAL / READING / WHEN / CALL / WHY
	// table, but caps READING so WHY can wrap inside the terminal instead
	// of spilling into the shell prompt.
	const minWhyW = 24
	fixedWithoutValue := 2 + 1 + 2 + w.nameW + 2 + 2 + w.asOfW + 2 + w.bandW + 2
	allowedValue := width - fixedWithoutValue - minWhyW
	if width < 96 || allowedValue < 22 {
		w.compact = true
		w.valueW = 0
		w.detailW = max(width-(2+1+2+w.nameW+2+w.bandW+2+w.asOfW+2), 24)
		return w
	}
	w.valueW = min(w.valueW, allowedValue)
	w.whyW = max(width-(2+1+2+w.nameW+2+w.valueW+2+w.asOfW+2+w.bandW+2), minWhyW)
	return w
}

func renderRegimeHeader(env *Env, w regimeTableWidths) string {
	if w.compact {
		header := fmt.Sprintf("     %s  %s  %s  %s",
			padRightVisible("SIGNAL", w.nameW),
			padRightVisible("CALL", w.bandW),
			padRightVisible("WHEN", w.asOfW),
			"READING / WHY")
		return env.dim(header)
	}
	// "  " = 2-space indent + " " = where the glyph sits + 2 spaces.
	header := fmt.Sprintf("     %s  %s  %s  %s  %s",
		padRightVisible("SIGNAL", w.nameW),
		padRightVisible("READING", w.valueW),
		padRightVisible("WHEN", w.asOfW),
		padRightVisible("CALL", w.bandW),
		"WHY")
	return env.dim(header)
}

// renderRow lays out one indicator line: 2-space indent, glyph, indicator
// name (left-padded to 17 — fits the combined-scope "γ-zero (SPY+SPX)"
// label), value cell (left-padded to 30), band word (color, padded to 7
// visible cells), reason (dim parenthetical), optional streak marker
// ("day 3"), optional quality/stale suffix. The band-word color is
// applied AFTER padding so column alignment stays correct under ANSI
// escapes — same trick as the account renderer's padLeftVisible.
func renderRow(env *Env, r regimeRow, w regimeTableWidths) []string {
	value := rowDisplayValue(r)
	callWord, colorFn := r.callLabel()
	callCell := padRightVisible(callWord, w.bandW)
	if colorFn != nil {
		callCell = padRightVisible(colorFn(env, callWord), w.bandW)
	}
	// In default view, NOTE = the reason string verbatim (no surrounding
	// parens, no quality clock). The reader sees a clean column with
	// short interpretive text per indicator. Quality / streak / stale
	// markers are promoted to --explain since they're noise on the
	// default scan and useful only when auditing one indicator's
	// provenance.
	if w.compact {
		detail := value
		if r.reason != "" {
			detail += " — " + r.reason
		}
		lines := wrapVisibleText(detail, w.detailW)
		out := make([]string, 0, len(lines))
		prefix := fmt.Sprintf("  %s  %s  %s  %s  ",
			r.glyph(env),
			padRightVisible(r.name, w.nameW),
			callCell,
			padRightVisible(regimeWhenLabel(r), w.asOfW))
		continuation := strings.Repeat(" ", visibleLen(prefix))
		for i, line := range lines {
			if i == 0 {
				out = append(out, prefix+line)
				continue
			}
			out = append(out, continuation+env.dim(line))
		}
		return out
	}
	valueLines := wrapVisibleText(value, w.valueW)
	whyLines := wrapVisibleText(r.reason, w.whyW)
	lineCount := max(len(valueLines), len(whyLines))
	out := make([]string, 0, lineCount)
	for i := range lineCount {
		valueCell := ""
		if i < len(valueLines) {
			valueCell = valueLines[i]
		}
		whyCell := ""
		if i < len(whyLines) {
			whyCell = env.dim(whyLines[i])
		}
		if i == 0 {
			out = append(out, fmt.Sprintf("  %s  %s  %s  %s  %s  %s",
				r.glyph(env), padRightVisible(r.name, w.nameW), padRightVisible(valueCell, w.valueW),
				padRightVisible(regimeWhenLabel(r), w.asOfW), callCell, whyCell))
			continue
		}
		out = append(out, fmt.Sprintf("     %s  %s  %s  %s  %s",
			strings.Repeat(" ", w.nameW), padRightVisible(valueCell, w.valueW),
			strings.Repeat(" ", w.asOfW), strings.Repeat(" ", w.bandW), whyCell))
	}
	return out
}

func rowDisplayValue(r regimeRow) string {
	value := strings.TrimSpace(r.value)
	if value != "" && value != "—" {
		return r.value
	}
	if r.stateNote != "" {
		return r.stateNote
	}
	if value != "" {
		return r.value
	}
	return "—"
}

func (r regimeRow) glyph(env *Env) string {
	switch r.band {
	case bandGreen:
		return env.green("●")
	case bandYellow:
		return env.yellow("●")
	case bandRed:
		return env.red("●")
	}
	// Unranked — split by status so each mode has its own glyph.
	switch r.status {
	case rpc.RegimeStatusComputing:
		return env.yellow("◌")
	case rpc.RegimeStatusError:
		return env.red("✕")
	default: // unavailable or unknown
		return env.dim("○")
	}
}

// callLabel returns the trader-facing call for the row. The raw
// green/yellow/red/unranked terms are still present in JSON and --explain,
// but the default terminal view reads as calm/watch/stress/no-vote language.
func (r regimeRow) callLabel() (string, func(*Env, string) string) {
	switch r.band {
	case bandGreen:
		return "calm", func(e *Env, s string) string { return e.green(s) }
	case bandYellow:
		return "watch", func(e *Env, s string) string { return e.yellow(s) }
	case bandRed:
		return "stress", func(e *Env, s string) string { return e.red(s) }
	}
	switch r.status {
	case rpc.RegimeStatusComputing:
		return "building", func(e *Env, s string) string { return e.yellow(s) }
	case rpc.RegimeStatusError:
		return "retry", func(e *Env, s string) string { return e.red(s) }
	case rpc.RegimeStatusUnavailable:
		return "skip", nil
	default:
		return "no vote", nil
	}
}

func regimeWhenLabel(row regimeRow) string {
	label := strings.TrimSpace(row.asOf)
	switch strings.ToLower(label) {
	case "":
		label = asOfLabel(nil, row.status)
	case "stale":
		return "stale"
	case "frozen":
		return "frozen"
	case "not_due":
		return "not due"
	case "overdue":
		return "overdue"
	case "computing":
		return "building"
	case "unavailable":
		return "missing"
	}
	if before, ok := strings.CutSuffix(strings.ToLower(label), " delayed"); ok {
		return "delayed " + before
	}
	return ifNonEmpty(label, "—")
}

// ----------------------------------------------------------------------------
// Composite tally + verdict

type regimeComposite struct {
	green, yellow, red int
	ranked, unranked   int
	total              int
	clusterGreen       int
	clusterYellow      int
	clusterRed         int
	clusterRanked      int
	clusterUnranked    int
	clusterTotal       int
}

func tallyComposite(rows []regimeRow) regimeComposite {
	c := regimeComposite{total: len(rows)}
	for _, r := range rows {
		switch r.band {
		case bandGreen:
			c.green++
			c.ranked++
		case bandYellow:
			c.yellow++
			c.ranked++
		case bandRed:
			c.red++
			c.ranked++
		default:
			c.unranked++
		}
	}
	clusters := map[string]regimeBand{}
	for _, r := range rows {
		key := r.cluster
		if key == "" {
			key = r.name
		}
		clusters[key] = worstRegimeBand(clusters[key], r.band)
	}
	c.clusterTotal = len(clusters)
	for _, b := range clusters {
		switch b {
		case bandGreen:
			c.clusterGreen++
			c.clusterRanked++
		case bandYellow:
			c.clusterYellow++
			c.clusterRanked++
		case bandRed:
			c.clusterRed++
			c.clusterRanked++
		default:
			c.clusterUnranked++
		}
	}
	return c
}

func tallyCompositeFromSnapshot(r *rpc.RegimeSnapshotResult, rows []regimeRow) regimeComposite {
	c := tallyComposite(rows)
	if r == nil {
		return c
	}
	rendered := renderedBandSnapshot(*r, rows)
	bands := rpc.BuildRegimeClusterBands(&rendered).Confirmed
	c.clusterGreen = 0
	c.clusterYellow = 0
	c.clusterRed = 0
	c.clusterRanked = 0
	c.clusterUnranked = 0
	c.clusterTotal = len(bands)
	for _, band := range bands {
		switch band {
		case "green":
			c.clusterGreen++
			c.clusterRanked++
		case "yellow":
			c.clusterYellow++
			c.clusterRanked++
		case "red":
			c.clusterRed++
			c.clusterRanked++
		default:
			c.clusterUnranked++
		}
	}
	return c
}

func renderedBandSnapshot(r rpc.RegimeSnapshotResult, rows []regimeRow) rpc.RegimeSnapshotResult {
	for _, row := range rows {
		band := row.band.String()
		switch row.name {
		case "VIX/VIX3M":
			r.VIXTermStructure.Band = band
		case "VVIX":
			r.VolOfVol.Band = band
		case "HYG vs SPY":
			r.HYGSPYDivergence.Band = band
		case "HY/IG OAS":
			r.CreditSpreads.Band = band
		case "funding spread":
			r.FundingStress.Band = band
		case "USD/JPY":
			r.USDJPY.Band = band
		case "SPX breadth":
			r.Breadth.Band = band
		default:
			if strings.Contains(row.name, "γ-zero") {
				r.GammaZero.Band = band
			}
		}
	}
	return r
}

func worstRegimeBand(a, b regimeBand) regimeBand {
	if a == bandRed || b == bandRed {
		return bandRed
	}
	if a == bandYellow || b == bandYellow {
		return bandYellow
	}
	if a == bandGreen || b == bandGreen {
		return bandGreen
	}
	return bandUnranked
}

// verdictFloor is the minimum ranked-row count required for the
// renderer to claim a verdict above "insufficient signal." The spec's
// interpretation table assumes the dashboard is built from multiple
// evidence clusters; below this floor any positive claim ("Normal regime")
// is based on too few measurements to be honest. Three was chosen
// because the spec's yellow- and red-band thresholds both reference
// "majority of indicators" — a verdict needs to see at least majority
// coverage before it stops being a guess.
const verdictFloor = 3

// regimeServedVerdict prefers the daemon's served verdict — the single
// wording table in internal/rpc that posture.label shares — and only falls
// back to the shared function over the renderer-local tally (older daemon /
// synthetic snapshot). The fallback uses the same shared policy function so
// the renderer does not create an independent verdict definition.
func regimeServedVerdict(monitor rpc.RegimeMonitorResult, c regimeComposite) string {
	if reason, impaired := regimeAuthorityIssue(monitor.AuthorityHealth); impaired {
		if !regimeAuthorityCurrent(monitor.AuthorityHealth) {
			return "Market state undefined — " + reason
		}
	}
	verdict := strings.TrimSpace(monitor.Composite.Verdict)
	if verdict == "" {
		// No eligibility context exists client-side, so fallback reds count as
		// provisional — the label degrades gracefully to "Stress signal
		// present" rather than claiming confirmation the renderer can't verify.
		comp := rpc.RegimeComposite{
			ClusterGreenCount:          c.clusterGreen,
			ClusterYellowCount:         c.clusterYellow,
			ClusterRedCount:            c.clusterRed,
			ClusterProvisionalRedCount: c.clusterRed,
			ClusterRankedCount:         c.clusterRanked,
			ClusterUnrankedCount:       c.clusterUnranked,
		}
		verdict = rpc.RegimeHeadline(comp, monitor.Lifecycle.Stage)
	}
	if reason, impaired := regimeAuthorityIssue(monitor.AuthorityHealth); impaired {
		return verdict + " — refresh degraded: " + reason
	}
	return verdict
}

// colorRegimeVerdict colors the headline from the SERVED posture tone so
// the terminal, SPA, and MCP read the same color policy; the local-tally
// heuristic only covers snapshots without a posture.
func colorRegimeVerdict(env *Env, monitor rpc.RegimeMonitorResult, c regimeComposite, label string) string {
	if _, impaired := regimeAuthorityIssue(monitor.AuthorityHealth); impaired {
		if !regimeAuthorityCurrent(monitor.AuthorityHealth) {
			return env.red(label)
		}
	}
	switch strings.TrimSpace(monitor.Posture.Tone) {
	case rpc.RegimeToneStress, rpc.RegimeToneRiskOff:
		return env.red(label)
	case rpc.RegimeToneWatch, rpc.RegimeToneDataQuality:
		return env.yellow(label)
	case rpc.RegimeToneNormal:
		return env.green(label)
	default:
		return colorRegimeLabel(env, c, label)
	}
}

// regimeGovernorLine renders the lifecycle's severity-governor disclosures:
// when policy withheld a severity rung, the reader sees what was capped and
// why instead of wondering why two red rows read "watch".
func regimeGovernorLine(govs []rpc.GovernorAction) string {
	if len(govs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(govs))
	for _, g := range govs {
		reason := g.Reason
		switch g.Reason {
		case "pending_backtest_no_tape_cosign":
			reason = "thresholds pending backtest, no tape co-sign"
		case "confirming_cluster_quality":
			reason = "confirming-cluster data quality impaired"
		}
		part := fmt.Sprintf("severity %s (capped from %s) — %s", g.To, g.From, reason)
		if len(g.Clusters) > 0 {
			part += " [" + strings.Join(g.Clusters, ", ") + "]"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// decorateRegimeRowPolicy reconciles the renderer-local row classification
// with the served confirmation policy: a daemon-side hysteresis hold keeps
// the row red, and a red whose evidence failed the eligibility gates is
// marked provisional so the reader knows it cannot confirm. Whether it can
// contribute to early_warning depends on the typed lifecycle's required-input
// health; broken or overdue evidence produces data_quality instead.
func decorateRegimeRowPolicy(rows []regimeRow, r *rpc.RegimeSnapshotResult) {
	metas := []rpc.RegimeIndicatorMeta{
		r.VIXTermStructure.RegimeIndicatorMeta,
		r.VolOfVol.RegimeIndicatorMeta,
		r.HYGSPYDivergence.RegimeIndicatorMeta,
		r.CreditSpreads.RegimeIndicatorMeta,
		r.FundingStress.RegimeIndicatorMeta,
		r.USDJPY.RegimeIndicatorMeta,
		r.GammaZero.RegimeIndicatorMeta,
		r.Breadth.RegimeIndicatorMeta,
	}
	for i := range rows {
		if i >= len(metas) {
			return
		}
		meta := metas[i]
		if meta.Freshness != nil {
			switch meta.Freshness.Class {
			case rpc.RegimeFreshnessNotDue:
				rows[i].asOf = "not_due"
			case rpc.RegimeFreshnessPending:
				rows[i].asOf = "pending"
			case rpc.RegimeFreshnessStale:
				rows[i].asOf = "stale"
			case rpc.RegimeFreshnessOverdue:
				rows[i].asOf = "overdue"
			}
		}
		if meta.Band != "red" {
			continue
		}
		if rows[i].band != bandRed && rows[i].band != bandUnranked {
			// Served hysteresis hold: the raw value left the red band but
			// the exit threshold has not cleared daemon-side.
			rows[i].band = bandRed
			if meta.BandReason != "" {
				rows[i].reason = meta.BandReason
			}
		}
		if rows[i].band == bandRed && meta.Eligibility != nil && !meta.Eligibility.Eligible {
			rows[i].reason = strings.TrimSpace(rows[i].reason + " · provisional (" + regimeEligibilityReasonText(meta.Eligibility.Reasons) + ")")
		}
	}
}

func regimeEligibilityReasonText(reasons []string) string {
	if len(reasons) == 0 {
		return "not confirmation-eligible"
	}
	out := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		switch {
		case reason == "depth_below_min":
			out = append(out, "depth below floor")
		case reason == "data_overdue":
			out = append(out, "data overdue")
		case reason == "data_not_due":
			out = append(out, "new session data not due yet")
		case reason == "data_refresh_pending":
			out = append(out, "current-period refresh still running")
		case reason == "data_stale":
			out = append(out, "carried within tolerance, not confirmable")
		case strings.HasPrefix(reason, "streak_"):
			if parts := strings.Split(reason, "_"); len(parts) == 4 {
				out = append(out, "day "+parts[1]+" of "+parts[3])
			} else {
				out = append(out, reason)
			}
		default:
			out = append(out, reason)
		}
	}
	return strings.Join(out, ", ")
}

func (c regimeComposite) evidence(authority *rpc.RegimeAuthorityHealth) string {
	var parts []string
	if c.clusterGreen > 0 {
		parts = append(parts, fmt.Sprintf("%d calm", c.clusterGreen))
	}
	if c.clusterYellow > 0 {
		parts = append(parts, fmt.Sprintf("%d watch", c.clusterYellow))
	}
	if c.clusterRed > 0 {
		parts = append(parts, fmt.Sprintf("%d stress", c.clusterRed))
	}
	if c.clusterUnranked > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting", c.clusterUnranked))
	}
	coverageWord := "usable"
	if !regimeAuthorityCurrent(authority) {
		coverageWord = "retained"
	}
	coverage := fmt.Sprintf("%d/%d evidence groups %s", c.clusterRanked, c.clusterTotal, coverageWord)
	if len(parts) == 0 {
		return coverage
	}
	return coverage + " · " + strings.Join(parts, " / ")
}

func colorRegimeLabel(env *Env, c regimeComposite, label string) string {
	switch {
	case c.clusterRed > 0 || c.clusterRanked == c.clusterTotal && c.clusterRed == c.clusterTotal:
		return env.red(label)
	case c.clusterYellow >= 3 || c.clusterRanked < verdictFloor:
		return env.yellow(label)
	default:
		return env.green(label)
	}
}

func regimeReadLine(c regimeComposite, monitor rpc.RegimeMonitorResult) string {
	coverage := fmt.Sprintf("%d/%d evidence groups usable", c.clusterRanked, c.clusterTotal)
	stage := strings.ToLower(strings.TrimSpace(monitor.Lifecycle.Stage))
	if reason, impaired := regimeAuthorityIssue(monitor.AuthorityHealth); impaired {
		if regimeAuthorityCurrent(monitor.AuthorityHealth) {
			if regimeLifecycleStageConfirmsStress(stage) {
				return "The current retained read still confirms stress, but " + reason + "."
			}
			return "The current retained Regime read remains usable, but " + reason + "."
		}
		return "No current regime read — " + reason + "; embedded rows are retained context only."
	}
	eligibleRed := monitor.Composite.ClusterEligibleRedCount
	provisionalRed := monitor.Composite.ClusterProvisionalRedCount
	switch {
	case stage == rpc.LifecycleDataQuality:
		return "Market state is undefined — required evidence is incomplete; provisional stress remains context only."
	case c.clusterRanked == 0:
		return "No regime read yet — every evidence group is still building or missing."
	case c.clusterRanked < verdictFloor:
		return "Thin read — " + coverage + "; treat the regime label as context until more inputs load."
	case stage == rpc.LifecyclePanic && eligibleRed >= 3:
		return "Broad stress is confirmed across independent groups."
	case regimeLifecycleStageConfirmsStress(stage):
		return "Current usable evidence confirms stress."
	case eligibleRed+provisionalRed > 0 || monitor.Composite.ClusterRedCount > 0 || c.clusterRed > 0:
		return "Stress evidence is visible, but current usable evidence has not confirmed broad stress."
	case c.clusterYellow > 0:
		return "Watch conditions are visible; not a confirmed broad stress regime."
	default:
		return "Constructive tape across the loaded evidence groups."
	}
}

func regimeInputHealthLine(c regimeComposite, rows []regimeRow, monitor rpc.RegimeMonitorResult) string {
	coverage := fmt.Sprintf("%d/%d evidence groups loaded", c.clusterRanked, c.clusterTotal)
	qualityProblems := regimeInputHealthDataQualityProblems(monitor.DataQuality)
	if reason, impaired := regimeAuthorityIssue(monitor.AuthorityHealth); impaired {
		if regimeAuthorityCurrent(monitor.AuthorityHealth) {
			return "Degraded — " + reason + "; " + coverage + " from the current retained snapshot."
		}
		return "Blocked — " + reason + "; embedded evidence is retained context only."
	}
	if strings.EqualFold(strings.TrimSpace(monitor.Lifecycle.Stage), rpc.LifecycleDataQuality) {
		return "Blocked — required evidence is incomplete, so the market state is undefined."
	}
	staleRows := regimeRowsByWhen(rows, "stale", "overdue")
	switch {
	case c.clusterRanked == 0:
		return "Blocked — no evidence group has loaded."
	case c.clusterRanked < verdictFloor:
		return "Degraded — " + coverage + "; wait for more inputs before trusting the label."
	case len(regimeRowsByStatuses(rows, rpc.RegimeStatusError, rpc.RegimeStatusUnavailable)) > 0:
		return "Needs confirmation — " + coverage + "; " + joinHumanList(regimeRowDisplayNames(regimeRowsByStatuses(rows, rpc.RegimeStatusError, rpc.RegimeStatusUnavailable))) + " did not load."
	case len(regimeRowsByStatuses(rows, rpc.RegimeStatusComputing)) > 0:
		return "Warming — " + coverage + "; " + joinHumanList(regimeRowDisplayNames(regimeRowsByStatuses(rows, rpc.RegimeStatusComputing))) + " still building."
	case len(staleRows) > 0:
		names := regimeRowDisplayNames(staleRows)
		return "Needs confirmation — " + coverage + "; " + joinHumanList(names) + " " + areOrIs(names) + " stale or overdue."
	case len(qualityProblems) > 0:
		return "Needs confirmation — " + coverage + "; " + joinHumanList(qualityProblems) + " need confirmation."
	default:
		var context []string
		if monitor.AuthorityHealth != nil && monitor.AuthorityHealth.Refreshing {
			context = append(context, "the current snapshot remains usable while a Regime refresh runs")
		}
		if len(regimeRowsByWhen(rows, "frozen")) > 0 {
			context = append(context, "frozen rows are closed-session context")
		}
		if len(regimeRowsByWhen(rows, "not due")) > 0 {
			context = append(context, "scheduled inputs are not due yet")
		}
		if len(context) == 0 {
			return "OK — " + coverage + "."
		}
		return "OK — " + coverage + "; " + strings.Join(context, "; ") + "."
	}
}

func regimeInputHealthColor(env *Env, c regimeComposite, rows []regimeRow, monitor rpc.RegimeMonitorResult) func(string) string {
	if _, impaired := regimeAuthorityIssue(monitor.AuthorityHealth); impaired {
		if regimeAuthorityCurrent(monitor.AuthorityHealth) {
			return env.yellow
		}
		return env.red
	}
	switch {
	case c.clusterRanked == 0 || c.clusterRanked < verdictFloor || strings.EqualFold(strings.TrimSpace(monitor.Lifecycle.Stage), rpc.LifecycleDataQuality):
		return env.red
	case len(regimeRowsByStatuses(rows, rpc.RegimeStatusError, rpc.RegimeStatusUnavailable, rpc.RegimeStatusComputing)) > 0 || len(regimeRowsByWhen(rows, "stale", "overdue")) > 0 || len(regimeInputHealthDataQualityProblems(monitor.DataQuality)) > 0:
		return env.yellow
	default:
		return env.green
	}
}

func regimeInputHealthDataQualityProblems(items []rpc.DataQualityHealth) []string {
	var out []string
	for _, item := range items {
		switch strings.ToLower(strings.TrimSpace(item.Status)) {
		case "stale":
			if strings.EqualFold(strings.TrimSpace(item.CadenceState), rpc.DataCadenceNotDue) {
				continue
			}
			for _, cluster := range item.StaleClusters {
				out = appendUniqueString(out, strings.TrimSpace(cluster))
			}
		case "partial":
			for _, cluster := range item.PartialClusters {
				out = appendUniqueString(out, strings.TrimSpace(cluster))
			}
		case "degraded":
			for _, cluster := range item.DegradedClusters {
				out = appendUniqueString(out, strings.TrimSpace(cluster))
			}
		}
	}
	return out
}

func regimeSupportLine(rows []regimeRow) string {
	names := regimeNamesByBand(rows, bandGreen)
	if len(names) == 0 {
		return ""
	}
	verb := "is"
	if len(names) > 1 {
		verb = "are"
	}
	return fmt.Sprintf("%s %s calm.", joinHumanList(names), verb)
}

func regimeWatchLine(rows []regimeRow) string {
	var parts []string
	if names := regimeNamesByBand(rows, bandRed); len(names) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s showing stress%s",
			joinHumanList(names), areOrIs(names), regimeBandContextSuffix(rows, bandRed)))
	}
	if names := regimeNamesByBand(rows, bandYellow); len(names) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s on watch%s",
			joinHumanList(names), areOrIs(names), regimeBandContextSuffix(rows, bandYellow)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + "."
}

func regimeSetAsideLine(rows []regimeRow) string {
	var building, missing []string
	type noVoteRow struct {
		name   string
		reason string
	}
	var noVotes []noVoteRow
	for _, row := range rows {
		if row.band != bandUnranked {
			continue
		}
		name := regimeRowDisplayName(row)
		switch row.status {
		case rpc.RegimeStatusComputing:
			building = append(building, name)
		case rpc.RegimeStatusError, rpc.RegimeStatusUnavailable:
			missing = append(missing, name)
		default:
			noVotes = append(noVotes, noVoteRow{name: name, reason: row.reason})
		}
	}
	var parts []string
	if len(building) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s still building", joinHumanList(building), areOrIs(building)))
	}
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s not in this read yet", joinHumanList(missing), areOrIs(missing)))
	}
	switch len(noVotes) {
	case 0:
	case 1:
		part := noVotes[0].name + " did not vote"
		if noVotes[0].reason != "" {
			part += ": " + noVotes[0].reason
		}
		parts = append(parts, part)
	default:
		names := make([]string, 0, len(noVotes))
		for _, row := range noVotes {
			names = append(names, row.name)
		}
		parts = append(parts, fmt.Sprintf("%s did not vote", joinHumanList(names)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + "."
}

func regimeNamesByBand(rows []regimeRow, band regimeBand) []string {
	var names []string
	for _, row := range rows {
		if row.band == band {
			names = append(names, regimeRowDisplayName(row))
		}
	}
	return names
}

func regimeRowsByStatuses(rows []regimeRow, statuses ...string) []regimeRow {
	if len(statuses) == 0 {
		return nil
	}
	statusSet := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		statusSet[status] = true
	}
	var out []regimeRow
	for _, row := range rows {
		if statusSet[row.status] {
			out = append(out, row)
		}
	}
	return out
}

func regimeRowsByWhen(rows []regimeRow, labels ...string) []regimeRow {
	if len(labels) == 0 {
		return nil
	}
	labelSet := make(map[string]bool, len(labels))
	for _, label := range labels {
		labelSet[strings.ToLower(strings.TrimSpace(label))] = true
	}
	var out []regimeRow
	for _, row := range rows {
		if labelSet[strings.ToLower(regimeWhenLabel(row))] {
			out = append(out, row)
		}
	}
	return out
}

func regimeRowDisplayNames(rows []regimeRow) []string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, regimeRowDisplayName(row))
	}
	return names
}

func regimeBandContextSuffix(rows []regimeRow, band regimeBand) string {
	for _, row := range rows {
		if row.band != band {
			continue
		}
		switch when := regimeWhenLabel(row); {
		case when == "stale":
			return " from stale data"
		case when == "overdue":
			return " from overdue data"
		case when == "frozen":
			return " from frozen market context"
		case when == "not due":
			return " from not-due context"
		case strings.HasPrefix(when, "cached"):
			return " from cached data"
		}
	}
	return ""
}

func areOrIs(parts []string) string {
	if len(parts) == 1 {
		return "is"
	}
	return "are"
}

func regimeDataQualityContext(items []rpc.DataQualityHealth) string {
	value := formatDataQualityValue(items)
	value = strings.ReplaceAll(value, "degraded", "context")
	return value
}

func regimeLifecycleStageConfirmsStress(stage string) bool {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case rpc.LifecycleConfirmedStress, rpc.LifecyclePanic:
		return true
	default:
		return false
	}
}

// regimeAuthorityIssue interprets only the typed whole-snapshot contract. A
// fresh last-good remains usable while a refresh runs. Every stale,
// unavailable, failed, or invalid authority state blocks an all-clear claim,
// even when all embedded indicator rows still look healthy.
func regimeAuthorityIssue(health *rpc.RegimeAuthorityHealth) (string, bool) {
	if health == nil {
		return "Regime snapshot health is unavailable", true
	}
	if err := rpc.ValidateRegimeAuthorityHealth(*health); err != nil {
		return "Regime snapshot health metadata is invalid", true
	}
	var parts []string
	switch health.Status {
	case rpc.RegimeAuthorityFresh:
	case rpc.RegimeAuthorityStale:
		parts = append(parts, "whole snapshot is stale")
	case rpc.RegimeAuthorityUnavailable:
		parts = append(parts, "no complete retained snapshot is available")
	default:
		parts = append(parts, "Regime snapshot health state is invalid")
	}
	if failure := regimeAuthorityFailureText(health.FailureCode); failure != "" && !(health.Status == rpc.RegimeAuthorityUnavailable && health.FailureCode == rpc.RegimeAuthorityFailureNoLastGood) {
		parts = append(parts, failure)
	}
	if health.Refreshing && health.Status != rpc.RegimeAuthorityFresh {
		parts = append(parts, "refresh in progress")
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "; "), true
}

func regimeAuthorityCurrent(health *rpc.RegimeAuthorityHealth) bool {
	return health != nil && rpc.ValidateRegimeAuthorityHealth(*health) == nil && health.Status == rpc.RegimeAuthorityFresh
}

func regimeAuthorityFailureText(code rpc.RegimeAuthorityFailureCode) string {
	switch code {
	case rpc.RegimeAuthorityFailureNone:
		return ""
	case rpc.RegimeAuthorityFailureNoLastGood:
		return "no complete Regime snapshot has been accepted"
	case rpc.RegimeAuthorityFailureRefreshTimeout:
		return "latest Regime refresh timed out"
	case rpc.RegimeAuthorityFailureRefreshIncomplete:
		return "latest Regime refresh was incomplete"
	case rpc.RegimeAuthorityFailureRefreshFailed:
		return "latest Regime refresh failed"
	case rpc.RegimeAuthorityFailurePublishFailed:
		return "latest Regime refresh could not be published"
	case rpc.RegimeAuthorityFailureInvalidPersistedState:
		return "saved Regime snapshot is invalid"
	case rpc.RegimeAuthorityFailureClockInvalid:
		return "daemon clock is behind the last committed snapshot"
	default:
		return "Regime snapshot health reported an unknown failure"
	}
}

func regimeEvidenceName(name string) string {
	switch {
	case strings.HasPrefix(name, "VIX/"):
		return "volatility term structure"
	case name == "VVIX":
		return "vol-of-vol"
	case strings.HasPrefix(name, "HYG"):
		return "ETF credit proxy"
	case strings.Contains(name, "OAS"):
		return "cash credit spreads"
	case strings.Contains(name, "funding"):
		return "funding spread"
	case strings.HasPrefix(name, "USD"):
		return "FX carry proxy"
	case strings.Contains(name, "γ-zero"):
		return "dealer gamma"
	case strings.Contains(name, "breadth"):
		return "breadth"
	default:
		return name
	}
}

func regimeRowDisplayName(row regimeRow) string {
	evidence := regimeEvidenceName(row.name)
	if evidence == row.name {
		return row.name
	}
	return row.name + " (" + evidence + ")"
}

func joinHumanList(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}

// renderRegimeHeadline returns the SPY + VIX tape line shown immediately
// above the indicator rows: spot price + day's dollar / percent change
// for SPY, spot + percent change for VIX (which has no shares so dollar
// change is meaningless). Each side renders only when its primary price
// arrived; either half can be missing without dropping the line. Returns
// "" when neither half has data.
//
// Color convention:
//   - SPY change green when positive, red when negative — the reader's
//     account is long SPY-correlated by default.
//   - VIX change red when positive, green when negative — vol expanding
//     is risk-off. Inverted on purpose; matches every trading desk's
//     intuition.
//
// The change cells fall back to "—" (dim) when the prev-close anchor
// didn't arrive, so the reader sees the price even without the delta.
func renderRegimeHeadline(env *Env, r *rpc.RegimeSnapshotResult) string {
	var parts []string
	if cell := spyHeadlineCell(env, r.HYGSPYDivergence); cell != "" {
		parts = append(parts, cell)
	}
	if cell := vixHeadlineCell(env, r.VIXTermStructure); cell != "" {
		parts = append(parts, cell)
	}
	return strings.Join(parts, "    ")
}

func spyHeadlineCell(env *Env, r rpc.RegimeHYGSPYDivergence) string {
	if r.SPYPrice == nil {
		return ""
	}
	price := fmt.Sprintf("SPY %.2f", *r.SPYPrice)
	if r.SPYChange == nil || r.SPYChangePct == nil {
		return price + "  " + env.dim("(—)")
	}
	dollar := signedFloat(*r.SPYChange, 2)
	pct := signedFloat(*r.SPYChangePct, 2) + "%"
	color := env.green
	if *r.SPYChange < 0 {
		color = env.red
	}
	return price + "  " + color(dollar) + "  " + color("("+pct+")")
}

func vixHeadlineCell(env *Env, r rpc.RegimeVIXTerm) string {
	if r.VIX == nil {
		return ""
	}
	price := fmt.Sprintf("VIX %.2f", *r.VIX)
	if r.VIXChangePct == nil {
		return price + "  " + env.dim("(—)")
	}
	pct := signedFloat(*r.VIXChangePct, 2) + "%"
	// Inverted: vol UP is risk-off (red), vol DOWN is constructive
	// (green). Flat (== 0) reads as no-change; dim is the honest call.
	color := env.dim
	switch {
	case *r.VIXChangePct > 0:
		color = env.red
	case *r.VIXChangePct < 0:
		color = env.green
	}
	return price + "  " + color("("+pct+")")
}

// signedFloat formats v with a leading sign and N decimals. Used by
// the headline cells so "+1.20" reads symmetrically with "−1.20".
// The Unicode minus (U+2212) is intentional — it's the same width as
// the plus sign, keeping the alignment clean.
func signedFloat(v float64, decimals int) string {
	sign := "+"
	if v < 0 {
		sign = "−"
		v = -v
	}
	return fmt.Sprintf("%s%.*f", sign, decimals, v)
}

// ----------------------------------------------------------------------------
// Per-indicator row builders. Each one consumes a raw RPC row and
// emits the (name, value, band, reason, status) tuple the renderer
// lays out. Threshold derivation lives here, with the spec defaults
// from the top of the file.

type regimeExplainQuality struct {
	name string
	q    *rpc.Quality
}

type regimeExplainEntry struct {
	name       string
	thresholds *rpc.RegimeThresholds
	band       string
	bandReason string
	inputs     string
	source     string
	missing    []string
	quals      []regimeExplainQuality
}

func renderExplainBlock(env *Env, out io.Writer, r *rpc.RegimeSnapshotResult, diagnostics bool) {
	entries := []regimeExplainEntry{
		{"VIX/VIX3M", r.VIXTermStructure.Thresholds, r.VIXTermStructure.Band, r.VIXTermStructure.BandReason,
			"VIX is Cboe's 30-day S&P 500 implied-volatility index; VIX3M is the roughly three-month version.",
			"IBKR index market data for Cboe VIX and VIX3M in session; off-window VIX3M is Cboe's official dated close, which also cross-checks the broker leg. Historical replay uses Cboe official CSVs.",
			r.VIXTermStructure.FieldsMissing, []regimeExplainQuality{
				{"VIX", r.VIXTermStructure.VIXQuality},
				{"VIX3M", r.VIXTermStructure.VIX3MQuality},
			}},
		{"VVIX", r.VolOfVol.Thresholds, r.VolOfVol.Band, r.VolOfVol.BandReason,
			"VVIX is Cboe's VIX-of-VIX index: expected volatility of VIX itself.",
			"Cboe official daily VVIX time series.",
			nil, []regimeExplainQuality{
				{"VVIX", r.VolOfVol.ValueQuality},
			}},
		{"HYG vs SPY", r.HYGSPYDivergence.Thresholds, r.HYGSPYDivergence.Band, r.HYGSPYDivergence.BandReason,
			"HYG is a high-yield corporate bond ETF; SPY is the large S&P 500 ETF used as the stock-market comparison.",
			"IBKR HYG/SPY quotes plus HMDS daily bars; SPY 52-week high uses Misc Stats tick 165 or daily-bar fallback.",
			r.HYGSPYDivergence.FieldsMissing, []regimeExplainQuality{
				{"HYG", r.HYGSPYDivergence.HYGQuality},
				{"HYG_50DMA", r.HYGSPYDivergence.HYG50DMAQuality},
				{"SPY", r.HYGSPYDivergence.SPYQuality},
				{"SPY_52w_high", r.HYGSPYDivergence.SPY52WHighQuality},
			}},
		{"HY/IG OAS", r.CreditSpreads.Thresholds, r.CreditSpreads.Band, r.CreditSpreads.BandReason,
			"HY OAS is high-yield corporate spread; IG OAS is investment-grade corporate spread. OAS is extra yield over Treasuries after option adjustment.",
			"FRED/St. Louis Fed CSVs for ICE BofA series BAMLH0A0HYM2 and BAMLC0A0CM.",
			r.CreditSpreads.FieldsMissing, []regimeExplainQuality{
				{"HY_OAS", r.CreditSpreads.HYOASQuality},
				{"IG_OAS", r.CreditSpreads.IGOASQuality},
				{"HY-IG_spread", r.CreditSpreads.SpreadQuality},
			}},
		{"funding spread", r.FundingStress.Thresholds, r.FundingStress.Band, r.FundingStress.BandReason,
			"90-day AA financial commercial paper is short-term financial-company borrowing; 3-month T-bills are short-term U.S. Treasury borrowing.",
			"Federal Reserve Commercial Paper DDP RIFSPPFAAD90_N.B and U.S. Treasury Daily Treasury Bill Rates 13-week bank discount.",
			r.FundingStress.FieldsMissing, []regimeExplainQuality{
				{"CP_3m", r.FundingStress.CP3MQuality},
				{"TBill_3m", r.FundingStress.TBill3MQuality},
				{"CP-TBill", r.FundingStress.SpreadQuality},
			}},
		{"USD/JPY", r.USDJPY.Thresholds, r.USDJPY.Band, r.USDJPY.BandReason,
			"USD/JPY is yen per U.S. dollar; a falling pair means yen strengthening, the carry-unwind direction.",
			"IBKR CASH/IDEALPRO USD.JPY tick plus HMDS midpoint bars; Tier 1 replay uses FRED DEXJPUS.",
			r.USDJPY.FieldsMissing, []regimeExplainQuality{
				{"Last", r.USDJPY.LastQuality},
				{"Close_7d_ago", r.USDJPY.Close7DAgoQuality},
			}},
		{gammaRowLabel(r.GammaZero), r.GammaZero.Thresholds, r.GammaZero.Band, r.GammaZero.BandReason,
			"SPY is the S&P 500 ETF; SPX is the S&P 500 index. Their option books are read separately and combined.",
			"IBKR SPY/SPX option chains, open interest, option quotes/model ticks, and the daemon's gamma cache.",
			r.GammaZero.FieldsMissing, []regimeExplainQuality{
				{"Zero_gamma", r.GammaZero.ZeroGammaQuality},
				{"|Gamma|.OI_sum", r.GammaZero.GammaTotalAbsQuality},
			}},
		{"SPX breadth", r.Breadth.Thresholds, r.Breadth.Band, r.Breadth.BandReason,
			"No single live symbol is used; this counts individual S&P 500 member stocks above their own moving averages.",
			"Local daemon compute from IBKR HMDS constituent daily bars and the generated S&P 500 membership list.",
			r.Breadth.FieldsMissing, []regimeExplainQuality{
				{"Value", r.Breadth.ValueQuality},
			}},
	}
	fmt.Fprintf(out, "  %s\n", env.bold("Explain"))
	if diagnostics && r.SpecDoc != "" {
		renderExplainKV(env, out, "Full methodology", r.SpecDoc, nil)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s\n", env.bold("Confirmation policy"))
	renderExplainKV(env, out, "Rule", "A red row may CONFIRM stress only when it is deep, persistent, and cadence-fresh; otherwise it is provisional — visible, but never confirms or rescues another cluster. A provisional red contributes to early_warning only while required inputs are current and usable; overdue or broken required evidence produces data_quality instead. While threshold sets carry pending_backtest, severity \"act\" additionally needs a tape co-sign (SPY ≤ −1.5%, VIX +10%, or a fresh same-session term inversion); pure-tape panic (SPY ≤ −4%/−7%) is exempt. Governed downgrades are disclosed on the Governed line.", nil)
	renderExplainKV(env, out, "Gates", regimeGateExplainLine(), nil)
	fmt.Fprintln(out)
	for _, e := range entries {
		fmt.Fprintf(out, "  %s\n", env.bold(e.name))
		if bandLine := explainBandLine(e.band, e.bandReason); bandLine != "" {
			renderExplainKV(env, out, "Band", bandLine, styleRegimeExplainBand(env, e.band))
		}
		if thresholdLine := explainThresholdLine(e.thresholds); thresholdLine != "" {
			renderExplainKV(env, out, "Bands", thresholdLine, nil)
		}
		if qualityLine := explainQualityLine(r.AsOf, e.quals, diagnostics); qualityLine != "" {
			renderExplainKV(env, out, "Quality", qualityLine, nil)
		}
		if source := explainSourceSummary(e.name); source != "" {
			renderExplainKV(env, out, "Source", source, nil)
		}
		// The gamma row gets extra surfaces specific to its modelled
		// output: data-quality notes, BS-IV fallback disclosure, and a
		// plain-English read of what γ-zero means.
		if e.name == gammaRowLabel(r.GammaZero) {
			if note := regimeGammaDataNote(r.GammaZero.Envelope.Result, diagnostics); note != "" {
				renderExplainKV(env, out, "Data note", note, nil)
			}
			if res := r.GammaZero.Envelope.Result; res != nil && res.DerivedIVLegs > 0 {
				denom := res.PricedLegCount
				if denom == 0 {
					denom = res.LegCount
				}
				renderExplainKV(env, out, "Model note", fmt.Sprintf(
					"%d/%d priced legs used derived IV from option quote/close fallback%s.",
					res.DerivedIVLegs, denom, gammaIVSourceSplitSuffix(res)), nil)
			}
		}
		if read := explainReadNote(e.name); read != "" {
			renderExplainKV(env, out, "Read", read, nil)
		}
		if diagnostics {
			if e.inputs != "" {
				renderExplainKV(env, out, "Inputs", e.inputs, nil)
			}
			if e.source != "" {
				renderExplainKV(env, out, "Raw source", e.source, nil)
			}
			if len(e.missing) > 0 {
				renderExplainKV(env, out, "Missing", strings.Join(e.missing, ", "), env.yellow)
			}
		}
		fmt.Fprintln(out)
	}
}

// regimeGateExplainLine renders the eligibility gate table from the shared
// rpc policy constants, so the explain text can never drift from the values
// the daemon enforces.
func regimeGateExplainLine() string {
	g := func(key string) rpc.RegimeGate {
		gate, _ := rpc.RegimeGateFor(key)
		return gate
	}
	vix := g(rpc.RegimeIndicatorVIXTerm)
	vvix := g(rpc.RegimeIndicatorVolOfVol)
	hyg := g(rpc.RegimeIndicatorHYGSPY)
	gamma := g(rpc.RegimeIndicatorGammaZero)
	breadth := g(rpc.RegimeIndicatorBreadth)
	return fmt.Sprintf(
		"VIX/VIX3M ratio ≥%.2f ×%d sessions (fast ≥%.2f); VVIX ≥%.0f ×%d (fast ≥%.0f); HYG ≥%.2f%% below 50DMA ×%d (fast ≥%.1f%%); gamma gap ≤−%.1f%% below γ-zero, the gamma-weighted SPY/SPX gap on the combined row (wholly-short profile = fast path); breadth ≤%.0f%% ×%d (fast ≤%.0f%%); HY OAS, funding, USD/JPY: the red band itself is the gate, 1 session.",
		vix.MinDepth, vix.MinSessions, vix.FastDepth,
		vvix.MinDepth, vvix.MinSessions, vvix.FastDepth,
		hyg.MinDepth, hyg.MinSessions, hyg.FastDepth,
		gamma.MinDepth,
		40-breadth.MinDepth, breadth.MinSessions, 40-breadth.FastDepth,
	)
}

func explainSourceSummary(name string) string {
	switch name {
	case "VIX/VIX3M":
		return "IBKR Cboe index market data in session, Cboe's official dated VIX3M close off-window; historical replay uses official Cboe files."
	case "VVIX":
		return "Cboe official daily VVIX time series."
	case "HYG vs SPY":
		return "IBKR HYG/SPY quotes plus IBKR daily bars for trend and high anchors."
	case "HY/IG OAS":
		return "FRED/St. Louis Fed official daily ICE BofA high-yield and investment-grade OAS series."
	case "funding spread":
		return "Federal Reserve commercial-paper release plus U.S. Treasury Daily Treasury Bill Rates."
	case "USD/JPY":
		return "IBKR IDEALPRO USD/JPY quote plus IBKR midpoint bars for the weekly move."
	case "SPX breadth":
		return "Local daemon breadth cache computed from IBKR daily bars for S&P 500 members."
	}
	if strings.Contains(name, "γ-zero") {
		return "IBKR SPY/SPX option chains, open interest, option quotes/model ticks, and the daemon gamma cache."
	}
	return ""
}

func regimeGammaDataNote(c *rpc.GammaZeroComputed, diagnostics bool) string {
	details := gammaWarningDetailsForRender(c)
	for _, prefix := range []string{"spx_cache_fallback", "spx_unavailable:"} {
		for _, d := range details {
			if strings.HasPrefix(d.Code, prefix) {
				return formatRegimeGammaDataNote(d, diagnostics)
			}
		}
	}
	for _, d := range details {
		if d.Code == "cache_stale_off_hours" || d.Code == "all_iv_derived" {
			return formatRegimeGammaDataNote(d, diagnostics)
		}
	}
	return ""
}

func formatRegimeGammaDataNote(d rpc.GammaWarningDetail, diagnostics bool) string {
	code := strings.ToLower(strings.TrimSpace(d.Code))
	if !diagnostics {
		switch {
		case strings.HasPrefix(code, "spx_cache_fallback"):
			return gammaSPXCacheFallbackContextLine(time.Now())
		case strings.HasPrefix(code, "spx_unavailable:"):
			return "SPX is unavailable; proxy gamma is awareness-only for S&P market structure."
		case code == "cache_stale_off_hours":
			return "Cached after-hours gamma is stale; refresh during option hours for a fresh market-structure read."
		case code == "all_iv_derived":
			return "SPX model IV ticks did not land; gamma is visible as context but does not vote."
		}
	}
	msg := strings.TrimSpace(d.Message)
	if msg == "" {
		msg = d.Code
	}
	scope := strings.TrimSpace(d.Scope)
	lowerMsg := strings.ToLower(msg)
	lowerScope := strings.ToLower(scope)
	if scope != "" && !strings.HasPrefix(lowerMsg, lowerScope+" ") && !strings.HasPrefix(lowerMsg, lowerScope+":") {
		msg = scope + ": " + msg
	}
	parts := []string{msg}
	if impact := strings.TrimSpace(d.Impact); impact != "" {
		parts = append(parts, impact)
	}
	if diagnostics {
		if action := strings.TrimSpace(d.Action); action != "" {
			parts = append(parts, "Action: "+action)
		}
	}
	return strings.Join(parts, " ")
}

func renderExplainKV(env *Env, out io.Writer, label, value string, style func(string) string) {
	const maxWidth = 106
	prefix := "    " + label + ": "
	available := max(maxWidth-visibleLen(prefix), 32)
	for i, line := range wrapVisibleText(value, available) {
		if style != nil {
			line = style(line)
		}
		if i == 0 {
			fmt.Fprintf(out, "    %s %s\n", env.yellow(label+":"), line)
			continue
		}
		fmt.Fprintf(out, "%s%s\n", strings.Repeat(" ", visibleLen(prefix)), line)
	}
}

func explainBandLine(band, reason string) string {
	switch {
	case band != "" && reason != "":
		return band + " · " + reason
	case band != "":
		return band
	case reason != "":
		return reason
	default:
		return ""
	}
}

func explainThresholdLine(t *rpc.RegimeThresholds) string {
	if t == nil {
		return ""
	}
	parts := []string{}
	if t.Green != "" {
		parts = append(parts, "green "+t.Green)
	}
	if t.Yellow != "" {
		parts = append(parts, "yellow "+t.Yellow)
	}
	if t.Red != "" {
		parts = append(parts, "red "+t.Red)
	}
	return strings.Join(parts, "; ")
}

func explainQualityLine(now time.Time, fields []regimeExplainQuality, diagnostics bool) string {
	parts := []string{}
	for _, field := range fields {
		if field.q == nil {
			continue
		}
		part := field.name + " " + field.q.Confidence + "/" + field.q.FreshnessClass
		if age := compactQualityAge(now.Sub(field.q.AsOf)); age != "" {
			part += ", " + age
		}
		if diagnostics && field.q.Source != "" {
			part += " (" + field.q.Source + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

func compactQualityAge(age time.Duration) string {
	if age <= time.Second {
		return ""
	}
	switch {
	case age >= 48*time.Hour:
		return fmt.Sprintf("%dd old", int(age.Hours()/24))
	case age >= time.Hour:
		return fmt.Sprintf("%dh old", int(age.Hours()))
	default:
		return fmt.Sprintf("%ds old", int(age.Seconds()))
	}
}

func styleRegimeExplainBand(env *Env, band string) func(string) string {
	switch band {
	case "green":
		return env.green
	case "yellow":
		return env.yellow
	case "red":
		return env.red
	default:
		return nil
	}
}

func explainReadNote(name string) string {
	switch {
	case strings.HasPrefix(name, "VIX/"):
		return "Volatility term structure. Sustained inversion matters; one-day spikes are noise."
	case name == "VVIX":
		return "Vol-of-vol demand. Use beside VIX term structure because the two can diverge."
	case strings.HasPrefix(name, "HYG"):
		return "ETF credit proxy versus SPY. Watch sustained HYG weakness, especially while SPY stays near highs."
	case strings.Contains(name, "OAS"):
		return "Official cash-credit close. Slower than HYG, but cleaner spread evidence."
	case strings.Contains(name, "funding"):
		return "Short-term funding pressure. A wider CP/T-bill spread means more funding stress."
	case strings.HasPrefix(name, "USD"):
		return "FX carry proxy. Yen strengthening can flag carry-unwind pressure."
	case strings.Contains(name, "γ-zero"):
		return "Dealer-gamma model. Above γ-zero tends to dampen moves (stabilizing); below it tends to amplify (amplifying). No crossing means the row bands on profile sign."
	case strings.Contains(name, "breadth"):
		return "SPX participation. Weak breadth means fewer names are carrying the index."
	default:
		return ""
	}
}

// ----------------------------------------------------------------------------
// Tiny helpers — local to this file, not promoted to cli.go because no
// other renderer needs them.
