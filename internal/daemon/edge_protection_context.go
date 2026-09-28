package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v2/internal/edge"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) edgeProtectionEvidence(ctx context.Context, scope brokerStateScope) ([]orderJournalEvent, []proposalEvent, string) {
	records, err := s.coreStore.LoadOrderEvents(ctx, corestore.OrderQuery{FromAtMS: s.edgeNow().AddDate(0, 0, -edgeFullLookbackDays).Truncate(24 * time.Hour).UnixMilli(), Limit: 10000})
	if err != nil || len(records) == 10000 {
		return nil, nil, ""
	}
	events := make([]orderJournalEvent, 0, len(records))
	for _, record := range records {
		ev, err := decodeCoreOrderEvent(record)
		if err != nil {
			return nil, nil, ""
		}
		if orderJournalEventMatchesBrokerScope(ev, scope) {
			events = append(events, ev)
		}
	}
	// Bound optional provenance independently of retained journal size. Reaching
	// the cap withholds context; it never silently treats a prefix as complete.
	proposalRows, err := s.coreStore.LoadEvents(ctx, corestore.EventQuery{ScopeKey: daemonStateScope, Type: proposalCoreEventType, Limit: 10000})
	if err != nil || len(proposalRows) == 10000 {
		return nil, nil, ""
	}
	proposals := make([]proposalEvent, 0, len(proposalRows))
	for _, row := range proposalRows {
		var p proposalEvent
		if json.Unmarshal(row.PayloadJSON, &p) != nil || !proposalEventValid(p) || !p.At.Equal(row.OccurredAt) {
			return nil, nil, ""
		}
		if strings.EqualFold(p.AccountID, scope.Account) && strings.EqualFold(p.AccountMode, scope.Mode) {
			proposals = append(proposals, p)
		}
	}
	raw, err := json.Marshal(struct {
		Orders    []orderJournalEvent
		Proposals []proposalEvent
	}{events, proposals})
	if err != nil {
		return nil, nil, ""
	}
	sum := sha256.Sum256(raw)
	return events, proposals, hex.EncodeToString(sum[:])
}

func matchEdgeProtectionRecords(scope brokerStateScope, statements []flexstmt.Statement, events []orderJournalEvent, proposals []proposalEvent) map[string]edgecore.ProtectionRecord {
	byExecution := map[string][]orderJournalEvent{}
	for _, ev := range events {
		if ev.ExecID != "" && ev.ConID != 0 && orderJournalEventMatchesBrokerScope(ev, scope) {
			byExecution[ev.ExecID] = append(byExecution[ev.ExecID], ev)
		}
	}
	out := map[string]edgecore.ProtectionRecord{}
	for _, st := range statements {
		if !strings.EqualFold(st.AccountID, scope.Account) {
			continue
		}
		for _, trade := range st.Trades {
			if trade.ExecutionID == "" || !strings.EqualFold(trade.AccountID, scope.Account) {
				continue
			}
			var reference, token string
			valid := true
			linked := byExecution[trade.ExecutionID]
			if len(linked) == 0 {
				continue
			}
			for _, ev := range linked {
				if int64(ev.ConID) != trade.ConID || ev.OrderRef == "" || ev.PreviewTokenID == "" {
					valid = false
					break
				}
				if reference == "" {
					reference, token = ev.OrderRef, ev.PreviewTokenID
				} else if reference != ev.OrderRef || token != ev.PreviewTokenID {
					valid = false
					break
				}
			}
			if !valid {
				continue
			}
			var match edgecore.ProtectionRecord
			count := 0
			for _, p := range proposals {
				if p.Type != "submitted" || !strings.EqualFold(p.AccountID, scope.Account) || !strings.EqualFold(p.AccountMode, scope.Mode) {
					continue
				}
				if p.OrderRef != reference && p.PreviewTokenID != token {
					continue
				}
				if p.OrderRef != reference || p.PreviewTokenID != token || !edgeProtectionBucket(p.Bucket) || p.At.IsZero() || p.At.After(trade.ExecutedAt) || p.Key == "" || p.Revision == "" || p.PolicyID == "" || p.PolicyVersion <= 0 || p.PolicyFingerprint.Key == "" || p.PolicyFingerprint.Version == "" {
					valid = false
					break
				}
				identityRaw, _ := json.Marshal(struct {
					Key, Revision, PolicyID string
					Version                 int
					Fingerprint             rpc.Fingerprint
				}{p.Key, p.Revision, p.PolicyID, p.PolicyVersion, p.PolicyFingerprint})
				identity := sha256.Sum256(identityRaw)
				record := edgecore.ProtectionRecord{Bucket: p.Bucket, At: p.At, Identity: hex.EncodeToString(identity[:])}
				if count > 0 && record != match {
					valid = false
					break
				}
				match = record
				count++
			}
			if valid && count > 0 {
				out[trade.RecordID] = match
			}
		}
	}
	return out
}

func edgeProtectionBucket(bucket string) bool {
	switch bucket {
	case rpc.TradeProposalBucketThetaHygiene, rpc.TradeProposalBucketRiskReduction, rpc.TradeProposalBucketTrailingStop, rpc.TradeProposalBucketOptionLossExit, rpc.TradeProposalBucketOptionExpiryClose, rpc.TradeProposalBucketOptionExitReview:
		return true
	default:
		return false
	}
}
