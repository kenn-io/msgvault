package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIProgress_OnLatestDateBeforeOnStart(t *testing.T) {
	p := &CLIProgress{}
	p.OnLatestDate(time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))

	require.False(t, p.startTime.IsZero(), "startTime should be initialized when OnLatestDate is called before OnStart")
	require.LessOrEqual(t, time.Since(p.startTime), time.Second, "startTime should be recent")
}

func TestCLIProgress_OnProgressBeforeOnStart(t *testing.T) {
	p := &CLIProgress{}
	p.OnProgress(10, 5, 3)

	require.False(t, p.startTime.IsZero(), "startTime should be initialized when OnProgress is called before OnStart")
	require.LessOrEqual(t, time.Since(p.startTime), time.Second, "startTime should be recent")
}

func TestCLIProgress_OnStartResetsForReuse(t *testing.T) {
	p := &CLIProgress{}
	p.OnStart(100)
	first := p.startTime

	time.Sleep(5 * time.Millisecond)
	p.OnStart(200)

	require.True(t, p.startTime.After(first), "OnStart should reset startTime on subsequent calls")
}

func TestCLIProgress_PlainModeEmitsNewlineTerminatedUpdates(t *testing.T) {
	assert := assert.New(t)
	var buf bytes.Buffer
	p := &CLIProgress{mode: progressModePlain, out: &buf}
	p.OnStart(0)
	p.lastPrint = time.Now().Add(-time.Minute) // bypass the throttle
	p.OnProgress(1000, 500, 100)

	out := buf.String()
	require.NotEmpty(t, out, "expected a progress update")
	assert.True(strings.HasSuffix(out, "\n"),
		"a plain update is a permanent line and must end it: %q", out)
	assert.NotContains(out, "\r",
		"plain mode goes through pipes where \\r cannot overwrite")
	assert.Contains(out, "Scanned: 1000")
	assert.Contains(out, "Added: 500")

	p.OnComplete(nil)
	assert.Equal(out, buf.String(),
		"plain mode has no open line for OnComplete to terminate")
}

func TestCLIProgress_PlainModeLatestDateDoesNotConsumeThrottle(t *testing.T) {
	var buf bytes.Buffer
	p := &CLIProgress{mode: progressModePlain, out: &buf}
	p.OnStart(0)
	p.lastPrint = time.Now().Add(-time.Minute)

	// Full sync reports the latest date immediately before the counters;
	// the date alone must not print, or it would burn the 30s throttle on
	// a line with stale/zero counters and suppress the accurate one.
	p.OnLatestDate(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	require.Empty(t, buf.String(), "a date update alone must not print")

	p.OnProgress(1000, 500, 100)
	out := buf.String()
	assert.Contains(t, out, "Scanned: 1000", "rendered line must carry current counters")
	assert.Contains(t, out, "Apr 2026", "recorded date renders with the progress line")
}

func TestCLIProgress_PlainModeThrottlesToItsInterval(t *testing.T) {
	var buf bytes.Buffer
	p := &CLIProgress{mode: progressModePlain, out: &buf}
	p.OnStart(0)
	p.OnProgress(100, 50, 10) // first update always prints
	first := buf.String()
	require.NotEmpty(t, first, "the first update must print immediately")

	p.OnProgress(1000, 500, 100)
	assert.Equal(t, first, buf.String(),
		"an update inside the plain interval must not print")
}

func TestCLIProgress_FirstProgressLinePrintsImmediately(t *testing.T) {
	var buf bytes.Buffer
	p := &CLIProgress{mode: progressModePlain, out: &buf}
	p.OnStart(0) // sets lastPrint to now; the throttle alone would go silent for 30s
	p.OnProgress(100, 100, 0)

	assert.Contains(t, buf.String(), "Scanned: 100",
		"the first page of a slow sync must produce output immediately")
}

func TestCLIProgress_ListProgressFinalSummaryAlwaysPrints(t *testing.T) {
	assert := assert.New(t)
	var buf bytes.Buffer
	p := &CLIProgress{mode: progressModePlain, out: &buf}

	// An instant resync: every folder unchanged, no intermediate updates
	// survive the throttle, yet the summary must still appear.
	p.OnIMAPListProgress(0, 4, "", 0, 0)
	p.OnIMAPListProgress(4, 4, "INBOX", 0, 4)

	out := buf.String()
	assert.Contains(out, "Checking 4 folders...")
	assert.Contains(out, "Checked 4 folders: 0 messages to examine, 4 unchanged (skipped)")
	assert.True(p.printedAnything())
}

func TestCLIProgress_ListProgressIntermediateThrottled(t *testing.T) {
	assert := assert.New(t)
	var buf bytes.Buffer
	p := &CLIProgress{mode: progressModePlain, out: &buf}

	p.OnIMAPListProgress(0, 10, "", 0, 0)
	first := buf.String()
	p.OnIMAPListProgress(1, 10, "INBOX", 5, 0)
	assert.Equal(first, buf.String(),
		"intermediate updates inside the interval must not print")

	p.lastListPrint = time.Now().Add(-time.Minute)
	p.OnIMAPListProgress(2, 10, "Archive", 9, 0)
	assert.Contains(buf.String(), "Checking folders: 2/10")
}

func TestCLIProgress_TTYModeRedrawsInPlace(t *testing.T) {
	var buf bytes.Buffer
	p := &CLIProgress{mode: progressModeTTY, out: &buf}
	p.OnStart(0)
	p.lastPrint = time.Now().Add(-time.Minute)
	p.OnProgress(1000, 500, 100)

	out := buf.String()
	assert.True(t, strings.HasPrefix(out, "\r"),
		"a tty update redraws the status line in place: %q", out)
	assert.False(t, strings.HasSuffix(out, "\n"),
		"a tty update keeps the line open for the next redraw")

	p.OnComplete(nil)
	assert.True(t, strings.HasSuffix(buf.String(), "\n"),
		"OnComplete must terminate the open progress line")
}
