package proc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/behaviorengineering/processconcierge/internal/proctest"
)

func helper(t *testing.T, mode string) (*Process, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "grandchild")
	p, err := Config{
		Path:  os.Args[0],
		Env:   proctest.Env(mode, file),
		Grace: 500 * time.Millisecond,
	}.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Kill() })
	return p, file
}

func TestStopEndsTheChildAndWhatItStartedThroughItsJob(t *testing.T) {
	p, file := helper(t, proctest.Spawn)
	grandchild := proctest.ReadPid(t, file)

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	proctest.WaitGone(t, grandchild)
	if _, err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStopClearsWhatAChildLeftInItsJobAfterItExited(t *testing.T) {
	p, file := helper(t, proctest.SpawnExit)
	grandchild := proctest.ReadPid(t, file)
	exit, err := p.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exit.Code != 4 {
		t.Fatalf("exit = %+v, want code 4", exit)
	}
	if !proctest.Running(grandchild) {
		t.Fatal("the grandchild ended with the child, so this test proves nothing")
	}

	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	proctest.WaitGone(t, grandchild)
}

func TestKillEndsTheWholeJob(t *testing.T) {
	p, file := helper(t, proctest.Spawn)
	grandchild := proctest.ReadPid(t, file)

	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	proctest.WaitGone(t, grandchild)
}
