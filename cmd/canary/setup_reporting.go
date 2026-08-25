package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/cli"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func setupReporting(args []string) int {
	fs := flag.NewFlagSet("canary setup reporting", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	acceptUnproved := fs.Bool("accept-unproved", false, "activate when no field is proven missing but empty sections remain unproved")
	noRestart := fs.Bool("no-restart", false, "write validated configuration without restarting the daemon")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printSetupReportingUsage(os.Stdout)
			return 0
		}
		fmt.Fprintf(os.Stderr, "canary setup reporting: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "canary setup reporting: no positional arguments are accepted")
		return 2
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary setup reporting: an interactive terminal is required so the token is never echoed")
		return 1
	}
	defer tty.Close()
	reader := bufio.NewReader(tty)

	printReportingChecklist(os.Stdout)
	fmt.Fprint(os.Stdout, "IBKR Activity Flex Query ID: ")
	queryID, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary setup reporting: could not read the Query ID")
		return 1
	}
	queryID = strings.TrimSpace(queryID)
	if !validSetupReportingQueryID(queryID) {
		fmt.Fprintln(os.Stderr, "canary setup reporting: the Query ID must contain only digits")
		return 1
	}
	printSetupReportingTokenPrompt(os.Stdout)
	token, err := readSetupSecret(tty, reader)
	fmt.Fprintln(os.Stdout)
	if err != nil || len(token) == 0 {
		clear(token)
		fmt.Fprintln(os.Stderr, "canary setup reporting: could not read a non-empty token")
		return 1
	}
	defer clear(token)
	fmt.Fprintln(os.Stdout, "Token received; validating it without displaying or logging it.")

	configPath, err := filepath.Abs(config.DefaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary setup reporting: the local config path could not be resolved")
		return 1
	}
	configPath = filepath.Clean(configPath)
	candidatePath, err := writeCandidateReportingToken(filepath.Dir(configPath), token)
	clear(token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary setup reporting: could not create the private candidate token file")
		return 1
	}
	promoted := false
	defer func() {
		if !promoted {
			_ = os.Remove(candidatePath)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	conn, err := dial.Connect(dial.DefaultSocketPath())
	if errors.Is(err, dial.ErrSocketMissing) {
		conn, err = dial.AutospawnAndConnectContext(ctx, dial.DefaultSocketPath())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary setup reporting: the Canary daemon could not be reached; existing reporting configuration was not changed")
		return 1
	}
	var validation rpc.ReportingValidationResult
	err = conn.Call(ctx, rpc.MethodReportingValidate, rpc.ReportingValidateParams{QueryID: queryID, TokenPath: candidatePath}, &validation)
	_ = conn.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary setup reporting: candidate validation failed safely; existing reporting configuration was not changed")
		return 1
	}
	renderSetupReportingValidation(os.Stdout, &validation)
	if !validation.ReadyForRotation {
		fmt.Fprintln(os.Stderr, "Existing reporting configuration was not changed.")
		return 1
	}
	if validation.Outcome == rpc.ReportingValidationUnproved && !*acceptUnproved {
		fmt.Fprint(os.Stdout, "Activate this candidate while empty sections remain unproved? [y/N]: ")
		answer, readErr := reader.ReadString('\n')
		if readErr != nil || !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Fprintln(os.Stderr, "Existing reporting configuration was not changed.")
			return 1
		}
	}

	backup, err := config.UpdateFlexConfigAtomic(configPath, queryID, candidatePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary setup reporting: candidate was valid, but local configuration promotion failed")
		return 1
	}
	promoted = true
	fmt.Fprintln(os.Stdout, "Validated reporting configuration activated atomically.")
	if backup != "" {
		fmt.Fprintln(os.Stdout, "The previous local config and token remain available for rollback.")
	}
	keepTokens := []string{candidatePath}
	if backup != "" {
		backupConfig, loadErr := config.Load(backup)
		if loadErr != nil {
			fmt.Fprintln(os.Stderr, "Reporting is active, but superseded token cleanup was skipped because the rollback config could not be read.")
		} else {
			keepTokens = append(keepTokens, backupConfig.Flex.TokenPath)
			if err := pruneSupersededReportingTokens(filepath.Dir(configPath), keepTokens...); err != nil {
				fmt.Fprintln(os.Stderr, "Reporting is active, but a superseded private token file could not be removed.")
			}
		}
	} else if err := pruneSupersededReportingTokens(filepath.Dir(configPath), keepTokens...); err != nil {
		fmt.Fprintln(os.Stderr, "Reporting is active, but a superseded private token file could not be removed.")
	}
	if *noRestart {
		fmt.Fprintln(os.Stdout, "Run `canary restart`, then `canary reporting status` to load and check it.")
		return 0
	}
	if code := cli.RunRestart(ctx, nil, os.Stdout, os.Stderr); code != 0 {
		fmt.Fprintln(os.Stderr, "Reporting configuration is saved, but the daemon restart failed; run `canary restart`.")
		return code
	}
	return printPostSetupReportingStatus(ctx)
}

