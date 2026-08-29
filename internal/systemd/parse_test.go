package systemd

import (
	"reflect"
	"testing"
	"time"
)

// The samples below are real output, captured from systemd 257 on Fedora 42
// and trimmed to the interesting rows. When a parser is wrong on your machine,
// paste your own output in as a new case: that is the fastest bug report.

const listUnitsJSON = `[
{"unit":"boot.automount","load":"not-found","active":"inactive","sub":"dead","description":"boot.automount"},
{"unit":"dev-disk-by\\x2ddiskseq-1\\x2dpart1.device","load":"loaded","active":"active","sub":"plugged","description":"XPG GAMMIX S70 BLADE Basic\\x20data\\x20partition"},
{"unit":"zfs-import-cache.service","load":"loaded","active":"failed","sub":"failed","description":"Import ZFS pools by cache file"},
{"unit":"sshd.service","load":"loaded","active":"active","sub":"running","description":"OpenSSH server daemon"},
{"unit":"fstrim.timer","load":"loaded","active":"active","sub":"waiting","description":"Discard unused blocks once a week"}
]`

const listUnitFilesJSON = `[
{"unit_file":"proc-sys-fs-binfmt_misc.automount","state":"static","preset":null},
{"unit_file":"boot.mount","state":"generated","preset":null},
{"unit_file":"sshd.service","state":"enabled","preset":"enabled"},
{"unit_file":"getty@.service","state":"enabled","preset":"enabled"},
{"unit_file":"apache2.service","state":"masked","preset":"disabled"},
{"unit_file":"redis.service","state":"disabled","preset":"disabled"}
]`

const listTimersJSON = `[
{"next":1788040800000000,"left":1788040800000000,"last":1788040206846549,"passed":886209581656,"unit":"sysstat-collect.timer","activates":"sysstat-collect.service"},
{"next":1788058800000000,"left":1788058800000000,"last":0,"passed":0,"unit":"certbot-renew.timer","activates":"certbot-renew.service"}
]`

const blameOutput = `1h 39min 446ms fstrim.service
  2min 37.902s plocate-updatedb.service
       55.549s systemd-cryptsetup@home.service
        6.103s docker.service
        4.227s sys-module-fuse.device
         311ms cron.service
`

func TestParseUnits(t *testing.T) {
	units, err := ParseUnits(listUnitsJSON)
	if err != nil {
		t.Fatalf("ParseUnits: %v", err)
	}
	if len(units) != 5 {
		t.Fatalf("got %d units, want 5", len(units))
	}

	tests := []struct {
		name string
		want Unit
	}{
		{
			name: "a unit whose file is gone is still listed",
			want: Unit{Name: "boot.automount", Load: "not-found",
				Active: "inactive", Sub: "dead", Description: "boot.automount"},
		},
		{
			name: "escapes in a device name and its description are decoded",
			want: Unit{Name: "dev-disk-by-diskseq-1-part1.device", Load: "loaded",
				Active: "active", Sub: "plugged",
				Description: "XPG GAMMIX S70 BLADE Basic data partition"},
		},
		{
			name: "a failed unit keeps both of its states",
			want: Unit{Name: "zfs-import-cache.service", Load: "loaded",
				Active: "failed", Sub: "failed",
				Description: "Import ZFS pools by cache file"},
		},
	}
	byName := map[string]Unit{}
	for _, u := range units {
		byName[u.Name] = u
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := byName[tc.want.Name]
			if !ok {
				t.Fatalf("%q is missing from the parsed list", tc.want.Name)
			}
			if got != tc.want {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestParseUnitsEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{name: "empty output", input: "", wantLen: 0},
		{name: "an empty list", input: "[]", wantLen: 0},
		{name: "blank whitespace", input: "   \n ", wantLen: 0},
		{name: "an entry with no name is skipped",
			input: `[{"unit":"","active":"active"},{"unit":"a.service"}]`, wantLen: 1},
		{name: "the text table is not JSON", input: "UNIT LOAD ACTIVE\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			units, err := ParseUnits(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseUnits: %v", err)
			}
			if len(units) != tc.wantLen {
				t.Errorf("got %d units, want %d", len(units), tc.wantLen)
			}
		})
	}
}

func TestParseUnitFiles(t *testing.T) {
	files, err := ParseUnitFiles(listUnitFilesJSON)
	if err != nil {
		t.Fatalf("ParseUnitFiles: %v", err)
	}
	if len(files) != 6 {
		t.Fatalf("got %d unit files, want 6", len(files))
	}
	// A null preset must not break the decode, which is why Preset is a
	// pointer in the wire struct.
	if got := files["boot.mount"]; got.FileState != "generated" || got.Preset != "" {
		t.Errorf("boot.mount = %+v", got)
	}
	if got := files["sshd.service"]; got.FileState != "enabled" || got.Preset != "enabled" {
		t.Errorf("sshd.service = %+v", got)
	}
}

