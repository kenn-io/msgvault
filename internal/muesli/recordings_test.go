package muesli

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordingWAV() []byte {
	data := make([]byte, 48)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 16000)
	binary.LittleEndian.PutUint32(data[28:], 32000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], 4)
	return data
}

func writeRecording(t *testing.T, dbPath, name string, data []byte) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(dbPath), "meeting-recordings")
	require.NoError(t, os.MkdirAll(dir, 0700))
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0600))
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(path, old, old))
	return path
}

func TestImportRecordingOptInLateArrivalAndReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newImporterFixture(t)
	path := filepath.Join(filepath.Dir(f.path), "meeting-recordings", "private-title.m4a")
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"saved_recording_path": path}))
	opts := ImportOptions{Recordings: true, AttachmentsDir: t.TempDir()}
	_, err := f.run(t, opts)
	require.NoError(err, "missing audio must not fail transcript sync")
	writeRecording(t, f.path, "private-title.m4a", recordingWAV())
	_, err = f.run(t, ImportOptions{})
	require.NoError(err)
	candidates, err := f.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	assert.Empty(candidates, "recordings are off by default")
	for range 2 {
		_, err = f.run(t, opts)
		require.NoError(err)
	}
	candidates, err = f.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal("audio/wav", candidates[0].MIMEType, "detect bytes rather than extension")
	assert.Equal("audio", candidates[0].MediaType)
	assert.NotContains(candidates[0].Filename, "private-title")
	firstHash := candidates[0].ContentHash
	data := recordingWAV()
	data[len(data)-1] = 1
	writeRecording(t, f.path, "private-title.m4a", data)
	_, err = f.run(t, opts)
	require.NoError(err)
	candidates, err = f.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.NotEqual(firstHash, candidates[0].ContentHash)
	require.NoError(os.Remove(path))
	_, err = f.run(t, opts)
	require.NoError(err)
	after, err := f.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	assert.Equal(candidates, after, "deleting source audio preserves archived bytes")
}

func TestImportRecordingThroughDatabaseSymlink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	path := filepath.Join(filepath.Dir(f.path), "meeting-recordings", "meeting.wav")
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"saved_recording_path": path}))
	writeRecording(t, f.path, "meeting.wav", recordingWAV())

	databaseAlias := filepath.Join(t.TempDir(), "muesli.db")
	require.NoError(os.Symlink(f.path, databaseAlias))
	require.NoError(f.muesli.Close())
	recording, err := PrepareRecording(t.Context(), databaseAlias, path, 0)
	require.NoError(err)
	require.NotNil(recording, "the recording path must be confined beside the resolved database")
	require.NoError(recording.Close())
	summary, err := f.run(t, ImportOptions{DBPath: databaseAlias, Recordings: true, AttachmentsDir: t.TempDir()})
	require.NoError(err)
	require.Equal(int64(1), summary.MeetingsProcessed)

	candidates, err := f.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(candidates, 1, "recordings must resolve beside the database target")
	assert.Equal("audio/wav", candidates[0].MIMEType)
}

func TestPrepareRecordingDefersUnsafeAndUnfinishedFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dbPath := filepath.Join(t.TempDir(), "muesli.db")
	path := writeRecording(t, dbPath, "meeting.wav", recordingWAV())
	recent := time.Now().Add(24 * time.Hour) // a future timestamp keeps the quiet-period test independent of scheduling
	require.NoError(os.Chtimes(path, recent, recent))
	recording, err := PrepareRecording(t.Context(), dbPath, path, 0)
	require.NoError(err)
	assert.Nil(recording)
	old := time.Now().Add(-time.Minute)
	require.NoError(os.Chtimes(path, old, old))
	recording, err = PrepareRecording(t.Context(), dbPath, path, 12)
	require.ErrorIs(err, ErrRecordingTooLarge)
	assert.Nil(recording)
	outside := filepath.Join(filepath.Dir(dbPath), "outside.wav")
	require.NoError(os.WriteFile(outside, recordingWAV(), 0600))
	old = time.Now().Add(-time.Minute)
	require.NoError(os.Chtimes(outside, old, old))
	for _, target := range []string{outside, "../outside.wav", filepath.Dir(path)} {
		recording, err = PrepareRecording(t.Context(), dbPath, target, 0)
		require.NoError(err)
		assert.Nil(recording)
	}
	link := filepath.Join(filepath.Dir(path), "link.wav")
	require.NoError(os.Symlink(path, link))
	recording, err = PrepareRecording(t.Context(), dbPath, link, 0)
	require.NoError(err)
	assert.Nil(recording)
}

