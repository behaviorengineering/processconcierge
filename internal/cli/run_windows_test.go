package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/behaviorengineering/processconcierge/internal/proctest"
)

func startReaper(t *testing.T, mode string, extra ...string) (*exec.Cmd, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "grandchild")
	args := append([]string{"run", "--grace", "300ms"}, extra...)
	args = append(args, "--", os.Args[0])
	cmd := exec.Command(binary, args...) //nolint:gosec // G204: the command these tests built.
	cmd.Env = proctest.Env(mode, file)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, file
}

func TestTheReaperStopsTheProgramAndWhatItStartedWhenTheOwnersProcessExits(t *testing.T) {
	owner := exec.Command(os.Args[0]) //nolint:gosec // G204: the test binary itself.
	owner.Env = proctest.Env(proctest.Sleep, "")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	cmd, file := startReaper(t, proctest.Spawn, "--owner-pid", strconv.Itoa(owner.Process.Pid))
	grandchild := proctest.ReadPid(t, file)

	_ = owner.Process.Kill()
	_ = owner.Wait()
	proctest.ExitCode(t, cmd)
	proctest.WaitFor(t, "the grandchild outlived its owner", func() bool { return !proctest.Running(grandchild) })
}

func TestAJobEndsWithTheReaperEvenWhenTheReaperIsKilledOutright(t *testing.T) {
	cmd, file := startReaper(t, proctest.Spawn)
	grandchild := proctest.ReadPid(t, file)

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	proctest.WaitFor(t, "the grandchild outlived its killed reaper", func() bool { return !proctest.Running(grandchild) })
}

func TestTheReaperClearsWhatTheProgramLeftAndExitsWithItsCode(t *testing.T) {
	cmd, file := startReaper(t, proctest.SpawnExit)
	grandchild := proctest.ReadPid(t, file)

	if code := proctest.ExitCode(t, cmd); code != 4 {
		t.Fatalf("exit code %d, want the program's 4", code)
	}
	proctest.WaitFor(t, "the program's grandchild outlived it", func() bool { return !proctest.Running(grandchild) })
}

func TestOwnerFDIsRefusedOnWindows(t *testing.T) {
	cmd := exec.Command(binary, "run", "--owner-fd", "3", "--", os.Args[0]) //nolint:gosec // G204: the command these tests built.
	cmd.Env = proctest.Env(proctest.Exit4, "")
	if code := proctest.ExitCode(t, cmd); code != ExitUsage {
		t.Fatalf("run --owner-fd exited %d on Windows, want %d", code, ExitUsage)
	}
}
