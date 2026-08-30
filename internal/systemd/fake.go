package systemd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tui-tools/tui-kit/runner"
)

// Fake is an in-memory backend used by --demo and by the tests. It builds and
// previews exactly the commands the real backend would, and then applies them
// to its own state instead of to the machine, so the UI cannot tell the two
// apart and nothing on the host is touched.
type Fake struct {
	mu     sync.Mutex
	units  []Unit
	timers []Timer
	blame  []BlameEntry
	// run records and answers the commands, and is what the preview goes
	// through, so the demo shows the same "sudo -n systemctl …" a real run
	// would.
	run *runner.Fake
	// FailWith, when set, makes the next Run fail with this error.
	FailWith error
}

// NewFake returns a Fake preloaded with a realistic machine.
func NewFake() *Fake {
	f := &Fake{
		units:  demoUnits(),
		timers: demoTimers(),
		blame:  demoBlame(),
	}
	f.run = &runner.Fake{Prefix: "sudo -n", Hook: f.apply}
	return f
}

// Name identifies the backend.
func (f *Fake) Name() string { return "demo" }

// Describe is the one-line summary shown in the header.
func (f *Fake) Describe() string { return "demo (no changes are applied)" }

// Preview renders the command the way the real backend would.
func (f *Fake) Preview(cmd runner.Command) string { return f.run.Preview(cmd) }

// Run applies a confirmed command to the in-memory state.
func (f *Fake) Run(ctx context.Context, cmd runner.Command) (string, error) {
	if f.FailWith != nil {
		err := f.FailWith
		f.FailWith = nil
		return "", err
	}
	return f.run.Run(ctx, cmd)
}

// Commands returns every command the fake was asked to run, for the tests.
func (f *Fake) Commands() []runner.Command { return f.run.Ran }

// apply mutates the sample machine the way the real command would. It is the
// runner.Fake hook, so it runs only for a command that was previewed and
// confirmed — the same path the real backend takes.
func (f *Fake) apply(cmd runner.Command) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(cmd.Argv) < 2 {
		return "", fmt.Errorf("systemd: malformed command %q", cmd)
	}
	action := Action(cmd.Argv[1])
	if action == DaemonReload {
		return "", nil
	}
	if len(cmd.Argv) < 3 {
		return "", fmt.Errorf("systemd: %s needs a unit", action)
	}
	name := cmd.Argv[2]
	for i := range f.units {
		if f.units[i].Name != name {
			continue
		}
		u := &f.units[i]
		switch action {
		case Start, Restart:
			if u.Masked() {
				//nolint:staticcheck // mirrors systemctl's exact message
				return "", fmt.Errorf(
					"Failed to %s %s: Unit %s is masked.", action, name, name)
			}
			u.Active, u.Sub = ActiveActive, "running"
		case Stop:
			u.Active, u.Sub = ActiveInactive, "dead"
		case Reload:
			if !u.Running() {
				//nolint:staticcheck // mirrors systemctl's exact message
				return "", fmt.Errorf(
					"Failed to reload %s: Job type reload is not applicable "+
						"for unit %s.", name, name)
			}
		case Enable:
			u.FileState = "enabled"
			return fmt.Sprintf("Created symlink "+
				"/etc/systemd/system/multi-user.target.wants/%s -> "+
				"/usr/lib/systemd/system/%s.", name, name), nil
		case Disable:
			u.FileState = "disabled"
			return fmt.Sprintf("Removed "+
				"/etc/systemd/system/multi-user.target.wants/%s.", name), nil
		case Mask:
			u.Load, u.FileState = "masked", "masked"
			u.Active, u.Sub = ActiveInactive, "dead"
			return fmt.Sprintf("Created symlink /etc/systemd/system/%s -> "+
				"/dev/null.", name), nil
		case Unmask:
			u.Load, u.FileState = "loaded", "disabled"
			return fmt.Sprintf("Removed /etc/systemd/system/%s.", name), nil
		}
		return "", nil
	}
	//nolint:staticcheck // mirrors systemctl's exact message
	return "", fmt.Errorf("Failed to %s %s: Unit %s not found.",
		action, name, name)
}

