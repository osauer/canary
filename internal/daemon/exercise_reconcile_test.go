package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func exerciseReconcileFixture(t *testing.T) (*Server, rpc.ExerciseReconcileParams, orderJournalEvent) {
	t.Helper()
	s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	s.cfg.Flex = config.Flex{Enabled: true, QueryID: "synthetic-query"}
	at := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return at.Add(time.Hour) }
	e := orderJournalEvent{At: at, Type: orderJournalEventSendAttempted, Endpoint: "127.0.0.1:4002", ClientID: 31, ActionKind: corestore.ActionExercise, Action: rpc.OpportunityActionExercise, Account: "DU1234567", Mode: rpc.AccountModePaper, PreviewTokenID: "exercise-token", OrderRef: "exercise-request", AttemptID: "exercise-attempt", Symbol: "SYN", ConID: 101, Quantity: 1, ExerciseReservation: &exerciseReservation{Option: rpc.ContractParams{ConID: 101, Symbol: "SYN", SecType: "OPT", Multiplier: 100}, Underlying: rpc.ContractParams{ConID: 102, Symbol: "SYN", SecType: "STK"}, Quantity: 1, UnderlyingAction: rpc.OrderActionSell, UnderlyingBefore: 100, UnderlyingAfter: 0, OptionBefore: 1, PortfolioGeneration: 1}}
	if err := s.orderJournal.Append(e); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("authenticated-synthetic-active-query-file"))
	file := corestore.StatementFileRecord{FileKey: "synthetic.xml", SHA256: digest, Status: statementProjectionStatus}
	option := flexstmt.OptionEvent{AccountID: e.Account, ConID: 101, UnderlyingConID: 102, AssetClass: "OPT", TransactionType: "EXERCISE", Quantity: new(1.0), TradeID: "trade-one"}
	trade := flexstmt.Trade{AccountID: e.Account, ConID: 101, UnderlyingConID: 102, AssetClass: "OPT", Side: "SELL", OpenClose: "C", Quantity: new(-1.0), TradeID: "trade-one", ExecutionID: "execution-one", ExecutedAt: at.Add(time.Minute)}
	optionRaw, _ := json.Marshal(option)
	tradeRaw, _ := json.Marshal(trade)
	records := []corestore.StatementRecord{{Kind: corestore.StatementRecordOptionEvent, RecordKey: "option-one", AccountKey: e.Account, EffectiveAt: at, GeneratedAt: at.Add(time.Hour), StatementFileKey: file.FileKey, StatementFileSHA256: digest, RawJSON: optionRaw}, {Kind: corestore.StatementRecordTrade, RecordKey: "trade-one", AccountKey: e.Account, EffectiveAt: trade.ExecutedAt, GeneratedAt: at.Add(time.Hour), StatementFileKey: file.FileKey, StatementFileSHA256: digest, RawJSON: tradeRaw}}
	if err := s.coreStore.ReplaceStatementProjection(t.Context(), s.activeStatementProjectionScope(), []corestore.StatementFileRecord{file}, nil, records, nil); err != nil {
		t.Fatal(err)
	}
	s.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkrlib.RawPosition, error) { return nil, nil }
	s.stableBrokerEvidenceForTest = func(_ daemonBrokerEvidenceBinding, commit func() error) (bool, error) {
		err := commit()
		return err == nil, err
	}
	list, err := s.handleOpportunitiesExercises(t.Context())
	if err != nil || len(list.Requests) != 1 || len(list.Requests[0].Evidence) != 1 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	row := list.Requests[0]
	return s, rpc.ExerciseReconcileParams{RequestRef: row.RequestRef, Revision: row.Revision, EvidenceID: row.Evidence[0].ID, ConfirmComplete: true, Origin: rpc.OrderOriginHumanTTY}, e
}

func reconcileExerciseForTest(t *testing.T, s *Server, p rpc.ExerciseReconcileParams) (*rpc.ExerciseReconcileResult, error) {
	t.Helper()
	raw, _ := json.Marshal(p)
	return s.handleOpportunitiesReconcileExercise(t.Context(), &rpc.Request{Params: raw})
}

