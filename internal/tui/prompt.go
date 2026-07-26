package tui

import (
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/cli"
)

type lineEditor struct {
	input        []rune
	cursor       int
	history      []string
	historyIndex int
	savedInput   string
	suggestions  []string
}

func newLineEditor() *lineEditor {
	return &lineEditor{historyIndex: -1}
}

func (e *lineEditor) line() string {
	return string(e.input)
}

func (e *lineEditor) setLine(line string) {
	e.input = []rune(line)
	e.cursor = len(e.input)
	e.historyIndex = -1
	e.savedInput = ""
}

func (e *lineEditor) clear() {
	e.setLine("")
	e.suggestions = nil
}

func (e *lineEditor) insert(r rune) {
	e.input = slices.Insert(e.input, e.cursor, r)
	e.cursor++
	e.suggestions = nil
}

func (e *lineEditor) backspace() {
	if e.cursor == 0 {
		return
	}
	e.input = slices.Delete(e.input, e.cursor-1, e.cursor)
	e.cursor--
	e.suggestions = nil
}

func (e *lineEditor) delete() {
	if e.cursor >= len(e.input) {
		return
	}
	e.input = slices.Delete(e.input, e.cursor, e.cursor+1)
	e.suggestions = nil
}

func (e *lineEditor) left() {
	if e.cursor > 0 {
		e.cursor--
	}
}

func (e *lineEditor) right() {
	if e.cursor < len(e.input) {
		e.cursor++
	}
}

func (e *lineEditor) home() {
	e.cursor = 0
}

func (e *lineEditor) end() {
	e.cursor = len(e.input)
}

func (e *lineEditor) acceptHistory(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if len(e.history) == 0 || e.history[len(e.history)-1] != line {
		e.history = append(e.history, line)
	}
	e.historyIndex = -1
	e.savedInput = ""
}

func (e *lineEditor) historyUp() {
	if len(e.history) == 0 {
		return
	}
	if e.historyIndex < 0 {
		e.savedInput = e.line()
		e.historyIndex = len(e.history) - 1
	} else if e.historyIndex > 0 {
		e.historyIndex--
	}
	e.input = []rune(e.history[e.historyIndex])
	e.cursor = len(e.input)
}

func (e *lineEditor) historyDown() {
	if e.historyIndex < 0 {
		return
	}
	if e.historyIndex < len(e.history)-1 {
		e.historyIndex++
		e.input = []rune(e.history[e.historyIndex])
	} else {
		e.historyIndex = -1
		e.input = []rune(e.savedInput)
		e.savedInput = ""
	}
	e.cursor = len(e.input)
}

func (e *lineEditor) complete(catalog []cli.CommandSpec, dynamic []string) {
	line := e.line()
	completions := completeLine(line, e.cursor, catalog, dynamic)
	e.suggestions = completions
	if len(completions) != 1 {
		return
	}
	start, end := tokenBounds([]rune(line), e.cursor)
	next := []rune(completions[0])
	repl := append(next, ' ')
	e.input = append(append(append([]rune(nil), e.input[:start]...), repl...), e.input[end:]...)
	e.cursor = start + len(repl)
	e.suggestions = nil
}

func completeLine(line string, cursor int, catalog []cli.CommandSpec, dynamic []string) []string {
	rs := []rune(line)
	if cursor > len(rs) {
		cursor = len(rs)
	}
	atTokenStart := cursor > 0 && isPromptSpace(rs[cursor-1])
	start, _ := tokenBounds(rs, cursor)
	prefix := string(rs[start:cursor])
	left := strings.TrimSpace(string(rs[:cursor]))
	tokens, _ := parseCommandLine(left)
	tokens = stripExecutablePrefix(tokens)
	if strings.HasPrefix(left, ":") {
		return prefixed(prefix, []string{":help", ":clear", ":quit", ":layout"})
	}
	if len(tokens) == 0 || (len(tokens) == 1 && !atTokenStart) {
		names := make([]string, 0, len(catalog))
		for _, spec := range catalog {
			names = append(names, spec.Name)
		}
		return prefixed(prefix, names)
	}
	spec, ok := findSpec(catalog, tokens[0])
	if !ok {
		return nil
	}
	if strings.HasPrefix(prefix, "-") {
		opts := make([]string, 0, len(spec.Flags))
		for _, flag := range spec.Flags {
			opts = append(opts, "--"+flag.Name)
		}
		return prefixed(prefix, opts)
	}
	if len(tokens) >= 2 {
		prevIdx := len(tokens) - 2
		if atTokenStart {
			prevIdx = len(tokens) - 1
		}
		prev := tokens[prevIdx]
		if strings.HasPrefix(prev, "-") {
			prev = strings.TrimLeft(prev, "-")
			if i := strings.IndexByte(prev, '='); i >= 0 {
				prev = prev[:i]
			}
			for _, flag := range spec.Flags {
				if flag.Name == prev && len(flag.Values) > 0 {
					return prefixed(prefix, flag.Values)
				}
			}
		}
	}
	if len(tokens) <= 2 {
		subs := make([]string, 0, len(spec.Subcommands))
		for _, sub := range spec.Subcommands {
			subs = append(subs, sub.Name)
		}
		if matches := prefixed(prefix, subs); len(matches) > 0 {
			return matches
		}
	}
	return prefixed(prefix, dynamic)
}

func prefixed(prefix string, values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	upperPrefix := strings.ToUpper(prefix)
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(value), upperPrefix) {
			seen[value] = true
			out = append(out, value)
		}
	}
	slices.Sort(out)
	return out
}

func tokenBounds(rs []rune, cursor int) (int, int) {
	start := cursor
	for start > 0 && !isPromptSpace(rs[start-1]) {
		start--
	}
	end := cursor
	for end < len(rs) && !isPromptSpace(rs[end]) {
		end++
	}
	return start, end
}

func isPromptSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func findSpec(catalog []cli.CommandSpec, name string) (cli.CommandSpec, bool) {
	for _, spec := range catalog {
		if spec.Name == name {
			return spec, true
		}
	}
	return cli.CommandSpec{}, false
}
