package systemd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tui-tools/tui-kit/runner"
)

// This file holds everything tui-systemd can write: the closed set of
// properties a drop-in may carry, the two unit templates a new service or
// timer is rendered from, and the commands that put either on disk.
//
// The recipe is the family's, and it is the same one tui-ssh uses for
// sshd_config: render the whole file, stage it in a private temporary
// directory, hand it to the daemon's own parser, and only then install it with
// `install -m 644` and reload. Nothing is appended to a file that already
// exists, and nothing reaches /etc that systemd has not already read.

// UnitDir is where a unit written by this tool lands. It is the
// administrator's directory: a distribution's own units live in /usr/lib and
// are never touched.
const UnitDir = "/etc/systemd/system"

// DropInFile is the name of the drop-in this tool owns. The 90 prefix puts it
// after anything a package ships, and the name says who rewrites it.
const DropInFile = "90-tui-systemd.conf"

// FileMode and DirMode are what `install` is asked for. A unit file is read by
// PID 1 and by everyone else on the machine; it is not a place for a secret,
// which is what the Environment help says on the form.
const (
	FileMode = "0644"
	DirMode  = "0755"
)

// unitNameRe is the shape of a full unit name. It is the gate every unit name
// passes before it reaches an argv or a path, whether it came from the machine
// or from the form.
var unitNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:+-]{0,127}\.` +
	`(service|timer|socket|target|mount|path|slice|scope|swap|automount)$`)

// baseNameRe is the shape of the name typed on the new-unit form: the unit
// name without its suffix.
var baseNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// stagedRe accepts a staging path this package built. It is our own value and
// it is checked anyway, because it ends up in an argv.
var stagedRe = regexp.MustCompile(`^/[A-Za-z0-9@._:+/-]+$`)

// fileNameRe accepts a file name read out of `systemctl cat`. Those names come
// from the machine's filesystem, and a staging directory is assembled from
// them, so they are checked like everything else that makes that trip.
var fileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:+-]{0,127}$`)

// ValidUnitName reports whether a name is one this tool will put in a command
// line or a path.
func ValidUnitName(name string) bool { return unitNameRe.MatchString(name) }

// DropInDirFor is the drop-in directory of a unit.
func DropInDirFor(unit string) string { return UnitDir + "/" + unit + ".d" }

// DropInPathFor is the file this tool writes for a unit.
func DropInPathFor(unit string) string { return DropInDirFor(unit) + "/" + DropInFile }

// UnitPathFor is where a unit this tool creates is installed.
func UnitPathFor(name string) string { return UnitDir + "/" + name }

// Property is one setting the drop-in editor offers.
//
// The set is closed on purpose. A drop-in can say anything a unit file can,
// and an editor that offered all of it would be a worse vi with none of its
// power; what this one is for is the handful of properties an operator reaches
// for at three in the morning — make it come back, cap what it can eat, tell
// it when to run — each offered with the values systemd actually accepts, so a
// typo cannot reach /etc.
type Property struct {
	// Section is the ini section the property belongs to.
	Section string
	// Name is the property name, in systemd's own spelling.
	Name string
	// Help explains the property on the form, in one or two lines.
	Help string
	// Options is the closed value set, empty when the value is free text.
	// The first entry is always "", which means "do not set it at all".
	Options []string
	// Pattern is what a free-text value must match.
	Pattern *regexp.Regexp
	// Placeholder is the example shown in an empty text field.
	Placeholder string
	// Types are the unit types that offer the property.
	Types []string
	// check is an extra test a pattern cannot express (a numeric range).
	check func(string) error
}

// Closed reports whether the property has a fixed value set.
func (p Property) Closed() bool { return len(p.Options) > 0 }

// Applies reports whether the property is offered for a unit type.
func (p Property) Applies(unitType string) bool {
	for _, t := range p.Types {
		if t == unitType {
			return true
		}
	}
	return false
}

