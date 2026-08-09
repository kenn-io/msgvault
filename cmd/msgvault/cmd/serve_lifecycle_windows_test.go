//go:build windows

package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestWindowsBackgroundProcessDoesNotRunBeforeJobAttachment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	cmd := helperProcessCommand(context.Background(), "spawn-blocking-child")
	cmd.Env = append(cmd.Env, "GO_HELPER_CHILD_PID_PATH="+pidPath)
	commandConfig, err := configureServeBackgroundCommand(cmd)
	require.NoError(err, "configure process tree")
	tree := commandConfig.ProcessTree
	require.NotNil(tree, "Windows background daemon must own a process tree")
	require.NoError(cmd.Start(), "start suspended parent helper")
	t.Cleanup(func() {
		_ = tree.Terminate()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = tree.Close()
	})

	assert.Never(func() bool {
		_, statErr := os.Stat(pidPath)
		return statErr == nil || !errors.Is(statErr, os.ErrNotExist)
	}, 500*time.Millisecond, 10*time.Millisecond,
		"daemon work must not begin before Job Object attachment")

	require.NoError(tree.Attach(cmd.Process), "attach and resume parent helper")
	require.Eventually(func() bool {
		_, statErr := os.Stat(pidPath)
		return statErr == nil
	}, 10*time.Second, 25*time.Millisecond, "blocking child PID")
}

func TestStopBackgroundServeStartupTerminatesWindowsProcessTree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	cmd := helperProcessCommand(context.Background(), "spawn-blocking-child")
	cmd.Env = append(cmd.Env, "GO_HELPER_CHILD_PID_PATH="+pidPath)
	commandConfig, err := configureServeBackgroundCommand(cmd)
	require.NoError(err, "configure process tree")
	tree := commandConfig.ProcessTree
	require.NotNil(tree, "Windows background daemon must own a process tree")
	require.NoError(cmd.Start(), "start parent helper")
	t.Cleanup(func() {
		_ = tree.Terminate()
		_ = tree.Close()
		_ = cmd.Process.Kill()
	})
	require.NoError(tree.Attach(cmd.Process), "attach parent helper to job")

	var childPID int
	require.Eventually(func() bool {
		contents, readErr := os.ReadFile(pidPath)
		if readErr != nil {
			return false
		}
		childPID, readErr = strconv.Atoi(strings.TrimSpace(string(contents)))
		return readErr == nil && childPID > 0
	}, 10*time.Second, 25*time.Millisecond, "blocking child PID")
	child, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(childPID))
	require.NoError(err, "open blocking child")
	t.Cleanup(func() { _ = windows.CloseHandle(child) })

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	proc := &backgroundServeProcess{
		PID:         cmd.Process.Pid,
		Process:     cmd.Process,
		ProcessTree: tree,
		Wait:        waitCh,
	}

	require.NoError(stopBackgroundServeStartup(proc, 5*time.Second))
	waitResult, err := windows.WaitForSingleObject(child, 5_000)
	require.NoError(err, "wait for blocking child")
	assert.Equal(uint32(windows.WAIT_OBJECT_0), waitResult,
		"terminating daemon startup must terminate its blocking child")
}

func TestReleaseWindowsProcessTreeLeavesDetachedDaemonRunning(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cmd := helperProcessCommand(context.Background(), "block")
	commandConfig, err := configureServeBackgroundCommand(cmd)
	require.NoError(err, "configure process tree")
	tree := commandConfig.ProcessTree
	require.NotNil(tree, "Windows background daemon must own a process tree")
	require.NoError(cmd.Start(), "start daemon helper")
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = tree.Close()
	})
	require.NoError(tree.Attach(cmd.Process), "attach daemon helper to job")
	require.NoError(tree.Close(), "release launcher Job handle")

	var waitResult uint32
	var waitErr error
	require.NoError(cmd.Process.WithHandle(func(handle uintptr) {
		waitResult, waitErr = windows.WaitForSingleObject(windows.Handle(handle), 250)
	}), "access daemon helper handle")
	require.NoError(waitErr, "probe daemon helper")
	assert.Equal(uint32(windows.WAIT_TIMEOUT), waitResult,
		"the daemon's inherited Job handle must keep it running after launcher release")
}
