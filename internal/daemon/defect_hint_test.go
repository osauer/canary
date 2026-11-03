package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A Canary defect is classified internal and its message ends with the
// feedback hint; a user's bad request is classified as before and carries
// no hint, so configuration errors never point at the bug form.
func TestDefectsCarryTheFeedbackHintAndBadRequestsDoNot(t *testing.T) {
	code, msg := classifyError(rpc.Defect(errors.New("regime snapshot projection identity is inconsistent")))
	if code != rpc.CodeInternal || !strings.HasSuffix(msg, rpc.DefectHint) {
		t.Fatalf("defect classified %s %q", code, msg)
	}
	code, msg = classifyError(errBadRequest("contract.symbol required"))
	if code != rpc.CodeBadRequest || strings.Contains(msg, "canary feedback") {
		t.Fatalf("bad request classified %s %q", code, msg)
	}
	code, msg = classifyError(errors.New("read config: permission denied"))
	if code != rpc.CodeInternal || strings.Contains(msg, "canary feedback") {
		t.Fatalf("plain error classified %s %q; only a marked defect carries the hint", code, msg)
	}
}
