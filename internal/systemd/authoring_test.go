package systemd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tui-tools/tui-kit/runner"
)

// okVerifier stands in for a systemd that accepted the staged files.
func okVerifier() Verifier {
	return func(_ context.Context, cmd runner.Command) (string, string, error) {
		return cmd.String(), "", nil
	}
}

// noisyVerifier stands in for the case the whole recipe exists for: systemd
// parsed the file, disliked a line, and exited 0 anyway.
func noisyVerifier(message string) Verifier {
	return func(_ context.Context, cmd runner.Command) (string, string, error) {
		return cmd.String(), message, nil
	}
}

func TestRenderDropInWritesTheWholeFile(t *testing.T) {
	content, err := RenderDropIn("service", map[string]string{
		"Restart":     "on-failure",
		"RestartSec":  "5s",
		"MemoryMax":   "512M",
		"Environment": "LOG_LEVEL=debug",
		"Nice":        "-5",
	})
	if err != nil {
		t.Fatalf("RenderDropIn: %v", err)
	}
	for _, want := range []string{
		"# Written by tui-systemd",
		"[Service]",
		"Restart=on-failure",
		"RestartSec=5s",
		"MemoryMax=512M",
		`Environment="LOG_LEVEL=debug"`,
		"Nice=-5",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("the drop-in does not contain %q:\n%s", want, content)
		}
	}
	// The order is the table's, so a file this tool wrote twice is the same
	// file both times.
	if strings.Index(content, "Restart=") > strings.Index(content, "MemoryMax=") {
		t.Errorf("the properties are out of table order:\n%s", content)
	}
	// One section header, however many properties are under it.
	if n := strings.Count(content, "[Service]"); n != 1 {
		t.Errorf("[Service] appears %d times, want 1:\n%s", n, content)
	}
}

func TestRenderDropInLeavesOutAnEmptyValue(t *testing.T) {
	content, err := RenderDropIn("service", map[string]string{
		"Restart": "always", "RestartSec": "",
	})
	if err != nil {
		t.Fatalf("RenderDropIn: %v", err)
	}
	if strings.Contains(content, "RestartSec") {
		t.Errorf("an empty value should leave the property out:\n%s", content)
	}
}

func TestRenderDropInRefusesAPropertyTheTypeDoesNotHave(t *testing.T) {
	if _, err := RenderDropIn("timer", map[string]string{"Restart": "always"}); err == nil {
		t.Fatal("a timer has no Restart=, and writing one should be refused")
	}
	if _, err := RenderDropIn("service", map[string]string{"ExecStart": "/bin/true"}); err == nil {
		t.Fatal("only the properties in the table may be written")
	}
}

// TestPropertyValuesRefuseInjection is the guard the closed property set
// exists for: a value that could carry a second directive, or end the line and
// start another, never becomes a file.
func TestPropertyValuesRefuseInjection(t *testing.T) {
	cases := []struct{ property, value string }{
		{"Restart", "always\nExecStart=/bin/sh"},
		{"RestartSec", "5s\nExecStartPre=/bin/sh -c 'curl evil'"},
		{"RestartSec", "5s;reboot"},
		{"MemoryMax", "512M\n[Service]"},
		{"CPUQuota", "50%\nUser=root"},
		{"Nice", "0\nExecStart=/bin/sh"},
		{"Nice", "-21"},
		{"Nice", "20"},
		{"Environment", "K=v\nExecStart=/bin/sh"},
		{"Environment", `K=v" ExecStart="/bin/sh`},
		{"Environment", "K=$(reboot)"},
		{"Environment", "K=%h"},
		{"Environment", `K=v\nX=y`},
		{"OnCalendar", "daily\nOnBootSec=1"},
		{"OnCalendar", "daily; reboot"},
		{"OnCalendar", "$(reboot)"},
	}
	for _, c := range cases {
		property, ok := PropertyNamed(c.property)
		if !ok {
			t.Fatalf("%s is not in the property table", c.property)
		}
		if err := property.Check(c.value); err == nil {
			t.Errorf("%s accepted %q", c.property, c.value)
		}
		unitType := "service"
		if c.property == "OnCalendar" {
			unitType = "timer"
		}
		if _, err := RenderDropIn(unitType,
			map[string]string{c.property: c.value}); err == nil {
			t.Errorf("RenderDropIn accepted %s=%q", c.property, c.value)
		}
	}
}

