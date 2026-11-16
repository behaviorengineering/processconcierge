// Package proc starts a child process in a unit that holds it and everything it starts (a process group on macOS
// and Linux, a Job Object on Windows), and stops that whole unit: gently first, then for good once a grace period
// runs out. On Linux the child also gets a parent-death signal, and on Windows the job ends with the process that
// started it. On macOS, a parent that can be killed outright sets Config.Reaper, so its children never outlive it.
package proc

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
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

// killConfirm is how long Stop waits, after the hard stop, for the child to be gone before it checks whether the
// child is stuck in the kernel.
const killConfirm = 2 * time.Second

// errAfterCreate marks a start that failed after the child process existed, such as joining its Job Object. Such a
// start is never retried, so one failed start can never leave two children.
var errAfterCreate = errors.New("the child was created but could not be set up")

// errStuck means the child did not exit after the hard stop and is no zombie: it is inside a kernel call, with the
// kill pending.
var errStuck = errors.New("the child did not exit after the hard stop")

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
	// Reaper, when set, is the path of the processconcierge command. On macOS and Linux the child then runs under
	// `processconcierge run --owner-fd 3`, with this process holding the pipe's other end, so the child's unit is
	// stopped however this process ends, a SIGKILL included. Pid, Wait and the exit code are then the reaper's, which
	// exits with the child's code. Windows ignores it: there the child's Job Object already ends with this process.
	Reaper string
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
	// owner is this process's end of the reaper's owner pipe, when the child runs under a reaper.
	owner *os.File
}

// Start runs the child in a unit of its own. The context only guards the start: cancelling it later does not stop
// the child, which lives until Stop, Kill or its own exit, because a supervisor decides when its children end.
//
// A start the system refuses for a reason that passes (too many processes or open files, the program being replaced,
// Windows antivirus holding the file) is tried again, up to three more times within about two seconds. A start that
// fails once the child exists is never tried again.
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
	ctx, span := tracer.Start(ctx, "processconcierge.start", trace.WithAttributes(
		attribute.String("process.path", c.Path), attribute.Bool("process.reaper", c.underReaper())))
	defer span.End()

	var started launch
	attempts := 0
	u, err := failsafe.With(startRetry).WithContext(ctx).Get(func() (unit, error) {
		attempts++
		l, err := c.launch(grace)
		if err != nil {
			return unit{}, err
		}
		u, err := startInUnit(l.cmd)
		l.afterStart()
		if err != nil {
			l.abandon()
			return unit{}, err
		}
		started = l
		return u, nil
	})
	span.SetAttributes(attribute.Int("process.start_attempts", attempts))
	if err != nil {
		return nil, fail(span, procerr.Wrap(err, procerr.CodeStart, op, "start the child").With("path", c.Path))
	}
	span.SetAttributes(attribute.Int("process.pid", started.cmd.Process.Pid))
	p := &Process{cmd: started.cmd, grace: grace, unit: u, done: make(chan struct{}), owner: started.owner}
	go p.wait()
	return p, nil
}

// startRetry retries only starts the system refused for a reason that passes, with no child yet in existence.
var startRetry = retrypolicy.NewBuilder[unit]().
	HandleIf(func(_ unit, err error) bool { return retryable(err) }).
	WithMaxRetries(3).
	WithBackoff(100*time.Millisecond, time.Second).
	WithJitterFactor(0.25).
	ReturnLastFailure().
	Build()

// retryable reports whether a failed start may be tried again: the system refused it for a reason that passes, and
// no child came into existence.
func retryable(err error) bool {
	return err != nil && !errors.Is(err, errAfterCreate) && transientStart(err)
}

func (c Config) underReaper() bool {
	return c.Reaper != "" && runtime.GOOS != "windows"
}

// launch is one attempt's command; an exec.Cmd cannot be started twice, so each attempt builds its own.
type launch struct {
	cmd *exec.Cmd
	// owner and reaperEnd are the owner pipe's two ends when the child runs under a reaper.
	owner     *os.File
	reaperEnd *os.File
}

func (c Config) launch(grace time.Duration) (launch, error) {
	path, args := c.Path, c.Args
	var l launch
	if c.underReaper() {
		reaperEnd, owner, err := os.Pipe()
		if err != nil {
			return launch{}, err
		}
		l.owner, l.reaperEnd = owner, reaperEnd
		path = c.Reaper
		args = append([]string{"run", "--owner-fd", "3", "--grace", grace.String(), "--", c.Path}, c.Args...)
	}
	cmd := exec.Command(path, args...) //nolint:gosec // G204: running the caller's program is this package's job.
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	if l.reaperEnd != nil {
		cmd.ExtraFiles = []*os.File{l.reaperEnd}
	}
	l.cmd = cmd
	return l, nil
}

// afterStart closes this process's copy of the reaper's end of the pipe, which the reaper now holds.
func (l launch) afterStart() {
	if l.reaperEnd != nil {
		_ = l.reaperEnd.Close()
	}
}

