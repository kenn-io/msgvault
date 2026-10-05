package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const daemonCLISubprocessEnv = "MSGVAULT_DAEMON_CLI_PARENT_PID"

var daemonCLIExecutableResolver = os.Executable

func isDaemonCLISubprocess() bool {
	return os.Getenv(daemonCLISubprocessEnv) == strconv.Itoa(os.Getppid())
}

func isDaemonConsoleSubprocess() bool {
	return isDaemonCLISubprocess() || isDaemonBuildCacheChild()
}

func runDaemonCLISubprocessStream(
	ctx context.Context,
	args []string,
	emit func(stream, data string) error,
) error {
	return runDaemonCLISubprocessStreamWithEnv(ctx, args, nil, "", emit)
}

func runDaemonCLISubprocessStreamWithEnv(
	ctx context.Context,
	args []string,
	env map[string]string,
	cwd string,
	emit func(stream, data string) error,
) error {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := newDaemonCLISubprocessCommand(childCtx, args, env, cwd)
	if err != nil {
		return err
	}
	var emitMu sync.Mutex
	var streamErr error
	emitLocked := func(stream, data string) error {
		emitMu.Lock()
		defer emitMu.Unlock()
		if streamErr != nil {
			return streamErr
		}
		if emit == nil {
			return nil
		}
		if err := emit(stream, data); err != nil {
			streamErr = fmt.Errorf("stream CLI subprocess %s: %w", stream, err)
			cancel()
			return streamErr
		}
		return nil
	}
	// Let exec own its pipe-copy goroutines. Wait can then close inherited
	// pipes after WaitDelay; manual pre-Wait drains cannot use that bound.
	cmd.Stdout = daemonCLIStreamWriter{stream: cliStreamStdout, emit: emitLocked}
	cmd.Stderr = daemonCLIStreamWriter{stream: cliStreamStderr, emit: emitLocked}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start CLI subprocess: %w", err)
	}
	waitErr := cmd.Wait()
	emitMu.Lock()
	firstErr := streamErr
	emitMu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	return classifyDaemonCLIWaitErr(waitErr, args)
}

type daemonCLIStreamWriter struct {
	stream string
	emit   func(stream, data string) error
}

var _ io.Writer = daemonCLIStreamWriter{}

func (w daemonCLIStreamWriter) Write(data []byte) (int, error) {
	if err := w.emit(w.stream, string(data)); err != nil {
		return 0, err
	}
	return len(data), nil
}

// cliSubprocessExitSentinel marks a daemon CLI subprocess that ran and exited
// non-zero. It crosses the daemon→client boundary as a plain string (via the
// NDJSON error event), so both the subprocess runner and the proxy client
// match on this exact value.
const cliSubprocessExitSentinel = "msgvault: cli subprocess exited non-zero"

// classifyDaemonCLIWaitErr maps a subprocess Wait() error to what the proxy
// should report. A non-zero exit (the command ran and failed) becomes the
// sentinel: its real error was already streamed to the caller's stderr, so the
// client must not print a second, redundant "CLI subprocess ...: exit status
// 1" wrapper. Any other failure (couldn't start, signal, etc.) is wrapped with
// context because nothing else surfaced it.
func classifyDaemonCLIWaitErr(waitErr error, args []string) error {
	if waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && exitErr.Exited() {
		// A normal non-zero exit: the command ran and streamed its own error,
		// so collapse to the silent sentinel. A signal-terminated process
		// (Exited() is false, ExitCode() == -1) streamed nothing, so keep it
		// wrapped with context below.
		return errors.New(cliSubprocessExitSentinel)
	}
	return fmt.Errorf("CLI subprocess %s: %w", strings.Join(args, " "), waitErr)
}

func newDaemonCLISubprocessCommand(ctx context.Context, commandArgs []string, env map[string]string, cwd string) (*exec.Cmd, error) {
	exe, err := daemonCLIExecutableResolver()
	if err != nil {
		return nil, fmt.Errorf("locate msgvault executable: %w", err)
	}
	args := globalConfigFlagArgs(optionsFromContext(ctx))
	args = append(args, "--no-log-file")
	args = append(args, commandArgs...)

	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // exe is os.Executable; args are internally constructed.
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = daemonRuntimeChildEnv(ctx, daemonCLIChildEnv(os.Environ(), os.Getpid(), env))
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return signalDaemonProcess(cmd.Process)
	}
	cmd.WaitDelay = 30 * time.Second
	return cmd, nil
}

func daemonCLIChildEnv(base []string, parentPID int, extra map[string]string) []string {
	out := make([]string, 0, len(base)+1+len(extra))
	prefix := daemonCLISubprocessEnv + "="
	value := prefix + strconv.Itoa(parentPID)
	replaced := false
	extraReplaced := make(map[string]bool, len(extra))
	for _, entry := range base {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				out = append(out, value)
				replaced = true
			}
			continue
		}
		if key, ok := splitEnvEntry(entry); ok {
			if strings.EqualFold(key, remoteDeleteEnvVar) {
				continue
			}
			if extraValue, exists := extra[key]; exists {
				if !extraReplaced[key] {
					out = append(out, key+"="+extraValue)
					extraReplaced[key] = true
				}
				continue
			}
		}
		out = append(out, entry)
	}
	if !replaced {
		out = append(out, value)
	}
	for _, key := range sortedEnvKeys(extra) {
		if strings.EqualFold(key, remoteDeleteEnvVar) {
			if key == remoteDeleteEnvVar {
				out = append(out, remoteDeleteEnvVar+"="+extra[key])
			}
			continue
		}
		if !extraReplaced[key] {
			out = append(out, key+"="+extra[key])
		}
	}
	return out
}

func splitEnvEntry(entry string) (string, bool) {
	key, _, ok := strings.Cut(entry, "=")
	return key, ok
}

func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
