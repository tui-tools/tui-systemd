package systemd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/tui-tools/tui-kit/runner"
)

// The outputs below were captured from `systemd-analyze verify` on systemd
// 249 (Ubuntu 22.04), 252 (Debian 12), 255 (Ubuntu 24.04), 257 (Debian 13)
// and 259 (Ubuntu 26.04, Fedora 44), and from the ubuntu-latest CI runner.
// STAGE stands for the staging directory the files were verified in; the
// verifier stub replaces it with the real one, because whether a line is
// about the file being written is decided by its path.

// ciHostWarnings is what the ubuntu-26.04 runner printed for the timer this
// tool writes: three lines about units the test never touched, exit 0.
const ciHostWarnings = `/usr/lib/systemd/system/xfs_scrub_all.service:26: Support for option CPUAccounting= has been removed and it is ignored
/usr/lib/systemd/system/system-xfs_scrub.slice:15: Support for option CPUAccounting= has been removed and it is ignored
/usr/lib/systemd/system/walinuxagent.service:21: Support for option CPUAccounting= has been removed and it is ignored`

// stagedVerifier answers the check with a captured output, STAGE replaced by
// the directory the plan staged its files in, and an exit status.
func stagedVerifier(output string, exitErr error) Verifier {
	return func(_ context.Context, cmd runner.Command) (string, string, error) {
		stage := filepath.Dir(cmd.Argv[len(cmd.Argv)-1])
		return cmd.String(), strings.ReplaceAll(output, "STAGE", stage), exitErr
	}
}

// errExit1 is a non-zero exit, the way the runner reports one.
var errExit1 = errors.New("exit status 1")

// timerRequest is the new-unit form filled in for a daily timer.
func timerRequest() NewUnitRequest {
	return NewUnitRequest{Name: "backup", Kind: KindTimer,
		Description: "nightly backup", ExecStart: "/usr/bin/env true",
		OnCalendar: "daily"}
}

func TestVerifyDecidesByTheFileBeingWritten(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		exitErr error
		// refused is a substring the refusal must carry, empty when the plan
		// must go through.
		refused string
		host    int
	}{
		{
			// F13: systemd 259 on the CI runner. Nothing about the timer.
			name:   "259: warnings about host units only, exit 0",
			output: ciHostWarnings,
			host:   3,
		},
		{
			name:    "259: a value in the file being written, exit 0",
			output:  "STAGE/backup.service:7: Failed to parse Restart=sometimes, ignoring: Invalid argument",
			refused: "Failed to parse Restart=sometimes",
		},
		{
			// --recursive-errors=no makes the same warning exit 1.
			name:    "259 --recursive-errors=no: a value in the file, exit 1",
			output:  "STAGE/backup.service:7: Failed to parse Restart=sometimes, ignoring: Invalid argument",
			exitErr: errExit1,
			refused: "Failed to parse Restart=sometimes",
		},
		{
			name:    "259: a removed option in the file being written",
			output:  "STAGE/backup.service:8: Support for option CPUAccounting= has been removed and it is ignored",
			refused: "CPUAccounting",
		},
		{
			name:    "255: an unknown key in the file being written",
			output:  "STAGE/backup.timer:7: Unknown key name 'Bogus' in section 'Timer', ignoring.",
			refused: "Unknown key name 'Bogus'",
		},
		{
			// Not a path: the unit's own name leads the line.
			name:    "252-259: a command that is not executable, exit 1",
			output:  "backup.service: Command /nonexistent/bin/thing is not executable: No such file or directory",
			exitErr: errExit1,
			refused: "not executable",
		},
		{
			name: "259: host warnings around a real error in the file",
			output: ciHostWarnings + "\n" +
				"STAGE/backup.service:6: Unknown key 'Bogus' in section [Service], ignoring.",
			refused: "Unknown key 'Bogus'",
			host:    3,
		},
		{
			// Fail closed: the exit status says something failed, and the
			// output does not say it was someone else's file.
			name:    "a failed check that printed only host lines",
			output:  ciHostWarnings,
			exitErr: errExit1,
			refused: "other units",
		},
		{
			// A line this tool cannot attribute counts against the file.
			name:    "a line about nothing in particular",
			output:  "Failed to prepare something: Permission denied",
			refused: "Failed to prepare something",
		},
		{
			// 249 has no --recursive-errors; the flag is gated on the
			// version, and if it ever slipped through it must not pass.
			name:    "249: the option is unknown",
			output:  "systemd-analyze: unrecognized option '--recursive-errors=no'",
			exitErr: errExit1,
			refused: "unrecognized option",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := NewUnitPlan(context.Background(),
				stagedVerifier(tc.output, tc.exitErr), timerRequest())
			if plan.Stage != "" {
				t.Cleanup(func() { _ = os.RemoveAll(plan.Stage) })
			}
			if tc.refused != "" {
				if err == nil {
					t.Fatalf("a check that said %q let the plan through", tc.output)
				}
				if !strings.Contains(err.Error(), tc.refused) ||
					!strings.Contains(err.Error(), "refused") {
					t.Errorf("the refusal is %q, want it to quote %q", err, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("the plan was refused: %v", err)
			}
			if !plan.Validated {
				t.Errorf("the plan is not marked validated: %q", plan.Validation)
			}
			if len(plan.HostWarnings) != tc.host {
				t.Errorf("host warnings = %q, want %d", plan.HostWarnings, tc.host)
			}
			if tc.host > 0 && !strings.Contains(plan.Validation, "other units") {
				t.Errorf("the validation does not mention the host warnings: %q",
					plan.Validation)
			}
		})
	}
}

