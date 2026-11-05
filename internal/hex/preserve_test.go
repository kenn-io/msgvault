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
			check := assert.New(t)
			must := require.New(t)

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
			must.NoError(err)
			sync := func() (*ImportSummary, error) {
				if remote {
					return ScanRemote(t.Context(), opts, imp.ImportRemote)
				}
				return imp.Import(t.Context(), opts)
			}
			sum, err := sync()
			must.NoError(err)
			id := messageID(t, st, sum.SourceID, "preserve-all")
			raw, err := st.GetMessageRaw(id)
			must.NoError(err)
			before, err := st.GetMessage(id)
			must.NoError(err)
			assertRetained := func(sum *ImportSummary) {
				check.Positive(sum.Warnings)
				after, err := st.GetMessageRaw(id)
				must.NoError(err)
				check.Equal(string(raw), string(after))
				message, err := st.GetMessage(id)
				must.NoError(err)
				check.Equal(before.Body, message.Body)
				matches, total, err := st.SearchMessagesContext(t.Context(), "Synthetic transcript", 0, 10)
				must.NoError(err)
				check.Equal(int64(1), total)
				check.Len(matches, 1)
				check.Len(message.Attachments, 1)
				ids := []int64{id}
				packet, err := st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: &ids}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 100000})
				must.NoError(err)
				check.Contains(packet.Content, "Synthetic transcript")
				check.Contains(packet.Content, "60.333")
			}
			// Cover the written round-3 regression, a valid prefix with an unfinished
			// suffix, and malformed complete input through both shared guard callers.
			for _, full := range []bool{false, true} {
				opts.Full = full
				for _, text := range []string{
					`{"source":"system","start_ms":0,"end_ms":100,"text":"Unfinished replacement"}`,
					"{\"source\":\"system\",\"start_ms\":0,\"end_ms\":100,\"text\":\"Partial prefix\"}\nunfinished",
					"malformed complete line\n",
				} {
					must.NoError(os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte(text), 0600))
					sum, err = sync()
					must.NoError(err)
					assertRetained(sum)
				}
			}
			must.NoError(os.Remove(filepath.Join(dir, "transcript.ndjson")))
			opts.Full = true
			for _, rolling := range []bool{false, true} {
				if rolling {
					must.NoError(os.WriteFile(filepath.Join(dir, "transcript.rolling.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":0,\"end_ms\":500,\"text\":\"rolling replacement\"}\n"), 0600))
				}
				sum, err = sync()
				must.NoError(err)
				assertRetained(sum)
			}
			must.NoError(os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":0,\"end_ms\":100,\"text\":\"Short\"}\n \t"), 0600))
			sum, err = sync()
			must.NoError(err)
			check.Equal(int64(1), sum.MeetingsUpdated)
			after, err := st.GetMessageRaw(id)
			must.NoError(err)
			check.Contains(meetingcontent.Decode(RawFormat, after, nil).Transcript.Text, "Short")
			for _, empty := range [][]byte{[]byte(" \t"), nil} {
				must.NoError(os.WriteFile(filepath.Join(dir, "transcript.ndjson"), empty, 0600))
				sum, err = sync()
				must.NoError(err)
				check.Zero(sum.Warnings)
			}
			empty, err := st.GetMessageRaw(id)
			must.NoError(err)
			check.Empty(meetingcontent.Decode(RawFormat, empty, nil).Transcript.Text)
			var wire map[string]any
			must.NoError(json.Unmarshal(after, &wire))
			wire["version"] = 2
			future, err := json.Marshal(wire)
			must.NoError(err)
			must.NoError(st.UpsertMessageRawWithFormat(id, future, RawFormat))
			check.Equal(meetingcontent.StateUnavailable, meetingcontent.Decode(RawFormat, future, nil).Transcript.State)
			_, err = sync()
			must.Error(err)
			untouched, err := st.GetMessageRaw(id)
			must.NoError(err)
			check.Equal(string(future), string(untouched))
			manifestPath := filepath.Join(dir, "manifest.json")
			manifestBytes, err := os.ReadFile(manifestPath)
			must.NoError(err)
			var manifest Manifest
			must.NoError(json.Unmarshal(manifestBytes, &manifest))
			manifest.TitleSource = "user"
			manifestBytes, err = json.Marshal(manifest)
			must.NoError(err)
			must.NoError(os.WriteFile(manifestPath, manifestBytes, 0600))
			_, err = sync()
			must.Error(err, "clean explicit-title finals must also refuse future evidence")
			untouched, err = st.GetMessageRaw(id)
			must.NoError(err)
			check.Equal(string(future), string(untouched))
		})
	}
}
func TestHexTitlesResolvedOnRecorder(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	zone, err := time.LoadLocation("America/Los_Angeles")
	must.NoError(err)
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
		must.NoError(err)
		must.NoError(os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600))
		reader, err := Open(t.Context(), opts.MeetingsDir)
		must.NoError(err)
		scan, err := reader.ListMeetings(t.Context())
		must.NoError(err)
		must.NoError(reader.Close())
		must.Len(scan.Meetings, 1)
		m := scan.Meetings[0]
		time.Local = time.UTC
		snapshot, err := m.ArchiveSnapshot(1, opts.Identifier, opts.AccountEmail)
		must.NoError(err)
		want := v.Title
		if source == "" || source == "default" {
			want = "Notes title"
		}
		check.Equal(want, snapshot.Title)
		check.Contains(snapshot.Body, "Provisional")
		check.Contains(snapshot.Body, v.Title)
		account := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
		_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: account})
		must.NoError(err)
		_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "upsert", Source: account, Meeting: &m})
		must.NoError(err)
		if source == "" {
			for _, remote := range []bool{false, true} {
				var sum *ImportSummary
				if remote {
					sum, err = ScanRemote(t.Context(), opts, imp.ImportRemote)
				} else {
					sum, err = imp.Import(t.Context(), opts)
				}
				must.NoError(err)
				check.Zero(sum.MeetingsUpdated, "timezone changes must not alter an archived legacy title")
				message, err := st.GetMessage(messageID(t, st, sum.SourceID, "titles"))
				must.NoError(err)
				check.Equal(want, message.Subject)
			}
		}
		time.Local = zone
	}
	v.TitleSource = "default"
	v.Notes.Title = " "
	check.Equal("Provisional", v.DisplayTitle())
}
func TestReaderSharedLockAndNDJSONOnlyPublication(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	root := t.TempDir()
	dir := fixtureMeeting(t, root, "read-lock", "transcribing")
	fixtureTranscript(t, dir)
	must.NoError(os.Remove(filepath.Join(dir, "transcript.md")))
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
}

func TestMetadataOnlyMeetingCanUpdate(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	st, imp, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "metadata", "failed")
	sum, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.MeetingsAdded)
	var manifest Manifest
	path := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(path)
	must.NoError(err)
	must.NoError(json.Unmarshal(raw, &manifest))
	manifest.Title = "Updated metadata"
	raw, err = json.Marshal(manifest)
	must.NoError(err)
	must.NoError(os.WriteFile(path, raw, 0600))
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.MeetingsUpdated)
	message, err := st.GetMessage(messageID(t, st, sum.SourceID, "metadata"))
	must.NoError(err)
	check.Equal("Updated metadata", message.Subject)
}
