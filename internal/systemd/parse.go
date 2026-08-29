package systemd

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// unitJSON is one entry of `systemctl list-units --output=json`.
type unitJSON struct {
	Unit        string `json:"unit"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
}

// unitFileJSON is one entry of `systemctl list-unit-files --output=json`.
// Preset is a pointer because systemd emits a JSON null for a unit with no
// vendor preset, which would not decode into a string.
type unitFileJSON struct {
	UnitFile string  `json:"unit_file"`
	State    string  `json:"state"`
	Preset   *string `json:"preset"`
}

// ParseUnits reads `systemctl list-units --all --output=json`.
//
// JSON rather than the text table on purpose: the table is column-aligned,
// pads a leading bullet onto failed units and truncates the description to the
// terminal width, none of which is a contract. `--output=json` is.
func ParseUnits(out string) ([]Unit, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var raw []unitJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("systemd: cannot read the unit list: %w", err)
	}
	units := make([]Unit, 0, len(raw))
	for _, r := range raw {
		if r.Unit == "" {
			continue
		}
		units = append(units, Unit{
			Name:        UnescapeUnitName(r.Unit),
			Load:        r.Load,
			Active:      r.Active,
			Sub:         r.Sub,
			Description: UnescapeUnitName(r.Description),
		})
	}
	return units, nil
}

// ParseUnitFiles reads `systemctl list-unit-files --output=json`, returning a
// lookup keyed by unit name.
func ParseUnitFiles(out string) (map[string]Unit, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return map[string]Unit{}, nil
	}
	var raw []unitFileJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("systemd: cannot read the unit file list: %w", err)
	}
	files := make(map[string]Unit, len(raw))
	for _, r := range raw {
		if r.UnitFile == "" {
			continue
		}
		name := UnescapeUnitName(r.UnitFile)
		preset := ""
		if r.Preset != nil {
			preset = *r.Preset
		}
		files[name] = Unit{Name: name, FileState: r.State, Preset: preset}
	}
	return files, nil
}

// MergeUnits folds the unit-file states into the runtime list.
//
// The two lists genuinely differ, and both halves matter. A unit can be
// loaded and running with no unit file at all (a device, or a mount systemd
// generated), and a unit file can exist for something never loaded. The
// running list is the spine, and any unit file with no running counterpart is
// appended as inactive, because "installed but never started" is exactly the
// state someone opens this tool to find.
func MergeUnits(units []Unit, files map[string]Unit) []Unit {
	seen := make(map[string]bool, len(units))
	merged := make([]Unit, 0, len(units)+len(files))
	for _, u := range units {
		if file, ok := files[u.Name]; ok {
			u.FileState = file.FileState
			u.Preset = file.Preset
		}
		seen[u.Name] = true
		merged = append(merged, u)
	}
	for name, file := range files {
		if seen[name] {
			continue
		}
		// A template unit file ("getty@.service") is not an instantiable unit;
		// listing it would offer actions that cannot work.
		if strings.Contains(name, "@.") {
			continue
		}
		merged = append(merged, Unit{
			Name: name, Load: "stub", Active: ActiveInactive, Sub: "dead",
			FileState: file.FileState, Preset: file.Preset,
		})
	}
	SortUnits(merged)
	return merged
}

// UnescapeUnitName reverses systemd's `\xNN` escaping, which shows up in
// device and mount unit names ("dev-disk-by\x2ddiskseq-1.device"). An escape
// that does not parse is left exactly as it was: a name we cannot decode is
// still the name the user has to type.
func UnescapeUnitName(name string) string {
	if !strings.Contains(name, `\x`) {
		return name
	}
	var b strings.Builder
	b.Grow(len(name))
	for i := 0; i < len(name); {
		if name[i] == '\\' && i+3 < len(name) && name[i+1] == 'x' {
			if v, err := strconv.ParseUint(name[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 4
				continue
			}
		}
		b.WriteByte(name[i])
		i++
	}
	return b.String()
}

// timerJSON is one entry of `systemctl list-timers --output=json`. next and
// last are microseconds since the epoch, 0 when the timer has no next run or
// has never fired.
//
// The left and passed fields are deliberately ignored: systemd emits its raw
// internal values there, which are not the elapsed times the text table shows.
// The UI computes both against the current clock instead, which also keeps the
// rendering locale-independent.
type timerJSON struct {
	Next      int64  `json:"next"`
	Last      int64  `json:"last"`
	Unit      string `json:"unit"`
	Activates string `json:"activates"`
}

// ParseTimers reads `systemctl list-timers --all --output=json`.
//
// JSON is the only format this parses. The text table right-aligns its time
// columns against headers narrower than the data, so a value can start well to
// the left of its own header: there is no column offset that slices those rows
// correctly, and a parser that appears to work while quietly mangling
// timestamps is worse than none. Callers report the systemd version
// requirement when this fails.
func ParseTimers(out string) ([]Timer, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var raw []timerJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("systemd: cannot read the timer list "+
			"(needs systemd 250 or newer for `list-timers --output=json`): %w", err)
	}
	timers := make([]Timer, 0, len(raw))
	for _, r := range raw {
		if r.Unit == "" {
			continue
		}
		timers = append(timers, Timer{
			Next:      microsToTime(r.Next),
			Last:      microsToTime(r.Last),
			Unit:      UnescapeUnitName(r.Unit),
			Activates: UnescapeUnitName(r.Activates),
		})
	}
	return timers, nil
}

// microsToTime converts systemd's microsecond epoch to a time. Zero and the
// "infinity" sentinel both mean "no such moment" and yield the zero time.
func microsToTime(micros int64) time.Time {
	if micros <= 0 || micros == math.MaxInt64 {
		return time.Time{}
	}
	return time.UnixMicro(micros)
}

// ParseBlame reads `systemd-analyze blame`, whose lines are a duration
// right-aligned against a unit name: "  2min 37.902s plocate-updatedb.service".
func ParseBlame(out string) []BlameEntry {
	var entries []BlameEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// The unit is the last field; everything before it is the duration.
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		raw := strings.TrimSpace(line[:i])
		unit := strings.TrimSpace(line[i+1:])
		if raw == "" || unit == "" || !strings.Contains(unit, ".") {
			continue
		}
		entries = append(entries, BlameEntry{
			Duration: ParseDuration(raw), Raw: raw, Unit: unit,
		})
	}
	return entries
}

// ParseDuration reads systemd's compound duration format: "1h 39min 446ms",
// "2min 37.902s", "55.549s", "4.227s". An unrecognised unit is skipped rather
// than failing the line, because the number is only used for sorting and the
// text the user sees is kept verbatim in BlameEntry.Raw.
func ParseDuration(s string) time.Duration {
	var total time.Duration
	for _, field := range strings.Fields(s) {
		// Split the number from its unit suffix.
		i := 0
		for i < len(field) && (field[i] >= '0' && field[i] <= '9' || field[i] == '.') {
			i++
		}
		if i == 0 || i == len(field) {
			continue
		}
		value, err := strconv.ParseFloat(field[:i], 64)
		if err != nil {
			continue
		}
		var unit time.Duration
		switch field[i:] {
		case "us", "µs":
			unit = time.Microsecond
		case "ms":
			unit = time.Millisecond
		case "s":
			unit = time.Second
		case "min":
			unit = time.Minute
		case "h":
			unit = time.Hour
		case "d":
			unit = 24 * time.Hour
		default:
			continue
		}
		total += time.Duration(value * float64(unit))
	}
	return total
}
