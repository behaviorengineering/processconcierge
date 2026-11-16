//go:build unix

package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/behaviorengineering/processconcierge/internal/proctest"
	"github.com/behaviorengineering/processconcierge/pkg/procerr"
)

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
	grandchild := proctest.ReadPid(t, file)

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	proctest.WaitGone(t, grandchild)
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
	proctest.ReadPid(t, ready)

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
	proctest.ReadPid(t, ready)

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
	leftover := proctest.ReadPid(t, file)
	if _, err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !proctest.Running(leftover) {
		t.Fatal("the background job ended with the child, so this test proves nothing")
	}

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	proctest.WaitGone(t, leftover)
}

func TestStopGivesWhatAChildLeftRunningTheGracePeriodToo(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "leftover")
	done := filepath.Join(dir, "cleaned-up")
	// The background job cleans up on SIGTERM, as an MCP server might; the grace period lets it.
	p := start(t, "(trap 'touch "+done+"; exit 0' TERM; while true; do sleep 0.05; done) > /dev/null 2>&1 & echo $! > "+file, 5*time.Second)
	leftover := proctest.ReadPid(t, file)
	if _, err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	proctest.WaitGone(t, leftover)
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

// failingWriter refuses every write, as a closed pipe would.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the reader went away") }

func TestWaitReportsAFailedCopyOfTheChildsOutput(t *testing.T) {
	p, err := Config{Path: "/bin/sh", Args: []string{"-c", "echo hello"}, Stdout: failingWriter{}}.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(context.Background()); !procerr.Is(err, procerr.CodeWait) {
		t.Fatalf("Wait returned %v, want the copy's failure", err)
	}
}

func TestStopUnderTheReaperEndsTheChildAndWhatItStarted(t *testing.T) {
	file := filepath.Join(t.TempDir(), "grandchild")
	p, err := Config{Path: os.Args[0], Env: proctest.Env(proctest.Spawn, file), Grace: 300 * time.Millisecond, Reaper: reaperBinary}.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Kill() })
	grandchild := proctest.ReadPid(t, file)

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	proctest.WaitGone(t, grandchild)
	exit, err := p.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exit.ShellCode() != 128+int(syscall.SIGTERM) {
		t.Fatalf("the reaper exited %+v, want the child's 143", exit)
	}
}

func TestKillUnderTheReaperEndsTheChildAndWhatItStarted(t *testing.T) {
	file := filepath.Join(t.TempDir(), "grandchild")
	// Both ignore SIGTERM, so only the reaper's hard stop, not its gentle one, can end them within the test.
	script := "trap '' TERM; sleep 60 & echo $! > " + file + "; wait"
	p, err := Config{Path: "/bin/sh", Args: []string{"-c", script}, Grace: 10 * time.Second, Reaper: reaperBinary}.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	grandchild := proctest.ReadPid(t, file)

	began := time.Now()
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	proctest.WaitGone(t, grandchild)
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("Kill took %v, as if it waited out the 10-second grace period", took)
	}
}

func TestAGoParentKilledOutrightLeavesNothingRunningUnderTheReaper(t *testing.T) {
	file := filepath.Join(t.TempDir(), "grandchild")
	parent := exec.Command(os.Args[0]) //nolint:gosec // G204: the test binary itself.
	parent.Env = append(os.Environ(), goParentEnv+"="+file, reaperEnv+"="+reaperBinary)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Process.Kill(); _ = parent.Wait() })
	grandchild := proctest.ReadPid(t, file)

	_ = parent.Process.Kill()
	_ = parent.Wait()
	proctest.WaitGone(t, grandchild)
}
