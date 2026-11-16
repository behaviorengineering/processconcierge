//go:build unix

package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// alive reports whether a process with that pid still exists and is not a zombie waiting to be collected.
func alive(t *testing.T, pid int) bool {
	t.Helper()
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// waitGone waits up to five seconds for a process to disappear. A grandchild the test's own shell started is
// collected by launchd or init once killed, which takes a moment.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readPid waits for a shell to write a pid into file.
func readPid(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(file) //nolint:gosec // G304: a file in the test's own temp folder.
		if err == nil && strings.HasSuffix(string(data), "\n") {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr != nil {
				t.Fatalf("pid file holds %q", data)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", file)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func start(t *testing.T, script string, grace time.Duration) *Process {
	t.Helper()
	p, err := Config{Path: "/bin/sh", Args: []string{"-c", script}, Grace: grace}.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Kill() })
	return p
}

func TestStopEndsTheChildAndWhatItStarted(t *testing.T) {
	file := filepath.Join(t.TempDir(), "grandchild")
	p := start(t, "sleep 60 & echo $! > "+file+"; wait", time.Second)
	grandchild := readPid(t, file)

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitGone(t, grandchild)
	exit, err := p.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exit.Signal != "terminated" || exit.ShellCode() != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit = %+v, shell code %d", exit, exit.ShellCode())
	}
}

func TestStopLetsAChildThatHonoursTheGentleSignalExitByItself(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	p := start(t, "trap 'exit 7' TERM; echo $$ > "+ready+"; while true; do sleep 0.05; done", 10*time.Second)
	readPid(t, ready)

	began := time.Now()
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("Stop waited %v for a child that exits on SIGTERM", took)
	}
	exit, err := p.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exit.Code != 7 || exit.Signal != "" {
		t.Fatalf("exit = %+v, want code 7", exit)
	}
}

func TestStopKillsAChildThatIgnoresTheGentleSignalOnceTheGracePeriodEnds(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	p := start(t, "trap '' TERM; echo $$ > "+ready+"; while true; do sleep 0.05; done", 200*time.Millisecond)
	readPid(t, ready)

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	exit, err := p.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exit.Signal != "killed" {
		t.Fatalf("exit = %+v, want killed", exit)
	}
}

func TestStopClearsWhatAChildLeftRunningAfterItExited(t *testing.T) {
	file := filepath.Join(t.TempDir(), "leftover")
	// The background job ignores SIGHUP, as a detached MCP server or shell would, so only the group kill ends it.
	p := start(t, "trap '' HUP; sleep 60 > /dev/null 2>&1 & echo $! > "+file, 200*time.Millisecond)
	leftover := readPid(t, file)
	if _, err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !alive(t, leftover) {
		t.Fatal("the background job ended with the child, so this test proves nothing")
	}

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitGone(t, leftover)
}

func TestStopGivesWhatAChildLeftRunningTheGracePeriodToo(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "leftover")
	done := filepath.Join(dir, "cleaned-up")
	// The background job cleans up on SIGTERM, as an MCP server might; the grace period lets it.
	p := start(t, "(trap 'touch "+done+"; exit 0' TERM; while true; do sleep 0.05; done) > /dev/null 2>&1 & echo $! > "+file, 5*time.Second)
	leftover := readPid(t, file)
	if _, err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitGone(t, leftover)
	if _, err := os.Stat(done); err != nil {
		t.Fatal("the left-over job was killed before it could clean up")
	}
}

func TestStartRefusesNoProgramAndANilContext(t *testing.T) {
	if _, err := (Config{}).Start(context.Background()); err == nil {
		t.Fatal("started with no program")
	}
	//nolint:staticcheck // SA1012: a nil context is what this checks.
	if _, err := (Config{Path: "/bin/true"}).Start(nil); err == nil {
		t.Fatal("started with a nil context")
	}
}

func TestKillGroupRefusesGroupsZeroAndOne(t *testing.T) {
	for _, pgid := range []int{0, 1} {
		if err := KillGroup(pgid); err == nil {
			t.Fatalf("KillGroup(%d) did not refuse", pgid)
		}
	}
}
