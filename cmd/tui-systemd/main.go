// Command tui-systemd is a terminal UI for systemd units. It shows what is
// loaded, what is running and what failed, and previews the exact command line
// of every change before running it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/config"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-systemd/internal/systemd"
)

// toolName is the binary name, which is also the configuration directory:
// /etc/tui-systemd/config.toml and ~/.config/tui-systemd/config.toml.
const toolName = "tui-systemd"

// keyJournalLines is the configuration key for the journal backlog size.
const keyJournalLines = "journal_lines"

// version is stamped by the release build (-ldflags "-X main.version=…").
var version = "dev"

// defaults declares the configuration keys tui-systemd understands. Only these
// are read from the environment (TUI_SYSTEMD_SUDO, …).
func defaults() map[string]string {
	return map[string]string{
		config.KeySudo:  "sudo -n",
		config.KeyTheme: "",
		keyJournalLines: fmt.Sprint(systemd.DefaultJournalLines),
	}
}

// options holds the parsed command line.
type options struct {
	demo        bool
	check       bool
	themePath   string
	sudo        string
	showVersion bool
	// sudoSet records whether -sudo was passed, so `--sudo ""` can disable
	// escalation instead of reading as "not given".
	sudoSet bool
}

// parseFlags defines and reads the command line.
func parseFlags(args []string, out *os.File) (options, error) {
	var opts options
	fs := flag.NewFlagSet(toolName, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.BoolVar(&opts.demo, "demo", false,
		"run against a sample machine, without touching the real one")
	fs.BoolVar(&opts.check, "check", false,
		"read the machine and print the parsed model as JSON, then exit "+
			"(no UI, no changes); exit 1 if the backend cannot be read")
	fs.StringVar(&opts.themePath, "theme", "",
		"path to an Omarchy-style colors.toml (overrides the config file)")
	fs.StringVar(&opts.sudo, "sudo", "",
		"privilege escalation prefix, e.g. \"sudo -n\" or \"\" to disable")
	fs.BoolVar(&opts.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(out, "tui-systemd — a terminal UI for systemd units\n\n"+
			"Usage:\n  tui-systemd [flags]\n\nFlags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(out, "\nConfiguration is read from %s, then %s, "+
			"then TUI_SYSTEMD_* in the environment.\n",
			config.SystemPathFor(toolName), config.UserPathFor(toolName))
	}
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "sudo" {
			opts.sudoSet = true
		}
	})
	return opts, nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, toolName+":", err)
		os.Exit(1)
	}
}

// run wires the configuration, the backend and the Bubble Tea program.
func run(args []string) error {
	opts, err := parseFlags(args, os.Stdout)
	if err != nil {
		// flag already printed the reason and the usage.
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if opts.showVersion {
		fmt.Println(toolName, version)
		return nil
	}

	cfg, err := config.Load(config.Options{Tool: toolName, Defaults: defaults()})
	if err != nil {
		return err
	}
	applyOverrides(&cfg, opts)

	backend, err := pickBackend(cfg, opts)
	if err != nil {
		return err
	}

	// --check is the non-interactive path: it reads the machine and prints,
	// and never starts a terminal program.
	// The systemd version is probed once, here, and used by both paths: the
	// header shows it, the app gates its version-dependent views on it, and
	// --check reports it so the smoke test can record it.
	backendCompat := probeCompat(context.Background(), opts.demo)

	if opts.check {
		return runCheck(backend, backendCompat, os.Stdout)
	}

	// The configured theme is handed to the kit through the same variable the
	// user could set by hand, so precedence stays in one place.
	if path := cfg.Theme(); path != "" {
		if err := os.Setenv("TUI_THEME", path); err != nil {
			return err
		}
	}

	app := newApp(backend, theme.New(),
		cfg.Int(keyJournalLines, systemd.DefaultJournalLines), backendCompat)
	program := tea.NewProgram(app, tea.WithAltScreen())
	_, err = program.Run()
	return err
}

// applyOverrides folds the command line into the configuration, which is the
// last and highest-precedence layer.
func applyOverrides(cfg *config.Config, opts options) {
	if opts.themePath != "" {
		cfg.Set(config.KeyTheme, opts.themePath)
	}
	// An explicitly empty -sudo disables escalation, so the flag is applied
	// whenever it was passed, empty value included.
	if opts.sudoSet {
		cfg.Set(config.KeySudo, opts.sudo)
	}
}

// pickBackend returns the demo backend or the real one.
func pickBackend(cfg config.Config, opts options) (systemd.Backend, error) {
	if opts.demo {
		return systemd.NewFake(), nil
	}
	return systemd.New(cfg.SudoPrefix())
}
