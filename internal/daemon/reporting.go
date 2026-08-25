package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

const reportingValidationPollAttempts = 12

func (s *Server) handleReportingStatus(ctx context.Context) (*rpc.ReportingStatusResult, error) {
	now := time.Now()
	if s != nil && s.now != nil {
		now = s.now()
	}
	result := &rpc.ReportingStatusResult{
		SchemaVersion:   rpc.ReportingSchemaVersion,
		State:           rpc.ReportingStateConfigured,
		ManifestVersion: flexstmt.ManifestVersion,
	}
	if s != nil && s.cfg != nil {
		result.Local.Enabled = s.cfg.Flex.Enabled
		result.Local.QueryConfigured = strings.TrimSpace(s.cfg.Flex.QueryID) != ""
		result.Local.TokenFilePresent, result.Local.TokenFilePrivate = reportingTokenFileStatus(s.cfg.Flex.TokenPath)
	}

	fetch := s.flexFetchStatusAt(now)
	result.Broker = rpc.ReportingBrokerStatus{
		State: fetch.State, Reason: fetch.Reason, Reachability: reportingBrokerReachability(fetch),
		BrokerCode: fetch.BrokerCode, LastSuccess: fetch.LastSuccess, LastAttempt: fetch.LastAttempt,
		NextAttempt: fetch.NextAttempt, RetryAutomatic: fetch.RetryAutomatic, Busy: fetch.Busy,
	}

	var (
		statements []flexstmt.Statement
		loadErr    error
	)
	if s == nil || s.coreStore == nil {
		loadErr = fmt.Errorf("reporting projection authority unavailable")
	} else {
		projectionScope := s.activeStatementProjectionScope()
		var records []corestore.StatementRecord
		records, loadErr = s.coreStore.LoadStatementRecords(ctx, projectionScope, []string{corestore.StatementRecordMetadata}, statementProjectionMaxRows)
		if loadErr == nil && len(records) == statementProjectionMaxRows {
			loadErr = fmt.Errorf("reporting metadata projection exceeds supported size")
		}
		if loadErr == nil {
			statements, loadErr = reportingStatementsFromMetadata(records)
		}
	}
	manifest := flexstmt.CanonicalQueryManifest()
	evidence := flexstmt.QueryRequirementEvidence(statements)
	result.Requirements = make([]rpc.ReportingSectionRequirement, 0, len(manifest))
	for i, required := range manifest {
		observed := evidence[i]
		result.Requirements = append(result.Requirements, rpc.ReportingSectionRequirement{
			Key: required.Key, Label: required.Label, Status: observed.Status,
			LevelOfDetail: required.LevelOfDetail,
			Fields:        append([]string(nil), required.RequiredFields...),
			MissingFields: append([]string(nil), observed.MissingFields...),
		})
		if observed.Status == flexstmt.QueryRequirementUnproved {
			result.UnprovedSections = append(result.UnprovedSections, required.Key)
		}
	}
	result.MissingRequirements = flexstmt.MissingQueryRequirements(statements)
	result.Evidence.SchemaFingerprint = flexstmt.QuerySchemaFingerprint(statements)
	for _, statement := range statements {
		if statement.ToDate.After(result.Evidence.CoverageTo) {
			result.Evidence.CoverageTo = statement.ToDate.UTC()
		}
	}
	switch {
	case loadErr != nil:
		result.Evidence.State = rpc.ReportingEvidenceDegraded
	case len(statements) == 0:
		result.Evidence.State = rpc.ReportingEvidenceNotReceived
	default:
		result.Evidence.State = rpc.ReportingEvidenceObserved
	}

	setReportingOverallStatus(result, loadErr != nil)
	if err := rpc.ValidateReportingStatusResult(*result); err != nil {
		return nil, err
	}
	return result, nil
}