func TestScanRemoteRecordingLateArrivalAndLostAcknowledgement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	path := filepath.Join(filepath.Dir(f.path), "meeting-recordings", "meeting.wav")
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"saved_recording_path": path}))
	dir := t.TempDir()
	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir(), Recordings: true}
	_, err := ScanRemote(t.Context(), opts, f.imp.ImportRemote)
	require.NoError(err)
	writeRecording(t, f.path, "meeting.wav", recordingWAV())
	attempts := 0
	opts.SendRecording = func(ctx context.Context, upload RecordingUpload, recording *Recording) error {
		attempts++
		_, err := f.imp.ImportRecording(ctx, upload, dir, io.NewSectionReader(recording.File, 0, recording.Size), 0)
		if err != nil {
			return err
		}
		if attempts == 1 {
			return errors.New("lost acknowledgement")
		}
		return nil
	}
	_, err = ScanRemote(t.Context(), opts, f.imp.ImportRemote)
	require.ErrorContains(err, "lost acknowledgement")
	for range 2 {
		_, err = ScanRemote(t.Context(), opts, f.imp.ImportRemote)
		require.NoError(err)
	}
	assert.Equal(2, attempts, "acknowledged audio is not retransmitted")
	candidates, err := f.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	assert.Len(candidates, 1)
}

func TestScanRemoteReoffersContactReviewWithoutResendingRecording(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newResolveFixture(t,
		fixtureCard{uniqueID: "CARD-A:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+16045550100"}},
		fixtureCard{uniqueID: "CARD-B:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+16045550101"}},
	)
	registerRemoteFixture(t, f.importerFixture)
	path := filepath.Join(filepath.Dir(f.path), "meeting-recordings", "meeting.wav")
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"saved_recording_path": path}))
	insertRow(t, f.muesli, "meeting_participants", map[string]any{
		"meeting_id": meetingID, "participant_identifier": "email:attendee@example.com",
		"email_address": "attendee@example.com", "display_name": "Meeting Attendee",
		"source": "calendar", "insertion_order": 0,
	})
	writeRecording(t, f.path, "meeting.wav", recordingWAV())

	opts := ImportOptions{
		Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path,
		LockDir: t.TempDir(), ContactsEnabled: true, ContactsPath: f.contacts, Recordings: true,
	}
	uploads := 0
	dir := t.TempDir()
	opts.SendRecording = func(ctx context.Context, upload RecordingUpload, recording *Recording) error {
		uploads++
		_, err := f.imp.ImportRecording(ctx, upload, dir, io.NewSectionReader(recording.File, 0, recording.Size), 0)
		return err
	}
	requests := 0
	send := func(ctx context.Context, request RemoteRequest) (RemoteResult, error) {
		requests++
		assert.True(hasContactReview(request.Meeting), "the source continues to offer contact-review suggestions")
		return f.imp.ImportRemote(ctx, request)
	}
	for range 2 {
		_, err := ScanRemote(t.Context(), opts, send)
		require.NoError(err)
	}
	assert.Equal(2, requests, "contact-review meetings are reoffered")
	assert.Equal(1, uploads, "acknowledged audio is not retransmitted with the JSON")
}

func TestScanRemoteRecordingNotResentAfterTranscriptEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	path := filepath.Join(filepath.Dir(f.path), "meeting-recordings", "meeting.wav")
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"saved_recording_path": path}))
	writeRecording(t, f.path, "meeting.wav", recordingWAV())

	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir(), Recordings: true}
	uploads := 0
	dir := t.TempDir()
	opts.SendRecording = func(ctx context.Context, upload RecordingUpload, recording *Recording) error {
		uploads++
		_, err := f.imp.ImportRecording(ctx, upload, dir, io.NewSectionReader(recording.File, 0, recording.Size), 0)
		return err
	}
	requests := 0
	send := func(ctx context.Context, request RemoteRequest) (RemoteResult, error) {
		requests++
		return f.imp.ImportRemote(ctx, request)
	}
	_, err := ScanRemote(t.Context(), opts, send)
	require.NoError(err)
	_, err = f.muesli.Exec(`UPDATE meetings SET raw_transcript = 'Edited synthetic transcript' WHERE id = ?`, meetingID)
	require.NoError(err)
	_, err = ScanRemote(t.Context(), opts, send)
	require.NoError(err)
	assert.Equal(2, requests, "the changed transcript is reoffered")
	assert.Equal(1, uploads, "unchanged acknowledged audio is not retransmitted")
}

