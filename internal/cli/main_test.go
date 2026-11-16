package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// helperEnv tells the test binary to act as a helper process instead of running the tests, so the Windows tests have
// programs to supervise without a shell: "sleep" sleeps a minute, "spawn" starts a sleeping grandchild, writes its pid
// to the file in pidFileEnv, then sleeps, and "spawn-exit" does the same but exits at once with code 4.
const (
	helperEnv  = "PROCESSCONCIERGE_TEST_HELPER"
	pidFileEnv = "PROCESSCONCIERGE_TEST_PIDFILE"
)

// binary is the processconcierge command, built once for these tests, so each test drives it as its owner would.
var binary string

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode))
	}
	dir, err := os.MkdirTemp("", "processconcierge-test-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "processconcierge")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "../../cmd/processconcierge") //nolint:gosec // G204: builds this module's own command.
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func runHelper(mode string) int {
	if mode == "sleep" {
		time.Sleep(time.Minute)
		return 0
	}
	grandchild := exec.Command(os.Args[0]) //nolint:gosec // G204: the test binary itself.
	grandchild.Env = append(os.Environ(), helperEnv+"=sleep")
	if err := grandchild.Start(); err != nil {
		return 3
	}
	//nolint:gosec // G703: the test itself names a file in its own temp folder.
	if err := os.WriteFile(os.Getenv(pidFileEnv), []byte(strconv.Itoa(grandchild.Process.Pid)+"\n"), 0o600); err != nil {
		return 3
	}
	if mode == "spawn-exit" {
		return 4
	}
	time.Sleep(time.Minute)
	return 0
}

func TestTheGuideVersionAndAnUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	if code := Execute(context.Background(), nil, &out, &out, "v1.2.3"); code != ExitOK {
		t.Fatalf("bare invoke exited %d", code)
	}
	if lines := strings.Count(out.String(), "\n"); lines < 10 || !strings.HasPrefix(out.String(), "processconcierge v1.2.3\n") {
		t.Fatalf("guide:\n%s", out.String())
	}
	out.Reset()
	if code := Execute(context.Background(), []string{"version"}, &out, &out, "v1.2.3"); code != ExitOK || out.String() != "processconcierge v1.2.3\n" {
		t.Fatalf("version exited %d: %q", code, out.String())
	}
	out.Reset()
	if code := Execute(context.Background(), []string{"nope"}, &out, &out, "v1.2.3"); code != ExitUsage {
		t.Fatalf("an unknown command exited %d", code)
	}
}
