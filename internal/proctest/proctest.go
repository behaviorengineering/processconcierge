// Package proctest holds what the process tests share: helper processes the test binary turns into when asked, so
// every platform has programs to supervise without a shell, and the waits and reads the tests repeat.
package proctest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// HelperEnv names the helper a test binary should act as, and PidFileEnv the file a spawning helper writes its
// grandchild's pid to.
const (
	HelperEnv  = "PROCESSCONCIERGE_TEST_HELPER"
	PidFileEnv = "PROCESSCONCIERGE_TEST_PIDFILE"
)

// Helper modes.
const (
	// Sleep sleeps a minute.
	Sleep = "sleep"
	// Spawn starts a sleeping grandchild, writes its pid to the file in PidFileEnv, then sleeps.
	Spawn = "spawn"
	// SpawnExit starts a sleeping grandchild, writes its pid, then exits at once with code 4.
	SpawnExit = "spawn-exit"
	// Exit4 exits at once with code 4.
	Exit4 = "exit-4"
)

// Deadline is how long the waits in these helpers allow; Windows runners can be slow to start processes.
const Deadline = 15 * time.Second

// RunHelperIfAsked turns the test binary into the helper HelperEnv names and exits, or returns when none is named.
// Each TestMain calls it first.
func RunHelperIfAsked() {
	mode := os.Getenv(HelperEnv)
	if mode == "" {
		return
	}
	os.Exit(runHelper(mode))
}

func runHelper(mode string) int {
	switch mode {
	case Sleep:
		time.Sleep(time.Minute)
		return 0
	case Exit4:
		return 4
	case Spawn, SpawnExit:
		grandchild := exec.Command(os.Args[0]) //nolint:gosec // G204: the test binary itself.
		grandchild.Env = append(os.Environ(), HelperEnv+"="+Sleep)
		if err := grandchild.Start(); err != nil {
			return 3
		}
		//nolint:gosec // G703: the test names a file in its own temp folder.
		if err := os.WriteFile(os.Getenv(PidFileEnv), []byte(strconv.Itoa(grandchild.Process.Pid)+"\n"), 0o600); err != nil {
			return 3
		}
		if mode == SpawnExit {
			return 4
		}
		time.Sleep(time.Minute)
		return 0
	default:
		return 2
	}
}

// Env is the environment that makes a child started from os.Args[0] act as a helper in that mode, writing any pid to
// file.
func Env(mode, file string) []string {
	return append(os.Environ(), HelperEnv+"="+mode, PidFileEnv+"="+file)
}

// WaitFor waits up to Deadline for done to report true, and fails the test with what otherwise.
func WaitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(Deadline)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ReadPid waits for a helper or a shell to write a pid and a newline into file.
func ReadPid(t *testing.T, file string) int {
	t.Helper()
	var pid int
	WaitFor(t, "no pid in "+file, func() bool {
		data, err := os.ReadFile(file) //nolint:gosec // G304: a file in the test's own temp folder.
		if err != nil || !strings.HasSuffix(string(data), "\n") {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	})
	return pid
}

// WaitGone waits for the process with that pid to end.
func WaitGone(t *testing.T, pid int) {
	t.Helper()
	WaitFor(t, "process "+strconv.Itoa(pid)+" is still running", func() bool { return !Running(pid) })
}

// ExitCode waits up to Deadline for cmd to exit and returns its code.
func ExitCode(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(Deadline):
		t.Fatalf("%s did not exit", filepath.Base(cmd.Path))
		return -1
	}
}

// BuildCommand builds the processconcierge command into dir and returns its path, for tests that drive it as an owner
// would.
func BuildCommand(dir string) (string, error) {
	binary := filepath.Join(dir, "processconcierge")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "github.com/behaviorengineering/processconcierge/cmd/processconcierge") //nolint:gosec // G204: builds this module's own command.
	build.Stderr = os.Stderr
	return binary, build.Run()
}
