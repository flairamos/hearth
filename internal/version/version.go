// Package version holds the shared release version of both binaries.
package version

// Version is the release version. Source of truth is the git tag: release
// builds inject it via -ldflags "-X hearth/internal/version.Version=...".
// The value below is the fallback for plain `go build` dev binaries.
var Version = "0.4.2"
