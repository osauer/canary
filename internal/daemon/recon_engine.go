package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/flexstmt"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Reconciliation engine (internal-docs/design/post-trade-truth.md): matches broker
// statement flows against the declared capital-event ledger under the
// constitution's [recon] policy keys. Deterministic and regenerated from
// retained files on every call — the report id pins the exception and
// baseline sets a reconcile signs off. Never-false-match: an ambiguous
// pairing is an exception, never a best-effort pick.

// reconFlow is one statement-side flow candidate after merge.
type reconFlow struct {
	id         string
	typ        string
	desc       string
	valueDate  time.Time
	amountBase float64
}

type retainedStatementMerge struct {
	flows            []reconFlow
	exceptions       []rpc.ReconException
	equityByDay      map[string]flexstmt.EquityRow
	classifiedCounts map[string]int
	statementAsOf    time.Time
	coverageFrom     time.Time
	coverageTo       time.Time
}

// buildReconReport regenerates the report. It is cheap (local files only)
// and side-effect free apart from reading journals.
func (s *Server) buildReconReport() *rpc.ReconResult {
	res, _ := s.buildReconReportContext(context.Background())
	return res
}

func (s *Server) buildReconReportWithSnapshot() (*rpc.ReconResult, *statementCapitalSnapshot) {
	res, snapshot, _ := s.buildReconReportWithSnapshotContext(context.Background())
	return res, snapshot
}

func (s *Server) buildReconReportContext(ctx context.Context) (*rpc.ReconResult, error) {
	res, _, err := s.buildReconReportWithSnapshotContext(ctx)
	return res, err
}