func reportingStatementsFromMetadata(records []corestore.StatementRecord) ([]flexstmt.Statement, error) {
	statements := make([]flexstmt.Statement, 0, len(records))
	for _, record := range records {
		if record.Kind != corestore.StatementRecordMetadata {
			return nil, fmt.Errorf("reporting projection returned an unexpected record kind")
		}
		var item statementMetadataProjectionPayload
		if err := json.Unmarshal(record.RawJSON, &item); err != nil || item.Version != statementProjectionVersion ||
			item.FromDate.IsZero() || item.ToDate.IsZero() || item.FromDate.After(item.ToDate) ||
			item.QueryFingerprint != "" && !validFlexQueryFingerprint(item.QueryFingerprint) {
			return nil, fmt.Errorf("decode reporting statement metadata projection")
		}
		statements = append(statements, flexstmt.Statement{
			AccountID: record.AccountKey, FromDate: item.FromDate.UTC(), ToDate: item.ToDate.UTC(),
			WhenGenerated: record.GeneratedAt.UTC(), ManifestVersion: item.ManifestVersion,
			Coverage: append([]flexstmt.SectionCoverage(nil), item.Coverage...),
		})
	}
	return statements, nil
}

func (s *Server) handleReportingValidate(ctx context.Context, req *rpc.Request) (*rpc.ReportingValidationResult, error) {
	var params rpc.ReportingValidateParams
	if err := decodeParams(req.Params, &params); err != nil {
		return nil, err
	}
	queryID := strings.TrimSpace(params.QueryID)
	tokenPath := expandUserPath(strings.TrimSpace(params.TokenPath))
	if !validReportingQueryID(queryID) || !filepath.IsAbs(tokenPath) {
		return nil, &rpc.Error{Code: rpc.CodeBadRequest, Message: "candidate reporting credentials are invalid"}
	}
	info, err := os.Lstat(tokenPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return nil, &rpc.Error{Code: rpc.CodeBadRequest, Message: "candidate reporting token file must be a private 0600 regular file"}
	}

	now := time.Now()
	if s != nil && s.now != nil {
		now = s.now()
	}
	to := latestCompletedFlexDate(now)
	from := to.AddDate(0, 0, -(edgeDailyLookbackDays - 1))
	result := newReportingValidationResult()

	s.flexBrokerMu.Lock()
	defer s.flexBrokerMu.Unlock()
	var raw []byte
	if s.flexRawDateRangeLockedFn != nil {
		raw, err = s.flexRawDateRangeLockedFn(ctx, from, to, reportingValidationPollAttempts, queryID, tokenPath)
	} else {
		raw, err = fetchFlexRawDateRangeWithCredentialsLocked(ctx, from, to, reportingValidationPollAttempts, queryID, tokenPath)
	}
	if err != nil {
		reason, _ := flexFailureStatus(err)
		result.Outcome = rpc.ReportingValidationActionRequired
		result.Reason = reason
		result.BrokerCode = flexFailureBrokerCode(err)
		result.Action = reportingValidationFailureAction(reason, result.BrokerCode)
		return result, nil
	}
	defer clear(raw)
	statements, err := flexstmt.Parse(raw)
	if err != nil {
		result.Outcome = rpc.ReportingValidationActionRequired
		result.Reason = rpc.ReconReportReasonReportInvalid
		result.Action = "IBKR returned a report Canary could not validate; keep the active query and review the reporting guide."
		return result, nil
	}
	if reason := validateReportingCandidateAccountScope(statements, s.currentBrokerStateScope()); reason != "" {
		result.Outcome = rpc.ReportingValidationActionRequired
		result.Reason = reason
		result.Action = "Keep the active query and select exactly the one account Canary uses in the candidate query."
		return result, nil
	}
	populateReportingValidationEvidence(result, statements)
	return result, nil
}

func newReportingValidationResult() *rpc.ReportingValidationResult {
	result := &rpc.ReportingValidationResult{
		SchemaVersion: rpc.ReportingSchemaVersion, ManifestVersion: flexstmt.ManifestVersion,
		Outcome: rpc.ReportingValidationActionRequired,
	}
	manifest := flexstmt.CanonicalQueryManifest()
	result.Requirements = make([]rpc.ReportingSectionRequirement, 0, len(manifest))
	for _, section := range manifest {
		result.Requirements = append(result.Requirements, rpc.ReportingSectionRequirement{
			Key: section.Key, Label: section.Label, Status: flexstmt.QueryRequirementNotReceived,
			LevelOfDetail: section.LevelOfDetail, Fields: append([]string(nil), section.RequiredFields...),
		})
	}
	return result
}

