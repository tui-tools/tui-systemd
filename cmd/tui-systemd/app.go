package main

import (
	"context"
	"fmt"
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

// ranMsg carries the result of a Run.
type ranMsg struct {
	cmd    runner.Command
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

// run executes a confirmed command in the background.
func (a *app) run(cmd runner.Command) tea.Cmd {
	backend := a.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		out, err := backend.Run(ctx, cmd)
		return ranMsg{cmd: cmd, output: out, err: err}
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

	case ranMsg:
		a.busy = false
		if msg.err != nil {
			a.setStatus(ui.StatusError, msg.err.Error())
			return a, a.loadUnits()
		}
		summary := strings.TrimSpace(msg.output)
		if summary == "" {
			summary = "done"
		}
		a.setStatusf(ui.StatusOK, "%s: %s", msg.cmd.Description,
			runner.FirstLine(summary))
		a.loading = true
		return a, a.loadUnits()

	case tea.KeyMsg:
		return a.handleKey(msg)
	}

	// Anything else (cursor blink, …) only concerns an open text input.
	if a.mode == modeFilter {
		cmd, _ := a.input.Update(msg)
		return a, cmd
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
	cmd, ok := a.confirm.Payload.(runner.Command)
	a.confirm = ui.Confirm{}
	if !confirmed || !ok {
		a.setStatus(ui.StatusInfo, "cancelled")
		return a, nil
	}
	a.busy = true
	a.setStatusf(ui.StatusInfo, "running %s…", a.backend.Preview(cmd))
	return a, a.run(cmd)
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
	case "t":
		return a, a.openTimers()
	case "b":
		return a, a.openBoot()
	}
	return a, nil
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
		Payload: cmd,
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