func TestPropertyValuesAccepted(t *testing.T) {
	cases := []struct{ property, value string }{
		{"Restart", "on-failure"},
		{"RestartSec", "500ms"}, {"RestartSec", "30"}, {"RestartSec", "5min"},
		{"MemoryMax", "512M"}, {"MemoryMax", "infinity"}, {"MemoryMax", "2G"},
		{"CPUQuota", "200%"},
		{"Nice", "-20"}, {"Nice", "19"}, {"Nice", "0"},
		{"Environment", "LOG_LEVEL=debug"},
		{"Environment", "PATH=/usr/local/bin:/usr/bin"},
		{"OnCalendar", "daily"}, {"OnCalendar", "Mon..Fri 09:00"},
		{"OnCalendar", "*-*-01 02:30:00"},
	}
	for _, c := range cases {
		property, _ := PropertyNamed(c.property)
		if err := property.Check(c.value); err != nil {
			t.Errorf("%s rejected %q: %v", c.property, c.value, err)
		}
	}
}

func TestEditableUnitRefusesWhatItCannotEdit(t *testing.T) {
	cases := []struct {
		name string
		unit Unit
		want string
	}{
		{"masked", Unit{Name: "apache2.service", Load: "masked"}, "masked"},
		{"transient", Unit{Name: "run-x.service", Load: "loaded",
			FileState: "transient"}, "transient"},
		{"a target", Unit{Name: "nfs-client.target", Load: "loaded"}, "target"},
		{"a bogus name", Unit{Name: "../../etc/passwd"}, "unit name"},
	}
	for _, c := range cases {
		err := EditableUnit(c.unit)
		if err == nil {
			t.Errorf("%s: the editor should refuse %s", c.name, c.unit.Name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v does not mention %q", c.name, err, c.want)
		}
	}
	if err := EditableUnit(Unit{Name: "nginx.service", Load: "loaded"}); err != nil {
		t.Errorf("an ordinary service should be editable: %v", err)
	}
	if err := EditableUnit(Unit{Name: "fstrim.timer", Load: "loaded"}); err != nil {
		t.Errorf("a timer should be editable: %v", err)
	}
}

// sampleCat is `systemctl cat nginx.service` on a machine where this tool has
// already written once: the vendor's unit, a drop-in somebody else shipped,
// and ours. The last one is written the way RenderDropIn writes it, banner
// included, because the no-op check compares the two texts.
var sampleCat = `# /usr/lib/systemd/system/nginx.service
[Unit]
Description=The nginx HTTP server
After=network.target

[Service]
Type=forking
ExecStart=/usr/sbin/nginx

# /etc/systemd/system/nginx.service.d/10-vendor.conf
[Service]
LimitNOFILE=8192

# /etc/systemd/system/nginx.service.d/90-tui-systemd.conf
` + dropInHeader + `
[Service]
Restart=on-failure
RestartSec=5s
`

func TestParseCatSplitsTheFiles(t *testing.T) {
	sections := ParseCat(sampleCat)
	if len(sections) != 3 {
		t.Fatalf("got %d sections, want 3: %+v", len(sections), sections)
	}
	if sections[0].Path != "/usr/lib/systemd/system/nginx.service" {
		t.Errorf("the first section is %q", sections[0].Path)
	}
	if !strings.Contains(sections[0].Body, "ExecStart=/usr/sbin/nginx") {
		t.Errorf("the fragment lost its body: %q", sections[0].Body)
	}
	if !strings.Contains(sections[1].Body, "LimitNOFILE=8192") {
		t.Errorf("the vendor drop-in lost its body: %q", sections[1].Body)
	}
}

func TestParseDropInSeedsTheForm(t *testing.T) {
	values := ParseDropIn(DropInIn("nginx.service", sampleCat))
	if values["Restart"] != "on-failure" || values["RestartSec"] != "5s" {
		t.Fatalf("the form would open on %v", values)
	}
	// A value the table would refuse is dropped rather than carried into a
	// file the tool would then be blamed for.
	values = ParseDropIn("[Service]\nRestart=nonsense\nLimitNOFILE=8192\n")
	if len(values) != 0 {
		t.Errorf("got %v, want nothing usable", values)
	}
}

func TestStageDropInCopiesEverythingSystemdReads(t *testing.T) {
	stage, file, err := StageDropIn("nginx.service", sampleCat,
		"[Service]\nRestart=always\n")
	if err != nil {
		t.Fatalf("StageDropIn: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stage.Dir) })

	if want := filepath.Join(stage.Dir, "nginx.service"); stage.Verify[0] != want {
		t.Errorf("verify runs on %q, want %q", stage.Verify[0], want)
	}
	for _, name := range []string{
		"nginx.service",
		"nginx.service.d/10-vendor.conf",
		"nginx.service.d/" + DropInFile,
	} {
		if _, err := os.Stat(filepath.Join(stage.Dir, name)); err != nil {
			t.Errorf("the staging directory is missing %s: %v", name, err)
		}
	}
	if file.Path != "/etc/systemd/system/nginx.service.d/"+DropInFile {
		t.Errorf("the file lands at %q", file.Path)
	}
	// The diff has two sides, because the unit already had a drop-in.
	if !strings.Contains(file.Diff, "-Restart=on-failure") ||
		!strings.Contains(file.Diff, "+Restart=always") {
		t.Errorf("the diff does not show the change:\n%s", file.Diff)
	}
}

func TestDropInPlanIsStageVerifyInstallReload(t *testing.T) {
	plan, err := DropInPlan(context.Background(), okVerifier(), DropInRequest{
		Unit: Unit{Name: "nginx.service", Load: "loaded"},
		Cat:  sampleCat,
		// The same properties, one of them changed: the plan exists.
		Values:  map[string]string{"Restart": "always", "RestartSec": "5s"},
		Restart: true,
	})
	if err != nil {
		t.Fatalf("DropInPlan: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(plan.Files[0].TempPath))) })

	if !plan.Validated {
		t.Errorf("the plan should be validated: %s", plan.Validation)
	}
	want := []string{
		"install -d -m 0755 /etc/systemd/system/nginx.service.d",
		"install -m 0644 " + plan.Files[0].TempPath +
			" /etc/systemd/system/nginx.service.d/" + DropInFile,
		"systemctl daemon-reload",
	}
	got := make([]string, 0, len(plan.Commands))
	for _, cmd := range plan.Commands {
		got = append(got, cmd.String())
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the plan runs\n%s\nwant\n%s", strings.Join(got, "\n"),
			strings.Join(want, "\n"))
	}
	if plan.Follow == nil || plan.Follow.String() != "systemctl restart nginx.service" {
		t.Errorf("the second step is %v", plan.Follow)
	}
}

func TestDropInPlanRefusesWhatSystemdComplainsAbout(t *testing.T) {
	// systemd-analyze warns and exits 0 for a value it cannot parse. A plan
	// that took the exit code at its word would put that value in /etc.
	_, err := DropInPlan(context.Background(),
		noisyVerifier("/tmp/x.d/90.conf:2: Failed to parse Restart=, ignoring"),
		DropInRequest{
			Unit:   Unit{Name: "nginx.service", Load: "loaded"},
			Cat:    sampleCat,
			Values: map[string]string{"Restart": "always"},
		})
	if err == nil {
		t.Fatal("a file systemd complained about must not reach /etc")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("the message does not say systemd refused it: %v", err)
	}
}

func TestDropInPlanRefusesANoOp(t *testing.T) {
	values := ParseDropIn(DropInIn("nginx.service", sampleCat))
	_, err := DropInPlan(context.Background(), okVerifier(), DropInRequest{
		Unit: Unit{Name: "nginx.service", Load: "loaded"},
		Cat:  sampleCat, Values: values,
	})
	if err == nil || !strings.Contains(err.Error(), "already says") {
		t.Fatalf("writing the same file again should be refused, got %v", err)
	}
}

func TestCheckNewUnitRefusesBadInput(t *testing.T) {
	base := NewUnitRequest{
		Name: "backup", Kind: KindService, Description: "Backup",
		ExecStart: "/usr/bin/rsync -a /srv/ backup:/srv/",
	}
	mutate := func(f func(*NewUnitRequest)) NewUnitRequest {
		req := base
		f(&req)
		return req
	}
	cases := []struct {
		name string
		req  NewUnitRequest
	}{
		{"a relative program", mutate(func(r *NewUnitRequest) { r.ExecStart = "rsync -a /srv/" })},
		{"a newline in ExecStart", mutate(func(r *NewUnitRequest) {
			r.ExecStart = "/bin/true\nExecStartPost=/bin/sh -c reboot"
		})},
		{"a shell in ExecStart", mutate(func(r *NewUnitRequest) {
			r.ExecStart = "/bin/sh -c 'curl x | sh'"
		})},
		{"a path in the name", mutate(func(r *NewUnitRequest) { r.Name = "../../etc/cron" })},
		{"a suffix in the name", mutate(func(r *NewUnitRequest) { r.Name = "backup nightly" })},
		{"an empty name", mutate(func(r *NewUnitRequest) { r.Name = "" })},
		{"a newline in the description", mutate(func(r *NewUnitRequest) {
			r.Description = "Backup\n[Service]\nExecStart=/bin/sh"
		})},
		{"a bogus user", mutate(func(r *NewUnitRequest) { r.User = "root; reboot" })},
		{"a kind that is not one", mutate(func(r *NewUnitRequest) { r.Kind = "socket" })},
		{"a timer with no schedule", mutate(func(r *NewUnitRequest) { r.Kind = KindTimer })},
		{"a timer with a bogus schedule", mutate(func(r *NewUnitRequest) {
			r.Kind, r.OnCalendar = KindTimer, "daily\nOnBootSec=1"
		})},
		{"a name that exists", mutate(func(r *NewUnitRequest) {
			r.Existing = []string{"backup.service"}
		})},
		{"a timer whose timer exists", mutate(func(r *NewUnitRequest) {
			r.Kind, r.OnCalendar = KindTimer, "daily"
			r.Existing = []string{"backup.timer"}
		})},
	}
	for _, c := range cases {
		if err := CheckNewUnit(c.req); err == nil {
			t.Errorf("%s should be refused", c.name)
		}
	}
	if err := CheckNewUnit(base); err != nil {
		t.Errorf("the ordinary request was refused: %v", err)
	}
}

func TestRenderUnitsWritesTheTemplates(t *testing.T) {
	files, err := RenderUnits(NewUnitRequest{
		Name: "backup", Kind: KindTimer, Description: "Nightly backup",
		ExecStart:  "/usr/bin/rsync -a /srv/ backup:/srv/",
		OnCalendar: "*-*-* 02:30:00", User: "backup",
	})
	if err != nil {
		t.Fatalf("RenderUnits: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("a timer writes %d files, want 2", len(files))
	}
	service, timer := files[0], files[1]
	if service.Path != "/etc/systemd/system/backup.service" ||
		timer.Path != "/etc/systemd/system/backup.timer" {
		t.Fatalf("the files land at %s and %s", service.Path, timer.Path)
	}
	for _, want := range []string{"Type=oneshot",
		"ExecStart=/usr/bin/rsync -a /srv/ backup:/srv/", "User=backup"} {
		if !strings.Contains(service.Content, want) {
			t.Errorf("the service is missing %q:\n%s", want, service.Content)
		}
	}
	// The timer is what gets enabled, so the service it starts must not also
	// have an [Install] section.
	if strings.Contains(service.Content, "[Install]") {
		t.Errorf("a timer's service must not be separately enablable:\n%s",
			service.Content)
	}
	for _, want := range []string{"OnCalendar=*-*-* 02:30:00", "Persistent=true",
		"Unit=backup.service", "WantedBy=timers.target"} {
		if !strings.Contains(timer.Content, want) {
			t.Errorf("the timer is missing %q:\n%s", want, timer.Content)
		}
	}
}

func TestNewUnitPlanInstallsThenReloads(t *testing.T) {
	plan, err := NewUnitPlan(context.Background(), okVerifier(), NewUnitRequest{
		Name: "backup", Kind: KindTimer, Description: "Nightly backup",
		ExecStart:  "/usr/bin/rsync -a /srv/ backup:/srv/",
		OnCalendar: "daily", EnableNow: true,
	})
	if err != nil {
		t.Fatalf("NewUnitPlan: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(plan.Files[0].TempPath)) })

	if len(plan.Commands) != 3 {
		t.Fatalf("the plan runs %d commands, want 3: %v", len(plan.Commands), plan.Commands)
	}
	if last := plan.Commands[2].String(); last != "systemctl daemon-reload" {
		t.Errorf("the plan ends with %q", last)
	}
	if plan.Follow == nil || plan.Follow.String() != "systemctl enable --now backup.timer" {
		t.Errorf("the second step is %v", plan.Follow)
	}
	for _, file := range plan.Files {
		if file.Diff == "" || !strings.Contains(file.Diff, "/dev/null") {
			t.Errorf("a new file's diff should come from nothing:\n%s", file.Diff)
		}
	}
}

func TestCommandBuildersRefuseAPathTheyDidNotMake(t *testing.T) {
	if _, err := BuildInstallFile(StagedFile{
		TempPath: "/tmp/x/90.conf", Path: "/etc/passwd"}); err == nil {
		t.Error("install must only write under " + UnitDir)
	}
	if _, err := BuildInstallFile(StagedFile{
		TempPath: "/tmp/$(reboot)/90.conf",
		Path:     UnitDir + "/x.service"}); err == nil {
		t.Error("a staging path with a metacharacter must be refused")
	}
	if _, err := BuildInstallDir("/etc/cron.d"); err == nil {
		t.Error("install -d must only create a drop-in directory")
	}
	if _, err := BuildVerify([]string{"x.service; reboot"}); err == nil {
		t.Error("verify must only run on a staging path")
	}
	if _, err := BuildUnitAction("nginx.service; reboot", "restart"); err == nil {
		t.Error("an action must only run on a unit name")
	}
	if _, err := BuildCat("../../etc/shadow"); err == nil {
		t.Error("cat must only run on a unit name")
	}
	if _, err := BuildCalendar("daily\nOnBootSec=1"); err == nil {
		t.Error("the calendar check must only run on a calendar expression")
	}
}

// TestDemoCreatesAndThenEditsAUnit is the parity the demo exists for: the
// sample machine runs the same commands a real one would, and the unit it ends
// up with is the one those commands describe.
func TestDemoCreatesAndThenEditsAUnit(t *testing.T) {
	fake := NewFake()
	ctx := context.Background()

	plan, err := fake.BuildNewUnit(ctx, NewUnitRequest{
		Name: "report", Kind: KindTimer, Description: "Weekly report",
		ExecStart: "/usr/bin/env true", OnCalendar: "weekly", EnableNow: true,
		Existing: SortedNames(mustUnits(t, fake)),
	})
	if err != nil {
		t.Fatalf("BuildNewUnit: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(plan.Files[0].TempPath)) })
	// The demo never claims systemd read the file, because it did not.
	if plan.Validated {
		t.Error("the demo must not claim a check it did not run")
	}
	for _, cmd := range append(plan.Commands, *plan.Follow) {
		if _, err := fake.Run(ctx, cmd); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}

	units := mustUnits(t, fake)
	for _, name := range []string{"report.service", "report.timer"} {
		found := false
		for _, u := range units {
			if u.Name == name {
				found = true
				if name == "report.timer" && !u.Enabled() {
					t.Errorf("%s should be enabled after enable --now", name)
				}
			}
		}
		if !found {
			t.Errorf("%s is not on the sample machine after the plan ran", name)
		}
	}

	// The new unit can now be read and edited, which is the whole point of
	// the demo being able to create one.
	out, err := fake.Cat(ctx, "report.timer")
	if err != nil {
		t.Fatalf("Cat: %v", err)
	}
	if !strings.Contains(out, "OnCalendar=weekly") {
		t.Errorf("the demo's unit file is not what was installed:\n%s", out)
	}
	edit, err := fake.BuildDropIn(ctx, DropInRequest{
		Unit: Unit{Name: "report.timer", Load: "loaded", FileState: "enabled"},
		Cat:  out, Values: map[string]string{"OnCalendar": "daily"},
	})
	if err != nil {
		t.Fatalf("BuildDropIn: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(filepath.Dir(filepath.Dir(edit.Files[0].TempPath)))
	})
	for _, cmd := range edit.Commands {
		if _, err := fake.Run(ctx, cmd); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
	out, err = fake.Cat(ctx, "report.timer")
	if err != nil {
		t.Fatalf("Cat: %v", err)
	}
	if !strings.Contains(out, DropInPathFor("report.timer")) ||
		!strings.Contains(out, "OnCalendar=daily") {
		t.Errorf("the drop-in did not land on the sample machine:\n%s", out)
	}
}

// mustUnits reads the fake's unit list or fails the test.
func mustUnits(t *testing.T, fake *Fake) []Unit {
	t.Helper()
	units, err := fake.Units(context.Background())
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	return units
}

// TestSystemdAcceptsWhatThisToolWrites is the check the whole recipe rests on,
// run against the systemd of the machine the tests run on. It is skipped where
// systemd-analyze is not installed, which is most CI containers.
func TestSystemdAcceptsWhatThisToolWrites(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not installed")
	}

	files, err := RenderUnits(NewUnitRequest{
		Name: "tui-systemd-selftest", Kind: KindTimer,
		Description: "tui-systemd self test",
		ExecStart:   "/usr/bin/env true", OnCalendar: "daily",
	})
	if err != nil {
		t.Fatalf("RenderUnits: %v", err)
	}
	stage, staged, err := StageNewUnit(files)
	if err != nil {
		t.Fatalf("StageNewUnit: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stage.Dir) })
	if len(staged) != 2 {
		t.Fatalf("staged %d files", len(staged))
	}

	//nolint:gosec // the argv is built here, from paths this package staged
	out, err := exec.Command(analyze, append([]string{"verify"}, stage.Verify...)...).
		CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("systemd-analyze verify refused the templates: %v\n%s", err, out)
	}
}
