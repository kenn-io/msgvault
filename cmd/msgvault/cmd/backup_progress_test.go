package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/backup"
)

// withZeroBackupProgressTick forces every backupProgressRenderer redraw to
// emit by zeroing the throttle interval for the duration of one test,
// restoring it afterward so other tests keep the real throttling behavior.
// newTestBackupProgressRenderer builds a renderer whose idle-elapsed ticker
// is stopped at test cleanup, so no goroutine outlives the test.
func newTestBackupProgressRenderer(t *testing.T, out io.Writer, mode progressOutputMode) *backupProgressRenderer {
	t.Helper()
	r := newBackupProgressRenderer(out, mode)
	t.Cleanup(r.finish)
	return r
}

func withZeroBackupProgressTick(t *testing.T) {
	t.Helper()
	old := backupProgressTickInterval
	backupProgressTickInterval = 0
	t.Cleanup(func() { backupProgressTickInterval = old })
}

func TestBackupProgressRenderer_PlainModeEmitsNewlineTerminatedLines(t *testing.T) {
	assert := assert.New(t)
	withZeroBackupProgressTick(t)

	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModePlain)

	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageFreeze, Total: 1})
	r.handle(backup.ProgressEvent{
		Stage: backup.ProgressStageFreeze, Done: 1, Total: 1, BytesDone: 2048, BytesTotal: 2048, Final: true,
	})
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 5, Total: 10})

	out := buf.String()
	t.Logf("plain mode output:\n%s", out)

	assert.NotContains(out, "\r", "plain mode goes through pipes where \\r cannot overwrite")
	assert.Equal(3, strings.Count(out, "\n"), "each plain update is its own permanent, newline-terminated line")
	assert.True(strings.HasSuffix(out, "\n"), "plain output ends each update with a newline")
	assert.Contains(out, "freeze")
	assert.Contains(out, "scan")
	assert.Contains(out, "1/1")
	assert.Contains(out, "5/10")
	assert.Contains(out, "(done)", "the Final freeze event marks itself done")
}

func TestBackupProgressRenderer_TTYModeRedrawsInPlaceAndTerminatesOnFinal(t *testing.T) {
	assert := assert.New(t)
	withZeroBackupProgressTick(t)

	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeTTY)

	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 3, Total: 10})
	out := buf.String()
	assert.True(strings.HasPrefix(out, "\r"), "a tty update redraws the status line in place: %q", out)
	assert.False(strings.HasSuffix(out, "\n"), "a tty update keeps the line open for the next redraw")
	assert.Contains(out, "█", "the bar draws with the unicode filled glyph")
	assert.Contains(out, "░", "the bar draws with the unicode empty glyph")

	buf.Reset()
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 10, Total: 10, Final: true})
	out = buf.String()
	assert.True(strings.HasPrefix(out, "\r"))
	assert.True(strings.HasSuffix(out, "\n"), "a Final event terminates the in-place line")
}

func TestBackupProgressRenderer_TTYPadsShorterRedraws(t *testing.T) {
	assert := assert.New(t)
	withZeroBackupProgressTick(t)

	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeTTY)

	// A long update (byte count plus throughput) followed by a shorter final
	// line: without padding, the old tail would survive the \r redraw.
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 3, Total: 10})
	r.stageStart = time.Now().Add(-10 * time.Second) // age the stage so a rate renders
	buf.Reset()
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 5, Total: 10, BytesDone: 123456789})
	long := strings.TrimPrefix(buf.String(), "\r")

	buf.Reset()
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 10, Total: 10, Final: true})
	final := strings.TrimSuffix(strings.TrimPrefix(buf.String(), "\r"), "\n")
	assert.GreaterOrEqual(len([]rune(final)), len([]rune(long)),
		"a shorter redraw must be padded to cover the previous line's tail")
	assert.True(strings.HasSuffix(final, " "), "the cover is trailing spaces")
}

