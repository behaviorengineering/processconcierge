// Package reaper runs one child the way tini does in a container, for a parent that cannot supervise processes
// itself, such as a Node or Electron backend. The child gets the reaper's own stdin, stdout and stderr, untouched,
// and runs in a unit of its own. The reaper stops that whole unit when the child exits (clearing anything it left
// running), when the reaper is asked to stop, or when its owner goes away. The reaper notices its owner going in one
// of two ways: the owner holds the other end of a pipe the reaper reads, which every way the owner can die closes,
// or the reaper waits on the owner's process id, which is how a Node owner on Windows is watched, since Node hands
// extra pipes to a child in a way Go does not read.
package reaper

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/behaviorengineering/processconcierge/pkg/proc"
	"github.com/behaviorengineering/processconcierge/pkg/procerr"
	"github.com/behaviorengineering/processconcierge/pkg/slots"
	"github.com/behaviorengineering/processconcierge/pkg/sweep"
)

var tracer = otel.Tracer("github.com/behaviorengineering/processconcierge/internal/reaper")

// Exit codes the reaper uses for itself; otherwise it exits with the child's.
const (
	// ExitInternal means the reaper failed after starting the child.
	ExitInternal = 1
	// ExitNotStarted means the child could not be started, as a shell reports a command it cannot run.
	ExitNotStarted = 127
	// ExitNoSlot means no slot came free in time, so the child was not started: try again later (EX_TEMPFAIL).
	ExitNoSlot = 75
)

// Config is one run of the reaper.
type Config struct {
	// Path and Args are the child to run.
	Path string
	Args []string
	// Grace is the time between the gentle signal and the hard stop. Zero uses proc.DefaultGrace.
	Grace time.Duration
	// Owner is the pipe the owner holds open, or nil to watch no pipe.
	Owner io.Reader
	// OwnerPID is the owner's process id, watched until it exits, or zero to watch no process.
	OwnerPID int
	// Registry is the folder the child is recorded in while it runs, so a later sweep can find it; empty records
	// nothing.
	Registry string
	// Slots, when set, must give the reaper a slot before it starts the child; the reaper holds it until the child's
	// unit has stopped, and the system gives it back if the reaper dies.
	Slots *slots.Config
	// SlotWait is how long to wait for a slot before giving up with ExitNoSlot. Zero waits as long as ctx allows.
	SlotWait time.Duration
	// Signals carries the stop requests the reaper receives, such as SIGTERM. Nil means none.
	Signals <-chan os.Signal
	// Logger receives what the reaper does. Nil discards it.
	Logger *slog.Logger
}

// Run starts the child, waits for a reason to stop, stops the child's whole unit, and returns the exit code to use:
// the child's, as a shell would report it.
func (c Config) Run(ctx context.Context) (int, error) {
	const op = "reaper.Run"
	if ctx == nil {
		return ExitInternal, procerr.New(procerr.CodeInvalid, op, "context is required")
	}
	if strings.TrimSpace(c.Path) == "" {
		return ExitInternal, procerr.New(procerr.CodeInvalid, op, "no program to run")
	}
	logger := c.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.Slots != nil {
		slot, err := c.takeSlot(ctx)
		if err != nil {
			return ExitNoSlot, err
		}
		logger.Info("slot taken.", "slot", slot.Index)
		defer func() {
			if err := slot.Release(); err != nil {
				logger.Error("could not release the slot.", "slot", slot.Index, "error", err)
			}
		}()
	}
	child, err := proc.Config{
		Path:   c.Path,
		Args:   c.Args,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Grace:  c.Grace,
	}.Start(ctx)
	if err != nil {
		return ExitNotStarted, procerr.Wrap(err, procerr.CodeStart, op, "start the child")
	}
	logger.Info("child started.", "pid", child.Pid(), "path", c.Path)

	forget, err := c.record(ctx, child, logger)
	if err != nil {
		return ExitInternal, err
	}
	defer forget()

	reason := c.awaitReason(child)
	logger.Info("stopping the child's unit.", "pid", child.Pid(), "reason", reason)
	stopCtx, span := tracer.Start(ctx, "processconcierge.reaper.stop")
	span.SetAttributes(attribute.Int("process.pid", child.Pid()), attribute.String("reaper.reason", reason))
	// Stop also clears a unit whose child already exited, so nothing the child started outlives it.
	err = child.Stop(stopCtx)
	span.End()
	if err != nil {
		return ExitInternal, procerr.Wrap(err, procerr.CodeSignal, op, "stop the child's unit").With("pid", child.Pid())
	}
	exit, err := child.Wait(ctx)
	if err != nil {
		return ExitInternal, procerr.Wrap(err, procerr.CodeWait, op, "wait for the child").With("pid", child.Pid())
	}
	logger.Info("child ended.", "pid", child.Pid(), "code", exit.Code, "signal", exit.Signal)
	return exit.ShellCode(), nil
}

// awaitReason blocks until the child exits, a stop request arrives, or the owner goes away, and says which.
func (c Config) awaitReason(child *proc.Process) string {
	pipeClosed := make(chan struct{})
	if c.Owner != nil {
		go func() {
			// Nothing is ever written; the read ends when the owner's end closes.
			_, _ = io.Copy(io.Discard, c.Owner)
			close(pipeClosed)
		}()
	}
	var ownerExited <-chan struct{}
	if c.OwnerPID > 0 {
		ownerExited = watchPID(c.OwnerPID)
	}
	select {
	case <-child.Done():
		return "the child exited"
	case sig := <-c.Signals:
		return "the reaper got " + sig.String()
	case <-pipeClosed:
		return "the owner's pipe closed"
	case <-ownerExited:
		return "the owner's process exited"
	}
}

func (c Config) takeSlot(ctx context.Context) (*slots.Slot, error) {
	if c.SlotWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.SlotWait)
		defer cancel()
	}
	slot, err := c.Slots.Take(ctx)
	if err != nil {
		return nil, procerr.Wrap(err, procerr.CodeSlot, "reaper.Run", "take a slot").With("dir", c.Slots.Dir)
	}
	return slot, nil
}

// record adds the child to the registry, when there is one, and returns the function that removes it. A child that
// cannot be recorded is stopped, since a later sweep could not find it.
func (c Config) record(ctx context.Context, child *proc.Process, logger *slog.Logger) (func(), error) {
	if c.Registry == "" {
		return func() {}, nil
	}
	reg := sweep.Registry{Dir: c.Registry}
	if err := reg.Record(ctx, child.Pid()); err != nil {
		logger.Error("could not record the child, so stopping it.", "pid", child.Pid(), "error", err)
		if stopErr := child.Stop(ctx); stopErr != nil {
			logger.Error("could not stop the child.", "pid", child.Pid(), "error", stopErr)
		}
		return nil, procerr.Wrap(err, procerr.CodeRegistry, "reaper.Run", "record the child").With("pid", child.Pid())
	}
	return func() {
		if err := reg.Remove(child.Pid()); err != nil {
			logger.Error("could not remove the child's entry.", "pid", child.Pid(), "error", err)
		}
	}, nil
}