func TestScanRemoteRecordingAcknowledgementUsesMeetingIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	path := filepath.Join(filepath.Dir(f.path), "meeting-recordings", "meeting.wav")
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"saved_recording_path": path}))
	writeRecording(t, f.path, "meeting.wav", recordingWAV())

	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir(), Recordings: true}
	uploads := 0
	dir := t.TempDir()
	opts.SendRecording = func(ctx context.Context, upload RecordingUpload, recording *Recording) error {
		uploads++
		_, err := f.imp.ImportRecording(ctx, upload, dir, io.NewSectionReader(recording.File, 0, recording.Size), 0)
		return err
	}
	requests := 0
	send := func(ctx context.Context, request RemoteRequest) (RemoteResult, error) {
		requests++
		return f.imp.ImportRemote(ctx, request)
	}
	_, err := ScanRemote(t.Context(), opts, send)
	require.NoError(err)

	_, err = f.muesli.Exec(`DELETE FROM meetings WHERE id = ?`, meetingID)
	require.NoError(err)
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{
		"id": meetingID, "created_at": "2026-09-02 14:00:03", "saved_recording_path": path,
	}))
	seeded := seedRemoteMeetingWithoutRecording(t, f.imp, f.path, meetingID)
	assert.Equal("created", seeded.Status)
	_, err = ScanRemote(t.Context(), opts, send)
	require.NoError(err)

	assert.Equal(2, requests, "the replacement transcript is reoffered")
	assert.Equal(2, uploads, "a replacement meeting identity needs audio despite the old receipt")
}

func seedRemoteMeetingWithoutRecording(t *testing.T, imp *Importer, dbPath string, meetingID int64) RemoteResult {
	t.Helper()
	require := require.New(t)
	reader, err := Open(t.Context(), dbPath)
	require.NoError(err)
	meetings, err := reader.listMeetings(t.Context(), meetingID)
	require.NoError(err)
	require.NoError(reader.Close())
	require.Len(meetings, 1)

	request := RemoteRequest{Action: "upsert", Source: remoteTestRequest().Source, Meeting: NewRemoteMeeting(meetings[0])}
	request, err = request.Normalize()
	require.NoError(err)
	result, err := imp.ImportRemote(t.Context(), request)
	require.NoError(err)
	return result
}

func TestScanRemoteRetriesRecordingAfterLostCreatedResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	path := filepath.Join(filepath.Dir(f.path), "meeting-recordings", "meeting.wav")
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"saved_recording_path": path}))
	writeRecording(t, f.path, "meeting.wav", recordingWAV())

	opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir(), Recordings: true}
	activeImporter := f.imp
	uploads := 0
	dir := t.TempDir()
	opts.SendRecording = func(ctx context.Context, upload RecordingUpload, recording *Recording) error {
		uploads++
		_, err := activeImporter.ImportRecording(ctx, upload, dir, io.NewSectionReader(recording.File, 0, recording.Size), 0)
		return err
	}
	_, err := ScanRemote(t.Context(), opts, f.imp.ImportRemote)
	require.NoError(err)
	require.Equal(1, uploads)

	restored := newImporterFixture(t)
	registerRemoteFixture(t, restored)
	activeImporter = restored.imp
	_, err = f.muesli.Exec(`UPDATE meetings SET raw_transcript = 'Edited synthetic transcript' WHERE id = ?`, meetingID)
	require.NoError(err)
	lostResponse := errors.New("synthetic lost creation response")
	_, err = ScanRemote(t.Context(), opts, func(ctx context.Context, request RemoteRequest) (RemoteResult, error) {
		result, err := restored.imp.ImportRemote(ctx, request)
		require.NoError(err)
		require.Equal("created", result.Status)
		return RemoteResult{}, lostResponse
	})
	require.ErrorIs(err, lostResponse)
	require.Equal(1, uploads, "the recording is not sent before the meeting response is acknowledged")

	_, err = ScanRemote(t.Context(), opts, restored.imp.ImportRemote)
	require.NoError(err)
	assert.Equal(2, uploads, "an indeterminate create must not leave a receipt that suppresses recovery audio")
	candidates, err := restored.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	assert.Len(candidates, 1)
}

