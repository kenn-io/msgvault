package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHelperProcess is a cross-platform subprocess used by the classify tests.
// It re-execs the test binary rather than depending on an external shell (sh),
// which may be absent on minimal Linux images and on Windows.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("GO_HELPER_MODE") {
	case "paused":
		os.Exit(ExitCodeSyncPaused)
	case "exit3":
		os.Exit(3)
	case "stdout-stderr-ok":
		_, _ = fmt.Fprintln(os.Stdout, "cache build detail")
		_, _ = fmt.Fprintln(os.Stderr, "cache build warning")
		os.Exit(0)
	case "block":
		select {}
	case "spawn-blocking-child":
		child := helperProcessCommand(t.Context(), "block")
		if err := child.Start(); err != nil {
			os.Exit(10)
		}
		pidPath := os.Getenv("GO_HELPER_CHILD_PID_PATH")
		if err := os.WriteFile(pidPath, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			os.Exit(11)
		}
		_ = child.Wait()
		os.Exit(0)
	default:
		os.Exit(0)
	}
}

func helperProcessCommand(ctx context.Context, mode string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess") //nolint:gosec // os.Args[0] is the test binary; args are fixed test flags.
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "GO_HELPER_MODE="+mode)
	return cmd
}

func TestClassifyDaemonCLIWaitErrExitStatus(t *testing.T) {
	for _, mode := range []string{"exit3", "paused"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			waitErr := helperProcessCommand(t.Context(), mode).Run()
			require.Error(waitErr)
			var events []string
			got := classifyDaemonCLIWaitErr(waitErr, []string{"sync-omi"}, func(stream, data string) error {
				events = append(events, stream+"="+data)
				return nil
			})
			if mode == "paused" {
				require.NoError(got)
				assert.Equal([]string{"outcome=paused"}, events)
			} else {
				require.EqualError(got, cliSubprocessExitSentinel)
				assert.Empty(events)
			}
		})
	}
}

// TestClassifyDaemonCLIWaitErrSignalTerminated verifies a signal-terminated
// subprocess (which also surfaces as *exec.ExitError) stays wrapped with
// context rather than collapsing to the silent sentinel — nothing was streamed
// to the caller for a killed process. Unix-only: Windows lacks signal exits
// (TerminateProcess reports a normal exit code).
func TestClassifyDaemonCLIWaitErrSignalTerminated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-terminated exit semantics are Unix-specific")
	}
	require := require.New(t)

	cmd := helperProcessCommand(t.Context(), "block")
	require.NoError(cmd.Start(), "start blocking helper")
	require.NoError(cmd.Process.Kill(), "kill helper")
	waitErr := cmd.Wait()
	require.Error(waitErr, "killed process should error")
	var exitErr *exec.ExitError
	require.ErrorAs(waitErr, &exitErr, "want *exec.ExitError")
	require.False(exitErr.Exited(), "killed process did not exit normally")

	got := classifyDaemonCLIWaitErr(waitErr, []string{"show-deletion", "0"}, nil)
	require.Error(got, "want error")
	require.ErrorContains(got, "CLI subprocess show-deletion 0",
		"signal termination keeps context instead of the silent sentinel")
	require.ErrorIs(got, waitErr, "wraps the original wait error")
}

func TestClassifyDaemonCLIWaitErrOtherFailure(t *testing.T) {
	require := require.New(t)

	base := errors.New("fork/exec: permission denied")
	got := classifyDaemonCLIWaitErr(base, []string{"logs"}, nil)
	require.Error(got, "want error")
	require.ErrorContains(got, "CLI subprocess logs", "non-exit failures keep context")
	require.ErrorIs(got, base, "wraps the original error")
}

func TestDaemonCLIChildEnvAppliesAllowlistedEnvOverrides(t *testing.T) {
	got := daemonCLIChildEnv(
		[]string{
			"PATH=/usr/bin",
			daemonCLISubprocessEnv + "=old",
			"MSGVAULT_IMAP_PASSWORD=old-secret",
		},
		123,
		map[string]string{"MSGVAULT_IMAP_PASSWORD": "new-secret"},
	)

	assert.Equal(t, []string{
		"PATH=/usr/bin",
		daemonCLISubprocessEnv + "=" + strconv.Itoa(123),
		"MSGVAULT_IMAP_PASSWORD=new-secret",
	}, got)
}

