package main

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-kit/ui"
	"github.com/tui-tools/tui-systemd/internal/systemd"
)

// This file is the guided editor both authoring screens are built from.
//
// It is one form type rather than two, because the drop-in editor and the
// new-unit form ask the same kind of question: a short list of fields, each
// either a closed set of values or a line of text, with the property's own
// help under the cursor. What differs between them is the field list, and that
// is all that differs in the code.

// unsetLabel is how an empty choice reads on the form. An empty cell would
// look like a rendering bug; "(not set)" says the property is deliberately
// absent from the file.
const unsetLabel = "(not set)"

// The values of a yes/no field, spelled out so the form never shows a bare
// boolean.
const (
	answerNo  = "no"
	answerYes = "yes"
)

// field is one row of a form: a closed choice, or a line of text.
type field struct {
	// name identifies the field to the code that reads the form. For a
	// drop-in it is the systemd property name.
	name string
	// label is what the row is called on screen.
	label string
	// help is shown under the fields while this row is selected.
	help string
	// options is the closed value set; empty makes the row a text box.
	options []string
	choice  int
	input   textinput.Model
	// hidden takes a row out of the form without removing it, which is how
	// OnCalendar disappears when the kind is service.
	hidden bool
}

// isChoice reports whether the row is a picker rather than a text box.
func (f field) isChoice() bool { return len(f.options) > 0 }

// value is what the field would write.
func (f field) value() string {
	if f.isChoice() {
		if f.choice < 0 || f.choice >= len(f.options) {
			return ""
		}
		return f.options[f.choice]
	}
	return strings.TrimSpace(f.input.Value())
}

// display is the value as the row renders it.
func (f field) display() string {
	if value := f.value(); value != "" {
		return value
	}
	if f.isChoice() {
		return unsetLabel
	}
	return ""
}

// form is a list of fields with one of them active.
type form struct {
	title  string
	fields []field
	active int
	// footer is the one line under the fields that says where the form's
	// answer will be written. A form whose destination is only visible on the
	// confirm dialog asks the user to commit before telling them where.
	footer string
}

// newTextField builds a text row.
func newTextField(name, label, help, placeholder, value string) field {
	input := textinput.New()
	input.CharLimit = 240
	input.Prompt = ""
	input.Placeholder = placeholder
	input.SetValue(value)
	return field{name: name, label: label, help: help, input: input}
}

// newChoiceField builds a closed-choice row, positioned on the current value.
func newChoiceField(name, label, help string, options []string, current string) field {
	f := field{name: name, label: label, help: help, options: options}
	for i, option := range options {
		if option == current {
			f.choice = i
			break
		}
	}
	return f
}

// visible reports whether a row takes part in navigation.
func (f *form) visible(i int) bool {
	return i >= 0 && i < len(f.fields) && !f.fields[i].hidden
}

// move steps the cursor to the next visible row in a direction, wrapping.
func (f *form) move(delta int) {
	if len(f.fields) == 0 {
		return
	}
	for i := 0; i < len(f.fields); i++ {
		f.active = (f.active + delta + len(f.fields)) % len(f.fields)
		if f.visible(f.active) {
			break
		}
	}
	f.focusActive()
}

// focusActive puts the text cursor in the active row, and only there.
func (f *form) focusActive() {
	for i := range f.fields {
		if i == f.active && !f.fields[i].isChoice() {
			f.fields[i].input.Focus()
			continue
		}
		f.fields[i].input.Blur()
	}
}

// activeField is the row under the cursor.
func (f *form) activeField() *field {
	if f.active < 0 || f.active >= len(f.fields) {
		return nil
	}
	return &f.fields[f.active]
}

// cycle moves a choice row one step. It does nothing on a text row, where the
// arrow keys belong to the text cursor.
func (f *form) cycle(delta int) {
	active := f.activeField()
	if active == nil || !active.isChoice() {
		return
	}
	active.choice = (active.choice + delta + len(active.options)) % len(active.options)
}

// set applies a value chosen in the picker.
func (f *form) set(name, value string) {
	for i := range f.fields {
		if f.fields[i].name != name {
			continue
		}
		for j, option := range f.fields[i].options {
			if option == value {
				f.fields[i].choice = j
				return
			}
		}
	}
}

