package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// A broken config.toml stops the daemon only when the account-identity pins
// cannot be read (owner decision 2026-09-26). Everything else that cannot be
// read runs on Canary's default for that part and is reported, so a typo in
// an unrelated section never blocks exits, trims or reads.

// identityPins are the keys that decide which broker account Canary acts on:
// the gateway endpoint, account, client and transport pins, and the
// paper/live pin.
var identityPins = map[string][]string{
	"gateway": {"host", "port", "account", "client_id", "tls"},
	"trading": {"mode"},
}

// pinNames are the leaf names of the identity pins. One outside its table is
// a pin Canary would not read, so it stops the daemon rather than letting
// discovery pick an account on its own.
var pinNames = []string{"host", "port", "account", "client_id", "tls", "mode"}

// Issue is one part of config.toml the daemon could not read. The daemon
// starts on Canary's default for that part and reports the issue.
type Issue struct {
	// Key names the setting ("flex.enabled"), a section ("[flex]"), or
	// "config.toml" for the file as a whole.
	Key string `json:"key"`
	// Problem says what was wrong and what is in force instead.
	Problem string `json:"problem"`
}

// String renders the issue as "key: problem".
func (i Issue) String() string { return i.Key + ": " + i.Problem }

// Section returns the top-level table the issue sits in ("flex" for
// "flex.enabled" or "[flex]"), or "" for the file as a whole.
func (i Issue) Section() string {
	if i.Key == "config.toml" {
		return ""
	}
	name, _, _ := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(i.Key, "["), "]"), ".")
	return strings.Trim(strings.TrimSpace(name), `"`)
}

// IdentityError reports account-identity pins the daemon cannot read. It is
// the only config failure that stops the daemon.
type IdentityError struct {
	Path   string
	Detail string
}

// Error names the unreadable pin and points to the troubleshooting entry.
func (e *IdentityError) Error() string {
	return fmt.Sprintf("config %s: %s. The [gateway] pins and [trading].mode decide which broker account Canary acts on, so the daemon does not start until they read; see \"The daemon stops at start with a config error\" in docs/docs/start/troubleshooting.md", e.Path, e.Detail)
}

// LoadForDaemon reads config.toml the way the daemon starts from it. A
// missing file is fully automatic, as with Load. It returns an
// *IdentityError, and nothing else, when the file cannot be read or the
// account-identity pins cannot be read from it: the file cannot be parsed
// well enough to read [gateway] and [trading] on their own, a pin has the
// wrong type, [gateway] or [trading] carries a key Canary does not know (a
// misspelled pin looks exactly like that), or a pin name sits outside its
// table. Every other part that cannot be read is left at Canary's default
// and named in the returned issues.
func LoadForDaemon(path string) (*Config, []Issue, error) {
	if path == "" {
		path = DefaultPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil, nil
		}
		return nil, nil, &IdentityError{Path: path, Detail: "the file cannot be read (" + err.Error() + ")"}
	}
	// A file the strict loader accepts is read exactly as before.
	strict := &Config{}
	if md, err := toml.Decode(string(data), strict); err == nil && len(md.Undecoded()) == 0 {
		return strict, strict.Daemon.sanitizeLogging(), nil
	}
	var root map[string]toml.Primitive
	md, err := toml.Decode(string(data), &root)
	if err != nil {
		return salvageForDaemon(path, string(data), err)
	}
	return decodeForDaemon(path, md, root)
}

