package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func running(pid int) bool {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: Windows process ids are 32-bit.
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(process) }()
	event, err := windows.WaitForSingleObject(process, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func readPid(t *testing.T, file string) int {
	t.Helper()
	var pid int
	waitFor(t, "no pid in "+file, func() bool {
		data, err := os.ReadFile(file) //nolint:gosec // G304: a file in the test's own temp folder.
		if err != nil || !strings.HasSuffix(string(data), "\n") {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	})
	return pid
}

// startReaper runs processconcierge run on the test binary in a helper mode, with any extra flags.
func startReaper(t *testing.T, mode string, extra ...string) (*exec.Cmd, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "grandchild")
	args := append([]string{"run", "--grace", "300ms"}, extra...)
	args = append(args, "--", os.Args[0])
	cmd := exec.Command(binary, args...) //nolint:gosec // G204: the command these tests built.
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, pidFileEnv+"="+file)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, file
}

func exitCode(t *testing.T, cmd *exec.Cmd) int {
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
	case <-time.After(15 * time.Second):
		t.Fatal("the reaper did not exit")
		return -1
	}
}

func TestTheReaperStopsTheProgramAndWhatItStartedWhenTheOwnersProcessExits(t *testing.T) {
	owner := exec.Command(os.Args[0]) //nolint:gosec // G204: the test binary itself.
	owner.Env = append(os.Environ(), helperEnv+"=sleep")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	cmd, file := startReaper(t, "spawn", "--owner-pid", strconv.Itoa(owner.Process.Pid))
	grandchild := readPid(t, file)

	_ = owner.Process.Kill()
	_ = owner.Wait()
	exitCode(t, cmd)
	waitFor(t, "the grandchild outlived its owner", func() bool { return !running(grandchild) })
}

func TestAJobEndsWithTheReaperEvenWhenTheReaperIsKilledOutright(t *testing.T) {
	cmd, file := startReaper(t, "spawn")
	grandchild := readPid(t, file)

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	waitFor(t, "the grandchild outlived its killed reaper", func() bool { return !running(grandchild) })
}

func TestTheReaperClearsWhatTheProgramLeftAndExitsWithItsCode(t *testing.T) {
	cmd, file := startReaper(t, "spawn-exit")
	grandchild := readPid(t, file)

	if code := exitCode(t, cmd); code != 4 {
		t.Fatalf("exit code %d, want the program's 4", code)
	}
	waitFor(t, "the program's grandchild outlived it", func() bool { return !running(grandchild) })
}
