//go:build windows

package peoplesweep

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsCodexVersionJob struct {
	handle windows.Handle
}

func runCodexVersionCommand(command *exec.Cmd) (retErr error) {
	job, err := newWindowsCodexVersionJob()
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := windows.CloseHandle(job.handle); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("close Codex version Job Object: %w", closeErr)
		}
	}()
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	command.Cancel = func() error {
		if err := windows.TerminateJobObject(job.handle, 1); err != nil {
			return fmt.Errorf("terminate Codex version Job Object: %w", err)
		}
		return nil
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Codex version command: %w", err)
	}
	if err := assignWindowsCodexVersionJob(job.handle, command.Process); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	if err := resumeWindowsCodexVersionProcess(uint32(command.Process.Pid)); err != nil {
		_ = windows.TerminateJobObject(job.handle, 1)
		_ = command.Wait()
		return err
	}
	waitErr := command.Wait()
	terminateErr := windows.TerminateJobObject(job.handle, 1)
	if waitErr != nil {
		return fmt.Errorf("wait for Codex version command: %w", waitErr)
	}
	if terminateErr != nil {
		return fmt.Errorf("terminate Codex version descendants: %w", terminateErr)
	}
	return nil
}

func newWindowsCodexVersionJob() (*windowsCodexVersionJob, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create Codex version Job Object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("configure Codex version Job Object: %w", err)
	}
	return &windowsCodexVersionJob{handle: handle}, nil
}

func assignWindowsCodexVersionJob(handle windows.Handle, process *os.Process) error {
	var assignErr error
	if err := process.WithHandle(func(processHandle uintptr) {
		assignErr = windows.AssignProcessToJobObject(handle, windows.Handle(processHandle))
	}); err != nil {
		return fmt.Errorf("access Codex version process handle: %w", err)
	}
	if assignErr != nil {
		return fmt.Errorf("assign Codex version process to Job Object: %w", assignErr)
	}
	return nil
}

func resumeWindowsCodexVersionProcess(processID uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot Codex version process threads: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("enumerate Codex version process threads: %w", err)
	}
	for {
		if entry.OwnerProcessID == processID {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return fmt.Errorf("open Codex version primary thread: %w", err)
			}
			previousSuspendCount, resumeErr := windows.ResumeThread(thread)
			closeErr := windows.CloseHandle(thread)
			if resumeErr != nil {
				return fmt.Errorf("resume Codex version primary thread: %w", resumeErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close Codex version primary thread: %w", closeErr)
			}
			if previousSuspendCount != 1 {
				return fmt.Errorf("resume Codex version primary thread: unexpected suspend count %d", previousSuspendCount)
			}
			return nil
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			return fmt.Errorf("find Codex version primary thread for process %d: %w", processID, err)
		}
	}
}