func TestMergeUnits(t *testing.T) {
	units, err := ParseUnits(listUnitsJSON)
	if err != nil {
		t.Fatalf("ParseUnits: %v", err)
	}
	files, err := ParseUnitFiles(listUnitFilesJSON)
	if err != nil {
		t.Fatalf("ParseUnitFiles: %v", err)
	}
	merged := MergeUnits(units, files)

	byName := map[string]Unit{}
	for _, u := range merged {
		byName[u.Name] = u
	}

	// The file state lands on the running unit.
	if got := byName["sshd.service"]; got.FileState != "enabled" || !got.Enabled() {
		t.Errorf("sshd.service = %+v, want enabled", got)
	}
	// A unit file with nothing running is added as inactive, because
	// "installed but never started" is a state worth finding.
	redis, ok := byName["redis.service"]
	if !ok {
		t.Fatal("redis.service should be added from the unit file list")
	}
	if redis.Active != ActiveInactive || redis.FileState != "disabled" {
		t.Errorf("redis.service = %+v", redis)
	}
	// A template unit file is not an instantiable unit.
	if _, ok := byName["getty@.service"]; ok {
		t.Error("a template unit file must not be listed as a unit")
	}
	// Masked is reported through either field.
	if got := byName["apache2.service"]; !got.Masked() {
		t.Errorf("apache2.service = %+v, want masked", got)
	}
	// Failed units sort first: that is why anyone opened the tool.
	if !merged[0].Failed() {
		t.Errorf("merged[0] = %+v, want a failed unit first", merged[0])
	}
}

func TestParseTimers(t *testing.T) {
	timers, err := ParseTimers(listTimersJSON)
	if err != nil {
		t.Fatalf("ParseTimers: %v", err)
	}
	if len(timers) != 2 {
		t.Fatalf("got %d timers, want 2", len(timers))
	}
	if got, want := timers[0].Unit, "sysstat-collect.timer"; got != want {
		t.Errorf("Unit = %q, want %q", got, want)
	}
	if got, want := timers[0].Next.UnixMicro(), int64(1788040800000000); got != want {
		t.Errorf("Next = %d, want %d", got, want)
	}
	if timers[0].Last.IsZero() {
		t.Error("Last should be set for a timer that has fired")
	}
	// A timer that never fired reports the zero time rather than the epoch,
	// so the view can say "never" instead of 1970.
	if !timers[1].Last.IsZero() {
		t.Errorf("Last = %v, want the zero time for a timer that never fired",
			timers[1].Last)
	}
}

func TestParseTimersRejectsText(t *testing.T) {
	// The text table is deliberately unsupported: its time columns are
	// right-aligned against narrower headers, so no column offset slices them
	// correctly. Failing loudly beats mangling timestamps quietly.
	text := "NEXT                            LEFT LAST\n" +
		"Sat 2026-08-29 19:00:00 -03 4min 31s Sat 2026-08-29 18:50:06 -03\n"
	_, err := ParseTimers(text)
	if err == nil {
		t.Fatal("expected an error for the text table")
	}
	if got := err.Error(); !contains(got, "systemd 250") {
		t.Errorf("the error should name the version requirement, got %q", got)
	}
}

