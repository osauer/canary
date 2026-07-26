package apphttp

import (
	"context"
	"strings"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

// BrokerWriteConfirmation carries a paired user's account/mode affirmation.
// Matching it is necessary request validation, not broker-write authority;
// the daemon's current policy, freeze, preview, and execution gates remain
// binding.
type BrokerWriteConfirmation struct {
	ConfirmAccount string `json:"confirm_account,omitempty"`
	ConfirmMode    string `json:"confirm_mode,omitempty"`
}

func (h *handler) requireBrokerWriteConfirmation(ctx context.Context, req BrokerWriteConfirmation) (*rpc.TradingStatus, error) {
	return h.requireBrokerConfirmation(ctx, req, true)
}

func (h *handler) requireBrokerSessionConfirmation(ctx context.Context, req BrokerWriteConfirmation) (*rpc.TradingStatus, error) {
	return h.requireBrokerConfirmation(ctx, req, false)
}

// requireBrokerCancelConfirmation validates the same confirm_account /
// confirm_mode affirmation as requireBrokerWriteConfirmation but does not
// require CanWrite: a runtime trading freeze must never strand an open order
// that needs cancelling. Write-policy authorization stays daemon-side, where
// the cancel path strips only the freeze blocker (forCancel) and every other
// blocker still rejects.
func (h *handler) requireBrokerCancelConfirmation(ctx context.Context, req BrokerWriteConfirmation) (*rpc.TradingStatus, error) {
	return h.requireBrokerConfirmation(ctx, req, false)
}

func (h *handler) requireBrokerConfirmation(ctx context.Context, req BrokerWriteConfirmation, requireCanWrite bool) (*rpc.TradingStatus, error) {
	status, err := h.deps.Daemon.TradingStatus(ctx)
	if err != nil {
		return nil, err
	}
	if requireCanWrite && status.Mode == config.TradingModeDisabled {
		return nil, &rpc.Error{Code: rpc.CodeTradingDisabled, Message: "trading is disabled"}
	}
	if requireCanWrite && !status.CanWrite {
		return nil, &rpc.Error{Code: rpc.CodeTradingDisabled, Message: "broker writes are not enabled by trading.status"}
	}
	if strings.TrimSpace(status.Account) == "" || strings.TrimSpace(status.Mode) == "" {
		return nil, &rpc.Error{Code: rpc.CodeBadRequest, Message: "current trading account and mode are required"}
	}
	if !strings.EqualFold(strings.TrimSpace(req.ConfirmAccount), strings.TrimSpace(status.Account)) {
		return nil, &rpc.Error{Code: rpc.CodeBadRequest, Message: "confirm_account must match current trading account"}
	}
	if !strings.EqualFold(strings.TrimSpace(req.ConfirmMode), strings.TrimSpace(status.Mode)) {
		return nil, &rpc.Error{Code: rpc.CodeBadRequest, Message: "confirm_mode must match current trading mode"}
	}
	return status, nil
}
