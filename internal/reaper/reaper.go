// Package reaper runs one child the way tini does in a container, for a parent that cannot supervise processes
// itself, such as a Node or Electron backend. The child gets the reaper's own stdin, stdout and stderr, untouched,
// and runs in a unit of its own. The reaper stops that whole unit when the child exits (clearing anything it left
// running), when the reaper is asked to stop, or when its owner goes away. The reaper notices its owner going in one
// of two ways: the owner holds the other end of a pipe the reaper reads, which every way the owner can die closes,
// or the reaper waits on the owner's process id, which is how a Node owner on Windows is watched, since Node hands
// extra pipes to a child in a way Go does not read.
//
// The reaper watches for all of these from the moment it starts, so a stop request while it waits for a slot means
// the child is never started. A second stop request while it stops the child means a hard stop, at once.
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
	"go.opentelemetry.io/otel/trace"

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
	// ExitStuck means the child was hard-stopped but is stuck inside a kernel call with the kill pending: it can do no
	// more work and dies when the call returns. Its registry entry is kept for the next sweep (EX_SOFTWARE).
	ExitStuck = 70
	// ExitNoSlot means no slot came free in time, so the child was not started: try again later (EX_TEMPFAIL).
	ExitNoSlot = 75
	// ExitNotStarted means the child could not be started, as a shell reports a command it cannot run.
	ExitNotStarted = 127
	// ExitStoppedBeforeStart means a stop request arrived before the child started, so it never did. It is the code a
	// shell reports for SIGTERM.
	ExitStoppedBeforeStart = 143
)

