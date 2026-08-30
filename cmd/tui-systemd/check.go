package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-systemd/internal/systemd"
)

// checkTimeout bounds the whole read. Every call shells out to systemctl or
// journalctl, and a non-interactive check must not hang on a wedged manager.
const checkTimeout = 60 * time.Second

// checkJournalLines is how much log --check pulls. Enough to prove the journal
// is readable, small enough that the JSON stays a report rather than a dump.
const checkJournalLines = 20

// checkReport is what --check prints: the counts a test asserts on, a sample of
// the parsed units, and the result of one real journal read.
//
// It is a report of the read path only. --check never builds and never runs an
// action, so it is safe to run anywhere.
type checkReport struct {
	Tool     string `json:"tool"`
	Version  string `json:"version"`
	Backend  string `json:"backend"`
	Describe string `json:"describe"`

	// Compat is what the version probe found. It is reported rather than
	// asserted: an untested version is a fact about the machine, not a
	// failure of the read path.
	Compat compat.Result `json:"compat"`

	Units   int `json:"units"`
	Active  int `json:"active"`
	Failed  int `json:"failed"`
	Enabled int `json:"enabled"`
	Timers  int `json:"timers"`
	Blame   int `json:"blame"`

	// Sample is the first few units after the tool's own sort order, which is
	// what proves the list was parsed rather than merely fetched.
	Sample []systemd.Unit `json:"sample"`
	// Journal records one real journal read, so the smoke test covers that
	// path too without needing a second invocation.
	Journal journalProbe `json:"journal"`
}

// journalProbe is the outcome of reading one unit's log.
type journalProbe struct {
	// Unit is the unit that was read; empty when the machine had no active
	// service to read, which is not a failure.
	Unit string `json:"unit"`
	// Lines and Bytes describe what came back.
	Lines int `json:"lines"`
	Bytes int `json:"bytes"`
	// Error is why the read failed, when it did. journalctl may be absent or
	// refuse the user, and neither makes the unit list wrong, so this is
	// reported rather than fatal.
	Error string `json:"error,omitempty"`
}

// runCheck exercises the backend's real read paths and prints the parsed model
// as JSON. It returns an error when the unit list cannot be read, which main
// turns into a non-zero exit — so a caller can treat the exit code alone as
// the verdict.
func runCheck(backend systemd.Backend, backendCompat compat.Result,
	out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	units, err := backend.Units(ctx)
	if err != nil {
		return fmt.Errorf("%s backend read failed: %w", backend.Name(), err)
	}
	systemd.SortUnits(units)

	report := checkReport{
		Tool:     toolName,
		Version:  version,
		Backend:  backend.Name(),
		Describe: backend.Describe(),
		Compat:   backendCompat,
		Units:    len(units),
	}
	for _, unit := range units {
		switch {
		case unit.Failed():
			report.Failed++
		case unit.Running():
			report.Active++
		}
		if unit.Enabled() {
			report.Enabled++
		}
	}
	if len(units) > 5 {
		report.Sample = units[:5]
	} else {
		report.Sample = units
	}

	// Timers and blame are optional views: a host without systemd-analyze
	// still has a correct unit list, so a failure here is left out of the
	// counts rather than made fatal.
	if timers, err := backend.Timers(ctx); err == nil {
		report.Timers = len(timers)
	}
	if blame, err := backend.Blame(ctx, 20); err == nil {
		report.Blame = len(blame)
	}

	report.Journal = probeJournal(ctx, backend, units)

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

// probeJournal reads the log of the first active service it finds. Picking a
// unit from the machine's own list rather than naming one keeps the check
// portable: the three distros in the lab agree on no single service name.
func probeJournal(ctx context.Context, backend systemd.Backend,
	units []systemd.Unit) journalProbe {
	var target string
	for _, unit := range units {
		if unit.Running() && unit.Type() == "service" {
			target = unit.Name
			break
		}
	}
	if target == "" {
		return journalProbe{}
	}
	text, err := backend.Journal(ctx, target, checkJournalLines)
	probe := journalProbe{Unit: target, Bytes: len(text)}
	if err != nil {
		probe.Error = err.Error()
		return probe
	}
	if trimmed := strings.TrimRight(text, "\n"); trimmed != "" {
		probe.Lines = strings.Count(trimmed, "\n") + 1
	}
	return probe
}
