package systemd

import (
	"context"
	"fmt"
	"time"

	"github.com/tui-tools/tui-kit/runner"
)

// Real drives systemd on the host. It satisfies Backend.
//
// Three binaries are involved and each gets its own runner, because each has
// its own resolution and its own failure message: systemctl for the unit list
// and every action, journalctl for the log panel, systemd-analyze for the boot
// view. Only systemctl escalates, and only for actions — every read in this
// tool works as an unprivileged user, which is why the tool opens instantly
// and without a password.
type Real struct {
	systemctl *runner.Runner
	journal   *runner.Runner
	analyze   *runner.Runner
	// journalErr and analyzeErr hold why an optional binary is unusable, so
	// the matching view can say so instead of silently showing nothing.
	journalErr error
	analyzeErr error
}

// readTimeout bounds a read. It is generous because `list-units` on a busy
// machine with hundreds of units is not instant.
const readTimeout = 20 * time.Second

// actionTimeout bounds an action. systemctl blocks until the unit settles, and
// a stubborn service can take its full stop timeout to go down.
const actionTimeout = 120 * time.Second

// unprivileged is the address-of-false the runner options need.
var unprivileged = false

// Available reports whether systemctl is installed on this host.
func Available() bool { return runner.Available("systemctl", "/usr/bin/systemctl") }

// New builds the real backend. sudoPrefix comes from the configuration
// ("sudo -n"); pass nil to run systemctl directly. Only systemctl is required:
// a host without journalctl or systemd-analyze still gets the unit list, and
// the views that need them say what is missing.
func New(sudoPrefix []string) (*Real, error) {
	systemctl, err := runner.New(runner.Options{
		Bin:         "systemctl",
		SearchPaths: []string{"/usr/bin/systemctl", "/bin/systemctl"},
		SudoPrefix:  sudoPrefix,
		Timeout:     actionTimeout,
		// Reads are unprivileged: only the actions need root.
		PrivilegedReads: &unprivileged,
		InstallHint:     "this tool needs systemd; use --demo to explore the UI",
	})
	if err != nil {
		return nil, err
	}

	r := &Real{systemctl: systemctl}
	r.journal, r.journalErr = runner.New(runner.Options{
		Bin:             "journalctl",
		SearchPaths:     []string{"/usr/bin/journalctl", "/bin/journalctl"},
		Timeout:         readTimeout,
		PrivilegedReads: &unprivileged,
	})
	r.analyze, r.analyzeErr = runner.New(runner.Options{
		Bin:             "systemd-analyze",
		SearchPaths:     []string{"/usr/bin/systemd-analyze"},
		Timeout:         readTimeout,
		PrivilegedReads: &unprivileged,
	})
	return r, nil
}

// Name identifies the backend.
func (r *Real) Name() string { return "systemd" }

// Describe is the one-line summary shown in the header.
func (r *Real) Describe() string { return r.systemctl.Describe() }

// Preview renders the exact command line Run will execute.
func (r *Real) Preview(cmd runner.Command) string { return r.systemctl.Preview(cmd) }

// Run executes a previewed command.
func (r *Real) Run(ctx context.Context, cmd runner.Command) (string, error) {
	return r.systemctl.Run(ctx, cmd)
}

// Units reads the runtime list and the unit-file list and merges them.
func (r *Real) Units(ctx context.Context) ([]Unit, error) {
	unitsOut, err := r.systemctl.Read(ctx,
		"systemctl", "list-units", "--all", "--no-pager", "--output=json")
	if err != nil {
		return nil, err
	}
	units, err := ParseUnits(unitsOut)
	if err != nil {
		return nil, err
	}
	// The unit-file list is a bonus: without it the tool still shows every
	// running unit, it just cannot say which ones start at boot.
	filesOut, filesErr := r.systemctl.Read(ctx,
		"systemctl", "list-unit-files", "--no-pager", "--output=json")
	if filesErr != nil {
		SortUnits(units)
		return units, nil
	}
	files, err := ParseUnitFiles(filesOut)
	if err != nil {
		SortUnits(units)
		return units, nil
	}
	return MergeUnits(units, files), nil
}

// Timers reads the timer list.
func (r *Real) Timers(ctx context.Context) ([]Timer, error) {
	out, err := r.systemctl.Read(ctx,
		"systemctl", "list-timers", "--all", "--no-pager", "--output=json")
	if err != nil {
		return nil, err
	}
	return ParseTimers(out)
}

// Blame reads the boot times, slowest first, capped at limit entries.
func (r *Real) Blame(ctx context.Context, limit int) ([]BlameEntry, error) {
	if r.analyzeErr != nil {
		return nil, fmt.Errorf("the boot view needs systemd-analyze: %w", r.analyzeErr)
	}
	out, err := r.analyze.Read(ctx, "systemd-analyze", "blame", "--no-pager")
	if err != nil {
		return nil, err
	}
	entries := ParseBlame(out)
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// Journal reads the last lines of a unit's log.
func (r *Real) Journal(ctx context.Context, unit string, lines int) (string, error) {
	if r.journalErr != nil {
		return "", fmt.Errorf("the journal view needs journalctl: %w", r.journalErr)
	}
	if unit == "" {
		return "", fmt.Errorf("no unit selected")
	}
	if lines <= 0 {
		lines = DefaultJournalLines
	}
	return r.journal.Read(ctx, "journalctl", "-u", unit,
		"-n", fmt.Sprint(lines), "--no-pager")
}

// Build turns an action into a previewable command.
func (r *Real) Build(spec ActionSpec, unit string) (runner.Command, error) {
	return BuildCommand(spec, unit)
}

// DefaultJournalLines is how much backlog the journal panel asks for.
const DefaultJournalLines = 200

// BuildCommand assembles the systemctl invocation for an action. It is shared
// by the real and the fake backend, so the demo previews exactly the command
// the real thing would run.
func BuildCommand(spec ActionSpec, unit string) (runner.Command, error) {
	if spec.Action == "" {
		return runner.Command{}, fmt.Errorf("no action given")
	}
	if spec.Manager {
		return runner.Command{
			Argv:        []string{"systemctl", string(spec.Action)},
			Description: spec.Label,
			Destructive: spec.Destructive,
		}, nil
	}
	if unit == "" {
		return runner.Command{}, fmt.Errorf("no unit selected")
	}
	// `--no-block` is deliberately not used: the user asked for a change and
	// should see whether it took, so systemctl is left to wait for the job.
	return runner.Command{
		Argv:        []string{"systemctl", string(spec.Action), unit},
		Description: spec.Label + " " + unit,
		Destructive: spec.Destructive,
	}, nil
}