func (s *Server) buildReconReportWithSnapshotContext(ctx context.Context) (*rpc.ReconResult, *statementCapitalSnapshot, error) {
	if err := s.nudgeScanCheck(ctx, "recon_start"); err != nil {
		return nil, nil, err
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	res := &rpc.ReconResult{AsOf: now, Counts: map[string]int{}}
	res.Fetch = s.flexFetchStatusAt(now)
	defer func() {
		res.Automation = s.reconAutomationStatus(res, now)
	}()
	if s.riskCapital != nil {
		res.LastAutoExtendReportID, res.LastAutoExtendedAt = s.riskCapital.LastAutoExtend()
	}

	var health []rpc.SourceHealth
	pol := s.riskPolicies.snapshot().policy
	rc := reconPolicyOf(pol)
	if rc == nil {
		res.Status = rpc.ReconStatusUnapproved
		res.Message = "recon.* policy keys are unapproved; write them in the risk policy before reconciliation can classify anything"
		health = append(health, rpc.SourceHealth{Source: "risk_policy", Status: "unapproved"})
		res.InputHealth = health
		return res, nil, nil
	}

	statements, problems, err := loadRetainedFlexStatementsContext(ctx, func(stage string) error { return s.nudgeScanCheck(ctx, stage) })
	if err != nil && ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	switch {
	case err != nil:
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "cannot read retained statements: " + err.Error()
		res.InputHealth = append(health, rpc.SourceHealth{Source: "statements", Status: "unavailable", Notes: []string{err.Error()}})
		return res, nil, nil
	case len(statements) == 0:
		res.Status = rpc.ReconStatusUnavailable
		res.Message = "no retained Flex statements yet; enable [flex] and wait for the daily fetch, or check fetch.last_error"
		res.InputHealth = append(health, rpc.SourceHealth{Source: "statements", Status: "unavailable"})
		return res, nil, nil
	}
	res.Status = rpc.ReconStatusActive
	if len(problems) > 0 {
		res.Status = rpc.ReconStatusDegraded
		health = append(health, rpc.SourceHealth{Source: "statements", Status: "degraded", Notes: problems})
	} else {
		health = append(health, rpc.SourceHealth{Source: "statements", Status: "ok"})
	}

	merged := mergeRetainedStatements(statements)
	res.StatementAsOf = merged.statementAsOf
	res.CoverageFrom = merged.coverageFrom
	res.CoverageTo = merged.coverageTo

	replayCtx := s.riskCapital.ReplayContext()
	res.GenesisAt = replayCtx.GenesisAt
	matchableFlows, baseline := partitionReconBaselineFlows(merged.flows, replayCtx)
	events, err := s.riskCapital.CapitalFlowEventsContext(ctx, func(stage string) error { return s.nudgeScanCheck(ctx, stage) })
	if err != nil {
		return nil, nil, err
	}
	bridgeFlows := 0.0
	var bridgeEvents []capitalEventV1
	if pol.PolicyVersion >= 3 {
		events, bridgeEvents, bridgeFlows = splitV3ReconEvents(events, replayCtx, res.CoverageTo)
	}
	matchedExceptions, matched := matchReconFlows(matchableFlows, events, rc)
	var confirmed []rpc.ReconException
	if pol.PolicyVersion >= 3 {
		kept := matchedExceptions[:0]
		for _, ex := range matchedExceptions {
			if ex.Category == rpc.ReconMissingFromLedger {
				ex.Category = rpc.ReconConfirmed
				ex.Note = "broker-confirmed external flow; no declaration required under policy v3"
				confirmed = append(confirmed, ex)
				continue
			}
			kept = append(kept, ex)
		}
		matchedExceptions = kept
	}
	exceptions := append(merged.exceptions, matchedExceptions...)
	if err := s.applyReconDismissalsContext(ctx, exceptions, func(stage string) error { return s.nudgeScanCheck(ctx, stage) }); err != nil {
		return nil, nil, err
	}

	sort.Slice(exceptions, func(i, j int) bool { return exceptions[i].LineID < exceptions[j].LineID })
	sort.Slice(baseline, func(i, j int) bool { return baseline[i].LineID < baseline[j].LineID })
	sort.Slice(confirmed, func(i, j int) bool { return confirmed[i].LineID < confirmed[j].LineID })
	for _, ex := range exceptions {
		res.Counts[ex.Category]++
		if !ex.Dismissed {
			res.Unresolved++
		}
	}
	res.Counts["matched"] = len(matched)
	res.Counts[rpc.ReconBaseline] = len(baseline)
	res.Counts[rpc.ReconConfirmed] = len(confirmed)
	res.Exceptions = exceptions
	res.Baseline = baseline
	res.Confirmed = confirmed
	statementFlows := bridgeFlows
	for _, flow := range matchableFlows {
		statementFlows += flow.amountBase
	}
	if pol.PolicyVersion >= 3 {
		res.StatementCumFlowsBase = &statementFlows
	}
	res.Equity = s.reconEquityCheck(merged.equityByDay)
	res.ReportID = reconReportID(exceptions, baseline, confirmed, matchableFlows, bridgeEvents, res.CoverageFrom, res.CoverageTo, res.StatementAsOf, pol)
	res.InputHealth = health
	snapshot := &statementCapitalSnapshot{
		CoverageTo: res.CoverageTo,
		Flows:      matchableFlows,
		NudgeConfirmedFlows: nudgeConfirmedFlowSnapshot{
			PolicyVersion:     pol.PolicyVersion,
			PolicyIdentity:    nudgePolicyIdentity(pol),
			ReportStatus:      res.Status,
			ReportIdentity:    opaqueIdentity("recon-report", res.ReportID),
			StatementAsOf:     res.StatementAsOf,
			StatementsHealthy: statementsHealthOK(res.InputHealth),
		},
	}
	for _, row := range confirmed {
		snapshot.NudgeConfirmedFlows.ConfirmedRows = append(snapshot.NudgeConfirmedFlows.ConfirmedRows, confirmedFlowContentIdentity(row))
	}
	for _, flow := range matchableFlows {
		snapshot.FlowsBase += flow.amountBase
	}
	if err := s.nudgeScanCheck(ctx, "recon_complete"); err != nil {
		return nil, nil, err
	}
	return res, snapshot, nil
}

func (s *Server) nudgeScanCheck(ctx context.Context, stage string) error {
	if s != nil && s.nudgeScanCheckpoint != nil {
		s.nudgeScanCheckpoint(stage)
	}
	return ctx.Err()
}

func splitV3ReconEvents(events []capitalEventV1, ctx capitalReplayContext, coverageTo time.Time) (within, bridges []capitalEventV1, bridgeFlows float64) {
	within = make([]capitalEventV1, 0, len(events))
	for _, ev := range events {
		effectiveAt := ev.EffectiveAt
		if effectiveAt.IsZero() {
			effectiveAt = ev.At
		}
		preGenesis := ctx.Seeded && !ctx.GenesisAt.IsZero() && utcDateBefore(effectiveAt, ctx.GenesisAt)
		if preGenesis || (!coverageTo.IsZero() && !utcDateAfter(effectiveAt, coverageTo)) {
			within = append(within, ev)
			continue
		}
		bridges = append(bridges, ev)
		if ev.Type == "deposit" {
			bridgeFlows += ev.AmountBase
		} else if ev.Type == "withdrawal" {
			bridgeFlows -= ev.AmountBase
		}
	}
	return within, bridges, bridgeFlows
}

// mergeRetainedStatements folds all retained Flex statements into the
// restatement-aware flow, exception, and equity inputs shared by the live
// recon report and the full-window backtest. A newer broker generation wins;
// for the same generation, the most recently retained query result wins. A
// later download of an older broker generation must never roll a correction
// back.
func mergeRetainedStatements(statements []flexstmt.Statement) retainedStatementMerge {
	statements = append([]flexstmt.Statement(nil), statements...)
	sort.SliceStable(statements, func(i, j int) bool {
		return statements[i].WhenGenerated.After(statements[j].WhenGenerated)
	})
	merged := retainedStatementMerge{
		equityByDay:      make(map[string]flexstmt.EquityRow),
		classifiedCounts: make(map[string]int),
	}
	seenLine := make(map[string]bool)
	for _, st := range statements {
		if st.WhenGenerated.After(merged.statementAsOf) {
			merged.statementAsOf = st.WhenGenerated
		}
		if merged.coverageFrom.IsZero() || st.FromDate.Before(merged.coverageFrom) {
			merged.coverageFrom = st.FromDate
		}
		if st.ToDate.After(merged.coverageTo) {
			merged.coverageTo = st.ToDate
		}
		for _, c := range st.Cash {
			if seenLine[c.ID] {
				continue
			}
			seenLine[c.ID] = true
			switch {
			case c.Category == flexstmt.CategoryFlow && c.AmountBase != nil:
				merged.flows = append(merged.flows, reconFlow{id: c.ID, typ: c.Type, desc: c.Description, valueDate: c.ValueDate, amountBase: *c.AmountBase})
			case c.Category == flexstmt.CategoryFlow:
				merged.exceptions = append(merged.exceptions, rpc.ReconException{LineID: c.ID, Category: rpc.ReconUncategorized, Type: c.Type,
					Description: c.Description, ValueDate: c.ValueDate, Note: "flow line without a usable base amount"})
			case c.Category == flexstmt.CategoryClassified:
				merged.classifiedCounts[c.Type]++
			case c.Category == flexstmt.CategoryUncategorized:
				amt := c.AmountBase
				merged.exceptions = append(merged.exceptions, rpc.ReconException{LineID: c.ID, Category: rpc.ReconUncategorized, Type: c.Type,
					Description: c.Description, ValueDate: c.ValueDate, AmountBase: amt, Note: "unknown statement line type; classify or dismiss"})
			}
		}
		for _, t := range st.Transfers {
			if seenLine[t.ID] {
				continue
			}
			seenLine[t.ID] = true
			if t.AmountBase == nil {
				merged.exceptions = append(merged.exceptions, rpc.ReconException{LineID: t.ID, Category: rpc.ReconUncategorized, Type: "Transfer " + t.Direction,
					Description: t.Description, ValueDate: t.Date, Note: "transfer without a computable base value"})
				continue
			}
			amount := *t.AmountBase
			if t.Direction == "OUT" && amount > 0 {
				amount = -amount
			}
			merged.flows = append(merged.flows, reconFlow{id: t.ID, typ: "Transfer " + t.Direction, desc: t.Description, valueDate: t.Date, amountBase: amount})
		}
		for _, e := range st.Equity {
			key := e.ReportDate.Format("2006-01-02")
			if _, ok := merged.equityByDay[key]; !ok {
				merged.equityByDay[key] = e
			}
		}
	}
	return merged
}

func reconPolicyOf(c *risk.Constitution) *risk.ConstitutionRecon {
	if c == nil {
		return nil
	}
	r := c.Recon
	if r.AmountTolerancePct == nil || r.AmountToleranceMin == nil || r.DateWindowBusinessDays == nil || r.MaxReportAgeDays == nil {
		return nil
	}
	return &r
}

// matchReconFlows pairs statement flows with declared events. Bijective
// matches only: a flow or event with more than one candidate is ambiguous
// and surfaces as an exception (never-false-match).
func matchReconFlows(flows []reconFlow, events []capitalEventV1, rc *risk.ConstitutionRecon) ([]rpc.ReconException, map[string]bool) {
	tolerance := func(stmtAmount float64) float64 {
		return max(math.Abs(stmtAmount)**rc.AmountTolerancePct/100, *rc.AmountToleranceMin)
	}
	signed := func(ev capitalEventV1) float64 {
		if ev.Type == "withdrawal" {
			return -ev.AmountBase
		}
		return ev.AmountBase
	}
	amountOK := func(f reconFlow, ev capitalEventV1) bool {
		return math.Abs(f.amountBase-signed(ev)) <= tolerance(f.amountBase)
	}
	dateOK := func(f reconFlow, ev capitalEventV1, window int) bool {
		return businessDaysApart(f.valueDate, ev.EffectiveAt) <= window
	}

	flowCands := make([][]int, len(flows))
	eventCands := make([][]int, len(events))
	for fi, f := range flows {
		for ei, ev := range events {
			if amountOK(f, ev) && dateOK(f, ev, *rc.DateWindowBusinessDays) {
				flowCands[fi] = append(flowCands[fi], ei)
				eventCands[ei] = append(eventCands[ei], fi)
			}
		}
	}

	var out []rpc.ReconException
	matched := make(map[string]bool)
	flowDone := make([]bool, len(flows))
	eventDone := make([]bool, len(events))
	for fi, cands := range flowCands {
		switch {
		case len(cands) == 1 && len(eventCands[cands[0]]) == 1:
			flowDone[fi] = true
			eventDone[cands[0]] = true // matched
			matched[flows[fi].id] = true
		case len(cands) > 1:
			f := flows[fi]
			amt := f.amountBase
			out = append(out, rpc.ReconException{LineID: f.id, Category: rpc.ReconAmbiguous, Type: f.typ, Description: f.desc,
				ValueDate: f.valueDate, AmountBase: &amt, Note: fmt.Sprintf("%d declared events qualify; tighten dates or amounts, never guessed", len(cands))})
			flowDone[fi] = true
		case len(cands) == 1: // its event serves multiple flows
			f := flows[fi]
			amt := f.amountBase
			out = append(out, rpc.ReconException{LineID: f.id, Category: rpc.ReconAmbiguous, Type: f.typ, Description: f.desc,
				ValueDate: f.valueDate, AmountBase: &amt, Note: "the qualifying declared event also matches another statement flow"})
			flowDone[fi] = true
		}
	}
	// Unmatched flows: probe for near-misses to name the exception well.
	for fi, f := range flows {
		if flowDone[fi] {
			continue
		}
		amt := f.amountBase
		ex := rpc.ReconException{LineID: f.id, Type: f.typ, Description: f.desc, ValueDate: f.valueDate, AmountBase: &amt}
		found := false
		for ei, ev := range events {
			if eventDone[ei] {
				continue
			}
			evAmt := signed(ev)
			// Plausibility bound for the amount_mismatch pairing: same
			// direction and within 10% of each other. Anything looser
			// glues unrelated flows together and hides a genuinely
			// missing line behind a nonsense pair. Categorization only —
			// both categories are exceptions the operator must resolve.
			sameSign := (f.amountBase >= 0) == (evAmt >= 0)
			rel := math.Abs(f.amountBase-evAmt) / max(math.Abs(f.amountBase), math.Abs(evAmt))
			switch {
			case dateOK(f, ev, *rc.DateWindowBusinessDays) && sameSign && rel <= 0.10:
				ex.Category = rpc.ReconAmountMismatch
				ex.EventAt = ev.EffectiveAt
				ex.EventAmountBase = &evAmt
				ex.Note = "dates align but amounts differ beyond tolerance"
			case amountOK(f, ev) && dateOK(f, ev, 3**rc.DateWindowBusinessDays):
				ex.Category = rpc.ReconDateMismatch
				ex.EventAt = ev.EffectiveAt
				ex.EventAmountBase = &evAmt
				ex.Note = "amounts align but the declared effective date is outside the window"
			default:
				continue
			}
			eventDone[ei] = true
			found = true
			break
		}
		if !found {
			ex.Category = rpc.ReconMissingFromLedger
			ex.Note = "statement flow with no declared capital event; declare it or dismiss with a reason"
		}
		out = append(out, ex)
	}
	for ei, ev := range events {
		if eventDone[ei] {
			continue
		}
		evAmt := signed(ev)
		out = append(out, rpc.ReconException{
			LineID:          "event-" + ev.At.UTC().Format("20060102-150405") + fmt.Sprintf("-%d", ei),
			Category:        rpc.ReconLedgerOnly,
			Type:            ev.Type,
			Description:     ev.Note,
			EventAt:         ev.EffectiveAt,
			EventAmountBase: &evAmt,
			Note:            "declared event with no statement flow; a loss dressed as a withdrawal would land here",
		})
	}
	return out, matched
}

func partitionReconBaselineFlows(flows []reconFlow, ctx capitalReplayContext) ([]reconFlow, []rpc.ReconException) {
	if !ctx.Seeded || ctx.GenesisAt.IsZero() {
		return flows, nil
	}
	matchable := make([]reconFlow, 0, len(flows))
	var baseline []rpc.ReconException
	for _, flow := range flows {
		if !utcDateBefore(flow.valueDate, ctx.GenesisAt) {
			matchable = append(matchable, flow)
			continue
		}
		amount := flow.amountBase
		baseline = append(baseline, rpc.ReconException{
			LineID:      flow.id,
			Category:    rpc.ReconBaseline,
			Type:        flow.typ,
			Description: flow.desc,
			ValueDate:   flow.valueDate,
			AmountBase:  &amount,
			PreGenesis:  true,
			Note:        "embedded in the seeded baseline (pre-genesis); no ledger event belongs here",
		})
	}
	return matchable, baseline
}

func utcDateBefore(a, b time.Time) bool {
	a = a.UTC()
	b = b.UTC()
	ad := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	bd := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return ad.Before(bd)
}

func utcDateAfter(a, b time.Time) bool {
	return utcDateBefore(b, a)
}

// businessDaysApart counts weekdays strictly between the earlier and later
// timestamp's calendar days (same day = 0). Exchange holidays deliberately
// not modeled: a one-holiday slack belongs in the policy window.
func businessDaysApart(a, b time.Time) int {
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	db := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	if da.After(db) {
		da, db = db, da
	}
	n := 0
	for d := da.AddDate(0, 0, 1); !d.After(db); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n++
		}
	}
	return n
}

