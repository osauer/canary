package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/cli"
	"github.com/osauer/ibkr/v2/internal/dial"
)

type commandRunner struct {
	version    string
	socketPath string
	color      bool
}

type commandResult struct {
	block outputBlock
}

func (r commandRunner) run(ctx context.Context, line string, started time.Time, catalog []cli.CommandSpec) commandResult {
	var stdout, stderr bytes.Buffer
	code := r.runParsed(ctx, line, catalog, &stdout, &stderr)
	return commandResult{block: outputBlock{
		Command:  line,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: code,
		Started:  started,
		Finished: time.Now(),
	}}
}

func (r commandRunner) runParsed(ctx context.Context, line string, catalog []cli.CommandSpec, stdout, stderr *bytes.Buffer) int {
	tokens, err := parseCommandLine(line)
	if err != nil {
		fmt.Fprintf(stderr, "ibkr: %v\n", err)
		return 2
	}
	if len(tokens) == 0 {
		return 0
	}
	if tokens[0] == "ibkr" {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 || tokens[0] == "help" {
		cli.PrintUsage(stdout)
		return 0
	}
	cmd, args := tokens[0], tokens[1:]
	if cmd == "--version" || cmd == "version" {
		fmt.Fprintf(stdout, "ibkr %s\n", r.version)
		return 0
	}
	spec, known := findSpec(catalog, cmd)
	if !known {
		fmt.Fprintln(stderr, unknownCommandMessage(cmd, catalog))
		return 2
	}
	if cmd == "update" {
		if !hasFlag(args, "check") && !hasFlag(args, "restart") && !hasFlag(args, "no-restart") {
			fmt.Fprintln(stderr, "ibkr update: inside the TUI, pass --restart or --no-restart to avoid an interactive installer prompt")
			return 2
		}
		return cli.RunUpdate(ctx, args, r.version, bytes.NewReader(nil), stdout, stderr)
	}
	if spec.TUI == cli.TUIExternal {
		fmt.Fprintf(stdout, "Run `ibkr %s` in a normal terminal. This command owns a process, installer, or stdio lifecycle outside the TUI.\n", strings.Join(tokens, " "))
		return 0
	}
	if cmd == "restart" {
		return cli.RunRestart(ctx, args, stdout, stderr)
	}
	if hasHelpFlag(args) || !needsDaemon(cmd, args) {
		env := &cli.Env{Stdout: stdout, Stderr: stderr, Version: r.version, Color: r.color}
		return cli.Run(ctx, env, cmd, args)
	}
	runCtx := ctx
	if !isStreamingInvocation(cmd, args) {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, commandBudget(cmd))
		defer cancel()
	}
	conn, err := connectDaemon(runCtx, r.socketPath)
	if err != nil {
		fmt.Fprintf(stderr, "ibkr: %v\n", err)
		return 1
	}
	defer conn.Close()
	env := &cli.Env{Stdout: stdout, Stderr: stderr, Conn: conn, Version: r.version, Color: r.color}
	return cli.Run(runCtx, env, cmd, args)
}

func unknownCommandMessage(cmd string, catalog []cli.CommandSpec) string {
	matches := prefixed(cmd, commandNames(catalog))
	if len(matches) == 0 {
		matches = fuzzyCommandMatches(cmd, catalog, 3)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "unknown command %q", cmd)
	if len(matches) > 0 {
		b.WriteString("; did you mean ")
		for i, match := range matches {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(match)
		}
		b.WriteByte('?')
	} else {
		b.WriteString("; press Tab for completion or run :help")
	}
	return b.String()
}

func commandNames(catalog []cli.CommandSpec) []string {
	names := make([]string, 0, len(catalog))
	for _, spec := range catalog {
		names = append(names, spec.Name)
	}
	return names
}

func fuzzyCommandMatches(cmd string, catalog []cli.CommandSpec, limit int) []string {
	type candidate struct {
		name string
		dist int
	}
	cands := []candidate{}
	for _, spec := range catalog {
		dist := editDistance(strings.ToLower(cmd), strings.ToLower(spec.Name))
		if dist <= 3 || strings.Contains(spec.Name, cmd) {
			cands = append(cands, candidate{name: spec.Name, dist: dist})
		}
	}
	slices.SortFunc(cands, func(a, b candidate) int {
		if a.dist != b.dist {
			return a.dist - b.dist
		}
		return strings.Compare(a.name, b.name)
	})
	out := []string{}
	for _, cand := range cands {
		if len(out) >= limit {
			break
		}
		out = append(out, cand.name)
	}
	return out
}

func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ra := range ar {
		cur := make([]int, len(br)+1)
		cur[0] = i + 1
		for j, rb := range br {
			cost := 0
			if ra != rb {
				cost = 1
			}
			cur[j+1] = min(cur[j]+1, prev[j+1]+1, prev[j]+cost)
		}
		prev = cur
	}
	return prev[len(br)]
}

func connectDaemon(ctx context.Context, socketPath string) (*dial.Conn, error) {
	path := socketPath
	if path == "" {
		path = dial.DefaultSocketPath()
	}
	conn, err := dial.Connect(path)
	if errors.Is(err, dial.ErrSocketMissing) {
		conn, err = dial.AutospawnAndConnectContext(ctx, path)
	}
	return conn, err
}

func needsDaemon(cmd string, args []string) bool {
	if cmd == "backtest" {
		return false
	}
	if cmd == "watch" && !isWatchDaemonInvocation(args) {
		return false
	}
	return true
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" || arg == "-help" {
			return true
		}
	}
	return false
}

func commandBudget(cmd string) time.Duration {
	if cmd == "scan" || cmd == "technical" || cmd == "stress" {
		return 90 * time.Second
	}
	return 60 * time.Second
}

func isStreamingInvocation(cmd string, args []string) bool {
	switch cmd {
	case "quote", "account", "positions", "watch", "regime":
	default:
		return false
	}
	for _, arg := range args {
		if arg == "--watch" || arg == "-watch" || arg == "--watch=true" {
			return true
		}
	}
	return false
}

func isWatchDaemonInvocation(args []string) bool {
	localOnly := false
	for _, arg := range args {
		name := strings.TrimLeft(arg, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		switch arg {
		case "--watch", "-watch", "--watch=true", "--quotes", "-quotes", "--quotes=true":
			return true
		}
		switch name {
		case "add", "remove", "list", "clear":
			localOnly = true
		case "quotes", "watch", "timeout":
			return true
		}
	}
	return !localOnly
}

func parseCommandLine(line string) ([]string, error) {
	tokens := []string{}
	var cur strings.Builder
	var quote rune
	escaped := false
	for _, r := range line {
		if escaped {
			cur.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case ' ', '\t', '\n', '\r':
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if escaped {
		cur.WriteRune('\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}
