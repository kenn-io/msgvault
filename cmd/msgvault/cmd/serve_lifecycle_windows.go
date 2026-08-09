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
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP | windowsDetachedProcess
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
	return nil
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
