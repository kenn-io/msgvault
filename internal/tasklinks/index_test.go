package tasklinks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/taskclient"
)

type listClient struct {
	pages   []taskclient.TaskList
	errAt   int
	calls   int
	limits  []int
	cursors []string
}

func (c *listClient) ListTasks(_ context.Context, _ string, limit int, cursor string) (taskclient.TaskList, error) {
	c.calls++
	c.limits = append(c.limits, limit)
	c.cursors = append(c.cursors, cursor)
	if c.errAt == c.calls {
		return taskclient.TaskList{}, errors.New("unavailable")
	}
	return c.pages[c.calls-1], nil
}

func indexedTask(id, title string, identity MessageIdentity) taskclient.Task {
	return taskclient.Task{ID: id, Project: "project", Title: title, Revision: "r1", Metadata: MetadataWithLink(nil, NewMailLink(identity, time.Now()))}
}

func testCacheIdentity() CacheIdentity {
	return CacheIdentity{Project: "project", ArchiveUID: "archive-a", ArchiveRevision: "archive-rev-1"}
}

func TestReverseIndexBuildPaginationPersistenceAndArchiveChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "tasks", "reverse-index.json")
	now := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	idx := NewIndex(path, func() time.Time { return now })
	identity := testIdentity()
	otherIdentity := identity
	otherIdentity.SourceMessageID = "other"
	otherIdentity.MessageID = 43
	client := &listClient{pages: []taskclient.TaskList{
		{Tasks: []taskclient.Task{indexedTask("one", "First", identity)}, NextCursor: "next"},
		{Tasks: []taskclient.Task{indexedTask("two", "Second", otherIdentity)}},
	}}
	cacheIdentity := testCacheIdentity()
	status, err := idx.Rebuild(context.Background(), client, cacheIdentity, "remote-1", 100)
	require.NoError(err)
	assert.True(status.Complete)
	assert.Equal(2, client.calls)
	lookup := idx.Lookup(cacheIdentity, identity, true)
	require.Len(lookup.Tasks, 1)
	assert.Equal("First", lookup.Tasks[0].Title)
	assert.Equal(now, lookup.LastScan)
	assert.Equal("remote-1", lookup.RemoteRevision)

	reloaded := NewIndex(path, func() time.Time { return now.Add(time.Hour) })
	require.NoError(reloaded.Load())
	assert.Len(reloaded.Lookup(cacheIdentity, identity, true).Tasks, 1)

	changed := &listClient{pages: []taskclient.TaskList{{}}}
	changedIdentity := cacheIdentity
	changedIdentity.ArchiveUID = "archive-b"
	_, err = reloaded.Rebuild(context.Background(), changed, changedIdentity, "remote-2", 100)
	require.NoError(err)
	assert.Empty(reloaded.Lookup(changedIdentity, identity, true).Tasks, "archive identity change discards old entries")
}

func TestReverseIndexReportsPartialStaleUnavailableAndHidesTitlesWithoutAuth(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	idx := NewIndex(filepath.Join(t.TempDir(), "index.json"), func() time.Time { return now })
	identity := testIdentity()
	partial := &listClient{pages: []taskclient.TaskList{{Tasks: []taskclient.Task{indexedTask("one", "Secret title", identity)}, NextCursor: "more"}}}
	cacheIdentity := testCacheIdentity()
	status, err := idx.Rebuild(context.Background(), partial, cacheIdentity, "remote-1", 1)
	require.NoError(t, err)
	assert.False(status.Complete)
	assert.Equal(ReasonSafetyLimit, status.Reason)

	unauth := idx.Lookup(cacheIdentity, identity, false)
	assert.Equal(StateAuthenticationRequired, unauth.State)
	assert.Empty(unauth.Tasks)

	now = now.Add(2 * DefaultStaleAfter)
	stale := idx.Lookup(cacheIdentity, identity, true)
	assert.Equal(StateStale, stale.State)
	assert.NotEmpty(stale.Tasks)

	unavailable := &listClient{pages: []taskclient.TaskList{{}}, errAt: 1}
	status, err = idx.Rebuild(context.Background(), unavailable, cacheIdentity, "remote-2", 100)
	require.Error(t, err)
	assert.Equal(ReasonUnavailable, status.Reason)
	failed := idx.Lookup(cacheIdentity, identity, true)
	assert.NotEmpty(failed.Tasks, "last successful result is retained")
	assert.Equal(StateUnavailable, failed.State)
}