// Config is one run of the reaper.
type Config struct {
	// Path and Args are the child to run.
	Path string
	Args []string
	// Env is the child's environment; nil means the reaper's own.
	Env []string
	// Stdin, Stdout and Stderr connect the child; nil means the reaper's own, passed straight through.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Grace is the time between the gentle signal and the hard stop. Zero uses proc.DefaultGrace.
	Grace time.Duration
	// Owner is the pipe the owner holds open, or nil to watch no pipe. Run closes it when it returns, when it is an
	// io.Closer, which ends the read that watches it.
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
// the child's, as a shell would report it, or one of the reaper's own Exit codes.
func (c Config) Run(ctx context.Context) (int, error) {
	const op = "reaper.Run"
	if ctx == nil {
		return ExitInternal, procerr.New(procerr.CodeInvalid, op, "context is required")
	}
	if strings.TrimSpace(c.Path) == "" {
		return ExitInternal, procerr.New(procerr.CodeInvalid, op, "no program to run")
	}
	c = c.withDefaults()
	requests, stopWatching := c.watch()
	defer stopWatching()

	if c.Slots != nil {
		slot, reason, err := c.takeSlot(ctx, requests)
		if reason != "" {
			c.Logger.Info("asked to stop before the child started, so it was never started.", "reason", reason)
			return ExitStoppedBeforeStart, nil
		}
		if err != nil {
			return ExitNoSlot, err
		}
		c.Logger.Info("slot taken.", "slot", slot.Index)
		defer func() {
			if err := slot.Release(); err != nil {
				c.Logger.Error("could not release the slot.", "slot", slot.Index, "error", err)
			}
		}()
	}
	// A request that arrived as the slot came free still keeps the child from starting.
	select {
	case reason := <-requests:
		c.Logger.Info("asked to stop before the child started, so it was never started.", "reason", reason)
		return ExitStoppedBeforeStart, nil
	default:
	}

	child, err := proc.Config{
		Path: c.Path, Args: c.Args, Env: c.Env,
		Stdin: c.Stdin, Stdout: c.Stdout, Stderr: c.Stderr,
		Grace: c.Grace,
	}.Start(ctx)
	if err != nil {
		return ExitNotStarted, procerr.Wrap(err, procerr.CodeStart, op, "start the child")
	}
	c.Logger.Info("child started.", "pid", child.Pid(), "path", c.Path)

	forget, err := c.record(ctx, child)
	if err != nil {
		return ExitInternal, err
	}
	keepEntry := false
	defer func() {
		if !keepEntry {
			forget()
		}
	}()

	var reason string
	select {
	case <-child.Done():
		reason = "the child exited"
	case reason = <-requests:
	}
	c.Logger.Info("stopping the child's unit.", "pid", child.Pid(), "reason", reason)
	code, keep, err := stopOutcome(c.stop(ctx, child, requests, reason))
	if err != nil {
		keepEntry = keep
		c.Logger.Error("could not stop the child's unit cleanly.", "pid", child.Pid(), "error", err, "entry_kept", keep)
		return code, procerr.Wrap(err, procerr.CodeSignal, op, "stop the child's unit").With("pid", child.Pid())
	}
	exit, err := child.Wait(ctx)
	if err != nil {
		return ExitInternal, procerr.Wrap(err, procerr.CodeWait, op, "wait for the child").With("pid", child.Pid())
	}
	c.Logger.Info("child ended.", "pid", child.Pid(), "code", exit.Code, "signal", exit.Signal)
	return exit.ShellCode(), nil
}

func (c Config) withDefaults() Config {
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	return c
}

// stopOutcome turns a failed stop into the reaper's exit code, and says whether the child's registry entry stays: it
// does for a child stuck in the kernel, so the next sweep confirms it is gone.
func stopOutcome(err error) (int, bool, error) {
	switch {
	case err == nil:
		return 0, false, nil
	case procerr.Is(err, procerr.CodeStuck):
		return ExitStuck, true, err
	default:
		return ExitInternal, false, err
	}
}

// stop runs the child's Stop, and turns a second stop request during it into a hard stop.
func (c Config) stop(ctx context.Context, child *proc.Process, requests <-chan string, reason string) error {
	ctx, span := tracer.Start(ctx, "processconcierge.reaper.stop", trace.WithAttributes(
		attribute.Int("process.pid", child.Pid()), attribute.String("reaper.reason", reason)))
	defer span.End()
	result := make(chan error, 1)
	// Stop also clears a unit whose child already exited, so nothing the child started outlives it.
	go func() { result <- child.Stop(ctx) }()
	for {
		select {
		case err := <-result:
			return err
		case again := <-requests:
			span.AddEvent("hard stop", trace.WithAttributes(attribute.String("reaper.reason", again)))
			c.Logger.Info("asked again, so stopping the child's unit at once.", "pid", child.Pid(), "reason", again)
			if err := child.Kill(); err != nil {
				c.Logger.Error("could not kill the child's unit.", "pid", child.Pid(), "error", err)
			}
		}
	}
}

// watch starts watching the owner's pipe, the owner's process and the stop signals, and returns the channel each stop
// request arrives on, with the function that ends every watch.
func (c Config) watch() (<-chan string, func()) {
	requests := make(chan string, 8)
	done := make(chan struct{})
	send := func(reason string) {
		select {
		case requests <- reason:
		case <-done:
		}
	}
	if c.Owner != nil {
		go c.watchPipe(send, done)
	}
	if c.OwnerPID > 0 {
		go func() {
			select {
			case <-watchPID(c.OwnerPID, done):
				send("the owner's process exited")
			case <-done:
			}
		}()
	}
	if c.Signals != nil {
		go c.watchSignals(send, done)
	}
	return requests, func() {
		close(done)
		if closer, ok := c.Owner.(io.Closer); ok {
			_ = closer.Close()
		}
	}
}

// watchPipe reads the owner's pipe until it closes. Nothing is ever written; the read ends when the owner's end
// closes, or fails at once on a bad descriptor.
func (c Config) watchPipe(send func(string), done <-chan struct{}) {
	_, err := io.Copy(io.Discard, c.Owner)
	select {
	case <-done:
		return // Run closed the pipe on its way out
	default:
	}
	if err != nil {
		send("the owner's pipe could not be read: " + err.Error())
		return
	}
	send("the owner's pipe closed")
}

// watchSignals passes each stop signal on as a stop request, until done closes.
func (c Config) watchSignals(send func(string), done <-chan struct{}) {
	for {
		select {
		case sig := <-c.Signals:
			send("the reaper got " + sig.String())
		case <-done:
			return
		}
	}
}

// takeSlot waits for a slot, and gives up as soon as a stop request arrives, returning its reason.
func (c Config) takeSlot(ctx context.Context, requests <-chan string) (*slots.Slot, string, error) {
	if c.SlotWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.SlotWait)
		defer cancel()
	}
	ctx, giveUp := context.WithCancel(ctx)
	defer giveUp()
	type taken struct {
		slot *slots.Slot
		err  error
	}
	result := make(chan taken, 1)
	go func() {
		slot, err := c.Slots.Take(ctx)
		result <- taken{slot, err}
	}()
	select {
	case t := <-result:
		if t.err != nil {
			return nil, "", procerr.Wrap(t.err, procerr.CodeSlot, "reaper.Run", "take a slot").With("dir", c.Slots.Dir)
		}
		return t.slot, "", nil
	case reason := <-requests:
		giveUp()
		if t := <-result; t.slot != nil {
			_ = t.slot.Release()
		}
		return nil, reason, nil
	}
}

// record adds the child to the registry, when there is one, and returns the function that removes it. A child that
// cannot be recorded is stopped, since a later sweep could not find it.
func (c Config) record(ctx context.Context, child *proc.Process) (func(), error) {
	if c.Registry == "" {
		return func() {}, nil
	}
	reg := sweep.Registry{Dir: c.Registry}
	if err := reg.Record(ctx, child.Pid()); err != nil {
		c.Logger.Error("could not record the child, so stopping it.", "pid", child.Pid(), "error", err)
		if stopErr := child.Stop(ctx); stopErr != nil {
			c.Logger.Error("could not stop the child.", "pid", child.Pid(), "error", stopErr)
		}
		return nil, procerr.Wrap(err, procerr.CodeRegistry, "reaper.Run", "record the child").With("pid", child.Pid())
	}
	return func() {
		if err := reg.Remove(child.Pid()); err != nil {
			c.Logger.Error("could not remove the child's entry.", "pid", child.Pid(), "error", err)
		}
	}, nil
}
