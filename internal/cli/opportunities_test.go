package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestOpportunitiesSubcommandIndexFindsHoistedSubcommand(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{args: []string{"--json", "list"}, want: 1},
		{args: []string{"preview", "--quantity", "1", "key", "rev"}, want: 0},
		{args: []string{"--timeout", "5s", "exercise", "key", "rev"}, want: 2},
		{args: []string{"--json"}, want: -1},
	}
	for _, tc := range cases {
		if got := opportunitiesSubcommandIndex(tc.args); got != tc.want {
			t.Fatalf("opportunitiesSubcommandIndex(%v)=%d, want %d", tc.args, got, tc.want)
		}
	}
}

func TestRunOpportunitiesGroupHelp(t *testing.T) {
	t.Parallel()
	for _, help := range []string{"--help", "-h", "-help", "help"} {
		t.Run(help, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &stderr}
			if code := Run(context.Background(), env, "opportunities", []string{help}); code != 0 {
				t.Fatalf("Run(opportunities, %s)=%d, want 0", help, code)
			}
			got := stdout.String()
			for _, want := range []string{
				"canary opportunities",
				"Daemon-owned option exercise opportunities",
				"status|refresh|list|preview|exercise|ignore",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("opportunities help missing %q:\n%s", want, got)
				}
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q, want empty", stderr.String())
			}
		})
	}
}

func TestRenderOpportunitiesText(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderOpportunitiesText(env, &rpc.OpportunitySnapshot{
		Revision:      "sha256:rev",
		PolicyID:      "opportunity-option-exercise-mvp",
		PolicyVersion: 1,
		Counts: rpc.OpportunityCounts{
			Total:                1,
			Actionable:           1,
			ExpectedGain:         100,
			ExpectedGainCurrency: "USD",
		},
		Opportunities: []rpc.Opportunity{{
			Key:            "option_exercise:key",
			Bucket:         rpc.OpportunityBucketOptionExercise,
			Action:         rpc.OpportunityActionExercise,
			Quantity:       1,
			Symbol:         "AAPL",
			PositionEffect: rpc.ExercisePositionEffectClose,
			PostExerciseRisk: &rpc.OpportunityPostExerciseRisk{
				Underlying:             "AAPL",
				BeforeQuantity:         -100,
				AfterQuantity:          0,
				ShareChange:            100,
				PositionEffect:         rpc.ExercisePositionEffectClose,
				RiskChange:             rpc.ExerciseRiskChangeClosed,
				ProtectionReviewNeeded: false,
			},
			ExpectedGain:         100,
			ExpectedGainCurrency: "USD",
			Contract:             rpc.ContractParams{Right: "C", Strike: 100},
			Details:              []string{"intrinsic 300.00 USD"},
		}},
	})
	got := stdout.String()
	for _, want := range []string{
		"Canary Opportunities  1 actionable / 1 total",
		"Expected gain",
		"option_exercise:key",
		"gain=$ 100.00",
		"post-exercise risk: AAPL -100 -> 0 shares",
		"protection review not required",
		"intrinsic 300.00 USD",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("opportunities render missing %q:\n%s", want, got)
		}
	}
}

func TestRenderOpportunityPreviewText(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderOpportunityPreviewText(env, &rpc.OpportunityExercisePreviewResult{
		Accepted:       true,
		TokenMinted:    true,
		SubmitEligible: true,
		PreviewTokenID: "opprev-1",
		AsOf:           time.Now(),
		Opportunity: rpc.Opportunity{
			Key:                      "option_exercise:key",
			Action:                   rpc.OpportunityActionExercise,
			Quantity:                 1,
			Symbol:                   "AAPL",
			Contract:                 rpc.ContractParams{Right: "C", Strike: 100, Expiry: "20260619"},
			ExpectedGain:             100,
			ExpectedGainCurrency:     "USD",
			UnderlyingQuantityBefore: 100,
			UnderlyingQuantityAfter:  200,
			UnderlyingShareChange:    100,
			PositionEffect:           rpc.ExercisePositionEffectIncrease,
			PostExerciseRisk: &rpc.OpportunityPostExerciseRisk{
				Underlying:                 "AAPL",
				BeforeQuantity:             100,
				AfterQuantity:              200,
				ShareChange:                100,
				PositionEffect:             rpc.ExercisePositionEffectIncrease,
				RiskChange:                 rpc.ExerciseRiskChangeIncreased,
				RiskIncreased:              true,
				ProtectionReviewNeeded:     true,
				ProtectionReviewReason:     "exercise opens, increases, or flips underlying exposure; review protective stops after exercise",
				ProtectionCoverageState:    rpc.ProtectionCoverageStateCovered,
				CurrentProtectedQuantity:   100,
				CurrentUnprotectedQuantity: 0,
			},
		},
	})
	got := stdout.String()
	for _, want := range []string{
		"Opportunity Exercise Preview  accepted=true token_minted=true submit_eligible=true",
		"Token ID",
		"opprev-1",
		"Expected gain",
		"Post-exercise risk",
		"risk increased",
		"protection review needed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("opportunity preview render missing %q:\n%s", want, got)
		}
	}
}

func TestRenderOpportunityPreviewTextBlockedOmitsToken(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderOpportunityPreviewText(env, &rpc.OpportunityExercisePreviewResult{
		Accepted:       false,
		TokenMinted:    false,
		SubmitEligible: false,
		Opportunity:    rpc.Opportunity{Key: "option_exercise:key", Action: rpc.OpportunityActionExercise, Quantity: 1, Symbol: "AAPL"},
		Blockers:       []rpc.TradingBlocker{{Code: "trading_frozen", Message: "trading writes are frozen"}},
	})
	got := stdout.String()
	for _, want := range []string{
		"Opportunity Exercise Preview  accepted=false token_minted=false submit_eligible=false",
		"trading_frozen: trading writes are frozen",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("opportunity blocked preview render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Token ID") {
		t.Fatalf("blocked preview rendered token line:\n%s", got)
	}
}
