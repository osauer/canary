package update

import (
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

// AvailabilityState describes the ordering relationship between the running
// binary and the newest stable release selected for its maintained major.
type AvailabilityState string

// Availability states distinguish a provable update from every fail-closed
// ordering outcome.
const (
	AvailabilityAvailable        AvailabilityState = "available"
	AvailabilityCurrent          AvailabilityState = "current"
	AvailabilityNewerInstalled   AvailabilityState = "newer_installed"
	AvailabilityDevelopmentBuild AvailabilityState = "development_build"
	AvailabilityInvalidVersion   AvailabilityState = "invalid_version"
	AvailabilityDifferentMajor   AvailabilityState = "different_major"
)

// Availability is the typed, side-effect-free update decision shared by the
// CLI and app host. Available is true only when LatestVersion is provably newer
// than a comparable installed version on the same major line.
type Availability struct {
	InstalledVersion string
	LatestVersion    string
	State            AvailabilityState
	Available        bool
}

var (
	gitDescribeAheadVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+-[0-9]+-g[0-9a-fA-F]+(?:-dirty)?$`)
	dirtyBuildVersion       = regexp.MustCompile(`(?i)(?:^|[-+.])dirty(?:$|[-+.])`)
)

// EvaluateAvailability compares one installed build with one stable release.
// git-describe ahead/dirty builds are deliberately incomparable: semver would
// otherwise rank vX.Y.Z-N-gSHA below vX.Y.Z and offer a downgrade.
func EvaluateAvailability(installedVersion, latestVersion string) Availability {
	installed := normalizeReleaseVersion(installedVersion)
	latest := normalizeReleaseVersion(latestVersion)
	result := Availability{InstalledVersion: installed, LatestVersion: latest}

	if IsDevelopmentBuild(installedVersion) {
		result.State = AvailabilityDevelopmentBuild
		return result
	}
	if !semver.IsValid(installed) || !semver.IsValid(latest) {
		result.State = AvailabilityInvalidVersion
		return result
	}
	if semver.Major(installed) != semver.Major(latest) {
		result.State = AvailabilityDifferentMajor
		return result
	}
	switch semver.Compare(latest, installed) {
	case 1:
		result.State = AvailabilityAvailable
		result.Available = true
	case 0:
		result.State = AvailabilityCurrent
	default:
		result.State = AvailabilityNewerInstalled
	}
	return result
}

// IsDevelopmentBuild reports versions whose ordering against a stable release
// cannot be proven from the embedded version string. Exact semver prereleases
// remain comparable; only local/dev identities fail closed here.
func IsDevelopmentBuild(version string) bool {
	trimmed := strings.TrimSpace(version)
	if trimmed == "" || trimmed == "dev" || trimmed == "(devel)" {
		return true
	}
	return dirtyBuildVersion.MatchString(trimmed) || gitDescribeAheadVersion.MatchString(trimmed)
}

func normalizeReleaseVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || version == "dev" || version == "(devel)" {
		return version
	}
	if !strings.HasPrefix(version, "v") {
		return "v" + version
	}
	return version
}
