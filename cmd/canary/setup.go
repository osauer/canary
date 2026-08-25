package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/productidentity"
)

func runSetup(args []string) int {
	target := "claude-desktop"
	if len(args) > 0 {
		if args[0] == "--help" || args[0] == "-h" {
			fmt.Printf("%s setup — write local integration config.\n", productidentity.Executable)
			fmt.Println()
			fmt.Printf("Usage: %s setup [target]\n", productidentity.Executable)
			fmt.Printf("       %s setup app [--remote] [--remote-url URL]\n", productidentity.Executable)
			fmt.Printf("       %s setup reporting [--accept-unproved] [--no-restart]\n", productidentity.Executable)
			fmt.Println()
			fmt.Println("Supported targets:")
			fmt.Println("  claude-desktop  (default)")
			fmt.Println("  app             install the Canary app macOS LaunchAgent")
			fmt.Println("  reporting       securely validate and activate IBKR Flex reporting")
			fmt.Println()
			fmt.Println("claude-desktop writes an mcpServers.canary entry pointing at this binary.")
			fmt.Println("app writes ~/Library/LaunchAgents/com.osauer.ibkr-app.plist.")
			return 0
		}
		target = args[0]
	}

	switch target {
	case "claude-desktop":
		return setupClaudeDesktop()
	case "app":
		return setupAppLaunchAgent(args[1:])
	case "reporting":
		return setupReporting(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "%s setup: unknown client %q\n", productidentity.Executable, target)
		fmt.Fprintln(os.Stderr, "supported: claude-desktop, app, reporting")
		return 2
	}
}

// claudeDesktopConfigPath returns the platform-specific path to
// claude_desktop_config.json. macOS is the only supported target — Claude
// Desktop ships for macOS and Windows; we don't support Windows (the
// daemon uses Unix-only primitives), and there's no official Linux build.
func claudeDesktopConfigPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	case "windows":
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			return "", errors.New("%APPDATA% not set")
		}
		return filepath.Join(appdata, "Claude", "claude_desktop_config.json"), nil
	default:
		return "", fmt.Errorf("claude-desktop is not available on %s — try Cursor, Continue, or Zed (see the README for the JSON snippet)", runtime.GOOS)
	}
}

func setupClaudeDesktop() int {
	configPath, err := claudeDesktopConfigPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s setup: %v\n", productidentity.Executable, err)
		return 1
	}

	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s setup: locate own binary: %v\n", productidentity.Executable, err)
		return 1
	}
	// Resolve symlinks so the config holds the canonical path. Without
	// this, a launcher symlink could record a fragile command path that breaks
	// across reinstalls.
	if resolved, err := filepath.EvalSymlinks(binPath); err == nil {
		binPath = resolved
	}

	// Read existing config (or start from an empty object). We round-trip
	// through a generic map so unrelated top-level keys the user may have
	// (theme, telemetry, etc.) survive untouched.
	cfg := map[string]any{}
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "%s setup: existing %s is not valid JSON: %v\n", productidentity.Executable, configPath, err)
			fmt.Fprintln(os.Stderr, "  fix or delete the file and re-run, or restore from backup")
			return 1
		}
		// Backup so the user has a one-step rollback.
		stamp := time.Now().Format("20060102-150405")
		backup := configPath + ".bak-" + stamp
		if err := os.WriteFile(backup, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "%s setup: backup %s: %v\n", productidentity.Executable, backup, err)
			return 1
		}
		fmt.Printf("Backed up existing config → %s\n", backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "%s setup: read %s: %v\n", productidentity.Executable, configPath, err)
		return 1
	} else {
		// Config doesn't exist yet — make sure the parent dir does so the
		// write doesn't fail. Claude Desktop creates this dir on first
		// launch, so it should already exist if Claude Desktop was ever
		// opened. We mkdir defensively for the fresh-install case.
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "%s setup: create config dir: %v\n", productidentity.Executable, err)
			return 1
		}
	}

	out, err := mergeCanaryMCPEntry(cfg, binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s setup: encode config: %v\n", productidentity.Executable, err)
		return 1
	}
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "%s setup: write %s: %v\n", productidentity.Executable, configPath, err)
		return 1
	}

	fmt.Printf("Wired Canary into %s (mcpServers.canary)\n", configPath)
	fmt.Println()
	fmt.Println("Next:")
	fmt.Println("  1. Fully quit Claude Desktop (⌘Q on macOS — not just close the window).")
	fmt.Println("  2. Reopen it.")
	fmt.Println("  3. Ask Claude something like \"what's in my IBKR account?\" and it'll call the canary_* tools.")
	fmt.Println()
	fmt.Println("Troubleshooting log: ~/Library/Logs/Claude/mcp-server-canary.log")
	return 0
}