func TestDaemonCLIChildEnvStripsUnforwardedRemoteDeleteConsent(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"msgvault_enable_remote_delete=1",
		"MsgVault_Enable_Remote_Delete=true",
		remoteDeleteEnvVar + "=1",
	}
	tests := []struct {
		name  string
		extra map[string]string
		want  []string
	}{
		{name: "no consent"},
		{name: "lowercase extra ignored", extra: map[string]string{"msgvault_enable_remote_delete": "1"}},
		{name: "mixed case extra ignored", extra: map[string]string{"MsgVault_Enable_Remote_Delete": "1"}},
		{name: "canonical consent", extra: map[string]string{remoteDeleteEnvVar: "1"}, want: []string{remoteDeleteEnvVar + "=1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := daemonCLIChildEnv(base, 123, tt.extra)
			var equivalents []string
			for _, entry := range got {
				key, ok := splitEnvEntry(entry)
				if ok && strings.EqualFold(key, remoteDeleteEnvVar) {
					equivalents = append(equivalents, entry)
				}
			}
			assert.Equal(t, tt.want, equivalents)
		})
	}
}

func TestNewDaemonCLISubprocessCommandAppliesWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()

	cmd, err := newDaemonCLISubprocessCommand(t.Context(), []string{"version"}, nil, cwd)

	require.NoError(t, err, "newDaemonCLISubprocessCommand")
	assert.Equal(t, cwd, cmd.Dir, "working directory")
}

// pipeReadScript returns each supplied fragment separately, then its terminal error.
type pipeReadScript struct {
	fragments [][]byte
	terminal  error
}

func (r *pipeReadScript) Read(p []byte) (int, error) {
	if len(r.fragments) == 0 {
		return 0, r.terminal
	}
	n := copy(p, r.fragments[0])
	r.fragments[0] = r.fragments[0][n:]
	if len(r.fragments[0]) == 0 {
		r.fragments = r.fragments[1:]
	}
	return n, nil
}

func TestStreamDaemonCLIPipeValidUTF8Chunks(t *testing.T) {
	for _, stream := range []string{cliStreamStdout, cliStreamStderr} {
		for _, value := range []string{"", "ascii", "é", "界", "😀", strings.Repeat("x", 33000) + "é界😀"} {
			t.Run(fmt.Sprintf("%s/%q", stream, value[:min(len(value), 8)]), func(t *testing.T) {
				var got strings.Builder
				reader := io.Reader(strings.NewReader(value))
				if len(value) < 100 {
					reader = iotest.OneByteReader(reader)
				}
				err := streamDaemonCLIPipe(reader, stream, func(label, data string) error {
					assert.Equal(t, stream, label)
					assert.NotEmpty(t, data)
					assert.True(t, utf8.ValidString(data), "chunk must be valid UTF-8: %q", data)
					assert.LessOrEqual(t, len(data), 32*1024)
					got.WriteString(data)
					return nil
				})
				require.NoError(t, err)
				assert.Equal(t, value, got.String())
			})
		}
	}
}

type pipeTerminalReader struct {
	data []byte
	err  error
}

func (r *pipeTerminalReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestStreamDaemonCLIPipeTerminalAndMalformedBytes(t *testing.T) {
	readErr := errors.New("synthetic read failure")
	for _, tc := range []struct {
		name   string
		reader io.Reader
		want   string
		err    error
	}{
		{"data with EOF", &pipeTerminalReader{data: []byte("é"), err: io.EOF}, "é", nil},
		{"data with error", &pipeTerminalReader{data: []byte("界"), err: readErr}, "界", readErr},
		{"held suffix then EOF", &pipeReadScript{fragments: [][]byte{[]byte("prefix\xe2\x82")}, terminal: io.EOF}, "prefix\xe2\x82", nil},
		{"held suffix then error", &pipeReadScript{fragments: [][]byte{[]byte("prefix\xf0\x9f")}, terminal: readErr}, "prefix\xf0\x9f", readErr},
		{"malformed bytes", &pipeReadScript{fragments: [][]byte{[]byte("\xe0\x80"), []byte("\x80"), []byte("a\xff")}, terminal: io.EOF}, "\xe0\x80\x80a\xff", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bytes.Buffer
			err := streamDaemonCLIPipe(tc.reader, cliStreamStdout, func(_ string, data string) error { _, _ = got.WriteString(data); return nil })
			assert.ErrorIs(t, err, tc.err)
			assert.Equal(t, tc.want, got.String())
		})
	}
}

func TestStreamDaemonCLIPipeDrainsAfterEmitFailure(t *testing.T) {
	emitErr, readErr := errors.New("emit failure"), errors.New("read failure")
	reader := &pipeReadScript{fragments: [][]byte{[]byte("first"), []byte("second"), []byte("third")}, terminal: readErr}
	calls := 0
	err := streamDaemonCLIPipe(reader, cliStreamStderr, func(_, _ string) error { calls++; return emitErr })
	assert.ErrorIs(t, err, emitErr)
	assert.Equal(t, 1, calls)
	assert.Empty(t, reader.fragments)
}