func TestParseBlame(t *testing.T) {
	entries := ParseBlame(blameOutput)
	want := []BlameEntry{
		{Raw: "1h 39min 446ms", Unit: "fstrim.service",
			Duration: time.Hour + 39*time.Minute + 446*time.Millisecond},
		{Raw: "2min 37.902s", Unit: "plocate-updatedb.service",
			Duration: 2*time.Minute + 37902*time.Millisecond},
		{Raw: "55.549s", Unit: "systemd-cryptsetup@home.service",
			Duration: 55549 * time.Millisecond},
		{Raw: "6.103s", Unit: "docker.service", Duration: 6103 * time.Millisecond},
		{Raw: "4.227s", Unit: "sys-module-fuse.device", Duration: 4227 * time.Millisecond},
		{Raw: "311ms", Unit: "cron.service", Duration: 311 * time.Millisecond},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i := range want {
		if entries[i].Unit != want[i].Unit || entries[i].Raw != want[i].Raw {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
		// Float arithmetic on the fractional seconds: compare with a tolerance
		// rather than exactly.
		if diff := entries[i].Duration - want[i].Duration; diff > time.Millisecond ||
			diff < -time.Millisecond {
			t.Errorf("entry %d duration = %v, want %v",
				i, entries[i].Duration, want[i].Duration)
		}
	}
}

func TestParseBlameIgnoresNoise(t *testing.T) {
	// systemd-analyze prints nothing useful on a container, and a blank or
	// unitless line must not become an entry.
	entries := ParseBlame("\n\nBootup is not yet finished\n   12.000s a.service\n")
	if len(entries) != 1 || entries[0].Unit != "a.service" {
		t.Errorf("entries = %+v, want just a.service", entries)
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{in: "1h 39min 446ms", want: time.Hour + 39*time.Minute + 446*time.Millisecond},
		{in: "2min 37.902s", want: 2*time.Minute + 37902*time.Millisecond},
		{in: "55.549s", want: 55549 * time.Millisecond},
		{in: "311ms", want: 311 * time.Millisecond},
		{in: "1min 30s", want: 90 * time.Second},
		{in: "2d 4h", want: 52 * time.Hour},
		{in: "890us", want: 890 * time.Microsecond},
		// Nothing parseable: zero, not a panic. The UI shows Raw anyway.
		{in: "a while", want: 0},
		{in: "", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := ParseDuration(tc.in)
			if diff := got - tc.want; diff > time.Millisecond || diff < -time.Millisecond {
				t.Errorf("ParseDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestUnescapeUnitName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "sshd.service", want: "sshd.service"},
		{in: `dev-disk-by\x2ddiskseq-1.device`, want: "dev-disk-by-diskseq-1.device"},
		{in: `Basic\x20data\x20partition`, want: "Basic data partition"},
		// An escape we cannot decode is left exactly as it was: it is still
		// the name the user has to type.
		{in: `bad\xZZescape`, want: `bad\xZZescape`},
		{in: `trailing\x`, want: `trailing\x`},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := UnescapeUnitName(tc.in); got != tc.want {
				t.Errorf("UnescapeUnitName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestUnitHelpers(t *testing.T) {
	tests := []struct {
		name                             string
		unit                             Unit
		typ                              string
		failed, running, masked, enabled bool
	}{
		{
			name: "a running enabled service",
			unit: Unit{Name: "sshd.service", Load: "loaded", Active: "active",
				FileState: "enabled"},
			typ: "service", running: true, enabled: true,
		},
		{
			name: "a failed service",
			unit: Unit{Name: "nginx.service", Active: "failed", FileState: "enabled"},
			typ:  "service", failed: true, enabled: true,
		},
		{
			name: "masked through the load state",
			unit: Unit{Name: "apache2.service", Load: "masked", Active: "inactive"},
			typ:  "service", masked: true,
		},
		{
			name: "masked through the unit file state",
			unit: Unit{Name: "apache2.service", Load: "loaded", FileState: "masked-runtime"},
			typ:  "service", masked: true,
		},
		{
			name: "a static unit is not enabled",
			unit: Unit{Name: "journald.service", FileState: "static"},
			typ:  "service",
		},
		{
			name: "a unit with no dot has no type",
			unit: Unit{Name: "weird"},
		},
		{
			name: "an enabled-runtime unit counts as enabled",
			unit: Unit{Name: "x.service", FileState: "enabled-runtime"},
			typ:  "service", enabled: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := tc.unit
			if got := u.Type(); got != tc.typ {
				t.Errorf("Type() = %q, want %q", got, tc.typ)
			}
			if got := u.Failed(); got != tc.failed {
				t.Errorf("Failed() = %v, want %v", got, tc.failed)
			}
			if got := u.Running(); got != tc.running {
				t.Errorf("Running() = %v, want %v", got, tc.running)
			}
			if got := u.Masked(); got != tc.masked {
				t.Errorf("Masked() = %v, want %v", got, tc.masked)
			}
			if got := u.Enabled(); got != tc.enabled {
				t.Errorf("Enabled() = %v, want %v", got, tc.enabled)
			}
		})
	}
}

func TestStateMatch(t *testing.T) {
	failed := Unit{Active: ActiveFailed}
	active := Unit{Active: ActiveActive}
	inactive := Unit{Active: ActiveInactive}
	tests := []struct {
		state State
		want  []bool // failed, active, inactive
	}{
		{state: StateAll, want: []bool{true, true, true}},
		{state: StateFailed, want: []bool{true, false, false}},
		{state: StateActive, want: []bool{false, true, false}},
		{state: StateInactive, want: []bool{false, false, true}},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			got := []bool{tc.state.Match(failed), tc.state.Match(active),
				tc.state.Match(inactive)}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// contains is strings.Contains, kept local so the test file needs no import
// for a single call.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