func printSetupReportingUsage(out io.Writer) {
	fmt.Fprintln(out, "canary setup reporting — securely validate and activate IBKR broker reporting")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage: canary setup reporting [--accept-unproved] [--no-restart]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "The wizard reads the Query ID and token interactively, validates a candidate")
	fmt.Fprintln(out, "without retaining its XML, and changes local config only after validation.")
}

func printReportingChecklist(out io.Writer) {
	fmt.Fprintln(out, "IBKR Activity Flex Query checklist — select all fields:")
	fmt.Fprintln(out, "  Reporting → Flex Queries → Activity Flex Query → XML")
	fmt.Fprintln(out, "  Configure with AI may draft it; use Edit Manually to verify before saving.")
	for _, section := range flexstmt.CanonicalQueryManifest() {
		detail := ""
		if section.LevelOfDetail != "" {
			detail = " — " + section.LevelOfDetail
		}
		fmt.Fprintf(out, "  - %s%s\n", section.Label, detail)
	}
	fmt.Fprintln(out, "  Optional when offered: Currency Conversion Rate")
	fmt.Fprintln(out, "Enable Flex Web Service and generate a token.")
	fmt.Fprintln(out, "Canary validates a saved Query ID; IBKR exposes no documented query-definition API.")
	fmt.Fprintln(out, "Exact fields and screenshots: https://osauer.dev/canary/docs/start/reporting.html")
	fmt.Fprintln(out)
}

func printSetupReportingTokenPrompt(out io.Writer) {
	fmt.Fprintln(out, "Paste the Flex Web Service token using your terminal's Paste command")
	fmt.Fprintln(out, "(Command-V on macOS), then press Return. Nothing will appear while you paste.")
	fmt.Fprint(out, "Token: ")
}

func readSetupSecretLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(line)), nil
}

func renderSetupReportingValidation(out io.Writer, result *rpc.ReportingValidationResult) {
	fmt.Fprintf(out, "Candidate validation: %s", result.Outcome)
	if result.Reason != "" {
		fmt.Fprintf(out, " (%s)", result.Reason)
	}
	if result.BrokerCode != "" {
		fmt.Fprintf(out, " · IBKR code %s", result.BrokerCode)
	}
	fmt.Fprintln(out)
	for _, requirement := range result.MissingRequirements {
		fmt.Fprintf(out, "  missing: %s\n", requirement)
	}
	for _, section := range result.UnprovedSections {
		fmt.Fprintf(out, "  unproved: %s (no representative row)\n", section)
	}
	if result.SchemaFingerprint != "" {
		fmt.Fprintf(out, "  schema: %s\n", result.SchemaFingerprint)
	}
	if result.Action != "" {
		fmt.Fprintf(out, "  action: %s\n", result.Action)
	}
}

func writeCandidateReportingToken(dir string, token []byte) (string, error) {
	if len(token) == 0 {
		return "", errors.New("empty token")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "flex-token-*")
	if err != nil {
		return "", err
	}
	path := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := tmp.Write(append(append([]byte(nil), token...), '\n')); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

func pruneSupersededReportingTokens(dir string, keepPaths ...string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if filepath.Dir(dir) == dir {
		return errors.New("refusing to prune reporting tokens from a filesystem root")
	}
	keep := make(map[string]bool, len(keepPaths))
	for _, path := range keepPaths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		absolute, err := absoluteReportingTokenPath(path)
		if err != nil {
			return err
		}
		keep[filepath.Clean(absolute)] = true
		if !filepath.IsAbs(path) && path != "~" && !strings.HasPrefix(path, "~/") {
			keep[filepath.Clean(filepath.Join(dir, path))] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "flex-token-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if keep[path] {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func absoluteReportingTokenPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return filepath.Abs(path)
}

func validSetupReportingQueryID(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func printPostSetupReportingStatus(ctx context.Context) int {
	conn, err := dial.Connect(dial.DefaultSocketPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "Reporting configuration is active; run `canary reporting status` to inspect it.")
		return 0
	}
	defer conn.Close()
	var status rpc.ReportingStatusResult
	if err := conn.Call(ctx, rpc.MethodReportingStatus, struct{}{}, &status); err != nil {
		fmt.Fprintln(os.Stderr, "Reporting configuration is active; run `canary reporting status` to inspect it.")
		return 0
	}
	fmt.Fprintf(os.Stdout, "Reporting is now %s", status.State)
	if status.Reason != "" {
		fmt.Fprintf(os.Stdout, " (%s)", status.Reason)
	}
	fmt.Fprintln(os.Stdout, ".")
	fmt.Fprintln(os.Stdout, "Edge will pace four year-backfill chunks, then collect exact-contract daily bars; check `canary edge --window 365d` in several minutes.")
	return 0
}
