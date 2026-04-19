package main

import (
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/mahdi-salmanzade/hippo/web"
)

// version / commit are set via -ldflags at build time:
//   go build -ldflags "-X main.version=0.1.0 -X main.commit=$(git rev-parse --short HEAD)"
var (
	version = "dev"
	commit  = ""
)

func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	_ = fs.Parse(args)

	c := commit
	if c == "" {
		c = readVCS()
	}
	web.Version = version
	fmt.Printf("hippo %s (%s) %s %s/%s\n",
		version, c, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}

// readVCS attempts to pull the commit short SHA from Go's embedded
// build info (set when the module is built in a checked-out repo).
// Returns "" on any failure.
func readVCS() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			if len(s.Value) >= 7 {
				return s.Value[:7]
			}
			return s.Value
		}
	}
	return ""
}
