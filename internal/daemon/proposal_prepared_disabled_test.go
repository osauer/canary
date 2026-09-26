//go:build !trading

package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestPreparedProposalCannotSubmitFromReadOnlyBuild(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	engine := &proposalEngine{server: srv, now: srv.now}
	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true})
	payload, err := srv.orderTokens.verify(token)
	if err != nil {
		t.Fatal(err)
	}
	preview := &rpc.OrderPreviewResult{SubmitEligible: true, PreviewToken: token, PreviewTokenID: payload.TokenID, PreviewTokenExpiresAt: payload.ExpiresAt, Draft: payload.Draft, Account: payload.Account, Mode: payload.Mode, Endpoint: payload.Endpoint, ClientID: payload.ClientID}
	prop := rpc.TradeProposal{Key: "synthetic-key", Revision: "synthetic-revision"}
	reference, _, err := engine.retainPreparation(t.Context(), prop, preview)
	if err != nil {
		t.Fatal(err)
	}
	out, err := engine.Submit(t.Context(), rpc.TradeProposalSubmitParams{Key: prop.Key, Revision: prop.Revision, PreparedRef: reference, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
	if err != nil || out.Accepted || len(out.Blockers) == 0 || out.Preparation == nil || out.Preparation.Consumed == nil || *out.Preparation.Consumed {
		t.Fatal("read-only build admitted a prepared broker write")
	}
}
