// Package version carries build metadata stamped in at link time.
//
// A Makefile build passes the exact values with -ldflags. A `go install` build
// does not run the Makefile, so the getters fall back to the module and VCS
// information the Go toolchain embeds automatically.
package version

import "runtime/debug"

var (
	Version string
	Commit  string
)

// GetVersion returns the semantic version this binary was built from.
//
// It prefers the linker stamp. Failing that it uses the module version the
// toolchain records, which is what makes `go install <pkg>@v0.1.0` report
// v0.1.0 rather than "unknown".
func GetVersion() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		// "(devel)" is what a build from a working tree reports. It is not a
		// version, so treat it as absent.
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "unknown"
}

// GetCommit returns the git commit this binary was built from.
//
// It prefers the linker stamp, then the VCS revision the toolchain embeds.
// Release binaries are built with -buildvcs=false and carry the stamp instead,
// so the fallback serves `go install` and plain `go build`.
func GetCommit() string {
	if Commit != "" {
		return Commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, modified string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		if rev != "" {
			if len(rev) > 7 {
				rev = rev[:7]
			}
			if modified == "true" {
				// Say so. A commit alone would imply the tree was clean.
				return rev + "-dirty"
			}
			return rev
		}
	}
	return "unknown"
}
