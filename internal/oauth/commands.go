package oauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/mattn/go-isatty"
)

const secretCommandTimeout = 30 * time.Second
const secretCommandOutputLimit = 1 << 20

// commandExitError intentionally omits executable, argv and captured output.
type commandExitError struct{ code int }

func (e *commandExitError) Error() string {
	return fmt.Sprintf("secret command exited with status %d", e.code)
}

var errSecretOutputLimit = errors.New("secret command stdout exceeds 1 MiB")

// ErrSecretCommandStart marks a configured command that could not be started,
// such as a missing or non-executable program. Retrying cannot fix it.
var ErrSecretCommandStart = errors.New(
	"secret command could not start; check the configured executable path and permissions")

type secretOutput struct {
	buffer    bytes.Buffer
	cancel    context.CancelFunc
	overLimit bool
}

func (b *secretOutput) Write(p []byte) (int, error) {
	if len(p) > secretCommandOutputLimit-b.buffer.Len() {
		b.overLimit = true
		b.cancel()
		return 0, errSecretOutputLimit
	}
	n, err := b.buffer.Write(p)
	if err != nil {
		return n, fmt.Errorf("buffer secret command output: %w", err)
	}
	return n, nil
}

// secretCommandStderr shows a wrapper's diagnostics only to a person at a
// terminal. Daemon and redirected output discard it, so logs never capture it.
func secretCommandStderr() io.Writer {
	fd := os.Stderr.Fd()
	if isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd) {
		return os.Stderr
	}
	return nil
}

func runSecretCommand(
	ctx context.Context, argv []string, env []string, input []byte,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, secretCommandTimeout)
	defer cancel()
	//nolint:gosec // The user explicitly configures this executable and literal arguments.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = secretCommandStderr()
	// Closing inherited pipes bounds waiting even if a wrapper leaves descendants.
	cmd.WaitDelay = time.Second
	output := &secretOutput{cancel: cancel}
	cmd.Stdout = output
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("secret command: %w", ctx.Err())
		}
		return nil, ErrSecretCommandStart
	}
	err := cmd.Wait()
	if output.overLimit {
		return nil, errSecretOutputLimit
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("secret command: %w", ctx.Err())
	}
	if err != nil {
		if status, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, &commandExitError{code: status.ExitCode()}
		}
		return nil, errors.New("secret command did not complete; it may have left a process holding its output")
	}
	return output.buffer.Bytes(), nil
}
