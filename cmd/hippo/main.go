// Command hippo is a CLI wrapper around the hippo library. The CLI is
// a stub at this stage; a real subcommand surface (run, ask, budget,
// memory) will be added in v0.4 per the roadmap.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "hippo CLI: not yet implemented (see roadmap v0.4)")
	os.Exit(1)
}