// Check reports whether a value may be written for this property. The empty
// value always may: it means the property is left out of the file, which is
// how an override is removed.
func (p Property) Check(value string) error {
	if value == "" {
		return nil
	}
	if p.Closed() {
		for _, option := range p.Options {
			if option == value {
				return nil
			}
		}
		return fmt.Errorf("%s does not accept %q", p.Name, value)
	}
	if p.Pattern != nil && !p.Pattern.MatchString(value) {
		return fmt.Errorf("%s does not accept %q: %s", p.Name, value, p.Help)
	}
	if p.check != nil {
		return p.check(value)
	}
	return nil
}

// Properties is the closed set the drop-in editor writes, in the order the
// form shows them and the file carries them.
//
// Every pattern here refuses a newline, a quote and a backslash, because the
// value is written into an ini file this tool then asks systemd to parse: a
// value that could carry a line break could carry a directive nobody
// confirmed. The tests assert exactly that.
var Properties = []Property{
	{
		Section: "Service", Name: "Restart",
		Help:  "When systemd should start the unit again after it exits.",
		Types: []string{"service"},
		Options: []string{"", "no", "on-success", "on-failure", "on-abnormal",
			"on-abort", "on-watchdog", "always"},
	},
	{
		Section: "Service", Name: "RestartSec",
		Help: "How long to wait before restarting: a number of seconds, " +
			"or a systemd time span like 500ms, 30s, 5min.",
		Types:       []string{"service"},
		Placeholder: "5s",
		Pattern:     regexp.MustCompile(`^[0-9]{1,6}(ms|s|sec|min|m|h)?$`),
	},
	{
		Section: "Service", Name: "MemoryMax",
		Help: "The hard memory limit: a size like 512M or 2G, or infinity " +
			"for no limit. The unit is killed when it goes over.",
		Types:       []string{"service"},
		Placeholder: "512M",
		Pattern:     regexp.MustCompile(`^([0-9]{1,9}(K|M|G|T)?|infinity)$`),
	},
	{
		Section: "Service", Name: "CPUQuota",
		Help: "The share of one CPU the unit may use, as a percentage. " +
			"200% means two cores' worth.",
		Types:       []string{"service"},
		Placeholder: "50%",
		Pattern:     regexp.MustCompile(`^[0-9]{1,6}%$`),
	},
	{
		Section: "Service", Name: "Environment",
		Help: "One KEY=VALUE pair for the unit's environment. This file is " +
			"world-readable, so it is not a place for a password or a token: " +
			"point the unit at a file with EnvironmentFile= instead.",
		Types:       []string{"service"},
		Placeholder: "LOG_LEVEL=debug",
		Pattern: regexp.MustCompile(
			`^[A-Za-z_][A-Za-z0-9_]{0,63}=[A-Za-z0-9 ,.:;/@_+=()\[\]{}?!*^~<>|-]{0,200}$`),
	},
	{
		Section: "Service", Name: "Nice",
		Help:  "The scheduling priority, -20 (most favoured) to 19 (least).",
		Types: []string{"service"},

		Placeholder: "10",
		Pattern:     regexp.MustCompile(`^-?[0-9]{1,2}$`),
		check: func(value string) error {
			n, err := strconv.Atoi(value)
			if err != nil || n < -20 || n > 19 {
				return fmt.Errorf("the priority is between -20 and 19, not %q", value)
			}
			return nil
		},
	},
	{
		Section: "Timer", Name: "OnCalendar",
		Help: "When the timer fires, as a systemd calendar expression: " +
			"daily, Mon..Fri 09:00, *-*-01 02:30:00.",
		Types:       []string{"timer"},
		Placeholder: "daily",
		Pattern:     regexp.MustCompile(`^[A-Za-z0-9 ,:*/.+-]{1,100}$`),
	},
}

// PropertiesFor is the subset of the table a unit type offers. An empty answer
// means the editor has nothing to say about this unit, which is why the key is
// refused on a target or a device rather than opening an empty form.
func PropertiesFor(unitType string) []Property {
	var out []Property
	for _, p := range Properties {
		if p.Applies(unitType) {
			out = append(out, p)
		}
	}
	return out
}

