package systemd

import (
	"context"
	"fmt"
	"os"
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
// `systemd-analyze verify` reports a property it cannot parse as a warning and
// still exits 0, so a zero exit is not enough: any output at all is treated as
// a refusal. That is stricter than systemd itself, and deliberately — a line
// systemd would silently ignore is a line the operator thought they were
// setting.
func verify(ctx context.Context, verifier Verifier, plan *WritePlan,
	paths []string) error {
	cmd, err := BuildVerify(paths)
	if err != nil {
		return err
	}
	preview, out, err := verifier(ctx, cmd)
	plan.ValidationCommand = preview
	out = strings.TrimSpace(out)
	switch {
	case err != nil && out != "":
		return fmt.Errorf("systemd refused the file: %s", runner.FirstLine(out))
	case err != nil:
		// The check itself could not run — systemd-analyze is not installed,
		// most likely. That is not a reason to refuse the edit, but it is a
		// reason for the dialog to say the file was never read.
		plan.Validation = "could not run: " + runner.FirstLine(err.Error())
	case out != "":
		// Exit 0 with something to say: a property systemd would parse and
		// then ignore. It never reaches /etc.
		return fmt.Errorf("systemd refused the file: %s", runner.FirstLine(out))
	default:
		plan.Validated = true
		plan.Validation = "accepted by " + preview
	}
	return nil
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
