package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/runner"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-kit/ui"
	"github.com/tui-tools/tui-systemd/internal/systemd"
)

// mode is the screen the app currently shows. Only one is open at a time,
// which keeps the update loop flat.
type mode int

const (
	modeUnits mode = iota
	modeJournal
	modeTimers
	modeBoot
	modeConfirm
	modeFilter
	modeHelp
	// modeCat shows the unit file as systemd assembles it.
	modeCat
	// modeForm is the guided editor: the drop-in form and the create form
	// share it, because they share the widget.
	modeForm
	// modePicker is the list a choice field opens.
	modePicker
)

// formKind says which form modeForm is showing, so enter knows what to build.
type formKind int

const (
	formNone formKind = iota
	formDropIn
	formNewUnit
)

// blameLimit is how many boot entries the boot view asks for. Thirty is what
// fits on a screen and covers everything anyone acts on.
const blameLimit = 30

// followInterval is how often the journal re-reads while following.
//
// This is a re-read on a timer, not a `journalctl -f` child process. The family
// rule is no daemon and no long-lived state, and a re-read gives the same
// answer with nothing to leak if the tool is killed.
const followInterval = 2 * time.Second

// app is the tui-systemd Bubble Tea model.
type app struct {
	backend systemd.Backend
	theme   theme.Theme
	// backendCompat is what the version probe found: it is rendered in the
	// header and it answers which views this systemd supports.
	backendCompat compat.Result

	units   []systemd.Unit
	timers  []systemd.Timer
	blame   []systemd.BlameEntry
	visible []systemd.Unit

	width, height int
	cursor        int
	offset        int
	filter        string
	state         systemd.State

	mode    mode
	confirm ui.Confirm
	input   ui.Input
	picker  ui.Picker
	// pickerFor is the form field the open picker is filling.
	pickerFor string

	// form is the open editor, and formKind says which one it is.
	form     form
	formKind formKind
	// formUnit is the unit the drop-in editor was opened on.
	formUnit systemd.Unit

	// catUnit and catText hold the unit-file viewer.
	catUnit string
	catText string
	catLine int

	// journal holds the log panel's state.
	journalUnit  string
	journalText  string
	journalLines int
	following    bool
	// followEpoch invalidates the ticks of an older follow session, so
	// leaving and re-entering follow does not double the refresh rate.
	followEpoch int

	status     string
	statusKind ui.StatusKind
	loading    bool
	// loadFailed reports that the last read failed, so the empty state does
	// not claim the machine simply has no units.
	loadFailed bool
	// busy blocks input while a command runs.
	busy bool
}

// unitsMsg carries the result of a unit list read.
type unitsMsg struct {
	units []systemd.Unit
	err   error
}

// timersMsg carries the result of a timer list read.
type timersMsg struct {
	timers []systemd.Timer
	err    error
}

// blameMsg carries the result of a boot times read.
type blameMsg struct {
	entries []systemd.BlameEntry
	err     error
}

// journalMsg carries the result of a journal read.
type journalMsg struct {
	unit string
	text string
	err  error
}

// tickMsg re-reads the journal while following.
type tickMsg struct{ epoch int }

// catMsg carries the result of a `systemctl cat` read.
type catMsg struct {
	unit string
	text string
	err  error
	// edit asks for the drop-in editor to open on the answer, which is how E
	// seeds the form from the file the unit carries today.
	edit bool
}

// plan is what a confirm dialog holds: the commands it will run in order, and
// the optional second step that gets its own dialog once they have.
//
// Most actions are one command. Writing a drop-in is three — create the
// directory, install the file, reload the manager — and creating a timer is
// four, and every one of them is on the dialog before any of them runs.
type plan struct {
	title    string
	commands []runner.Command
	// follow is the optional second step: restart the unit after an edit,
	// `enable --now` after a create. It is a separate confirmation because it
	// is a separate decision.
	follow     *runner.Command
	followBody string
	// stage is the temporary directory the plan's files were staged in. It
	// has to survive the dialog, because the install command copies from it,
	// and it is removed once the plan has run or been cancelled.
	stage string
}

// cleanup removes a plan's staging directory. A staged file is a copy of a
// unit file, not a secret, but it is this tool's litter and the tool clears
// it: nothing outside the dialog it was made for ever reads it again.
func (p plan) cleanup() {
	if p.stage != "" {
		_ = os.RemoveAll(p.stage)
	}
}

