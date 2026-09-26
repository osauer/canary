package daemon

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// tomlDoc is a line-preserving view of a policy file. Canary edits the files
// the owner reads and annotates, so it changes only the lines it must and
// keeps every comment and every other value byte for byte. It understands
// the subset Canary's policy files use: [table] and [a.b] headers, bare
// key = value lines, and arrays that may span lines. Inline tables and
// dotted keys are left alone; an edit that needs one is refused.
type tomlDoc struct {
	lines []string
}

var (
	tomlHeaderRe = regexp.MustCompile(`^\s*\[\s*([A-Za-z0-9_.\-]+)\s*\]\s*(#.*)?$`)
	tomlKeyRe    = regexp.MustCompile(`^\s*("(?:[^"\\]|\\.)*"|[A-Za-z0-9_\-]+)\s*=`)
)

func parseTOMLDoc(data []byte) *tomlDoc {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return &tomlDoc{}
	}
	return &tomlDoc{lines: strings.Split(text, "\n")}
}

func (d *tomlDoc) bytes() []byte {
	return []byte(strings.Join(d.lines, "\n") + "\n")
}

// tomlKeySpan is one key's lines: [start, end) with the table it sits in.
type tomlKeySpan struct {
	table string
	key   string
	start int
	end   int
}

// spans lists every bare key = value in the document with its table.
func (d *tomlDoc) spans() []tomlKeySpan {
	var out []tomlKeySpan
	table := ""
	for i := 0; i < len(d.lines); i++ {
		line := d.lines[i]
		if m := tomlHeaderRe.FindStringSubmatch(line); m != nil {
			table = m[1]
			continue
		}
		m := tomlKeyRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		end := i + 1
		value := tomlStripComment(line[strings.Index(line, "=")+1:])
		if strings.Count(value, "[") > strings.Count(value, "]") {
			depth := strings.Count(value, "[") - strings.Count(value, "]")
			for end < len(d.lines) && depth > 0 {
				v := tomlStripComment(d.lines[end])
				depth += strings.Count(v, "[") - strings.Count(v, "]")
				end++
			}
		}
		key := m[1]
		if unquoted, err := strconv.Unquote(key); err == nil && strings.HasPrefix(key, `"`) {
			key = unquoted
		}
		out = append(out, tomlKeySpan{table: table, key: key, start: i, end: end})
		i = end - 1
	}
	return out
}

// tomlStripComment drops a trailing # comment outside a quoted string.
func tomlStripComment(s string) string {
	inString := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inString && c == '\\' && quote == '"':
			i++
		case inString && c == quote:
			inString = false
		case !inString && (c == '"' || c == '\''):
			inString, quote = true, c
		case !inString && c == '#':
			return s[:i]
		}
	}
	return s
}

// find returns the span of table.key, or false.
func (d *tomlDoc) find(table, key string) (tomlKeySpan, bool) {
	for _, s := range d.spans() {
		if s.table == table && s.key == key {
			return s, true
		}
	}
	return tomlKeySpan{}, false
}

// headerLine returns the index of [table], or -1.
func (d *tomlDoc) headerLine(table string) int {
	for i, line := range d.lines {
		if m := tomlHeaderRe.FindStringSubmatch(line); m != nil && m[1] == table {
			return i
		}
	}
	return -1
}

// firstHeaderLine returns the index of the first table header, or len.
func (d *tomlDoc) firstHeaderLine() int {
	for i, line := range d.lines {
		if tomlHeaderRe.MatchString(line) {
			return i
		}
	}
	return len(d.lines)
}

// set replaces table.key's value in place, keeping a trailing comment on a
// single-line value, or inserts the key when it is absent.
func (d *tomlDoc) set(table, key, value string, comment []string) {
	if s, ok := d.find(table, key); ok {
		line := d.lines[s.start]
		trailing := ""
		if s.end == s.start+1 {
			rest := line[strings.Index(line, "=")+1:]
			if stripped := tomlStripComment(rest); len(stripped) < len(rest) {
				trailing = "  " + strings.TrimSpace(rest[len(stripped):])
			}
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		replacement := indent + tomlBareKey(key) + " = " + value + trailing
		d.lines = append(d.lines[:s.start], append([]string{replacement}, d.lines[s.end:]...)...)
		return
	}
	d.insert(table, append(append([]string{}, comment...), tomlBareKey(key)+" = "+value))
}

// insert adds lines at the end of table's key block (before trailing blank
// or comment lines ahead of the next header), creating the table at the end
// of the file when it is missing. Top-level lines go before the first table.
func (d *tomlDoc) insert(table string, lines []string) {
	if table == "" {
		at := d.firstHeaderLine()
		for at > 0 && strings.TrimSpace(d.lines[at-1]) == "" {
			at--
		}
		d.lines = append(d.lines[:at], append(append([]string{}, lines...), d.lines[at:]...)...)
		return
	}
	header := d.headerLine(table)
	if header < 0 {
		if len(d.lines) > 0 && strings.TrimSpace(d.lines[len(d.lines)-1]) != "" {
			d.lines = append(d.lines, "")
		}
		d.lines = append(d.lines, "["+table+"]")
		d.lines = append(d.lines, lines...)
		return
	}
	at := header + 1
	for _, s := range d.spans() {
		if s.table == table && s.end > at {
			at = s.end
		}
	}
	d.lines = append(d.lines[:at], append(append([]string{}, lines...), d.lines[at:]...)...)
}

// commentOut turns table.key's lines into comments with a note.
func (d *tomlDoc) commentOut(table, key, note string) bool {
	s, ok := d.find(table, key)
	if !ok {
		return false
	}
	for i := s.start; i < s.end; i++ {
		d.lines[i] = "# " + d.lines[i]
	}
	d.lines[s.end-1] += "  # " + note
	return true
}

// remove deletes table.key's lines.
func (d *tomlDoc) remove(table, key string) bool {
	s, ok := d.find(table, key)
	if !ok {
		return false
	}
	d.lines = append(d.lines[:s.start], d.lines[s.end:]...)
	return true
}

var tomlBareKeyRe = regexp.MustCompile(`^[A-Za-z0-9_\-]+$`)

// tomlBareKey quotes a key that is not a TOML bare key.
func tomlBareKey(k string) string {
	if tomlBareKeyRe.MatchString(k) {
		return k
	}
	return fmt.Sprintf("%q", k)
}
