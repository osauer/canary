package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/flexstmt"
)

// flexCashBaseline is historical broker statement evidence, not proof of
// intraday cash or absence of unsettled debits. Mode is bound to the caller's
// concrete current broker scope; Flex does not assert a paper/live mode.
type flexCashBaseline struct {
	QueryFingerprint, ReportFingerprint string
	Scope                               brokerStateScope
	FromDate, ToDate, ReportDate        time.Time
	// GeneratedAt retains the parser's timezone-less generation ordering label.
	// It must never be used as an actual UTC event cutoff.
	GeneratedAt time.Time
	// AcceptedAt is the latest local inventory acceptance, refreshed on ingestion.
	// It is neither the original network receipt nor broker generation time.
	AcceptedAt time.Time
	// ActivityFrom deliberately includes the complete reporting day. No exact
	// statement cutoff is documented, so even same-day buys may be overreserved.
	ActivityFrom time.Time
	Currencies   map[string]flexCashBaselineCurrency
}

type flexCashBaselineCurrency struct{ EndingCash, EndingSettledCash *float64 }

type flexCashSourceStatement struct {
	Statement  flexstmt.Statement
	Digest     [sha256.Size]byte
	AcceptedAt time.Time
}

const flexCashReportOwnerAction = "enable Cash Report with currency, fromDate, toDate, reportDate, endingCash and endingSettledCash in the existing Activity Flex Query, then fetch a new statement"

// flexSettledCashBaseline reads only bytes already accepted in the current
// query-scoped SQLite inventory. New, changed or missing files invalidate the
// read until ordinary reporting ingestion accepts a complete snapshot.
func (s *Server) flexSettledCashBaseline(scope brokerStateScope, now time.Time) (flexCashBaseline, error) {
	fail := func(reason string) (flexCashBaseline, error) { return flexCashBaseline{}, fmt.Errorf("%s", reason) }
	if s == nil || s.cfg == nil || s.coreStore == nil || !s.cfg.Flex.Enabled || !brokerScopeConcrete(scope) || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return fail("Flex cash baseline requires a concrete current broker scope and accepted reporting authority")
	}
	selection := s.flexEvidenceSelection()
	if !validFlexQueryFingerprint(selection.ActiveQueryFingerprint) {
		return fail("Flex cash baseline requires an active Activity Flex Query")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	projectionScope := statementProjectionScopeForSelection(selection)
	recorded, err := s.coreStore.LoadStatementFiles(ctx, projectionScope)
	if err != nil {
		return fail("accepted Flex statement inventory is unavailable")
	}
	files, err := readStatementProjectionFiles(ctx, selection)
	if err != nil || !statementProjectionInventoryMatches(recorded, files) {
		return fail("Flex statement bytes do not match accepted active-query evidence")
	}
	byName := map[string]corestore.StatementFileRecord{}
	for _, record := range recorded {
		byName[record.FileKey] = record
	}
	statements := []flexCashSourceStatement{}
	for _, file := range files {
		if ctx.Err() != nil {
			return fail("Flex cash baseline read exceeded its budget")
		}
		record := byName[file.name]
		if record.IngestedAt == nil || record.IngestedAt.IsZero() || record.IngestedAt.After(now) {
			return fail("Flex cash statement has no valid local acceptance receipt")
		}
		parsed, err := flexstmt.Parse(file.data)
		if err != nil {
			return fail("accepted Flex statement could not be parsed")
		}
		for _, statement := range parsed {
			statements = append(statements, flexCashSourceStatement{Statement: statement, Digest: file.digest, AcceptedAt: record.IngestedAt.UTC()})
		}
	}
	baseline, err := selectFlexCashBaseline(selection.ActiveQueryFingerprint, scope, statements, now)
	if err != nil {
		return flexCashBaseline{}, err
	}
	// Scope, query rotation and accepted inventory can change while local files
	// are read. Recheck all three before returning any historical cash values.
	finalRecorded, err := s.coreStore.LoadStatementFiles(ctx, projectionScope)
	if err != nil || !statementProjectionInventoryMatches(finalRecorded, files) || s.flexEvidenceSelection() != selection || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return fail("Flex cash baseline authority changed during the read")
	}
	return baseline, nil
}

