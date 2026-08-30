package main

import (
	"context"
	"strings"
	"testing"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/manifest"
	tuisystemd "github.com/tui-tools/tui-systemd"
)

// The embedded manifest is what the header and the capability gate read, so it
// has to parse and describe the backend the tool actually drives.
func TestEmbeddedManifestDeclaresSystemd(t *testing.T) {
	m, err := manifest.Load(tuisystemd.ManifestJSON)
	if err != nil {
		t.Fatalf("the embedded tool.json does not parse: %v", err)
	}
	if m.Name != toolName {
		t.Errorf("manifest name = %q, want %q", m.Name, toolName)
	}
	backend, ok := m.Backend(backendName)
	if !ok {
		t.Fatal("no systemd backend in the manifest")
	}
	if _, ok := compat.NewCaps("250", backend.Features).Since("timers"); !ok {
		t.Error("the manifest should declare when timers appeared")
	}
}

func TestProbeCompatSkipsDemo(t *testing.T) {
	if got := probeCompat(context.Background(), true); got.Backend != "" {
		t.Errorf("demo probe = %+v, want the zero result", got)
	}
}

// The version reaches the header, with the qualifier that says how much we
// know about it.
func TestHeaderShowsTheSystemdVersion(t *testing.T) {
	a, _ := newTestApp(t)
	if got := a.View(); !strings.Contains(got, "systemd 257") {
		t.Errorf("the header should carry the probed version, got:\n%s", got)
	}

	a.backendCompat = testCompat(t, "systemd 229")
	if got := a.View(); !strings.Contains(got, "below minimum 230") {
		t.Errorf("a version below the minimum should say so, got:\n%s", got)
	}
}

// The capability replaces the version comparison that used to be implied by
// the parse error: on an old systemd the timers view is not offered at all,
// and pressing the key explains why instead of failing on the parse.
func TestTimersAreGatedOnTheSystemdVersion(t *testing.T) {
	a, _ := newTestApp(t)
	if !a.hasTimers() {
		t.Fatal("257 has JSON timers")
	}
	if got := a.View(); !strings.Contains(got, "timers") {
		t.Error("the hint bar should offer timers on a machine that has them")
	}

	a.backendCompat = testCompat(t, "systemd 249")
	if a.hasTimers() {
		t.Error("249 has no JSON timers")
	}
	if got := a.View(); strings.Contains(got, "t timers") {
		t.Errorf("the hint bar should not offer timers on 249, got:\n%s", got)
	}

	if cmd := a.openTimers(); cmd != nil {
		t.Error("opening the timers view on 249 should do nothing")
	}
	if a.mode == modeTimers {
		t.Error("the app should have stayed on the unit list")
	}
	if !strings.Contains(a.status, "systemd 250 or newer") ||
		!strings.Contains(a.status, "249") {
		t.Errorf("status = %q, want the version requirement and the version found",
			a.status)
	}
}

// An unreadable version must not hide a working view.
func TestTimersOnAnUnknownVersion(t *testing.T) {
	a, _ := newTestApp(t)
	a.backendCompat = compat.Result{}
	if !a.hasTimers() {
		t.Error("an unknown version is treated as capable")
	}
}