// ranMsg carries the result of a Run.
type ranMsg struct {
	plan   plan
	output string
	err    error
}

// newApp builds the model around a backend.
func newApp(backend systemd.Backend, th theme.Theme, journalLines int,
	backendCompat compat.Result) *app {
	if journalLines <= 0 {
		journalLines = systemd.DefaultJournalLines
	}
	a := &app{
		backend:       backend,
		theme:         th,
		backendCompat: backendCompat,
		width:         80,
		height:        24,
		loading:       true,
		state:         systemd.StateAll,
		journalLines:  journalLines,
	}
	if th.Warning != "" {
		a.setStatus(ui.StatusWarn, th.Warning)
	}
	return a
}

// Init starts the first read.
func (a *app) Init() tea.Cmd { return a.loadUnits() }

// readTimeout bounds a background read so a stuck command cannot wedge the UI.
const readTimeout = 30 * time.Second

// loadUnits reads the unit list in the background.
func (a *app) loadUnits() tea.Cmd {
	backend := a.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
		defer cancel()
		units, err := backend.Units(ctx)
		return unitsMsg{units: units, err: err}
	}
}

// loadTimers reads the timer list in the background.
func (a *app) loadTimers() tea.Cmd {
	backend := a.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
		defer cancel()
		timers, err := backend.Timers(ctx)
		return timersMsg{timers: timers, err: err}
	}
}

// loadBlame reads the boot times in the background.
func (a *app) loadBlame() tea.Cmd {
	backend := a.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
		defer cancel()
		entries, err := backend.Blame(ctx, blameLimit)
		return blameMsg{entries: entries, err: err}
	}
}

// loadJournal reads one unit's log in the background.
func (a *app) loadJournal(unit string) tea.Cmd {
	backend, lines := a.backend, a.journalLines
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
		defer cancel()
		text, err := backend.Journal(ctx, unit, lines)
		return journalMsg{unit: unit, text: text, err: err}
	}
}

// tick schedules the next follow refresh.
func (a *app) tick() tea.Cmd {
	epoch := a.followEpoch
	return tea.Tick(followInterval, func(time.Time) tea.Msg {
		return tickMsg{epoch: epoch}
	})
}

// loadCat reads a unit's files in the background.
func (a *app) loadCat(unit string, edit bool) tea.Cmd {
	backend := a.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
		defer cancel()
		text, err := backend.Cat(ctx, unit)
		return catMsg{unit: unit, text: text, err: err, edit: edit}
	}
}

// run executes a confirmed plan in the background, one command at a time,
// stopping at the first failure. A plan that fails half way leaves the machine
// where it stopped, which is why the status line reports the command that
// failed rather than only the plan.
func (a *app) run(p plan) tea.Cmd {
	backend := a.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		var outputs []string
		for _, cmd := range p.commands {
			out, err := backend.Run(ctx, cmd)
			if err != nil {
				return ranMsg{plan: p, output: out, err: err}
			}
			if trimmed := strings.TrimSpace(out); trimmed != "" {
				outputs = append(outputs, trimmed)
			}
		}
		return ranMsg{plan: p, output: strings.Join(outputs, "; ")}
	}
}

// setStatus records a message for the status line.
func (a *app) setStatus(kind ui.StatusKind, message string) {
	a.status = message
	a.statusKind = kind
}

// setStatusf records a formatted message for the status line.
func (a *app) setStatusf(kind ui.StatusKind, format string, args ...any) {
	a.setStatus(kind, fmt.Sprintf(format, args...))
}

