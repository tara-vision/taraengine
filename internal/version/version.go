// Package version holds the build version, set by the release pipeline through ldflags.
package version

// Version is "dev" for local builds and the release tag without its "v" for releases.
var Version = "dev"
