// Command hippo is the CLI wrapper around the hippo library.
//
// Subcommands:
//
//	hippo serve     start the embedded web UI on 127.0.0.1:7844
//	hippo version   print version + build info
//	hippo init      create ~/.hippo/config.yaml with defaults
//
// Flags are parsed with stdlib flag — no cobra dependency. hippo's CLI
// surface is intentionally minimal and Pass 9 is the first pass that
// actually ships a usable binary.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]
	var err error
	switch sub {
	case "serve":
		err = runServe(args)
	case "init":
		err = runInit(args)
	case "version":
		err = runVersion(args)
	case "-h", "--help", "help":
		printUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "hippo: unknown subcommand %q\n\n", sub)
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hippo:", err)
		os.Exit(1)
	}
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, `hippo — embedded LLM client with memory and a web UI

Usage:
  hippo <subcommand> [flags]

Subcommands:
  serve    start the web UI
  init     create ~/.hippo/config.yaml with defaults
  version  print version, commit, and Go version

Run "hippo <subcommand> --help" for per-command flags.`)
}