func TestScanRemoteRestoresRecordingAfterArchiveBackupRollback(t *testing.T) {
	for _, tt := range []struct {
		name               string
		full               bool
		editTranscript     bool
		seedExisting       bool
		deferRecording     bool
		recordingsDisabled bool
	}{
		{name: "full scan with existing restored message", full: true, seedExisting: true},
		{name: "full scan with existing message and deferred recording", full: true, seedExisting: true, deferRecording: true},
		{name: "newly created after transcript edit", editTranscript: true},
		{name: "created while recording is deferred", deferRecording: true},
		{name: "created while recordings are disabled", recordingsDisabled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newImporterFixture(t)
			registerRemoteFixture(t, f)
			path := filepath.Join(filepath.Dir(f.path), "meeting-recordings", "meeting.wav")
			meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"saved_recording_path": path}))
			writeRecording(t, f.path, "meeting.wav", recordingWAV())

			opts := ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, LockDir: t.TempDir(), Recordings: true}
			activeImporter := f.imp
			uploads := 0
			dir := t.TempDir()
			opts.SendRecording = func(ctx context.Context, upload RecordingUpload, recording *Recording) error {
				uploads++
				_, err := activeImporter.ImportRecording(ctx, upload, dir, io.NewSectionReader(recording.File, 0, recording.Size), 0)
				return err
			}
			_, err := ScanRemote(t.Context(), opts, f.imp.ImportRemote)
			require.NoError(err)
			require.Equal(1, uploads)

			// Model restoring a daemon backup: the recorder's cache survives
			// while the archive state is set independently for each case.
			restored := newImporterFixture(t)
			registerRemoteFixture(t, restored)
			activeImporter = restored.imp
			if tt.seedExisting {
				seeded := seedRemoteMeetingWithoutRecording(t, restored.imp, f.path, meetingID)
				require.Equal("created", seeded.Status)
			}
			if tt.editTranscript {
				_, err := f.muesli.Exec(`UPDATE meetings SET raw_transcript = 'Edited synthetic transcript' WHERE id = ?`, meetingID)
				require.NoError(err)
			}
			if tt.deferRecording {
				require.NoError(os.Remove(path))
			}
			recoveryOpts := opts
			recoveryOpts.Full = tt.full
			if tt.recordingsDisabled {
				recoveryOpts.Recordings = false
			}
			_, err = ScanRemote(t.Context(), recoveryOpts, restored.imp.ImportRemote)
			require.NoError(err)
			if tt.deferRecording {
				assert.Equal(1, uploads, "a deferred recording is not uploaded")
				writeRecording(t, f.path, "meeting.wav", recordingWAV())
				recoveryOpts.Full = false
				_, err = ScanRemote(t.Context(), recoveryOpts, restored.imp.ImportRemote)
				require.NoError(err)
			}
			if tt.recordingsDisabled {
				recoveryOpts.Recordings = true
				_, err = ScanRemote(t.Context(), recoveryOpts, restored.imp.ImportRemote)
				require.NoError(err)
			}
			assert.Equal(2, uploads, "a newly created archive message needs its recording even when the recorder has a receipt")
			candidates, err := restored.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
			require.NoError(err)
			assert.Len(candidates, 1)
		})
	}
}

func TestImportRecordingRejectsSourceMismatchAndTruncatedUpload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	result, err := f.imp.ImportRemote(t.Context(), remoteTestRequest())
	require.NoError(err)
	data := recordingWAV()
	hash := sha256.Sum256(data)
	upload := RecordingUpload{Source: remoteTestRequest().Source, MessageID: result.MessageID, MIMEType: "audio/wav", Size: int64(len(data)), Hash: hex.EncodeToString(hash[:])}
	dir := t.TempDir()
	wrong := upload
	wrong.Source.AccountEmail = "other@example.com"
	_, err = f.imp.ImportRecording(t.Context(), wrong, dir, bytes.NewReader(data), 0)
	require.ErrorIs(err, ErrRemoteValidation)
	for _, body := range [][]byte{data[:len(data)-1], append(append([]byte{}, data...), 0)} {
		_, err = f.imp.ImportRecording(t.Context(), upload, dir, bytes.NewReader(body), 0)
		require.Error(err)
	}
	candidates, err := f.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	assert.Empty(candidates)
	_, err = f.imp.ImportRecording(t.Context(), upload, dir, bytes.NewReader(data), int64(len(data)-1))
	require.ErrorIs(err, ErrRecordingTooLarge)
	changed, err := f.imp.ImportRecording(t.Context(), upload, dir, bytes.NewReader(data), 0)
	require.NoError(err)
	assert.True(changed)
	changed, err = f.imp.ImportRecording(t.Context(), upload, dir, bytes.NewReader(data), 0)
	require.NoError(err)
	assert.False(changed)
}

