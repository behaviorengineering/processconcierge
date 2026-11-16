// Package proc starts a child process in a unit that holds it and everything it starts (a process group on macOS
// and Linux, a Job Object on Windows), and stops that whole unit: gently first, then for good once a grace period
// runs out. On Linux the child also gets a parent-death signal, so it stops even if its parent is killed outright.
package proc

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/behaviorengineering/processconcierge/pkg/procerr"
)

var tracer = otel.Tracer("github.com/behaviorengineering/processconcierge/pkg/proc")

// fail marks a span as failed and hands the error back.
func fail(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return err
}

// DefaultGrace is how long Stop waits after the gentle signal before it stops the whole unit for good.
const DefaultGrace = 5 * time.Second

// Config describes a child process and how it is stopped.
type Config struct {
	// Path is the program to run, found on the PATH when it has no slash.
	Path string
	// Args are the program's arguments, without the program itself.
	Args []string
	// Dir is the working folder; empty means the caller's.
	Dir string
	// Env is the child's environment; nil means the caller's.
	Env []string
	// Stdin, Stdout and Stderr connect the child. An *os.File passes straight through, with no copying, so a reaper
	// can hand its own stdio to the child. Nil connects the null device.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Grace is the time between the gentle signal and the hard stop. Zero uses DefaultGrace.
	Grace time.Duration
}

// Exit is how a child ended.
type Exit struct {
	// Code is the exit code, or -1 when a signal ended the child.
	Code int
	// Signal names the signal that ended the child, empty when it exited by itself.
	Signal string
	signal syscall.Signal
}

// ShellCode is the code a shell would report: the exit code, or 128 plus the signal's number.
func (e Exit) ShellCode() int {
	if e.Signal != "" {
		return 128 + int(e.signal)
	}
	return e.Code
}

// Process is a running child and the unit it leads.
type Process struct {
	cmd   *exec.Cmd
	grace time.Duration
	unit  unit
	done  chan struct{}
	exit  Exit
	err   error
}

// Start runs the child in a unit of its own. The context only guards the start: cancelling it later does not stop
// the child, which lives until Stop, Kill or its own exit, because a supervisor decides when its children end.
func (c Config) Start(ctx context.Context) (*Process, error) {
	const op = "proc.Start"
	if ctx == nil {
		return nil, procerr.New(procerr.CodeInvalid, op, "context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, procerr.Wrap(err, procerr.CodeStart, op, "cancelled before the start")
	}
	if strings.TrimSpace(c.Path) == "" {
		return nil, procerr.New(procerr.CodeInvalid, op, "no program to run")
	}
	grace := c.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	_, span := tracer.Start(ctx, "processconcierge.start", trace.WithAttributes(attribute.String("process.path", c.Path)))
	defer span.End()
	cmd := exec.Command(c.Path, c.Args...) //nolint:gosec // G204: running the caller's program is this package's job.
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	u, err := startInUnit(cmd)
	if err != nil {
		return nil, fail(span, procerr.Wrap(err, procerr.CodeStart, op, "start the child").With("path", c.Path))
	}
	span.SetAttributes(attribute.Int("process.pid", cmd.Process.Pid))
	p := &Process{cmd: cmd, grace: grace, unit: u, done: make(chan struct{})}
	go p.wait()
	return p, nil
}

func (p *Process) wait() {
	err := p.cmd.Wait()
	p.exit = exitOf(p.cmd.ProcessState)
	if p.cmd.ProcessState == nil {
		p.err = procerr.Wrap(err, procerr.CodeWait, "proc.Wait", "wait for the child").With("pid", p.Pid())
	}
	close(p.done)
}

// Pid is the child's process id, which is also its group's id on macOS and Linux.
func (p *Process) Pid() int {
	return p.cmd.Process.Pid
}

// Done is closed once the child has exited. Others in its unit may still be running.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Wait blocks until the child exits or ctx ends, and returns how the child ended.
func (p *Process) Wait(ctx context.Context) (Exit, error) {
	const op = "proc.Wait"
	if ctx == nil {
		return Exit{}, procerr.New(procerr.CodeInvalid, op, "context is required")
	}
	select {
	case <-p.done:
		return p.exit, p.err
	case <-ctx.Done():
		return Exit{}, procerr.Wrap(ctx.Err(), procerr.CodeWait, op, "stopped waiting").With("pid", p.Pid())
	}
}

// Signal sends sig to every process still in the child's unit.
func (p *Process) Signal(sig os.Signal) error {
	if err := p.unit.signal(sig); err != nil {
		return procerr.Wrap(err, procerr.CodeSignal, "proc.Signal", "signal the unit").With("pid", p.Pid()).With("signal", sig.String())
	}
	return nil
}

// Kill stops every process in the child's unit at once.
func (p *Process) Kill() error {
	if err := p.unit.kill(); err != nil {
		return procerr.Wrap(err, procerr.CodeSignal, "proc.Kill", "kill the unit").With("pid", p.Pid())
	}
	return nil
}

// Stop ends the child's whole unit: it sends the gentle signal (SIGTERM on macOS and Linux), waits up to the grace
// period for the child and everything else in its unit to exit, then kills whatever is left. A unit whose child already
// exited but left others running, such as a shell's background jobs, gets the same gentle signal and grace period.
// Stop returns once the child has exited and the unit is empty, or when ctx ends.
func (p *Process) Stop(ctx context.Context) error {
	const op = "proc.Stop"
	if ctx == nil {
		return procerr.New(procerr.CodeInvalid, op, "context is required")
	}
	ctx, span := tracer.Start(ctx, "processconcierge.stop", trace.WithAttributes(
		attribute.Int("process.pid", p.Pid()), attribute.Int64("process.grace_ms", p.grace.Milliseconds())))
	defer span.End()
	if err := p.unit.terminate(); err != nil {
		return fail(span, procerr.Wrap(err, procerr.CodeSignal, op, "send the gentle signal").With("pid", p.Pid()))
	}
	settled, err := p.settle(ctx, p.grace)
	if err != nil {
		return fail(span, procerr.Wrap(err, procerr.CodeWait, op, "stopped before the grace period ended").With("pid", p.Pid()))
	}
	// True when something was still running once the grace period ended, and only the hard stop ended it.
	span.SetAttributes(attribute.Bool("process.hard_stop", !settled))
	// Whatever is still in the unit after the gentle signal and the grace period is stopped for good.
	if err := p.unit.kill(); err != nil {
		return fail(span, procerr.Wrap(err, procerr.CodeSignal, op, "kill what is left of the unit").With("pid", p.Pid()))
	}
	select {
	case <-p.done:
		span.SetAttributes(attribute.Int("process.exit_code", p.exit.Code), attribute.String("process.exit_signal", p.exit.Signal))
		return nil
	case <-ctx.Done():
		return fail(span, procerr.Wrap(ctx.Err(), procerr.CodeWait, op, "stopped before the child exited").With("pid", p.Pid()))
	}
}

// settlePoll is how often Stop looks whether the unit has emptied during the grace period.
const settlePoll = 25 * time.Millisecond

// settle waits until the child has exited and its unit is empty, or the grace period ends, and reports which. Only
// ctx ending is an error; a grace period that runs out leaves the rest to the hard stop.
func (p *Process) settle(ctx context.Context, grace time.Duration) (bool, error) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	tick := time.NewTicker(settlePoll)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			if p.unit.empty() {
				return true, nil
			}
		default:
		}
		select {
		case <-timer.C:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		case <-tick.C:
		}
	}
}
