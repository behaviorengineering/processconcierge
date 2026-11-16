package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// unit is the child's Job Object, set to kill every process in it when its last handle closes. The child starts
// suspended, joins the job, and only then runs, so nothing it starts can begin outside the job. Children of a process
// in a job join the same job, and the job allows no breakaway. The handle is not inheritable and only this process
// holds it, so whenever this process ends, by any means, Windows ends the whole job.
type unit struct {
	state *jobState
}

type jobState struct {
	mu     sync.Mutex
	job    windows.Handle
	pid    uint32
	closed bool
	// gentleFailed is why the last CTRL_BREAK did not reach the child, if it did not.
	gentleFailed string
}

// jobAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which x/sys/windows does not define.
type jobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// startInUnit ignores parentDeath: the job already ends with this process.
func startInUnit(cmd *exec.Cmd, _ bool) (unit, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return unit{}, fmt.Errorf("create a job object: %w", err)
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil { //nolint:gosec // G103: the Win32 call takes the struct by pointer.
		_ = windows.CloseHandle(job)
		return unit{}, fmt.Errorf("set the job to kill on close: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return unit{}, err
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: Windows process ids are 32-bit.
	if err := assignAndResume(job, pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // releases the process handle os/exec holds
		_ = windows.CloseHandle(job)
		// The child existed, so this start is never retried.
		return unit{}, fmt.Errorf("%w: %w", errAfterCreate, err)
	}
	return unit{state: &jobState{job: job, pid: pid}}, nil
}

func assignAndResume(job windows.Handle, pid uint32) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("open the child to put it in its job: %w", err)
	}
	defer func() { _ = windows.CloseHandle(process) }()
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return fmt.Errorf("put the child in its job: %w", err)
	}
	return resumeThreads(pid)
}

// resumeThreads resumes every thread of a process started suspended. The main thread.s handle is closed by os/exec, so
// the threads are found through a snapshot instead.
func resumeThreads(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("list threads to resume the child: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return fmt.Errorf("open a thread of the child: %w", openErr)
		}
		_, resumeErr := windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		if resumeErr != nil {
			return fmt.Errorf("resume a thread of the child: %w", resumeErr)
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("list threads to resume the child: %w", err)
	}
	if resumed == 0 {
		return errors.New("found no thread of the child to resume")
	}
	return nil
}

func (u unit) signal(sig os.Signal) error {
	switch sig {
	case os.Kill:
		return u.kill()
	case os.Interrupt:
		return u.terminate()
	default:
		return fmt.Errorf("%v is not a signal Windows can send to a job", sig)
	}
}

// terminate sends CTRL_BREAK to the child's process group. It reaches the child only when both share a console, which
// a child with piped stdio often does not, so a failure is not an error: the grace period and the hard stop follow,
// and gentleFailure says why, for the stop's span.
func (u unit) terminate() error {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	u.state.gentleFailed = ""
	if !u.state.closed {
		if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, u.state.pid); err != nil {
			u.state.gentleFailed = err.Error()
		}
	}
	return nil
}

// gentleFailure says why the last CTRL_BREAK did not reach the child, or nothing when it did.
func (u unit) gentleFailure() string {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	return u.state.gentleFailed
}

// kill ends every process in the job, then closes the job, whose kill-on-close also ends anything that joined it
// in between.
func (u unit) kill() error {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	if u.state.closed {
		return nil
	}
	err := windows.TerminateJobObject(u.state.job, 1)
	closeErr := windows.CloseHandle(u.state.job)
	u.state.closed = true
	if err != nil {
		return fmt.Errorf("terminate the job: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close the job: %w", closeErr)
	}
	return nil
}

func (u unit) empty() bool {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	if u.state.closed {
		return true
	}
	var info jobAccounting
	if err := windows.QueryInformationJobObject(u.state.job, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil { //nolint:gosec // G103: the Win32 call takes the struct by pointer.
		return false
	}
	return info.ActiveProcesses == 0
}

// GroupAlive reports no group on Windows: a child's job dies with the process that held it, so nothing is left to
// find by the child's pid.
func GroupAlive(int) bool {
	return false
}

// KillGroup ends the process with that pid. On Windows its job already ended whatever else it started when the job's
// holder exited, so only the process itself can be left.
func KillGroup(pid int) error {
	if pid <= 4 {
		return fmt.Errorf("refusing to kill process %d", pid)
	}
	process, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)) //nolint:gosec // G115: Windows process ids are 32-bit.
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(process) }()
	return windows.TerminateProcess(process, 1)
}

func exitOf(state *os.ProcessState) Exit {
	if state == nil {
		return Exit{Code: -1}
	}
	return Exit{Code: state.ExitCode()}
}
