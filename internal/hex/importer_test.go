package hex

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
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
	st, imp, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "synthetic", "complete")
	fixtureTranscript(t, dir)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sum.MeetingsAdded)
	id := messageID(t, st, sum.SourceID, "synthetic")
	raw, err := st.GetMessageRaw(id)
	require.NoError(t, err)
	content := meetingcontent.Decode(RawFormat, raw, nil)
	assert.Equal(t, meetingcontent.StateAvailable, content.Transcript.State)
	assert.InDelta(t, 60.333, *content.DurationSeconds, 0.0001)
	require.Len(t, content.SourceParticipants, 2)
	assert.Equal(t, "Synthetic Guest", content.SourceParticipants[0].Name)
	assert.Equal(t, "guest@example.com", content.SourceParticipants[1].Email)
	var recipients int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`), id).Scan(&recipients))
	assert.Equal(t, 2, recipients, "only organizer and explicit email attendee are recipients")
	ids := []int64{id}
	packet, err := st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: &ids}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 100000})
	require.NoError(t, err)
	assert.Contains(t, packet.Content, "Synthetic transcript")
	assert.Contains(t, packet.Content, "Opening")
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Zero(t, sum.MeetingsAdded+sum.MeetingsUpdated)
	manifestPath := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, bytes.ReplaceAll(data, []byte("Literal title"), []byte("Edited title")), 0600))
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sum.MeetingsUpdated)
	opts.Full = true
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sum.MeetingsUpdated)
	require.NoError(t, os.RemoveAll(dir))
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Zero(t, sum.MeetingsProcessed)
	_, err = st.GetMessageContext(t.Context(), id)
	require.NoError(t, err)
}
func TestImportMetadataOnlyAndFilters(t *testing.T) {
	_, imp, opts := importFixture(t)
	fixtureMeeting(t, opts.MeetingsDir, "a", "failed")
	fixtureMeeting(t, opts.MeetingsDir, "b", "failed")
	for _, id := range []string{"a", "b"} {
		data, err := json.Marshal(Manifest{SchemaVersion: 1, ID: id, Title: "Metadata only", Status: "failed", CreatedAtMS: 1790000000123})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(opts.MeetingsDir, id, "manifest.json"), data, 0600))
	}

	opts.Limit = 1
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sum.MeetingsAdded)
	opts.Limit = 0
	opts.StartedAfter = time.UnixMilli(1790000000124)
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Zero(t, sum.MeetingsProcessed)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = imp.Import(ctx, opts)
	assert.ErrorIs(t, err, context.Canceled)
}
func wavBytes() []byte {
	b := make([]byte, 48)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 40)
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 4)
	return b
}
func writeTrack(t *testing.T, dir, track string) {
	t.Helper()
	path := filepath.Join(dir, track+".wav")
	require.NoError(t, os.WriteFile(path, wavBytes(), 0600))
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(path, old, old))
}
func TestTwoTrackAudioAndLateCapture(t *testing.T) {
	st, imp, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "audio", "complete")
	fixtureTranscript(t, dir)
	writeTrack(t, dir, "system")
	writeTrack(t, dir, "microphone")
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(t, err)
	id := messageID(t, st, sum.SourceID, "audio")
	msg, err := st.GetMessageContext(t.Context(), id)
	require.NoError(t, err)
	assert.Empty(t, msg.Attachments)
	opts.Recordings = true
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(2), sum.RecordingsStored)
	assert.Equal(t, int64(1), sum.MeetingsUpdated, "two audio tracks update one meeting")
	msg, err = st.GetMessageContext(t.Context(), id)
	require.NoError(t, err)
	assert.Len(t, msg.Attachments, 2, "identical bytes in different tracks retain two occurrences")
	for _, track := range []string{"system", "microphone"} {
		refs, err := st.MessageAttachmentsByPartKeyContext(t.Context(), id, "hex:"+track)
		require.NoError(t, err)
		assert.Len(t, refs, 1)
	}
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Zero(t, sum.MeetingsUpdated)
	opts.RecordingMaxBytes = 8
	_, err = imp.Import(t.Context(), opts)
	assert.ErrorIs(t, err, meetingarchive.ErrRecordingTooLarge)
	opts.RecordingMaxBytes = 0
	require.NoError(t, os.Remove(filepath.Join(dir, "microphone.wav")))
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sum.RecordingsSkipped)
	writeTrack(t, dir, "microphone")
	_, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
}
func TestRemoteEquivalenceReceiptsAndRecordingRecovery(t *testing.T) {
	st, imp, opts := importFixture(t)
	source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
	_, err := imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
	require.NoError(t, err)
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
	require.NoError(t, err)
	assert.Equal(t, 1, sends)
	assert.Equal(t, 1, tracks)
	id := messageID(t, st, sum.SourceID, "remote")
	raw, err := st.GetMessageRaw(id)
	require.NoError(t, err)
	reader, err := Open(t.Context(), opts.MeetingsDir)
	require.NoError(t, err)
	scan, err := reader.ListMeetings(t.Context())
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	snapshot, err := scan.Meetings[0].ArchiveSnapshot(sum.SourceID, opts.Identifier, opts.AccountEmail)
	require.NoError(t, err)
	assert.JSONEq(t, string(snapshot.Raw), string(raw))
	_, err = ScanRemote(t.Context(), opts, sender)
	require.NoError(t, err)
	assert.Equal(t, 1, sends)
	writeTrack(t, dir, "microphone")
	failMic = true
	_, err = ScanRemote(t.Context(), opts, sender)
	require.Error(t, err)
	assert.Equal(t, 2, sends)
	failMic = false
	_, err = ScanRemote(t.Context(), opts, sender)
	require.NoError(t, err)
	assert.Equal(t, 3, sends)
	assert.Equal(t, 3, tracks)
	_, err = ScanRemote(t.Context(), opts, sender)
	require.NoError(t, err)
	assert.Equal(t, 3, sends)
	opts.Full = true
	_, err = ScanRemote(t.Context(), opts, sender)
	require.NoError(t, err)
	assert.Equal(t, 4, sends)
	assert.Equal(t, 5, tracks)
	require.NoError(t, ForgetRemoteUploads(t.Context(), opts))
	opts.Full = false
	_, err = ScanRemote(t.Context(), opts, sender)
	require.NoError(t, err)
	assert.Equal(t, 5, sends)
}
func TestRemoteStrictValidationAndRecorderBoundary(t *testing.T) {
	_, imp, opts := importFixture(t)
	source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
	_, err := DecodeRemoteRequest(bytes.NewBufferString(`{"action":"register","source":{"identifier":"x","account_email":"you@example.com"},"path":"private"}`), MaxRemoteRequestBytes)
	assert.ErrorIs(t, err, ErrRemoteMalformed)
	_, err = DecodeRemoteRequest(bytes.NewBufferString(`{}`), 1)
	assert.ErrorIs(t, err, ErrRemoteTooLarge)
	_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
	require.NoError(t, err)
	source.AccountEmail = "other@example.com"
	_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
	assert.ErrorIs(t, err, ErrRemoteValidation)
	var m Meeting
	require.NoError(t, json.Unmarshal([]byte(`{"manifest":{"schema_version":1,"id":"../escape","status":"failed","created_at_ms":1},"transcript_state":"missing"}`), &m))
	_, err = (RemoteRequest{Action: "upsert", Source: source, Meeting: &m}).Normalize()
	assert.ErrorIs(t, err, ErrRemoteRecordValidation)
}

func TestRemoteLostMeetingResponseReoffersAudio(t *testing.T) {
	_, imp, opts := importFixture(t)
	source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
	_, err := imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
	require.NoError(t, err)
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
	require.Error(t, err)
	assert.Zero(t, audioCalls)
	lose = false
	_, err = ScanRemote(t.Context(), opts, sender)
	require.NoError(t, err)
	assert.Equal(t, 1, audioCalls)
	_, err = ScanRemote(t.Context(), opts, sender)
	require.NoError(t, err)
	assert.Equal(t, 1, audioCalls)
}
