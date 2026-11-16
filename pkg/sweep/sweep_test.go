package sweep

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// sleeperEnv makes the test binary sleep a minute instead of running the tests, so every platform has a plain child
// to record and sweep.
const sleeperEnv = "PROCESSCONCIERGE_TEST_SLEEPER"

func TestMain(m *testing.M) {
	if os.Getenv(sleeperEnv) != "" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestSweepStopsAChildALostSupervisorLeftRunning plays a supervisor that started a child the plain way, recorded it,
// and died: the next run's sweep must stop it, on every platform.
func TestSweepStopsAChildALostSupervisorLeftRunning(t *testing.T) {
	ctx := context.Background()
	reg := Registry{Dir: t.TempDir()}
	child := exec.Command(os.Args[0]) //nolint:gosec // G204: the test binary itself.
	child.Env = append(os.Environ(), sleeperEnv+"=1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = child.Wait(); close(exited) }()
	t.Cleanup(func() { _ = child.Process.Kill(); <-exited })
	if err := reg.Record(ctx, child.Process.Pid); err != nil {
		t.Fatal(err)
	}

	results, err := reg.Sweep(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != Stopped {
		t.Fatalf("results = %+v", results)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the swept child is still running")
	}
}
