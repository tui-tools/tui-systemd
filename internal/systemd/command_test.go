package systemd

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestBuildCommand(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		unit        string
		wantArgv    []string
		wantPreview string
		destructive bool
		wantErr     bool
	}{
		{
			name: "start", key: "s", unit: "sshd.service",
			wantArgv:    []string{"systemctl", "start", "sshd.service"},
			wantPreview: "sudo -n systemctl start sshd.service",
		},
		{
			name: "stop is destructive", key: "x", unit: "nginx.service",
			wantArgv:    []string{"systemctl", "stop", "nginx.service"},
			wantPreview: "sudo -n systemctl stop nginx.service",
			destructive: true,
		},
		{
			name: "restart is destructive", key: "r", unit: "docker.service",
			wantArgv:    []string{"systemctl", "restart", "docker.service"},
			wantPreview: "sudo -n systemctl restart docker.service",
			destructive: true,
		},
		{
			name: "reload", key: "l", unit: "nginx.service",
			wantArgv:    []string{"systemctl", "reload", "nginx.service"},
			wantPreview: "sudo -n systemctl reload nginx.service",
		},
		{
			name: "enable", key: "e", unit: "redis.service",
			wantArgv:    []string{"systemctl", "enable", "redis.service"},
			wantPreview: "sudo -n systemctl enable redis.service",
		},
		{
			name: "disable is destructive", key: "D", unit: "redis.service",
			wantArgv:    []string{"systemctl", "disable", "redis.service"},
			wantPreview: "sudo -n systemctl disable redis.service",
			destructive: true,
		},
		{
			name: "mask is destructive", key: "m", unit: "apache2.service",
			wantArgv:    []string{"systemctl", "mask", "apache2.service"},
			wantPreview: "sudo -n systemctl mask apache2.service",
			destructive: true,
		},
		{
			name: "unmask", key: "M", unit: "apache2.service",
			wantArgv:    []string{"systemctl", "unmask", "apache2.service"},
			wantPreview: "sudo -n systemctl unmask apache2.service",
		},
		{
			name: "daemon-reload takes no unit", key: "d", unit: "",
			wantArgv:    []string{"systemctl", "daemon-reload"},
			wantPreview: "sudo -n systemctl daemon-reload",
		},
		{
			name: "a unit action without a unit fails", key: "s", unit: "",
			wantErr: true,
		},
	}

	fake := NewFake()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, ok := ActionFor(tc.key)
			if !ok {
				t.Fatalf("no action bound to %q", tc.key)
			}
			cmd, err := BuildCommand(spec, tc.unit)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildCommand: %v", err)
			}
			if !reflect.DeepEqual(cmd.Argv, tc.wantArgv) {
				t.Errorf("Argv = %q, want %q", cmd.Argv, tc.wantArgv)
			}
			if cmd.Destructive != tc.destructive {
				t.Errorf("Destructive = %v, want %v", cmd.Destructive, tc.destructive)
			}
			// The preview is the promise: it must name the escalation prefix
			// and the exact argv, with nothing else in it.
			if got := fake.Preview(cmd); got != tc.wantPreview {
				t.Errorf("Preview = %q, want %q", got, tc.wantPreview)
			}
		})
	}
}

func TestActionKeysAreUnique(t *testing.T) {
	// A duplicate binding would silently shadow an action in the key handler.
	seen := map[string]Action{}
	for _, spec := range Actions {
		if other, ok := seen[spec.Key]; ok {
			t.Errorf("key %q is bound to both %q and %q", spec.Key, other, spec.Action)
		}
		seen[spec.Key] = spec.Action
		if spec.Label == "" || spec.Body == "" {
			t.Errorf("%q needs a label and a body for the confirm dialog", spec.Action)
		}
	}
}

func TestFakeAppliesConfirmedCommands(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	unitState := func(name string) Unit {
		t.Helper()
		units, err := f.Units(ctx)
		if err != nil {
			t.Fatalf("Units: %v", err)
		}
		for _, u := range units {
			if u.Name == name {
				return u
			}
		}
		t.Fatalf("%s is missing from the demo machine", name)
		return Unit{}
	}

	// Starting a stopped unit brings it up, exactly as the real command would.
	if unitState("redis.service").Running() {
		t.Fatal("redis.service should start out stopped in the demo")
	}
	start, _ := ActionFor("s")
	cmd, err := f.Build(start, "redis.service")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := f.Run(ctx, cmd); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !unitState("redis.service").Running() {
		t.Error("redis.service should be running after start")
	}

	// A masked unit cannot be started, and the demo says so the way systemd
	// does rather than pretending it worked.
	cmd, _ = f.Build(start, "apache2.service")
	if _, err := f.Run(ctx, cmd); err == nil {
		t.Error("starting a masked unit should fail")
	}

	// Enable changes only the unit-file state, never the runtime state.
	before := unitState("ssh.socket")
	enable, _ := ActionFor("e")
	cmd, _ = f.Build(enable, "ssh.socket")
	if _, err := f.Run(ctx, cmd); err != nil {
		t.Fatalf("Run: %v", err)
	}
	after := unitState("ssh.socket")
	if !after.Enabled() {
		t.Error("ssh.socket should be enabled")
	}
	if after.Active != before.Active {
		t.Errorf("enable must not change the runtime state: %q -> %q",
			before.Active, after.Active)
	}

	// Every command that ran was one the backend built and previewed.
	if len(f.Commands()) != 3 {
		t.Errorf("recorded %d commands, want 3", len(f.Commands()))
	}
}

func TestFakeUnknownUnit(t *testing.T) {
	f := NewFake()
	start, _ := ActionFor("s")
	cmd, _ := f.Build(start, "nope.service")
	_, err := f.Run(context.Background(), cmd)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want a not-found failure", err)
	}
}

func TestFakeReadsAreConsistent(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	units, err := f.Units(ctx)
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if len(units) == 0 || !units[0].Failed() {
		t.Error("the demo should open on a failed unit")
	}

	timers, err := f.Timers(ctx)
	if err != nil {
		t.Fatalf("Timers: %v", err)
	}
	var neverFired bool
	for _, timer := range timers {
		if timer.Last.IsZero() {
			neverFired = true
		}
	}
	if !neverFired {
		t.Error("the demo should include a timer that has never fired")
	}

	blame, err := f.Blame(ctx, 3)
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	if len(blame) != 3 {
		t.Errorf("Blame(3) returned %d entries", len(blame))
	}

	// A failed unit's journal must explain the failure: that is the whole
	// point of the panel.
	log, err := f.Journal(ctx, "nginx.service", 200)
	if err != nil {
		t.Fatalf("Journal: %v", err)
	}
	if !strings.Contains(log, "Address already in use") {
		t.Errorf("the demo journal should explain the failure, got %q", log)
	}
	// A unit with no scripted log still gets one, so no key dead-ends.
	if log, err = f.Journal(ctx, "cron.service", 0); err != nil || log == "" {
		t.Errorf("Journal(cron.service) = %q, %v", log, err)
	}
	if _, err := f.Journal(ctx, "", 0); err == nil {
		t.Error("Journal with no unit should fail")
	}
}
