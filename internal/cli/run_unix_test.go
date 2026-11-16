//go:build unix

package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/behaviorengineering/processconcierge/internal/proctest"
)

func startReaper(t *testing.T, script string, extra ...string) (*exec.Cmd, *os.File) {
	t.Helper()
	reaperEnd, ownerEnd, err := os.Pipe() // the reaper reads; the owner holds the write end
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"run", "--owner-fd", "3", "--grace", "300ms"}, extra...)
	args = append(args, "--", "/bin/sh", "-c", script)
	cmd := exec.Command(binary, args...) //nolint:gosec // G204: the command these tests built.
	cmd.ExtraFiles = []*os.File{reaperEnd}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = reaperEnd.Close()
	t.Cleanup(func() {
		_ = ownerEnd.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, ownerEnd
}

func TestTheReaperStopsTheProgramAndWhatItStartedWhenTheOwnerGoesAway(t *testing.T) {
	file := filepath.Join(t.TempDir(), "grandchild")
	cmd, owner := startReaper(t, "sleep 60 & echo $! > "+file+"; wait")
	grandchild := proctest.ReadPid(t, file)

	_ = owner.Close()
	if code := proctest.ExitCode(t, cmd); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code %d", code)
	}
	proctest.WaitFor(t, "the grandchild outlived its owner", func() bool { return !proctest.Running(grandchild) })
}

func TestTheReaperStopsTheProgramWhenItGetsSIGTERM(t *testing.T) {
	file := filepath.Join(t.TempDir(), "grandchild")
	cmd, _ := startReaper(t, "sleep 60 & echo $! > "+file+"; wait")
	grandchild := proctest.ReadPid(t, file)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := proctest.ExitCode(t, cmd); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code %d", code)
	}
	proctest.WaitFor(t, "the grandchild outlived the reaper", func() bool { return !proctest.Running(grandchild) })
}

func TestTheReaperClearsWhatTheProgramLeftRunningAndExitsWithItsCode(t *testing.T) {
	file := filepath.Join(t.TempDir(), "leftover")
	cmd, _ := startReaper(t, "trap '' HUP; sleep 60 > /dev/null 2>&1 & echo $! > "+file+"; exit 4")
	leftover := proctest.ReadPid(t, file)

	if code := proctest.ExitCode(t, cmd); code != 4 {
		t.Fatalf("exit code %d, want the program's 4", code)
	}
	proctest.WaitFor(t, "the program's background job outlived it", func() bool { return !proctest.Running(leftover) })
}

func TestTheReaperRecordsTheProgramWhileItRunsAndRemovesItAfter(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "registry")
	ready := filepath.Join(dir, "ready")
	cmd, owner := startReaper(t, "echo $$ > "+ready+"; sleep 60", "--registry", registry)
	pid := proctest.ReadPid(t, ready)

	proctest.WaitFor(t, "no registry entry for the program", func() bool {
		_, err := os.Stat(filepath.Join(registry, strconv.Itoa(pid)+".json"))
		return err == nil
	})
	_ = owner.Close()
	proctest.ExitCode(t, cmd)
	if entries, _ := os.ReadDir(registry); len(entries) != 0 {
		t.Fatalf("entries left after the program ended: %v", entries)
	}
}

func TestSweepStopsWhatACrashedReaperLeftAndReportsIt(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "registry")
	file := filepath.Join(dir, "grandchild")
	cmd, _ := startReaper(t, "sleep 60 & echo $! > "+file+"; wait", "--registry", registry)
	grandchild := proctest.ReadPid(t, file)
	proctest.WaitFor(t, "no registry entry for the program", func() bool {
		entries, _ := os.ReadDir(registry)
		return len(entries) == 1
	})
	// A reaper killed outright leaves its entry behind, as a force-quit would. On macOS its program keeps running;
	// on Linux the parent-death signal ends the program, but not the grandchild, which stays in the program's group.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !proctest.Running(grandchild) {
		t.Fatal("the grandchild died with the reaper, so this test proves nothing")
	}

	var out bytes.Buffer
	code := Execute(context.Background(), []string{"sweep", "--registry", registry}, &out, &out, "test")
	if code != ExitOK {
		t.Fatalf("sweep exited %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "Outcome:  stopped") {
		t.Fatalf("sweep printed:\n%s", out.String())
	}
	proctest.WaitFor(t, "the left-over grandchild is still running", func() bool { return !proctest.Running(grandchild) })
}

func TestTheReaperStopsTheProgramWhenTheOwnersProcessExits(t *testing.T) {
	owner := exec.Command("/bin/sleep", "60")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	file := filepath.Join(t.TempDir(), "grandchild")
	cmd := exec.Command(binary, "run", "--owner-pid", strconv.Itoa(owner.Process.Pid), "--grace", "300ms", "--", //nolint:gosec // G204: the command these tests built.
		"/bin/sh", "-c", "sleep 60 & echo $! > "+file+"; wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	grandchild := proctest.ReadPid(t, file)

	_ = owner.Process.Kill()
	_ = owner.Wait()
	if code := proctest.ExitCode(t, cmd); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code %d", code)
	}
	proctest.WaitFor(t, "the grandchild outlived its owner", func() bool { return !proctest.Running(grandchild) })
}

func TestASecondReaperWaitsForTheOneSlotAndGivesUpWith75(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	ready := filepath.Join(dir, "ready")
	first, owner := startReaper(t, "echo $$ > "+ready+"; sleep 60", "--slots", slotsDir, "--slot-count", "1")
	proctest.ReadPid(t, ready)

	second := exec.Command(binary, "run", "--slots", slotsDir, "--slot-count", "1", "--slot-wait", "300ms", "--", "/bin/sh", "-c", "exit 0") //nolint:gosec // G204: the command these tests built.
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	if code := proctest.ExitCode(t, second); code != 75 {
		t.Fatalf("the second reaper exited %d, want 75 for no free slot", code)
	}

	_ = owner.Close()
	proctest.ExitCode(t, first)
	third := exec.Command(binary, "run", "--slots", slotsDir, "--slot-count", "1", "--slot-wait", "5s", "--", "/bin/sh", "-c", "exit 0") //nolint:gosec // G204: the command these tests built.
	if err := third.Start(); err != nil {
		t.Fatal(err)
	}
	if code := proctest.ExitCode(t, third); code != 0 {
		t.Fatalf("a reaper after the first ended exited %d, so the slot never came back", code)
	}
}
