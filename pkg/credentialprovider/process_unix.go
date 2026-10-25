//go:build !windows

package credentialprovider

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func runHelper(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		if err != nil {
			return fmt.Errorf("stop credential helper: %w", err)
		}
		return nil
	}
	err := cmd.Run()
	if cmd.Process != nil {
		cleanupErr := cmd.Cancel()
		if !errors.Is(cleanupErr, os.ErrProcessDone) {
			err = errors.Join(err, cleanupErr)
		}
	}
	return err
}
