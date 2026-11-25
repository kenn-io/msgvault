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
	"go.kenn.io/msgvault/internal/meetingcontent"
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
	check := assert.New(t)
	must := require.New(t)

	root := t.TempDir()
	final := fixtureMeeting(t, root, "a-final", "transcribing")
	must.NoError(os.WriteFile(filepath.Join(final, "transcript.ndjson"), []byte(" \t\n{\"source\":\"microphone\",\"start_ms\":1500,\"end_ms\":2000,\"text\":\"Final words\"}\ninvalid\n{\"source\":\"system\"}"), 0600))
	must.NoError(os.WriteFile(filepath.Join(final, "transcript.rolling.ndjson"), []byte("ignored\n"), 0600))
	rolling := fixtureMeeting(t, root, "b-rolling", "recording")
	must.NoError(os.WriteFile(filepath.Join(rolling, "transcript.rolling.ndjson"), []byte("{\"covered_ms\":{\"system\":1000}}\n{\"source\":\"system\",\"start_ms\":0,\"end_ms\":1000,\"text\":\"Rolling words\"}\n"), 0600))
	fixtureMeeting(t, root, "c-empty", "failed")
	bad := fixtureMeeting(t, root, "d-bad", "complete")
	must.NoError(os.WriteFile(filepath.Join(bad, "manifest.json"), []byte(`{"schema_version":2,"id":"d-bad"}`), 0600))
	mismatch := fixtureMeeting(t, root, "e-mismatch", "failed")
	must.NoError(os.WriteFile(filepath.Join(mismatch, "manifest.json"), []byte(`{"schema_version":1,"id":"elsewhere"}`), 0600))

	unreadable := fixtureMeeting(t, root, "unreadable-transcript", "complete")
	must.NoError(os.Mkdir(filepath.Join(unreadable, "transcript.ndjson"), 0700))
	reader, err := Open(t.Context(), root)
	must.NoError(err)
	defer func() { _ = reader.Close() }()
	scan, err := reader.ListMeetings(t.Context())
	must.NoError(err)
	must.Len(scan.Meetings, 3)
	check.Equal(int64(5), scan.Warnings)
	check.Equal("complete", scan.Meetings[0].Manifest.Status)
	check.Equal("final", scan.Meetings[0].TranscriptState)
	must.Len(scan.Meetings[0].Entries, 1)
	check.Equal("Final words", scan.Meetings[0].Entries[0].Text)
	check.Equal(Summary("First point Second point"), scan.Meetings[0].Manifest.Notes.Summary)
	check.Equal("interrupted", scan.Meetings[1].Manifest.Status)
	check.Equal("rolling", scan.Meetings[1].TranscriptState)
	check.Equal("missing", scan.Meetings[2].TranscriptState)
	raw, err := json.Marshal(scan.Meetings)
	must.NoError(err)
	check.NotContains(string(raw), "/private/source")
	check.NotContains(string(raw), "future")
	_, err = os.Stat(filepath.Join(rolling, "active.lock"))
	must.ErrorIs(err, os.ErrNotExist)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = reader.ListMeetings(ctx)
	must.ErrorIs(err, context.Canceled)
}
func TestReaderHeldStaleAndNonRegularLocks(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	root := t.TempDir()
	dir := fixtureMeeting(t, root, "locked", "recording")
	lock := flock.New(filepath.Join(dir, "active.lock"))
	must.NoError(lock.Lock())
	defer func() { _ = lock.Close() }()
	reader, err := Open(t.Context(), root)
	must.NoError(err)
	defer func() { _ = reader.Close() }()
	scan, err := reader.ListMeetings(t.Context())
	must.NoError(err)
	check.Empty(scan.Meetings)
	check.Equal(int64(1), scan.Deferred)
	must.NoError(lock.Unlock())
	scan, err = reader.ListMeetings(t.Context())
	must.NoError(err)
	must.Len(scan.Meetings, 1)
	check.Equal("interrupted", scan.Meetings[0].Manifest.Status)
	unreadable := fixtureMeeting(t, root, "unreadable", "complete")
	must.NoError(os.Mkdir(filepath.Join(unreadable, "active.lock"), 0700))
	scan, err = reader.ListMeetings(t.Context())
	must.NoError(err)
	check.Equal(int64(1), scan.Deferred)

	t.Run("shared lock and NDJSON publication", func(t *testing.T) {
		check := assert.New(t)
		must := require.New(t)

		root := t.TempDir()
		dir := fixtureMeeting(t, root, "read-lock", "transcribing")
		fixtureTranscript(t, dir)
		lock := flock.New(filepath.Join(dir, "active.lock"))
		must.NoError(lock.RLock())
		defer func() { _ = lock.Close() }()
		reader, err := Open(t.Context(), root)
		must.NoError(err)
		defer func() { _ = reader.Close() }()
		scan, err := reader.ListMeetings(t.Context())
		must.NoError(err)
		must.Len(scan.Meetings, 1)
		check.Zero(scan.Deferred)
		check.Equal("complete", scan.Meetings[0].Manifest.Status)
	})
}

