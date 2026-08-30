package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tui-tools/tui-kit/config"
	"github.com/tui-tools/tui-kit/report"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-systemd/internal/systemd"
)

// runReport prints the block a bug report needs and exits. Everything generic
// — the kit version, the distribution, the kernel, the terminal, where the
// binary came from — is collected by the kit, so the whole family answers
// --report in the same shape. What this function adds is the part only
// tui-systemd knows: the version the compat probe read off systemd, which of
// the two optional binaries the tool would have driven are actually here, and
// how large a journal backlog the log panel asks for.
//
// It never reads the machine. --check is the flag that does that; a report has
// to work where the read fails, because the failing read may be the bug. For
// the same reason a host without systemd at all still gets a report, with the
// selection error as one of its lines: "there is nothing here to drive" is a
// bug report, not a refusal.
func runReport(cfg config.Config, opts options, out io.Writer) error {
	palette, _ := theme.ResolvePalette()

	// selected is the backend's own name; the package-level backendName is
	// the manifest's, and in --demo the two differ on purpose.
	var selected, selectError string
	if backend, err := pickBackend(cfg, opts); err != nil {
		selectError = err.Error()
	} else {
		selected = backend.Name()
	}

	// The same probe --check and the header use. There is one version probe in
	// this tool and this is it.
	backendCompat := probeCompat(context.Background(), opts.demo)

	info := report.Info{
		Tool:           toolName,
		Version:        version,
		Backend:        selected,
		BackendVersion: backendCompat.Version,
		BackendDetail:  backendCompat.Detail,
		Demo:           opts.demo,
		Sudo:           cfg.String(config.KeySudo, ""),
		Theme:          palette.Name,
	}
	if opts.demo {
		// The fake imitates the one real backend this tool has, and saying so
		// is what keeps a demo report readable next to a live one. The name is
		// taken from the constant the manifest uses rather than from the
		// fake's own name, because a fake is free to call itself "demo".
		info.Backend = "demo"
		info.Extra = append(info.Extra, report.Field{
			Key: "demo backend", Value: backendName,
		})
	}
	info.Extra = append(info.Extra,
		report.Field{Key: "helpers", Value: describeHelpers(opts.demo)},
		report.Field{Key: "journal lines", Value: strconv.Itoa(
			cfg.Int(keyJournalLines, systemd.DefaultJournalLines))},
	)
	if selectError != "" {
		info.Extra = append(info.Extra, report.Field{
			Key: "backend error", Value: selectError,
		})
	}

	_, err := io.WriteString(out, report.Render(info))
	return err
}

// describeHelpers reports the two binaries the tool drives besides systemctl.
// Neither is required, and a report that says only "systemd 257" leaves the
// reader guessing whether the journal panel was empty because the parser broke
// or because journalctl is not installed — which is most of the "the log is
// blank" reports. It resolves paths and starts nothing.
func describeHelpers(demo bool) string {
	if demo {
		// Nothing on the host was consulted to build the sample machine, so
		// reporting what is installed here would describe the wrong machine.
		return "not consulted (demo)"
	}
	helpers := []struct {
		name    string
		present bool
	}{
		{"journalctl", systemd.JournalAvailable()},
		{"systemd-analyze", systemd.AnalyzeAvailable()},
	}
	parts := make([]string, 0, len(helpers))
	for _, h := range helpers {
		state := "absent"
		if h.present {
			state = "present"
		}
		parts = append(parts, h.name+" "+state)
	}
	return strings.Join(parts, ", ")
}

// reportUsage is the flag's one-line help, kept here next to what it prints.
var reportUsage = fmt.Sprintf(
	"print the versions and machine facts a bug report needs, then exit "+
		"(no UI, no privileges, nothing about you: paste it into a %s issue)",
	toolName)
