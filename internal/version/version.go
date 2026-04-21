// Package version is the single source of truth for hippo's release
// identity. Everything that needs to report a version - the CLI,
// the web UI's footer, the MCP initialize handshake - reads from
// here instead of maintaining its own constant.
//
// Release process: bump Version in the same commit as the v*.*.* tag.
// The release workflow overrides Commit via -ldflags at build time so
// prebuilt binaries report the concrete SHA rather than "unknown".
package version

// Version is the semantic-version tag of this build. Bump before
// tagging a release; the tag itself stays the source of truth for
// the published binary.
const Version = "1.0.0-beta"

// Commit is the short SHA of the commit this binary was built from.
// The release workflow overrides it via -ldflags; local `go build`
// leaves the default so callers know they're on an unreleased build.
var Commit = "unknown"
