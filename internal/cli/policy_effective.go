package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/daemon"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The full policy print of `canary policy show --explain` and
// `canary policy show SECTION`: every key that governs behaviour, grouped by
// file and table, in aligned key / value / source columns with the key's
// meaning wrapped beneath it. Values and meanings are the daemon's
// (rpc.PolicyEffectiveView); this file only lays them out.

const (
	policyKeyIndent   = "    "
	policyKeyMaxWidth = 40
	policyValueWidth  = 14
	// policyMeaningIndent sits under the key, so a long meaning keeps most
	// of the line instead of a sliver beside the value column.
	policyMeaningIndent = "      "
)

// policySectionAliases maps the names an owner may type to section ids.
var policySectionAliases = map[string]string{
	"risk": rpc.PolicySectionConstitution, "risk-policy": rpc.PolicySectionConstitution,
	"rules": rpc.PolicySectionRulebook, "settings": rpc.PolicySectionRuntime, "config": rpc.PolicySectionTrading,
}

// policyEffectiveFor returns the daemon's view, or reads the files itself
// when the daemon predates the view.
func policyEffectiveFor(ctx context.Context, env *Env, res *rpc.RiskPolicyResult) *rpc.PolicyEffectiveView {
	if res.Effective != nil {
		return res.Effective
	}
	var settings *rpc.PlatformSettings
	var got rpc.PlatformSettings
	if err := env.Conn.Call(ctx, rpc.MethodSettingsGet, struct{}{}, &got); err == nil {
		settings = &got
	}
	return daemon.PolicyEffectiveFromFiles("", res.Files, res.Limits, settings)
}

// filterPolicyEffective keeps the sections, or the groups within them, that
// name matches: a section id or alias, or a table such as cash_sweep,
// trailing_stop or budget_reduction (its sub-tables included).
func filterPolicyEffective(view *rpc.PolicyEffectiveView, name string) (*rpc.PolicyEffectiveView, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if alias, ok := policySectionAliases[name]; ok {
		name = alias
	}
	out := &rpc.PolicyEffectiveView{Origin: view.Origin}
	for _, sec := range view.Sections {
		if sec.ID == name {
			out.Sections = append(out.Sections, sec)
			continue
		}
		kept := sec
		kept.Groups = nil
		for _, g := range sec.Groups {
			id := strings.ToLower(g.ID)
			if id == name || strings.HasSuffix(id, "."+name) || strings.Contains(id, "."+name+".") || strings.HasPrefix(id, name+".") {
				kept.Groups = append(kept.Groups, g)
			}
		}
		if len(kept.Groups) > 0 {
			out.Sections = append(out.Sections, kept)
		}
	}
	return out, len(out.Sections) > 0
}

// policySectionNames lists what `canary policy show SECTION` accepts.
func policySectionNames(view *rpc.PolicyEffectiveView) string {
	var names []string
	for _, sec := range view.Sections {
		names = append(names, sec.ID)
	}
	for _, extra := range []string{"cash_sweep", "trailing_stop", "budget_reduction", "authority", "regime"} {
		if !slices.Contains(names, extra) {
			names = append(names, extra)
		}
	}
	return strings.Join(names, ", ")
}

// renderPolicyEffective prints every section of the view. overrides marks
// the constitution keys under an active temporary exception.
func renderPolicyEffective(env *Env, view *rpc.PolicyEffectiveView, overrides map[string]rpc.OverrideRecord) {
	if view == nil {
		return
	}
	out := env.Stdout
	if view.Origin == "files" {
		fmt.Fprintln(out)
		riskDisplayLine(env, "", "Read from the policy files: the running daemon predates this view, so a file it has not reloaded may differ from the policy in force. Restart Canary to print the policies in force.", env.yellow)
	}
	for _, sec := range view.Sections {
		// One key and value column per section keeps a file's tables
		// aligned with each other without the widest file's keys pushing
		// every other section's values right.
		keyWidth, valueWidth := 0, 0
		for _, g := range sec.Groups {
			for _, r := range g.Rows {
				keyWidth = max(keyWidth, visibleLen(policyRowKey(g, r)))
				if len(g.Columns) == 0 && visibleLen(r.Value) <= policyValueWidth {
					valueWidth = max(valueWidth, visibleLen(r.Value))
				}
			}
		}
		keyWidth = min(keyWidth, policyKeyMaxWidth)
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.bold(sec.Title)+policySectionMeta(env, sec))
		if sec.Path != "" {
			fmt.Fprintln(out, "  "+env.dim(sanitizeRunText(policyDisplayPath(sec.Path))))
		}
		for _, n := range sec.Notes {
			riskDisplayLine(env, "  ", n, env.dim)
		}
		for _, g := range sec.Groups {
			renderPolicyGroup(env, sec, g, keyWidth, valueWidth, overrides)
		}
	}
}

func policySectionMeta(env *Env, sec rpc.PolicyEffectiveSection) string {
	var parts []string
	if sec.Identity != "" {
		parts = append(parts, sec.Identity)
	}
	if sec.Review == rpc.PolicyReviewUnreviewed {
		parts = append(parts, "default, unreviewed")
	} else if sec.Status != "" {
		parts = append(parts, sec.Status)
	}
	if len(parts) == 0 {
		return ""
	}
	return "  " + env.dim(strings.Join(parts, " · "))
}

// policyDisplayPath shortens the home directory to ~, as the files are
// usually written.
func policyDisplayPath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rest, ok := strings.CutPrefix(path, home+string(os.PathSeparator)); ok {
			return "~/" + rest
		}
	}
	return path
}

