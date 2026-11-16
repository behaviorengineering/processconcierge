//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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

// startReaper starts processconcierge run with a script, and hands it the read end of an owner pipe as fd 3. It returns
// the command and the owner's end, which the test closes to play an owner that died.
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
	case <-time.After(10 * time.Second):
		t.Fatal("the reaper did not exit")
		return -1
	}
}

func TestTheReaperStopsTheProgramAndWhatItStartedWhenTheOwnerGoesAway(t *testing.T) {
	file := filepath.Join(t.TempDir(), "grandchild")
	cmd, owner := startReaper(t, "sleep 60 & echo $! > "+file+"; wait")
	grandchild := readPid(t, file)

	_ = owner.Close()
	if code := exitCode(t, cmd); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code %d", code)
	}
	waitFor(t, "the grandchild outlived its owner", func() bool { return !alive(grandchild) })
}

func TestTheReaperStopsTheProgramWhenItGetsSIGTERM(t *testing.T) {
	file := filepath.Join(t.TempDir(), "grandchild")
	cmd, _ := startReaper(t, "sleep 60 & echo $! > "+file+"; wait")
	grandchild := readPid(t, file)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(t, cmd); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code %d", code)
	}
	waitFor(t, "the grandchild outlived the reaper", func() bool { return !alive(grandchild) })
}

func TestTheReaperClearsWhatTheProgramLeftRunningAndExitsWithItsCode(t *testing.T) {
	file := filepath.Join(t.TempDir(), "leftover")
	cmd, _ := startReaper(t, "trap '' HUP; sleep 60 > /dev/null 2>&1 & echo $! > "+file+"; exit 4")
	leftover := readPid(t, file)

	if code := exitCode(t, cmd); code != 4 {
		t.Fatalf("exit code %d, want the program's 4", code)
	}
	waitFor(t, "the program's background job outlived it", func() bool { return !alive(leftover) })
}

func TestTheReaperRecordsTheProgramWhileItRunsAndRemovesItAfter(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "registry")
	ready := filepath.Join(dir, "ready")
	cmd, owner := startReaper(t, "echo $$ > "+ready+"; sleep 60", "--registry", registry)
	pid := readPid(t, ready)

	waitFor(t, "no registry entry for the program", func() bool {
		_, err := os.Stat(filepath.Join(registry, strconv.Itoa(pid)+".json"))
		return err == nil
	})
	_ = owner.Close()
	exitCode(t, cmd)
	if entries, _ := os.ReadDir(registry); len(entries) != 0 {
		t.Fatalf("entries left after the program ended: %v", entries)
	}
}

func TestSweepStopsWhatACrashedReaperLeftAndReportsIt(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "registry")
	file := filepath.Join(dir, "grandchild")
	cmd, _ := startReaper(t, "sleep 60 & echo $! > "+file+"; wait", "--registry", registry)
	grandchild := readPid(t, file)
	waitFor(t, "no registry entry for the program", func() bool {
		entries, _ := os.ReadDir(registry)
		return len(entries) == 1
	})
	// A reaper killed outright leaves its entry behind, as a force-quit would. On macOS its program keeps running;
	// on Linux the parent-death signal ends the program, but not the grandchild, which stays in the program's group.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !alive(grandchild) {
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
	waitFor(t, "the left-over grandchild is still running", func() bool { return !alive(grandchild) })
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
	grandchild := readPid(t, file)

	_ = owner.Process.Kill()
	_ = owner.Wait()
	if code := exitCode(t, cmd); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code %d", code)
	}
	waitFor(t, "the grandchild outlived its owner", func() bool { return !alive(grandchild) })
}
