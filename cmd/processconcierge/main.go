// Command processconcierge supervises child processes; see internal/cli.
package main

import (
	"context"
	"os"
	"runtime/debug"

	"github.com/behaviorengineering/processconcierge/internal/cli"
)

// buildVersion is set at release time with -ldflags "-X main.buildVersion=v1.2.3".
var buildVersion string

func main() {
	os.Exit(cli.Execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr, version()))
}

func version() string {
	if buildVersion != "" && buildVersion != "dev" {
		return buildVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
