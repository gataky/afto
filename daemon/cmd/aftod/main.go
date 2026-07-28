// Command aftod is the afto suggestion daemon and its CLI.
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