func TestReverseIndexRetainsValidLinksButMarksMixedMetadataPartial(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	idx := NewIndex(filepath.Join(t.TempDir(), "index.json"), time.Now)
	identity := testIdentity()
	metadata := MetadataWithLink(nil, NewMailLink(identity, time.Now()))
	raw, ok := metadata[MailLinksKey].([]any)
	require.True(ok)
	metadata[MailLinksKey] = append(raw, "incompatible-extension")
	task := taskclient.Task{ID: "mixed", Project: "project", Title: "Valid target", Revision: "r1", Metadata: metadata}

	status, err := idx.Rebuild(context.Background(), &listClient{pages: []taskclient.TaskList{{Tasks: []taskclient.Task{task}}}}, testCacheIdentity(), "remote-1", 100)
	require.NoError(err)
	assert.Equal(StatePartial, status.State)
	assert.Equal("incompatible_metadata", status.Reason)
	assert.False(status.Complete)
	lookup := idx.Lookup(testCacheIdentity(), identity, true)
	require.Len(lookup.Tasks, 1)
	assert.Equal("Valid target", lookup.Tasks[0].Title)
}

func TestReverseIndexCancellationRetainsLastSuccessfulSnapshot(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	idx := NewIndex(filepath.Join(t.TempDir(), "index.json"), func() time.Time { return now })
	identity := testIdentity()
	cacheIdentity := testCacheIdentity()
	_, err := idx.Rebuild(context.Background(), &listClient{pages: []taskclient.TaskList{{Tasks: []taskclient.Task{indexedTask("one", "Last good", identity)}}}}, cacheIdentity, "remote-1", 100)
	require.NoError(t, err)
	lastGood := idx.Lookup(cacheIdentity, identity, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now = now.Add(time.Minute)
	status, err := idx.Rebuild(ctx, &listClient{}, cacheIdentity, "remote-2", 100)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(StateStale, status.State)
	assert.Equal(ReasonInterrupted, status.Reason)
	assert.False(status.Complete)
	retained := idx.Lookup(cacheIdentity, identity, true)
	assert.Equal(lastGood.Tasks, retained.Tasks)
	assert.Equal(lastGood.LastScan, retained.LastScan)
	assert.Equal(lastGood.RemoteRevision, retained.RemoteRevision)
}

func TestReverseIndexSaveFailureRetainsLastGoodAndMarksMemoryStale(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "index.json")
	idx := NewIndex(path, func() time.Time { return now })
	identity := testIdentity()
	cacheIdentity := testCacheIdentity()
	_, err := idx.Rebuild(context.Background(), &listClient{pages: []taskclient.TaskList{{Tasks: []taskclient.Task{indexedTask("one", "Last good", identity)}}}}, cacheIdentity, "remote-1", 100)
	require.NoError(err)
	lastGood := idx.Lookup(cacheIdentity, identity, true)

	require.NoError(os.Remove(path))
	require.NoError(os.Mkdir(path, 0o700), "a directory at the target forces the production atomic rename to fail")
	now = now.Add(time.Minute)
	status, err := idx.Rebuild(context.Background(), &listClient{pages: []taskclient.TaskList{{Tasks: []taskclient.Task{indexedTask("two", "Must not replace", identity)}}}}, cacheIdentity, "remote-2", 100)
	require.Error(err)
	assert.Equal(StateStale, status.State)
	assert.Equal("persistence_failure", status.Reason)
	assert.False(status.Complete)

	retained := idx.Lookup(cacheIdentity, identity, true)
	assert.Equal(StateStale, retained.State)
	assert.Equal("persistence_failure", retained.Reason)
	assert.Equal(lastGood.Tasks, retained.Tasks)
	assert.Equal(lastGood.LastScan, retained.LastScan)
	assert.Equal(lastGood.RemoteRevision, retained.RemoteRevision)
}

