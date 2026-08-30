package main

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/manifest"
	"github.com/tui-tools/tui-kit/theme"
	tuisystemd "github.com/tui-tools/tui-systemd"
	"github.com/tui-tools/tui-systemd/internal/systemd"
)

// testCompat probes the manifest's real systemd backend against a canned
// `systemctl --version` output, so the header and the capability gate are
// exercised with the block the binary ships rather than a hand-written one.
func testCompat(t *testing.T, versionOutput string) compat.Result {
	t.Helper()
	m, err := manifest.Load(tuisystemd.ManifestJSON)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	backend, ok := m.Backend(backendName)
	if !ok {
		t.Fatalf("the manifest declares no %s backend", backendName)
	}
	return compat.ProbeWith(context.Background(), backend,
		func(context.Context, []string) (string, error) { return versionOutput, nil })
}

// newTestApp builds an app on the demo backend with its first read already
// applied, which is the state the user actually types into.
func newTestApp(t *testing.T) (*app, *systemd.Fake) {
	t.Helper()
	fake := systemd.NewFake()
	a := newApp(fake, theme.FromPalette(theme.TokyoNight()),
		systemd.DefaultJournalLines, testCompat(t, "systemd 257 (257.2-1-arch)"))
	a.width, a.height = 100, 30

	msg := a.Init()()
	if _, ok := msg.(unitsMsg); !ok {
		t.Fatalf("Init produced %T, want unitsMsg", msg)
	}
	a.Update(msg)
	if len(a.visible) == 0 {
		t.Fatal("the demo machine should have units")
	}
	return a, fake
}

// selectUnit narrows the list to one unit and puts the cursor on it, so a
// test says which unit it acts on instead of depending on the sort order.
func selectUnit(t *testing.T, a *app, name string) {
	t.Helper()
	a.filter = name
	a.applyFilter()
	if len(a.visible) == 0 || a.visible[0].Name != name {
		t.Fatalf("%s is not in the demo machine, got %v", name, names(a.visible))
	}
	a.cursor = 0
}

// press sends one key and returns the command it produced.
func press(a *app, key string) tea.Cmd {
	var msg tea.KeyMsg
	switch key {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	_, cmd := a.Update(msg)
	return cmd
}

func TestActionKeyOpensAConfirmWithThePreview(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		wantCommand string
		wantDanger  bool
	}{
		{name: "start", key: "s",
			wantCommand: "sudo -n systemctl start nginx.service"},
		{name: "stop is dangerous", key: "x",
			wantCommand: "sudo -n systemctl stop nginx.service", wantDanger: true},
		{name: "restart is dangerous", key: "r",
			wantCommand: "sudo -n systemctl restart nginx.service", wantDanger: true},
		{name: "enable", key: "e",
			wantCommand: "sudo -n systemctl enable nginx.service"},
		{name: "mask is dangerous", key: "m",
			wantCommand: "sudo -n systemctl mask nginx.service", wantDanger: true},
		{name: "daemon-reload takes no unit", key: "d",
			wantCommand: "sudo -n systemctl daemon-reload"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, fake := newTestApp(t)
			selectUnit(t, a, "nginx.service")

			press(a, tc.key)
			if a.mode != modeConfirm {
				t.Fatalf("mode = %v, want a confirm dialog", a.mode)
			}
			if a.confirm.Command != tc.wantCommand {
				t.Errorf("preview = %q, want %q", a.confirm.Command, tc.wantCommand)
			}
			if a.confirm.Danger != tc.wantDanger {
				t.Errorf("Danger = %v, want %v", a.confirm.Danger, tc.wantDanger)
			}
			if a.confirm.Body == "" {
				t.Error("the dialog should explain what will happen")
			}
			// Nothing may run before the user answers.
			if len(fake.Commands()) != 0 {
				t.Errorf("a command ran before confirmation: %v", fake.Commands())
			}
		})
	}
}