// Units returns the sample unit list.
func (f *Fake) Units(_ context.Context) ([]Unit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	units := append([]Unit(nil), f.units...)
	SortUnits(units)
	return units, nil
}

// Timers returns the sample timers.
func (f *Fake) Timers(_ context.Context) ([]Timer, error) {
	return append([]Timer(nil), f.timers...), nil
}

// Blame returns the sample boot times.
func (f *Fake) Blame(_ context.Context, limit int) ([]BlameEntry, error) {
	entries := append([]BlameEntry(nil), f.blame...)
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// Build turns an action into a previewable command, exactly as Real does.
func (f *Fake) Build(spec ActionSpec, unit string) (runner.Command, error) {
	return BuildCommand(spec, unit)
}

// Journal returns a plausible log for a unit. The sample logs are fixed
// excerpts, short enough that the requested backlog size never bites, so the
// line count is accepted and ignored.
func (f *Fake) Journal(_ context.Context, unit string, _ int) (string, error) {
	if unit == "" {
		return "", fmt.Errorf("no unit selected")
	}
	if body, ok := demoJournals[unit]; ok {
		return body, nil
	}
	base := time.Date(2026, 8, 29, 9, 14, 2, 0, time.UTC)
	var b strings.Builder
	short := strings.TrimSuffix(unit, ".service")
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&b, "%s demo-host %s[%d]: unit is running normally\n",
			base.Add(time.Duration(i)*7*time.Second).Format("Jan 02 15:04:05"),
			short, 1200+i)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// unit is a small helper keeping demoUnits readable.
func unit(name, load, active, sub, fileState, description string) Unit {
	return Unit{Name: name, Load: load, Active: active, Sub: sub,
		FileState: fileState, Description: description}
}

// demoUnits is the sample machine shown by --demo: a couple of failures worth
// looking at, a masked unit, something enabled but not running, and enough
// ordinary services for the list to feel real.
func demoUnits() []Unit {
	return []Unit{
		unit("nginx.service", "loaded", ActiveFailed, "failed", "enabled",
			"The nginx HTTP and reverse proxy server"),
		unit("backup-nightly.service", "loaded", ActiveFailed, "failed", "static",
			"Nightly off-site backup"),
		unit("sshd.service", "loaded", ActiveActive, "running", "enabled",
			"OpenSSH server daemon"),
		unit("postgresql.service", "loaded", ActiveActive, "running", "enabled",
			"PostgreSQL database server"),
		unit("docker.service", "loaded", ActiveActive, "running", "enabled",
			"Docker Application Container Engine"),
		unit("cron.service", "loaded", ActiveActive, "running", "enabled",
			"Regular background program processing daemon"),
		unit("systemd-journald.service", "loaded", ActiveActive, "running", "static",
			"Journal Service"),
		unit("systemd-resolved.service", "loaded", ActiveActive, "running", "enabled",
			"Network Name Resolution"),
		unit("redis.service", "loaded", ActiveInactive, "dead", "enabled",
			"Advanced key-value store"),
		unit("apache2.service", "masked", ActiveInactive, "dead", "masked",
			"The Apache HTTP Server"),
		unit("unattended-upgrades.service", "loaded", ActiveActive, "running", "enabled",
			"Unattended Upgrades Shutdown"),
		unit("fstrim.timer", "loaded", ActiveActive, "waiting", "enabled",
			"Discard unused filesystem blocks once a week"),
		unit("logrotate.timer", "loaded", ActiveActive, "waiting", "enabled",
			"Daily rotation of log files"),
		unit("backup-nightly.timer", "loaded", ActiveActive, "waiting", "enabled",
			"Run the nightly backup at 02:30"),
		unit("ssh.socket", "loaded", ActiveInactive, "dead", "disabled",
			"OpenSSH Server Socket"),
		unit("nfs-client.target", "loaded", ActiveActive, "active", "enabled",
			"NFS client services"),
		unit("dev-nvme0n1p2.device", "loaded", ActiveActive, "plugged", "",
			"Samsung SSD 980 PRO 1TB"),
		unit("var-log.mount", "loaded", ActiveActive, "mounted", "generated",
			"/var/log"),
	}
}

// demoTimers is the sample timer list. The times are relative to now so the
// view always reads sensibly, however long after this was written it runs.
func demoTimers() []Timer {
	now := time.Now()
	return []Timer{
		{Next: now.Add(23 * time.Minute), Last: now.Add(-37 * time.Minute),
			Unit: "logrotate.timer", Activates: "logrotate.service"},
		{Next: now.Add(6*time.Hour + 12*time.Minute), Last: now.Add(-17 * time.Hour),
			Unit: "backup-nightly.timer", Activates: "backup-nightly.service"},
		{Next: now.Add(52 * time.Hour), Last: now.Add(-5 * 24 * time.Hour),
			Unit: "fstrim.timer", Activates: "fstrim.service"},
		// A timer that has never fired: both halves of the view must cope.
		{Next: now.Add(4 * time.Hour), Unit: "certbot-renew.timer",
			Activates: "certbot-renew.service"},
	}
}

// demoBlame is the sample boot breakdown.
func demoBlame() []BlameEntry {
	return []BlameEntry{
		{Raw: "4min 12.418s", Duration: ParseDuration("4min 12.418s"), Unit: "fstrim.service"},
		{Raw: "51.203s", Duration: ParseDuration("51.203s"), Unit: "plocate-updatedb.service"},
		{Raw: "12.980s", Duration: ParseDuration("12.980s"), Unit: "docker.service"},
		{Raw: "8.117s", Duration: ParseDuration("8.117s"), Unit: "postgresql.service"},
		{Raw: "4.402s", Duration: ParseDuration("4.402s"), Unit: "systemd-udev-settle.service"},
		{Raw: "2.771s", Duration: ParseDuration("2.771s"), Unit: "NetworkManager.service"},
		{Raw: "1.905s", Duration: ParseDuration("1.905s"), Unit: "systemd-journald.service"},
		{Raw: "998ms", Duration: ParseDuration("998ms"), Unit: "sshd.service"},
		{Raw: "764ms", Duration: ParseDuration("764ms"), Unit: "systemd-resolved.service"},
		{Raw: "311ms", Duration: ParseDuration("311ms"), Unit: "cron.service"},
	}
}

// demoJournals holds the logs worth reading in the demo: the two failures, so
// pressing j on a failed unit shows why it failed.
var demoJournals = map[string]string{
	"nginx.service": `Aug 29 09:14:02 demo-host systemd[1]: Starting The nginx HTTP and reverse proxy server...
Aug 29 09:14:02 demo-host nginx[2841]: nginx: [emerg] bind() to 0.0.0.0:80 failed (98: Address already in use)
Aug 29 09:14:02 demo-host nginx[2841]: nginx: [emerg] still could not bind()
Aug 29 09:14:02 demo-host systemd[1]: nginx.service: Control process exited, code=exited, status=1/FAILURE
Aug 29 09:14:02 demo-host systemd[1]: nginx.service: Failed with result 'exit-code'.
Aug 29 09:14:02 demo-host systemd[1]: Failed to start The nginx HTTP and reverse proxy server.`,
	"backup-nightly.service": `Aug 29 02:30:00 demo-host systemd[1]: Starting Nightly off-site backup...
Aug 29 02:30:01 demo-host backup[9912]: rsync: connection unexpectedly closed
Aug 29 02:30:01 demo-host backup[9912]: rsync error: unexplained error (code 255) at io.c(231)
Aug 29 02:30:01 demo-host systemd[1]: backup-nightly.service: Main process exited, code=exited, status=255/n/a
Aug 29 02:30:01 demo-host systemd[1]: backup-nightly.service: Failed with result 'exit-code'.
Aug 29 02:30:01 demo-host systemd[1]: Failed to start Nightly off-site backup.`,
	"sshd.service": `Aug 29 08:02:11 demo-host systemd[1]: Started OpenSSH server daemon.
Aug 29 08:02:11 demo-host sshd[1104]: Server listening on 0.0.0.0 port 22.
Aug 29 08:02:11 demo-host sshd[1104]: Server listening on :: port 22.
Aug 29 09:41:38 demo-host sshd[3320]: Accepted publickey for deploy from 10.0.0.14 port 51233 ssh2
Aug 29 10:12:04 demo-host sshd[3320]: Received disconnect from 10.0.0.14 port 51233:11: disconnected by user`,
}
