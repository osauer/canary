//go:build ignore

// Read-only probe: prints the nudges snapshot per-input source health.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := dial.Connect(dial.DefaultSocketPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close()
	var result rpc.NudgesSnapshotResult
	if err := conn.Call(ctx, rpc.MethodNudgesSnapshot, map[string]any{}, &result); err != nil {
		fmt.Fprintln(os.Stderr, "nudges.snapshot:", err)
		os.Exit(1)
	}
	raw, _ := json.MarshalIndent(result.SourceHealth, "", " ")
	fmt.Println(string(raw))
}
