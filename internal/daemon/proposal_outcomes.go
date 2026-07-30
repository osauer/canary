package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	proposalOutcomeFileVersion = 1

	proposalOutcomeStateSubmitted = "submitted"
	proposalOutcomeStateFilled    = "filled"
	proposalOutcomeStateMarked    = "marked"
)

type proposalOutcomeStore struct {
	Path        string // legacy unit/import helper only
	core        *corestore.Store
	mu          sync.Mutex
	outcomeKeys map[string]struct{}
}

type proposalOutcomeMark struct {
	Version            int                                 `json:"version"`
	At                 time.Time                           `json:"at"`
	MarkDate           string                              `json:"mark_date"`
	State              string                              `json:"state"`
	ProposalKey        string                              `json:"proposal_key,omitempty"`
	Revision           string                              `json:"revision,omitempty"`
	Bucket             string                              `json:"bucket,omitempty"`
	Symbol             string                              `json:"symbol,omitempty"`
	SecType            string                              `json:"sec_type,omitempty"`
	Action             string                              `json:"action,omitempty"`
	Quantity           float64                             `json:"quantity,omitempty"`
	OrderRef           string                              `json:"order_ref,omitempty"`
	PreviewTokenID     string                              `json:"preview_token_id,omitempty"`
	ExecID             string                              `json:"exec_id,omitempty"`
	PolicyID           string                              `json:"policy_id,omitempty"`
	PolicyVersion      int                                 `json:"policy_version,omitempty"`
	PolicyFingerprint  rpc.Fingerprint                     `json:"policy_fingerprint,omitzero"`
	SourceFingerprints rpc.TradeProposalSourceFingerprints `json:"source_fingerprints,omitzero"`
	BaselinePrice      float64                             `json:"baseline_price,omitempty"`
	MarkPrice          float64                             `json:"mark_price,omitempty"`
	AvgFillPrice       float64                             `json:"avg_fill_price,omitempty"`
	ExecutionPnL       float64                             `json:"execution_pnl,omitempty"`
	BenchmarkSymbol    string                              `json:"benchmark_symbol,omitempty"`
	Message            string                              `json:"message,omitempty"`
}

func defaultProposalOutcomesPath() (string, error) {
	return defaultTradingStatePath("trade-proposal-outcomes.jsonl")
}

func newProposalOutcomeStore(path string) *proposalOutcomeStore {
	return &proposalOutcomeStore{Path: path}
}

func (s *Server) installProposalOutcomeStore() {
	path, err := defaultProposalOutcomesPath()
	if err != nil {
		s.warnf("trade proposal outcomes: resolve state path: %v (outcome book disabled)", err)
		return
	}
	s.proposalOutcomes = newProposalOutcomeStore(path)
}

