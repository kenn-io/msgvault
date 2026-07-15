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

func isDaemonCLISubprocess() bool {
	return os.Getenv(daemonCLISubprocessEnv) == strconv.Itoa(os.Getppid())
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
	cmd, err := newDaemonCLISubprocessCommand(ctx, args, env, cwd)
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open CLI subprocess stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open CLI subprocess stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start CLI subprocess: %w", err)
	}

	var emitMu sync.Mutex
	emitLocked := func(stream, data string) error {
		if emit == nil {
			return nil
		}
		emitMu.Lock()
		defer emitMu.Unlock()
		return emit(stream, data)
	}

	streamErrCh := make(chan error, 2)
	go func() {
		streamErrCh <- streamDaemonCLIPipe(stdout, cliStreamStdout, emitLocked)
	}()
	go func() {
		streamErrCh <- streamDaemonCLIPipe(stderr, cliStreamStderr, emitLocked)
	}()

	firstStreamErr := <-streamErrCh
	secondStreamErr := <-streamErrCh
	waitErr := cmd.Wait()
	// The child may have changed the analytics cache regardless of how it
	// exited: an ingest rebuilds it (rebuildCacheAfterWrite) and can then
	// fail for unrelated reasons, and remove-account deletes it outright.
	// Reconcile the daemon's engine on every termination — it is a no-op
	// for read-only children and for an already-consistent engine.
	maybeAdoptAnalyticsCache()
	if firstStreamErr != nil {
		return firstStreamErr
	}
	if secondStreamErr != nil {
		return secondStreamErr
	}
	return classifyDaemonCLIWaitErr(waitErr, args)
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
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate msgvault executable: %w", err)
	}
	args := globalConfigFlagArgs()
	args = append(args, "--no-log-file")
	args = append(args, commandArgs...)

	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // exe is os.Executable; args are internally constructed.
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = daemonCLIChildEnv(os.Environ(), os.Getpid(), env)
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

func streamDaemonCLIPipe(
	r io.Reader,
	stream string,
	emit func(stream, data string) error,
) error {
	buf := make([]byte, 32*1024)
	var firstErr error
	for {
		n, err := r.Read(buf)
		if n > 0 && firstErr == nil {
			if emitErr := emit(stream, string(buf[:n])); emitErr != nil {
				firstErr = emitErr
			}
		}
		if errors.Is(err, io.EOF) {
			return firstErr
		}
		if err != nil {
			if firstErr != nil {
				return firstErr
			}
			return fmt.Errorf("read CLI subprocess %s: %w", stream, err)
		}
	}
}
