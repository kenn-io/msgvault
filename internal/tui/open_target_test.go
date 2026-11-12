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

	started := time.Now()
	err := openSystemTarget(t.Context(), "document.pdf")

	require.NoError(t, err)
	assert.Less(t, time.Since(started), time.Second, "launcher handoff must not wait for the opened application")
}

func TestOpenSystemTargetLauncherSurvivesCallerCancellation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("test supplies a fake xdg-open executable")
	}

	binDir := t.TempDir()
	launcher := filepath.Join(binDir, "xdg-open")
	require.NoError(t, os.WriteFile(launcher, []byte("#!/bin/sh\nsleep 0.2\n: > \"$1\"\n"), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	opened := filepath.Join(t.TempDir(), "opened")

	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, openSystemTarget(ctx, opened))
	cancel()

	assert.Eventually(t, func() bool {
		_, err := os.Stat(opened)
		return err == nil
	}, 10*time.Second, 20*time.Millisecond, "the TUI cancels its context as soon as the launcher starts; the launcher must still open the target")
}

func TestOpenSystemTargetRejectsCanceledContextBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	assert.ErrorIs(t, openSystemTarget(ctx, "document.pdf"), context.Canceled)
}