// update forwards a message to the active text row.
func (f *form) update(msg tea.Msg) tea.Cmd {
	active := f.activeField()
	if active == nil || active.isChoice() {
		return nil
	}
	var cmd tea.Cmd
	active.input, cmd = active.input.Update(msg)
	return cmd
}

// value returns a field's value by name.
func (f *form) value(name string) string {
	for i := range f.fields {
		if f.fields[i].name == name {
			return f.fields[i].value()
		}
	}
	return ""
}

// setHidden shows or hides a row, and moves the cursor off it when it goes.
func (f *form) setHidden(name string, hidden bool) {
	for i := range f.fields {
		if f.fields[i].name != name {
			continue
		}
		f.fields[i].hidden = hidden
		if hidden && f.active == i {
			f.move(1)
		}
		return
	}
}

// pickerOptions renders a choice row's options for the picker dialog, with the
// empty one spelled out.
func (f field) pickerOptions() []string {
	options := make([]string, 0, len(f.options))
	for _, option := range f.options {
		if option == "" {
			option = unsetLabel
		}
		options = append(options, option)
	}
	return options
}

// fromPickerLabel turns what the picker returned back into a value.
func fromPickerLabel(label string) string {
	if label == unsetLabel {
		return ""
	}
	return label
}

// view renders the form as a dialog.
func (f form) view(t theme.Theme, width, height int) string {
	inner := min(max(width-8, 34), 76)
	labelWidth := 0
	for i, row := range f.fields {
		if !f.visible(i) {
			continue
		}
		labelWidth = max(labelWidth, len(row.label))
	}
	labelWidth = min(labelWidth, max(inner-24, 10))
	valueWidth := max(inner-labelWidth-6, 12)

	lines := []string{t.Title.Render(f.title), ""}
	for i, row := range f.fields {
		if !f.visible(i) {
			continue
		}
		label := t.Muted.Render(ui.Pad(ui.Truncate(row.label, labelWidth), labelWidth))
		var value string
		switch {
		case row.isChoice():
			value = renderChoice(t, row.display(), i == f.active, valueWidth)
		case i == f.active:
			input := row.input
			input.Width = valueWidth - 2
			value = input.View()
		default:
			value = t.Base.Render(ui.Truncate(row.display(), valueWidth))
		}
		marker := "  "
		if i == f.active {
			marker = t.Accent.Render("> ")
		}
		lines = append(lines, marker+label+"  "+value)
	}

	if active := f.fields[min(f.active, max(len(f.fields)-1, 0))]; active.help != "" {
		lines = append(lines, "")
		for _, line := range wrapText(active.help, inner-4) {
			lines = append(lines, t.Muted.Render(line))
		}
	}
	if f.footer != "" {
		lines = append(lines, "", t.Muted.Render(ui.Truncate(f.footer, inner-4)))
	}
	lines = append(lines, "",
		t.Key.Render("tab")+t.KeyDesc.Render(" next  ")+
			t.Key.Render("←/→")+t.KeyDesc.Render(" change  ")+
			t.Key.Render("space")+t.KeyDesc.Render(" list  ")+
			t.Key.Render("enter")+t.KeyDesc.Render(" review  ")+
			t.Key.Render("esc")+t.KeyDesc.Render(" cancel"))

	box := t.Dialog.Width(inner).Render(strings.Join(lines, "\n"))
	return placeCenter(box, width, height)
}

// renderChoice draws a choice row with the arrows that say it cycles.
func renderChoice(t theme.Theme, value string, active bool, width int) string {
	value = ui.Truncate(value, width-4)
	if active {
		return t.Accent.Render("‹ ") + t.Base.Render(value) + t.Accent.Render(" ›")
	}
	return t.Base.Render("  " + value)
}

