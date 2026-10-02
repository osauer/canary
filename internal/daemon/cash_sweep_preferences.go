package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const cashPriorityReceiptType = "cash_sweep_priority_saved"

var cashPriorityRequestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func nullableCashSweepPriority(raw json.RawMessage) (*string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errBadRequest("cash_sweep.currency_priority must be usd_first, balanced, eur_first, or null")
	}
	switch value {
	case rpc.CashSweepPriorityUSDFirst, rpc.CashSweepPriorityBalanced, rpc.CashSweepPriorityEURFirst:
		return &value, nil
	}
	return nil, errBadRequest("cash_sweep.currency_priority must be usd_first, balanced, eur_first, or null")
}

func (d *platformCashSweepSettingsData) UnmarshalJSON(raw []byte) error {
	var fields struct {
		CurrencyPriority json.RawMessage `json:"currency_priority"`
	}
	if err := decodeStrictPlatformSettingsJSON(raw, &fields); err != nil {
		return err
	}
	if len(fields.CurrencyPriority) == 0 {
		d.CurrencyPriority = nil
		return nil
	}
	v, err := nullableCashSweepPriority(fields.CurrencyPriority)
	if err != nil {
		return err
	}
	d.CurrencyPriority = v
	return nil
}

func cashPriorityEffective(override *string, policy *protectionCashSweepPolicy) (value, source string) {
	if override != nil {
		return *override, rpc.SettingsSourceRuntime
	}
	if policy != nil && policy.CurrencyPriority != "" {
		return policy.CurrencyPriority, rpc.SettingsSourceConfig
	}
	return rpc.CashSweepPriorityUSDFirst, "default"
}

func (s *Server) platformCashSweepSettings(data platformSettingsData) rpc.PlatformCashSweepSettings {
	policy, _ := s.protectionPolicies.Active()
	value, source := cashPriorityEffective(data.CashSweep.CurrencyPriority, policy.Buckets.CashSweep)
	return rpc.PlatformCashSweepSettings{CurrencyPriority: settingsString(value, rpc.SettingsAccessWrite, source, "ordering only; reserve policy, cash readiness and execution authority are unchanged")}
}

func (s *Server) cashPreferencesLocked() rpc.CashSweepPreferences {
	policy, _ := s.protectionPolicies.Active()
	value, source := cashPriorityEffective(s.platformSettings.data.CashSweep.CurrencyPriority, policy.Buckets.CashSweep)
	var configured *string
	if s.platformSettings.data.CashSweep.CurrencyPriority != nil {
		value := *s.platformSettings.data.CashSweep.CurrencyPriority
		configured = &value
	}
	return rpc.CashSweepPreferences{Revision: s.platformSettings.revision, CurrencyPriority: configured,
		EffectivePriority: value, Source: source, Writable: s.platformSettings.core != nil && s.platformSettings.core.Health().Ready, AsOf: s.orderNow()}
}

func (s *Server) handleCashSweepPreferences() (*rpc.CashSweepPreferences, error) {
	ctx, cancel := context.WithTimeout(context.Background(), unaryDeadline(rpc.MethodCashSweepPreferencesGet))
	defer cancel()
	return s.handleCashSweepPreferencesContext(ctx)
}

func (s *Server) handleCashSweepPreferencesContext(ctx context.Context) (*rpc.CashSweepPreferences, error) {
	if s.platformSettings == nil {
		return nil, errBadRequest("runtime settings unavailable")
	}
	store := s.platformSettings
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.core != nil && store.core.Health().Ready {
		if out, err := s.acceptCashPriorityLocked(ctx, "", 0, false); err == nil {
			return out, nil
		}
	}
	out := s.cashPreferencesLocked()
	out.Writable = false
	out.Reason = "Durable settings unavailable; previous accepted priority shown"
	return &out, nil
}

type cashPriorityReceipt struct {
	Version       int                             `json:"version"`
	Request       rpc.SetCashSweepPriorityRequest `json:"request"`
	RequestDigest string                          `json:"request_digest"`
	Before        *string                         `json:"before"`
	SavedRevision int64                           `json:"saved_revision"`
}

