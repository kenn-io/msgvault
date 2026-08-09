//go:build windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsDetachedProcess = 0x00000008

type windowsBackgroundProcessTree struct {
	handle    windows.Handle
	closeOnce sync.Once
	closeErr  error
}

func configureServeBackgroundCommand(cmd *exec.Cmd) (backgroundServeCommandConfig, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return backgroundServeCommandConfig{}, fmt.Errorf("create Job Object: %w", err)
	}
	tree := &windowsBackgroundProcessTree{handle: job}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = tree.Close()
		}
	}()

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return backgroundServeCommandConfig{}, fmt.Errorf("configure Job Object termination: %w", err)
	}
	if err := windows.SetHandleInformation(
		job,
		windows.HANDLE_FLAG_INHERIT,
		windows.HANDLE_FLAG_INHERIT,
	); err != nil {
		return backgroundServeCommandConfig{}, fmt.Errorf("make Job Object handle inheritable: %w", err)
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP |
		windows.CREATE_SUSPENDED |
		windowsDetachedProcess
	// Keep one Job handle in the detached daemon. The launching CLI releases
	// its copy after readiness, while the daemon's inherited copy keeps the Job
	// alive until daemon exit; KILL_ON_JOB_CLOSE then removes any straggling
	// cache-build descendants.
	cmd.SysProcAttr.AdditionalInheritedHandles = append(
		cmd.SysProcAttr.AdditionalInheritedHandles,
		syscall.Handle(job),
	)
	closeOnError = false
	return backgroundServeCommandConfig{ProcessTree: tree}, nil
}

func (t *windowsBackgroundProcessTree) Attach(process *os.Process) error {
	var assignErr error
	if err := process.WithHandle(func(handle uintptr) {
		assignErr = windows.AssignProcessToJobObject(t.handle, windows.Handle(handle))
	}); err != nil {
		return fmt.Errorf("access background process handle: %w", err)
	}
	if assignErr != nil {
		return fmt.Errorf("assign process to Job Object: %w", assignErr)
	}
	if err := resumeSuspendedProcess(uint32(process.Pid)); err != nil {
		return fmt.Errorf("resume background process: %w", err)
	}
	return nil
}

func resumeSuspendedProcess(processID uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot process threads: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("enumerate process threads: %w", err)
	}
	// CREATE_SUSPENDED prevents user code from creating more threads, so the
	// process-owned thread in this snapshot is the primary thread to resume.
	for {
		if entry.OwnerProcessID == processID {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return fmt.Errorf("open primary thread: %w", err)
			}
			defer func() { _ = windows.CloseHandle(thread) }()

			previousSuspendCount, err := windows.ResumeThread(thread)
			if err != nil {
				return fmt.Errorf("resume primary thread: %w", err)
			}
			if previousSuspendCount != 1 {
				return fmt.Errorf("resume primary thread: unexpected suspend count %d", previousSuspendCount)
			}
			return nil
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			return fmt.Errorf("find primary thread for process %d: %w", processID, err)
		}
	}
}

func (t *windowsBackgroundProcessTree) Terminate() error {
	if err := windows.TerminateJobObject(t.handle, 1); err != nil {
		return fmt.Errorf("terminate Job Object: %w", err)
	}
	return nil
}

func (t *windowsBackgroundProcessTree) Close() error {
	t.closeOnce.Do(func() {
		t.closeErr = windows.CloseHandle(t.handle)
	})
	return t.closeErr
}

func signalDaemonProcess(process *os.Process) error {
	return process.Kill()
}

func killDaemonProcess(process *os.Process) error {
	return process.Kill()
}
