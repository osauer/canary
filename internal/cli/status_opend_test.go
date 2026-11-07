package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The hint names the gateway the error names: an OpenD reader has no IB Gateway
// to start.
func TestUnavailableHintNamesTheBrokersGateway(t *testing.T) {
	for _, tc := range []struct{ msg, want, not string }{
		{"market: gateway_unavailable: OpenD connection unavailable", "start OpenD and log in", "IB Gateway"},
		{"market: gateway_unavailable: IBKR connection unavailable", "start IB Gateway", "OpenD"},
	} {
		var stderr bytes.Buffer
		fail(&Env{Stderr: &stderr}, "%s", tc.msg)
		hint := stderr.String()[len("canary: ")+len(tc.msg):]
		if !strings.Contains(hint, tc.want) || strings.Contains(hint, tc.not) {
			t.Errorf("%s: hint %q", tc.msg, hint)
		}
	}
}
