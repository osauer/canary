package daemon

import "runtime/debug"

// buildRevision names the commit this binary was built from, short form, with
// "+modified" for a dirty tree; "unknown" when the build carries no VCS stamp.
func buildRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	revision, modified := "", false
	for _, setting := range bi.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return "unknown"
	}
	revision = revision[:min(len(revision), 8)]
	if modified {
		revision += "+modified"
	}
	return revision
}