func TestReverseIndexRejectsIdentityMismatchBeforeReturningTitles(t *testing.T) {
	idx := NewIndex(filepath.Join(t.TempDir(), "index.json"), time.Now)
	identity := testIdentity()
	cacheIdentity := testCacheIdentity()
	_, err := idx.Rebuild(context.Background(), &listClient{pages: []taskclient.TaskList{{Tasks: []taskclient.Task{indexedTask("one", "Must not leak", identity)}}}}, cacheIdentity, "remote-1", 100)
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*CacheIdentity)
		want   IndexState
	}{
		{name: "project", mutate: func(got *CacheIdentity) { got.Project = "other" }, want: StateWrongProject},
		{name: "archive uid", mutate: func(got *CacheIdentity) { got.ArchiveUID = "other" }, want: StateStale},
		{name: "archive revision", mutate: func(got *CacheIdentity) { got.ArchiveRevision = "other" }, want: StateStale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expected := cacheIdentity
			tt.mutate(&expected)
			result := idx.Lookup(expected, identity, true)
			assert.Equal(t, tt.want, result.State)
			assert.Empty(t, result.Tasks)
		})
	}
}

func TestReverseIndexBoundsOversizedPagesAndCyclicCursors(t *testing.T) {
	assert := assert.New(t)
	idx := NewIndex(filepath.Join(t.TempDir(), "index.json"), time.Now)
	identity := testIdentity()
	cacheIdentity := testCacheIdentity()
	oversized := &listClient{pages: []taskclient.TaskList{{
		Tasks: []taskclient.Task{
			indexedTask("one", "One", identity), indexedTask("two", "Two", identity), indexedTask("three", "Three", identity),
		},
		NextCursor: "more",
	}}}
	status, err := idx.Rebuild(context.Background(), oversized, cacheIdentity, "remote-1", 2)
	require.NoError(t, err)
	assert.Equal(StatePartial, status.State)
	assert.Equal(ReasonSafetyLimit, status.Reason)
	assert.Len(idx.Lookup(cacheIdentity, identity, true).Tasks, 2)
	assert.Equal([]int{2}, oversized.limits)

	cyclic := &listClient{pages: []taskclient.TaskList{
		{Tasks: []taskclient.Task{{ID: "unlinked-1", Project: "project", Title: "Unlinked", Revision: "r1"}}, NextCursor: "again"},
		{Tasks: []taskclient.Task{{ID: "unlinked-2", Project: "project", Title: "Unlinked", Revision: "r1"}}, NextCursor: "again"},
	}}
	status, err = idx.Rebuild(context.Background(), cyclic, cacheIdentity, "remote-2", 100)
	require.NoError(t, err)
	assert.Equal(StatePartial, status.State)
	assert.Equal(ReasonCursorCycle, status.Reason)
	assert.Equal(2, cyclic.calls)
}

func TestReverseIndexCacheFileSecurityAndSizeBounds(t *testing.T) {
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "tasks")
	require.NoError(os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "reverse-index.json")
	require.NoError(os.WriteFile(path, []byte(`{}`), 0o644))
	require.NoError(os.Chmod(path, 0o644))
	idx := NewIndex(path, time.Now)
	require.Error(idx.Load(), "group/world-readable caches may contain task titles")

	require.NoError(os.Remove(path))
	target := filepath.Join(dir, "target")
	require.NoError(os.WriteFile(target, []byte(`{}`), 0o600))
	require.NoError(os.Symlink(target, path))
	require.Error(idx.Load(), "cache symlinks must be rejected")

	require.NoError(os.Remove(path))
	require.NoError(os.WriteFile(path, make([]byte, MaxCacheFileBytes+1), 0o600))
	require.Error(idx.Load(), "cache reads must be bounded")
}

func TestReverseIndexSaveRepairsDirectoryAndUsesPrivateFileMode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "tasks")
	require.NoError(os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "reverse-index.json")
	idx := NewIndex(path, time.Now)
	require.NoError(idx.save(indexFile{Identity: testCacheIdentity()}))
	dirInfo, err := os.Stat(dir)
	require.NoError(err)
	assert.Equal(os.FileMode(0o700), dirInfo.Mode().Perm())
	fileInfo, err := os.Stat(path)
	require.NoError(err)
	assert.Equal(os.FileMode(0o600), fileInfo.Mode().Perm())
}
