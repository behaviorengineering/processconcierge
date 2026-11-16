package slots

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/behaviorengineering/processconcierge/pkg/pressure"
	"github.com/behaviorengineering/processconcierge/pkg/procerr"
)

// holderEnv makes the test binary hold one slot in the folder it names, print "held", and wait to be killed, so a
// test can see a slot held by another process come free when that process dies.
const holderEnv = "PROCESSCONCIERGE_TEST_SLOT_HOLDER"

func TestMain(m *testing.M) {
	if dir := os.Getenv(holderEnv); dir != "" {
		if _, err := (Config{Dir: dir, Count: 1}).Take(context.Background()); err != nil {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("held\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestTakeHandsOutEachSlotOnceAndReleaseGivesItBack(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Dir: t.TempDir(), Count: 2, Poll: 10 * time.Millisecond}
	first, err := cfg.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cfg.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Windows will not delete a file still held open, so every slot is given back before the temp folder goes.
	t.Cleanup(func() { _ = second.Release() })
	if first.Index == second.Index {
		t.Fatalf("both took slot %d", first.Index)
	}
	if free, _ := cfg.Free(ctx); free != 0 {
		t.Fatalf("free = %d with both slots held", free)
	}

	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := cfg.Take(short); !procerr.Is(err, procerr.CodeSlot) {
		t.Fatalf("a third Take returned %v, want a slot error", err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	third, err := cfg.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Release() })
	if third.Index != first.Index {
		t.Fatalf("took slot %d, want the released %d", third.Index, first.Index)
	}
}

func TestASlotHeldByAProcessComesFreeWhenThatProcessIsKilled(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	holder := exec.Command(os.Args[0]) //nolint:gosec // G204: the test binary itself.
	holder.Env = append(os.Environ(), holderEnv+"="+dir)
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	buf := make([]byte, 5)
	if _, err := out.Read(buf); err != nil || string(buf) != "held\n" {
		t.Fatalf("the holder said %q, %v", buf, err)
	}
	cfg := Config{Dir: dir, Count: 1, Poll: 10 * time.Millisecond}
	if free, _ := cfg.Free(ctx); free != 0 {
		t.Fatal("the slot the other process holds counts as free")
	}

	_ = holder.Process.Kill()
	_ = holder.Wait()
	waiting, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	slot, err := cfg.Take(waiting)
	if err != nil {
		t.Fatalf("the killed holder's slot never came free: %v", err)
	}
	_ = slot.Release()
}

func TestSlotsAreHeldBackWhileMemoryIsShort(t *testing.T) {
	ctx := context.Background()
	level := pressure.Critical
	cfg := Config{
		Dir: t.TempDir(), Count: 1, Poll: 10 * time.Millisecond,
		Pressure: func(context.Context) (pressure.Reading, error) { return pressure.Reading{Level: level}, nil },
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := cfg.Take(short); err == nil {
		t.Fatal("took a slot while memory was critical")
	}

	level = pressure.Warning
	slot, err := cfg.Take(ctx)
	if err != nil {
		t.Fatalf("a warning held the slot back, though HoldAt defaults to critical: %v", err)
	}
	_ = slot.Release()

	cfg.HoldAt = pressure.Warning
	short2, cancel2 := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel2()
	if _, err := cfg.Take(short2); err == nil {
		t.Fatal("took a slot at a warning with HoldAt set to warning")
	}
}

func TestAnUnreadablePressureSignalHoldsNothingBack(t *testing.T) {
	cfg := Config{
		Dir: t.TempDir(), Count: 1,
		Pressure: func(context.Context) (pressure.Reading, error) { return pressure.Reading{}, errors.New("no signal") },
	}
	slot, err := cfg.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = slot.Release()
}

func TestFreeCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-made")
	free, err := (Config{Dir: dir, Count: 3}).Free(context.Background())
	if err != nil || free != 3 {
		t.Fatalf("free = %d, err = %v; want all 3 free", free, err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("Free made the slots folder")
	}
}

func TestTakeRefusesABadConfig(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []Config{{Count: 1}, {Dir: t.TempDir()}, {Dir: t.TempDir(), Count: -1}} {
		if _, err := cfg.Take(ctx); !procerr.Is(err, procerr.CodeInvalid) {
			t.Errorf("Take(%+v) = %v, want an invalid error", cfg, err)
		}
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "slot-"+strconv.Itoa(0)+".lock")); err == nil {
		t.Error("a bad config made a slot file")
	}
}
