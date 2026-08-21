package tui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenSystemTargetReturnsAfterLauncherStarts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("test supplies a fake xdg-open executable")
	}

	binDir := t.TempDir()
	launcher := filepath.Join(binDir, "xdg-open")
	require.NoError(t, os.WriteFile(launcher, []byte("#!/bin/sh\nsleep 2\n"), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := openSystemTarget(ctx, "document.pdf")

	require.NoError(t, err)
	assert.Less(t, time.Since(started), time.Second, "launcher handoff must not wait for the opened application")
}

func TestOpenSystemTargetRejectsCanceledContextBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.ErrorIs(t, openSystemTarget(ctx, "document.pdf"), context.Canceled)
}