// PropertyNamed finds a property by name.
func PropertyNamed(name string) (Property, bool) {
	for _, p := range Properties {
		if p.Name == name {
			return p, true
		}
	}
	return Property{}, false
}

// EditableUnit reports whether the drop-in editor will open for a unit, and
// why it will not.
//
// Three answers are no. A masked unit is a symlink to /dev/null and a drop-in
// for it would be read by nothing. A transient unit exists only in /run and
// disappears with the manager that made it. And a unit type this tool has no
// properties for — a device, a target — would open an empty form.
func EditableUnit(u Unit) error {
	if !ValidUnitName(u.Name) {
		return fmt.Errorf("%q is not a unit name this tool will write for", u.Name)
	}
	if u.Masked() {
		return fmt.Errorf("%s is masked: unmask it before editing it, "+
			"a drop-in for a masked unit is read by nothing", u.Name)
	}
	if strings.HasPrefix(u.FileState, "transient") {
		return fmt.Errorf("%s is transient: it lives in /run and disappears "+
			"with the manager that created it, so there is nothing to edit",
			u.Name)
	}
	if len(PropertiesFor(u.Type())) == 0 {
		return fmt.Errorf("this editor has nothing to offer a %s unit; "+
			"it edits services and timers", u.Type())
	}
	return nil
}

// dropInHeader is the banner the generated drop-in always carries. It names
// the tool and states the one rule a reader of the file has to know.
const dropInHeader = "# Written by tui-systemd, and rewritten whole on every change.\n" +
	"# Only the properties tui-systemd offers appear here; anything else you\n" +
	"# add by hand is lost the next time this file is written.\n"

// RenderDropIn produces the text of the drop-in for a unit: every property the
// form holds a value for, in the table's order, under its section.
//
// The whole file is regenerated rather than edited in place, because a file
// that grew by appending would end up saying Restart= twice and systemd would
// take the last one — which is exactly the confusion this tool exists to
// remove. A property with an empty value is left out, and that is how an
// override is removed.
func RenderDropIn(unitType string, values map[string]string) (string, error) {
	properties := PropertiesFor(unitType)
	if len(properties) == 0 {
		return "", fmt.Errorf("systemd: nothing is editable on a %s unit", unitType)
	}
	// A value for a property this unit type does not offer is a bug in the
	// caller, and it would silently do nothing: say so instead.
	for name := range values {
		property, ok := PropertyNamed(name)
		if !ok {
			return "", fmt.Errorf("systemd: %q is not a property this tool writes", name)
		}
		if !property.Applies(unitType) {
			return "", fmt.Errorf("systemd: %s does not apply to a %s unit",
				name, unitType)
		}
	}

	var b strings.Builder
	b.WriteString(dropInHeader)
	section := ""
	for _, property := range properties {
		value := strings.TrimSpace(values[property.Name])
		if value == "" {
			continue
		}
		if err := property.Check(value); err != nil {
			return "", err
		}
		if property.Section != section {
			section = property.Section
			fmt.Fprintf(&b, "\n[%s]\n", section)
		}
		fmt.Fprintf(&b, "%s=%s\n", property.Name, renderValue(property, value))
	}
	return b.String(), nil
}

// renderValue quotes the values that need it. Environment= is the only one:
// its value may contain spaces, and systemd splits an unquoted assignment on
// them. The pattern already refuses a quote, so quoting cannot be escaped out
// of.
func renderValue(p Property, value string) string {
	if p.Name == "Environment" {
		return `"` + value + `"`
	}
	return value
}

// ParseDropIn reads the values back out of a file this tool wrote, so the form
// opens on what the unit says today rather than on nothing.
//
// It is deliberately forgiving: a line it does not recognise is skipped, and a
// property that is not in the table is ignored. The file is regenerated whole
// on write, so anything it drops here it would drop there too — which is what
// the banner in the file says.
func ParseDropIn(content string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") ||
			strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		property, known := PropertyNamed(name)
		if !known {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.TrimSuffix(strings.TrimPrefix(value, `"`), `"`)
		if property.Check(value) != nil {
			continue
		}
		values[name] = value
	}
	return values
}

