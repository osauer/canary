package canary

import (
	"context"

	"github.com/osauer/canary/v2/internal/rpc"
)

// CashPolicySnapshot is the cash management settings as the daemon reads
// them from the protection policy file, with their facts.
type CashPolicySnapshot = rpc.CashPolicySnapshot

// CashPolicySetting is one setting in a CashPolicySnapshot.
type CashPolicySetting = rpc.CashPolicySetting

// CashPolicyCheckRequest names a draft of cash management settings.
type CashPolicyCheckRequest = rpc.CashPolicyCheckRequest

// CashPolicyCheckResult is the daemon's verdict on a draft; it writes nothing.
type CashPolicyCheckResult = rpc.CashPolicyCheckResult

// CashPolicyChange is one changed setting in a CashPolicyCheckResult.
type CashPolicyChange = rpc.CashPolicyChange

// CashPolicyApplyRequest saves the terms a check returned, with the owner's
// device confirmation.
type CashPolicyApplyRequest = rpc.CashPolicyApplyRequest

// CashPolicyConfirmation is the owner's device confirmation of one save.
type CashPolicyConfirmation = rpc.CashPolicyConfirmation

// CashPolicyApplyResult is the snapshot after a save, with its receipt.
type CashPolicyApplyResult = rpc.CashPolicyApplyResult

// CashPolicyFile is one policy file's state in a CashPolicySnapshot: the
// protection file's fields on the snapshot itself, the risk constitution's
// under Constitution.
type CashPolicyFile = rpc.CashPolicyFile

// CashPolicyPreset is the stance the files' values derive to.
type CashPolicyPreset = rpc.CashPolicyPreset

// CashPolicyPresetOption is one preset the owner may apply, with its values
// and what they come to at today's NLV.
type CashPolicyPresetOption = rpc.CashPolicyPresetOption

// CashPolicyPresetFacts is what a preset comes to at today's NLV.
type CashPolicyPresetFacts = rpc.CashPolicyPresetFacts

// CashPolicyRestore offers the owner's own values back after a preset
// replaced them.
type CashPolicyRestore = rpc.CashPolicyRestore

// CashPolicyDevice says which of the owner's credentials Canary verifies
// itself.
type CashPolicyDevice = rpc.CashPolicyDevice

// CashPolicyPartial reports a two-file save that stopped after its first
// file.
type CashPolicyPartial = rpc.CashPolicyPartial

// Preset ids a CashPolicyPreset carries.
const (
	CashPolicyPresetCautious   = rpc.CashPolicyPresetCautious
	CashPolicyPresetBalanced   = rpc.CashPolicyPresetBalanced
	CashPolicyPresetAggressive = rpc.CashPolicyPresetAggressive
	CashPolicyPresetCustom     = rpc.CashPolicyPresetCustom
)

// CashPolicySectionOrderLimits is the section of the order cap settings,
// which live in the risk constitution.
const CashPolicySectionOrderLimits = rpc.CashPolicySectionOrderLimits

// Error codes a cash policy save reports besides CodeSettingsConflict.
const (
	CodePolicyInvalid            = rpc.CodePolicyInvalid
	CodePolicyUnwritable         = rpc.CodePolicyUnwritable
	CodeConfirmationRequired     = rpc.CodeConfirmationRequired
	CodeConfirmationUnverifiable = rpc.CodeConfirmationUnverifiable
	CodeRequestReused            = rpc.CodeRequestReused
)

// CashPolicy reads the cash management settings. Like the methods below it is
// outside the MCP catalogue and the CLI: Desk's console calls it.
func (c *Client) CashPolicy(ctx context.Context) (*CashPolicySnapshot, error) {
	var out CashPolicySnapshot
	if err := c.callCashPolicy(ctx, rpc.MethodCashPolicyGet, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckCashPolicy validates a draft and works out its figures; it writes
// nothing.
func (c *Client) CheckCashPolicy(ctx context.Context, in CashPolicyCheckRequest) (*CashPolicyCheckResult, error) {
	var out CashPolicyCheckResult
	if err := c.callCashPolicy(ctx, rpc.MethodCashPolicyCheck, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ApplyCashPolicy saves checked terms with the owner's device confirmation:
// the daemon writes only the changed lines, raises policy_version by one and
// records a receipt. Retrying a request id with the same terms writes nothing
// and returns the receipt's version.
func (c *Client) ApplyCashPolicy(ctx context.Context, in CashPolicyApplyRequest) (*CashPolicyApplyResult, error) {
	var out CashPolicyApplyResult
	if err := c.callCashPolicy(ctx, rpc.MethodCashPolicyApply, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) callCashPolicy(ctx context.Context, method string, in, out any) error {
	return c.callPrivate(ctx, method, in, out)
}

// callPrivate calls a non-catalogue daemon method on one connection.
func (c *Client) callPrivate(ctx context.Context, method string, in, out any) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return daemonError(conn.Call(ctx, method, in, out))
}
