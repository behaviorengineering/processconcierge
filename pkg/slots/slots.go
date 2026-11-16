// Package slots limits how many children start at once across every program on the machine that shares a folder.
// Each slot is a file in that folder, held with an operating-system file lock (flock on macOS and Linux, LockFileEx on
// Windows). The system releases a lock when its holder ends, however it ends, so a crash never leaks a slot, and no
// cleanup is needed. Slots can also be held back while the machine is short of memory.
package slots

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/behaviorengineering/processconcierge/pkg/pressure"
	"github.com/behaviorengineering/processconcierge/pkg/procerr"
)

// DefaultPoll is how often Take looks again while every slot is held.
const DefaultPoll = 250 * time.Millisecond

var tracer = otel.Tracer("github.com/behaviorengineering/processconcierge/pkg/slots")

// Config is a set of slots shared through a folder. Every program using the folder must agree on Count; slot files
// past a program's Count are never taken by it.
type Config struct {
	// Dir is the shared folder.
	Dir string
	// Count is how many slots there are.
	Count int
	// Poll is how often Take looks again while every slot is held. Zero uses DefaultPoll.
	Poll time.Duration
	// Pressure, when set, is read before each try, and slots are held back while it reports HoldAt or worse.
	Pressure func(context.Context) (pressure.Reading, error)
	// HoldAt is the memory pressure level that holds slots back. Zero (Normal) means Critical.
	HoldAt pressure.Level
}

// Slot is a held slot. Release gives it back; the system also gives it back when the holder ends.
type Slot struct {
	// Index is which slot this is, from 0 to Count-1.
	Index int
	lock  *flock.Flock
}

// Release gives the slot back.
func (s *Slot) Release() error {
	if s == nil || s.lock == nil {
		return nil
	}
	if err := s.lock.Unlock(); err != nil {
		return procerr.Wrap(err, procerr.CodeSlot, "slots.Release", "release the slot").With("slot", s.Index)
	}
	return nil
}

// Take holds a free slot, waiting while every slot is held or memory is short, until ctx ends. A ctx that has
// already ended, or ends while waiting, returns an error with CodeSlot.
func (c Config) Take(ctx context.Context) (*Slot, error) {
	const op = "slots.Take"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	ctx, span := tracer.Start(ctx, "processconcierge.slots.take")
	defer span.End()
	span.SetAttributes(attribute.String("slots.dir", c.Dir), attribute.Int("slots.count", c.Count))
	began := time.Now()
	ticker := time.NewTicker(c.poll())
	defer ticker.Stop()
	for {
		held, err := c.heldBack(ctx)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		if !held {
			slot, err := c.tryAll()
			if err != nil {
				span.SetStatus(codes.Error, err.Error())
				return nil, err
			}
			if slot != nil {
				span.SetAttributes(attribute.Int("slots.index", slot.Index), attribute.Int64("slots.waited_ms", time.Since(began).Milliseconds()))
				return slot, nil
			}
		}
		select {
		case <-ctx.Done():
			err := procerr.Wrap(ctx.Err(), procerr.CodeSlot, op, "no free slot before giving up").With("dir", c.Dir).With("count", c.Count)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		case <-ticker.C:
		}
	}
}

// Free counts the slots no one holds right now, by trying each and letting go at once.
func (c Config) Free(ctx context.Context) (int, error) {
	const op = "slots.Free"
	if err := c.check(ctx, op); err != nil {
		return 0, err
	}
	free := 0
	for i := range c.Count {
		lock := flock.New(c.path(i))
		ok, err := lock.TryLock()
		if err != nil {
			return 0, procerr.Wrap(err, procerr.CodeSlot, op, "try a slot").With("slot", i)
		}
		if ok {
			free++
			if err := lock.Unlock(); err != nil {
				return 0, procerr.Wrap(err, procerr.CodeSlot, op, "let go of a slot").With("slot", i)
			}
		}
	}
	return free, nil
}

func (c Config) tryAll() (*Slot, error) {
	for i := range c.Count {
		lock := flock.New(c.path(i))
		ok, err := lock.TryLock()
		if err != nil {
			return nil, procerr.Wrap(err, procerr.CodeSlot, "slots.Take", "try a slot").With("slot", i)
		}
		if ok {
			return &Slot{Index: i, lock: lock}, nil
		}
	}
	return nil, nil
}

func (c Config) heldBack(ctx context.Context) (bool, error) {
	if c.Pressure == nil {
		return false, nil
	}
	reading, err := c.Pressure(ctx)
	if err != nil {
		// A pressure reading that fails holds nothing back: the slots still limit how many start.
		return false, nil //nolint:nilerr // an unreadable pressure signal must not stop every start.
	}
	holdAt := c.HoldAt
	if holdAt == pressure.Normal {
		holdAt = pressure.Critical
	}
	return reading.Level >= holdAt, nil
}

func (c Config) check(ctx context.Context, op string) error {
	if ctx == nil {
		return procerr.New(procerr.CodeInvalid, op, "context is required")
	}
	if strings.TrimSpace(c.Dir) == "" {
		return procerr.New(procerr.CodeInvalid, op, "no slots folder")
	}
	if c.Count <= 0 {
		return procerr.New(procerr.CodeInvalid, op, "the slot count must be at least 1").With("count", c.Count)
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return procerr.Wrap(err, procerr.CodeSlot, op, "make the slots folder").With("dir", c.Dir)
	}
	return nil
}

func (c Config) poll() time.Duration {
	if c.Poll > 0 {
		return c.Poll
	}
	return DefaultPoll
}

func (c Config) path(i int) string {
	return filepath.Join(c.Dir, "slot-"+strconv.Itoa(i)+".lock")
}