func populateReportingValidationEvidence(result *rpc.ReportingValidationResult, statements []flexstmt.Statement) {
	manifest := flexstmt.CanonicalQueryManifest()
	evidence := flexstmt.QueryRequirementEvidence(statements)
	result.Requirements = result.Requirements[:0]
	for i, section := range manifest {
		observed := evidence[i]
		result.Requirements = append(result.Requirements, rpc.ReportingSectionRequirement{
			Key: section.Key, Label: section.Label, Status: observed.Status,
			LevelOfDetail: section.LevelOfDetail, Fields: append([]string(nil), section.RequiredFields...),
			MissingFields: append([]string(nil), observed.MissingFields...),
		})
		switch observed.Status {
		case flexstmt.QueryRequirementMissing:
			for _, field := range observed.MissingFields {
				result.MissingRequirements = append(result.MissingRequirements, section.Key+"."+field)
			}
		case flexstmt.QueryRequirementUnproved:
			result.UnprovedSections = append(result.UnprovedSections, section.Key)
		}
	}
	result.SchemaFingerprint = flexstmt.QuerySchemaFingerprint(statements)
	switch {
	case len(result.MissingRequirements) > 0:
		result.Outcome = rpc.ReportingValidationActionRequired
		result.Reason = rpc.ReportingReasonFlexQueryIncomplete
		result.Action = "Keep the active query and add the named missing fields to the candidate."
	case len(result.UnprovedSections) > 0:
		result.Outcome = rpc.ReportingValidationUnproved
		result.Reason = rpc.ReportingReasonEmptySectionsUnproved
		result.ReadyForRotation = true
		result.Action = "No field is proven missing; empty or absent sections remain unproved until representative activity arrives."
	default:
		result.Outcome = rpc.ReportingValidationReady
		result.ReadyForRotation = true
	}
}

func validateReportingCandidateAccountScope(statements []flexstmt.Statement, scope brokerStateScope) string {
	accounts := make(map[string]struct{})
	for _, statement := range statements {
		account := strings.ToUpper(strings.TrimSpace(statement.AccountID))
		if !brokerScopeAccountConcrete(account) {
			return rpc.ReportingReasonAccountScopeInvalid
		}
		accounts[account] = struct{}{}
	}
	if len(accounts) != 1 {
		return rpc.ReportingReasonAccountScopeInvalid
	}
	if brokerScopeAccountConcrete(scope.Account) {
		for account := range accounts {
			if !strings.EqualFold(account, strings.TrimSpace(scope.Account)) {
				return rpc.ReportingReasonAccountScopeMismatch
			}
		}
	}
	return ""
}

func validReportingQueryID(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func reportingValidationFailureAction(reason, brokerCode string) string {
	if brokerCode == "1003" {
		return "IBKR reports that no statement is available for the latest completed reporting date; retry after the next statement publication, or review the query/account selection if it persists."
	}
	if brokerCode == "1025" {
		return "IBKR returned an undocumented Flex response; review Flex Web Service/query configuration or contact IBKR."
	}
	switch reason {
	case rpc.ReconReportReasonTokenInvalid, rpc.ReconReportReasonTokenExpired,
		rpc.ReconReportReasonIPRestricted, rpc.ReconReportReasonServiceInactive,
		rpc.ReconReportReasonQueryInvalid:
		return "Keep the active query and review the candidate Flex Web Service/query configuration."
	case rpc.ReconReportReasonReportNotReady, rpc.ReconReportReasonServiceBusy,
		rpc.ReconReportReasonRateLimited, rpc.ReconReportReasonNetworkUnavailable:
		return "Keep the active query and retry candidate validation after the broker service recovers."
	default:
		return "Keep the active query; IBKR did not return a candidate report Canary could validate."
	}
}

func reportingTokenFileStatus(path string) (present, private bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "~/.config/ibkr/flex-token"
	}
	info, err := os.Lstat(expandUserPath(path))
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, false
	}
	return true, info.Mode().Perm() == 0o600
}

