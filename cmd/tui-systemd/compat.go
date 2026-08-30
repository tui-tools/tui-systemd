package main

import (
	"context"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/manifest"
	tuisystemd "github.com/tui-tools/tui-systemd"
)

// backendName is the name the manifest gives the backend this tool drives.
const backendName = "systemd"

// probeCompat reads the version of systemd the tool is about to drive.
//
// The facts it is judged against — the minimum version, the versions the lab
// has run against, which feature appeared when — come from the repository's
// own tool.json, embedded in the binary. That is what lets the app ask
// caps.Has("timers") instead of comparing version numbers in the update loop.
//
// It never fails: a manifest that cannot be parsed and a missing systemctl
// both produce a Result the header renders as "version unknown", and an
// unknown version is treated as capable, so nothing is hidden over a version
// string nobody could read.
func probeCompat(ctx context.Context, demo bool) compat.Result {
	// --demo drives an in-memory machine; probing the host's systemd would
	// report a version that has nothing to do with what is on screen.
	if demo {
		return compat.Result{}
	}
	m, err := manifest.Load(tuisystemd.ManifestJSON)
	if err != nil {
		return compat.Result{}
	}
	backend, ok := m.Backend(backendName)
	if !ok {
		return compat.Result{}
	}
	return compat.Probe(ctx, backend)
}