// TestBackupProgressRenderer_ElapsedSuffix pins the per-stage elapsed time:
// lines for a stage older than a second carry its wall-clock age, and the
// final line records the stage's total duration.
func TestBackupProgressRenderer_ElapsedSuffix(t *testing.T) {
	assert := assert.New(t)
	withZeroBackupProgressTick(t)

	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeTTY)

	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 1, Total: 10})
	assert.NotContains(buf.String(), "1m30s", "a fresh stage shows no elapsed time yet")

	r.mu.Lock()
	r.stageStart = time.Now().Add(-90 * time.Second)
	r.mu.Unlock()
	buf.Reset()
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 10, Total: 10, Final: true})
	assert.Contains(buf.String(), " 1m30s", "the final line records the stage's total duration")
}

// TestBackupProgressRenderer_IdleTickerKeepsElapsedCounting pins the fix for
// silent long stages (integrity_check during restore's proof): with no new
// events arriving, the open TTY line must keep redrawing with fresh elapsed
// time instead of sitting frozen, and finish() must stop the ticker.
func TestBackupProgressRenderer_IdleTickerKeepsElapsedCounting(t *testing.T) {
	require := require.New(t)
	withZeroBackupProgressTick(t)
	oldTick := backupProgressElapsedTick
	backupProgressElapsedTick = 5 * time.Millisecond
	t.Cleanup(func() { backupProgressElapsedTick = oldTick })

	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeTTY)
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageProof, Done: 0, Total: 2})
	r.mu.Lock()
	r.stageStart = time.Now().Add(-10 * time.Second)
	buf.Reset()
	r.mu.Unlock()

	require.Eventually(func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return strings.Contains(buf.String(), "10s")
	}, time.Second, 5*time.Millisecond, "the idle ticker must redraw with elapsed time")

	r.finish()
	r.mu.Lock()
	buf.Reset()
	r.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Empty(buf.String(), "finish must stop the idle ticker")
}

func TestBackupProgressRenderer_FinishClosesOpenLine(t *testing.T) {
	assert := assert.New(t)
	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeTTY)

	// Nothing open yet: finish is a no-op.
	r.finish()
	assert.Empty(buf.String())

	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 3, Total: 10})
	buf.Reset()
	r.finish()
	assert.Equal("\n", buf.String(), "finish must terminate the open in-place line")

	buf.Reset()
	r.finish()
	assert.Empty(buf.String(), "finish is idempotent")
}

func TestBackupProgressRenderer_ThrottleSuppressesRapidNonFinalUpdates(t *testing.T) {
	require := require.New(t)
	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeTTY)

	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 1, Total: 100})
	require.NotEmpty(buf.String(), "the first event for a stage always renders")

	buf.Reset()
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 2, Total: 100})
	require.Empty(buf.String(), "a second non-final update inside the throttle window must not render")
}

func TestBackupProgressRenderer_FinalEventAlwaysRendersDespiteThrottle(t *testing.T) {
	require := require.New(t)
	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeTTY)

	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 1, Total: 100})
	buf.Reset()
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 100, Total: 100, Final: true})
	require.NotEmpty(buf.String(), "a Final event must render even inside the throttle window")
}

func TestBackupProgressRenderer_StageSwitchBypassesThrottle(t *testing.T) {
	require := require.New(t)
	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeTTY)

	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageFreeze, Done: 1, Total: 1})
	buf.Reset()
	// A different stage's first event must render immediately, even though
	// it arrives well inside the previous stage's throttle window.
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 1, Total: 100})
	require.NotEmpty(buf.String(), "a new stage's first event must not be throttled by the prior stage")
}

func TestBackupProgressRenderer_StageSwitchWithoutFinalTerminatesOpenLine(t *testing.T) {
	assert := assert.New(t)
	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeTTY)

	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageFreeze, Done: 0, Total: 1})
	assert.True(r.stageOpen)

	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageScan, Done: 1, Total: 100})
	out := buf.String()
	// The defensive newline that closes the freeze stage's stray open line,
	// followed by the scan stage's own in-place redraw.
	assert.Contains(out, "\n\r", "switching stages without a Final terminates the prior open line first")
}

