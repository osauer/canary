package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/cli"
	"github.com/osauer/canary/v2/internal/productidentity"
)

// versionInfo is the wire+text shape returned by collectVersionInfo.
// Field order matches the rendered text block; JSON tags are
// snake_case for parity with the rest of `ibkr --json`. omitempty
// drops the optional fields when the build doesn't have them.
type versionInfo struct {
	Program     string `json:"program"`
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	VCSState    string `json:"vcs_state,omitempty"`
	Built       string `json:"built,omitempty"`
	Binary      string `json:"binary,omitempty"`
	BinaryMtime string `json:"binary_mtime,omitempty"`
	GoVersion   string `json:"go_version"`
	GOOS        string `json:"goos"`
	GOARCH      string `json:"goarch"`
}

// collectVersionInfo gathers the three signal sources (ldflags vars,
// runtime/debug BuildInfo, on-disk mtime) into one struct. All file/
// runtime probes are best-effort: empty strings on failure so the
// renderer (and JSON via omitempty) silently skips them.
func collectVersionInfo(program string) versionInfo {
	v := versionInfo{
		Program:   program,
		Version:   effectiveVersion(),
		Commit:    commit,
		Built:     date,
		GoVersion: runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
	// "none" / "unknown" are the ldflags zero-values from main.go. Map
	// them to empty so omitempty drops them from JSON and the text
	// renderer suppresses the corresponding line.
	if v.Commit == "none" {
		v.Commit = ""
	}
	if v.Built == "unknown" {
		v.Built = ""
	}

	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.modified" {
				if s.Value == "true" {
					v.VCSState = "modified"
				} else {
					v.VCSState = "clean"
				}
				break
			}
		}
	}

	if bin, err := os.Executable(); err == nil {
		v.Binary = bin
		if fi, err := os.Stat(bin); err == nil {
			v.BinaryMtime = fi.ModTime().Local().Format(time.RFC3339)
		}
	}
	return v
}

// effectiveVersion returns the runtime version other subcommands should
// advertise to daemons, MCP clients, and the updater. Direct `go install` of
// the legacy source command does not run the Makefile, so the
// ldflags var remains "dev"; the Go module system still embeds the selected
// release in BuildInfo.
func effectiveVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

// shortCommit truncates a git sha to the conventional 7-char prefix
// for the human-readable text block. JSON keeps the full value.
func shortCommit(c string) string {
	if len(c) >= 7 {
		return c[:7]
	}
	return c
}

// printVersion renders the version info to w in the requested format.
// Text form mirrors Docker/kubectl/helm: `<program> <version>` headline
// followed by indented `Key:   Value` lines.
func printVersion(w io.Writer, program string, jsonOut bool) {
	v := collectVersionInfo(program)
	if jsonOut {
		buf, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Fprintf(w, `{"error":%q}`+"\n", err.Error())
			return
		}
		_, _ = w.Write(buf)
		_, _ = w.Write([]byte("\n"))
		return
	}
	renderVersionText(w, v, versionTextStyle{color: cli.ShouldColor(w)})
}

func renderVersionText(w io.Writer, v versionInfo, style versionTextStyle) {
	fmt.Fprintf(w, "%s  %s\n", programDisplayName(v.Program), style.versionBadge(v.Version))
	fmt.Fprintln(w)
	versionRow(w, style, "Commit", formatVersionCommit(v))
	versionRow(w, style, "Built", nonEmptyVersion(formatVersionTime(v.Built), "not stamped"))
	versionRow(w, style, "Runtime", fmt.Sprintf("%s %s/%s", v.GoVersion, v.GOOS, v.GOARCH))
	if v.Binary != "" {
		versionRow(w, style, "Binary", v.Binary)
	}
	if v.BinaryMtime != "" {
		versionRow(w, style, "Modified", formatVersionTime(v.BinaryMtime))
	}
	trust := versionTrust(v)
	versionRow(w, style, "Trust", style.trustText(trust))
}

func versionRow(w io.Writer, style versionTextStyle, label, value string) {
	fmt.Fprintf(w, "%s %s\n", style.dim(fmt.Sprintf("%-12s", label)), value)
}

func programDisplayName(program string) string {
	switch strings.TrimSpace(program) {
	case productidentity.Executable:
		return productidentity.ProductName + " CLI"
	case productidentity.Executable + " daemon":
		return productidentity.ProductName + " Daemon"
	default:
		return program
	}
}

func formatVersionCommit(v versionInfo) string {
	switch {
	case v.Commit != "" && v.VCSState != "":
		return fmt.Sprintf("%s, %s tree", shortCommit(v.Commit), v.VCSState)
	case v.Commit != "":
		return shortCommit(v.Commit)
	case v.VCSState != "":
		return v.VCSState + " tree, no commit stamp"
	default:
		return "not stamped"
	}
}

func formatVersionTime(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}

func nonEmptyVersion(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

type versionTrustLevel int

const (
	versionTrustOK versionTrustLevel = iota
	versionTrustNotice
	versionTrustWarn
)

type versionTrustStatus struct {
	Text  string
	Level versionTrustLevel
}

func versionTrust(v versionInfo) versionTrustStatus {
	dirty := v.VCSState == "modified" || strings.Contains(v.Version, "-dirty")
	switch {
	case dirty:
		return versionTrustStatus{Text: "dirty tree; rebuild after commit", Level: versionTrustWarn}
	case v.Version == "dev" || v.Commit == "" || v.Built == "":
		return versionTrustStatus{Text: "local build; provenance incomplete", Level: versionTrustWarn}
	case v.VCSState == "clean":
		return versionTrustStatus{Text: "stamped build, clean tree", Level: versionTrustOK}
	default:
		return versionTrustStatus{Text: "stamped build; tree state unavailable", Level: versionTrustNotice}
	}
}

// versionTextStyle mirrors internal/cli's tiny ANSI palette. Kept local
// because `canary version` is rendered before the normal CLI Env exists.
type versionTextStyle struct {
	color bool
}

const (
	versionAnsiReset  = "\x1b[0m"
	versionAnsiGreen  = "\x1b[32m"
	versionAnsiYellow = "\x1b[33m"
	versionAnsiDim    = "\x1b[2m"
	versionAnsiBold   = "\x1b[1m"
)

func (s versionTextStyle) wrap(code, text string) string {
	if !s.color {
		return text
	}
	return code + text + versionAnsiReset
}

func (s versionTextStyle) green(text string) string {
	return s.wrap(versionAnsiGreen, text)
}

func (s versionTextStyle) yellow(text string) string {
	return s.wrap(versionAnsiYellow, text)
}

func (s versionTextStyle) dim(text string) string {
	return s.wrap(versionAnsiDim, text)
}

func (s versionTextStyle) bold(text string) string {
	return s.wrap(versionAnsiBold, text)
}

func (s versionTextStyle) versionBadge(version string) string {
	text := s.bold(version)
	if version == "dev" || strings.Contains(version, "-dirty") {
		return s.yellow(text)
	}
	return s.green(text)
}

func (s versionTextStyle) trustText(trust versionTrustStatus) string {
	switch trust.Level {
	case versionTrustOK:
		return s.green(trust.Text)
	case versionTrustNotice:
		return s.dim(trust.Text)
	default:
		return s.yellow(trust.Text)
	}
}

// hasJSONFlag is the version subcommand's mini-parser. The subcommand
// has no other flags, so spinning up a flag.FlagSet for one bool would
// be more code than this and would refuse unknown args (which the
// user might pass by mistake — let the caller handle that).
func hasJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "--json" || a == "-json" {
			return true
		}
	}
	return false
}
