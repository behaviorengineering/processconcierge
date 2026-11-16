//go:build unix

package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/behaviorengineering/processconcierge/pkg/proc"
)

func sleeper(t *testing.T) *proc.Process {
	t.Helper()
	p, err := proc.Config{Path: "/bin/sleep", Args: []string{"60"}}.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Kill() })
	return p
}

func TestSweepStopsARecordedChildStillRunningAndRemovesItsEntry(t *testing.T) {
	ctx := context.Background()
	reg := Registry{Dir: filepath.Join(t.TempDir(), "children")}
	p := sleeper(t)
	if err := reg.Record(ctx, p.Pid()); err != nil {
		t.Fatal(err)
	}

	results, err := reg.Sweep(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != Stopped || results[0].Entry.PID != p.Pid() {
		t.Fatalf("results = %+v", results)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the recorded child is still running")
	}
	if entries, _ := reg.Entries(ctx); len(entries) != 0 {
		t.Fatalf("entries left: %+v", entries)
	}
}

func TestADryRunReportsAndChangesNothing(t *testing.T) {
	ctx := context.Background()
	reg := Registry{Dir: t.TempDir()}
	p := sleeper(t)
	if err := reg.Record(ctx, p.Pid()); err != nil {
		t.Fatal(err)
	}

	results, err := reg.Sweep(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != WouldStop {
		t.Fatalf("results = %+v", results)
	}
	if err := syscall.Kill(p.Pid(), 0); err != nil {
		t.Fatalf("the dry run stopped the child: %v", err)
	}
	if entries, _ := reg.Entries(ctx); len(entries) != 1 {
		t.Fatalf("the dry run removed the entry: %+v", entries)
	}
}

func TestSweepLeavesAProcessThatReusedThePidAlone(t *testing.T) {
	ctx := context.Background()
	reg := Registry{Dir: t.TempDir()}
	p := sleeper(t)
	// An entry for the same pid with another start time stands for a child that ended, its pid since given to a
	// stranger.
	write(t, reg, Entry{PID: p.Pid(), StartedMS: 1})

	results, err := reg.Sweep(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != Reused {
		t.Fatalf("results = %+v", results)
	}
	if err := syscall.Kill(p.Pid(), 0); err != nil {
		t.Fatalf("the sweep stopped a process that only shares the pid: %v", err)
	}
}

func TestSweepDropsEntriesForGoneAndUnreadableChildren(t *testing.T) {
	ctx := context.Background()
	reg := Registry{Dir: t.TempDir()}
	p, err := proc.Config{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, reg, Entry{PID: p.Pid(), StartedMS: 1})
	if err := os.WriteFile(filepath.Join(reg.Dir, "99.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	results, err := reg.Sweep(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != Gone {
		t.Fatalf("results = %+v", results)
	}
	if left, _ := os.ReadDir(reg.Dir); len(left) != 0 {
		t.Fatalf("files left: %v", left)
	}
}

func TestEntriesOfAMissingFolderAreNone(t *testing.T) {
	entries, err := Registry{Dir: filepath.Join(t.TempDir(), "never-made")}.Entries(context.Background())
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries = %+v, err = %v", entries, err)
	}
}

func TestRemoveOfAMissingEntryIsNotAnError(t *testing.T) {
	if err := (Registry{Dir: t.TempDir()}).Remove(12345); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRefusesNoFolder(t *testing.T) {
	if err := (Registry{}).Record(context.Background(), os.Getpid()); err == nil {
		t.Fatal("recorded with no folder")
	}
}

func write(t *testing.T, reg Registry, entry Entry) {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reg.path(entry.PID), data, 0o600); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
}
