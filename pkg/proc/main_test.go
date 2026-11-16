package proc

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/behaviorengineering/processconcierge/internal/proctest"
)

// goParentEnv makes the test binary play a Go parent that starts a spawning helper under the reaper named in
// reaperEnv, then sleeps until the test kills it outright.
const (
	goParentEnv = "PROCESSCONCIERGE_TEST_GO_PARENT"
	reaperEnv   = "PROCESSCONCIERGE_TEST_REAPER"
)

// reaperBinary is the processconcierge command, built once for the reaper tests.
var reaperBinary string

func TestMain(m *testing.M) {
	if file := os.Getenv(goParentEnv); file != "" {
		os.Exit(playGoParent(file))
	}
	proctest.RunHelperIfAsked()
	dir, err := os.MkdirTemp("", "processconcierge-proc-test-")
	if err != nil {
		panic(err)
	}
	if reaperBinary, err = proctest.BuildCommand(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func playGoParent(file string) int {
	env := []string{}
	for _, kv := range proctest.Env(proctest.Spawn, file) {
		if len(kv) < len(goParentEnv) || kv[:len(goParentEnv)] != goParentEnv {
			env = append(env, kv)
		}
	}
	if _, err := (Config{Path: os.Args[0], Env: env, Reaper: os.Getenv(reaperEnv)}).Start(context.Background()); err != nil {
		return 3
	}
	time.Sleep(time.Minute)
	return 0
}