// CatSection is one file of `systemctl cat`: its path and its contents.
type CatSection struct {
	Path string
	Body string
}

// ParseCat splits `systemctl cat` output into the files it concatenated.
//
// The output is the unit's own fragment followed by every drop-in, each
// introduced by a comment naming its absolute path. That structure is what
// lets the editor seed itself from the drop-in already on disk, and what lets
// the staging directory be assembled as a faithful copy of what systemd reads.
func ParseCat(out string) []CatSection {
	var sections []CatSection
	var current *CatSection
	var body []string
	flush := func() {
		if current == nil {
			return
		}
		current.Body = strings.TrimLeft(strings.Join(body, "\n"), "\n")
		if !strings.HasSuffix(current.Body, "\n") && current.Body != "" {
			current.Body += "\n"
		}
		sections = append(sections, *current)
		current, body = nil, nil
	}
	for _, line := range strings.Split(out, "\n") {
		if path, ok := catHeader(line); ok {
			flush()
			current = &CatSection{Path: path}
			continue
		}
		if current == nil {
			continue
		}
		body = append(body, line)
	}
	flush()
	return sections
}

// catHeader recognises the "# /path/to/file" line systemctl prints before each
// file. A comment that is not an absolute path is a comment in the unit.
func catHeader(line string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "# ")
	if !ok || !strings.HasPrefix(rest, "/") || strings.ContainsAny(rest, " \t") {
		return "", false
	}
	return rest, true
}

// DropInIn returns the body of this tool's drop-in inside `systemctl cat`
// output, or "" when the unit has none yet.
func DropInIn(unit, out string) string {
	want := DropInPathFor(unit)
	for _, section := range ParseCat(out) {
		if section.Path == want {
			return section.Body
		}
	}
	return ""
}

// StagedFile is one file of a write plan: what it will contain, where it goes,
// and where it is staged in the meantime.
type StagedFile struct {
	// Path is the destination under /etc/systemd/system.
	Path string
	// Content is the whole text that will be installed.
	Content string
	// TempPath is the staged copy `install` will copy from.
	TempPath string
	// Diff is the unified diff against what is on disk today.
	Diff string
}

// WritePlan is a file change the user has not confirmed yet: the files it
// writes, what the syntax check made of them, and the commands that apply it.
//
// It is the same shape tui-ssh uses, for the same reason: everything the
// confirm dialog needs to be honest is decided before the dialog opens, so the
// answer to "what will this do" is a value rather than a promise.
type WritePlan struct {
	// Title is the dialog's title.
	Title string
	// Files are the files the plan writes, in install order.
	Files []StagedFile
	// Validated reports that systemd's own parser accepted the staged files.
	Validated bool
	// Validation says what the check made of them, accepted or not.
	Validation string
	// ValidationCommand is the exact command line the check ran.
	ValidationCommand string
	// Warning is what the dialog must say beyond the diff.
	Warning string
	// Stage is the temporary directory holding the staged files. It has to
	// outlive the dialog, because the install command copies from it, so the
	// caller removes it once the plan has run or been cancelled.
	Stage string
	// Commands apply the plan, in order.
	Commands []runner.Command
	// Follow is an optional second step — restart the unit, enable the timer —
	// which gets its own preview and its own confirmation.
	Follow *runner.Command
	// FollowBody explains the second step on its dialog.
	FollowBody string
}

// Diffs joins the diff of every file, for the dialog.
func (p WritePlan) Diffs() string {
	parts := make([]string, 0, len(p.Files))
	for _, file := range p.Files {
		if file.Diff != "" {
			parts = append(parts, file.Diff)
		}
	}
	return strings.Join(parts, "\n")
}

