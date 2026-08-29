// Package systemd holds the model tui-systemd renders, the actions it can
// perform, and the interface every backend satisfies. The UI knows only these
// types: it never assembles a systemctl command line itself. Mutations are
// runner.Command values produced by the backend, shown in a preview dialog and
// only then executed.
package systemd

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/tui-tools/tui-kit/runner"
)

// Unit is one systemd unit, merged from `list-units` (its runtime state) and
// `list-unit-files` (whether it starts at boot).
type Unit struct {
	// Name is the full unit name, "sshd.service".
	Name string
	// Load is the load state: loaded, not-found, bad-setting, error, masked.
	Load string
	// Active is the high-level state: active, reloading, inactive, failed,
	// activating, deactivating.
	Active string
	// Sub is the type-specific state: running, exited, dead, failed, waiting.
	Sub string
	// Description is the unit's own one-line description.
	Description string
	// FileState comes from list-unit-files: enabled, disabled, static,
	// masked, generated, indirect… It is empty for a unit with no unit file
	// (a device or a mount systemd generated at runtime).
	FileState string
	// Preset is the vendor preset, when the unit file declares one.
	Preset string
}

// The Active states worth naming in code.
const (
	ActiveActive   = "active"
	ActiveInactive = "inactive"
	ActiveFailed   = "failed"
)

// Type returns the unit suffix without the dot: "service", "timer", "socket".
func (u Unit) Type() string {
	if i := strings.LastIndexByte(u.Name, '.'); i >= 0 && i < len(u.Name)-1 {
		return u.Name[i+1:]
	}
	return ""
}

// Failed reports whether the unit is in the failed state.
func (u Unit) Failed() bool { return u.Active == ActiveFailed }

// Running reports whether the unit is active.
func (u Unit) Running() bool { return u.Active == ActiveActive }

// Masked reports whether the unit is masked, which systemd reports through
// either the load state or the unit file state.
func (u Unit) Masked() bool {
	return u.Load == "masked" || strings.HasPrefix(u.FileState, "masked")
}

// Enabled reports whether the unit starts at boot. A unit file that is static
// or generated is not "enabled": it has no [Install] section to enable.
func (u Unit) Enabled() bool { return strings.HasPrefix(u.FileState, "enabled") }

// Timer is one entry of `systemctl list-timers`.
type Timer struct {
	// Next is when the timer fires next; the zero time when it has no
	// scheduled next run.
	Next time.Time
	// Last is when it last fired; the zero time when it never has.
	Last time.Time
	// Unit is the timer unit, Activates the unit it starts.
	Unit      string
	Activates string
}

// BlameEntry is one line of `systemd-analyze blame`: how long a unit took to
// initialize during the last boot.
type BlameEntry struct {
	// Duration is the parsed time; zero when the format was not understood.
	Duration time.Duration
	// Raw is the duration exactly as systemd printed it ("1h 39min 446ms"),
	// which is what the UI shows.
	Raw  string
	Unit string
}

// State filters the unit list by Active state. The zero value shows
// everything, which is what `list-units --all` returns.
type State string

// The state filters the UI cycles through.
const (
	StateAll      State = "all"
	StateActive   State = "active"
	StateFailed   State = "failed"
	StateInactive State = "inactive"
)

// States is the cycle order of the state filter.
var States = []State{StateAll, StateFailed, StateActive, StateInactive}

// Match reports whether a unit passes this state filter.
func (s State) Match(u Unit) bool {
	switch s {
	case StateFailed:
		return u.Failed()
	case StateActive:
		return u.Running()
	case StateInactive:
		return u.Active == ActiveInactive
	default:
		return true
	}
}

// Action is something the user can do to a unit.
type Action string

// The actions v0.1 supports. DaemonReload applies to the manager rather than
// to a unit, and is the only one that takes no unit name.
const (
	Start        Action = "start"
	Stop         Action = "stop"
	Restart      Action = "restart"
	Reload       Action = "reload"
	Enable       Action = "enable"
	Disable      Action = "disable"
	Mask         Action = "mask"
	Unmask       Action = "unmask"
	DaemonReload Action = "daemon-reload"
)