// Update is the main event loop.
func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.clampCursor()
		return a, nil

	case unitsMsg:
		a.loading = false
		if msg.err != nil {
			a.loadFailed = true
			a.setStatus(ui.StatusError, msg.err.Error())
			return a, nil
		}
		a.loadFailed = false
		a.units = msg.units
		a.applyFilter()
		return a, nil

	case timersMsg:
		a.loading = false
		if msg.err != nil {
			a.setStatus(ui.StatusError, msg.err.Error())
			return a, nil
		}
		a.timers = msg.timers
		a.clampCursor()
		return a, nil

	case blameMsg:
		a.loading = false
		if msg.err != nil {
			a.setStatus(ui.StatusError, msg.err.Error())
			return a, nil
		}
		a.blame = msg.entries
		a.clampCursor()
		return a, nil

	case journalMsg:
		a.loading = false
		if msg.err != nil {
			a.setStatus(ui.StatusError, msg.err.Error())
			a.journalText = ""
			return a, nil
		}
		// A late answer for a unit we have already left must not overwrite
		// the panel the user is now looking at.
		if msg.unit != a.journalUnit {
			return a, nil
		}
		a.journalText = msg.text
		return a, nil

	case tickMsg:
		// Ignore the ticks of a follow session that already ended.
		if !a.following || a.mode != modeJournal || msg.epoch != a.followEpoch {
			return a, nil
		}
		return a, tea.Batch(a.loadJournal(a.journalUnit), a.tick())

	case catMsg:
		a.loading = false
		if msg.err != nil {
			a.setStatus(ui.StatusError, msg.err.Error())
			return a, nil
		}
		// A late answer for a unit the user has already left must not
		// overwrite the screen they are now looking at.
		if msg.unit != a.catUnit {
			return a, nil
		}
		a.catText = msg.text
		if msg.edit {
			return a, a.openDropInForm(msg.text)
		}
		return a, nil

	case ranMsg:
		a.busy = false
		msg.plan.cleanup()
		if msg.err != nil {
			a.setStatus(ui.StatusError, msg.err.Error())
			return a, a.loadUnits()
		}
		summary := strings.TrimSpace(msg.output)
		if summary == "" {
			summary = "done"
		}
		a.setStatusf(ui.StatusOK, "%s: %s", msg.plan.title,
			runner.FirstLine(summary))
		a.loading = true
		// The second step of a plan is asked for only once the first has
		// worked, and it is asked for the same way anything else is: a preview
		// and a confirmation.
		if follow := msg.plan.follow; follow != nil {
			a.confirmFollow(*follow, msg.plan.followBody)
		}
		return a, a.loadUnits()

	case tea.KeyMsg:
		return a.handleKey(msg)
	}

	// Anything else (cursor blink, …) only concerns an open text input.
	switch a.mode {
	case modeFilter:
		cmd, _ := a.input.Update(msg)
		return a, cmd
	case modeForm:
		return a, a.form.update(msg)
	}
	return a, nil
}

// handleKey routes a key press to the open screen.
func (a *app) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// ctrl+c always quits, even mid-dialog.
	if msg.Type == tea.KeyCtrlC {
		return a, tea.Quit
	}
	if a.busy {
		// A command is running: swallow input rather than queueing surprises.
		return a, nil
	}

	switch a.mode {
	case modeConfirm:
		return a.handleConfirm(msg)
	case modeFilter:
		return a.handleFilter(msg)
	case modeForm:
		return a.handleForm(msg)
	case modePicker:
		return a.handlePicker(msg)
	case modeCat:
		return a.handleCat(msg)
	case modeHelp:
		a.mode = modeUnits
		return a, nil
	case modeJournal:
		return a.handleJournal(msg)
	case modeTimers, modeBoot:
		return a.handleListView(msg)
	default:
		return a.handleUnitsKey(msg)
	}
}

// handleConfirm resolves the confirm dialog.
func (a *app) handleConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	a.confirm.Update(msg)
	if !a.confirm.Done {
		return a, nil
	}
	a.mode = modeUnits
	confirmed := a.confirm.Confirmed
	pending, ok := a.confirm.Payload.(plan)
	a.confirm = ui.Confirm{}
	if !confirmed || !ok || len(pending.commands) == 0 {
		pending.cleanup()
		a.setStatus(ui.StatusInfo, "cancelled")
		return a, nil
	}
	a.busy = true
	a.setStatusf(ui.StatusInfo, "running %s…", a.backend.Preview(pending.commands[0]))
	return a, a.run(pending)
}

