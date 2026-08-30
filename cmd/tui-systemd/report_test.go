package main

import (
	"os"
	"strings"
	"testing"

	"github.com/tui-tools/tui-kit/config"
)

// baseReportConfig is the configuration a report is rendered against: the
// declared defaults, with nothing read off this machine.
func baseReportConfig() config.Config {
	return config.Config{Tool: toolName, Values: defaults()}
}

// TestRunReportDemo checks the half of the block this tool owns. The kit's own
// tests cover the machine facts and the scrubbing; what has to be right here is
// that --demo says demo, that the backend the fake imitates is named rather
// than whatever the fake calls itself, and that nothing on the host was
// consulted to produce any of it.
func TestRunReportDemo(t *testing.T) {
	var out strings.Builder
	opts := options{demo: true, report: true}
	if err := runReport(baseReportConfig(), opts, &out); err != nil {
		t.Fatalf("runReport: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"backend: demo\n",
		"mode: demo (sample data, the system was not read)\n",
		"demo backend: systemd\n",
		"helpers: not consulted (demo)\n",
		"journal lines: 200\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, toolName+" ") {
		t.Errorf("report should start with the tool name:\n%s", got)
	}
}

// TestRunReportLive renders the live block and asserts it names the backend
// this tool drives and says the run was live.
func TestRunReportLive(t *testing.T) {
	var out strings.Builder
	if err := runReport(baseReportConfig(), options{report: true}, &out); err != nil {
		t.Fatalf("runReport: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "mode: live\n") {
		t.Errorf("a live report should say so:\n%s", got)
	}
	// On a host without systemd the backend cannot be selected, and the point
	// of the flag is that a report is produced anyway, with the reason in it.
	if !strings.Contains(got, "backend: "+backendName) &&
		!strings.Contains(got, "backend error: ") {
		t.Errorf("report names neither the backend nor why there is none:\n%s", got)
	}
}

// TestReportLeaksNothingAboutTheUser is the privacy promise the bug form makes
// on this block's behalf: it is pasted into a public issue as it is, so the
// host name, the user name and any path under a home directory have to be
// absent from every mode the flag has.
func TestReportLeaksNothingAboutTheUser(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	// A machine named after its distribution ("fedora") would make this
	// assertion fire on the distro line, which is a fact the block is supposed
	// to carry. In that case the host name proves nothing either way, so it is
	// dropped rather than asserted on wrongly.
	if osRelease, err := os.ReadFile("/etc/os-release"); err == nil &&
		host != "" && strings.Contains(strings.ToLower(string(osRelease)),
		strings.ToLower(host)) {
		host = ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}

	for _, demo := range []bool{false, true} {
		var out strings.Builder
		opts := options{demo: demo, report: true}
		if err := runReport(baseReportConfig(), opts, &out); err != nil {
			t.Fatalf("runReport(demo=%v): %v", demo, err)
		}
		got := out.String()
		for _, secret := range []string{host, home, "/home/", os.Getenv("USER")} {
			if secret == "" {
				continue
			}
			if strings.Contains(got, secret) {
				t.Errorf("report(demo=%v) leaks %q:\n%s", demo, secret, got)
			}
		}
	}
}

// TestDescribeHelpers renders the optional binaries as one line, which is what
// tells "the journal panel is broken" from "journalctl is not installed".
func TestDescribeHelpers(t *testing.T) {
	if got := describeHelpers(true); got != "not consulted (demo)" {
		t.Errorf("describeHelpers(demo) = %q", got)
	}
	got := describeHelpers(false)
	for _, want := range []string{"journalctl ", "systemd-analyze "} {
		if !strings.Contains(got, want) {
			t.Errorf("describeHelpers = %q, missing %q", got, want)
		}
	}
	if !strings.Contains(got, "present") && !strings.Contains(got, "absent") {
		t.Errorf("describeHelpers = %q, says nothing about either binary", got)
	}
}
