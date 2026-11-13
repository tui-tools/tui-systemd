package systemd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tui-tools/tui-kit/runner"
)

// This file assembles the two write plans. It is shared by the real backend
// and the fake one, so what --demo previews is what a real machine would run,
// down to the argument order — the only difference is who answers the syntax
// check and where the files end up.

// Verifier runs the read-only syntax check of a plan. It returns the command
// line it ran, its output, and whether it could run at all.
//
// It is a function rather than a method so the demo can stand in for it
// honestly: the fake says the check was not run instead of pretending systemd
// accepted a file it never saw.
type Verifier func(ctx context.Context, cmd runner.Command) (preview, output string, err error)

// verify runs the check and records its verdict on the plan.
//
// `systemd-analyze verify` loads the staged units and, through their
// dependencies, a good part of the host's own units, and it prints what it
// thinks of all of them. So the verdict is read line by line, and a line
// counts against the plan when it is about a file being written: it names a
// path inside the staging directory or the name of a unit being written (see
// ownLine). A line that cannot be attributed counts against it too.
//
// A warning about a file being written refuses the plan even when the exit
// status is 0. systemd reports a property it cannot parse as a warning and
// goes on, so that is stricter than systemd itself, and deliberately: a line
// systemd would silently ignore is a line the operator thought they were
// setting.
//
// A line about some other unit — on systemd 259, the CPUAccounting= warnings
// for vendor units nobody here touched — does not refuse anything. It is
// kept on the plan and shown as context, because it is true and it is this
// machine, but it is not about the file being written.
//
// The exit status still counts: a check that failed is a refusal even when
// everything it printed was about other units. With --recursive-errors=no
// (systemd 250 and later, see WithOwnErrorsOnly) a failure means one of the
// units being written failed; without it, it may be a dependency, and the
// plan stays refused rather than guessing.
func verify(ctx context.Context, verifier Verifier, plan *WritePlan,
	paths []string) error {
	cmd, err := BuildVerify(paths)
	if err != nil {
		return err
	}
	preview, out, err := verifier(ctx, cmd)
	plan.ValidationCommand = preview
	out = strings.TrimSpace(out)
	own, host := splitVerifyOutput(out, paths)
	switch {
	case len(own) > 0:
		return fmt.Errorf("systemd refused the file: %s", own[0])
	case err != nil && out == "":
		// The check itself could not run — systemd-analyze is not installed,
		// most likely. That is not a reason to refuse the edit, but it is a
		// reason for the dialog to say the file was never read.
		plan.Validation = "could not run: " + runner.FirstLine(err.Error())
	case err != nil:
		return fmt.Errorf("systemd refused the file: the check failed, and "+
			"what it printed is about other units: %s", host[0])
	default:
		plan.Validated = true
		plan.Validation = "accepted by " + preview
		if len(host) > 0 {
			plan.HostWarnings = host
			plan.Validation += fmt.Sprintf("; it also printed %d %s about "+
				"other units on this machine, not about these files",
				len(host), plural(len(host), "warning", "warnings"))
		}
	}
	return nil
}

// plural picks the word for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// splitVerifyOutput sorts the lines `systemd-analyze verify` printed into the
// ones about the files being written and the ones about other units.
func splitVerifyOutput(out string, paths []string) (own, host []string) {
	if out == "" {
		return nil, nil
	}
	dirs, names := verifySubjects(paths)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if ownLine(line, dirs, names) {
			own = append(own, line)
		} else {
			host = append(host, line)
		}
	}
	return own, host
}

// verifySubjects returns what identifies the files being written: the
// directories they were staged in, and the names of their units.
func verifySubjects(paths []string) (dirs, names []string) {
	seen := map[string]bool{}
	for _, path := range paths {
		dir := filepath.Dir(path)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir+"/")
		}
		names = append(names, filepath.Base(path))
	}
	return dirs, names
}

// ownLine decides whether one line of the check's output is about a file
// being written. systemd prints two shapes of line, and they were checked
// against systemd 249 to 259:
//
//	<path>:<line>: <message>   a problem found parsing a file
//	<unit>: <message>          a problem found checking a loaded unit
//
// A line is about the files being written when it names the staging
// directory (the unit itself, and the drop-ins staged beside it) or the name
// of a unit being written anywhere — which also covers a drop-in of an edited
// unit that lives on the host, since that file is part of the same unit.
// A line is about another unit when it starts with another path or another
// unit's name. Anything else is counted as being about the file: an output
// this tool cannot place is not one it may ignore.
func ownLine(line string, dirs, names []string) bool {
	for _, dir := range dirs {
		if strings.Contains(line, dir) {
			return true
		}
	}
	for _, name := range names {
		if mentionsUnit(line, name) {
			return true
		}
	}
	if subject, _, ok := strings.Cut(line, ": "); ok {
		if pathLineRe.MatchString(subject) || ValidUnitName(subject) {
			return false
		}
	}
	return true
}

