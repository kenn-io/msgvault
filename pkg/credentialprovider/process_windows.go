//go:build windows

package credentialprovider

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type helperJob struct {
	handle windows.Handle
}

func runHelper(command *exec.Cmd) (retErr error) {
	job, err := newHelperJob()
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := windows.CloseHandle(job.handle); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("close credential helper Job Object: %w", closeErr)
		}
	}()
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	command.Cancel = func() error {
		if err := windows.TerminateJobObject(job.handle, 1); err != nil {
			return fmt.Errorf("terminate credential helper Job Object: %w", err)
		}
		return nil
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start credential helper command: %w", err)
	}
	if err := assignHelperJob(job.handle, command.Process); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	// #nosec G115 -- this PID came from Windows process creation and is a native DWORD.
	if err := resumeHelperProcess(uint32(command.Process.Pid)); err != nil {
		_ = windows.TerminateJobObject(job.handle, 1)
		_ = command.Wait()
		return err
	}
	waitErr := command.Wait()
	terminateErr := windows.TerminateJobObject(job.handle, 1)
	if waitErr != nil {
		return fmt.Errorf("wait for credential helper command: %w", waitErr)
	}
	if terminateErr != nil {
		return fmt.Errorf("terminate credential helper descendants: %w", terminateErr)
	}
	return nil
}

func newHelperJob() (*helperJob, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create credential helper Job Object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		handle,
		windows.JobObjectExtendedLimitInformation,
		// #nosec G103 -- SetInformationJobObject receives the address and exact size of the live JOBOBJECT_EXTENDED_LIMIT_INFORMATION value.
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("configure credential helper Job Object: %w", err)
	}
	return &helperJob{handle: handle}, nil
}

func assignHelperJob(handle windows.Handle, process *os.Process) error {
	var assignErr error
	if err := process.WithHandle(func(processHandle uintptr) {
		assignErr = windows.AssignProcessToJobObject(handle, windows.Handle(processHandle))
	}); err != nil {
		return fmt.Errorf("access credential helper process handle: %w", err)
	}
	if assignErr != nil {
		return fmt.Errorf("assign credential helper process to Job Object: %w", assignErr)
	}
	return nil
}

func resumeHelperProcess(processID uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot credential helper process threads: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("enumerate credential helper process threads: %w", err)
	}
	for {
		if entry.OwnerProcessID == processID {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return fmt.Errorf("open credential helper primary thread: %w", err)
			}
			previousSuspendCount, resumeErr := windows.ResumeThread(thread)
			closeErr := windows.CloseHandle(thread)
			if resumeErr != nil {
				return fmt.Errorf("resume credential helper primary thread: %w", resumeErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close credential helper primary thread: %w", closeErr)
			}
			if previousSuspendCount != 1 {
				return fmt.Errorf("resume credential helper primary thread: unexpected suspend count %d", previousSuspendCount)
			}
			return nil
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			return fmt.Errorf("find credential helper primary thread for process %d: %w", processID, err)
		}
	}
}