// applyReconDismissals folds journaled human dismissals into the exception
// list. The journal is the source of truth; nothing else stores them.
func (s *Server) applyReconDismissalsContext(ctx context.Context, exceptions []rpc.ReconException, checkpoint func(string) error) error {
	if s == nil || s.riskCapital == nil || s.riskCapital.core == nil {
		// Legacy unit/import helper only; Start binds core before runtime.
		return applyReconDismissalsContext(ctx, exceptions, checkpoint)
	}
	if checkpoint != nil {
		if err := checkpoint("recon_dismissals_start"); err != nil {
			return err
		}
	} else if err := ctx.Err(); err != nil {
		return err
	}
	payloads, err := s.riskCapital.GovernanceEventPayloads(ctx)
	if err != nil {
		return err
	}
	reasons := map[string]string{}
	for _, payload := range payloads {
		if err := ctx.Err(); err != nil {
			return err
		}
		var entry struct {
			Kind   string `json:"kind"`
			LineID string `json:"line_id"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(payload, &entry) == nil && entry.Kind == "recon_dismiss" && entry.LineID != "" {
			reasons[entry.LineID] = entry.Reason
		}
	}
	for i := range exceptions {
		if reason, ok := reasons[exceptions[i].LineID]; ok {
			exceptions[i].Dismissed = true
			exceptions[i].DismissReason = reason
		}
	}
	return nil
}

func applyReconDismissalsContext(ctx context.Context, exceptions []rpc.ReconException, checkpoint func(string) error) error {
	if checkpoint != nil {
		if err := checkpoint("recon_dismissals_start"); err != nil {
			return err
		}
	} else if err := ctx.Err(); err != nil {
		return err
	}
	path, err := defaultTradingStatePath(riskPolicyJournalFile)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	reasons := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if err := ctx.Err(); err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			Kind   string `json:"kind"`
			LineID string `json:"line_id"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Kind == "recon_dismiss" && entry.LineID != "" {
			reasons[entry.LineID] = entry.Reason
		}
	}
	for i := range exceptions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if reason, ok := reasons[exceptions[i].LineID]; ok {
			exceptions[i].Dismissed = true
			exceptions[i].DismissReason = reason
		}
	}
	return nil
}

