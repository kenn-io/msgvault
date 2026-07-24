//go:build windows

package tasklinks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/taskclient"
)

func TestWindowsDiskCacheFailsClosedWithoutConsumingOrReplacingExistingFile(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "reverse-index.json")
	original := []byte(`{"must_not_be_consumed":true}`)
	requirements.NoError(os.WriteFile(path, original, 0o600))
	idx := NewIndex(path, time.Now)
	identity := MessageIdentity{ArchiveUID: "archive-a", MessageID: 42}

	requirements.ErrorIs(idx.Load(), ErrDiskCacheSecurityUnsupported)
	assertions.Empty(idx.Lookup(testCacheIdentity(), identity, true).Tasks)

	status, err := idx.Rebuild(context.Background(), &listClient{pages: []taskclient.TaskList{{
		Tasks: []taskclient.Task{indexedTask("task-1", "Current-session task", identity)},
	}}}, testCacheIdentity(), "remote-1", 100)
	requirements.NoError(err)
	assertions.Equal(StateReady, status.State)
	assertions.Equal(ReasonCachePersistenceUnsupported, status.Reason)
	assertions.True(status.Complete)
	currentSession := idx.Lookup(testCacheIdentity(), identity, true)
	requirements.Len(currentSession.Tasks, 1)
	assertions.Equal("Current-session task", currentSession.Tasks[0].Title)

	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal(original, got)
}
