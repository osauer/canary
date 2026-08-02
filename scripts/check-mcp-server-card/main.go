// Command check-mcp-server-card verifies that public MCP discovery metadata
// exposes exactly the canonical runtime tool names and input schemas.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/osauer/canary/v2/internal/mcp"
)

type serverCard struct {
	ServerInfo      json.RawMessage `json:"serverInfo"`
	Authentication  json.RawMessage `json:"authentication"`
	Transports      json.RawMessage `json:"transports"`
	Requirements    json.RawMessage `json:"requirements"`
	CompatibleHosts json.RawMessage `json:"compatibleHosts"`
	Tools           []cardTool      `json:"tools"`
	Resources       json.RawMessage `json:"resources"`
	Prompts         json.RawMessage `json:"prompts"`
	Safety          json.RawMessage `json:"safety"`
	Links           json.RawMessage `json:"links"`
	Keywords        json.RawMessage `json:"keywords"`
}

type cardTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type versionOnly struct {
	Version string `json:"version"`
}

// checkServerInfoVersion holds the card's stamp to the canonical discovery
// file. -write round-trips serverInfo as opaque bytes, so no generator run
// produces this value and nothing caught it drifting until the release gate —
// which could only offer a hint naming a generator that never writes it.
// Comparing here moves the catch to every `make check`, where the edit that
// caused it is still in hand.
func checkServerInfoVersion(cardPath, canonicalPath string, serverInfo json.RawMessage) int {
	raw, err := os.ReadFile(canonicalPath)
	if err != nil {
		fail("read %s: %v", canonicalPath, err)
	}
	var want versionOnly
	if err := json.Unmarshal(raw, &want); err != nil {
		fail("parse %s: %v", canonicalPath, err)
	}
	var got versionOnly
	if err := json.Unmarshal(serverInfo, &got); err != nil {
		fail("parse %s serverInfo: %v", cardPath, err)
	}
	if want.Version == "" {
		fmt.Fprintf(os.Stderr, "mcp-server-card-check: %s: no version to track\n", canonicalPath)
		return 1
	}
	if got.Version == want.Version {
		return 0
	}
	fmt.Fprintf(os.Stderr, "mcp-server-card-check: %s: serverInfo.version is %q, but %s is %q\n",
		cardPath, got.Version, canonicalPath, want.Version)
	fmt.Fprintf(os.Stderr, "                       edit serverInfo.version in %s by hand; no generator writes it\n", cardPath)
	return 1
}

func main() {
	path := flag.String("card", "docs/.well-known/mcp/server-card.json", "server card path")
	canonical := flag.String("canonical", "docs/mcp-server.json", "canonical MCP discovery file the card's serverInfo.version must track")
	write := flag.Bool("write", false, "rewrite the card tool array from the runtime registry")
	flag.Parse()
	raw, err := os.ReadFile(*path)
	if err != nil {
		fail("read %s: %v", *path, err)
	}
	var card serverCard
	if err := json.Unmarshal(raw, &card); err != nil {
		fail("parse %s: %v", *path, err)
	}
	if *write {
		descriptions := make(map[string]string, len(card.Tools))
		for _, tool := range card.Tools {
			descriptions[tool.Name] = tool.Description
		}
		card.Tools = make([]cardTool, 0, len(mcp.Tools))
		for _, tool := range mcp.Tools {
			description := descriptions[tool.Name]
			if description == "" {
				description = tool.Description
			}
			card.Tools = append(card.Tools, cardTool{
				Name: tool.Name, Description: description, InputSchema: tool.JSONSchema,
			})
		}
		body, err := json.MarshalIndent(card, "", "  ")
		if err != nil {
			fail("render %s: %v", *path, err)
		}
		body = append(body, '\n')
		if err := os.WriteFile(*path, body, 0o644); err != nil {
			fail("write %s: %v", *path, err)
		}
		return
	}
	actual := make(map[string]json.RawMessage, len(card.Tools))
	problems := 0
	problems += checkServerInfoVersion(*path, *canonical, card.ServerInfo)
	for _, tool := range card.Tools {
		if _, exists := actual[tool.Name]; exists {
			fmt.Fprintf(os.Stderr, "mcp-server-card-check: %s: duplicate tool %q\n", *path, tool.Name)
			problems++
		}
		actual[tool.Name] = tool.InputSchema
	}
	if len(actual) != len(mcp.Tools) {
		fmt.Fprintf(os.Stderr, "mcp-server-card-check: %s: has %d unique tools; runtime registry has %d\n", *path, len(actual), len(mcp.Tools))
		problems++
	}
	for _, tool := range mcp.Tools {
		got, ok := actual[tool.Name]
		if !ok {
			fmt.Fprintf(os.Stderr, "mcp-server-card-check: %s: missing runtime tool %q\n", *path, tool.Name)
			problems++
			continue
		}
		if !jsonEqual(got, tool.JSONSchema) {
			fmt.Fprintf(os.Stderr, "mcp-server-card-check: %s: input schema for %q differs from runtime registry\n", *path, tool.Name)
			problems++
		}
		delete(actual, tool.Name)
	}
	for name := range actual {
		fmt.Fprintf(os.Stderr, "mcp-server-card-check: %s: advertises unknown tool %q\n", *path, name)
		problems++
	}
	if problems > 0 {
		os.Exit(1)
	}
}

func jsonEqual(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mcp-server-card-check: "+format+"\n", args...)
	os.Exit(1)
}