func (s *Server) reconEquityCheck(equityByDay map[string]flexstmt.EquityRow) *rpc.ReconEquityCheck {
	var newest flexstmt.EquityRow
	for _, row := range equityByDay {
		if row.ReportDate.After(newest.ReportDate) {
			newest = row
		}
	}
	if newest.ReportDate.IsZero() {
		return nil
	}
	check := &rpc.ReconEquityCheck{StatementDate: newest.ReportDate, StatementTotalBase: newest.TotalBase}
	if s.riskCapital != nil {
		paired := newest
		var pairedEquity float64
		var pairedOK bool
		for _, row := range equityByDay {
			day := row.ReportDate.UTC().Format("2006-01-02")
			if equity, ok := s.riskCapital.DailySample(day); ok && (!pairedOK || row.ReportDate.After(paired.ReportDate)) {
				paired, pairedEquity, pairedOK = row, equity, true
			}
		}
		if pairedOK {
			check.StatementDate = paired.ReportDate
			check.StatementTotalBase = paired.TotalBase
			equity := pairedEquity
			check.RuntimeEquityBase = &equity
			check.SameDay = true
			if paired.TotalBase != 0 {
				div := (equity - paired.TotalBase) / math.Abs(paired.TotalBase) * 100
				check.DivergencePct = &div
			}
		} else if equity, asOf := s.riskCapital.LastEquity(); equity > 0 {
			check.RuntimeEquityBase = &equity
			check.RuntimeAsOf = asOf
		}
	}
	return check
}

