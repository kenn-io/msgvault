package hex

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/store"
)

func TestPreserveSnapshotUntilReadableFinalReplacement(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			st, imp, opts := importFixture(t)
			dir := fixtureMeeting(t, opts.MeetingsDir, "preserve-all", "complete")
			fixtureTranscript(t, dir)
			writeTrack(t, dir, "system")
			opts.Recordings = true
			opts.SendRecording = func(ctx context.Context, u RecordingUpload, r *meetingarchive.Recording) error {
				_, err := imp.ImportRecording(ctx, u, opts.AttachmentsDir, r.File, 0)
				return err
			}
			source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
			_, err := imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
			require.NoError(t, err)
			sync := func() (*ImportSummary, error) {
				if remote {
					return ScanRemote(t.Context(), opts, imp.ImportRemote)
				}
				return imp.Import(t.Context(), opts)
			}
			sum, err := sync()
			require.NoError(t, err)
			id := messageID(t, st, sum.SourceID, "preserve-all")
			raw, err := st.GetMessageRaw(id)
			require.NoError(t, err)
			before, err := st.GetMessage(id)
			require.NoError(t, err)
			require.NoError(t, os.Remove(filepath.Join(dir, "transcript.ndjson")))
			opts.Full = true
			for _, rolling := range []bool{false, true} {
				if rolling {
					require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.rolling.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":0,\"end_ms\":500,\"text\":\"rolling replacement\"}\n"), 0600))
				}
				sum, err = sync()
				require.NoError(t, err)
				assert.Positive(t, sum.Warnings)
				after, err := st.GetMessageRaw(id)
				require.NoError(t, err)
				assert.Equal(t, string(raw), string(after))
				message, err := st.GetMessage(id)
				require.NoError(t, err)
				assert.Equal(t, before.Body, message.Body)
				matches, total, err := st.SearchMessagesContext(t.Context(), "Synthetic transcript", 0, 10)
				require.NoError(t, err)
				assert.Equal(t, int64(1), total)
				assert.Len(t, matches, 1)
				assert.Len(t, message.Attachments, 1)
				ids := []int64{id}
				packet, err := st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: &ids}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 100000})
				require.NoError(t, err)
				assert.Contains(t, packet.Content, "Synthetic transcript")
				assert.Contains(t, packet.Content, "60.333")
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":0,\"end_ms\":100,\"text\":\"Short\"}\n"), 0600))
			sum, err = sync()
			require.NoError(t, err)
			assert.Equal(t, int64(1), sum.MeetingsUpdated)
			after, err := st.GetMessageRaw(id)
			require.NoError(t, err)
			assert.Contains(t, meetingcontent.Decode(RawFormat, after, nil).Transcript.Text, "Short")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.ndjson"), nil, 0600))
			_, err = sync()
			require.NoError(t, err)
			empty, err := st.GetMessageRaw(id)
			require.NoError(t, err)
			assert.Empty(t, meetingcontent.Decode(RawFormat, empty, nil).Transcript.Text)
			var wire map[string]any
			require.NoError(t, json.Unmarshal(after, &wire))
			wire["version"] = 2
			future, err := json.Marshal(wire)
			require.NoError(t, err)
			require.NoError(t, st.UpsertMessageRawWithFormat(id, future, RawFormat))
			assert.Equal(t, meetingcontent.StateUnavailable, meetingcontent.Decode(RawFormat, future, nil).Transcript.State)
			_, err = sync()
			require.Error(t, err)
			untouched, err := st.GetMessageRaw(id)
			require.NoError(t, err)
			assert.Equal(t, string(future), string(untouched))
			manifestPath := filepath.Join(dir, "manifest.json")
			manifestBytes, err := os.ReadFile(manifestPath)
			require.NoError(t, err)
			var manifest Manifest
			require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
			manifest.TitleSource = "user"
			manifestBytes, err = json.Marshal(manifest)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(manifestPath, manifestBytes, 0600))
			_, err = sync()
			require.Error(t, err, "clean explicit-title finals must also refuse future evidence")
			untouched, err = st.GetMessageRaw(id)
			require.NoError(t, err)
			assert.Equal(t, string(future), string(untouched))
		})
	}
}
func TestHexTitlesResolvedOnRecorder(t *testing.T) {
	zone, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	old := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = old })
	st, imp, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "titles", "complete")
	fixtureTranscript(t, dir)
	v := Manifest{SchemaVersion: 1, ID: "titles", Status: "complete", CreatedAtMS: 1790000000123, App: "Zoom", Notes: &Notes{Title: " Notes title "}, ProvisionalTitle: "Provisional"}
	v.Title = "Zoom call — " + time.UnixMilli(v.CreatedAtMS).In(zone).Format("3:04 PM")
	for _, source := range []string{"", "default", "user", "calendar", "call"} {
		v.TitleSource = source
		data, err := json.Marshal(v)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600))
		reader, err := Open(t.Context(), opts.MeetingsDir)
		require.NoError(t, err)
		scan, err := reader.ListMeetings(t.Context())
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		require.Len(t, scan.Meetings, 1)
		m := scan.Meetings[0]
		time.Local = time.UTC
		snapshot, err := m.ArchiveSnapshot(1, opts.Identifier, opts.AccountEmail)
		require.NoError(t, err)
		want := v.Title
		if source == "" || source == "default" {
			want = "Notes title"
		}
		assert.Equal(t, want, snapshot.Title)
		assert.Contains(t, snapshot.Body, "Provisional")
		assert.Contains(t, snapshot.Body, v.Title)
		account := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
		_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: account})
		require.NoError(t, err)
		_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "upsert", Source: account, Meeting: &m})
		require.NoError(t, err)
		if source == "" {
			for _, remote := range []bool{false, true} {
				var sum *ImportSummary
				if remote {
					sum, err = ScanRemote(t.Context(), opts, imp.ImportRemote)
				} else {
					sum, err = imp.Import(t.Context(), opts)
				}
				require.NoError(t, err)
				assert.Zero(t, sum.MeetingsUpdated, "timezone changes must not alter an archived legacy title")
				message, err := st.GetMessage(messageID(t, st, sum.SourceID, "titles"))
				require.NoError(t, err)
				assert.Equal(t, want, message.Subject)
			}
		}
		time.Local = zone
	}
	v.TitleSource = "default"
	v.Notes.Title = " "
	assert.Equal(t, "Provisional", v.DisplayTitle())
}
func TestReaderSharedLockAndNDJSONOnlyPublication(t *testing.T) {
	root := t.TempDir()
	dir := fixtureMeeting(t, root, "read-lock", "transcribing")
	fixtureTranscript(t, dir)
	require.NoError(t, os.Remove(filepath.Join(dir, "transcript.md")))
	lock := flock.New(filepath.Join(dir, "active.lock"))
	require.NoError(t, lock.RLock())
	defer func() { _ = lock.Close() }()
	reader, err := Open(t.Context(), root)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	scan, err := reader.ListMeetings(t.Context())
	require.NoError(t, err)
	require.Len(t, scan.Meetings, 1)
	assert.Zero(t, scan.Deferred)
	assert.Equal(t, "complete", scan.Meetings[0].Manifest.Status)
}

func TestMetadataOnlyMeetingCanUpdate(t *testing.T) {
	st, imp, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "metadata", "failed")
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sum.MeetingsAdded)
	var manifest Manifest
	path := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &manifest))
	manifest.Title = "Updated metadata"
	raw, err = json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0600))
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sum.MeetingsUpdated)
	message, err := st.GetMessage(messageID(t, st, sum.SourceID, "metadata"))
	require.NoError(t, err)
	assert.Equal(t, "Updated metadata", message.Subject)
}
