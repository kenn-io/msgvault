package tui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// openSystemTarget hands a local path or validated HTTP(S) URL to the
// platform's default application. Arguments are passed directly to the
// process, never through a shell.
func openSystemTarget(ctx context.Context, target string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{target}
	case "linux", "freebsd", "openbsd", "netbsd":
		command, args = "xdg-open", []string{target}
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		return fmt.Errorf("opening attachments is unsupported on %s", runtime.GOOS)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start %s: %w", command, err)
	}
	cmd := exec.Command(command, args...) //nolint:gosec // fixed command catalog; target is a single argument
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", command, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
