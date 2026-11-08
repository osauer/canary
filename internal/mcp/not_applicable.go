package mcp

import (
	"encoding/json"
	"errors"

	"github.com/osauer/canary/v2/internal/rpc"
)

// notApplicable reports a daemon's answer that this broker does not serve the
// tool's data.
func notApplicable(err error) bool {
	var rpcErr *rpc.Error
	return errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeNotApplicable
}

// notApplicableResult is a typed result: an AI host must never mistake "off for
// this broker" for "broken". The reason is the daemon's own words.
func notApplicableResult(tool string, err error) json.RawMessage {
	reason := "this daemon's broker does not serve it"
	var rpcErr *rpc.Error
	if errors.As(err, &rpcErr) && rpcErr.Message != "" {
		reason = rpcErr.Message
	}
	out, _ := json.Marshal(map[string]string{
		"status": rpc.CodeNotApplicable,
		"tool":   tool,
		"reason": reason,
	})
	return out
}
