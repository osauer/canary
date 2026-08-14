package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	selfupdate "github.com/osauer/canary/v2/internal/update"
)

func TestRunUpdateCoreRefusesDevelopmentBuildAndStableDowngrades(t *testing.T) {
	t.Parallel()
	for _, installed := range []string{"v3.0.1-39-g7b62a7d4", "v2.8.5-0.20260813190449-dcc9af72fc6b+dirty", "v3.1.0"} {
		t.Run(installed, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			installedCalled := false
			opts := updateOptions{
				installedVersion: installed,
				noRestart:        true,
				out:              &stdout,
				err:              &stderr,
			}
			exit := runUpdateCore(
				context.Background(),
				&opts,
				func(context.Context, string) (*selfupdate.Release, error) {
					return &selfupdate.Release{TagName: "v3.0.1"}, nil
				},
				func(context.Context, *selfupdate.Plan) error {
					installedCalled = true
					return nil
				},
				nil,
			)
			if exit != 0 || installedCalled || stderr.Len() != 0 {
				t.Fatalf("exit=%d installed=%t stderr=%q", exit, installedCalled, stderr.String())
			}
			if got := stdout.String(); !strings.Contains(got, "no update is offered") && !strings.Contains(got, "refusing to downgrade") {
				t.Fatalf("output=%q, want fail-closed explanation", got)
			}
		})
	}
}