func TestExerciseReconcilePersistsEvidenceAndScopesReplay(t *testing.T) {
	s, p, e := exerciseReconcileFixture(t)
	result, err := reconcileExerciseForTest(t, s, p)
	if err != nil || !result.Accepted || result.Replay || result.Provenance != exerciseHumanEvidenceProvenance {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result, err = reconcileExerciseForTest(t, s, p)
	if err != nil || !result.Replay {
		t.Fatalf("replay=%+v err=%v", result, err)
	}
	// Recreate the read authority facade; the resolution does not depend on a cache.
	restarted := &Server{coreStore: s.coreStore, orderJournal: s.orderJournal}
	rows, err := restarted.pendingExercises(t.Context(), s.currentBrokerStateScope())
	if err != nil || len(rows) != 0 {
		t.Fatalf("durable resolution rows=%v err=%v", rows, err)
	}
	// An old source event cannot be spent a second time for another instruction.
	e.PreviewTokenID = "second-token"
	e.OrderRef = "second-request"
	e.AttemptID = "second-attempt"
	if err := s.orderJournal.Append(e); err != nil {
		t.Fatal(err)
	}
	list, err := s.handleOpportunitiesExercises(t.Context())
	if err != nil || len(list.Requests) != 1 || len(list.Requests[0].Evidence) != 0 {
		t.Fatalf("evidence reused list=%+v err=%v", list, err)
	}
	s.cfg.Gateway.Account = "DU0000000"
	if result, err := reconcileExerciseForTest(t, s, p); err == nil || result != nil && result.Accepted {
		t.Fatalf("cross-account replay accepted=%+v err=%v", result, err)
	}
}

func TestExerciseReconcileRefusesUnconfirmedAndChangedAuthority(t *testing.T) {
	for _, scenario := range []string{"agent", "missing-confirmation", "revision", "evidence", "positions", "broker-frontier", "authority-head", "ambiguous", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			s, p, e := exerciseReconcileFixture(t)
			switch scenario {
			case "agent":
				p.Origin = rpc.OrderOriginAgent
			case "missing-confirmation":
				p.ConfirmComplete = false
			case "revision":
				p.Revision = "changed"
			case "evidence":
				p.EvidenceID = "changed"
			case "positions":
				s.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkrlib.RawPosition, error) {
					return []*ibkrlib.RawPosition{{Account: e.Account, Contract: *previewIBKRContract(e.ExerciseReservation.Underlying), Position: 100}}, nil
				}
			case "broker-frontier":
				s.stableBrokerEvidenceForTest = func(daemonBrokerEvidenceBinding, func() error) (bool, error) { return false, nil }
			case "authority-head":
				s.stableBrokerEvidenceForTest = func(_ daemonBrokerEvidenceBinding, commit func() error) (bool, error) {
					_, err := s.coreStore.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: "concurrent-evidence", JSON: []byte(`{}`)})
					if err != nil {
						t.Fatal(err)
					}
					err = commit()
					return err == nil, err
				}
			case "ambiguous":
				e.AttemptID = "other-attempt"
				e.PreviewTokenID = "other-token"
				e.OrderRef = "other-request"
				if err := s.orderJournal.Append(e); err != nil {
					t.Fatal(err)
				}
			case "legacy":
				e.ExerciseReservation = nil
				if err := s.orderJournal.Append(e); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := reconcileExerciseForTest(t, s, p); err == nil || result != nil && result.Accepted {
				t.Fatalf("unsafe reconciliation accepted=%+v err=%v", result, err)
			}
			rows, err := s.pendingExercises(t.Context(), s.currentBrokerStateScope())
			if err != nil || len(rows) == 0 {
				t.Fatalf("failed reconciliation released reservation rows=%v err=%v", rows, err)
			}
			resolutions, _, err := s.exerciseResolutions(t.Context())
			if err != nil || len(resolutions.Resolutions) != 0 {
				t.Fatalf("failed reconciliation consumed evidence %+v err=%v", resolutions, err)
			}
		})
	}
}

func TestExerciseEvidenceRejectsPartialWrongScopeAndUntimedTrade(t *testing.T) {
	for _, scenario := range []string{"partial", "other-account", "other-option", "other-underlying", "old", "date-only", "cancellation", "missing-digest", "missing-execution"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, _ := exerciseReconcileFixture(t)
			pending, err := s.pendingExercises(t.Context(), s.currentBrokerStateScope())
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := s.exerciseEvidenceSnapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for i := range snapshot.Records {
				row := &snapshot.Records[i]
				if row.Kind != corestore.StatementRecordTrade {
					continue
				}
				var trade flexstmt.Trade
				if err := json.Unmarshal(row.RawJSON, &trade); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "partial":
					trade.Quantity = new(-0.5)
				case "other-account":
					trade.AccountID = "DU0000000"
				case "other-option":
					trade.ConID++
				case "other-underlying":
					trade.UnderlyingConID++
				case "old":
					trade.ExecutedAt = pending[0].Event.At.Add(-time.Second)
				case "date-only":
					trade.ExecutedAt = time.Date(2026, 5, 28, 0, 0, 0, 0, flexstmt.StatementLocation())
				case "cancellation":
					trade.Side = "CANCEL"
				case "missing-digest":
					row.StatementFileSHA256 = [32]byte{}
				case "missing-execution":
					trade.ExecutionID = ""
				}
				row.RawJSON, _ = json.Marshal(trade)
			}
			if got := matchingExerciseEvidence(pending[0], snapshot, exerciseResolutionDocument{}); len(got) != 0 {
				t.Fatalf("invalid evidence accepted %+v", got)
			}
		})
	}
}
