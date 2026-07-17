//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"syscall"
)

const windowsDetachedProcess = 0x00000008

func configureServeBackgroundCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP | windowsDetachedProcess
}

func signalDaemonProcess(process *os.Process) error {
	return process.Kill()
}

func killDaemonProcess(process *os.Process) error {
	return process.Kill()
}