func selectFlexCashBaseline(query string, scope brokerStateScope, statements []flexCashSourceStatement, now time.Time) (flexCashBaseline, error) {
	fail := func(reason string) (flexCashBaseline, error) { return flexCashBaseline{}, fmt.Errorf("%s", reason) }
	if !validFlexQueryFingerprint(query) || !brokerScopeConcrete(scope) || now.IsZero() {
		return fail("Flex cash baseline identity is unavailable")
	}
	metadata := make([]flexstmt.Statement, 0, len(statements))
	for _, item := range statements {
		metadata = append(metadata, item.Statement)
	}
	latest := latestReportingStatements(metadata)
	if len(latest) == 0 {
		return fail("no accepted current-query Flex statement is available")
	}
	target := latestCompletedFlexDate(now)
	if !latest[0].ToDate.Equal(target) {
		return fail("Flex cash baseline must cover the latest completed reporting day")
	}
	type semanticBaseline struct {
		From, To, Report time.Time
		Currencies       map[string]flexCashBaselineCurrency
	}
	var selected flexCashBaseline
	var canonical []byte
	digests := []string{}
	for _, item := range statements {
		st := item.Statement
		if !slices.ContainsFunc(latest, func(candidate flexstmt.Statement) bool {
			return candidate.AccountID == st.AccountID && candidate.FromDate.Equal(st.FromDate) && candidate.ToDate.Equal(st.ToDate) && candidate.WhenGenerated.Equal(st.WhenGenerated)
		}) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(st.AccountID), strings.TrimSpace(scope.Account)) {
			return fail("current Flex cash statement does not match the selected account")
		}
		if st.FromDate.IsZero() || st.FromDate.After(st.ToDate) || st.WhenGenerated.IsZero() || item.AcceptedAt.IsZero() || item.AcceptedAt.After(now) {
			return fail("Flex cash baseline dates or acceptance receipt are invalid")
		}
		activityFrom := time.Date(st.ToDate.Year(), st.ToDate.Month(), st.ToDate.Day(), 0, 0, 0, 0, flexReportingLocation())
		generationDay := time.Date(st.WhenGenerated.Year(), st.WhenGenerated.Month(), st.WhenGenerated.Day(), 0, 0, 0, 0, time.UTC)
		if item.AcceptedAt.Before(activityFrom) || generationDay.Before(st.ToDate) || generationDay.After(cashSweepDay(now).AddDate(0, 0, 1)) {
			return fail("Flex cash baseline has contradictory receipt or generation dates")
		}
		// A timezone-less label represents possible instants, not UTC. Even
		// with a deliberately broad whole-day offset envelope, generation
		// must be able to precede local acceptance. This bound is never used
		// to infer an event or settlement cutoff.
		if st.WhenGenerated.Add(-24 * time.Hour).After(item.AcceptedAt) {
			return fail("Flex cash generation label cannot precede local acceptance")
		}
		if st.CashBalanceError != "" || len(st.CashBalances) == 0 {
			return fail("Flex settled cash is unavailable: " + flexCashReportOwnerAction)
		}
		currencies := map[string]flexCashBaselineCurrency{}
		for _, row := range st.CashBalances {
			if len(row.Currency) != 3 || strings.IndexFunc(row.Currency, func(c rune) bool { return c < 'A' || c > 'Z' }) >= 0 {
				return fail("Flex cash baseline requires exact native currency rows")
			}
			if row.AccountID != st.AccountID || !row.FromDate.Equal(st.FromDate) || !row.ToDate.Equal(st.ToDate) || !row.ReportDate.Equal(st.ToDate) || row.EndingCash == nil || row.EndingSettledCash == nil {
				return fail("Flex cash row identity, dates or balances are incomplete")
			}
			if _, duplicate := currencies[row.Currency]; duplicate {
				return fail("Flex cash baseline contains duplicate currency rows")
			}
			currencies[row.Currency] = flexCashBaselineCurrency{EndingCash: new(*row.EndingCash), EndingSettledCash: new(*row.EndingSettledCash)}
		}
		raw, err := json.Marshal(semanticBaseline{From: st.FromDate, To: st.ToDate, Report: st.ToDate, Currencies: currencies})
		if err != nil {
			return fail("Flex cash baseline amounts are invalid")
		}
		if canonical != nil && string(canonical) != string(raw) {
			return fail("same-generation Flex cash statements have ambiguous baseline values or dates")
		}
		canonical = raw
		digests = append(digests, hex.EncodeToString(item.Digest[:]))
		if selected.Currencies == nil || item.AcceptedAt.After(selected.AcceptedAt) {
			selected = flexCashBaseline{QueryFingerprint: query, Scope: scope, FromDate: st.FromDate, ToDate: st.ToDate, ReportDate: st.ToDate, GeneratedAt: st.WhenGenerated, AcceptedAt: item.AcceptedAt, Currencies: currencies,
				ActivityFrom: time.Date(st.ToDate.Year(), st.ToDate.Month(), st.ToDate.Day(), 0, 0, 0, 0, flexReportingLocation())}
		}
	}
	if selected.Currencies == nil {
		return fail("current Flex cash baseline could not be selected")
	}
	slices.Sort(digests)
	digests = slices.Compact(digests)
	identity, err := json.Marshal(struct {
		Query   string
		Scope   brokerStateScope
		Dates   []time.Time
		Cash    json.RawMessage
		Digests []string
	}{query, scope, []time.Time{selected.FromDate, selected.ToDate, selected.GeneratedAt}, canonical, digests})
	if err != nil {
		return fail("Flex cash baseline identity could not be bound")
	}
	fingerprint := sha256.Sum256(identity)
	selected.ReportFingerprint = "flex_cash_" + hex.EncodeToString(fingerprint[:])
	return selected, nil
}