func renderPolicyGroup(env *Env, sec rpc.PolicyEffectiveSection, g rpc.PolicyEffectiveGroup, keyWidth, valueWidth int, overrides map[string]rpc.OverrideRecord) {
	out := env.Stdout
	fmt.Fprintln(out, "  "+g.Title)
	for _, n := range g.Notes {
		riskDisplayLine(env, policyKeyIndent, n, env.yellow)
	}
	if len(g.Columns) > 0 {
		renderPolicyColumns(env, g, keyWidth)
		return
	}
	for _, r := range g.Rows {
		value := r.Value
		if visibleLen(value) <= policyValueWidth {
			value += strings.Repeat(" ", max(0, valueWidth-visibleLen(value)))
		}
		renderPolicyRow(env, policyRowKey(g, r), keyWidth, value, policySourceText(env, r), r.Meaning)
		if o, ok := overrides[r.Key]; ok && sec.ID == rpc.PolicySectionConstitution {
			riskDisplayLine(env, policyMeaningIndent, fmt.Sprintf("override until %s: %s", o.ExpiresAt.Local().Format("2006-01-02 15:04"), o.Reason), env.yellow)
		}
	}
}

// renderPolicyColumns prints a regime-conditional group: one value per set.
func renderPolicyColumns(env *Env, g rpc.PolicyEffectiveGroup, keyWidth int) {
	widths := make([]int, len(g.Columns))
	for i, c := range g.Columns {
		widths[i] = visibleLen(strings.ReplaceAll(c, "_", " "))
		for _, r := range g.Rows {
			if i < len(r.Values) {
				widths[i] = max(widths[i], visibleLen(r.Values[i]))
			}
		}
	}
	cells := func(values []string) string {
		var b strings.Builder
		for i, w := range widths {
			v := ""
			if i < len(values) {
				v = values[i]
			}
			b.WriteString(v + strings.Repeat(" ", max(0, w-visibleLen(v))) + "  ")
		}
		return b.String()
	}
	headers := make([]string, len(g.Columns))
	for i, c := range g.Columns {
		headers[i] = strings.ReplaceAll(c, "_", " ")
	}
	fmt.Fprintln(env.Stdout, policyKeyIndent+strings.Repeat(" ", keyWidth+2)+env.dim(strings.TrimRight(cells(headers), " ")))
	for _, r := range g.Rows {
		renderPolicyRow(env, policyRowKey(g, r), keyWidth, strings.TrimSuffix(cells(r.Values), "  "), policySourceText(env, r), r.Meaning)
	}
}

// renderPolicyRow prints the key, the value cell and the source tag on one
// line when they fit. A long value wraps beside the key column and the tag
// follows its last line; the meaning wraps beneath, under the key. Text is
// sanitised and wrapped before any colour is applied, so wrapping never
// splits an escape sequence.
func renderPolicyRow(env *Env, key string, keyWidth int, value, tag, meaning string) {
	out := env.Stdout
	prefix := policyKeyIndent + key + strings.Repeat(" ", max(0, keyWidth-visibleLen(key))) + "  "
	indent := strings.Repeat(" ", visibleLen(prefix))
	width := max(1, briefProseWidth(out)-visibleLen(prefix))
	value = sanitizeRunText(value)
	lines := []string{value}
	if visibleLen(value) > width {
		lines = wrapVisibleText(value, width)
	}
	for i, line := range lines {
		lead := indent
		if i == 0 {
			lead = prefix
		}
		if i == len(lines)-1 {
			if visibleLen(line)+2+visibleLen(tag) <= width || tag == "" {
				fmt.Fprintln(out, strings.TrimRight(lead+line+"  "+tag, " "))
				break
			}
			fmt.Fprintln(out, strings.TrimRight(lead+line, " "))
			fmt.Fprintln(out, indent+tag)
			break
		}
		fmt.Fprintln(out, lead+line)
	}
	if meaning != "" {
		riskDisplayLine(env, policyMeaningIndent, meaning, env.dim)
	}
}

// policyRowKey is the key as written inside its table.
func policyRowKey(g rpc.PolicyEffectiveGroup, r rpc.PolicyEffectiveRow) string {
	if g.ID != "" {
		if k, ok := strings.CutPrefix(r.Key, g.ID+"."); ok {
			return k
		}
	}
	if len(g.Columns) > 0 {
		return r.Key[strings.LastIndex(r.Key, ".")+1:]
	}
	return r.Key
}

// policySourceText says where a value comes from, coloured only when the
// owner has something to do.
func policySourceText(env *Env, r rpc.PolicyEffectiveRow) string {
	var s string
	switch r.Source {
	case rpc.PolicySourceFile:
		s = "file"
	case rpc.PolicySourceDefault, rpc.PolicySourceMachine:
		s = env.dim(r.Source)
	case rpc.PolicySourceNeedsYourNumber:
		s = env.yellow("needs your number")
	case rpc.PolicySourceNotWritten:
		s = env.yellow("not written")
	case rpc.PolicySourceRetired:
		s = env.dim("retired")
	case rpc.PolicySourceUnapproved:
		s = env.yellow("unapproved")
	case rpc.PolicySourceRuntime:
		// The settings view reports every runtime-owned key as runtime; only
		// a value that replaces config.toml's is an override to notice.
		s = "runtime"
		if r.FileValue != "" {
			s = env.yellow("runtime override") + " " + env.dim("(config.toml "+r.FileValue+")")
		}
	default:
		s = env.dim(strings.ReplaceAll(r.Source, "_", " "))
	}
	if r.Enforcement != "" {
		s += " " + env.dim("· "+r.Enforcement)
	}
	return s
}
