// Package version carries build metadata stamped in at link time.
//
// Both variables are empty in a build that does not go through the Makefile.
// The getters report "unknown" for an empty value rather than an empty string,
// so a `version` output is never blank.
package version

var (
	Version string
	Commit  string
)

// GetVersion returns the semantic version this binary was built from.
func GetVersion() string {
	if Version == "" {
		return "unknown"
	}
	return Version
}

// GetCommit returns the git commit this binary was built from.
func GetCommit() string {
	if Commit == "" {
		return "unknown"
	}
	return Commit
}