// decodeForDaemon reads a parsed document key by key, so one unreadable
// value falls back to its default without taking its section down with it.
func decodeForDaemon(path string, md toml.MetaData, root map[string]toml.Primitive) (*Config, []Issue, error) {
	cfg := &Config{}
	sections := map[string]reflect.Value{
		"gateway": reflect.ValueOf(&cfg.Gateway).Elem(), "daemon": reflect.ValueOf(&cfg.Daemon).Elem(),
		"trading": reflect.ValueOf(&cfg.Trading).Elem(), "rulebook": reflect.ValueOf(&cfg.Rulebook).Elem(),
		"auto_trade": reflect.ValueOf(&cfg.AutoTrade).Elem(), "opportunities": reflect.ValueOf(&cfg.Opportunities).Elem(),
		"flex": reflect.ValueOf(&cfg.Flex).Elem(), "spx": reflect.ValueOf(&cfg.SPX).Elem(),
	}
	fail := func(format string, args ...any) (*Config, []Issue, error) {
		return nil, nil, &IdentityError{Path: path, Detail: fmt.Sprintf(format, args...)}
	}
	var issues []Issue
	for _, name := range orderedKeys(md, nil, root) {
		target, known := sections[name]
		_, identity := identityPins[name]
		if !known {
			if pin := pinUnder(md, toml.Key{name}); pin != "" {
				return fail("%q is not a section Canary reads, yet it sets %s, which Canary reads only from [gateway] or [trading]", name, pin)
			}
			issues = append(issues, Issue{Key: name, Problem: "not a setting Canary knows; ignored"})
			continue
		}
		// A table defined only through subtables or dotted keys has no key of
		// its own and reports no type; an array of tables or a value is not a
		// table.
		var table map[string]toml.Primitive
		if kind := md.Type(name); (kind != "Hash" && kind != "") || md.PrimitiveDecode(root[name], &table) != nil {
			if identity {
				return fail("[%s] is not a table Canary can read", name)
			}
			issues = append(issues, Issue{Key: "[" + name + "]", Problem: "not a table Canary can read; Canary's defaults are in force for the whole section"})
			continue
		}
		fields := tomlFields(target)
		for _, key := range orderedKeys(md, toml.Key{name}, table) {
			full := name + "." + key
			field, ok := fields[key]
			if !ok {
				if msg, removed := removedKeys[full]; removed {
					if identity {
						return fail("%s was removed: %s", full, msg)
					}
					issues = append(issues, Issue{Key: full, Problem: "was removed (" + msg + "); ignored"})
					continue
				}
				if identity {
					return fail("[%s] carries %s, a key Canary does not know, which may be a misspelled pin", name, key)
				}
				if pin := pinUnder(md, toml.Key{name, key}); pin != "" || slices.Contains(pinNames, key) {
					return fail("%s sets a pin Canary reads only from [gateway] or [trading]", full)
				}
				issues = append(issues, Issue{Key: full, Problem: "not a setting Canary knows; ignored"})
				continue
			}
			if err := md.PrimitiveDecode(table[key], field.Addr().Interface()); err != nil {
				field.Set(reflect.Zero(field.Type()))
				if slices.Contains(identityPins[name], key) {
					return fail("%s cannot be read (%s)", full, tomlErrorText(err))
				}
				issues = append(issues, Issue{Key: full, Problem: "cannot be read (" + tomlErrorText(err) + "); Canary's default is in force"})
			}
		}
	}
	return cfg, append(issues, cfg.Daemon.sanitizeLogging()...), nil
}

