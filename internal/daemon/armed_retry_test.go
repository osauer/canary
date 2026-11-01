package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestArmedRetryHardBlockerWinsAcrossRestart(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	prop := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.engine.revalidateForTest = func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		return prop, []rpc.TradingBlocker{{Code: previewQuoteUnavailableCode, Message: "temporary quote failure"}, {Code: previewRiskLimitCode, Message: "hard risk refusal"}}, nil
	}
	rig.advance(31 * time.Minute)
	rig.cycle()
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticFailed {
		t.Fatalf("mixed hard/transient refusal must be final: %+v", rec)
	}
	// The old format retained only the first blocker's text. A newly made
	// final decision must not be mistaken for a recoverable legacy failure.
	rig.restart()
	rig.install(prop)
	rig.cycle()
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticFailed {
		t.Fatalf("restart reclassified a known hard refusal from its first blocker: %+v", rec)
	}
}

func TestArmedRetrySavedFailuresKeepBrokerAndOwnerFences(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*automaticSubmissionRecord)
	}{
		{"preview token", func(r *automaticSubmissionRecord) { r.PreviewTokenID = "synthetic-preview" }},
		{"staging time", func(r *automaticSubmissionRecord) { r.SubmittingAt = r.ResolvedAt }},
		{"submitted time", func(r *automaticSubmissionRecord) { r.SubmittedAt = r.ResolvedAt }},
		{"order reference", func(r *automaticSubmissionRecord) { r.OrderRef = "synthetic-order" }},
		{"veto time", func(r *automaticSubmissionRecord) { r.VetoedAt = r.ResolvedAt }},
		{"unresolved failure", func(r *automaticSubmissionRecord) { r.ResolvedAt = time.Time{} }},
		{"generic preview failure", func(r *automaticSubmissionRecord) { r.Reason = previewGenericFailureCode + ": unknown" }},
		{"place failure", func(r *automaticSubmissionRecord) { r.Reason = "submit_failed: synthetic unavailable" }},
		{"hard risk failure", func(r *automaticSubmissionRecord) { r.Reason = previewRiskLimitCode + ": over cap" }},
		{"ambiguous legacy blockers", func(r *automaticSubmissionRecord) { r.Reason = "gateway_unavailable: unknown prior blockers" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
			prop := rig.stopProposal()
			revision := rig.install(prop)
			rig.cycle()
			rig.advance(31 * time.Minute)
			if err := rig.engine.automatic.update(t.Context(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
				rec := records[automaticRecordKey(prop.Key, revision)]
				rec.State, rec.ResolvedAt = rpc.TradeProposalAutomaticFailed, rig.now
				rec.Reason = previewNotionalUnavailableCode + ": synthetic FX quote unavailable"
				tc.change(rec)
				return []automaticSubmissionEvent{{At: rig.now, Type: automaticEventFailed, Key: rec.Key, Revision: rec.Revision, State: rec.State}}
			}); err != nil {
				t.Fatal(err)
			}
			rig.advance(time.Hour)
			rig.restart()
			rig.install(prop)
			rig.cycle()
			if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticFailed || rig.revalidations != 0 {
				t.Fatalf("unsafe legacy failure was revived: attempts=%d record=%+v", rig.revalidations, rec)
			}
		})
	}
}

func TestArmedRetryOnlyExplicitTransientRefusalsQualify(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		codes    []string
		err      error
		placeErr error
		accepted bool
		want     bool
	}{
		{name: "quote", codes: []string{previewQuoteUnavailableCode}, want: true},
		{name: "notional conversion", codes: []string{previewNotionalUnavailableCode}, want: true},
		{name: "broker and quote", codes: []string{"gateway_unavailable", previewQuoteStaleCode}, want: true},
		{name: "mixed hard", codes: []string{previewQuoteUnavailableCode, previewRiskLimitCode}},
		{name: "unknown", codes: []string{"future_unknown_refusal"}},
		{name: "blank", codes: []string{previewQuoteUnavailableCode, ""}},
		{name: "generic preview", codes: []string{previewGenericFailureCode}},
		{name: "unknown outcome"},
		{name: "error", codes: []string{previewQuoteUnavailableCode}, err: errors.New("unknown")},
		{name: "place path", codes: []string{previewQuoteUnavailableCode}, placeErr: errors.New("uncertain send")},
		{name: "accepted", codes: []string{previewQuoteUnavailableCode}, accepted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := rpc.TradeProposalSubmitResult{Accepted: tc.accepted}
			for _, code := range tc.codes {
				res.Blockers = append(res.Blockers, rpc.TradingBlocker{Code: code})
			}
			if got := automaticTransientRefusal(res, tc.err, tc.placeErr); got != tc.want {
				t.Fatalf("transient=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestArmedRetryPacingIsCheckedInsideWriteLock(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	prop := rig.mandateProposal(automaticTestStockRow(), 40, 1, 1)
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	attempts := 0
	rig.engine.revalidateForTest = func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		attempts++
		return prop, []rpc.TradingBlocker{{Code: previewQuoteUnavailableCode, Message: "temporary quote failure"}}, nil
	}
	rig.advance(31 * time.Minute)
	due := rig.record(prop.Key, revision)
	rig.engine.submitAutomatic(t.Context(), due)
	rig.engine.submitAutomatic(t.Context(), due)
	if attempts != 1 {
		t.Fatalf("a stale due snapshot bypassed the retry floor: %d attempts", attempts)
	}
	rig.advance(automaticDeferRetry)
	rig.cycle()
	if attempts != 2 {
		t.Fatalf("the paced retry was lost: %d attempts", attempts)
	}
	if rec := rig.record(prop.Key, revision); !rec.CreatedAt.Equal(due.CreatedAt) || !rec.NoticedAt.Equal(due.NoticedAt) || rec.Episode != due.Episode || rec.clockQuantity() != due.clockQuantity() {
		t.Fatalf("the retry changed the mandate's veto opportunity: before=%+v after=%+v", due, rec)
	}
}