// handleForm handles the guided editor, which both authoring screens use.
func (a *app) handleForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		a.mode, a.formKind = modeUnits, formNone
		a.setStatus(ui.StatusInfo, "cancelled")
		return a, nil
	case "tab", "down", "ctrl+n":
		a.form.move(1)
		return a, nil
	case "shift+tab", "up", "ctrl+p":
		a.form.move(-1)
		return a, nil
	case "left":
		a.form.cycle(-1)
		a.form.syncKind()
		return a, nil
	case "right":
		a.form.cycle(1)
		a.form.syncKind()
		return a, nil
	case " ":
		// Space opens the list for a choice field. It is not enter, because
		// enter has to mean "review" from every field: a form whose choice
		// fields could not submit would be a dead end.
		active := a.form.activeField()
		if active == nil || !active.isChoice() {
			break
		}
		a.pickerFor = active.name
		a.picker = ui.NewPicker(active.label, active.pickerOptions(), active.display())
		a.mode = modePicker
		return a, nil
	case "enter":
		return a, a.submitForm()
	}
	return a, a.form.update(msg)
}

// handlePicker resolves the list a choice field opened.
func (a *app) handlePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	a.picker.Update(msg)
	if !a.picker.Done {
		return a, nil
	}
	if a.picker.Accepted {
		a.form.set(a.pickerFor, fromPickerLabel(a.picker.Selected()))
		a.form.syncKind()
	}
	a.picker = ui.Picker{}
	a.pickerFor = ""
	a.mode = modeForm
	return a, nil
}

// handleCat handles the unit-file viewer, which is a read: it scrolls and it
// leaves, and the only thing it can start is the editor for the same unit.
func (a *app) handleCat(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "c", "enter":
		a.mode = modeUnits
		return a, nil
	case "?":
		a.mode = modeHelp
		return a, nil
	case "down", "ctrl+n", "j":
		a.catLine++
	case "up", "ctrl+p", "k":
		a.catLine--
	case "g", "home":
		a.catLine = 0
	case "G", "end":
		a.catLine = len(strings.Split(a.catText, "\n"))
	case "pgdown", "ctrl+f":
		a.catLine += a.listHeight()
	case "pgup", "ctrl+b":
		a.catLine -= a.listHeight()
	case "r", "ctrl+r":
		return a, a.loadCat(a.catUnit, false)
	case "E":
		unit, ok := a.unitNamed(a.catUnit)
		if !ok {
			return a, nil
		}
		a.formUnit = unit
		return a, a.openDropInForm(a.catText)
	}
	a.clampCatLine()
	return a, nil
}

// clampCatLine keeps the viewer's scroll offset inside the file.
func (a *app) clampCatLine() {
	lines := len(strings.Split(strings.TrimRight(a.catText, "\n"), "\n"))
	a.catLine = min(max(a.catLine, 0), max(lines-a.listHeight(), 0))
}

// handleFilter resolves the filter prompt.
func (a *app) handleFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	cmd, _ := a.input.Update(msg)
	if !a.input.Done {
		// Filter as the user types.
		a.filter = a.input.Value()
		a.applyFilter()
		return a, cmd
	}
	if a.input.Accepted {
		a.filter = a.input.Value()
	} else {
		a.filter = ""
	}
	a.applyFilter()
	a.mode = modeUnits
	return a, nil
}

// handleJournal handles the log panel.
func (a *app) handleJournal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "j", "enter":
		a.mode = modeUnits
		a.following = false
		// Bump the epoch so any in-flight tick is ignored.
		a.followEpoch++
		return a, nil
	case "f":
		a.following = !a.following
		a.followEpoch++
		if a.following {
			a.setStatusf(ui.StatusInfo, "following %s, re-reading every %s",
				a.journalUnit, followInterval)
			return a, a.tick()
		}
		a.setStatus(ui.StatusInfo, "stopped following")
		return a, nil
	case "r", "ctrl+r":
		return a, a.loadJournal(a.journalUnit)
	case "?":
		a.mode = modeHelp
		return a, nil
	}
	return a, nil
}

// handleListView handles the timers and boot screens, which are read-only.
func (a *app) handleListView(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		a.mode = modeUnits
		a.cursor, a.offset = 0, 0
		a.clampCursor()
		return a, nil
	case "?":
		a.mode = modeHelp
		return a, nil
	case "down", "ctrl+n":
		a.moveCursor(1)
	case "up", "ctrl+p", "k":
		a.moveCursor(-1)
	case "g", "home":
		a.cursor, a.offset = 0, 0
	case "G", "end":
		a.cursor = max(a.rowCount()-1, 0)
		a.clampCursor()
	case "pgdown", "ctrl+f":
		a.moveCursor(a.listHeight())
	case "pgup", "ctrl+b":
		a.moveCursor(-a.listHeight())
	case "r", "ctrl+r":
		a.loading = true
		if a.mode == modeTimers {
			return a, a.loadTimers()
		}
		return a, a.loadBlame()
	case "t":
		return a, a.openTimers()
	case "b":
		return a, a.openBoot()
	}
	return a, nil
}

