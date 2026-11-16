package reaper

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/behaviorengineering/processconcierge/internal/proctest"
	"github.com/behaviorengineering/processconcierge/pkg/procerr"
	"github.com/behaviorengineering/processconcierge/pkg/slots"
)

func TestMain(m *testing.M) {
	proctest.RunHelperIfAsked()
	os.Exit(m.Run())
}

// syncBuffer is a log the reaper's goroutines can write to while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func helperRun(mode, file string) Config {
	return Config{
		Path:   os.Args[0],
		Env:    proctest.Env(mode, file),
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: io.Discard,
		Grace:  300 * time.Millisecond,
	}
}

func TestRunExitsWithTheChildsCode(t *testing.T) {
	code, err := helperRun(proctest.Exit4, "").Run(context.Background())
	if err != nil || code != 4 {
		t.Fatalf("code = %d, err = %v; want the child's 4", code, err)
	}
}

func TestAStopRequestDuringTheSlotWaitStartsNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	shared := &slots.Config{Dir: filepath.Join(dir, "slots"), Count: 1, Poll: 10 * time.Millisecond}
	held, err := shared.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })
	file := filepath.Join(dir, "grandchild")
	signals := make(chan os.Signal, 1)
	cfg := helperRun(proctest.Spawn, file)
	cfg.Slots, cfg.SlotWait, cfg.Signals = shared, 10*time.Second, signals

	go func() { time.Sleep(200 * time.Millisecond); signals <- syscall.SIGTERM }()
	began := time.Now()
	code, err := cfg.Run(ctx)
	if err != nil || code != ExitStoppedBeforeStart {
		t.Fatalf("code = %d, err = %v; want %d", code, err, ExitStoppedBeforeStart)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("Run took %v, as if it waited out the slot wait", took)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("the child started though the stop came first")
	}
}

func TestAnOwnerGoneDuringTheSlotWaitStartsNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	shared := &slots.Config{Dir: filepath.Join(dir, "slots"), Count: 1, Poll: 10 * time.Millisecond}
	held, err := shared.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })
	reaperEnd, ownerEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "grandchild")
	cfg := helperRun(proctest.Spawn, file)
	cfg.Slots, cfg.SlotWait, cfg.Owner = shared, 10*time.Second, reaperEnd

	go func() { time.Sleep(200 * time.Millisecond); _ = ownerEnd.Close() }()
	code, err := cfg.Run(ctx)
	if err != nil || code != ExitStoppedBeforeStart {
		t.Fatalf("code = %d, err = %v; want %d", code, err, ExitStoppedBeforeStart)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("the child started though its owner had gone")
	}
}

// brokenPipe fails every read, as a descriptor that names no pipe does.
type brokenPipe struct{}

func (brokenPipe) Read([]byte) (int, error) { return 0, errors.New("bad file descriptor") }

func TestAnOwnerPipeThatCannotBeReadStopsTheChildAndSaysWhy(t *testing.T) {
	log := &syncBuffer{}
	file := filepath.Join(t.TempDir(), "grandchild")
	cfg := helperRun(proctest.Spawn, file)
	cfg.Owner = brokenPipe{}
	cfg.Logger = slog.New(slog.NewTextHandler(log, nil))

	if _, err := cfg.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "the owner's pipe could not be read: bad file descriptor") {
		t.Fatalf("the log does not say why the child was stopped:\n%s", log.String())
	}
}

func TestOnlyAStuckChildKeepsItsEntry(t *testing.T) {
	if code, keep, err := stopOutcome(nil); code != 0 || keep || err != nil {
		t.Fatalf("a clean stop = %d, %v, %v", code, keep, err)
	}
	stuck := procerr.New(procerr.CodeStuck, "proc.Stop", "stuck")
	if code, keep, _ := stopOutcome(stuck); code != ExitStuck || !keep {
		t.Fatalf("a stuck child = %d, keep %v; want %d and the entry kept", code, keep, ExitStuck)
	}
	other := procerr.New(procerr.CodeSignal, "proc.Stop", "could not signal")
	if code, keep, _ := stopOutcome(other); code != ExitInternal || keep {
		t.Fatalf("another failure = %d, keep %v; want %d and the entry removed", code, keep, ExitInternal)
	}
}