func reportingBrokerReachability(status rpc.ReconFetchStatus) string {
	if status.Reason == rpc.ReconReportReasonNetworkUnavailable {
		return rpc.ReportingReachabilityUnreachable
	}
	if status.BrokerCode != "" || !status.LastSuccess.IsZero() {
		return rpc.ReportingReachabilityReachable
	}
	switch status.Reason {
	case rpc.ReconReportReasonReportNotReady, rpc.ReconReportReasonServiceBusy,
		rpc.ReconReportReasonRateLimited, rpc.ReconReportReasonTokenInvalid,
		rpc.ReconReportReasonTokenExpired, rpc.ReconReportReasonQueryInvalid,
		rpc.ReconReportReasonIPRestricted, rpc.ReconReportReasonServiceInactive,
		rpc.ReconReportReasonResponseInvalid:
		return rpc.ReportingReachabilityReachable
	default:
		return rpc.ReportingReachabilityUnknown
	}
}

func setReportingOverallStatus(result *rpc.ReportingStatusResult, evidenceLoadFailed bool) {
	switch {
	case !result.Local.Enabled:
		result.State, result.Reason = rpc.ReportingStateActionRequired, rpc.ReconReportReasonFlexDisabled
		result.Action = "Enable broker reporting, then run canary setup reporting."
		return
	case !result.Local.QueryConfigured:
		result.State, result.Reason = rpc.ReportingStateActionRequired, rpc.ReconReportReasonQueryMissing
		result.Action = "Create the canonical Activity Flex Query and configure its Query ID."
		return
	case !result.Local.TokenFilePresent:
		result.State, result.Reason = rpc.ReportingStateActionRequired, rpc.ReconReportReasonTokenMissing
		result.Action = "Generate a Flex Web Service token and store it in the configured private token file."
		return
	case !result.Local.TokenFilePrivate:
		result.State, result.Reason = rpc.ReportingStateActionRequired, rpc.ReportingReasonTokenPermissions
		result.Action = "Change the Flex token file permissions to 0600."
		return
	case evidenceLoadFailed:
		result.State, result.Reason = rpc.ReportingStateUnavailable, rpc.ReconReportReasonAuthorityUnavailable
		result.Action = "Canary could not inspect retained broker evidence; check local reporting storage."
		return
	case result.Broker.BrokerCode == "1025":
		result.State, result.Reason = rpc.ReportingStateActionRequired, rpc.ReportingReasonBrokerResponseUndocumented
		result.Action = "IBKR returned an undocumented Flex response; review Flex Web Service/query configuration or contact IBKR."
		return
	case len(result.MissingRequirements) > 0:
		result.State, result.Reason = rpc.ReportingStateActionRequired, rpc.ReportingReasonFlexQueryIncomplete
		result.Action = "Create a replacement query with the named missing requirements, validate it, then rotate to it."
		return
	case result.Evidence.State == rpc.ReportingEvidenceNotReceived:
		result.State, result.Reason = rpc.ReportingStateBackfilling, rpc.ReportingReasonReportNotReceived
		result.Action = "Wait for the first usable report or inspect the broker status below."
		return
	case result.Broker.State == rpc.ReconReportStateActionRequired:
		result.State, result.Reason = rpc.ReportingStateActionRequired, result.Broker.Reason
		result.Action = "Review the Flex Web Service and Activity Flex Query configuration."
		return
	case result.Broker.State == rpc.ReconReportStateUnavailable:
		result.State, result.Reason = rpc.ReportingStateUnavailable, result.Broker.Reason
		result.Action = "Restore reporting authority, then run reporting status again."
		return
	case result.Broker.State != rpc.ReconReportStateCurrent:
		result.State, result.Reason = rpc.ReportingStateBackfilling, result.Broker.Reason
		if len(result.UnprovedSections) > 0 {
			result.Action = "Canary will retry the current report automatically. That retry does not prove unproved sections: if matching activity exists in the covered dates, validate a replacement query."
		} else {
			result.Action = "Canary will retry automatically; use reporting status to follow the next broker check."
		}
		return
	case len(result.UnprovedSections) > 0:
		result.State, result.Reason = rpc.ReportingStateConfigured, rpc.ReportingReasonEmptySectionsUnproved
		result.Action = "Review the named unproved sections: if matching broker activity exists in the covered dates, validate a replacement query; otherwise wait for future rows to prove them."
		return
	default:
		result.State = rpc.ReportingStateCurrent
	}
}