// A drop-in is verified through a copy of the unit it belongs to. The unit is
// one the host already has, so its name in a line is the unit being written,
// whatever path the line starts with.
func TestVerifyDropInLinesAboutTheEditedUnitRefuse(t *testing.T) {
	cases := map[string]string{
		"the staged drop-in": "STAGE/nginx.service.d/" + DropInFile +
			":2: Nice priority out of range, ignoring: 40",
		"another drop-in of the same unit on the host": "/etc/systemd/system/" +
			"nginx.service.d/20-local.conf:3: Unknown key 'Bogus' in section [Service], ignoring.",
		"the unit's own name": "nginx.service: Command /usr/sbin/nginx is not executable: " +
			"No such file or directory",
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DropInPlan(context.Background(), stagedVerifier(output, nil),
				DropInRequest{
					Unit:   Unit{Name: "nginx.service", Load: "loaded"},
					Cat:    sampleCat,
					Values: map[string]string{"Restart": "always"},
				})
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("a line about the edited unit did not refuse: %v", err)
			}
		})
	}

	// And a host unit whose name only contains the edited one is not it.
	plan, err := DropInPlan(context.Background(), stagedVerifier(
		"/usr/lib/systemd/system/my-nginx.service:4: Support for option "+
			"CPUAccounting= has been removed and it is ignored", nil),
		DropInRequest{
			Unit:   Unit{Name: "nginx.service", Load: "loaded"},
			Cat:    sampleCat,
			Values: map[string]string{"Restart": "always"},
		})
	if err != nil {
		t.Fatalf("a warning about my-nginx.service refused nginx.service: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(plan.Stage) })
	if len(plan.HostWarnings) != 1 {
		t.Errorf("host warnings = %q, want the one line", plan.HostWarnings)
	}
}

func TestWithOwnErrorsOnly(t *testing.T) {
	cmd, err := BuildVerify([]string{"/tmp/tui-systemd-1/demo.service"})
	if err != nil {
		t.Fatalf("BuildVerify: %v", err)
	}
	got := strings.Join(WithOwnErrorsOnly(WithoutManCheck(cmd)).Argv, " ")
	want := "systemd-analyze verify --recursive-errors=no --man=no /tmp/tui-systemd-1/demo.service"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(cmd.Argv, " "), "--recursive-errors") {
		t.Error("WithOwnErrorsOnly changed the command it was given")
	}
	other := WithOwnErrorsOnly(runner.Command{Argv: []string{"systemd-analyze", "calendar", "daily"}})
	if strings.Contains(strings.Join(other.Argv, " "), "--recursive") {
		t.Error("only verify takes --recursive-errors")
	}
}