// mergeCanaryMCPEntry takes a parsed config map and the binary's absolute
// path and returns the marshalled JSON bytes (with trailing newline) that
// claude_desktop_config.json should contain. Unrelated top-level keys are
// preserved; the mcpServers map is created if absent, the exact retired `ibkr`
// entry is removed, and the canonical `canary` entry is replaced. Pure — no
// I/O — so it carries the merge invariants for unit tests.
func mergeCanaryMCPEntry(cfg map[string]any, binPath string) ([]byte, error) {
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	delete(servers, "ibkr")
	servers["canary"] = map[string]any{
		"command": binPath,
		"args":    []string{"mcp"},
	}
	cfg["mcpServers"] = servers

	// Two-space indent matches what Claude Desktop writes by default.
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	return out, nil
}

type appLaunchAgentOptions struct {
	Remote    bool
	RemoteURL string
}

func setupAppLaunchAgent(args []string) int {
	opts := appLaunchAgentOptions{}
	fs := flag.NewFlagSet(productidentity.Executable+" setup app", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "%s setup app - install the Canary app macOS LaunchAgent.\n", productidentity.Executable)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Usage: %s setup app [--remote] [--remote-url URL]\n", productidentity.Executable)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Flags:")
		printFlagDefaults(w, fs)
	}
	fs.Usage = func() { usage(os.Stdout) }
	fs.BoolVar(&opts.Remote, "remote", false, "enable the outbound Cloudflare Worker relay")
	fs.StringVar(&opts.RemoteURL, "remote-url", "", "Cloudflare Worker relay base URL")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "%s setup app: %v\n", productidentity.Executable, err)
		return 2
	}
	if fs.NArg() != 0 {
		return rejectUnexpectedArgument(os.Stderr, productidentity.Executable+" setup app", fs, usage)
	}
	opts.RemoteURL = strings.TrimRight(strings.TrimSpace(opts.RemoteURL), "/")
	if runtime.GOOS != "darwin" {
		fmt.Fprintf(os.Stderr, "%s setup app: macOS LaunchAgents are not available on %s\n", productidentity.Executable, runtime.GOOS)
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s setup app: %v\n", productidentity.Executable, err)
		return 1
	}
	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s setup app: locate own binary: %v\n", productidentity.Executable, err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(binPath); err == nil {
		binPath = resolved
	}
	agentDir := filepath.Join(home, "Library", "LaunchAgents")
	logDir := filepath.Join(home, "Library", "Logs", "ibkr")
	plistPath := filepath.Join(agentDir, productidentity.AppLaunchAgentLabel+".plist")
	outPath := filepath.Join(logDir, "app.log")
	errPath := filepath.Join(logDir, "app.err.log")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "%s setup app: create LaunchAgents dir: %v\n", productidentity.Executable, err)
		return 1
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "%s setup app: create log dir: %v\n", productidentity.Executable, err)
		return 1
	}
	if data, err := os.ReadFile(plistPath); err == nil {
		stamp := time.Now().Format("20060102-150405")
		backup := plistPath + ".bak-" + stamp
		if err := os.WriteFile(backup, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "%s setup app: backup %s: %v\n", productidentity.Executable, backup, err)
			return 1
		}
		fmt.Printf("Backed up existing LaunchAgent -> %s\n", backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "%s setup app: read %s: %v\n", productidentity.Executable, plistPath, err)
		return 1
	}
	if err := os.WriteFile(plistPath, appLaunchAgentPlist(binPath, outPath, errPath, opts), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "%s setup app: write %s: %v\n", productidentity.Executable, plistPath, err)
		return 1
	}
	fmt.Printf("Installed Canary app LaunchAgent at %s\n", plistPath)
	fmt.Println()
	fmt.Println("Start it with:")
	fmt.Printf("  launchctl bootstrap gui/$(id -u) %s\n", plistPath)
	fmt.Println()
	fmt.Println("Pair a phone after it is running:")
	fmt.Printf("  %s app pair\n", productidentity.Executable)
	fmt.Println()
	fmt.Printf("Logs: %s and %s\n", outPath, errPath)
	return 0
}

func appLaunchAgentPlist(binPath, outPath, errPath string, opts appLaunchAgentOptions) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	writePlistString(&b, "Label", productidentity.AppLaunchAgentLabel)
	b.WriteString("  <key>ProgramArguments</key>\n")
	b.WriteString("  <array>\n")
	writePlistArrayString(&b, binPath)
	writePlistArrayString(&b, "app")
	if opts.Remote {
		writePlistArrayString(&b, "--remote")
	}
	if opts.RemoteURL != "" {
		writePlistArrayString(&b, "--remote-url")
		writePlistArrayString(&b, opts.RemoteURL)
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n")
	b.WriteString("  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n")
	b.WriteString("  <true/>\n")
	writePlistString(&b, "StandardOutPath", outPath)
	writePlistString(&b, "StandardErrorPath", errPath)
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.Bytes()
}

func writePlistString(b *bytes.Buffer, key, value string) {
	fmt.Fprintf(b, "  <key>%s</key>\n", xmlEscape(key))
	fmt.Fprintf(b, "  <string>%s</string>\n", xmlEscape(value))
}

func writePlistArrayString(b *bytes.Buffer, value string) {
	fmt.Fprintf(b, "    <string>%s</string>\n", xmlEscape(value))
}

func xmlEscape(value string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}
