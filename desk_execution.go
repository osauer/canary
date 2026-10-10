package canary

import "github.com/osauer/canary/v2/internal/rpc"

// Desk execution contract (rpc.MethodDeskExecution*): one daemon path for a
// device-confirmed manual batch and the standing automatic mandate. These
// aliases let Desk build its adapter against the exact wire types; calling
// the methods goes through the gated private backend, not this Client.

// DeskExecutionContractVersion is the contract version a daemon must report.
const DeskExecutionContractVersion = rpc.DeskExecutionContractVersion

// Desk execution item classes and outcomes.
const (
	DeskClassProtect    = rpc.DeskClassProtect
	DeskClassReduce     = rpc.DeskClassReduce
	DeskClassAdd        = rpc.DeskClassAdd
	DeskOutcomeAccepted = rpc.DeskOutcomeAccepted
	DeskOutcomeRefused  = rpc.DeskOutcomeRefused
	DeskOutcomeUnknown  = rpc.DeskOutcomeUnknown
	DeskOutcomeAbsent   = rpc.DeskOutcomeAbsent
)

// Desk execution blocker codes.
const (
	DeskBlockerDrawdownBrake    = rpc.DeskBlockerDrawdownBrake
	DeskBlockerEpisodeConsumed  = rpc.DeskBlockerEpisodeConsumed
	DeskBlockerRequestConflict  = rpc.DeskBlockerRequestConflict
	DeskBlockerAuthorityChanged = rpc.DeskBlockerAuthorityChanged
	DeskBlockerClassMismatch    = rpc.DeskBlockerClassMismatch
	DeskBlockerPriorUnknown     = rpc.DeskBlockerPriorUnknown
	DeskBlockerProposalChanged  = rpc.DeskBlockerProposalChanged
)

// DeskExecutionCapabilitiesResult lists the served desk.execution methods.
type DeskExecutionCapabilitiesResult = rpc.DeskExecutionCapabilitiesResult

// DeskExecutionIntent is one order's exact intent.
type DeskExecutionIntent = rpc.DeskExecutionIntent

// DeskExecutionProposal names one served proposal revision.
type DeskExecutionProposal = rpc.DeskExecutionProposal

// DeskExecutionAdd is an automatic stock entry.
type DeskExecutionAdd = rpc.DeskExecutionAdd

// DeskExecutionItem is one order request with its stable identities.
type DeskExecutionItem = rpc.DeskExecutionItem

// DeskExecutionPrepareBatchParams names a manual batch's proposals.
type DeskExecutionPrepareBatchParams = rpc.DeskExecutionPrepareBatchParams

// DeskExecutionBatchTerms are a manual batch's confirmed terms.
type DeskExecutionBatchTerms = rpc.DeskExecutionBatchTerms

// DeskExecutionBatchItem is one confirmed batch item.
type DeskExecutionBatchItem = rpc.DeskExecutionBatchItem

// DeskExecutionPrepareBatchResult carries batch terms and the private reference.
type DeskExecutionPrepareBatchResult = rpc.DeskExecutionPrepareBatchResult

// DeskBatchAuthorization is the one-use manual authorisation.
type DeskBatchAuthorization = rpc.DeskBatchAuthorization

// DeskAutomaticAuthorization is the standing mandate as the controller holds it.
type DeskAutomaticAuthorization = rpc.DeskAutomaticAuthorization

// DeskExecutionSubmitParams sends items under exactly one authorisation.
type DeskExecutionSubmitParams = rpc.DeskExecutionSubmitParams

// DeskExecutionReceipt is one item's result.
type DeskExecutionReceipt = rpc.DeskExecutionReceipt

// DeskExecutionSubmitResult reports every item in send order.
type DeskExecutionSubmitResult = rpc.DeskExecutionSubmitResult

// DeskExecutionLookupParams names requests by ID.
type DeskExecutionLookupParams = rpc.DeskExecutionLookupParams

// DeskExecutionLookupResult answers every named request.
type DeskExecutionLookupResult = rpc.DeskExecutionLookupResult

// DeskIntentDigest binds a request to its receipt.
func DeskIntentDigest(item DeskExecutionItem) string { return rpc.DeskIntentDigest(item) }

// ValidateAutomaticOrder refuses an addition listed before protection or a reduction.
func ValidateAutomaticOrder(items []DeskExecutionItem) error {
	return rpc.ValidateAutomaticOrder(items)
}