// DropInRequest is what the drop-in editor asks for.
type DropInRequest struct {
	// Unit is the unit being edited, as the last read reported it.
	Unit Unit
	// Cat is the `systemctl cat` output for the unit: it seeds the diff and
	// it is what the staging directory is built from.
	Cat string
	// Values are the properties the form holds, by property name. An empty
	// value removes the override.
	Values map[string]string
	// Restart asks for the optional second step.
	Restart bool
}

// UnitKind is what the new-unit form creates.
type UnitKind string

// The two kinds the templates cover.
const (
	KindService UnitKind = "service"
	KindTimer   UnitKind = "timer"
)

// NewUnitRequest is what the new-unit form asks for.
type NewUnitRequest struct {
	// Name is the unit name without its suffix.
	Name string
	// Kind is service, or timer — which writes a service and the timer that
	// starts it, because a timer without a unit to activate does nothing.
	Kind UnitKind
	// Description is the unit's one-line description.
	Description string
	// ExecStart is the command the service runs. The program must be an
	// absolute path; arguments are allowed.
	ExecStart string
	// OnCalendar is when a timer fires.
	OnCalendar string
	// User is the account the service runs as; empty means root.
	User string
	// EnableNow asks for the optional second step.
	EnableNow bool
	// Existing is every unit name the last read reported, so a plan that
	// would overwrite one is refused before anything is staged.
	Existing []string
}

// ServiceName and TimerName are the units a request creates.
func (r NewUnitRequest) ServiceName() string { return r.Name + ".service" }
func (r NewUnitRequest) TimerName() string   { return r.Name + ".timer" }

// Target is the unit `enable --now` would act on: the timer when there is one,
// because enabling the service it starts would defeat the schedule.
func (r NewUnitRequest) Target() string {
	if r.Kind == KindTimer {
		return r.TimerName()
	}
	return r.ServiceName()
}

// execStartRe is the shape of a command line the template will write. The
// program is an absolute path — systemd requires it, and requiring it here
// means the error arrives on the form rather than from `verify` — and the
// arguments are ordinary printable text with no shell metacharacter in it.
//
// There is no shell between this string and execve: systemd splits it itself.
// The characters refused below are refused because they mean something to
// systemd (% is a specifier, a newline ends the directive), not because
// something downstream would interpret them.
var execStartRe = regexp.MustCompile(
	`^/[A-Za-z0-9._/-]{1,200}( [A-Za-z0-9 ,.:;/@_+=()\[\]{}?!*^~<>|-]{1,400})?$`)

