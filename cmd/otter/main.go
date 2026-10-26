// Command otter is the Otter Labs Cloud binary. One binary, four subcommands
// (architecture.md §3):
//
//	otter api       the control plane's HTTP API and portal
//	otter worker    the node-local worker that runs operations
//	otter admin     operator commands, run over SSH (Q16)
//	otter migrate   apply the platform database's migrations
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ottermq/otter_cloud/internal/buildinfo"
	"github.com/ottermq/otter_cloud/internal/config"
)

// getenv is os.Getenv, replaceable in tests.
var getenv = os.Getenv

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// command is one subcommand. run receives the arguments after the
// subcommand's name and returns the process exit code.
type command struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) int
}

func commands() []command {
	return []command{
		{"api", "serve the control plane's HTTP API and portal", runAPI},
		{"worker", "claim and run this node's operations", runWorker},
		{"admin", "operator commands (invites, users, nodes, operations)", runAdmin},
		{"migrate", "apply the platform database's migrations", runMigrate},
		{"version", "print the version", runVersion},
	}
}

// run dispatches to a subcommand. Exit codes: 0 success, 1 failure, 2 usage.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		usage(stdout)
		return 0
	}
	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "otter: unknown command %q\n\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: otter <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-8s  %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, `Run "otter <command> -h" for a command's flags.`)
}

// newFlagSet returns a flag set whose usage names the subcommand and says
// what it does. Errors are returned, never handled by exiting, so run stays
// testable.
func newFlagSet(name, summary string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("otter "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: otter %s [flags]\n\n%s.\n", name, summary)
		var any bool
		fs.VisitAll(func(*flag.Flag) { any = true })
		if any {
			fmt.Fprintln(fs.Output(), "\nFlags:")
			fs.PrintDefaults()
		}
	}
	return fs
}

// parse parses a subcommand's flags. It returns done when the caller should
// return code straight away: -h was asked for (0) or the flags were wrong (2).
func parse(fs *flag.FlagSet, args []string) (code int, done bool) {
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0, true
	case err != nil:
		return 2, true
	case fs.NArg() > 0:
		fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		fs.Usage()
		return 2, true
	}
	return 0, false
}

// loadConfig reads the settings a subcommand needs. On failure it prints one
// line per problem, each naming its variable, and returns false.
func loadConfig(name string, needs config.Needs, stderr io.Writer) (*config.Config, bool) {
	cfg, err := config.Load(getenv, needs)
	if err != nil {
		fmt.Fprintf(stderr, "otter %s: invalid configuration:\n", name)
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(stderr, "  - %s\n", line)
		}
		return nil, false
	}
	return cfg, true
}

// notYet reports a subcommand whose behaviour lands in a later milestone or issue.
func notYet(stderr io.Writer, name, issue string) int {
	fmt.Fprintf(stderr, "otter %s: not implemented yet (%s)\n", name, issue)
	return 1
}

func runAPI(args []string, _, stderr io.Writer) int {
	fs := newFlagSet("api", "Serve the control plane's HTTP API and portal", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	if _, ok := loadConfig("api", config.Needs{Database: true, HTTP: true, PublicURL: true}, stderr); !ok {
		return 1
	}
	return notYet(stderr, "api", "M1")
}

func runWorker(args []string, _, stderr io.Writer) int {
	fs := newFlagSet("worker", "Claim and run this node's operations", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	if _, ok := loadConfig("worker", config.Needs{Database: true}, stderr); !ok {
		return 1
	}
	return notYet(stderr, "worker", "M3")
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("version", "Print the version", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	fmt.Fprintln(stdout, buildinfo.Version())
	return 0
}
