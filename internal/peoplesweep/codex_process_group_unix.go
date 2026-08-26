//go:build !windows

package peoplesweep

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func runCodexVersionCommand(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		if err != nil {
			return errors.New("kill Codex version process group")
		}
		return nil
	}
	err := command.Run()
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		return fmt.Errorf("run codex version command: %w", err)
	}
	return nil
}