// handleUnitsKey handles the main screen.
func (a *app) handleUnitsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// An action key applies to the selected unit, and always opens a confirm
	// dialog first. This is checked before navigation so the action table is
	// the single source of truth for what each key does.
	if spec, ok := systemd.ActionFor(key); ok {
		return a, a.confirmAction(spec)
	}

	switch key {
	case "q", "esc":
		return a, tea.Quit
	case "?":
		a.mode = modeHelp
	case "down", "ctrl+n":
		a.moveCursor(1)
	case "up", "ctrl+p", "k":
		a.moveCursor(-1)
	case "g", "home":
		a.cursor, a.offset = 0, 0
	case "G", "end":
		a.cursor = max(len(a.visible)-1, 0)
		a.clampCursor()
	case "pgdown", "ctrl+f":
		a.moveCursor(a.listHeight())
	case "pgup", "ctrl+b":
		a.moveCursor(-a.listHeight())
	case "tab":
		a.cycleState(1)
	case "shift+tab":
		a.cycleState(-1)
	case "/":
		a.input = ui.NewInput("Filter units", "name, description, state…", a.filter)
		a.input.Help = "Matches the name, the description and every state. Empty clears the filter."
		a.mode = modeFilter
	case "ctrl+r":
		a.loading = true
		return a, a.loadUnits()
	case "j", "enter":
		return a, a.openJournal()
	case "c":
		return a, a.openCat()
	case "E":
		return a, a.openEditor()
	case "n":
		return a, a.openNewUnit()
	case "t":
		return a, a.openTimers()
	case "b":
		return a, a.openBoot()
	}
	return a, nil
}

// openCat shows the unit file of the selected unit.
func (a *app) openCat() tea.Cmd {
	unit, ok := a.selectedUnit()
	if !ok {
		a.setStatus(ui.StatusWarn, "no unit selected")
		return nil
	}
	a.catUnit, a.catText, a.catLine = unit.Name, "", 0
	a.mode = modeCat
	a.loading = true
	return a.loadCat(unit.Name, false)
}

// openEditor opens the drop-in editor for the selected unit. The read comes
// first: the form has to open on the drop-in the unit carries today, and the
// diff has to have a left side.
func (a *app) openEditor() tea.Cmd {
	unit, ok := a.selectedUnit()
	if !ok {
		a.setStatus(ui.StatusWarn, "no unit selected")
		return nil
	}
	if err := systemd.EditableUnit(unit); err != nil {
		a.setStatus(ui.StatusWarn, err.Error())
		return nil
	}
	a.formUnit = unit
	a.catUnit, a.catText = unit.Name, ""
	a.loading = true
	a.setStatusf(ui.StatusInfo, "reading the unit files of %s…", unit.Name)
	return a.loadCat(unit.Name, true)
}

// openDropInForm opens the editor once the unit's files have been read.
func (a *app) openDropInForm(catOutput string) tea.Cmd {
	if err := systemd.EditableUnit(a.formUnit); err != nil {
		a.setStatus(ui.StatusWarn, err.Error())
		return nil
	}
	a.form = newDropInForm(a.formUnit,
		systemd.ParseDropIn(systemd.Existing(a.formUnit.Name, catOutput)))
	a.formKind = formDropIn
	a.mode = modeForm
	a.setStatus(ui.StatusInfo, "")
	return nil
}

// openNewUnit opens the create form. It belongs to the manager rather than to
// a unit, so it needs no selection.
func (a *app) openNewUnit() tea.Cmd {
	a.form = newUnitForm()
	a.formKind = formNewUnit
	a.mode = modeForm
	a.setStatus(ui.StatusInfo, "")
	return nil
}

