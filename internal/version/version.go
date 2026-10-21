// Package version holds build-time version information.
package version

// Version is the release version. It is overridden at build time with
// -ldflags "-X github.com/cstripathy/forgeline/internal/version.Version=...".
var Version = "0.0.0-dev"

// String returns the version string shown by the command line.
func String() string {
	return "forgeline " + Version
}