func TestDecodeTranscriptSerdeVariants(t *testing.T) {
	// Synthetic JSON with Hex's serde field shapes: u64 offsets, snake-case
	// source, and optional coverage counters. No upstream personal fixture.
	entry := "{\"source\":\"system\",\"start_ms\":0,\"end_ms\":250,\"text\":\"Synthetic words\"}\n"
	coverage := "{\"covered_ms\":{\"system\":250,\"microphone\":null}}\n"
	for _, tc := range []struct {
		name, data string
		rolling    bool
		warnings   int64
		entries    int
	}{
		{"rolling coverage", coverage + entry, true, 0, 1},
		{"final coverage is not an entry", coverage + entry, false, 1, 1},
		{"valid prefix unfinished tail", entry + "unfinished", false, 1, 1},
		{"whitespace tail", entry + " \t", false, 0, 1},
		{"empty final", "", false, 0, 0},
		{"missing timestamps", "{\"source\":\"system\",\"text\":\"Missing timestamps\"}\n", false, 1, 0},
		{"missing start", "{\"source\":\"system\",\"end_ms\":0,\"text\":\"Missing start\"}\n", false, 1, 0},
		{"missing end", "{\"source\":\"system\",\"start_ms\":0,\"text\":\"Missing end\"}\n", false, 1, 0},
		{"null timestamps", "{\"source\":\"system\",\"start_ms\":null,\"end_ms\":null,\"text\":\"Null timestamps\"}\n", false, 1, 0},
		{"discarded empty entry", "{\"source\":\"system\",\"start_ms\":0,\"end_ms\":250,\"text\":\" \"}\n", false, 1, 0},
		{"final entry with coverage field", "{\"covered_ms\":{},\"source\":\"microphone\",\"start_ms\":0,\"end_ms\":250,\"text\":\"Synthetic words\"}\n", false, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, partial, warnings := decodeTranscript([]byte(tc.data), tc.rolling)
			assert.Len(t, entries, tc.entries)
			assert.Equal(t, tc.warnings, warnings)
			assert.Equal(t, tc.warnings > 0, partial)
		})
	}
}

func TestReaderEmptyRollingTranscriptIsMissing(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	root := t.TempDir()
	dir := fixtureMeeting(t, root, "empty-rolling", "recording")
	must.NoError(os.WriteFile(filepath.Join(dir, "transcript.rolling.ndjson"), []byte("{\"covered_ms\":{\"system\":1000,\"microphone\":null}}\n"), 0600))
	reader, err := Open(t.Context(), root)
	must.NoError(err)
	defer func() { _ = reader.Close() }()
	scan, err := reader.ListMeetings(t.Context())
	must.NoError(err)
	must.Len(scan.Meetings, 1)
	check.Zero(scan.Warnings)
	check.Equal("missing", scan.Meetings[0].TranscriptState)
	check.Empty(scan.Meetings[0].Entries)
	snapshot, err := scan.Meetings[0].ArchiveSnapshot(1, "recorder", "you@example.com")
	must.NoError(err)
	content := meetingcontent.Decode(RawFormat, snapshot.Raw, nil)
	check.Equal(meetingcontent.StateUnavailable, content.Transcript.State)
	check.Equal("hex_missing", content.Transcript.Reason)
}

func TestReaderDefersPendingImportWithoutManifest(t *testing.T) {
	root := t.TempDir()
	pending := filepath.Join(root, "pending")
	require.NoError(t, os.Mkdir(pending, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(pending, "import.pending"), nil, 0600))
	require.NoError(t, os.Mkdir(filepath.Join(root, "missing"), 0700))
	reader, err := Open(t.Context(), root)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	scan, err := reader.ListMeetings(t.Context())
	require.NoError(t, err)
	assert.Empty(t, scan.Meetings)
	assert.Equal(t, int64(1), scan.Deferred)
	assert.Equal(t, int64(1), scan.Warnings)
	fixtureMeeting(t, root, "pending", "complete")
	scan, err = reader.ListMeetings(t.Context())
	require.NoError(t, err)
	assert.Len(t, scan.Meetings, 1)
	assert.Zero(t, scan.Deferred)
}