// submitForm turns the open form into a write plan and opens the confirm
// dialog on it. Everything the dialog claims — that systemd read the file,
// what changes in it, which commands run — is decided here, before the
// question is asked.
func (a *app) submitForm() tea.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()

	var (
		write systemd.WritePlan
		err   error
	)
	switch a.formKind {
	case formDropIn:
		write, err = a.backend.BuildDropIn(ctx, systemd.DropInRequest{
			Unit:    a.formUnit,
			Cat:     a.catText,
			Values:  a.form.dropInValues(),
			Restart: a.form.value(fieldRestart) == answerYes,
		})
	case formNewUnit:
		write, err = a.backend.BuildNewUnit(ctx,
			a.form.newUnitRequest(systemd.SortedNames(a.units)))
	default:
		return nil
	}
	if err != nil {
		a.setStatus(ui.StatusError, err.Error())
		return nil
	}

	a.mode, a.formKind = modeConfirm, formNone
	a.confirm = ui.Confirm{
		Title:   write.Title,
		Body:    a.writeBody(write),
		Command: a.previewAll(write.Commands),
		Danger:  true,
		Payload: plan{
			title:      write.Title,
			commands:   write.Commands,
			follow:     write.Follow,
			followBody: write.FollowBody,
			stage:      write.Stage,
		},
	}
	return nil
}

// confirmFollow opens the dialog for the optional second step of a plan.
func (a *app) confirmFollow(cmd runner.Command, body string) {
	a.mode = modeConfirm
	a.confirm = ui.Confirm{
		Title:   cmd.Description,
		Body:    body,
		Command: a.backend.Preview(cmd),
		Danger:  cmd.Destructive,
		Payload: plan{title: cmd.Description, commands: []runner.Command{cmd}},
	}
}

// writeBody is what the confirm dialog says above the commands: what systemd's
// own parser made of the staged files, the caveat that applies to this change,
// and the diff itself.
func (a *app) writeBody(write systemd.WritePlan) string {
	var parts []string
	if write.Validated {
		parts = append(parts, "✓ "+write.Validation)
	} else if write.Validation != "" {
		parts = append(parts, "! the syntax check "+write.Validation)
	}
	if write.Warning != "" {
		parts = append(parts, write.Warning)
	}
	if diff := write.Diffs(); diff != "" {
		parts = append(parts, a.diffForDialog(diff))
	}
	return strings.Join(parts, "\n\n")
}

// diffForDialog trims a diff to what a dialog can hold. A file this tool
// writes is a dozen lines, so the cap only fires on a unit someone grew by
// hand, and it says so rather than silently cutting.
func (a *app) diffForDialog(diff string) string {
	limit := max(a.height-14, 6)
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	if len(lines) <= limit {
		return strings.Join(lines, "\n")
	}
	return strings.Join(append(lines[:limit],
		fmt.Sprintf("… and %d more lines", len(lines)-limit)), "\n")
}

// previewAll renders every command of a plan, one per line, each carrying the
// prompt the dialog puts in front of the first.
func (a *app) previewAll(commands []runner.Command) string {
	previews := make([]string, 0, len(commands))
	for _, cmd := range commands {
		previews = append(previews, a.backend.Preview(cmd))
	}
	return strings.Join(previews, "\n$ ")
}

// confirmAction builds an action's command and opens the confirm dialog. It is
// the only path to a change.
func (a *app) confirmAction(spec systemd.ActionSpec) tea.Cmd {
	name := ""
	if !spec.Manager {
		unit, ok := a.selectedUnit()
		if !ok {
			a.setStatus(ui.StatusWarn, "no unit selected")
			return nil
		}
		name = unit.Name
	}
	cmd, err := a.backend.Build(spec, name)
	if err != nil {
		a.setStatus(ui.StatusError, err.Error())
		return nil
	}
	a.mode = modeConfirm
	a.confirm = ui.Confirm{
		Title:   cmd.Description,
		Body:    spec.Body,
		Command: a.backend.Preview(cmd),
		Danger:  cmd.Destructive,
		Payload: plan{title: cmd.Description, commands: []runner.Command{cmd}},
	}
	return nil
}

// openJournal shows the log of the selected unit.
func (a *app) openJournal() tea.Cmd {
	unit, ok := a.selectedUnit()
	if !ok {
		a.setStatus(ui.StatusWarn, "no unit selected")
		return nil
	}
	a.journalUnit = unit.Name
	a.journalText = ""
	a.following = false
	a.followEpoch++
	a.mode = modeJournal
	a.loading = true
	return a.loadJournal(unit.Name)
}

