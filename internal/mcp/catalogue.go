package mcp

import (
	"encoding/json"
	"time"
)

// Lookup returns the catalogue tool named name.
func Lookup(name string) (Tool, bool) { return lookupTool(name) }

// ToolCallTimeout returns the budget the MCP server applies to one call of
// name with args: the largest client timeout across the daemon methods the
// call can reach, with the tool's headroom and floor. In-process callers
// apply the same budget so a call bounds itself the way an MCP call does.
func ToolCallTimeout(name string, args json.RawMessage) time.Duration {
	return mcpToolCallTimeout(name, args)
}