func TestConfirmRunsExactlyThePreviewedCommand(t *testing.T) {
	a, fake := newTestApp(t)
	selectUnit(t, a, "redis.service")
	press(a, "s")
	preview := a.confirm.Command

	cmd := press(a, "y")
	if cmd == nil {
		t.Fatal("confirming should produce a command to run")
	}
	msg := cmd()
	ran, ok := msg.(ranMsg)
	if !ok {
		t.Fatalf("got %T, want ranMsg", msg)
	}
	if ran.err != nil {
		t.Fatalf("run failed: %v", ran.err)
	}

	if len(fake.Commands()) != 1 {
		t.Fatalf("ran %d commands, want exactly 1", len(fake.Commands()))
	}
	// The command that ran must be the one the dialog showed, character for
	// character. This is the guarantee the whole tool is built around.
	if got := fake.Preview(fake.Commands()[0]); got != preview {
		t.Errorf("ran %q, but the dialog promised %q", got, preview)
	}
}

func TestCancelRunsNothing(t *testing.T) {
	a, fake := newTestApp(t)
	press(a, "x")
	if a.mode != modeConfirm {
		t.Fatal("expected a confirm dialog")
	}
	press(a, "n")
	if a.mode != modeUnits {
		t.Errorf("mode = %v, want the unit list", a.mode)
	}
	if len(fake.Commands()) != 0 {
		t.Errorf("cancelling ran %v", fake.Commands())
	}
}

func TestActionWithoutASelection(t *testing.T) {
	a, fake := newTestApp(t)
	// A filter that matches nothing leaves no selection.
	a.filter = "no-such-unit-anywhere"
	a.applyFilter()
	press(a, "s")
	if a.mode == modeConfirm {
		t.Error("an action with no selected unit must not open a dialog")
	}
	if len(fake.Commands()) != 0 {
		t.Errorf("ran %v", fake.Commands())
	}
	if a.status == "" {
		t.Error("the status line should say why nothing happened")
	}
}

func TestStateFilterCycles(t *testing.T) {
	a, _ := newTestApp(t)
	total := len(a.visible)

	press(a, "tab")
	if a.state != systemd.StateFailed {
		t.Fatalf("state = %q, want failed", a.state)
	}
	if len(a.visible) == 0 || len(a.visible) >= total {
		t.Errorf("the failed filter should narrow the list, got %d of %d",
			len(a.visible), total)
	}
	for _, u := range a.visible {
		if !u.Failed() {
			t.Errorf("%s is not failed but passed the filter", u.Name)
		}
	}

	press(a, "tab")
	if a.state != systemd.StateActive {
		t.Errorf("state = %q, want active", a.state)
	}
	press(a, "tab")
	press(a, "tab")
	if a.state != systemd.StateAll {
		t.Errorf("state = %q, want the cycle to return to all", a.state)
	}
	if len(a.visible) != total {
		t.Errorf("visible = %d, want the original %d", len(a.visible), total)
	}
}

func TestTextFilterMatchesDescription(t *testing.T) {
	a, _ := newTestApp(t)
	a.filter = "reverse proxy"
	a.applyFilter()
	if len(a.visible) != 1 || a.visible[0].Name != "nginx.service" {
		t.Errorf("visible = %v, want just nginx.service", names(a.visible))
	}
}

func TestJournalOpensAndFollows(t *testing.T) {
	a, _ := newTestApp(t)
	selectUnit(t, a, "nginx.service")
	cmd := press(a, "j")
	if a.mode != modeJournal {
		t.Fatalf("mode = %v, want the journal", a.mode)
	}
	if a.journalUnit != "nginx.service" {
		t.Errorf("journalUnit = %q", a.journalUnit)
	}
	if cmd == nil {
		t.Fatal("opening the journal should start a read")
	}
	a.Update(cmd())
	if !strings.Contains(a.journalText, "Address already in use") {
		t.Errorf("journal = %q, want the failure reason", a.journalText)
	}

	// Following schedules a tick; leaving must invalidate it, so an in-flight
	// tick cannot restart the loop behind the user's back.
	if press(a, "f") == nil {
		t.Fatal("f should schedule a refresh")
	}
	if !a.following {
		t.Error("f should turn following on")
	}
	stale := tickMsg{epoch: a.followEpoch}
	press(a, "esc")
	if a.mode != modeJournal && a.following {
		t.Error("esc should leave the journal and stop following")
	}
	if _, next := a.Update(stale); next != nil {
		t.Error("a tick from an ended follow session must be ignored")
	}
}