// pathLineRe is the subject of a parse warning: an absolute path to a unit
// file or drop-in, and the line number in it.
var pathLineRe = regexp.MustCompile(`^/[^\s:]+(:[0-9]+)?$`)

// mentionsUnit reports whether a line names a unit as a whole word: as
// itself, or as the directory its drop-ins live in. my-nginx.service does
// not mention nginx.service.
func mentionsUnit(line, name string) bool {
	for rest := line; ; {
		i := strings.Index(rest, name)
		if i < 0 {
			return false
		}
		before := byte(' ')
		if i > 0 {
			before = rest[i-1]
		}
		after := rest[i+len(name):]
		if unitBoundary(before) && (after == "" || unitBoundary(after[0]) ||
			strings.HasPrefix(after, ".d/")) {
			return true
		}
		rest = rest[i+1:]
	}
}

// unitBoundary is a character that can sit next to a unit name in a line
// without being part of a longer one.
func unitBoundary(c byte) bool {
	return strings.IndexByte(" \t/'\"()[],:=", c) >= 0
}

// DropInPlan renders, stages and checks the drop-in for a unit, and returns
// the plan that installs it.
//
// The order is the family's recipe and the reason a bad value cannot reach
// /etc: render the whole file, stage it beside a copy of everything systemd
// reads for this unit, let systemd parse that, and only then build the
// commands that install it and reload the manager.
func DropInPlan(ctx context.Context, verifier Verifier, req DropInRequest) (WritePlan, error) {
	if err := EditableUnit(req.Unit); err != nil {
		return WritePlan{}, err
	}
	unit := req.Unit.Name
	content, err := RenderDropIn(req.Unit.Type(), req.Values)
	if err != nil {
		return WritePlan{}, err
	}
	if content == Existing(unit, req.Cat) {
		return WritePlan{}, fmt.Errorf("%s already says exactly this",
			DropInPathFor(unit))
	}

	stage, file, err := StageDropIn(unit, req.Cat, content)
	if err != nil {
		return WritePlan{}, err
	}
	plan := WritePlan{
		Title: "Write " + DropInPathFor(unit),
		Files: []StagedFile{file},
		Stage: stage.Dir,
	}
	if err := verify(ctx, verifier, &plan, stage.Verify); err != nil {
		_ = os.RemoveAll(stage.Dir)
		return WritePlan{}, err
	}

	dir, err := BuildInstallDir(DropInDirFor(unit))
	if err != nil {
		return WritePlan{}, err
	}
	install, err := BuildInstallFile(file)
	if err != nil {
		return WritePlan{}, err
	}
	plan.Commands = []runner.Command{dir, install, BuildDaemonReload()}

	// The reload makes systemd read the new file; it does not make a running
	// unit adopt it. Saying so is the point of the second step, and it is a
	// separate confirmation because restarting a service is a different
	// decision from writing a file.
	if req.Restart {
		restart, err := BuildUnitAction(unit, "restart")
		if err != nil {
			return WritePlan{}, err
		}
		plan.Follow = &restart
		plan.FollowBody = "The file is written and systemd has re-read it. " +
			"A unit that is already running keeps the settings it started " +
			"with until it is restarted, which drops its current connections."
	} else {
		plan.Warning = "systemd will re-read the file, but a unit that is " +
			"already running keeps the settings it started with until it is " +
			"restarted."
	}
	return plan, nil
}

// NewUnitPlan renders, stages and checks a new service — or a timer and the
// service it starts — and returns the plan that installs them.
func NewUnitPlan(ctx context.Context, verifier Verifier, req NewUnitRequest) (WritePlan, error) {
	files, err := RenderUnits(req)
	if err != nil {
		return WritePlan{}, err
	}
	stage, staged, err := StageNewUnit(files)
	if err != nil {
		return WritePlan{}, err
	}

	plan := WritePlan{Title: "Create " + req.ServiceName(), Files: staged,
		Stage: stage.Dir}
	if req.Kind == KindTimer {
		plan.Title = "Create " + req.TimerName() + " and " + req.ServiceName()
	}
	if err := verify(ctx, verifier, &plan, stage.Verify); err != nil {
		_ = os.RemoveAll(stage.Dir)
		return WritePlan{}, err
	}

	for _, file := range staged {
		install, err := BuildInstallFile(file)
		if err != nil {
			return WritePlan{}, err
		}
		plan.Commands = append(plan.Commands, install)
	}
	plan.Commands = append(plan.Commands, BuildDaemonReload())

	if req.EnableNow {
		enable, err := BuildUnitAction(req.Target(), "enable", "--now")
		if err != nil {
			return WritePlan{}, err
		}
		plan.Follow = &enable
		plan.FollowBody = "The unit files are written and systemd has read " +
			"them. Enabling starts " + req.Target() + " now and again at " +
			"every boot."
	} else {
		plan.Warning = "The unit is installed but not enabled: nothing " +
			"starts it until you enable it with e, or start it with s."
	}
	return plan, nil
}
