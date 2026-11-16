// Command processconcierge supervises child processes; see internal/cli.
package main

import (
	"context"
	"os"
	"runtime/debug"
	"strings"

	"github.com/behaviorengineering/processconcierge/internal/cli"
)

// version is set by GoReleaser with -ldflags "-X main.version=...". A plain go install applies no ldflags, so
// reportVersion falls back to the module version in the build info.
var version = "dev"

func main() {
	os.Exit(cli.Execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr, reportVersion()))
}

func reportVersion() string {
	moduleVersion := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		moduleVersion = info.Main.Version
	}
	return resolveVersion(version, moduleVersion)
}

func resolveVersion(ldflag, moduleVersion string) string {
	if v := strings.TrimSpace(ldflag); v != "" && v != "dev" {
		return v
	}
	if v := strings.TrimSpace(moduleVersion); v != "" && v != "(devel)" {
		return v
	}
	return "dev"
}
