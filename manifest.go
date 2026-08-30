// Package tuisystemd exists for one reason: to embed the repository's
// tool.json into the binary.
//
// The manifest is the family's single source of truth about a tool — the
// website reads it, the README is rendered from it, and since it also carries
// the `backends[]` block, the running binary reads it too, to probe the
// version of systemd it is about to drive and to decide which views that
// version supports. go:embed cannot reach outside its own package directory,
// so the embedding package is the module root.
package tuisystemd

import _ "embed"

// ManifestJSON is the repository's tool.json. Pass it to
// tui-kit/manifest.Load.
//
//go:embed tool.json
var ManifestJSON []byte