// localVerifiers are the ways the real backend can run the check on the
// systemd this test runs on: with the flags that systemd knows, and without
// them, which is what a systemd older than the flags gets.
func localVerifiers(t *testing.T) map[string]Verifier {
	t.Helper()
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	//nolint:gosec // a fixed argv
	out, err := exec.Command(analyze, "--version").Output()
	if err != nil {
		t.Skipf("systemd-analyze --version: %v", err)
	}
	match := regexp.MustCompile(`systemd ([0-9]+)`).FindSubmatch(out)
	if match == nil {
		t.Skipf("no version in %q", out)
	}
	version, _ := strconv.Atoi(string(match[1]))
	t.Logf("systemd %d", version)

	run := func(flags func(runner.Command) runner.Command) Verifier {
		return func(_ context.Context, cmd runner.Command) (string, string, error) {
			cmd = flags(cmd)
			//nolint:gosec // the argv is built by BuildVerify, from paths this package staged
			out, err := exec.Command(analyze, cmd.Argv[1:]...).CombinedOutput()
			return cmd.String(), strings.TrimSpace(string(out)), err
		}
	}
	verifiers := map[string]Verifier{
		"no flags": run(func(cmd runner.Command) runner.Command { return cmd }),
	}
	if version >= 235 {
		verifiers["--man=no"] = run(WithoutManCheck)
	}
	if version >= 250 {
		verifiers["--man=no --recursive-errors=no"] = run(
			func(cmd runner.Command) runner.Command {
				return WithOwnErrorsOnly(WithoutManCheck(cmd))
			})
	}
	return verifiers
}

// TestSystemdAcceptsWhatThisToolWrites is the check the whole recipe rests on,
// run against the systemd of the machine the tests run on, through the same
// decision the tool makes. It is skipped where systemd-analyze is not
// installed, which is most CI containers.
//
// Whatever the host's own units make systemd-analyze say about them — on
// systemd 259 that includes CPUAccounting= warnings for units this test never
// touches — the templates must be accepted.
func TestSystemdAcceptsWhatThisToolWrites(t *testing.T) {
	for name, verifier := range localVerifiers(t) {
		t.Run(name, func(t *testing.T) {
			plan, err := NewUnitPlan(context.Background(), verifier,
				NewUnitRequest{
					Name: "tui-systemd-selftest", Kind: KindTimer,
					Description: "tui-systemd self test",
					ExecStart:   "/usr/bin/env true", OnCalendar: "daily",
				})
			if err != nil {
				t.Fatalf("systemd-analyze verify refused the templates: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(plan.Stage) })
			if !plan.Validated {
				t.Fatalf("the templates were not validated: %q", plan.Validation)
			}
			for _, line := range plan.HostWarnings {
				t.Logf("host warning: %s", line)
			}
		})
	}
}

// TestSystemdStillRefusesABrokenUnit is the other half: the same real check
// must refuse a unit that is wrong, with every flag combination, or accepting
// the host's warnings would have bought nothing.
func TestSystemdStillRefusesABrokenUnit(t *testing.T) {
	broken := map[string]string{
		"a value systemd cannot parse": "[Unit]\nDescription=broken\n\n" +
			"[Service]\nType=oneshot\nExecStart=/usr/bin/env true\nRestart=sometimes\n",
		"a key systemd does not know": "[Unit]\nDescription=broken\n\n" +
			"[Service]\nType=oneshot\nExecStart=/usr/bin/env true\nBogus=1\n",
		"a command that is not there": "[Unit]\nDescription=broken\n\n" +
			"[Service]\nType=oneshot\nExecStart=/nonexistent/tui-systemd-selftest\n",
	}
	for flags, verifier := range localVerifiers(t) {
		for name, content := range broken {
			t.Run(flags+"/"+name, func(t *testing.T) {
				stage, _, err := StageNewUnit([]StagedFile{{
					Path:    UnitPathFor("tui-systemd-broken.service"),
					Content: content,
				}})
				if err != nil {
					t.Fatalf("StageNewUnit: %v", err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(stage.Dir) })
				var plan WritePlan
				err = verify(context.Background(), verifier, &plan, stage.Verify)
				if err == nil {
					t.Fatalf("a broken unit was accepted: %q", plan.Validation)
				}
				if !strings.Contains(err.Error(), "refused") {
					t.Errorf("the refusal does not say so: %v", err)
				}
			})
		}
	}
}
