package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// helperEnv tells the test binary to act as a helper process instead of running the tests: "sleep" sleeps a minute,
// "spawn" starts a sleeping grandchild, writes its pid to the file in pidFileEnv, then sleeps, and "spawn-exit" does
// the same but exits at once with code 4, leaving the grandchild behind.
const (
	helperEnv  = "PROCESSCONCIERGE_TEST_HELPER"
	pidFileEnv = "PROCESSCONCIERGE_TEST_PIDFILE"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "spawn", "spawn-exit":
		grandchild := exec.Command(os.Args[0]) //nolint:gosec // G204: the test binary itself.
		grandchild.Env = append(os.Environ(), helperEnv+"=sleep")
		if err := grandchild.Start(); err != nil {
			os.Exit(3)
		}
		//nolint:gosec // G703: the test itself names a file in its own temp folder.
		if err := os.WriteFile(os.Getenv(pidFileEnv), []byte(strconv.Itoa(grandchild.Process.Pid)+"\n"), 0o600); err != nil {
			os.Exit(3)
		}
		if os.Getenv(helperEnv) == "spawn-exit" {
			os.Exit(4)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func helper(t *testing.T, mode string) (*Process, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "grandchild")
	p, err := Config{
		Path:  os.Args[0],
		Env:   append(os.Environ(), helperEnv+"="+mode, pidFileEnv+"="+file),
		Grace: 500 * time.Millisecond,
	}.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Kill() })
	return p, file
}

func readPid(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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
		time.Sleep(25 * time.Millisecond)
	}
}

func running(pid int) bool {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: Windows process ids are 32-bit.
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(process) }()
	event, err := windows.WaitForSingleObject(process, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for running(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestStopEndsTheChildAndWhatItStartedThroughItsJob(t *testing.T) {
	p, file := helper(t, "spawn")
	grandchild := readPid(t, file)

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitGone(t, grandchild)
	if _, err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStopClearsWhatAChildLeftInItsJobAfterItExited(t *testing.T) {
	p, file := helper(t, "spawn-exit")
	grandchild := readPid(t, file)
	exit, err := p.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exit.Code != 4 {
		t.Fatalf("exit = %+v, want code 4", exit)
	}
	if !running(grandchild) {
		t.Fatal("the grandchild ended with the child, so this test proves nothing")
	}

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitGone(t, grandchild)
}

func TestKillEndsTheWholeJob(t *testing.T) {
	p, file := helper(t, "spawn")
	grandchild := readPid(t, file)

	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	waitGone(t, grandchild)
}