func (s *Server) handleCashSweepPrioritySet(ctx context.Context, req *rpc.Request) (*rpc.CashSweepPreferences, error) {
	if len(req.Params) > 2048 {
		return nil, errBadRequest("cash priority request too large")
	}
	var wire struct {
		CurrencyPriority json.RawMessage `json:"currency_priority"`
		ExpectedRevision *int64          `json:"expected_revision"`
		RequestID        string          `json:"request_id"`
	}
	if err := decodeStrictPlatformSettingsJSON(req.Params, &wire); err != nil {
		return nil, errBadRequest("invalid cash priority request")
	}
	if len(wire.CurrencyPriority) == 0 || wire.ExpectedRevision == nil || *wire.ExpectedRevision < 0 || !cashPriorityRequestID.MatchString(wire.RequestID) {
		return nil, errBadRequest("cash priority, expected revision and immutable request id required")
	}
	priority, err := nullableCashSweepPriority(wire.CurrencyPriority)
	if err != nil {
		return nil, err
	}
	in := rpc.SetCashSweepPriorityRequest{CurrencyPriority: priority, ExpectedRevision: *wire.ExpectedRevision, RequestID: wire.RequestID}
	rawRequest, _ := json.Marshal(in)
	digest := sha256.Sum256(rawRequest)
	requestDigest := hex.EncodeToString(digest[:])
	idHash := sha256.Sum256([]byte(in.RequestID))
	eventKey := "cash-sweep-priority:" + hex.EncodeToString(idHash[:])
	store := s.platformSettings
	if store == nil {
		return nil, errBadRequest("runtime settings unavailable")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.core == nil || !store.core.Health().Ready {
		return nil, fmt.Errorf("cash priority durability unavailable")
	}
	event, found, err := store.core.GetEvent(ctx, daemonStateScope, eventKey)
	if err != nil {
		return nil, fmt.Errorf("cash priority receipt unavailable")
	}
	if found {
		var receipt cashPriorityReceipt
		if event.Type != cashPriorityReceiptType || event.Origin != rpc.OrderOriginAgent || decodeStrictPlatformSettingsJSON(event.PayloadJSON, &receipt) != nil || receipt.Version != 1 {
			return nil, fmt.Errorf("invalid cash priority receipt")
		}
		canonical, _ := json.Marshal(receipt.Request)
		verified := sha256.Sum256(canonical)
		if hex.EncodeToString(verified[:]) != receipt.RequestDigest {
			return nil, fmt.Errorf("cash priority receipt request digest mismatch")
		}
		if receipt.RequestDigest != requestDigest {
			return nil, errBadRequest("request id already names different cash priority terms")
		}
		return s.acceptCashPriorityLocked(ctx, in.RequestID, receipt.SavedRevision, true)
	}
	if in.ExpectedRevision != store.revision {
		return nil, &rpc.Error{Code: rpc.CodeSettingsConflict, Message: "Cash priority changed; refresh settings before saving"}
	}
	next := store.data
	next.Version = platformSettingsDocVersion
	next.CashSweep.CurrencyPriority = priority
	before, _ := json.Marshal(store.data.CashSweep.CurrencyPriority)
	after, _ := json.Marshal(priority)
	changed := !bytes.Equal(before, after)
	revision := store.revision
	if changed {
		revision++
	}
	rawReceipt, _ := json.Marshal(cashPriorityReceipt{Version: 1, Request: in, RequestDigest: requestDigest, Before: store.data.CashSweep.CurrencyPriority, SavedRevision: revision})
	input := corestore.EventInput{ScopeKey: daemonStateScope, EventKey: eventKey, Type: cashPriorityReceiptType, Action: coreEventActionUpdate, Origin: rpc.OrderOriginAgent, OccurredAt: s.orderNow().UTC(), PayloadJSON: rawReceipt}
	cas := corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindPlatformSettings, ExpectedRevision: store.revision}
	if changed {
		cas.JSON, err = json.Marshal(next)
		if err != nil {
			return nil, err
		}
		_, _, err = store.core.CompareAndSwapStateDocumentWithEvents(ctx, cas, []corestore.EventInput{input})
	} else {
		_, err = store.core.AppendEventsAtStateRevision(ctx, cas, []corestore.EventInput{input})
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nil, &rpc.Error{Code: rpc.CodeSettingsConflict, Message: "Cash priority changed; refresh settings before saving"}
	}
	if err != nil {
		return nil, fmt.Errorf("cash priority save unconfirmed")
	}
	return s.acceptCashPriorityLocked(ctx, in.RequestID, revision, false)
}

// The settings mutex is held by the caller. The authority write lock keeps a
// different writer's watermark failure from racing publication of this view.
func (s *Server) acceptCashPriorityLocked(ctx context.Context, requestID string, savedRevision int64, replay bool) (*rpc.CashSweepPreferences, error) {
	store := s.platformSettings
	var out rpc.CashSweepPreferences
	err := store.core.WithAcceptedStateDocument(ctx, daemonStateScope, stateKindPlatformSettings, func(doc corestore.StateDocument, found bool) error {
		if found {
			data, err := decodePlatformSettings(doc.JSON)
			if err != nil {
				return err
			}
			store.data, store.revision = data, doc.Revision
		} else if store.revision != 0 {
			return fmt.Errorf("runtime settings disappeared")
		}
		out = s.cashPreferencesLocked()
		out.RequestID, out.SavedRevision, out.Replay = requestID, savedRevision, replay
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cash priority durability unconfirmed")
	}
	return &out, nil
}

// currentCashSweepPriority is consumed only after planning and reserve gates.
func (s *platformSettingsStore) currentCashSweepPriority() *string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data.CashSweep.CurrencyPriority == nil {
		return nil
	}
	v := *s.data.CashSweep.CurrencyPriority
	return &v
}
