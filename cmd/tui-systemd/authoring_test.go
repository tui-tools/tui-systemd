package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-systemd/internal/systemd"
)

// deliver runs the command a key produced and feeds its message back, which is
// what the Bubble Tea runtime does between a key press and the screen the user
// sees next.
func deliver(t *testing.T, a *app, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a background read")
	}
	msg := cmd()
	a.Update(msg)
	return msg
}

// typeText sends a string to the form one rune at a time.
func typeText(a *app, text string) {
	for _, r := range text {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestCatShowsTheUnitFile(t *testing.T) {
	a, fake := newTestApp(t)
	selectUnit(t, a, "nginx.service")

	deliver(t, a, press(a, "c"))
	if a.mode != modeCat {
		t.Fatalf("mode = %v, want the unit-file viewer", a.mode)
	}
	if !strings.Contains(a.catText, "# /") ||
		!strings.Contains(a.catText, "Description=") {
		t.Errorf("the viewer shows:\n%s", a.catText)
	}
	// Reading a unit file is a read: nothing may run for it.
	if len(fake.Commands()) != 0 {
		t.Errorf("looking at a file ran %v", fake.Commands())
	}
	if view := a.View(); !strings.Contains(view, "nginx.service") {
		t.Errorf("the screen does not name the unit:\n%s", view)
	}
	press(a, "esc")
	if a.mode != modeUnits {
		t.Errorf("esc left the viewer in %v", a.mode)
	}
}

func TestEditorSeedsItselfFromTheDropInOnDisk(t *testing.T) {
	a, _ := newTestApp(t)
	selectUnit(t, a, "nginx.service")

	deliver(t, a, press(a, "E"))
	if a.mode != modeForm {
		t.Fatalf("mode = %v, want the editor", a.mode)
	}
	// The sample machine already carries a drop-in for nginx, and the form
	// opens on what it says rather than on nothing.
	if got := a.form.value("Restart"); got != "on-failure" {
		t.Errorf("Restart opened on %q, want on-failure", got)
	}
	if got := a.form.value("RestartSec"); got != "5s" {
		t.Errorf("RestartSec opened on %q, want 5s", got)
	}
	// A timer's properties have no business on a service's form.
	if a.form.value("OnCalendar") != "" {
		t.Error("a service form must not offer OnCalendar")
	}
}

func TestEditorRefusesAMaskedUnit(t *testing.T) {
	a, fake := newTestApp(t)
	selectUnit(t, a, "apache2.service")

	if cmd := press(a, "E"); cmd != nil {
		t.Fatal("a masked unit must not even be read for the editor")
	}
	if a.mode == modeForm {
		t.Error("the editor opened on a masked unit")
	}
	if !strings.Contains(a.status, "masked") {
		t.Errorf("the status line says %q", a.status)
	}
	if len(fake.Commands()) != 0 {
		t.Errorf("ran %v", fake.Commands())
	}
}

func TestEditorPlansStageVerifyInstallReload(t *testing.T) {
	a, fake := newTestApp(t)
	selectUnit(t, a, "nginx.service")
	deliver(t, a, press(a, "E"))

	// Change Restart to the next value systemd offers, and ask for the restart.
	press(a, "right")
	restart := a.form.value("Restart")
	if restart == "on-failure" {
		t.Fatal("the choice field did not move")
	}
	a.form.set(fieldRestart, answerYes)
	press(a, "enter")

	if a.mode != modeConfirm {
		t.Fatalf("mode = %v, want a confirm dialog", a.mode)
	}
	preview := a.confirm.Command
	for _, want := range []string{
		"sudo -n install -d -m 0755 /etc/systemd/system/nginx.service.d",
		"/etc/systemd/system/nginx.service.d/90-tui-systemd.conf",
		"sudo -n systemctl daemon-reload",
	} {
		if !strings.Contains(preview, want) {
			t.Errorf("the preview does not contain %q:\n%s", want, preview)
		}
	}
	// The dialog shows what changes, and says the file was not checked —
	// which in the demo is the truth.
	if !strings.Contains(a.confirm.Body, "-Restart=on-failure") ||
		!strings.Contains(a.confirm.Body, "+Restart="+restart) {
		t.Errorf("the dialog does not show the diff:\n%s", a.confirm.Body)
	}
	if len(fake.Commands()) != 0 {
		t.Errorf("a command ran before confirmation: %v", fake.Commands())
	}

	// Confirming runs exactly those commands, in that order.
	msg := deliver(t, a, press(a, "y"))
	if ran, ok := msg.(ranMsg); !ok || ran.err != nil {
		t.Fatalf("the plan failed: %+v", msg)
	}
	got := make([]string, 0, len(fake.Commands()))
	for _, cmd := range fake.Commands() {
		got = append(got, fake.Preview(cmd))
	}
	if strings.Join(got, "\n$ ") != preview {
		t.Errorf("ran\n%s\nbut the dialog promised\n%s",
			strings.Join(got, "\n$ "), preview)
	}

	// The restart is a second question, asked only once the file is written.
	if a.mode != modeConfirm {
		t.Fatalf("mode = %v, want the restart dialog", a.mode)
	}
	if a.confirm.Command != "sudo -n systemctl restart nginx.service" {
		t.Errorf("the second dialog offers %q", a.confirm.Command)
	}
}

func TestNewUnitFormRefusesANameThatExists(t *testing.T) {
	a, fake := newTestApp(t)
	press(a, "n")
	if a.mode != modeForm {
		t.Fatalf("mode = %v, want the create form", a.mode)
	}
	a.form.move(1) // Kind is first; Name is next.
	typeText(a, "nginx")
	a.form.set(fieldKind, string(systemd.KindService))
	for i := range a.form.fields {
		if a.form.fields[i].name == fieldExecStart {
			a.form.fields[i].input.SetValue("/usr/sbin/nginx")
		}
	}

	press(a, "enter")
	if a.mode == modeConfirm {
		t.Fatal("the form offered to overwrite an existing unit")
	}
	if !strings.Contains(a.status, "already exists") {
		t.Errorf("the status line says %q", a.status)
	}
	if len(fake.Commands()) != 0 {
		t.Errorf("ran %v", fake.Commands())
	}
}

func TestNewTimerWritesBothFilesAndOffersEnable(t *testing.T) {
	a, fake := newTestApp(t)
	// A tall terminal, so the dialog shows both files rather than trimming
	// the second one away.
	a.height = 80
	press(a, "n")

	a.form.set(fieldKind, string(systemd.KindTimer))
	a.form.syncKind()
	set := func(name, value string) {
		t.Helper()
		for i := range a.form.fields {
			if a.form.fields[i].name == name {
				a.form.fields[i].input.SetValue(value)
				return
			}
		}
		t.Fatalf("the form has no %s field", name)
	}
	set(fieldName, "nightly-report")
	set(fieldDescription, "Nightly report")
	set(fieldExecStart, "/usr/bin/env true")
	set("OnCalendar", "*-*-* 02:30:00")
	a.form.set(fieldEnableNow, answerYes)

	press(a, "enter")
	if a.mode != modeConfirm {
		t.Fatalf("mode = %v, want a confirm dialog: %s", a.mode, a.status)
	}
	for _, want := range []string{
		"/etc/systemd/system/nightly-report.service",
		"/etc/systemd/system/nightly-report.timer",
		"sudo -n systemctl daemon-reload",
	} {
		if !strings.Contains(a.confirm.Command, want) {
			t.Errorf("the preview does not contain %q:\n%s", want, a.confirm.Command)
		}
	}
	// Both files are shown before either is written.
	if !strings.Contains(a.confirm.Body, "OnCalendar=*-*-* 02:30:00") {
		t.Errorf("the dialog does not show the timer:\n%s", a.confirm.Body)
	}

	msg := deliver(t, a, press(a, "y"))
	if ran, ok := msg.(ranMsg); !ok || ran.err != nil {
		t.Fatalf("the plan failed: %+v", msg)
	}
	if len(fake.Commands()) != 3 {
		t.Fatalf("ran %d commands, want 3: %v", len(fake.Commands()), fake.Commands())
	}
	// Enabling is a second question, and it is asked about the timer rather
	// than about the service the timer starts.
	if a.confirm.Command != "sudo -n systemctl enable --now nightly-report.timer" {
		t.Errorf("the second dialog offers %q", a.confirm.Command)
	}
	deliver(t, a, press(a, "y"))

	units, err := fake.Units(t.Context())
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	for _, name := range []string{"nightly-report.service", "nightly-report.timer"} {
		found := false
		for _, u := range units {
			found = found || u.Name == name
		}
		if !found {
			t.Errorf("%s is not on the machine after the plan ran", name)
		}
	}
}

func TestFormEscapeWritesNothing(t *testing.T) {
	a, fake := newTestApp(t)
	press(a, "n")
	typeText(a, "whatever")
	press(a, "esc")
	if a.mode != modeUnits {
		t.Errorf("mode = %v, want the unit list", a.mode)
	}
	if len(fake.Commands()) != 0 {
		t.Errorf("ran %v", fake.Commands())
	}
}

func TestHelpMentionsTheAuthoringKeys(t *testing.T) {
	a, _ := newTestApp(t)
	press(a, "?")
	view := a.View()
	for _, key := range []string{"c", "E", "n"} {
		if !strings.Contains(view, key) {
			t.Errorf("the help screen does not mention %q", key)
		}
	}
	if !strings.Contains(view, "systemctl cat") {
		t.Errorf("the help does not say what c does:\n%s", view)
	}
}
