// Package reaper runs one child the way tini does in a container, for a parent that cannot supervise processes
// itself, such as a Node or Electron backend. The child gets the reaper's own stdin, stdout and stderr, untouched,
// and runs in a unit of its own. The reaper stops that whole unit when the child exits (clearing anything it left
// running), when the reaper is asked to stop, or when its owner goes away: the owner holds the other end of a pipe
// the reaper reads, and every way the owner can die, a crash or a SIGKILL included, closes it.
package reaper

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/behaviorengineering/processconcierge/pkg/proc"
	"github.com/behaviorengineering/processconcierge/pkg/procerr"
	"github.com/behaviorengineering/processconcierge/pkg/sweep"
)

// Exit codes the reaper uses for itself; otherwise it exits with the child's.
const (
	// ExitInternal means the reaper failed after starting the child.
	ExitInternal = 1
	// ExitNotStarted means the child could not be started, as a shell reports a command it cannot run.
	ExitNotStarted = 127
)

// Config is one run of the reaper.
type Config struct {
	// Path and Args are the child to run.
	Path string
	Args []string
	// Grace is the time between the gentle signal and the hard stop. Zero uses proc.DefaultGrace.
	Grace time.Duration
	// Owner is the pipe the owner holds open, or nil to watch no owner.
	Owner io.Reader
	// Registry is the folder the child is recorded in while it runs, so a later sweep can find it; empty records
	// nothing.
	Registry string
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

	if c.Registry != "" {
		reg := sweep.Registry{Dir: c.Registry}
		if err := reg.Record(ctx, child.Pid()); err != nil {
			logger.Error("could not record the child, so stopping it.", "pid", child.Pid(), "error", err)
			if stopErr := child.Stop(ctx); stopErr != nil {
				logger.Error("could not stop the child.", "pid", child.Pid(), "error", stopErr)
			}
			return ExitInternal, procerr.Wrap(err, procerr.CodeRegistry, op, "record the child").With("pid", child.Pid())
		}
		defer func() {
			if err := reg.Remove(child.Pid()); err != nil {
				logger.Error("could not remove the child's entry.", "pid", child.Pid(), "error", err)
			}
		}()
	}

	logger.Info("stopping the child's unit.", "pid", child.Pid(), "reason", c.awaitReason(child))
	// Stop also clears a unit whose child already exited, so nothing the child started outlives it.
	if err := child.Stop(ctx); err != nil {
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
	ownerGone := make(chan struct{})
	if c.Owner != nil {
		go func() {
			// Nothing is ever written; the read ends when the owner's end closes.
			_, _ = io.Copy(io.Discard, c.Owner)
			close(ownerGone)
		}()
	}
	select {
	case <-child.Done():
		return "the child exited"
	case sig := <-c.Signals:
		return "the reaper got " + sig.String()
	case <-ownerGone:
		return "the owner went away"
	}
}