// reconReportID pins the classified content, coverage, statement freshness,
// and policy identity. The v2 projection stays byte-identical; v3 additionally
// pins full row content so a confirmed restatement cannot reuse an id.
func reconReportID(exceptions, baseline, confirmed []rpc.ReconException, statementFlows []reconFlow, bridgeEvents []capitalEventV1, from, to, stmtAsOf time.Time, pol *risk.Constitution) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|", from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), stmtAsOf.UTC().Format(time.RFC3339))
	if pol != nil {
		fmt.Fprintf(h, "%s|", pol.FingerprintKey())
	}
	if pol == nil || pol.PolicyVersion < 3 {
		for _, ex := range exceptions {
			fmt.Fprintf(h, "%s|%s|%t\n", ex.LineID, ex.Category, ex.Dismissed)
		}
		for _, row := range baseline {
			fmt.Fprintf(h, "%s|%s|%t\n", row.LineID, rpc.ReconBaseline, false)
		}
	} else {
		flows := append([]reconFlow(nil), statementFlows...)
		sort.Slice(flows, func(i, j int) bool { return flows[i].id < flows[j].id })
		for _, flow := range flows {
			fmt.Fprintf(h, "flow|%s|%s|%s|%s|%.17g\n", flow.id, flow.typ, flow.desc, flow.valueDate.UTC().Format(time.RFC3339Nano), flow.amountBase)
		}
		for _, ev := range bridgeEvents {
			fmt.Fprintf(h, "bridge|%s|%s|%.17g|%s|%s\n", ev.At.UTC().Format(time.RFC3339Nano), ev.Type, ev.AmountBase,
				ev.EffectiveAt.UTC().Format(time.RFC3339Nano), ev.Note)
		}
		for _, rows := range [][]rpc.ReconException{exceptions, baseline, confirmed} {
			for _, row := range rows {
				raw, _ := json.Marshal(row)
				h.Write(raw)
				h.Write([]byte{'\n'})
			}
		}
	}
	return "recon-" + hex.EncodeToString(h.Sum(nil))[:16]
}