func (s *proposalOutcomeStore) AppendMark(mark proposalOutcomeMark) error {
	if s == nil || (s.core == nil && s.Path == "") {
		return fmt.Errorf("proposal outcome path is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendMarkLocked(mark)
}

func (s *proposalOutcomeStore) appendMarkLocked(mark proposalOutcomeMark) error {
	if mark.At.IsZero() {
		mark.At = time.Now().UTC()
	}
	if mark.MarkDate == "" {
		mark.MarkDate = mark.At.Format(time.DateOnly)
	}
	if mark.Version == 0 {
		mark.Version = proposalOutcomeFileVersion
	}
	if strings.TrimSpace(mark.State) == "" {
		return fmt.Errorf("proposal outcome state is required")
	}
	if strings.TrimSpace(mark.ProposalKey) == "" && strings.TrimSpace(mark.OrderRef) == "" && strings.TrimSpace(mark.PreviewTokenID) == "" {
		return fmt.Errorf("proposal outcome requires proposal_key, order_ref, or preview_token_id")
	}
	if s.outcomeKeys == nil {
		outcomeKeys, err := s.loadOutcomeKeysLocked()
		if err != nil {
			return err
		}
		s.outcomeKeys = outcomeKeys
	}
	identity := proposalOutcomeIdentity(mark)
	if _, seen := s.outcomeKeys[identity]; seen {
		return nil
	}
	data, err := json.Marshal(mark)
	if err != nil {
		return fmt.Errorf("marshal proposal outcome: %w", err)
	}
	if s.core != nil {
		key, err := coreStoreEventKey(context.Background(), s.core, coreEventProposalOutcome, mark.At, data, 0)
		if err != nil {
			return err
		}
		if _, err := s.core.AppendEvents(context.Background(), []corestore.EventInput{{
			ScopeKey: daemonStateScope, EventKey: key, Type: coreEventProposalOutcome,
			Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
			OccurredAt: mark.At, PayloadJSON: data,
			Projection: corestore.EventProjection{ProposalOutcome: &corestore.ProposalOutcomeProjection{
				ProposalKey: proposalOutcomeSubject(mark), Revision: mark.Revision,
				Bucket: mark.Bucket, Symbol: mark.Symbol, SecType: mark.SecType,
				Action: mark.Action, State: mark.State,
			}},
		}}); err != nil {
			return fmt.Errorf("append proposal outcome to SQLite: %w", err)
		}
		s.outcomeKeys[identity] = struct{}{}
		return nil
	}
	if err := ensurePrivateStateDir(s.Path); err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open proposal outcomes %s: %w", s.Path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod proposal outcomes: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		s.outcomeKeys = nil
		return fmt.Errorf("append proposal outcome: %w", err)
	}
	s.outcomeKeys[identity] = struct{}{}
	return nil
}

func (s *proposalOutcomeStore) loadOutcomeKeysLocked() (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if s.core != nil {
		events, err := loadAllCoreEvents(context.Background(), s.core, coreEventProposalOutcome)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			var mark proposalOutcomeMark
			if err := json.Unmarshal(event.PayloadJSON, &mark); err != nil {
				return nil, fmt.Errorf("decode proposal outcome event %d: %w", event.EventSeq, err)
			}
			out[proposalOutcomeIdentity(mark)] = struct{}{}
		}
		return out, nil
	}
	f, err := os.Open(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("open proposal outcomes %s: %w", s.Path, err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var mark proposalOutcomeMark
		if err := json.Unmarshal([]byte(line), &mark); err != nil {
			return nil, fmt.Errorf("decode proposal outcome: %w", err)
		}
		out[proposalOutcomeIdentity(mark)] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read proposal outcomes: %w", err)
	}
	return out, nil
}

// SessionSummary reports, for the most recent recorded session in the journal,
// how many distinct protection proposals were offered versus acted on. It reads
// the journal read-only and never mutates the append cache. "Offered" is any
// distinct proposal with a recorded outcome that day (marks are shadow-hold
// offers); "acted" is a proposal that was submitted or filled. Raw proposal
// identities are used only to count distinct subjects and never returned.
func (s *proposalOutcomeStore) SessionSummary() (offered, acted int, day string, ok bool, err error) {
	if s == nil || (s.core == nil && s.Path == "") {
		return 0, 0, "", false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var marks []proposalOutcomeMark
	if s.core != nil {
		events, loadErr := loadAllCoreEvents(context.Background(), s.core, coreEventProposalOutcome)
		if loadErr != nil {
			return 0, 0, "", false, loadErr
		}
		for _, event := range events {
			var mark proposalOutcomeMark
			if json.Unmarshal(event.PayloadJSON, &mark) == nil {
				marks = append(marks, mark)
			}
		}
	} else {
		f, err := os.Open(s.Path)
		if err != nil {
			if os.IsNotExist(err) {
				return 0, 0, "", false, nil
			}
			return 0, 0, "", false, fmt.Errorf("open proposal outcomes %s: %w", s.Path, err)
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var mark proposalOutcomeMark
			if json.Unmarshal([]byte(line), &mark) == nil {
				marks = append(marks, mark)
			}
		}
		if err := sc.Err(); err != nil {
			return 0, 0, "", false, fmt.Errorf("read proposal outcomes: %w", err)
		}
	}
	offeredByDay := map[string]map[string]struct{}{}
	actedByDay := map[string]map[string]struct{}{}
	latest := ""
	for _, mark := range marks {
		subject := proposalOutcomeSubject(mark)
		if subject == "" || mark.MarkDate == "" {
			continue
		}
		if mark.MarkDate > latest {
			latest = mark.MarkDate
		}
		if offeredByDay[mark.MarkDate] == nil {
			offeredByDay[mark.MarkDate] = map[string]struct{}{}
			actedByDay[mark.MarkDate] = map[string]struct{}{}
		}
		offeredByDay[mark.MarkDate][subject] = struct{}{}
		if mark.State == proposalOutcomeStateSubmitted || mark.State == proposalOutcomeStateFilled {
			actedByDay[mark.MarkDate][subject] = struct{}{}
		}
	}
	if latest == "" {
		return 0, 0, "", false, nil
	}
	return len(offeredByDay[latest]), len(actedByDay[latest]), latest, true, nil
}

// proposalOutcomeSubject is a stable per-proposal identity used only for
// distinct-count aggregation. It never reaches any wire contract.
func proposalOutcomeSubject(mark proposalOutcomeMark) string {
	if key := strings.TrimSpace(mark.ProposalKey); key != "" {
		return key
	}
	if ref := strings.TrimSpace(mark.OrderRef); ref != "" {
		return "order:" + ref
	}
	return strings.TrimSpace(mark.PreviewTokenID)
}

func proposalOutcomeIdentity(mark proposalOutcomeMark) string {
	return strings.Join([]string{
		strings.TrimSpace(mark.ProposalKey),
		strings.TrimSpace(mark.MarkDate),
		strings.TrimSpace(mark.State),
		strings.TrimSpace(mark.OrderRef),
		strings.TrimSpace(mark.PreviewTokenID),
		strings.TrimSpace(mark.ExecID),
	}, "|")
}

func proposalOutcomeSubmitted(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult, place *rpc.OrderPlaceResult, at time.Time) proposalOutcomeMark {
	var orderRef, tokenID string
	var baseline float64
	var qty float64
	if preview != nil {
		orderRef = preview.Draft.OrderRef
		tokenID = preview.PreviewTokenID
		baseline = preview.Draft.LimitPrice
		qty = float64(preview.Draft.Quantity)
	}
	if place != nil {
		if place.OrderRef != "" {
			orderRef = place.OrderRef
		}
		if place.PreviewTokenID != "" {
			tokenID = place.PreviewTokenID
		}
		if qty == 0 {
			qty = float64(place.Draft.Quantity)
		}
	}
	return proposalOutcomeMark{
		At:                 at,
		MarkDate:           at.Format(time.DateOnly),
		State:              proposalOutcomeStateSubmitted,
		ProposalKey:        prop.Key,
		Revision:           prop.Revision,
		Bucket:             prop.Bucket,
		Symbol:             prop.Symbol,
		SecType:            prop.SecType,
		Action:             prop.Action,
		Quantity:           qty,
		OrderRef:           orderRef,
		PreviewTokenID:     tokenID,
		PolicyID:           prop.PolicyID,
		PolicyVersion:      prop.PolicyVersion,
		PolicyFingerprint:  prop.PolicyFingerprint,
		SourceFingerprints: prop.SourceFingerprints,
		BaselinePrice:      baseline,
		BenchmarkSymbol:    "SPY",
		Message:            "proposal submitted; daily shadow-hold marks will be recorded",
	}
}

func proposalOutcomeMarked(prop rpc.TradeProposal, at time.Time) proposalOutcomeMark {
	markPrice := 0.0
	if prop.LimitPrice != nil {
		markPrice = *prop.LimitPrice
	}
	return proposalOutcomeMark{
		At:                 at,
		MarkDate:           at.Format(time.DateOnly),
		State:              proposalOutcomeStateMarked,
		ProposalKey:        prop.Key,
		Revision:           prop.Revision,
		Bucket:             prop.Bucket,
		Symbol:             prop.Symbol,
		SecType:            prop.SecType,
		Action:             prop.Action,
		Quantity:           float64(prop.Quantity),
		PolicyID:           prop.PolicyID,
		PolicyVersion:      prop.PolicyVersion,
		PolicyFingerprint:  prop.PolicyFingerprint,
		SourceFingerprints: prop.SourceFingerprints,
		BaselinePrice:      markPrice,
		MarkPrice:          markPrice,
		BenchmarkSymbol:    "SPY",
		Message:            "daily shadow-hold proposal mark",
	}
}

func proposalOutcomeFilledFromJournal(ev orderJournalEvent, submitted proposalEvent, at time.Time) proposalOutcomeMark {
	qty := ev.Filled
	if qty == 0 {
		qty = ev.Quantity
	}
	executionPnL := 0.0
	if ev.AvgFillPrice > 0 && ev.LimitPrice > 0 && qty != 0 {
		switch strings.ToUpper(ev.Action) {
		case rpc.OrderActionSell:
			executionPnL = (ev.AvgFillPrice - ev.LimitPrice) * qty * float64(max(ev.Multiplier, 1))
		case rpc.OrderActionBuy:
			executionPnL = (ev.LimitPrice - ev.AvgFillPrice) * qty * float64(max(ev.Multiplier, 1))
		}
	}
	return proposalOutcomeMark{
		At:                 at,
		MarkDate:           at.Format(time.DateOnly),
		State:              proposalOutcomeStateFilled,
		ProposalKey:        submitted.Key,
		Revision:           submitted.Revision,
		Bucket:             submitted.Bucket,
		Symbol:             ev.Symbol,
		SecType:            ev.SecType,
		Action:             ev.Action,
		Quantity:           qty,
		OrderRef:           ev.OrderRef,
		PreviewTokenID:     ev.PreviewTokenID,
		ExecID:             ev.ExecID,
		PolicyID:           submitted.PolicyID,
		PolicyVersion:      submitted.PolicyVersion,
		PolicyFingerprint:  submitted.PolicyFingerprint,
		SourceFingerprints: submitted.SourceFingerprints,
		BaselinePrice:      ev.LimitPrice,
		AvgFillPrice:       ev.AvgFillPrice,
		ExecutionPnL:       executionPnL,
		BenchmarkSymbol:    "SPY",
		Message:            "proposal order fill observed from order journal lifecycle",
	}
}
