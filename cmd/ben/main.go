// Command ben is the daemon CLI (SPEC §11), and the assembly: components the
// import boundaries keep apart are bound to each other here.
//
// Implemented so far: `config effective` (B01), `run` (B11) — what turns the
// assembled components into a process — and `status` over the state files `run`
// writes.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
)

// structural is the whole of `ben config effective`'s adapter validation: the
// kind lookups and both pure Structural checks, and deliberately nothing after
// them (see structuralKinds in runtime.go, which `ben run` continues past).
//
// Structural only — never New or Ready — so inspecting a config needs no
// credentials, no network, and no installed harness, which is what lets
// `make workflow-check` validate the dogfood WORKFLOW.md in CI (SPEC §5.8).
func structural(def *config.WorkflowDefinition) error {
	_, _, err := structuralKinds(def, registry.Tracker, registry.Runner)
	return err
}

const usage = `ben (Branch, Execute, Notify) — a daemon that works on GitHub Issues autonomously with coding agents

Usage:
  ben run [path]                     Run the daemon for the workflow at path.
                                          Graceful shutdown on SIGTERM/SIGINT.
  ben status [--json] [path]         Show the white-box state files for the workflow at
                                          path: current runs, attempts, next timers, and the
                                          transition log tail. Read-only, and safe to run
                                          while the daemon is running.
  ben config effective [--json] [path]
                                          Print the fully-resolved configuration with
                                          per-field provenance; secrets are redacted.

path defaults to ./WORKFLOW.md.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run carries all argument handling and exit codes so command tests can
// exercise them without exec'ing a built binary. Exit codes: 0 success,
// 1 operational failure, 2 usage error.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "config":
		if len(args) < 2 || args[1] != "effective" {
			fmt.Fprint(stderr, "unknown config subcommand; did you mean `ben config effective`?\n")
			return 2
		}
		return runConfigEffective(args[2:], stdout, stderr)
	case "run":
		return runDaemon(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runConfigEffective(args []string, stdout, stderr io.Writer) int {
	// ContinueOnError, not ExitOnError: a bad flag must report through the
	// usage-error exit code, not os.Exit from inside the flag package.
	fs := flag.NewFlagSet("config effective", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON instead of annotated text")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2 // the FlagSet already reported the flag error to stderr
	}

	// At most one positional path; silently ignoring extras would hide the
	// classic `ben config effective path --json` misplacement.
	if fs.NArg() > 1 {
		fmt.Fprintf(stderr, "ben: config effective takes at most one path argument; got %d: %s\n", fs.NArg(), strings.Join(fs.Args(), " "))
		for _, a := range fs.Args()[1:] {
			if strings.HasPrefix(a, "-") {
				fmt.Fprint(stderr, "(flags must come before the path: `ben config effective --json [path]`)\n")
				break
			}
		}
		return 2
	}

	path := "WORKFLOW.md"
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}

	def, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "ben: %v\n", err)
		return 1
	}
	if err := structural(def); err != nil {
		// Refusals that carry an offending value carry it as data; rendering
		// decides by provenance whether showing it would leak a secret
		// (SPEC §5.8).
		fmt.Fprintf(stderr, "ben: %s\n", config.RenderRefusal(def, err))
		return 1
	}

	if *asJSON {
		out, err := config.EffectiveJSON(def)
		if err != nil {
			fmt.Fprintf(stderr, "ben: rendering JSON: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(out))
		return 0
	}
	fmt.Fprint(stdout, config.EffectiveText(def))
	return 0
}
