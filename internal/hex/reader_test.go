package hex

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixtureMeeting(t *testing.T, root, id, status string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	require.NoError(t, os.MkdirAll(dir, 0700))
	data := `{"schema_version":1,"id":"` + id + `","title":"Literal title","status":"` + status + `","created_at_ms":1790000000123,"ended_at_ms":1790000060456,"duration_ms":60333,"export_path":"/private/source","notes":{"title":"AI title","summary":[" First point ","","  ","Second point"]},"calendar":{"title":"Planning","attendees":["Synthetic Guest","guest@example.com"]},"chapters":[{"title":"Opening","summary":"Goals","start_ms":1500}],"future":true}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(data), 0600))
	return dir
}
func TestReaderRecoveryAndWarnings(t *testing.T) {
	root := t.TempDir()
	final := fixtureMeeting(t, root, "a-final", "transcribing")
	require.NoError(t, os.WriteFile(filepath.Join(final, "transcript.ndjson"), []byte("{\"source\":\"microphone\",\"start_ms\":1500,\"end_ms\":2000,\"text\":\"Final words\"}\ninvalid\n{\"source\":\"system\"}"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(final, "transcript.md"), []byte("published"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(final, "transcript.rolling.ndjson"), []byte("ignored\n"), 0600))
	rolling := fixtureMeeting(t, root, "b-rolling", "recording")
	require.NoError(t, os.WriteFile(filepath.Join(rolling, "transcript.rolling.ndjson"), []byte("{\"covered_ms\":{\"system\":1000}}\n{\"source\":\"system\",\"start_ms\":0,\"end_ms\":1000,\"text\":\"Rolling words\"}\n"), 0600))
	fixtureMeeting(t, root, "c-empty", "failed")
	bad := fixtureMeeting(t, root, "d-bad", "complete")
	require.NoError(t, os.WriteFile(filepath.Join(bad, "manifest.json"), []byte(`{"schema_version":2,"id":"d-bad"}`), 0600))
	mismatch := fixtureMeeting(t, root, "e-mismatch", "failed")
	require.NoError(t, os.WriteFile(filepath.Join(mismatch, "manifest.json"), []byte(`{"schema_version":1,"id":"elsewhere"}`), 0600))
	reader, err := Open(t.Context(), root)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	scan, err := reader.ListMeetings(t.Context())
	require.NoError(t, err)
	require.Len(t, scan.Meetings, 3)
	assert.Equal(t, int64(3), scan.Warnings)
	assert.Equal(t, "complete", scan.Meetings[0].Manifest.Status)
	assert.Equal(t, "final", scan.Meetings[0].TranscriptState)
	require.Len(t, scan.Meetings[0].Entries, 1)
	assert.Equal(t, "Final words", scan.Meetings[0].Entries[0].Text)
	assert.Equal(t, Summary("First point Second point"), scan.Meetings[0].Manifest.Notes.Summary)
	assert.Equal(t, "interrupted", scan.Meetings[1].Manifest.Status)
	assert.Equal(t, "rolling", scan.Meetings[1].TranscriptState)
	assert.Equal(t, "missing", scan.Meetings[2].TranscriptState)
	raw, err := json.Marshal(scan.Meetings)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "/private/source")
	assert.NotContains(t, string(raw), "future")
	_, err = os.Stat(filepath.Join(rolling, "active.lock"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = reader.ListMeetings(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}
func TestReaderHeldStaleAndUnreadableLocks(t *testing.T) {
	root := t.TempDir()
	dir := fixtureMeeting(t, root, "locked", "recording")
	lock := flock.New(filepath.Join(dir, "active.lock"))
	require.NoError(t, lock.Lock())
	defer func() { _ = lock.Close() }()
	reader, err := Open(t.Context(), root)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	scan, err := reader.ListMeetings(t.Context())
	require.NoError(t, err)
	assert.Empty(t, scan.Meetings)
	assert.Equal(t, int64(1), scan.Deferred)
	require.NoError(t, lock.Unlock())
	scan, err = reader.ListMeetings(t.Context())
	require.NoError(t, err)
	require.Len(t, scan.Meetings, 1)
	assert.Equal(t, "interrupted", scan.Meetings[0].Manifest.Status)
	unreadable := fixtureMeeting(t, root, "unreadable", "complete")
	require.NoError(t, os.Mkdir(filepath.Join(unreadable, "active.lock"), 0700))
	scan, err = reader.ListMeetings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), scan.Deferred)
}