func TestJournalIgnoresALateAnswerForAnotherUnit(t *testing.T) {
	a, _ := newTestApp(t)
	selectUnit(t, a, "nginx.service")
	press(a, "j")
	a.Update(journalMsg{unit: "someone-else.service", text: "stale"})
	if a.journalText == "stale" {
		t.Error("a read for another unit must not land in the panel")
	}
}

func TestTimersAndBootViews(t *testing.T) {
	a, _ := newTestApp(t)

	cmd := press(a, "t")
	if a.mode != modeTimers || cmd == nil {
		t.Fatalf("mode = %v, cmd = %v", a.mode, cmd)
	}
	a.Update(cmd())
	if len(a.timers) == 0 {
		t.Error("the demo should have timers")
	}
	if view := a.View(); !strings.Contains(view, "logrotate.timer") {
		t.Error("the timers view should list the demo timers")
	}
	// A timer that has never fired must read as "never", not as 1970.
	if view := a.View(); !strings.Contains(view, "never") {
		t.Error("a timer that never fired should read as never")
	}

	cmd = press(a, "b")
	if a.mode != modeBoot || cmd == nil {
		t.Fatalf("mode = %v, cmd = %v", a.mode, cmd)
	}
	a.Update(cmd())
	if len(a.blame) == 0 {
		t.Error("the demo should have boot times")
	}
	if view := a.View(); !strings.Contains(view, "fstrim.service") {
		t.Error("the boot view should list the slowest unit first")
	}

	press(a, "esc")
	if a.mode != modeUnits {
		t.Errorf("mode = %v, want to be back on the unit list", a.mode)
	}
}

func TestHelpListsEveryAction(t *testing.T) {
	a, _ := newTestApp(t)
	press(a, "?")
	if a.mode != modeHelp {
		t.Fatalf("mode = %v, want help", a.mode)
	}
	view := a.View()
	for _, spec := range systemd.Actions {
		if !strings.Contains(view, strings.ToLower(spec.Label)) {
			t.Errorf("the help screen is missing %q", spec.Label)
		}
	}
}

func TestFailedUnitsRenderFirst(t *testing.T) {
	a, _ := newTestApp(t)
	if !a.visible[0].Failed() {
		t.Errorf("visible[0] = %+v, want a failed unit", a.visible[0])
	}
	// The view must actually draw without panicking at a realistic size, and
	// at a narrow one where columns are dropped.
	for _, width := range []int{40, 60, 80, 120} {
		a.width = width
		if view := a.View(); view == "" {
			t.Errorf("empty view at width %d", width)
		}
	}
}

func TestRunFailureIsReported(t *testing.T) {
	a, fake := newTestApp(t)
	selectUnit(t, a, "apache2.service")
	press(a, "s")
	cmd := press(a, "y")
	if cmd == nil {
		t.Fatal("confirming should produce a command to run")
	}
	// apache2.service is masked in the demo, so the real systemctl would
	// refuse: the app must surface that instead of reporting success.
	a.Update(cmd())
	if len(fake.Commands()) != 1 {
		t.Fatalf("ran %d commands, want 1", len(fake.Commands()))
	}
	if a.statusKind != 3 { // ui.StatusError
		t.Errorf("statusKind = %v, want an error", a.statusKind)
	}
	if a.busy {
		t.Error("the app should not stay busy after a failure")
	}
}

// names is a readable failure message for a unit slice.
func names(units []systemd.Unit) []string {
	out := make([]string, 0, len(units))
	for _, u := range units {
		out = append(out, u.Name)
	}
	return out
}
