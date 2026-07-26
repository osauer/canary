package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/osauer/canary/v2/internal/productidentity"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBrand  = "\x1b[1;37;44m"
	ansiOK     = "\x1b[32m"
	ansiDanger = "\x1b[31m"
	ansiWarn   = "\x1b[33m"
	ansiInfo   = "\x1b[36m"
	ansiDim    = "\x1b[2m"
	ansiStrong = "\x1b[1m"
)

func render(m *model) string {
	l := computeLayout(m.size)
	lines := make([]string, m.size.Rows)
	lines[l.status.y] = fit(statusLine(m), l.status.w)
	if l.showTicker {
		lines[l.ticker.y] = fit(tickerLine(m, l.ticker.w), l.ticker.w)
	}
	output := outputLines(m)
	start := max(0, len(output)-l.output.h-m.scroll)
	end := min(len(output), start+l.output.h)
	for i, line := range output[start:end] {
		lines[l.output.y+i] = fit(line, l.output.w)
	}
	if l.showWarning {
		box := riskPanelLines(m, l.warning.w, l.warning.h)
		for i, line := range box {
			if i >= l.warning.h || l.warning.y+i >= len(lines) {
				break
			}
			base := padRight(lines[l.warning.y+i], max(0, l.warning.x-1))
			lines[l.warning.y+i] = fit(base+styleDim("|")+line, m.size.Cols)
		}
	}
	prompt := promptLines(m, l.prompt.w)
	for i, line := range prompt {
		if l.prompt.y+i < len(lines) {
			lines[l.prompt.y+i] = fit(line, l.prompt.w)
		}
	}
	var b strings.Builder
	b.WriteString("\x1b[?25l")
	for i := range m.size.Rows {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", i+1)
		b.WriteString(lines[i])
	}
	row, col := promptCursor(m, l)
	fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", row, col)
	return b.String()
}

func outputLines(m *model) []string {
	if len(m.outputs) == 0 {
		return []string{styleDim("No output yet.")}
	}
	lines := []string{}
	for _, block := range m.outputs {
		header := fmt.Sprintf("%s  %s", styleDim(block.Started.Format("15:04:05")), block.Command)
		if !block.Finished.IsZero() && block.Command != "welcome" {
			exit := styleOK("ok")
			if block.ExitCode != 0 {
				exit = styleDanger(fmt.Sprintf("exit %d", block.ExitCode))
			}
			header += fmt.Sprintf("  %s  %s", exit, styleDim(block.Finished.Sub(block.Started).Round(time.Millisecond).String()))
		}
		lines = append(lines, header)
		for _, line := range splitBlock(block.Stdout) {
			lines = append(lines, "  "+line)
		}
		for _, line := range splitBlock(block.Stderr) {
			lines = append(lines, styleDanger("! ")+" "+line)
		}
		lines = append(lines, "")
	}
	if m.message != "" {
		lines = append(lines, styleDim(m.message))
	}
	return lines
}

func promptLines(m *model, width int) []string {
	line := m.editor.line()
	visible := promptPrefix(m) + line
	out := []string{fit(visible, width)}
	switch {
	case m.confirm != nil:
		out = append(out, fit(styleWarn(m.confirm.message), width))
	case len(m.editor.suggestions) > 0:
		out = append(out, fit(styleInfo("complete: ")+strings.Join(m.editor.suggestions, "  "), width))
	case m.liveErr != "":
		out = append(out, fit(styleDanger("live: "+m.liveErr), width))
	default:
		out = append(out, fit(styleDim("Tab complete | arrows history | Ctrl-L clear | Ctrl-C cancel/quit | Ctrl-D quit"), width))
	}
	return out
}

func promptPrefix(m *model) string {
	prefix := styleOK(productidentity.Executable + "> ")
	if m.confirm != nil {
		prefix = styleWarn("confirm> ")
	}
	return prefix
}

func promptCursor(m *model, l screenLayout) (int, int) {
	col := len([]rune(stripControl(promptPrefix(m)))) + m.editor.cursor + 1
	if l.prompt.w > 0 {
		col = min(col, l.prompt.w)
	}
	return l.prompt.y + 1, max(1, col)
}

func splitBlock(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	visible := 0
	truncated := false
	for len(s) > 0 {
		seq, n, ok := ansiSequence(s)
		if ok {
			b.WriteString(seq)
			s = s[n:]
			continue
		}
		r, n := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && n == 1 {
			s = s[n:]
			continue
		}
		s = s[n:]
		if r == '\t' {
			r = ' '
		}
		if r < 32 || r == 127 {
			continue
		}
		limit := width
		if visible >= limit {
			truncated = true
			break
		}
		if visible == limit-1 && visibleWidth(s) > 0 {
			b.WriteByte('~')
			truncated = true
			break
		}
		b.WriteRune(r)
		visible++
	}
	if truncated {
		b.WriteString(ansiReset)
	}
	return b.String()
}

func stripControl(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		if _, n, ok := ansiSequence(s); ok {
			s = s[n:]
			continue
		}
		r, n := firstVisibleRune(s)
		s = s[n:]
		if r == 0 {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func firstVisibleRune(s string) (rune, int) {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && n == 1 {
		return 0, n
	}
	if r == '\t' {
		return ' ', n
	}
	if r < 32 || r == 127 {
		return 0, n
	}
	return r, n
}

func visibleWidth(s string) int {
	return len([]rune(stripControl(s)))
}

func ansiSequence(s string) (string, int, bool) {
	if len(s) < 3 || s[0] != 0x1b || s[1] != '[' {
		return "", 0, false
	}
	for i := 2; i < len(s); i++ {
		c := s[i]
		if c >= 0x40 && c <= 0x7e {
			if c == 'm' {
				return s[:i+1], i + 1, true
			}
			return "", i + 1, true
		}
	}
	return "", len(s), true
}

func padRight(s string, width int) string {
	w := visibleWidth(s)
	if w >= width {
		return fit(s, width)
	}
	return s + strings.Repeat(" ", width-w)
}

func style(code, s string) string {
	if s == "" {
		return ""
	}
	return code + s + ansiReset
}

func styleBrand(s string) string {
	return style(ansiBrand, s)
}

func styleOK(s string) string {
	return style(ansiOK, s)
}

func styleDanger(s string) string {
	return style(ansiDanger, s)
}

func styleWarn(s string) string {
	return style(ansiWarn, s)
}

func styleCanary(s string) string {
	return styleWarn(s)
}

func styleInfo(s string) string {
	return style(ansiInfo, s)
}

func styleStrong(s string) string {
	return style(ansiStrong, s)
}

func styleDim(s string) string {
	return style(ansiDim, s)
}
