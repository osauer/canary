package rpc

// PolicyEffectiveView is everything that governs Canary's behaviour, as
// `canary policy show --explain` prints it: the risk constitution, the
// Rulebook, protection and opportunity policy files, the [trading] gates of
// config.toml and the runtime settings, each key with its value in force,
// where that value comes from and what it means. It is read-only; none of
// its rows is authority to place an order.
//
// Stable JSON field names (documented in docs/docs/understand/policy.md):
// sections[].id is one of PolicySection*; groups[].id is the dotted TOML or
// settings table the rows belong to ("" for a file's top-level keys);
// rows[].key is the full dotted path.
type PolicyEffectiveView struct {
	Sections []PolicyEffectiveSection `json:"sections"`
	// Origin says who assembled the view: "daemon" (the policies in force)
	// or "files" (the CLI read the files because the daemon predates this
	// view; runtime values still come from the daemon).
	Origin string `json:"origin"`
}

// Section identifiers, in print order.
const (
	PolicySectionConstitution = "constitution"
	PolicySectionRulebook     = "rulebook"
	PolicySectionProtection   = "protection"
	PolicySectionOpportunity  = "opportunity"
	PolicySectionTrading      = "trading"
	PolicySectionRuntime      = "runtime"
)

// Row sources.
const (
	// PolicySourceFile: the policy file (or config.toml) sets the key.
	PolicySourceFile = "file"
	// PolicySourceDefault: the key is absent and Canary's default applies.
	PolicySourceDefault = "default"
	// PolicySourceMachine: Canary maintains the value (for example a bill
	// settlement route) until the file sets one.
	PolicySourceMachine = "machine"
	// PolicySourceNeedsYourNumber: the feature behind the key stays off or
	// holds until the owner writes a number or decision.
	PolicySourceNeedsYourNumber = "needs_your_number"
	// PolicySourceUnapproved: a constitution key the owner has not chosen.
	PolicySourceUnapproved = "unapproved"
	// PolicySourceRuntime: a runtime override from `canary settings set`.
	PolicySourceRuntime = "runtime"
)

// PolicyEffectiveSection is one file or settings surface.
type PolicyEffectiveSection struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Path is the file the section reads ("" for runtime settings).
	Path string `json:"path,omitempty"`
	// Identity is the policy id and version, when the file carries one.
	Identity string `json:"identity,omitempty"`
	// Status is the file manager's status (active, default, drift, error).
	Status string `json:"status,omitempty"`
	// Review is PolicyReviewUnreviewed while the file is Canary's template.
	Review string                 `json:"review,omitempty"`
	Notes  []string               `json:"notes,omitempty"`
	Groups []PolicyEffectiveGroup `json:"groups"`
}

// PolicyEffectiveGroup is one table of a section.
type PolicyEffectiveGroup struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Columns names the per-column values of a regime-conditional group
	// (calm, early_warning, confirmed); rows then carry Values.
	Columns []string             `json:"columns,omitempty"`
	Notes   []string             `json:"notes,omitempty"`
	Rows    []PolicyEffectiveRow `json:"rows"`
}

// PolicyEffectiveRow is one key in force.
type PolicyEffectiveRow struct {
	// Key is the full dotted path in its file or settings tree.
	Key string `json:"key"`
	// Value is the value in force, rendered with its unit ("8%", "21 days").
	Value string `json:"value"`
	// Values holds one rendered value per group column, when the group has columns.
	Values []string `json:"values,omitempty"`
	// Source is one of PolicySource*.
	Source string `json:"source"`
	// FileValue is the config.toml value an active runtime override replaces.
	FileValue string `json:"file_value,omitempty"`
	// Enforcement is the constitution's class: advisory, shadow or structural.
	Enforcement string `json:"enforcement,omitempty"`
	// Meaning is the plain-English description shared with the generated
	// configuration reference.
	Meaning string `json:"meaning,omitempty"`
}