// wrapText breaks a help line to the dialog's width. The kit wraps inside its
// own widgets and does not export it, and a help line that runs off the box is
// help nobody reads.
func wrapText(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// newDropInForm builds the editor for one unit, seeded from the drop-in the
// unit carries today so the form opens on what is true rather than on nothing.
func newDropInForm(unit systemd.Unit, current map[string]string) form {
	f := form{
		title:  "Override properties of " + unit.Name,
		footer: "Written to " + systemd.DropInPathFor(unit.Name),
	}
	for _, property := range systemd.PropertiesFor(unit.Type()) {
		value := current[property.Name]
		if property.Closed() {
			f.fields = append(f.fields, newChoiceField(property.Name,
				property.Name, property.Help, property.Options, value))
			continue
		}
		f.fields = append(f.fields, newTextField(property.Name, property.Name,
			property.Help, property.Placeholder, value))
	}
	f.fields = append(f.fields, newChoiceField(fieldRestart, "Restart now",
		"systemd re-reads the file either way. A unit that is already running "+
			"keeps the settings it started with until it is restarted, which "+
			"drops its current connections.",
		[]string{answerNo, answerYes}, answerNo))
	f.focusActive()
	return f
}

// The fields of the two forms that are not systemd properties.
const (
	fieldRestart     = "restart"
	fieldName        = "name"
	fieldKind        = "kind"
	fieldDescription = "description"
	fieldExecStart   = "ExecStart"
	fieldUser        = "user"
	fieldEnableNow   = "enable"
)

// newUnitForm builds the create form. The kind comes first because it decides
// whether the calendar row exists at all.
func newUnitForm() form {
	f := form{
		title:  "Create a unit",
		footer: "Written to " + systemd.UnitDir + "/",
	}
	calendar, _ := systemd.PropertyNamed("OnCalendar")
	f.fields = []field{
		newChoiceField(fieldKind, "Kind",
			"A service runs when something starts it. A timer writes both the "+
				"service and the timer that starts it on a schedule.",
			[]string{string(systemd.KindService), string(systemd.KindTimer)},
			string(systemd.KindService)),
		newTextField(fieldName, "Name",
			"The unit name without its suffix: letters, digits, dot, dash and "+
				"underscore. tui-systemd never overwrites a unit that exists.",
			"backup-nightly", ""),
		newTextField(fieldDescription, "Description",
			"The one line `systemctl status` shows for the unit.",
			"Nightly off-site backup", ""),
		newTextField(fieldExecStart, "ExecStart",
			"The command the service runs. The program is an absolute path — "+
				"/usr/bin/rsync, not rsync — and arguments may follow it. "+
				"There is no shell: systemd splits the line itself.",
			"/usr/bin/rsync -a /srv/ backup:/srv/", ""),
		newTextField("OnCalendar", "OnCalendar", calendar.Help,
			calendar.Placeholder, ""),
		newTextField(fieldUser, "User",
			"The account the service runs as. Empty means root.", "root", ""),
		newChoiceField(fieldEnableNow, "Enable now",
			"Enabling starts the unit now and again at every boot. It is a "+
				"second confirmation, after the files are written.",
			[]string{answerNo, answerYes}, answerNo),
	}
	f.setHidden("OnCalendar", true)
	f.focusActive()
	return f
}

// syncKind shows or hides the calendar row to match the selected kind.
func (f *form) syncKind() {
	f.setHidden("OnCalendar", f.value(fieldKind) != string(systemd.KindTimer))
}

// dropInValues is what the drop-in form would write, by property name.
func (f *form) dropInValues() map[string]string {
	values := map[string]string{}
	for i := range f.fields {
		name := f.fields[i].name
		if _, ok := systemd.PropertyNamed(name); !ok {
			continue
		}
		if value := f.fields[i].value(); value != "" {
			values[name] = value
		}
	}
	return values
}

// newUnitRequest is what the create form would build, before validation.
//
// A unit with no description reads as its own file name in `systemctl status`,
// which is no worse than what a hand-written unit usually says, so an empty
// description falls back to the name rather than blocking the form.
func (f *form) newUnitRequest(existing []string) systemd.NewUnitRequest {
	description := f.value(fieldDescription)
	if description == "" {
		description = f.value(fieldName)
	}
	return systemd.NewUnitRequest{
		Name:        f.value(fieldName),
		Kind:        systemd.UnitKind(f.value(fieldKind)),
		Description: description,
		ExecStart:   f.value(fieldExecStart),
		OnCalendar:  f.value("OnCalendar"),
		User:        f.value(fieldUser),
		EnableNow:   f.value(fieldEnableNow) == answerYes,
		Existing:    existing,
	}
}