// ActionSpec describes one action for the key map, the help screen and the
// confirm dialog, so the three can never drift apart.
type ActionSpec struct {
	Action Action
	// Key is the key that triggers it.
	Key string
	// Label is the confirm dialog's title, "Restart sshd.service".
	Label string
	// Body explains what will happen, shown above the command preview.
	Body string
	// Destructive marks an action that can take a service down or make one
	// unstartable, so the dialog is painted in the danger color.
	Destructive bool
	// Manager marks an action that takes no unit (daemon-reload).
	Manager bool
}

// Actions is the full action table, in help-screen order.
var Actions = []ActionSpec{
	{Action: Start, Key: "s", Label: "Start",
		Body: "The unit will be started now. This does not change whether it starts at boot."},
	{Action: Stop, Key: "x", Label: "Stop", Destructive: true,
		Body: "The unit will be stopped now. Anything depending on it stops too."},
	{Action: Restart, Key: "r", Label: "Restart", Destructive: true,
		Body: "The unit will be stopped and started again, dropping its current connections."},
	{Action: Reload, Key: "l", Label: "Reload",
		Body: "The unit will re-read its configuration without restarting. Not every unit supports this."},
	{Action: Enable, Key: "e", Label: "Enable",
		Body: "The unit will start at boot. Its current state is not changed."},
	{Action: Disable, Key: "D", Label: "Disable", Destructive: true,
		Body: "The unit will no longer start at boot. It keeps running until stopped."},
	{Action: Mask, Key: "m", Label: "Mask", Destructive: true,
		Body: "The unit will be linked to /dev/null: it cannot be started at all, by anything, until it is unmasked."},
	{Action: Unmask, Key: "M", Label: "Unmask",
		Body: "The mask will be removed, so the unit can be started again."},
	{Action: DaemonReload, Key: "d", Label: "Reload the systemd manager", Manager: true,
		Body: "systemd will re-read every unit file from disk. Running units are not restarted."},
}

// ActionFor returns the spec bound to a key.
func ActionFor(key string) (ActionSpec, bool) {
	for _, spec := range Actions {
		if spec.Key == key {
			return spec, true
		}
	}
	return ActionSpec{}, false
}

// SortUnits orders the list the way the user wants to read it: failed units
// first, because those are the reason anyone opened the tool, then by name.
func SortUnits(units []Unit) {
	sort.SliceStable(units, func(i, j int) bool {
		if units[i].Failed() != units[j].Failed() {
			return units[i].Failed()
		}
		return units[i].Name < units[j].Name
	})
}

// Backend is the boundary between the UI and the machine. The read methods
// return the model; Build turns an intent into a previewable Command; Run
// executes a Command the user confirmed. Nothing else may mutate the system.
type Backend interface {
	// Name identifies the backend ("systemd", "demo").
	Name() string
	// Describe is the one-line summary shown in the header.
	Describe() string

	// Units lists every unit systemd knows, merged with its unit-file state.
	Units(ctx context.Context) ([]Unit, error)
	// Timers lists the timer units and when they fire.
	Timers(ctx context.Context) ([]Timer, error)
	// Blame reports how long each unit took during the last boot, slowest
	// first, capped at limit entries.
	Blame(ctx context.Context, limit int) ([]BlameEntry, error)
	// Journal returns the last n lines of a unit's log.
	Journal(ctx context.Context, unit string, lines int) (string, error)

	// Build turns an action on a unit into a previewable command. A Manager
	// action ignores the unit name.
	Build(spec ActionSpec, unit string) (runner.Command, error)
	// Preview renders the exact command line Run will execute.
	Preview(cmd runner.Command) string
	// Run executes a previously previewed command.
	Run(ctx context.Context, cmd runner.Command) (string, error)
}
