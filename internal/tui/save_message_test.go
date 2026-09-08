package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestSaveMessageKeyWritesRawEmailWithoutOverwriting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Chdir(t.TempDir())
	raw := []byte("From: sender@example.com\r\nTo: user@example.com\r\nSubject: Test\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=test\r\n\r\n--test\r\nContent-Type: text/plain\r\n\r\nFull message\r\n--test\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nAAEC\r\n--test--\r\n")
	detail := &query.MessageDetail{ID: 42, MessageType: "email", Subject: "../../unsafe\x1b[31m"}
	m := NewBuilder().WithLevel(levelMessageDetail).WithDetail(detail).Build()
	m.actions = NewActionController(&querytest.MockEngine{RawMessages: map[int64][]byte{42: raw}}, t.TempDir(), nil)
	require.NoError(os.WriteFile("message-42.eml", []byte("keep me"), 0o600))

	updated, cmd := m.Update(key('s'))
	require.NotNil(cmd, "s must start a save from message detail")
	result, ok := cmd().(saveMessageResultMsg)
	require.True(ok)
	require.NoError(result.Err)
	data, err := os.ReadFile("message-42_1.eml")
	require.NoError(err)
	assert.Equal(raw, data, "preserve the complete MIME message including attachments")
	original, err := os.ReadFile("message-42.eml")
	require.NoError(err)
	assert.Equal("keep me", string(original))
	info, err := os.Stat("message-42_1.eml")
	require.NoError(err)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
	finished, _ := updated.Update(result)
	saved := asModel(t, finished)
	assert.False(saved.loading)
	assert.Equal(modalExportResult, saved.modal)
	assert.Contains(saved.modalResult, "message-42_1.eml")
}

func TestSaveMessageKeyDoesNotWriteMissingOrNonEmailRaw(t *testing.T) {
	for _, tc := range []struct {
		name        string
		messageType string
		raw         []byte
	}{
		{name: "no raw message"},
		{name: "non-email payload", messageType: "slack", raw: []byte(`{"text":"Test message"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			t.Chdir(t.TempDir())
			m := NewBuilder().WithLevel(levelMessageDetail).WithDetail(&query.MessageDetail{ID: 42, MessageType: tc.messageType}).Build()
			m.actions = NewActionController(&querytest.MockEngine{RawMessages: map[int64][]byte{42: tc.raw}}, "", nil)
			_, cmd := m.Update(key('s'))
			require.NotNil(cmd)
			result, ok := cmd().(saveMessageResultMsg)
			require.True(ok)
			require.Error(result.Err)
			entries, err := os.ReadDir(".")
			require.NoError(err)
			assert.Empty(entries)
		})
	}
}

func TestSaveMessageKeyReportsReadFailureWithoutCreatingFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Chdir(t.TempDir())
	m := NewBuilder().WithLevel(levelMessageDetail).WithDetail(&query.MessageDetail{ID: 42}).Build()
	m.actions = NewActionController(&querytest.MockEngine{
		GetMessageRawFunc: func(context.Context, int64) ([]byte, error) {
			return nil, errors.New("archive unavailable")
		},
	}, "", nil)
	updated, cmd := m.Update(key('s'))
	require.NotNil(cmd)
	result, ok := cmd().(saveMessageResultMsg)
	require.True(ok)
	require.ErrorContains(result.Err, "archive unavailable")
	finished, _ := updated.Update(result)
	assert.Contains(asModel(t, finished).modalResult, "archive unavailable")
	entries, err := os.ReadDir(".")
	require.NoError(err)
	assert.Empty(entries)
}

func TestSaveMessageKeyWithoutLoadedDetail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Chdir(t.TempDir())
	m := NewBuilder().WithLevel(levelMessageDetail).WithLoading(false).Build()
	updated, _ := m.Update(key('s'))
	assert.False(asModel(t, updated).loading)
	entries, err := os.ReadDir(".")
	require.NoError(err)
	assert.Empty(entries)
}

func TestSaveMessageKeyDoesNotFollowSymlink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Chdir(t.TempDir())
	target := filepath.Join(t.TempDir(), "original.eml")
	require.NoError(os.WriteFile(target, []byte("keep me"), 0o600))
	if err := os.Symlink(target, "message-42.eml"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	m := NewBuilder().WithLevel(levelMessageDetail).WithDetail(&query.MessageDetail{ID: 42}).Build()
	m.actions = NewActionController(&querytest.MockEngine{RawMessages: map[int64][]byte{42: []byte("Subject: Test\r\n\r\nFull message")}}, "", nil)
	_, cmd := m.Update(key('s'))
	require.NotNil(cmd)
	result, ok := cmd().(saveMessageResultMsg)
	require.True(ok)
	require.NoError(result.Err)
	data, err := os.ReadFile(target)
	require.NoError(err)
	assert.Equal("keep me", string(data))
	assert.FileExists("message-42_1.eml")
}

func TestSaveMessageKeyReportsWriteFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "output")
	require.NoError(os.Mkdir(dir, 0o700))
	t.Chdir(dir)
	m := NewBuilder().WithLevel(levelMessageDetail).WithDetail(&query.MessageDetail{ID: 42}).Build()
	m.actions = NewActionController(&querytest.MockEngine{RawMessages: map[int64][]byte{42: []byte("Subject: Test\r\n\r\nFull message")}}, "", nil)
	_, cmd := m.Update(key('s'))
	require.NotNil(cmd)
	// Move the destination after queuing the save, before its I/O runs. Leave
	// with os.Chdir: t.Chdir would hold dir open, which blocks the rename on
	// Windows. The first t.Chdir already restores the original directory.
	require.NoError(os.Chdir(parent)) //nolint:usetesting // t.Chdir pins dir open
	moved := dir + "-moved"
	require.NoError(os.Rename(dir, moved))
	t.Cleanup(func() { require.NoError(os.Rename(moved, dir)) })
	result, ok := cmd().(saveMessageResultMsg)
	require.True(ok)
	require.Error(result.Err)
	assert.NoFileExists(filepath.Join(parent, "message-42.eml"), "must not change the destination at execution time")
}

func TestSaveMessageCompletionPreservesPendingDetailLoad(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Chdir(t.TempDir())
	m := NewBuilder().WithLevel(levelMessageDetail).
		WithMessages(query.MessageSummary{ID: 42}, query.MessageSummary{ID: 43}).
		WithDetail(&query.MessageDetail{ID: 42}).Build()
	m.actions = NewActionController(&querytest.MockEngine{RawMessages: map[int64][]byte{42: []byte("Subject: Test\r\n\r\nFull message")}}, "", nil)
	saving, saveCmd := sendKey(t, m, key('s'))
	require.NotNil(saveCmd)
	navigated, loadCmd := sendKey(t, saving, key('l'))
	require.NotNil(loadCmd)
	require.True(navigated.loading)
	finished := sendMsg(t, navigated, saveCmd())
	assert.True(finished.loading, "saving A must not finish B's pending load")
	dismissed, _ := sendKey(t, finished, key(' '))
	_, secondSave := sendKey(t, dismissed, key('s'))
	assert.Nil(secondSave, "cannot save stale detail while the next message loads")
}
