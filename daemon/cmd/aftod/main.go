// Command aftod is the afto suggestion daemon and its CLI.
//
// One binary, two roles:
//
//   - "aftod serve" runs the daemon: a per-user process that owns the
//     SQLite history store and answers suggestion queries over a unix
//     socket. It is started lazily by the zsh plugin on first use (there is
//     no launchd/systemd unit) and exits on its own after an idle period.
//
//   - "import", "query", "list" and "ping" act as clients of a running
//     daemon (or, for import and list, operate on the store directly) for
//     bootstrap, pickers, testing and diagnostics.
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

func run(cmd string, args []string) error {
	switch cmd {
	case "serve":
		return cmdServe(args)
	case "import":
		return cmdImport(args)
	case "query":
		return cmdQuery(args)
	case "list":
		return cmdList(args)
	case "ping":
		return cmdPing(args)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: aftod <command> [flags]

commands:
  serve     run the suggestion daemon (--daemonize to detach)
  import    import a zsh HISTFILE into the store
  query     send a suggest request to a running daemon
  list      print frecency-ranked history, one per line (for fzf and friends)
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
	case "serve", "import", "query", "list", "ping":
		err = run(os.Args[1], os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "aftod: %v\n", err)
		os.Exit(1)
	}
}