// userRe is the shape of an account name, as useradd enforces it.
var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}\$?$`)

// descriptionRe is one line of ordinary text.
var descriptionRe = regexp.MustCompile(`^[^\n\r\x00]{1,120}$`)

// CheckNewUnit reports whether a request may be rendered. It is the whole
// input validation of the new-unit form, in one place the tests can reach.
func CheckNewUnit(req NewUnitRequest) error {
	if !baseNameRe.MatchString(req.Name) {
		return fmt.Errorf("a unit name is letters, digits, dot, dash and "+
			"underscore, and starts with a letter or a digit — not %q", req.Name)
	}
	if req.Kind != KindService && req.Kind != KindTimer {
		return fmt.Errorf("systemd: %q is not a kind this tool creates", req.Kind)
	}
	if !ValidUnitName(req.ServiceName()) {
		return fmt.Errorf("%q would not be a valid unit name", req.ServiceName())
	}
	if !descriptionRe.MatchString(req.Description) {
		return fmt.Errorf("the description is one line of text, up to 120 characters")
	}
	if !execStartRe.MatchString(req.ExecStart) {
		return fmt.Errorf("ExecStart must start with the absolute path of the " +
			"program, /usr/bin/rsync rather than rsync; arguments may follow it")
	}
	if req.User != "" && !userRe.MatchString(req.User) {
		return fmt.Errorf("%q is not an account name", req.User)
	}
	if req.Kind == KindTimer {
		calendar, ok := PropertyNamed("OnCalendar")
		if !ok {
			return fmt.Errorf("systemd: OnCalendar is missing from the property table")
		}
		if req.OnCalendar == "" {
			return fmt.Errorf("a timer needs an OnCalendar expression")
		}
		if err := calendar.Check(req.OnCalendar); err != nil {
			return err
		}
	}
	for _, name := range req.Existing {
		if name == req.ServiceName() || (req.Kind == KindTimer && name == req.TimerName()) {
			return fmt.Errorf("%s already exists: this tool creates units, it "+
				"never overwrites one — edit it with the drop-in editor instead",
				name)
		}
	}
	return nil
}

// unitHeader is the banner every unit this tool writes carries.
const unitHeader = "# Written by tui-systemd. Edit the properties it offers " +
	"with E on the unit,\n# or edit this file by hand — tui-systemd only " +
	"rewrites the file it created.\n\n"

// RenderUnits renders the files a request creates, in install order: the
// service, and for a timer the timer that starts it.
//
// The templates are deliberately short. Everything in them is either required
// by systemd or is the answer an operator would give anyway — Restart on a
// long-running service, Persistent on a timer so a machine that was off
// catches up — and nothing in them is a value the form did not ask for.
func RenderUnits(req NewUnitRequest) ([]StagedFile, error) {
	if err := CheckNewUnit(req); err != nil {
		return nil, err
	}

	var service strings.Builder
	service.WriteString(unitHeader)
	service.WriteString("[Unit]\n")
	fmt.Fprintf(&service, "Description=%s\n", req.Description)
	service.WriteString("\n[Service]\n")
	if req.Kind == KindTimer {
		// A timer starts a job that runs and ends; oneshot is what says so,
		// and it is what makes the timer's own state readable.
		service.WriteString("Type=oneshot\n")
	} else {
		service.WriteString("Type=simple\n")
	}
	fmt.Fprintf(&service, "ExecStart=%s\n", req.ExecStart)
	if req.User != "" {
		fmt.Fprintf(&service, "User=%s\n", req.User)
	}
	if req.Kind == KindService {
		service.WriteString("Restart=on-failure\nRestartSec=5s\n")
	}
	if req.Kind == KindService {
		// A service started by a timer has no [Install] section: the timer is
		// what gets enabled, and a service that could also be enabled on its
		// own would run twice.
		service.WriteString("\n[Install]\nWantedBy=multi-user.target\n")
	}

	files := []StagedFile{{Path: UnitPathFor(req.ServiceName()), Content: service.String()}}
	if req.Kind != KindTimer {
		return files, nil
	}

	var timer strings.Builder
	timer.WriteString(unitHeader)
	timer.WriteString("[Unit]\n")
	fmt.Fprintf(&timer, "Description=%s (schedule)\n", req.Description)
	timer.WriteString("\n[Timer]\n")
	fmt.Fprintf(&timer, "OnCalendar=%s\n", req.OnCalendar)
	// A machine that was off when the timer was due runs the job at the next
	// boot rather than skipping it.
	timer.WriteString("Persistent=true\n")
	fmt.Fprintf(&timer, "Unit=%s\n", req.ServiceName())
	timer.WriteString("\n[Install]\nWantedBy=timers.target\n")

	return append(files, StagedFile{
		Path: UnitPathFor(req.TimerName()), Content: timer.String()}), nil
}

// StageDir is a private temporary directory holding the files of a plan
// exactly as systemd will read them: the unit, and its drop-ins in a `.d`
// directory beside it.
//
// This layout is the reason the check means anything. `systemd-analyze verify`
// on a path loads the `<name>.d/*.conf` next to it, so what the user confirms
// is a file systemd has already parsed in the company of the drop-ins it will
// live with — not a fragment checked on its own.
type StageDir struct {
	// Dir is the temporary directory; the caller removes it when done.
	Dir string
	// Verify are the unit paths inside it to hand to `systemd-analyze verify`.
	Verify []string
}

// stagePrefix names the temporary directories this tool makes, so a leftover
// one on a machine that was killed mid-dialog is recognisable.
const stagePrefix = "tui-systemd-"

// StageNewUnit writes the files of a new-unit plan to a temporary directory
// and returns where each landed.
func StageNewUnit(files []StagedFile) (StageDir, []StagedFile, error) {
	dir, err := os.MkdirTemp("", stagePrefix)
	if err != nil {
		return StageDir{}, nil, err
	}
	stage := StageDir{Dir: dir}
	staged := make([]StagedFile, 0, len(files))
	for _, file := range files {
		name := filepath.Base(file.Path)
		if !fileNameRe.MatchString(name) {
			return stage, nil, fmt.Errorf("systemd: %q is not a file name", name)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(file.Content), 0o600); err != nil {
			return stage, nil, err
		}
		file.TempPath = path
		file.Diff = Diff(file.Path, "", file.Content)
		staged = append(staged, file)
		stage.Verify = append(stage.Verify, path)
	}
	return stage, staged, nil
}

// StageDropIn writes a copy of everything systemd reads for a unit to a
// temporary directory, with the new drop-in in place of the old one, and
// returns the staged drop-in.
//
// The unit's own fragment and the drop-ins this tool did not write are copied
// verbatim, because a check that saw only our file would not see the
// dependency it contradicts.
func StageDropIn(unit, catOutput, content string) (StageDir, StagedFile, error) {
	if !ValidUnitName(unit) {
		return StageDir{}, StagedFile{}, fmt.Errorf("%q is not a unit name", unit)
	}
	dir, err := os.MkdirTemp("", stagePrefix)
	if err != nil {
		return StageDir{}, StagedFile{}, err
	}
	stage := StageDir{Dir: dir}
	dropInDir := filepath.Join(dir, unit+".d")
	if err := os.MkdirAll(dropInDir, 0o700); err != nil {
		return stage, StagedFile{}, err
	}

	destination := DropInPathFor(unit)
	before := ""
	wroteFragment := false
	for _, section := range ParseCat(catOutput) {
		name := filepath.Base(section.Path)
		if !fileNameRe.MatchString(name) {
			continue
		}
		switch {
		case section.Path == destination:
			// This is the file being replaced: it is the left side of the
			// diff, and the copy in the staging directory is the new text.
			before = section.Body
		case name == unit && !wroteFragment:
			if err := os.WriteFile(filepath.Join(dir, name),
				[]byte(section.Body), 0o600); err != nil {
				return stage, StagedFile{}, err
			}
			wroteFragment = true
		case strings.HasSuffix(name, ".conf"):
			if err := os.WriteFile(filepath.Join(dropInDir, name),
				[]byte(section.Body), 0o600); err != nil {
				return stage, StagedFile{}, err
			}
		}
	}
	if !wroteFragment {
		return stage, StagedFile{}, fmt.Errorf(
			"systemctl cat %s did not show the unit file, so there is "+
				"nothing to check the change against", unit)
	}

	path := filepath.Join(dropInDir, DropInFile)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return stage, StagedFile{}, err
	}
	stage.Verify = []string{filepath.Join(dir, unit)}
	return stage, StagedFile{
		Path:     destination,
		Content:  content,
		TempPath: path,
		Diff:     Diff(destination, before, content),
	}, nil
}

// Existing returns the drop-in text a unit has today, for the caller that
// needs to know whether a plan changes anything.
func Existing(unit, catOutput string) string { return DropInIn(unit, catOutput) }

// BuildVerify asks systemd's own parser to read the staged unit files.
//
// It is a read — verify parses and prints, it changes nothing — so it runs
// before the user is asked anything, which is the whole point of it: a value
// systemd will not accept should be a message on the form, not a unit that
// fails to load after the file is already in /etc.
func BuildVerify(paths []string) (runner.Command, error) {
	if len(paths) == 0 {
		return runner.Command{}, fmt.Errorf("systemd: nothing to verify")
	}
	argv := []string{"systemd-analyze", "verify"}
	for _, path := range paths {
		if !stagedRe.MatchString(path) {
			return runner.Command{}, fmt.Errorf("systemd: %q is not a staging path", path)
		}
		argv = append(argv, path)
	}
	return runner.Command{
		Argv:        argv,
		Description: "Check the staged files with systemd's own parser",
	}, nil
}

// BuildCalendar asks systemd to parse a calendar expression, which is how the
// timer form knows an expression is good before it writes it.
func BuildCalendar(expression string) (runner.Command, error) {
	calendar, ok := PropertyNamed("OnCalendar")
	if !ok {
		return runner.Command{}, fmt.Errorf("systemd: OnCalendar is missing from the table")
	}
	if err := calendar.Check(expression); err != nil {
		return runner.Command{}, err
	}
	if expression == "" {
		return runner.Command{}, fmt.Errorf("systemd: no calendar expression given")
	}
	return runner.Command{
		Argv:        []string{"systemd-analyze", "calendar", expression},
		Description: "Check the calendar expression " + expression,
	}, nil
}

// BuildInstallDir creates the drop-in directory. `install -d` rather than
// mkdir, because it sets the mode in the same call and does not fail on a
// directory that is already there.
func BuildInstallDir(dir string) (runner.Command, error) {
	if !stagedRe.MatchString(dir) || !strings.HasPrefix(dir, UnitDir+"/") {
		return runner.Command{}, fmt.Errorf("systemd: %q is not a directory this tool creates", dir)
	}
	return runner.Command{
		Argv:        []string{"install", "-d", "-m", DirMode, dir},
		Description: "Create " + dir,
	}, nil
}

// BuildInstallFile copies a staged file into place. `install` rather than cp,
// because it sets the mode in the same call, so there is no window where the
// file exists with the wrong one.
func BuildInstallFile(file StagedFile) (runner.Command, error) {
	if !stagedRe.MatchString(file.TempPath) {
		return runner.Command{}, fmt.Errorf("systemd: %q is not a staging path", file.TempPath)
	}
	if !stagedRe.MatchString(file.Path) || !strings.HasPrefix(file.Path, UnitDir+"/") {
		return runner.Command{}, fmt.Errorf("systemd: %q is not a path this tool writes", file.Path)
	}
	return runner.Command{
		Argv:        []string{"install", "-m", FileMode, file.TempPath, file.Path},
		Description: "Install " + file.Path,
		Destructive: true,
	}, nil
}

// BuildDaemonReload tells systemd to re-read the unit files from disk. Nothing
// this tool writes takes effect before it runs.
func BuildDaemonReload() runner.Command {
	return runner.Command{
		Argv:        []string{"systemctl", "daemon-reload"},
		Description: "Reload the systemd manager so it re-reads the unit files",
	}
}

// BuildUnitAction is the one-unit systemctl invocation the optional second
// step of a plan runs: restart after an edit, `enable --now` after a create.
func BuildUnitAction(unit string, argv ...string) (runner.Command, error) {
	if !ValidUnitName(unit) {
		return runner.Command{}, fmt.Errorf("%q is not a unit name", unit)
	}
	return runner.Command{
		Argv:        append(append([]string{"systemctl"}, argv...), unit),
		Description: strings.Join(argv, " ") + " " + unit,
		Destructive: true,
	}, nil
}

// BuildCat is the read behind the unit-file viewer: the unit as systemd
// assembles it, its fragment and every drop-in, each named by its path.
func BuildCat(unit string) ([]string, error) {
	if !ValidUnitName(unit) {
		return nil, fmt.Errorf("%q is not a unit name", unit)
	}
	return []string{"systemctl", "cat", unit, "--no-pager"}, nil
}

// SortedNames returns the unit names of a list, for a request that has to
// refuse a name already taken.
func SortedNames(units []Unit) []string {
	names := make([]string, 0, len(units))
	for _, u := range units {
		names = append(names, u.Name)
	}
	sort.Strings(names)
	return names
}
