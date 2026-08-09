package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// version is the build version; release builds inject the tag via
// -ldflags "-X main.version=<tag>" (see .github/workflows/release.yml).
var version = "dev"

// restoreMode decides what startup does with a saved session snapshot.
type restoreMode int

const (
	restoreAsk   restoreMode = iota // show the [y/n] restore prompt
	restoreAuto                     // restore without asking (-restore)
	restoreNever                    // discard the snapshot and start fresh (-fresh)
)

// startOptions carries the CLI flags that shape a fresh TUI launch.
type startOptions struct {
	panes   int         // initial pane count (-n)
	layout  string      // initial layout preset (-layout)
	restore restoreMode // -restore / -fresh
}

func defaultStartOptions() startOptions {
	return startOptions{panes: 2, layout: "grid", restore: restoreAsk}
}

// parseCLI dispatches pano's command line. Anything that is not a TUI launch
// (help, version, session subcommand, a parse error) is handled inline and
// reported as start=false with the process exit code; otherwise the parsed
// start options come back with start=true. The ctl subcommand is split off
// by main before this runs.
func parseCLI(args []string) (opts startOptions, start bool, code int) {
	opts = defaultStartOptions()
	if len(args) > 0 {
		switch args[0] {
		case "help":
			usage(os.Stdout)
			return opts, false, 0
		case "version":
			fmt.Println("pano", version)
			return opts, false, 0
		case "session":
			return opts, false, runSessionCmd(args[1:])
		}
	}

	fs := flag.NewFlagSet("pano", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are reported by us, with usage
	panes := fs.Int("n", opts.panes, "number of terminals to open at startup")
	layout := fs.String("layout", opts.layout, "initial layout: grid | main-left")
	restore := fs.Bool("restore", false, "restore the saved session without asking")
	fresh := fs.Bool("fresh", false, "discard the saved session and start fresh")
	help := fs.Bool("h", false, "show usage")
	helpLong := fs.Bool("help", false, "show usage")
	ver := fs.Bool("v", false, "show version")
	verLong := fs.Bool("version", false, "show version")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "pano:", err)
		usage(os.Stderr)
		return opts, false, 2
	}
	if *help || *helpLong {
		usage(os.Stdout)
		return opts, false, 0
	}
	if *ver || *verLong {
		fmt.Println("pano", version)
		return opts, false, 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "pano: unknown command %q\n", fs.Arg(0))
		usage(os.Stderr)
		return opts, false, 2
	}
	if *restore && *fresh {
		fmt.Fprintln(os.Stderr, "pano: -restore and -fresh are mutually exclusive")
		usage(os.Stderr)
		return opts, false, 2
	}
	if *panes < 1 {
		fmt.Fprintln(os.Stderr, "pano: -n must be >= 1")
		usage(os.Stderr)
		return opts, false, 2
	}
	if _, ok := presetByName(*layout); !ok {
		fmt.Fprintf(os.Stderr, "pano: unknown layout %q (want: grid | main-left)\n", *layout)
		usage(os.Stderr)
		return opts, false, 2
	}

	opts.panes = *panes
	opts.layout = *layout
	if *restore {
		opts.restore = restoreAuto
	} else if *fresh {
		opts.restore = restoreNever
	}
	return opts, true, 0
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `pano — multi-terminal TUI manager (%s)

usage:
  pano                     start the TUI (2 terminals by default)
  pano -n N                start with N terminals
  pano -layout PRESET      initial layout: grid | main-left
  pano -restore            restore the saved session without asking
  pano -fresh              discard the saved session and start fresh
  pano session             show the saved session snapshot
  pano session clear       delete the saved session snapshot
  pano ctl notify [text]   red-dot notification from inside a pane
  pano ctl focus           move focus to the current pane
  pano -h, -help           show this help
  pano -v, -version        show version

keys:  F2 new · F3 close · F4 grid/focus · F7 attention · F8 notes · F9 quit
config: ~/.config/pano/agents.toml
`, version)
}

// runSessionCmd implements `pano session [clear]`: inspect or delete the
// saved session snapshot without launching the TUI.
func runSessionCmd(args []string) int {
	path := sessionPath()
	if len(args) > 0 && args[0] == "clear" {
		if path == "" {
			fmt.Fprintln(os.Stderr, "pano session: config directory unknown")
			return 1
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "pano session:", err)
			return 1
		}
		fmt.Println("session snapshot cleared")
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "pano session: unknown argument %q (want: clear)\n", args[0])
		return 2
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "pano session: config directory unknown")
		return 1
	}
	sf, err := loadSession(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pano session:", err)
		return 1
	}
	if sf == nil {
		fmt.Println("no saved session")
		return 0
	}
	fmt.Printf("saved session: %d terminal(s) · %s · %s\n", len(sf.Panes), sf.Preset, sf.SavedAt.Format(time.DateTime))
	for i, p := range sf.Panes {
		focus := ""
		if i == sf.Focus {
			focus = " · focus"
		}
		fmt.Printf("  %d · %s · %s%s\n", i+1, p.Title, p.Dir, focus)
	}
	return 0
}
