package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
)

func TestFormatSearchStatus(t *testing.T) {
	tests := []struct {
		name    string
		elapsed time.Duration
		op      *api.OperationHealth
		want    string
	}{
		{
			name:    "elapsed only when daemon is idle",
			elapsed: 12 * time.Second,
			op:      nil,
			want:    "Searching... (12s)",
		},
		{
			name:    "daemon operation label wins",
			elapsed: 45 * time.Second,
			op:      &api.OperationHealth{Busy: true, Label: "checking the search index"},
			want:    "Searching... daemon is busy: checking the search index (45s)",
		},
		{
			name:    "busy without label degrades to elapsed only",
			elapsed: 5 * time.Second,
			op:      &api.OperationHealth{Busy: true},
			want:    "Searching... (5s)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatSearchStatus("Searching...", tt.elapsed, tt.op))
		})
	}
}

func TestSearchStatusLineRenderTTYRewritesInPlace(t *testing.T) {
	assert := assert.New(t)
	var buf bytes.Buffer
	l := &searchStatusLine{out: &buf, prefix: "Searching...", tty: true}

	l.render(10*time.Second, &api.OperationHealth{
		Busy:  true,
		Label: "building the search index (100/400 messages)",
	})
	long := buf.String()
	assert.True(strings.HasPrefix(long, "\r"), "render must return to line start")
	assert.Contains(long, "building the search index (100/400 messages)")

	buf.Reset()
	l.render(12*time.Second, nil)
	short := buf.String()
	assert.Contains(short, "Searching... (12s)")
	assert.Greater(len(short), len("\rSearching... (12s)"),
		"a shorter line must be padded to erase the previous one")

	buf.Reset()
	l.clear()
	cleared := buf.String()
	assert.True(strings.HasPrefix(cleared, "\r") && strings.HasSuffix(cleared, "\r"),
		"clear must blank the line and return to line start")
	assert.Empty(strings.Trim(cleared, "\r "), "clear must write only spaces")
}

func TestSearchStatusLineRenderNonTTYPrintsNoticeOnce(t *testing.T) {
	assert := assert.New(t)
	var buf bytes.Buffer
	l := &searchStatusLine{out: &buf, prefix: "Searching...", tty: false}

	l.render(5*time.Second, nil)
	assert.Empty(buf.String(), "no output while the daemon reports nothing")

	op := &api.OperationHealth{Busy: true, Label: "checking the search index"}
	l.render(7*time.Second, op)
	l.render(9*time.Second, op)
	notice := "Daemon is busy: checking the search index. The search will finish when it does."
	assert.Equal(1, strings.Count(buf.String(), notice),
		"the busy notice must be printed exactly once")
}

func TestStartSearchStatusLoopShowsDaemonActivity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	origQuiet, origTick := searchStatusQuietWindow, searchStatusTick
	searchStatusQuietWindow = 0
	searchStatusTick = 5 * time.Millisecond
	t.Cleanup(func() {
		searchStatusQuietWindow, searchStatusTick = origQuiet, origTick
	})

	var buf bytes.Buffer
	fetched := make(chan struct{}, 1)
	l := &searchStatusLine{
		out:    &buf,
		prefix: "Searching...",
		tty:    true,
		start:  time.Now(),
		fetchOp: func(context.Context) *api.OperationHealth {
			select {
			case fetched <- struct{}{}:
			default:
			}
			return &api.OperationHealth{Busy: true, Label: "checking the search index"}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.run(ctx)
	}()

	// The render for a tick runs unconditionally after its fetch returns, so
	// once a fetch is observed the corresponding render cannot be skipped by
	// the cancel below.
	select {
	case <-fetched:
	case <-time.After(time.Second):
		require.FailNow("run loop never polled the daemon")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow("run loop must stop on cancel")
	}

	assert.Contains(buf.String(), "daemon is busy: checking the search index",
		"the loop must render the daemon's reported operation")
}
