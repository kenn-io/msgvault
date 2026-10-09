package oauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

const secretCommandTimeout = 30 * time.Second
const secretCommandOutputLimit = 1 << 20

// commandExitError intentionally omits executable, argv and captured output.
type commandExitError struct{ code int }

func (e *commandExitError) Error() string {
	return fmt.Sprintf("secret command exited with status %d", e.code)
}

var errSecretOutputLimit = errors.New("secret command stdout exceeds 1 MiB")

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

func runSecretCommand(ctx context.Context, argv []string, env []string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, secretCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // The user explicitly configures this executable and literal arguments.
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(input)
	// Closing inherited pipes bounds waiting even if a wrapper leaves descendants.
	cmd.WaitDelay = time.Second
	output := &secretOutput{cancel: cancel}
	cmd.Stdout = output
	err := cmd.Run()
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
		return nil, errors.New("secret command could not start or complete")
	}
	return output.buffer.Bytes(), nil
}
