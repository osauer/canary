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

// renameTable renames the [from] header and every [from.sub] header to [to]
// and [to.sub], keeping indentation and a trailing comment; it reports
// whether any header changed. Keys move with their headers, so values and
// comments stay byte for byte.
func (d *tomlDoc) renameTable(from, to string) bool {
	changed := false
	for i, line := range d.lines {
		m := tomlHeaderRe.FindStringSubmatch(line)
		if m == nil || (m[1] != from && !strings.HasPrefix(m[1], from+".")) {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		comment := ""
		if m[2] != "" {
			comment = "  " + m[2]
		}
		d.lines[i] = indent + "[" + to + m[1][len(from):] + "]" + comment
		changed = true
	}
	return changed
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

// valueText returns table.key's value as written, without its comment, and
// its trailing comment ("# …"), for a single-line value.
func (d *tomlDoc) valueText(s tomlKeySpan) (value, comment string) {
	line := d.lines[s.start]
	rest := line[strings.Index(line, "=")+1:]
	stripped := tomlStripComment(rest)
	if s.end == s.start+1 {
		comment = strings.TrimSpace(rest[len(stripped):])
	}
	return strings.TrimSpace(stripped), comment
}

// setNoted writes table.key = value and records why: a trailing comment
// that Canary wrote (canaryWrote) is replaced by note, an owner's trailing
// comment stays and note goes on its own line above the key (replacing a
// note Canary put there before for the same key), and a key the table
// lacks is added with note as its trailing comment. An empty note writes the
// value alone and keeps the line's comment. It returns the value the line
// held before, as written, and whether the key was there.
func (d *tomlDoc) setNoted(table, key, value string, note func(was string, present bool) string) (was string, present bool) {
	s, ok := d.find(table, key)
	if !ok {
		line := tomlBareKey(key) + " = " + value
		if text := note("", false); text != "" {
			line += "  # " + text
		}
		d.insert(table, []string{line})
		return "", false
	}
	was, comment := d.valueText(s)
	line := d.lines[s.start]
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	replacement := []string{indent + tomlBareKey(key) + " = " + value}
	text := note(was, true)
	start := s.start
	switch {
	case text == "":
		if comment != "" {
			replacement[0] += "  " + comment
		}
	case comment == "" || canaryWrote(comment):
		replacement[0] += "  # " + text
	default:
		replacement[0] += "  " + comment
		above := indent + "# " + key + " " + text
		if start > 0 && strings.HasPrefix(strings.TrimSpace(d.lines[start-1]), "# "+key+" set in Desk ") {
			start--
		}
		replacement = append([]string{above}, replacement...)
	}
	d.lines = append(d.lines[:start], append(replacement, d.lines[s.end:]...)...)
	return was, true
}

// removeNoted replaces table.key's lines with one comment line naming the
// removal, the value it held and the owner's own trailing comment, if any.
func (d *tomlDoc) removeNoted(table, key string, note func(was string) string) bool {
	s, ok := d.find(table, key)
	if !ok {
		return false
	}
	was, comment := d.valueText(s)
	line := d.lines[s.start]
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	text := indent + "# " + key + " " + note(was)
	if comment != "" && !canaryWrote(comment) {
		text += "  " + comment
	}
	d.lines = append(d.lines[:s.start], append([]string{text}, d.lines[s.end:]...)...)
	return true
}

// canaryWrote reports whether a trailing comment is one Canary writes:
// provenance it can replace, never the owner's words.
func canaryWrote(comment string) bool {
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(comment), "#"))
	for _, prefix := range []string{"written by Canary", "added by Canary", "set in Desk", "raised in Desk"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// addTable writes an empty [table] after its family: after the last table
// named for its nearest ancestor that the file has (a.b.c goes after the
// last [a.b] or [a.b.*] table, else the last [a] or [a.*]), else at the end
// of the file. A blank line separates it from its neighbours.
func (d *tomlDoc) addTable(table string) {
	at := len(d.lines)
	for parent := table; ; {
		i := strings.LastIndex(parent, ".")
		if i < 0 {
			break
		}
		parent = parent[:i]
		last := -1
		for j, line := range d.lines {
			if m := tomlHeaderRe.FindStringSubmatch(line); m != nil && (m[1] == parent || strings.HasPrefix(m[1], parent+".")) {
				last = j
			}
		}
		if last < 0 {
			continue
		}
		header := tomlHeaderRe.FindStringSubmatch(d.lines[last])[1]
		at = last + 1
		for _, s := range d.spans() {
			if s.table == header && s.end > at {
				at = s.end
			}
		}
		break
	}
	block := []string{"[" + table + "]"}
	if at > 0 && strings.TrimSpace(d.lines[at-1]) != "" {
		block = append([]string{""}, block...)
	}
	if at < len(d.lines) && strings.TrimSpace(d.lines[at]) != "" {
		block = append(block, "")
	}
	d.lines = append(d.lines[:at], append(block, d.lines[at:]...)...)
}

var tomlBareKeyRe = regexp.MustCompile(`^[A-Za-z0-9_\-]+$`)

// tomlBareKey quotes a key that is not a TOML bare key.
func tomlBareKey(k string) string {
	if tomlBareKeyRe.MatchString(k) {
		return k
	}
	return fmt.Sprintf("%q", k)
}
