package canary

import (
	"context"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Desk automatic authority: the owner's device-confirmed standing mandate for
// Desk's controller. Like the cash policy methods these are outside the MCP
// catalogue and the CLI; only Desk's backend calls them. The controller
// capability passed to ControlDeskAuthority is private: never a browser,
// model, log or argv value. A daemon without these methods answers with an
// unknown-method error, which Desk treats as automatic trading unavailable.

// DeskAuthorityTerms are the exact mandate terms the owner confirms.
type DeskAuthorityTerms = rpc.DeskAuthorityTerms

// DeskAuthorityPrepareParams asks for terms at a maximum scope.
type DeskAuthorityPrepareParams = rpc.DeskAuthorityPrepareParams

// DeskAuthorityPrepared carries the canonical terms and their digest.
type DeskAuthorityPrepared = rpc.DeskAuthorityPrepared

// DeskAuthorityConfirmParams carries the terms and the device confirmation.
type DeskAuthorityConfirmParams = rpc.DeskAuthorityConfirmParams

// DeskAuthorityControlParams changes the running state under a generation fence.
type DeskAuthorityControlParams = rpc.DeskAuthorityControlParams

// DeskAuthorityStatus is the persisted and effective authority.
type DeskAuthorityStatus = rpc.DeskAuthorityStatus

// DeskAuthority reads the mandate's effective state, including a hold by the
// drawdown brake (HeldBy).
func (c *Client) DeskAuthority(ctx context.Context) (*DeskAuthorityStatus, error) {
	var out DeskAuthorityStatus
	if err := c.callPrivate(ctx, rpc.MethodDeskAuthorityStatus, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PrepareDeskAuthority returns exact terms for the owner's device review.
func (c *Client) PrepareDeskAuthority(ctx context.Context, in DeskAuthorityPrepareParams) (*DeskAuthorityPrepared, error) {
	var out DeskAuthorityPrepared
	if err := c.callPrivate(ctx, rpc.MethodDeskAuthorityPrepare, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ConfirmDeskAuthority stores the mandate after verifying the device
// confirmation. It does not start the controller.
func (c *Client) ConfirmDeskAuthority(ctx context.Context, in DeskAuthorityConfirmParams) (*DeskAuthorityStatus, error) {
	var out DeskAuthorityStatus
	if err := c.callPrivate(ctx, rpc.MethodDeskAuthorityConfirm, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ControlDeskAuthority runs, pauses, downgrades or disarms under the
// daemon-held generation. Upgrading needs a fresh confirmation.
func (c *Client) ControlDeskAuthority(ctx context.Context, in DeskAuthorityControlParams) (*DeskAuthorityStatus, error) {
	var out DeskAuthorityStatus
	if err := c.callPrivate(ctx, rpc.MethodDeskAuthorityControl, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
