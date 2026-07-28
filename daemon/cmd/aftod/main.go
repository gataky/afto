// Command aftod is the afto suggestion daemon and its CLI.
//
// One binary, two roles:
//
//   - "aftod serve" runs the daemon: a per-user process that owns the
//     SQLite history store and answers suggestion queries over a unix
//     socket. It is started lazily by the zsh plugin on first use (there is
//     no launchd/systemd unit) and exits on its own after an idle period.
//
//   - "import", "query" and "ping" act as clients of a running daemon (or,
//     for import, operate on the store directly) for bootstrap, testing and
//     diagnostics.
//
// The interactive consumer is shell/zsh/afto.plugin.zsh, which talks to the
// daemon from inside zsh. Architecture: DESIGN.md; Phase 1 scope and
// acceptance gates: plans/phase-1.md.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

// run dispatches subcommands; implemented incrementally (see plans/phase-1.md).
var run = func(cmd string, args []string) error {
	return fmt.Errorf("%s: not implemented yet", cmd)
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: aftod <command> [flags]

commands:
  serve     run the suggestion daemon (--daemonize to detach)
  import    import a zsh HISTFILE into the store
  query     send a suggest request to a running daemon
  ping      check daemon liveness
  version   print version
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "serve", "import", "query", "ping":
		err = run(os.Args[1], os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "aftod: %v\n", err)
		os.Exit(1)
	}
}
