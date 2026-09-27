package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/productidentity"
	"github.com/osauer/canary/v2/internal/update"
)

// installWithSpaces creates a stand-in executable under a directory whose
// name contains a space, as Desk's "Application Support" runtime does.
func installWithSpaces(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Application Support", "Desk", "runtime")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(dir, productidentity.Executable)
	if err := os.WriteFile(executable, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	return executable
}

func TestAppCommandMatchAcceptsInstallPathWithSpaces(t *testing.T) {
	t.Parallel()
	executable := installWithSpaces(t)
	args, exact, ok := appCommandMatch(executable+" app --addr 127.0.0.1:8765", executablePathVariants(executable))
	if !ok || !exact || strings.Join(args, " ") != "app --addr 127.0.0.1:8765" {
		t.Fatalf("appCommandMatch = %q exact %v ok %v; want the app server at its exact executable", args, exact, ok)
	}
	for _, cmdline := range []string{
		"echo " + executable + " app",
		"/bin/sh -c " + executable + " app",
		"/usr/bin/env " + executable + " app",
		executable + " app pair",
	} {
		if args, exact, ok := appCommandMatch(cmdline, nil); ok {
			t.Fatalf("appCommandMatch(%q) = %q exact %v; want no match", cmdline, args, exact)
		}
	}
}

func TestParseMCPPSLineKeepsInstallPathWithSpaces(t *testing.T) {
	t.Parallel()
	executable := installWithSpaces(t)
	pid, ppid, command, ok := parseMCPPSLine("  4711   901 " + executable + " mcp --profile monitor")
	if !ok || pid != 4711 || ppid != 901 || command != executable+" mcp --profile monitor" {
		t.Fatalf("parseMCPPSLine = %d %d %q %v", pid, ppid, command, ok)
	}
	if _, _, managed := update.SplitManagedCommand(command, "mcp"); !managed {
		t.Fatalf("%q was not recognised as a Canary MCP server", command)
	}
	if _, _, _, ok := parseMCPPSLine("PID PPID ARGS"); ok {
		t.Fatal("header row parsed as a process")
	}
}
