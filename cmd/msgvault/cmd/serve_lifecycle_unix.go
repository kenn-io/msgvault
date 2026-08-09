//go:build !windows

package cmd

import (
	"os"
	"os/exec"
	"syscall"
)

func configureServeBackgroundCommand(cmd *exec.Cmd) (backgroundServeCommandConfig, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	return backgroundServeCommandConfig{}, nil
}

func signalDaemonProcess(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}

func killDaemonProcess(process *os.Process) error {
	return process.Kill()
}
