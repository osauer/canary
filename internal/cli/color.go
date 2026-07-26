package cli

import (
	"io"
	"os"
	"strings"
)

// ANSI SGR codes used across renderers. Kept to a tight palette so colored
// output stays signal — green/red for sign, dim for the absent/zero
// placeholder, yellow for warning badges, bold for the section title and
// the single hero number per screen (e.g. Net Liquidation on `canary account`,
// EXPECTED MOVE on `canary chain`, effective/dollar delta on the Portfolio
// aggregate). Bold is the strongest emphasis — use it sparingly.
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiDim    = "\x1b[2m"
	ansiBold   = "\x1b[1m"
)

// ShouldColor reports whether ANSI color escapes should be emitted to w.
// Policy, in order:
//
//  1. CANARY_COLOR=always  → on (overrides TTY check)
//  2. CANARY_COLOR=never   → off
//  3. NO_COLOR set (any) → off (https://no-color.org)
//  4. w is a character device (interactive terminal) → on
//  5. otherwise → off (pipes, file redirects, bytes.Buffer in tests)
//
// Computed once per process and cached on Env.Color so colored renderers
// don't re-syscall on every value.
func ShouldColor(w io.Writer) bool {
	// docgen:env CANARY_COLOR | Force terminal colour on (`always`), off (`never`); any other value defers to NO_COLOR + TTY detection.
	switch os.Getenv("CANARY_COLOR") {
	case "always":
		return true
	case "never":
		return false
	}
	// docgen:env NO_COLOR | Standard https://no-color.org/ override. Any non-empty value disables colour regardless of CANARY_COLOR (unless CANARY_COLOR=always).
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// red returns s wrapped in red ANSI codes when env.Color is on, else s
// unchanged. The other helpers below mirror this contract; pre-checking
// env.Color at the call site is fine but not required.
func (e *Env) red(s string) string {
	if e == nil || !e.Color {
		return s
	}
	return ansiRed + s + ansiReset
}

func (e *Env) green(s string) string {
	if e == nil || !e.Color {
		return s
	}
	return ansiGreen + s + ansiReset
}

func (e *Env) yellow(s string) string {
	if e == nil || !e.Color {
		return s
	}
	return ansiYellow + s + ansiReset
}

func (e *Env) dim(s string) string {
	if e == nil || !e.Color {
		return s
	}
	return ansiDim + s + ansiReset
}

func (e *Env) bold(s string) string {
	if e == nil || !e.Color {
		return s
	}
	return ansiBold + s + ansiReset
}

// rule returns a dim horizontal-rule string of width n cells, used as a
// section separator under titles. Uses the box-drawing char "─" (one
// terminal cell wide) so visible width matches n regardless of color
// state. Width chosen by caller — a rule wider than the content beneath
// reads as a separator, narrower as a title underline.
func (e *Env) rule(n int) string {
	return e.dim(strings.Repeat("─", n))
}
