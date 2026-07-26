// Package productidentity defines Canary's public executable identity and the
// durable pre-rename identifiers that must remain stable for safety.
//
// The product and executable are Canary/canary. The old executable name is not
// an alias or supported command; it is recognized only while locating and
// quiescing a process left running across an upgrade. Persistent identifiers
// deliberately do not follow the product rename: changing them would create a
// second daemon authority or strand existing configuration, state, browser
// sessions, and launchd supervision.
package productidentity

import "path/filepath"

// Product and persistence identifiers separate Canary's public name from the
// exact on-disk and process identities retained for safety continuity.
const (
	// ProductName is the human-facing product name.
	ProductName = "Canary"

	// Executable is the canonical command and installed binary name.
	Executable = "canary"
	// PreUpgradeExecutable identifies processes started by releases before the
	// breaking rename. It must never be used to build, install, advertise, or
	// select a release asset.
	PreUpgradeExecutable = "ibkr"

	// PersistentNamespace pins all XDG configuration, state, data, cache, and
	// runtime paths to their pre-rename namespace.
	PersistentNamespace = "ibkr"
	// DaemonSocketName and DaemonLockName pin the single daemon authority.
	DaemonSocketName = "ibkr.sock"
	DaemonLockName   = "ibkr.lock"
	// AppLaunchAgentLabel pins the already-installed macOS supervisor.
	AppLaunchAgentLabel = "com.osauer.ibkr-app"
)

// IsManagedProcessExecutableBase reports whether base can belong to the
// currently installed executable or a still-running pre-upgrade process.
// This is solely a quiescence/dual-authority safety predicate. Callers should
// pass filepath.Base(path) when matching process argv.
func IsManagedProcessExecutableBase(base string) bool {
	return base == Executable || base == PreUpgradeExecutable
}

// CanonicalPath returns the canonical executable path under dir.
func CanonicalPath(dir string) string {
	return filepath.Join(dir, Executable)
}