func TestStageRecordingUploadValidatesAndSnapshotsBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	data := recordingWAV()
	digest := sha256.Sum256(data)
	upload := RecordingUpload{
		Source: remoteTestRequest().Source, MessageID: 1,
		Hash: hex.EncodeToString(digest[:]), Size: int64(len(data)), MIMEType: "audio/wav",
	}
	wrongHash := upload
	wrongHash.Hash = strings.Repeat("0", 64)
	wrongMediaType := upload
	wrongMediaType.MIMEType = "audio/mp4"
	for _, tt := range []struct {
		name    string
		upload  RecordingUpload
		body    []byte
		maxSize int64
		wantErr error
	}{
		{name: "valid", upload: upload, body: data},
		{name: "hash mismatch", upload: wrongHash, body: data, wantErr: ErrRecordingChanged},
		{name: "short body", upload: upload, body: data[:len(data)-1], wantErr: ErrRecordingChanged},
		{name: "wrong media type", upload: wrongMediaType, body: data, wantErr: ErrRemoteValidation},
		{name: "over cap", upload: upload, body: data, maxSize: int64(len(data) - 1), wantErr: ErrRecordingTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			staged, err := StageRecordingUpload(t.Context(), tt.upload, bytes.NewReader(tt.body), tt.maxSize)
			if tt.wantErr != nil {
				require.ErrorIs(err, tt.wantErr)
				assert.Nil(staged)
				return
			}
			require.NoError(err)
			require.NotNil(staged)
			t.Cleanup(func() { _ = staged.Close() })
			assert.Equal(tt.upload.Hash, staged.Hash)
			assert.Equal(tt.upload.Size, staged.Size)
			assert.Equal(tt.upload.MIMEType, staged.MIMEType)
			got, err := io.ReadAll(staged.File)
			require.NoError(err)
			assert.Equal(tt.body, got)
		})
	}
}

// Synthetic silence generated with ffmpeg's lavfi anullsrc at 16 kHz, mono,
// encoded as AAC for 0.1 seconds with metadata removed and bitexact flags.
//
//go:embed testdata/synthetic.m4a
var syntheticM4A []byte

func recordingM4A() []byte { return append([]byte{}, syntheticM4A...) }

func TestRecordingM4ADetectionAndIncompleteContainers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "muesli.db")
	data := recordingM4A()
	compatible := append([]byte{}, data...)
	copy(compatible[8:12], "isom")
	for _, tt := range []struct {
		name      string
		data      []byte
		available bool
	}{
		{"m4a", data, true}, {"compatible m4a", compatible, true}, {"short atom", data[:len(data)-1], false}, {"unfinished wav", recordingWAV()[:46], false}, {"unrelated", []byte("arbitrary non-audio file"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			path := writeRecording(t, dbPath, "recording.wav", tt.data)
			recording, err := PrepareRecording(t.Context(), dbPath, path, 0)
			require.NoError(err)
			if tt.available {
				require.NotNil(recording)
				assert.Equal("audio/mp4", recording.MIMEType)
				require.NoError(recording.Close())
			} else {
				assert.Nil(recording)
			}
		})
	}
}

func TestRecordingDefaultCapRejectsSparseFileAboveFourGiB(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dbPath := filepath.Join(t.TempDir(), "muesli.db")
	path := writeRecording(t, dbPath, "large.wav", recordingWAV())
	file, err := os.OpenFile(path, os.O_WRONLY, 0600)
	require.NoError(err)
	require.NoError(file.Truncate(DefaultRecordingMaxBytes + 1))
	require.NoError(file.Close())
	old := time.Now().Add(-time.Minute)
	require.NoError(os.Chtimes(path, old, old))
	recording, err := PrepareRecording(t.Context(), dbPath, path, 0)
	require.ErrorIs(err, ErrRecordingTooLarge)
	assert.Nil(recording)
}

func TestImportRecordingM4AUsesOrdinaryAudioAttachment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newImporterFixture(t)
	path := writeRecording(t, f.path, "recording.wav", recordingM4A())
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"saved_recording_path": path}))
	dir := t.TempDir()
	for range 2 {
		_, err := f.run(t, ImportOptions{Recordings: true, AttachmentsDir: dir})
		require.NoError(err)
	}
	candidates, err := f.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal("audio/mp4", candidates[0].MIMEType)
	assert.Equal("audio", candidates[0].MediaType)
	assert.Equal("standalone", candidates[0].Role)
	raw, err := f.st.GetMessageRawContext(t.Context(), candidates[0].MessageID)
	require.NoError(err)
	assert.NotContains(string(raw), path)
	assert.NotContains(string(raw), "saved_recording_path")
	stored, err := os.ReadFile(filepath.Join(dir, candidates[0].ContentHash[:2], candidates[0].ContentHash))
	require.NoError(err)
	assert.Equal(recordingM4A(), stored)
}