// orderedKeys lists a decoded table's keys in document order. An implicit
// table (one defined only through its subtables or dotted keys) has no key of
// its own, so the order comes from the first key under each name.
func orderedKeys(md toml.MetaData, prefix toml.Key, m map[string]toml.Primitive) []string {
	out := make([]string, 0, len(m))
	for _, k := range md.Keys() {
		if len(k) <= len(prefix) || !slices.Equal(k[:len(prefix)], prefix) {
			continue
		}
		if name := k[len(prefix)]; !slices.Contains(out, name) {
			if _, ok := m[name]; ok {
				out = append(out, name)
			}
		}
	}
	var rest []string
	for name := range m {
		if !slices.Contains(out, name) {
			rest = append(rest, name)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}

// pinUnder returns the first pin name set at or below prefix.
func pinUnder(md toml.MetaData, prefix toml.Key) string {
	for _, k := range md.Keys() {
		if len(k) >= len(prefix) && slices.Equal(k[:len(prefix)], prefix) && slices.Contains(pinNames, k[len(k)-1]) {
			return k.String()
		}
	}
	return ""
}

// tomlFields maps a config struct's toml tags to its settable fields.
func tomlFields(v reflect.Value) map[string]reflect.Value {
	out := map[string]reflect.Value{}
	for i := range v.NumField() {
		tag, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("toml"), ",")
		if tag != "" && tag != "-" {
			out[tag] = v.Field(i)
		}
	}
	return out
}

// tomlErrorText drops the decoder's "toml: " prefix.
func tomlErrorText(err error) string {
	return strings.TrimPrefix(err.Error(), "toml: ")
}

// sanitizeLogging resets each [daemon] logging value the daemon cannot use
// to Canary's default and names it, instead of refusing the whole file.
func (d *Daemon) sanitizeLogging() []Issue {
	checks := []struct {
		key   string
		alone Daemon
		reset func()
	}{
		{"daemon.log_calendar_mode", Daemon{LogCalendarMode: d.LogCalendarMode}, func() { d.LogCalendarMode = "" }},
		{"daemon.log_markets", Daemon{LogMarkets: d.LogMarkets}, func() { d.LogMarkets = nil }},
		{"daemon.log_before_open_minutes", Daemon{LogBeforeOpenMinutes: d.LogBeforeOpenMinutes}, func() { d.LogBeforeOpenMinutes = nil }},
		{"daemon.log_after_close_minutes", Daemon{LogAfterCloseMinutes: d.LogAfterCloseMinutes}, func() { d.LogAfterCloseMinutes = nil }},
	}
	var out []Issue
	for _, c := range checks {
		if err := c.alone.validateLogging(); err != nil {
			c.reset()
			out = append(out, Issue{Key: c.key, Problem: err.Error() + "; Canary's default is in force"})
		}
	}
	return out
}

// tableHeader matches a TOML table or array-of-tables header line.
var tableHeader = regexp.MustCompile(`^\s*\[\[?\s*([A-Za-z0-9_\-"' .]+?)\s*\]\]?\s*(#.*)?$`)

// tomlChunk is one table's lines: the root lines before the first header,
// or a header and the lines under it.
type tomlChunk struct {
	table     string // dotted table name; "" for the root
	startLine int    // 1-based line of the chunk's first line
	text      string
}

// splitTOMLTables cuts a document at its table headers. A header inside a
// multi-line string is content, not a cut; a string that never closes keeps
// every later line in its chunk.
func splitTOMLTables(doc string) []tomlChunk {
	chunks := []tomlChunk{{startLine: 1}}
	var b strings.Builder
	inBasic, inLiteral := false, false
	for i, line := range strings.SplitAfter(doc, "\n") {
		if !inBasic && !inLiteral {
			if m := tableHeader.FindStringSubmatch(strings.TrimRight(line, "\r\n")); m != nil {
				chunks[len(chunks)-1].text = b.String()
				b.Reset()
				chunks = append(chunks, tomlChunk{table: strings.ReplaceAll(m[1], " ", ""), startLine: i + 1})
			}
		}
		b.WriteString(line)
		if !inLiteral && strings.Count(line, `"""`)%2 == 1 {
			inBasic = !inBasic
		}
		if !inBasic && strings.Count(line, `'''`)%2 == 1 {
			inLiteral = !inLiteral
		}
	}
	chunks[len(chunks)-1].text = b.String()
	return chunks
}

// identityHint matches a line that opens [gateway] or [trading] or assigns
// a pin name.
var identityHint = regexp.MustCompile(`(?m)^\s*(\[\[?\s*"?(gateway|trading)"?\s*[\].]|"?(host|port|account|client_id|tls|mode)"?\s*=)`)

// salvageForDaemon reads a document that does not parse as a whole, one
// table at a time. It succeeds only when the root and every [gateway] and
// [trading] part parse on their own and no unparseable part could hold a
// pin; each unparseable section is then reported and left at its defaults.
func salvageForDaemon(path, doc string, parseErr error) (*Config, []Issue, error) {
	fail := func(format string, args ...any) (*Config, []Issue, error) {
		return nil, nil, &IdentityError{Path: path, Detail: fmt.Sprintf(format, args...)}
	}
	var issues []Issue
	var readable strings.Builder
	identityTables := map[string]int{}
	for _, c := range splitTOMLTables(doc) {
		top, _, _ := strings.Cut(c.table, ".")
		_, identity := identityPins[strings.Trim(top, `"`)]
		if identity {
			identityTables[c.table]++
		}
		_, err := toml.Decode(c.text, &map[string]any{})
		if err == nil {
			readable.WriteString(c.text)
			if !strings.HasSuffix(c.text, "\n") {
				readable.WriteString("\n")
			}
			continue
		}
		line, msg := c.startLine, tomlErrorText(err)
		if perr, ok := errors.AsType[toml.ParseError](err); ok {
			line, msg = c.startLine+perr.Position.Line-1, perr.Message
		}
		switch {
		case c.table == "":
			return fail("the top of the file cannot be parsed (line %d: %s), and it can set pins", line, msg)
		case identity:
			return fail("[%s] cannot be parsed (line %d: %s)", c.table, line, msg)
		case identityHint.MatchString(c.text):
			return fail("[%s] cannot be parsed (line %d: %s), and the unparseable lines hold [gateway], [trading] or a pin", c.table, line, msg)
		}
		issues = append(issues, Issue{Key: "[" + c.table + "]", Problem: fmt.Sprintf("TOML syntax error at line %d (%s); the lines under this header are ignored, so Canary's defaults are in force for them", line, msg)})
	}
	for table, n := range identityTables {
		if n > 1 {
			return fail("[%s] appears %d times", table, n)
		}
	}
	var root map[string]toml.Primitive
	md, err := toml.Decode(readable.String(), &root)
	if err != nil {
		return fail("the file cannot be parsed (%s), and its readable parts do not parse together either (%s)", tomlErrorText(parseErr), tomlErrorText(err))
	}
	cfg, more, err := decodeForDaemon(path, md, root)
	if err != nil {
		return nil, nil, err
	}
	return cfg, append(issues, more...), nil
}
