package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
)

// edgeProtectionPage is the page size of the protection evidence reads. A
// read pages to the end of the journal; a full page is never the end.
const edgeProtectionPage = 10_000

// edgeProtectionJournal keeps what the protection link reads from Canary's
// own records: the order-journal events that carry an execution id and the
// submitted proposal records. The order journal holds one event per order
// status change, so it is read once in pages and afterwards only from the
// last event read; nothing is read while the authority head stays put. A new
// authority epoch, a different store or a head that moved back starts over.
type edgeProtectionJournal struct {
	mu          sync.Mutex
	store       *corestore.Store
	head        corestore.AuthorityHead
	loaded      bool
	orderSeq    int64
	proposalSeq int64
	executions  []edgeExecutionEvent
	submitted   []proposalEvent
}

type edgeExecutionEvent struct {
	atMS int64
	ev   orderJournalEvent
}

func (s *Server) edgeProtectionEvidence(ctx context.Context, scope brokerStateScope) ([]orderJournalEvent, []proposalEvent, string) {
	if s == nil || s.coreStore == nil {
		return nil, nil, ""
	}
	fromMS := s.edgeNow().AddDate(0, 0, -edgeFullLookbackDays).Truncate(24 * time.Hour).UnixMilli()
	executions, submitted, ok := s.edgeProtection.read(ctx, s.coreStore, fromMS)
	if !ok {
		return nil, nil, ""
	}
	events := make([]orderJournalEvent, 0, len(executions))
	for _, ev := range executions {
		if orderJournalEventMatchesBrokerScope(ev, scope) {
			events = append(events, ev)
		}
	}
	proposals := make([]proposalEvent, 0, len(submitted))
	for _, p := range submitted {
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

// read returns the execution events at or after fromMS and every submitted
// proposal, or false when any page or row cannot be read: an incomplete read
// withholds the context and a prefix is never treated as complete.
func (c *edgeProtectionJournal) read(ctx context.Context, store *corestore.Store, fromMS int64) ([]orderJournalEvent, []proposalEvent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	head, err := store.AuthorityHead(ctx)
	if err != nil {
		return nil, nil, false
	}
	if c.store != store || head.AuthorityEpoch != c.head.AuthorityEpoch || head.LastEventSeq < c.head.LastEventSeq {
		c.store, c.head, c.loaded, c.orderSeq, c.proposalSeq, c.executions, c.submitted = store, corestore.AuthorityHead{}, false, 0, 0, nil, nil
	}
	if !c.loaded || head != c.head {
		executions, orderSeq := slices.Clip(c.executions), c.orderSeq
		for {
			page, err := store.LoadOrderEvents(ctx, corestore.OrderQuery{FromAtMS: fromMS, AfterEventSeq: orderSeq, Limit: edgeProtectionPage})
			if err != nil {
				return nil, nil, false
			}
			for _, record := range page {
				ev, err := decodeCoreOrderEvent(record)
				if err != nil {
					return nil, nil, false
				}
				if ev.ExecID != "" {
					executions = append(executions, edgeExecutionEvent{atMS: record.AtMS, ev: ev})
				}
			}
			if len(page) > 0 {
				orderSeq = page[len(page)-1].EventSeq
			}
			if len(page) < edgeProtectionPage {
				break
			}
		}
		// Proposal records have no time bound: a protective order can be
		// submitted long before it fills, and the link needs every submitted
		// record to prove a match is unique.
		submitted, proposalSeq := slices.Clip(c.submitted), c.proposalSeq
		for {
			page, err := store.LoadEvents(ctx, corestore.EventQuery{ScopeKey: daemonStateScope, Type: proposalCoreEventType, AfterEventSeq: proposalSeq, Limit: edgeProtectionPage})
			if err != nil {
				return nil, nil, false
			}
			for _, row := range page {
				var p proposalEvent
				if json.Unmarshal(row.PayloadJSON, &p) != nil || !proposalEventValid(p) || !p.At.Equal(row.OccurredAt) {
					return nil, nil, false
				}
				if p.Type == "submitted" {
					submitted = append(submitted, p)
				}
			}
			if len(page) > 0 {
				proposalSeq = page[len(page)-1].EventSeq
			}
			if len(page) < edgeProtectionPage {
				break
			}
		}
		// Every row at or below the head read before the queries was visible
		// to them, so neither stream rescans the rows other records appended.
		orderSeq, proposalSeq = max(orderSeq, head.LastEventSeq), max(proposalSeq, head.LastEventSeq)
		c.head, c.loaded, c.orderSeq, c.proposalSeq, c.executions, c.submitted = head, true, orderSeq, proposalSeq, executions, submitted
	}
	// The lookback only moves forward, so an execution that left it is dropped.
	kept := c.executions[:0:0]
	for _, ex := range c.executions {
		if ex.atMS >= fromMS {
			kept = append(kept, ex)
		}
	}
	c.executions = kept
	events := make([]orderJournalEvent, len(kept))
	for i, ex := range kept {
		events[i] = ex.ev
	}
	return events, append([]proposalEvent(nil), c.submitted...), true
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
