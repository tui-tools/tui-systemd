package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/tui-tools/tui-kit/ui"
	"github.com/tui-tools/tui-systemd/internal/systemd"
)

// Layout constants: the rows the list cannot use.
const (
	headerLines = 2
	footerLines = 2
	// minListHeight keeps at least one visible row on a very short terminal.
	minListHeight = 1
)

// listHeight is the number of rows that fit on screen.
func (a *app) listHeight() int {
	// header + table header + help bar + status line.
	return max(a.height-headerLines-footerLines-2, minListHeight)
}

// View renders the whole screen.
func (a *app) View() string {
	switch a.mode {
	case modeConfirm:
		return a.confirm.View(a.theme, a.width, a.height)
	case modeFilter:
		return a.input.View(a.theme, a.width, a.height)
	case modeHelp:
		return placeCenter(
			ui.HelpScreen(a.theme, "tui-systemd — keys", helpKeys(), a.width),
			a.width, a.height)
	case modeJournal:
		return a.journalView()
	case modeTimers:
		return a.timersView()
	case modeBoot:
		return a.bootView()
	default:
		return a.unitsView()
	}
}

// placeCenter centers a rendered box in the terminal.
func placeCenter(box string, width, height int) string {
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

// screen assembles the four bands every view shares.
func (a *app) screen(header, body string, hints []ui.KeyHint, fallback string) string {
	help := ui.HelpBar(a.theme, hints, a.width)
	status := ui.StatusLine(a.theme, a.statusKind, a.status, fallback, a.width)
	return strings.Join([]string{header, body, help, status}, "\n")
}

// unitsView renders the main screen.
func (a *app) unitsView() string {
	var body string
	switch {
	case a.loading && len(a.visible) == 0:
		body = ui.EmptyState(a.theme, "reading the units…", a.width, a.listHeight()+1)
	case len(a.visible) == 0 && a.loadFailed:
		body = ui.EmptyState(a.theme,
			"could not read the units — see the message below",
			a.width, a.listHeight()+1)
	case len(a.visible) == 0 && (a.filter != "" || a.state != systemd.StateAll):
		body = ui.EmptyState(a.theme, "no unit matches "+a.filterLabel(),
			a.width, a.listHeight()+1)
	case len(a.visible) == 0:
		body = ui.EmptyState(a.theme, "no units", a.width, a.listHeight()+1)
	default:
		body = a.unitsTable()
	}
	return a.screen(a.unitsHeader(), body, a.shortHelpKeys(), a.defaultStatus())
}

// filterLabel describes the active filters, for the empty state.
func (a *app) filterLabel() string {
	var parts []string
	if a.state != systemd.StateAll {
		parts = append(parts, "state "+string(a.state))
	}
	if a.filter != "" {
		parts = append(parts, strconv.Quote(a.filter))
	}
	return strings.Join(parts, " and ")
}

// unitsHeader renders the facts at the top of the main screen.
func (a *app) unitsHeader() string {
	t := a.theme

	failed := 0
	running := 0
	for _, u := range a.units {
		if u.Failed() {
			failed++
		}
		if u.Running() {
			running++
		}
	}

	failedStyle := t.OK
	if failed > 0 {
		failedStyle = t.Danger
	}
	facts := []ui.Fact{
		{Label: "failed", Value: strconv.Itoa(failed), Style: &failedStyle},
		{Label: "running", Value: strconv.Itoa(running)},
		{Label: "total", Value: strconv.Itoa(len(a.units))},
		{Label: "showing", Value: string(a.state)},
	}
	// The systemd version, when it was probed: quiet on a tested version,
	// coloured on one nobody has run against.
	if a.backendCompat.Backend != "" {
		facts = append(facts, ui.CompatFact(t, a.backendCompat))
	}

	subtitle := a.backend.Describe()
	if a.filter != "" {
		subtitle += "  ·  filter: " + a.filter
	}
	return ui.Header{Title: "tui-systemd", Subtitle: subtitle, Facts: facts}.
		Render(t, a.width)
}

// defaultStatus is the hint shown when there is no message to report.
func (a *app) defaultStatus() string {
	count := strconv.Itoa(len(a.visible))
	if len(a.visible) != len(a.units) {
		return count + " of " + strconv.Itoa(len(a.units)) + " units  ·  ? for help"
	}
	return count + " units  ·  ? for help"
}

// unitsTable renders the unit list, dropping columns on narrow terminals.
func (a *app) unitsTable() string {
	columns := []ui.Column{
		{Title: "UNIT", Width: 28, Flex: true},
		{Title: "ACTIVE", Width: 8},
		{Title: "SUB", Width: 8},
	}
	showBoot := a.width >= 70
	showDescription := a.width >= 92
	if showBoot {
		columns = append(columns, ui.Column{Title: "AT BOOT", Width: 9})
	}
	if showDescription {
		columns = append(columns, ui.Column{Title: "DESCRIPTION", Width: 24, Flex: true})
	}

	rows := make([][]string, 0, len(a.visible))
	styles := make([]*lipgloss.Style, 0, len(a.visible))
	for _, u := range a.visible {
		row := []string{u.Name, u.Active, u.Sub}
		if showBoot {
			row = append(row, bootLabel(u))
		}
		if showDescription {
			row = append(row, u.Description)
		}
		rows = append(rows, row)
		styles = append(styles, a.unitStyle(u))
	}

	return ui.Table{
		Columns:  columns,
		Rows:     rows,
		Styles:   styles,
		Selected: a.cursor,
		Offset:   a.offset,
		Height:   a.listHeight(),
	}.Render(a.theme, a.width)
}

// bootLabel says whether a unit starts at boot, in one short word. A unit with
// no unit file gets a dash rather than a misleading "disabled": there is
// nothing to enable.
func bootLabel(u systemd.Unit) string {
	switch {
	case u.Masked():
		return "masked"
	case u.FileState == "":
		return "-"
	default:
		return u.FileState
	}
}

// unitNamed finds a unit by name in the last read.
func (a *app) unitNamed(name string) (systemd.Unit, bool) {
	for _, u := range a.units {
		if u.Name == name {
			return u, true
		}
	}
	return systemd.Unit{}, false
}

// stateStyle is the color a unit's state deserves in a header fact.
func (a *app) stateStyle(u systemd.Unit) lipgloss.Style {
	switch {
	case u.Failed():
		return a.theme.Danger
	case u.Masked():
		return a.theme.Warn
	case u.Running():
		return a.theme.OK
	default:
		return a.theme.Muted
	}
}

// unitStyle colors a row by its state, so a failure stands out from a running
// unit at a glance.
func (a *app) unitStyle(u systemd.Unit) *lipgloss.Style {
	var style lipgloss.Style
	switch {
	case u.Failed():
		style = a.theme.Row.Foreground(a.theme.Danger.GetForeground())
	case u.Masked():
		style = a.theme.Row.Foreground(a.theme.Warn.GetForeground())
	case u.Running():
		style = a.theme.Row.Foreground(a.theme.OK.GetForeground())
	default:
		style = a.theme.Row
	}
	return &style
}

// journalView renders the log panel.
func (a *app) journalView() string {
	t := a.theme
	subtitle := "journal · last " + strconv.Itoa(a.journalLines) + " lines"

	// Every screen carries a two-line header band: the unit's own state is
	// worth having next to its log, and a header that keeps its height means
	// the body does not jump as the user moves between screens.
	facts := []ui.Fact{}
	if unit, ok := a.unitNamed(a.journalUnit); ok {
		style := a.stateStyle(unit)
		facts = append(facts,
			ui.Fact{Label: "state", Value: unit.Active, Style: &style},
			ui.Fact{Label: "sub", Value: unit.Sub},
			ui.Fact{Label: "at boot", Value: bootLabel(unit)})
	}
	following := "no"
	if a.following {
		following = "every " + followInterval.String()
	}
	facts = append(facts, ui.Fact{Label: "following", Value: following})

	header := ui.Header{
		Title:    a.journalUnit,
		Subtitle: subtitle,
		Facts:    facts,
	}.Render(t, a.width)

	height := a.listHeight() + 1
	var body string
	switch {
	case a.loading && a.journalText == "":
		body = ui.EmptyState(t, "reading the journal…", a.width, height)
	case a.journalText == "":
		body = ui.EmptyState(t, "no journal entries for this unit", a.width, height)
	default:
		// The tail is what matters in a log, so the last lines are the ones
		// kept when the panel cannot show them all.
		lines := strings.Split(strings.TrimRight(a.journalText, "\n"), "\n")
		if len(lines) > height {
			lines = lines[len(lines)-height:]
		}
		rendered := make([]string, 0, height)
		for _, line := range lines {
			rendered = append(rendered, t.Row.Width(a.width).
				Render(ui.Truncate(line, a.width-2)))
		}
		for len(rendered) < height {
			rendered = append(rendered, t.Row.Width(a.width).Render(""))
		}
		body = strings.Join(rendered, "\n")
	}

	followLabel := "follow"
	if a.following {
		followLabel = "stop following"
	}
	hints := []ui.KeyHint{
		{Key: "f", Desc: followLabel},
		{Key: "r", Desc: "re-read"},
		{Key: "esc", Desc: "back"},
		{Key: "?", Desc: "help"},
	}
	return a.screen(header, body, hints, a.journalUnit+"  ·  esc to go back")
}

// timersView renders the timer list.
func (a *app) timersView() string {
	header := ui.Header{
		Title:    "tui-systemd",
		Subtitle: "timers  ·  esc to go back",
		Facts:    []ui.Fact{{Label: "timers", Value: strconv.Itoa(len(a.timers))}},
	}.Render(a.theme, a.width)

	var body string
	if len(a.timers) == 0 {
		message := "reading the timers…"
		if !a.loading {
			message = "no timers on this machine"
		}
		body = ui.EmptyState(a.theme, message, a.width, a.listHeight()+1)
	} else {
		now := time.Now()
		columns := []ui.Column{
			{Title: "NEXT", Width: 12},
			{Title: "LAST", Width: 12},
			{Title: "TIMER", Width: 24, Flex: true},
		}
		showActivates := a.width >= 72
		if showActivates {
			columns = append(columns,
				ui.Column{Title: "ACTIVATES", Width: 24, Flex: true})
		}
		rows := make([][]string, 0, len(a.timers))
		for _, timer := range a.timers {
			row := []string{
				until(now, timer.Next),
				since(now, timer.Last),
				timer.Unit,
			}
			if showActivates {
				row = append(row, timer.Activates)
			}
			rows = append(rows, row)
		}
		body = ui.Table{
			Columns: columns, Rows: rows, Selected: a.cursor,
			Offset: a.offset, Height: a.listHeight(),
		}.Render(a.theme, a.width)
	}

	hints := []ui.KeyHint{
		{Key: "r", Desc: "re-read"},
		{Key: "b", Desc: "boot"},
		{Key: "esc", Desc: "back"},
		{Key: "?", Desc: "help"},
	}
	return a.screen(header, body, hints,
		strconv.Itoa(len(a.timers))+" timers  ·  esc to go back")
}

// bootView renders the boot breakdown.
func (a *app) bootView() string {
	header := ui.Header{
		Title:    "tui-systemd",
		Subtitle: "boot times, slowest first  ·  esc to go back",
		Facts: []ui.Fact{
			{Label: "showing", Value: "top " + strconv.Itoa(blameLimit)},
		},
	}.Render(a.theme, a.width)

	var body string
	if len(a.blame) == 0 {
		message := "reading the boot times…"
		if !a.loading {
			message = "systemd-analyze reported nothing for this boot"
		}
		body = ui.EmptyState(a.theme, message, a.width, a.listHeight()+1)
	} else {
		rows := make([][]string, 0, len(a.blame))
		for i, entry := range a.blame {
			rows = append(rows, []string{
				strconv.Itoa(i + 1), entry.Raw, entry.Unit,
			})
		}
		body = ui.Table{
			Columns: []ui.Column{
				{Title: "#", Width: 3},
				{Title: "TIME", Width: 14},
				{Title: "UNIT", Width: 32, Flex: true},
			},
			Rows: rows, Selected: a.cursor, Offset: a.offset,
			Height: a.listHeight(),
		}.Render(a.theme, a.width)
	}

	hints := []ui.KeyHint{
		{Key: "r", Desc: "re-read"},
		{Key: "t", Desc: "timers"},
		{Key: "esc", Desc: "back"},
		{Key: "?", Desc: "help"},
	}
	return a.screen(header, body, hints,
		strconv.Itoa(len(a.blame))+" units  ·  esc to go back")
}

// until renders how long until a moment, "in 4min". The zero time means there
// is no next run.
func until(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := t.Sub(now)
	if d < 0 {
		return "due"
	}
	return "in " + shortDuration(d)
}

// since renders how long ago a moment was, "37min ago".
func since(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	if d < 0 {
		return "just now"
	}
	return shortDuration(d) + " ago"
}

// shortDuration renders a duration in one unit, the largest that fits. A log
// view has no room for "5h32m17.4s", and the extra precision answers nothing.
func shortDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dmin", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// shortHelpKeys is the single-line hint bar on the main screen. A view this
// systemd cannot serve is not advertised: on a machine older than 250 the
// timers hint is left out rather than offered and then refused.
func (a *app) shortHelpKeys() []ui.KeyHint {
	hints := []ui.KeyHint{
		{Key: "s", Desc: "start"},
		{Key: "x", Desc: "stop"},
		{Key: "r", Desc: "restart"},
		{Key: "e", Desc: "enable"},
		{Key: "j", Desc: "journal"},
	}
	if a.hasTimers() {
		hints = append(hints, ui.KeyHint{Key: "t", Desc: "timers"})
	}
	return append(hints,
		ui.KeyHint{Key: "b", Desc: "boot"},
		ui.KeyHint{Key: "/", Desc: "filter"},
		ui.KeyHint{Key: "?", Desc: "help"},
		ui.KeyHint{Key: "q", Desc: "quit"})
}

// actionHints renders the action table two keys to a row, which is what keeps
// the help screen inside a short terminal. The rows are still generated from
// the action table, so a new action cannot go missing from the help.
func actionHints() []ui.KeyHint {
	var hints []ui.KeyHint
	for i := 0; i < len(systemd.Actions); {
		first := systemd.Actions[i]
		// A manager action takes no unit, so it never pairs with one.
		if first.Manager || i+1 >= len(systemd.Actions) ||
			systemd.Actions[i+1].Manager {
			hints = append(hints, ui.KeyHint{
				Key: first.Key, Desc: strings.ToLower(first.Label)})
			i++
			continue
		}
		second := systemd.Actions[i+1]
		hints = append(hints, ui.KeyHint{
			Key: first.Key + " / " + second.Key,
			Desc: strings.ToLower(first.Label) + " / " +
				strings.ToLower(second.Label) + " the selected unit",
		})
		i += 2
	}
	return hints
}

// helpKeys is the full key list shown on the help screen. It is kept short
// enough to fit a 24-row terminal, because a help screen that scrolls off the
// top is worse than no help at all.
func helpKeys() []ui.KeyHint {
	hints := []ui.KeyHint{
		{Key: "↑ / ↓", Desc: "move the selection (ctrl+p / ctrl+n also work)"},
		{Key: "g / G", Desc: "first / last row; pgup / pgdn scroll a page"},
		{Key: "tab", Desc: "cycle the state filter: all, failed, active, inactive"},
		{Key: "/", Desc: "filter by name, description or state (esc clears)"},
		{Key: "", Desc: ""},
	}
	hints = append(hints, actionHints()...)
	return append(hints,
		ui.KeyHint{Key: "", Desc: ""},
		ui.KeyHint{Key: "j / enter", Desc: "journal for the selected unit"},
		ui.KeyHint{Key: "f", Desc: "in the journal: follow, re-reading every 2s"},
		ui.KeyHint{Key: "t / b", Desc: "timers / boot times, slowest first"},
		ui.KeyHint{Key: "ctrl+r", Desc: "re-read the current view"},
		ui.KeyHint{Key: "? / q", Desc: "this help / quit"},
		ui.KeyHint{Key: "", Desc: ""},
		ui.KeyHint{Key: "note", Desc: "j opens the journal, so this list moves with the arrows"},
		ui.KeyHint{Key: "", Desc: "every change is previewed and confirmed first"},
	)
}
