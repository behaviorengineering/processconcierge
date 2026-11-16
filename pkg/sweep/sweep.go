// Package sweep keeps a folder of the child processes a program supervises, one file for each, with the time each
// started, so a later run can stop the ones a crash or a force-quit left running. The start time guards against a
// pid the system has since given to another program: an entry is only acted on when the process with that pid
// started when the entry says. A small window remains between that check and the kill; Linux pidfds and Windows
// process handles close it, and macOS has nothing better than the check.
package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/process"

	"github.com/behaviorengineering/processconcierge/pkg/proc"
	"github.com/behaviorengineering/processconcierge/pkg/procerr"
)

// Entry is one supervised child.
type Entry struct {
	// PID is the child's process id, which also leads its group.
	PID int `json:"pid"`
	// StartedMS is when the child started, in milliseconds since the Unix epoch, as the system reports it.
	StartedMS int64 `json:"started_ms"`
}

// Outcome is what a sweep did with one entry.
type Outcome string

const (
	// Stopped means the child was still running, and its group was killed.
	Stopped Outcome = "stopped"
	// WouldStop means a dry run found the child still running.
	WouldStop Outcome = "would-stop"
	// Gone means no process has that pid any more.
	Gone Outcome = "gone"
	// Reused means another program now has that pid, so it was left alone.
	Reused Outcome = "reused"
)

// Result is one entry and what the sweep did with it.
type Result struct {
	Entry   Entry   `json:"entry"`
	Outcome Outcome `json:"outcome"`
}

// Registry is the folder of entries. Each entry is its own file, named by pid, so several supervisors can add and
// remove entries at once without a lock.
type Registry struct {
	Dir string
}

// Record adds an entry for a running child, reading its start time from the system.
func (r Registry) Record(ctx context.Context, pid int) error {
	const op = "sweep.Record"
	if err := r.check(ctx, op); err != nil {
		return err
	}
	started, err := startedMS(ctx, pid)
	if err != nil {
		return procerr.Wrap(err, procerr.CodeRegistry, op, "read the child's start time").With("pid", pid)
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return procerr.Wrap(err, procerr.CodeRegistry, op, "make the folder").With("dir", r.Dir)
	}
	data, err := json.Marshal(Entry{PID: pid, StartedMS: started})
	if err != nil {
		return procerr.Wrap(err, procerr.CodeRegistry, op, "encode the entry").With("pid", pid)
	}
	// Written to a temporary file and renamed, so a sweep never reads half an entry.
	tmp, err := os.CreateTemp(r.Dir, ".entry-*")
	if err != nil {
		return procerr.Wrap(err, procerr.CodeRegistry, op, "create the entry").With("pid", pid)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return procerr.Wrap(err, procerr.CodeRegistry, op, "write the entry").With("pid", pid)
	}
	if err := tmp.Close(); err != nil {
		return procerr.Wrap(err, procerr.CodeRegistry, op, "close the entry").With("pid", pid)
	}
	if err := os.Rename(tmp.Name(), r.path(pid)); err != nil {
		return procerr.Wrap(err, procerr.CodeRegistry, op, "save the entry").With("pid", pid)
	}
	return nil
}

// Remove drops a child's entry once it has ended. An entry already gone is not an error.
func (r Registry) Remove(pid int) error {
	if err := os.Remove(r.path(pid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return procerr.Wrap(err, procerr.CodeRegistry, "sweep.Remove", "remove the entry").With("pid", pid)
	}
	return nil
}

// Entries lists the folder's entries, oldest pid first. A missing folder holds none.
func (r Registry) Entries(ctx context.Context) ([]Entry, error) {
	const op = "sweep.Entries"
	if err := r.check(ctx, op); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(r.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, procerr.Wrap(err, procerr.CodeRegistry, op, "read the folder").With("dir", r.Dir)
	}
	var entries []Entry
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.Dir, name)) //nolint:gosec // G304: an entry in the registry folder the caller named.
		if err != nil {
			return nil, procerr.Wrap(err, procerr.CodeRegistry, op, "read an entry").With("file", name)
		}
		var entry Entry
		if err := json.Unmarshal(data, &entry); err != nil || entry.PID <= 1 {
			// An entry that cannot be read names no process anyone can act on, so it is dropped.
			_ = os.Remove(filepath.Join(r.Dir, name))
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].PID < entries[j].PID })
	return entries, nil
}

// Sweep stops the group of every recorded child that is still running and removes its entry. With dryRun it only
// reports what it would do, and changes nothing.
func (r Registry) Sweep(ctx context.Context, dryRun bool) ([]Result, error) {
	const op = "sweep.Sweep"
	entries, err := r.Entries(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(entries))
	for _, entry := range entries {
		outcome := r.judge(ctx, entry)
		if outcome == Stopped && dryRun {
			outcome = WouldStop
		}
		if outcome == Stopped {
			if err := proc.KillGroup(entry.PID); err != nil {
				return results, procerr.Wrap(err, procerr.CodeSignal, op, "kill a left-over group").With("pid", entry.PID)
			}
		}
		if !dryRun {
			if err := r.Remove(entry.PID); err != nil {
				return results, err
			}
		}
		results = append(results, Result{Entry: entry, Outcome: outcome})
	}
	return results, nil
}

// judge says whether an entry's child is still running, gone, or replaced by another program with the same pid.
func (r Registry) judge(ctx context.Context, entry Entry) Outcome {
	started, err := startedMS(ctx, entry.PID)
	switch {
	case err != nil:
		return Gone
	case started != entry.StartedMS:
		return Reused
	default:
		return Stopped
	}
}

func (r Registry) check(ctx context.Context, op string) error {
	if ctx == nil {
		return procerr.New(procerr.CodeInvalid, op, "context is required")
	}
	if strings.TrimSpace(r.Dir) == "" {
		return procerr.New(procerr.CodeInvalid, op, "no registry folder")
	}
	return nil
}

func (r Registry) path(pid int) string {
	return filepath.Join(r.Dir, strconv.Itoa(pid)+".json")
}

func startedMS(ctx context.Context, pid int) (int64, error) {
	if pid <= 0 || pid > 1<<31-1 {
		return 0, errors.New("pid out of range")
	}
	p, err := process.NewProcessWithContext(ctx, int32(pid)) //nolint:gosec // G115: range checked above.
	if err != nil {
		return 0, err
	}
	return p.CreateTimeWithContext(ctx)
}