func TestBackupProgressModeFromFlag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, tc := range []struct {
		value string
		want  progressOutputMode
	}{
		{"", progressModeAuto},
		{"auto", progressModeAuto},
		{"bar", progressModeTTY},
		{"plain", progressModePlain},
	} {
		got, err := backupProgressModeFromFlag(tc.value)
		require.NoError(err, tc.value)
		assert.Equal(tc.want, got, tc.value)
	}

	_, err := backupProgressModeFromFlag("garbage")
	assert.Error(err)
}

func TestResolveClientBackupProgressFlag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Run("explicit bar and plain pass through unchanged", func(t *testing.T) {
		for _, value := range []string{"bar", "plain"} {
			cmd := &cobra.Command{Use: "test"}
			cmd.Flags().String("progress", "auto", "")
			require.NoError(cmd.Flags().Set("progress", value))

			require.NoError(resolveClientBackupProgressFlag(cmd))
			assert.Equal(value, cmd.Flags().Lookup("progress").Value.String())
		}
	})

	t.Run("auto resolves to a concrete bar or plain value", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		cmd.Flags().String("progress", "auto", "")

		require.NoError(resolveClientBackupProgressFlag(cmd))
		resolved := cmd.Flags().Lookup("progress").Value.String()
		assert.Contains([]string{"bar", "plain"}, resolved)
	})

	t.Run("an invalid value is an error", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		cmd.Flags().String("progress", "auto", "")
		require.NoError(cmd.Flags().Set("progress", "garbage"))

		require.Error(resolveClientBackupProgressFlag(cmd))
	})

	t.Run("a command with no progress flag is a no-op", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		require.NoError(resolveClientBackupProgressFlag(cmd))
	})
}

func TestBackupProgressRenderer_AutoModeDetectsFromNonFileWriter(t *testing.T) {
	require := require.New(t)
	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeAuto)

	require.Equal(progressModePlain, r.outputMode(), "a bytes.Buffer is never a terminal")
}

func TestBackupProgressRenderer_DefaultWriterIsStdout(t *testing.T) {
	require := require.New(t)
	r := newBackupProgressRenderer(nil, progressModeAuto)
	require.NotNil(r.writer())
}

// sanity check that the package-level throttle var is a time.Duration, so a
// future refactor can't silently change its type without breaking this file.
var _ time.Duration = backupProgressTickInterval

// TestBackupProgressRenderer_IdleTickerRepaintsThrottledEvent pins that an
// event suppressed by the render throttle still updates the counts the idle
// ticker repaints: a quick proof 1/2 arriving inside the throttle window
// must not leave the line stuck on 0/2 through the long stats comparison.
func TestBackupProgressRenderer_IdleTickerRepaintsThrottledEvent(t *testing.T) {
	require := require.New(t)
	oldInterval := backupProgressTickInterval
	backupProgressTickInterval = 100 * time.Millisecond
	t.Cleanup(func() { backupProgressTickInterval = oldInterval })
	oldTick := backupProgressElapsedTick
	backupProgressElapsedTick = 5 * time.Millisecond
	t.Cleanup(func() { backupProgressElapsedTick = oldTick })

	var buf bytes.Buffer
	r := newTestBackupProgressRenderer(t, &buf, progressModeTTY)
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageProof, Done: 0, Total: 2})
	r.mu.Lock()
	r.lastRender = time.Now() // pin the throttle window open around the next event
	r.mu.Unlock()
	r.handle(backup.ProgressEvent{Stage: backup.ProgressStageProof, Done: 1, Total: 2})
	r.mu.Lock()
	require.NotContains(buf.String(), "1/2", "second event should have been throttled")
	r.mu.Unlock()

	require.Eventually(func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return strings.Contains(buf.String(), "1/2")
	}, 2*time.Second, 5*time.Millisecond,
		"idle ticker must repaint the throttled event's counts")
}
