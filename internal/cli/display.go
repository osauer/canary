package cli

import (
	"fmt"
	"io"
	"strings"
)

// displayLine keeps untrusted text out of terminal control sequences. Styling
// is applied only after wrapping, so a narrow terminal cannot split an escape.
func displayLine(env *Env, text string, style func(string) string) {
	indent := text[:len(text)-len(strings.TrimLeft(text, " "))]
	riskDisplayLine(env, indent, strings.TrimLeft(text, " "), style)
}

func displayRow(env *Env, out io.Writer, label, value string) {
	copyEnv := *env
	copyEnv.Stdout = out
	prefix := env.dim(fmt.Sprintf("%-14s", label)) + " "
	riskDisplayLine(&copyEnv, prefix, value, nil)
}

func displayNumber(value *float64, scale float64, format string) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf(format, *value*scale)
}

// displayWrapped preserves diagnostic lines and their indentation, while
// bounding the older full-detail renderers to the current terminal width.
func displayWrapped(env *Env, text string) {
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		indent := strings.Repeat(" ", min(6, len(line)-len(strings.TrimLeft(line, " "))))
		for _, part := range wrapVisibleText(sanitizeRunText(strings.TrimSpace(line)), max(1, briefProseWidth(env.Stdout)-len(indent))) {
			fmt.Fprintln(env.Stdout, indent+part)
		}
	}
}