// abandon closes the owner's end of a pipe whose reaper never started.
func (l launch) abandon() {
	if l.owner != nil {
		_ = l.owner.Close()
	}
}

func (p *Process) wait() {
	err := p.cmd.Wait()
	p.exit = exitOf(p.cmd.ProcessState)
	var exitErr *exec.ExitError
	// A non-zero exit is reported through Exit. Anything else, such as a failed copy of the child's output to a writer
	// that is not a file, is an error the caller must see.
	if err != nil && !errors.As(err, &exitErr) {
		p.err = procerr.Wrap(err, procerr.CodeWait, "proc.Wait", "wait for the child").With("pid", p.Pid())
	}
	if p.owner != nil {
		_ = p.owner.Close()
	}
	close(p.done)
}

// Pid is the child's process id, which is also its group's id on macOS and Linux. Under a reaper it is the reaper's.
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

// Signal sends sig to every process still in the child's unit. Under a reaper it reaches the reaper, which stops the
// child's unit gently on SIGTERM, SIGINT or SIGHUP.
func (p *Process) Signal(sig os.Signal) error {
	if err := p.unit.signal(sig); err != nil {
		return procerr.Wrap(err, procerr.CodeSignal, "proc.Signal", "signal the unit").With("pid", p.Pid()).With("signal", sig.String())
	}
	return nil
}

// Kill stops every process in the child's unit at once. Under a reaper it sends the reaper two stop requests, a
// SIGTERM and the owner pipe closing, which the reaper takes as a hard stop of the child's unit; it never kills the
// reaper alone, which would leave the child running on macOS.
func (p *Process) Kill() error {
	if err := p.hardStop(); err != nil {
		return procerr.Wrap(err, procerr.CodeSignal, "proc.Kill", "kill the unit").With("pid", p.Pid())
	}
	return nil
}

func (p *Process) hardStop() error {
	if p.owner == nil {
		return p.unit.kill()
	}
	err := p.unit.terminate()
	_ = p.owner.Close()
	return err
}

// Stop ends the child's whole unit: it sends the gentle signal (SIGTERM on macOS and Linux, CTRL_BREAK on Windows),
// waits up to the grace period for the child and everything else in its unit to exit, then kills whatever is left. A
// unit whose child already exited but left others running, such as a shell's background jobs, gets the same gentle
// signal and grace period. Stop returns once the child has exited and the unit is empty, or when ctx ends.
//
// A child that has not exited shortly after the hard stop, and is no zombie, is stuck inside a kernel call with the
// kill pending; it can run none of its own code and dies when the call returns. Stop then returns an error with
// CodeStuck rather than wait on it.
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
	if note := p.unit.gentleFailure(); note != "" {
		span.SetAttributes(attribute.String("process.gentle_signal_failed", note))
	}
	settled, err := p.settle(ctx, p.settleTime())
	if err != nil {
		return fail(span, procerr.Wrap(err, procerr.CodeWait, op, "stopped before the grace period ended").With("pid", p.Pid()))
	}
	// True when something was still running once the grace period ended, and only the hard stop ended it.
	span.SetAttributes(attribute.Bool("process.hard_stop", !settled))
	// Whatever is still in the unit after the gentle signal and the grace period is stopped for good.
	if err := p.hardStop(); err != nil {
		return fail(span, procerr.Wrap(err, procerr.CodeSignal, op, "kill what is left of the unit").With("pid", p.Pid()))
	}
	switch err := confirmGone(ctx, p.done, p.confirmTime(), func() bool { return zombie(p.Pid()) }); {
	case err == nil:
		span.SetAttributes(attribute.Int("process.exit_code", p.exit.Code), attribute.String("process.exit_signal", p.exit.Signal))
		return nil
	case errors.Is(err, errStuck):
		span.SetAttributes(attribute.Bool("process.stuck_in_kernel", true))
		return fail(span, procerr.Wrap(err, procerr.CodeStuck, op, "the child is stuck in the kernel with a kill pending").With("pid", p.Pid()))
	default:
		return fail(span, procerr.Wrap(err, procerr.CodeWait, op, "stopped before the child exited").With("pid", p.Pid()))
	}
}

// settleTime is how long Stop waits after the gentle signal. Under a reaper it adds the reaper's own grace period and
// stuck check, so the reaper finishes stopping its child before this process acts.
func (p *Process) settleTime() time.Duration {
	if p.owner != nil {
		return p.grace + killConfirm + time.Second
	}
	return p.grace
}

// confirmTime is how long Stop waits for the child to be gone after the hard stop.
func (p *Process) confirmTime() time.Duration {
	if p.owner != nil {
		return 2*killConfirm + time.Second
	}
	return killConfirm
}

// confirmGone waits up to wait for done. When it has not closed by then, a zombie is only waiting to be collected, so
// confirmGone keeps waiting; anything else is stuck, and confirmGone returns errStuck. Only ctx ending stops it early.
func confirmGone(ctx context.Context, done <-chan struct{}, wait time.Duration, zombie func() bool) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	if !zombie() {
		return errStuck
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
