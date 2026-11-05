package hex

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func importFixture(t *testing.T) (*store.Store, *Importer, ImportOptions) {
	t.Helper()
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "recorder")
	require.NoError(t, err)
	return st, NewImporter(st), ImportOptions{Identifier: "recorder", AccountEmail: "you@example.com", MeetingsDir: t.TempDir(), AttachmentsDir: t.TempDir(), LockDir: t.TempDir(), RemoteTarget: "https://archive.example.com"}
}
func fixtureTranscript(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte("{\"source\":\"microphone\",\"start_ms\":1500,\"end_ms\":3000,\"text\":\"Synthetic transcript\"}\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.md"), []byte("published"), 0600))
}
func messageID(t *testing.T, st *store.Store, sourceID int64, id string) int64 {
	t.Helper()
	rows, err := st.MessageMetadataBatch(sourceID, []string{"meeting:" + id})
	require.NoError(t, err)
	require.Contains(t, rows, "meeting:"+id)
	return rows["meeting:"+id].ID
}
func TestImportIdempotencyEditsAndSourceExpiry(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	st, imp, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "synthetic", "transcribing")
	rolling := "{\"covered_ms\":{\"system\":4000,\"microphone\":4000}}\n{\"source\":\"system\",\"start_ms\":1000,\"end_ms\":4000,\"text\":\"Recovered synthetic discussion\"}\n"
	must.NoError(os.WriteFile(filepath.Join(dir, "transcript.rolling.ndjson"), []byte(rolling), 0600))
	sum, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.MeetingsAdded)
	id := messageID(t, st, sum.SourceID, "synthetic")
	raw, err := st.GetMessageRaw(id)
	must.NoError(err)
	content := meetingcontent.Decode(RawFormat, raw, nil)
	check.Contains(content.Transcript.Text, "Recovered synthetic discussion")
	check.Equal("hex_rolling", content.Transcript.Reason)
	must.Len(content.SourceParticipants, 2)
	check.InDelta(60.333, *content.DurationSeconds, 0.0001)
	fixtureTranscript(t, dir)
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.MeetingsUpdated, "a published final upgrades the rolling meeting")
	raw, err = st.GetMessageRaw(id)
	must.NoError(err)
	content = meetingcontent.Decode(RawFormat, raw, nil)

	check.Equal(meetingcontent.StateAvailable, content.Transcript.State)
	check.InDelta(60.333, *content.DurationSeconds, 0.0001)
	must.Len(content.SourceParticipants, 2)
	check.Equal("Synthetic Guest", content.SourceParticipants[0].Name)
	check.Equal("guest@example.com", content.SourceParticipants[1].Email)
	var recipients int
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`), id).Scan(&recipients))
	check.Equal(2, recipients, "only organizer and explicit email attendee are recipients")
	ids := []int64{id}
	packet, err := st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: &ids}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 100000})
	must.NoError(err)
	check.Contains(packet.Content, "Synthetic transcript")
	check.Contains(packet.Content, "Synthetic Guest")
	check.Contains(packet.Content, "Opening")
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Zero(sum.MeetingsAdded + sum.MeetingsUpdated)
	source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
	_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
	must.NoError(err)
	sends := 0
	sender := func(ctx context.Context, r RemoteRequest) (RemoteResult, error) {
		sends++
		return imp.ImportRemote(ctx, r)
	}
	_, err = ScanRemote(t.Context(), opts, sender)
	must.NoError(err)
	_, err = ScanRemote(t.Context(), opts, sender)
	must.NoError(err)
	check.Equal(1, sends)

	manifestPath := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	must.NoError(err)
	must.NoError(os.WriteFile(manifestPath, bytes.ReplaceAll(data, []byte("Literal title"), []byte("Edited title")), 0600))
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.MeetingsUpdated)
	opts.Full = true
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.MeetingsUpdated)
	must.NoError(os.RemoveAll(dir))
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Zero(sum.MeetingsProcessed)
	_, err = st.GetMessageContext(t.Context(), id)
	must.NoError(err)
	raw, err = st.GetMessageRaw(id)
	must.NoError(err)
	check.Contains(meetingcontent.Decode(RawFormat, raw, nil).Transcript.Text, "Synthetic transcript")

}
func TestImportMetadataOnlyAndFilters(t *testing.T) {
	t.Run("metadata import filters", func(t *testing.T) {
		check := assert.New(t)
		must := require.New(t)

		_, imp, opts := importFixture(t)
		fixtureMeeting(t, opts.MeetingsDir, "a", "failed")
		fixtureMeeting(t, opts.MeetingsDir, "b", "failed")
		for _, id := range []string{"a", "b"} {
			data, err := json.Marshal(Manifest{SchemaVersion: 1, ID: id, Title: "Metadata only", Status: "failed", CreatedAtMS: 1790000000123})
			must.NoError(err)
			must.NoError(os.WriteFile(filepath.Join(opts.MeetingsDir, id, "manifest.json"), data, 0600))
		}

		opts.Limit = 1
		sum, err := imp.Import(t.Context(), opts)
		must.NoError(err)
		check.Equal(int64(1), sum.MeetingsAdded)
		opts.Limit = 0
		opts.StartedAfter = time.UnixMilli(1790000000124)
		sum, err = imp.Import(t.Context(), opts)
		must.NoError(err)
		check.Zero(sum.MeetingsProcessed)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = imp.Import(ctx, opts)
		must.ErrorIs(err, context.Canceled)
	})
	t.Run("metadata only edits", func(t *testing.T) {
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
	})
}

func writeTrack(t *testing.T, dir, track string) {
	t.Helper()
	path := filepath.Join(dir, track+".wav")
	require.NoError(t, os.WriteFile(path, testutil.RecordingWAV(), 0600))
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(path, old, old))
}
func TestTwoTrackAudioAndLateCapture(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	st, imp, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "audio", "complete")
	fixtureTranscript(t, dir)
	writeTrack(t, dir, "system")
	writeTrack(t, dir, "microphone")
	sum, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	id := messageID(t, st, sum.SourceID, "audio")
	msg, err := st.GetMessageContext(t.Context(), id)
	must.NoError(err)
	check.Empty(msg.Attachments)
	opts.Recordings = true
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(2), sum.RecordingsStored)
	check.Equal(int64(1), sum.MeetingsUpdated, "two audio tracks update one meeting")
	msg, err = st.GetMessageContext(t.Context(), id)
	must.NoError(err)
	check.Len(msg.Attachments, 2, "identical bytes in different tracks retain two occurrences")
	for _, track := range []string{"system", "microphone"} {
		refs, err := st.MessageAttachmentsByPartKeyContext(t.Context(), id, "hex:"+track)
		must.NoError(err)
		check.Len(refs, 1)
	}
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Zero(sum.MeetingsUpdated)
	opts.RecordingMaxBytes = 8
	_, err = imp.Import(t.Context(), opts)
	must.ErrorIs(err, meetingarchive.ErrRecordingTooLarge)
	opts.RecordingMaxBytes = 0
	must.NoError(os.Remove(filepath.Join(dir, "microphone.wav")))
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.RecordingsSkipped)
}
func TestRemoteEquivalenceReceiptsAndRecordingRecovery(t *testing.T) {
	t.Run("final transcript", func(t *testing.T) {
		check := assert.New(t)
		must := require.New(t)

		st, imp, opts := importFixture(t)
		source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
		_, err := imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
		must.NoError(err)
		dir := fixtureMeeting(t, opts.MeetingsDir, "remote", "complete")
		fixtureTranscript(t, dir)
		writeTrack(t, dir, "system")
		opts.Recordings = true
		sends, tracks := 0, 0
		failMic := false
		opts.SendRecording = func(ctx context.Context, u RecordingUpload, r *meetingarchive.Recording) error {
			tracks++
			if failMic && u.Track == "microphone" {
				return errors.New("lost audio response")
			}
			_, err := imp.ImportRecording(ctx, u, opts.AttachmentsDir, r.File, 0)
			return err
		}
		sender := func(ctx context.Context, r RemoteRequest) (RemoteResult, error) {
			sends++
			return imp.ImportRemote(ctx, r)
		}
		sum, err := ScanRemote(t.Context(), opts, sender)
		must.NoError(err)
		check.Equal(1, sends)
		check.Equal(1, tracks)
		id := messageID(t, st, sum.SourceID, "remote")
		raw, err := st.GetMessageRaw(id)
		must.NoError(err)
		reader, err := Open(t.Context(), opts.MeetingsDir)
		must.NoError(err)
		scan, err := reader.ListMeetings(t.Context())
		must.NoError(err)
		must.NoError(reader.Close())
		snapshot, err := scan.Meetings[0].ArchiveSnapshot(sum.SourceID, opts.Identifier, opts.AccountEmail)
		must.NoError(err)
		check.JSONEq(string(snapshot.Raw), string(raw))
		_, err = ScanRemote(t.Context(), opts, sender)
		must.NoError(err)
		check.Equal(1, sends)
		writeTrack(t, dir, "microphone")
		failMic = true
		_, err = ScanRemote(t.Context(), opts, sender)
		must.Error(err)
		check.Equal(2, sends)
		failMic = false
		_, err = ScanRemote(t.Context(), opts, sender)
		must.NoError(err)
		check.Equal(3, sends)
		check.Equal(3, tracks)
		_, err = ScanRemote(t.Context(), opts, sender)
		must.NoError(err)
		check.Equal(3, sends)
		opts.Full = true
		_, err = ScanRemote(t.Context(), opts, sender)
		must.NoError(err)
		check.Equal(4, sends)
		check.Equal(5, tracks)
		must.NoError(ForgetRemoteUploads(t.Context(), opts))
		opts.Full = false
		_, err = ScanRemote(t.Context(), opts, sender)
		must.NoError(err)
		check.Equal(5, sends)
	})
	t.Run("retained transcript", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		st, imp, opts := importFixture(t)
		dir := fixtureMeeting(t, opts.MeetingsDir, "retained-receipt", "complete")
		fixtureTranscript(t, dir)
		_, err := imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}})
		must.NoError(err)
		sends, audio := 0, 0
		send := func(ctx context.Context, req RemoteRequest) (RemoteResult, error) {
			sends++
			return imp.ImportRemote(ctx, req)
		}
		sum, err := ScanRemote(t.Context(), opts, send)
		must.NoError(err)
		id := messageID(t, st, sum.SourceID, "retained-receipt")
		before, err := st.GetMessageRaw(id)
		must.NoError(err)
		must.NoError(os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte("unfinished"), 0600))
		for range 3 {
			_, err = ScanRemote(t.Context(), opts, send)
			must.NoError(err)
		}
		check.Equal(2, sends, "retained payload is acknowledged after its first successful offer")
		opts.Recordings = true
		writeTrack(t, dir, "system")
		fail := true
		opts.SendRecording = func(ctx context.Context, upload RecordingUpload, r *meetingarchive.Recording) error {
			audio++
			if fail {
				return meetingarchive.ErrRecordingTooLarge
			}
			_, err := imp.ImportRecording(ctx, upload, opts.AttachmentsDir, io.NewSectionReader(r.File, 0, r.Size), 0)
			return err
		}
		_, err = ScanRemote(t.Context(), opts, send)
		must.ErrorIs(err, meetingarchive.ErrRecordingTooLarge)
		fail = false
		_, err = ScanRemote(t.Context(), opts, send)
		must.NoError(err)
		check.Equal(2, audio, "failed recording remains pending despite the transcript receipt")
		_, err = ScanRemote(t.Context(), opts, send)
		must.NoError(err)
		check.Equal(4, sends)
		check.Equal(2, audio)
		path := filepath.Join(dir, "system.wav")
		data, err := os.ReadFile(path)
		must.NoError(err)
		data[len(data)-1]++
		must.NoError(os.WriteFile(path, data, 0600))
		old := time.Now().Add(-time.Minute)
		must.NoError(os.Chtimes(path, old, old))
		_, err = ScanRemote(t.Context(), opts, send)
		must.NoError(err)
		check.Equal(3, audio, "changed recording bypasses the acknowledged transcript")
		after, err := st.GetMessageRaw(id)
		must.NoError(err)
		check.Equal(string(before), string(after), "all retries preserve readable transcript evidence")
	})
}
func TestRemoteStrictValidationAndRecorderBoundary(t *testing.T) {
	must := require.New(t)

	_, imp, opts := importFixture(t)
	source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
	_, err := DecodeRemoteRequest(bytes.NewBufferString(`{"action":"register","source":{"identifier":"x","account_email":"you@example.com"},"path":"private"}`), MaxRemoteRequestBytes)
	must.ErrorIs(err, ErrRemoteMalformed)
	_, err = DecodeRemoteRequest(bytes.NewBufferString(`{}`), 1)
	must.ErrorIs(err, ErrRemoteTooLarge)
	_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
	must.NoError(err)
	source.AccountEmail = "other@example.com"
	_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
	must.ErrorIs(err, ErrRemoteValidation)
	var m Meeting
	must.NoError(json.Unmarshal([]byte(`{"manifest":{"schema_version":1,"id":"../escape","status":"failed","created_at_ms":1},"transcript_state":"missing"}`), &m))
	_, err = (RemoteRequest{Action: "upsert", Source: source, Meeting: &m}).Normalize()
	must.ErrorIs(err, ErrRemoteRecordValidation)
}

func TestRemoteLostMeetingResponseReoffersAudio(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	_, imp, opts := importFixture(t)
	source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
	_, err := imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
	must.NoError(err)
	dir := fixtureMeeting(t, opts.MeetingsDir, "lost-response", "complete")
	fixtureTranscript(t, dir)
	writeTrack(t, dir, "system")
	opts.Recordings = true
	audioCalls := 0
	opts.SendRecording = func(ctx context.Context, u RecordingUpload, r *meetingarchive.Recording) error {
		audioCalls++
		_, err := imp.ImportRecording(ctx, u, opts.AttachmentsDir, r.File, 0)
		return err
	}
	lose := true
	sender := func(ctx context.Context, r RemoteRequest) (RemoteResult, error) {
		result, err := imp.ImportRemote(ctx, r)
		if err == nil && lose {
			return RemoteResult{}, errors.New("lost meeting response")
		}
		return result, err
	}
	_, err = ScanRemote(t.Context(), opts, sender)
	must.Error(err)
	check.Zero(audioCalls)
	lose = false
	_, err = ScanRemote(t.Context(), opts, sender)
	must.NoError(err)
	check.Equal(1, audioCalls)
	_, err = ScanRemote(t.Context(), opts, sender)
	must.NoError(err)
	check.Equal(1, audioCalls)
}

func TestUnregisteredHexSourceGivesRegistrationHint(t *testing.T) {
	_, imp, opts := importFixture(t)
	opts.Identifier = "unregistered"
	_, err := imp.Import(t.Context(), opts)
	require.ErrorIs(t, err, store.ErrSourceNotFound)
	assert.ErrorContains(t, err, "msgvault add-hex unregistered first")
}