// hasTimers reports whether this systemd can produce a timer list the tool can
// parse. The answer comes from the manifest (`timers` since 250), not from a
// version comparison written here: `list-timers` grew --output=json in 250,
// and its text table cannot be sliced into columns without mangling the
// timestamps.
func (a *app) hasTimers() bool {
	return a.backendCompat.Caps().Has("timers")
}

// timersUnavailable is the message shown when the key is pressed anyway. It
// names the version that would be needed and the one that is running, because
// "unavailable" without a reason is what sends people to the issue tracker.
func (a *app) timersUnavailable() string {
	message := "the timers view needs systemd 250 or newer"
	if since, ok := a.backendCompat.Caps().Since("timers"); ok {
		message = "the timers view needs systemd " + since + " or newer"
	}
	if version := a.backendCompat.Version; version != "" {
		message += "; this machine runs " + version
	}
	return message
}

// openTimers shows the timer list, when this systemd has one.
func (a *app) openTimers() tea.Cmd {
	if !a.hasTimers() {
		a.setStatus(ui.StatusWarn, a.timersUnavailable())
		return nil
	}
	a.mode = modeTimers
	a.cursor, a.offset = 0, 0
	a.loading = true
	return a.loadTimers()
}

// openBoot shows the boot breakdown, when this systemd can produce one.
// `systemd-analyze blame` predates the minimum this tool declares, so the gate
// only fires on a machine the header has already flagged as too old — which is
// exactly the machine where a silent empty view would be confusing.
func (a *app) openBoot() tea.Cmd {
	if !a.backendCompat.Caps().Has("boot-blame") {
		message := "the boot view needs a newer systemd"
		if since, ok := a.backendCompat.Caps().Since("boot-blame"); ok {
			message = "the boot view needs systemd " + since + " or newer"
		}
		if version := a.backendCompat.Version; version != "" {
			message += "; this machine runs " + version
		}
		a.setStatus(ui.StatusWarn, message)
		return nil
	}
	a.mode = modeBoot
	a.cursor, a.offset = 0, 0
	a.loading = true
	return a.loadBlame()
}

// cycleState moves the state filter one step.
func (a *app) cycleState(delta int) {
	index := 0
	for i, s := range systemd.States {
		if s == a.state {
			index = i
			break
		}
	}
	index = (index + delta + len(systemd.States)) % len(systemd.States)
	a.state = systemd.States[index]
	a.cursor, a.offset = 0, 0
	a.applyFilter()
}

// applyFilter recomputes the visible units from the state and text filters.
func (a *app) applyFilter() {
	kept := make([]systemd.Unit, 0, len(a.units))
	needle := strings.ToLower(a.filter)
	for _, u := range a.units {
		if !a.state.Match(u) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(unitHaystack(u)), needle) {
			continue
		}
		kept = append(kept, u)
	}
	a.visible = kept
	a.clampCursor()
}

// unitHaystack is the text the filter matches against.
func unitHaystack(u systemd.Unit) string {
	return strings.Join([]string{
		u.Name, u.Description, u.Load, u.Active, u.Sub, u.FileState,
	}, " ")
}

// selectedUnit returns the highlighted unit.
func (a *app) selectedUnit() (systemd.Unit, bool) {
	if a.cursor < 0 || a.cursor >= len(a.visible) {
		return systemd.Unit{}, false
	}
	return a.visible[a.cursor], true
}

// rowCount is the number of rows the current screen holds.
func (a *app) rowCount() int {
	switch a.mode {
	case modeTimers:
		return len(a.timers)
	case modeBoot:
		return len(a.blame)
	default:
		return len(a.visible)
	}
}

// moveCursor moves the selection and keeps the viewport in sync.
func (a *app) moveCursor(delta int) {
	a.cursor += delta
	a.clampCursor()
}

// clampCursor keeps the cursor and the scroll offset within range.
func (a *app) clampCursor() {
	count := a.rowCount()
	if count == 0 {
		a.cursor, a.offset = 0, 0
		return
	}
	a.cursor = min(max(a.cursor, 0), count-1)

	height := a.listHeight()
	if a.cursor < a.offset {
		a.offset = a.cursor
	}
	if a.cursor >= a.offset+height {
		a.offset = a.cursor - height + 1
	}
	a.offset = max(min(a.offset, max(count-height, 0)), 0)
}
