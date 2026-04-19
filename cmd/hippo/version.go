package main

import (
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/mahdi-salmanzade/hippo/internal/version"
	"github.com/mahdi-salmanzade/hippo/web"
)

func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	_ = fs.Parse(args)

	commit := version.Commit
	if commit == "" || commit == "unknown" {
		if vcs := readVCS(); vcs != "" {
			commit = vcs
		}
	}
	web.Version = version.Version
	fmt.Printf("hippo %s (commit: %s, go: %s, %s/%s)\n",
		version.Version,
		commit,
		strings.TrimPrefix(runtime.Version(), "go"),
		runtime.GOOS, runtime.GOARCH)
	return nil
}

// readVCS pulls the commit short SHA from Go's embedded build info
// (populated when the module is built in a checked-out repo). Returns
// "" on any failure; callers fall back to version.Commit.
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
