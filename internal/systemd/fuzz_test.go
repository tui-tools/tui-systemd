package systemd

import (
	"strings"
	"testing"
	"time"
)

// This package is where output tui-systemd did not write becomes the list the
// screens draw and the unit names the commands are built from: `systemctl
// list-units`, `list-unit-files` and `list-timers` in JSON, and the duration
// table `systemd-analyze blame` prints. `go test` runs the seeds below on every
// commit, and `go test -run=^$ -fuzz=FuzzParseUnits ./internal/systemd`
// explores past them locally — see tui-kit/templates/FUZZING.md for the family
// rule.
//
// The seeds are the captured output the table tests in parse_test.go use, so
// the corpus starts on the real shapes and mutates from there instead of
// guessing them.

// seed adds the given captured samples to the corpus, plus the shapes a real
// capture never has: nothing, whitespace, a truncated document, a bare scalar.
func seed(f *testing.F, samples ...string) {
	f.Helper()
	for _, sample := range samples {
		f.Add(sample)
	}
	f.Add("")
	f.Add("   \n")
	f.Add("[]")
	f.Add("[{")
	f.Add("null")
	f.Add(`[{"unit":""}]`)
	f.Add(`[{"unit":"a\\x2db.service"}]`)
}

// checkUnitName asserts what every caller of a parsed unit is allowed to
// assume: a name it can put in front of `systemctl`, never a blank one.
func checkUnitName(t *testing.T, name string) {
	t.Helper()
	if name == "" {
		t.Fatalf("unit with no name")
	}
}

func FuzzParseUnits(f *testing.F) {
	seed(f, listUnitsJSON)
	f.Fuzz(func(t *testing.T, out string) {
		units, err := ParseUnits(out)
		if err != nil {
			if units != nil {
				t.Fatalf("failed to read the list and still returned %d units", len(units))
			}
			return
		}
		for _, unit := range units {
			checkUnitName(t, unit.Name)
			// What the list view reads off every row.
			_, _, _, _ = unit.Type(), unit.Failed(), unit.Running(), unit.Masked()
		}
	})
}

func FuzzParseUnitFiles(f *testing.F) {
	seed(f, listUnitFilesJSON)
	f.Fuzz(func(t *testing.T, out string) {
		files, err := ParseUnitFiles(out)
		if err != nil {
			if files != nil {
				t.Fatalf("failed to read the list and still returned %d files", len(files))
			}
			return
		}
		for name, unit := range files {
			checkUnitName(t, name)
			// The map is keyed by the unit's own name: a lookup that disagrees
			// with the value would attach one unit's file state to another.
			if unit.Name != name {
				t.Fatalf("unit file keyed as %q carries the name %q", name, unit.Name)
			}
		}
	})
}

// FuzzMergeUnits folds one parsed list into the other, which is what every
// refresh does and where a unit that exists in only one of them is decided.
func FuzzMergeUnits(f *testing.F) {
	f.Add(listUnitsJSON, listUnitFilesJSON)
	f.Add("", "")
	f.Add(listUnitsJSON, "")
	f.Add("", listUnitFilesJSON)
	f.Fuzz(func(t *testing.T, unitsOut, filesOut string) {
		units, err := ParseUnits(unitsOut)
		if err != nil {
			return
		}
		files, err := ParseUnitFiles(filesOut)
		if err != nil {
			return
		}
		merged := MergeUnits(units, files)
		if len(merged) < len(units) {
			t.Fatalf("the merge dropped a running unit: %d in, %d out", len(units), len(merged))
		}
		for _, unit := range merged {
			checkUnitName(t, unit.Name)
			// A template unit file is not something `systemctl start` can act
			// on, so it never reaches the list.
			if strings.Contains(unit.Name, "@.") {
				t.Fatalf("template unit reached the list: %q", unit.Name)
			}
		}
	})
}

func FuzzParseTimers(f *testing.F) {
	seed(f, listTimersJSON)
	f.Fuzz(func(t *testing.T, out string) {
		timers, err := ParseTimers(out)
		if err != nil {
			if timers != nil {
				t.Fatalf("failed to read the list and still returned %d timers", len(timers))
			}
			return
		}
		for _, timer := range timers {
			checkUnitName(t, timer.Unit)
			// "no such moment" is the zero time, never a moment before the
			// epoch: the timers screen prints a dash for one and a date for
			// the other.
			for _, moment := range []time.Time{timer.Next, timer.Last} {
				if !moment.IsZero() && moment.UnixMicro() <= 0 {
					t.Fatalf("%q carries a moment that is neither zero nor real: %v",
						timer.Unit, moment)
				}
			}
		}
	})
}

func FuzzParseBlame(f *testing.F) {
	seed(f, blameOutput)
	f.Add("1h 39min 446ms fstrim.service\n")
	f.Add("        4.227s sys-module-fuse.device")
	f.Fuzz(func(t *testing.T, out string) {
		for _, entry := range ParseBlame(out) {
			checkUnitName(t, entry.Unit)
			if strings.ContainsAny(entry.Unit, " \t\r\n") {
				t.Fatalf("unit is not a bare word: %q", entry.Unit)
			}
			if entry.Raw == "" {
				t.Fatalf("%q has no duration text to print", entry.Unit)
			}
			// The duration is only used to order the table, so it has to be a
			// length of time: a negative one sorts the slowest unit last.
			if entry.Duration < 0 {
				t.Fatalf("%q took a negative time: %v", entry.Unit, entry.Duration)
			}
		}
	})
}

func FuzzParseDuration(f *testing.F) {
	for _, s := range []string{
		"1h 39min 446ms", "2min 37.902s", "55.549s", "4.227s", "311ms",
		"", "  ", "s", "1", "1x", "1.2.3s", "1us", "1µs", "1d",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if d := ParseDuration(s); d < 0 {
			t.Fatalf("%q parsed as a negative duration: %v", s, d)
		}
	})
}

// FuzzUnescapeUnitName is the one that matters most here: whatever it returns
// is the name that goes in front of `systemctl`, so it may shrink a name and
// leave an escape it cannot read alone, but it may never invent one.
func FuzzUnescapeUnitName(f *testing.F) {
	for _, s := range []string{
		`dev-disk-by\x2ddiskseq-1\x2dpart1.device`,
		`XPG GAMMIX S70 BLADE Basic\x20data\x20partition`,
		`sshd.service`, ``, `\x`, `\xzz`, `\x2`, `\\x2d`, `\x00`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		got := UnescapeUnitName(name)
		if len(got) > len(name) {
			t.Fatalf("decoding %q grew it to %q", name, got)
		}
		if !strings.Contains(name, `\x`) && got != name {
			t.Fatalf("a name with nothing to decode came back as %q", got)
		}
	})
}
