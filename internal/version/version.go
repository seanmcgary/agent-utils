// Package version carries build metadata stamped in at link time.
package version

// Commit is the git commit this binary was built from. The Makefile sets it
// with -ldflags -X. A build that does not go through the Makefile leaves it
// "unknown", which is accurate rather than misleading.
var Commit = "unknown"
