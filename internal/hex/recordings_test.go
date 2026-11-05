package hex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRecordingConfinementAndImmutableSnapshot(t *testing.T) {
	_, _, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "audio", "complete")
	m := Meeting{Manifest: Manifest{ID: "audio"}}
	writeTrack(t, dir, "system")
	r, err := prepareTrack(t.Context(), opts, m, "system")
	require.NoError(t, err)
	require.NotNil(t, r)
	defer func() { _ = r.Close() }()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "system.wav"), []byte("changed source"), 0600))
	data, err := io.ReadAll(r.File)
	require.NoError(t, err)
	assert.Equal(t, testutil.RecordingWAV(), data)
	// A symlink cannot redirect retained audio out of the meeting root.
	outside := filepath.Join(t.TempDir(), "outside.wav")
	require.NoError(t, os.WriteFile(outside, testutil.RecordingWAV(), 0600))
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(outside, old, old))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "microphone.wav")))
	deferred, err := prepareTrack(t.Context(), opts, m, "microphone")
	require.NoError(t, err)
	assert.Nil(t, deferred)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "system.wav"), testutil.RecordingWAV(), 0600))
	future := time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "system.wav"), future, future))
	deferred, err = prepareTrack(t.Context(), opts, m, "system")
	require.NoError(t, err)
	assert.Nil(t, deferred, "recent audio waits")
}
func TestRecordingUploadValidationAndSourceMembership(t *testing.T) {
	st, imp, opts := importFixture(t)
	fixtureMeeting(t, opts.MeetingsDir, "upload", "failed")
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(t, err)
	id := messageID(t, st, sum.SourceID, "upload")
	data := testutil.RecordingWAV()
	digest := sha256.Sum256(data)
	upload := RecordingUpload{Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}, MessageID: id, Track: "system", Hash: hex.EncodeToString(digest[:]), Size: int64(len(data)), MIMEType: "audio/wav"}
	for _, tt := range []struct {
		name   string
		mutate func(*RecordingUpload)
		data   []byte
		cap    int64
	}{
		{"valid", func(*RecordingUpload) {}, data, 0},
		{"truncated", func(*RecordingUpload) {}, data[:len(data)-1], 0},
		{"hash", func(u *RecordingUpload) { u.Hash = strings.Repeat("0", 64) }, data, 0},
		{"track", func(u *RecordingUpload) { u.Track = "../../escape" }, data, 0},
		{"media", func(u *RecordingUpload) { u.MIMEType = "audio/mp4" }, data, 0},
		{"cap", func(*RecordingUpload) {}, data, 8},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := upload
			tt.mutate(&u)
			r, err := StageRecordingUpload(t.Context(), u, bytes.NewReader(tt.data), tt.cap)
			if tt.name == "valid" {
				require.NoError(t, err)
				require.NotNil(t, r)
				require.NoError(t, r.Close())
			} else {
				require.Error(t, err)
				assert.Nil(t, r)
			}
		})
	}
	wrong := upload
	wrong.Source.AccountEmail = "other@example.com"
	_, err = imp.ImportRecording(t.Context(), wrong, opts.AttachmentsDir, bytes.NewReader(data), 0)
	assert.ErrorIs(t, err, ErrRemoteValidation)
	other, err := st.GetOrCreateSource(SourceType, "other")
	require.NoError(t, err)
	require.NoError(t, st.AddAccountIdentityContext(t.Context(), other.ID, opts.AccountEmail, "account-email"))
	wrong = upload
	wrong.Source.Identifier = "other"
	_, err = imp.ImportRecording(t.Context(), wrong, opts.AttachmentsDir, bytes.NewReader(data), 0)
	assert.ErrorIs(t, err, ErrRemoteRecordValidation)
	_, err = imp.ImportRecording(t.Context(), upload, opts.AttachmentsDir, bytes.NewReader(data[:len(data)-1]), 0)
	require.Error(t, err)
	refs, err := st.MessageAttachmentsByPartKeyContext(t.Context(), id, "hex:system")
	require.NoError(t, err)
	assert.Empty(t, refs, "truncated audio publishes no attachment")
}

func TestHeaderOnlyTracksWaitForSamples(t *testing.T) {
	st, imp, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "empty-tracks", "complete")
	fixtureTranscript(t, dir)
	empty := testutil.RecordingWAV()[:44]
	binary.LittleEndian.PutUint32(empty[4:], 36)
	binary.LittleEndian.PutUint32(empty[40:], 0)
	for _, track := range []string{"system", "microphone"} {
		path := filepath.Join(dir, track+".wav")
		require.NoError(t, os.WriteFile(path, empty, 0600))
		old := time.Now().Add(-time.Minute)
		require.NoError(t, os.Chtimes(path, old, old))
	}
	opts.Recordings = true
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(t, err)
	id := messageID(t, st, sum.SourceID, "empty-tracks")
	message, err := st.GetMessage(id)
	require.NoError(t, err)
	assert.Empty(t, message.Attachments)
	assert.Equal(t, int64(2), sum.RecordingsSkipped)
	_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}})
	require.NoError(t, err)
	sum, err = ScanRemote(t.Context(), opts, imp.ImportRemote)
	require.NoError(t, err, "empty tracks need no recording transport")
	assert.Equal(t, int64(2), sum.RecordingsSkipped)
	writeTrack(t, dir, "system")
	writeTrack(t, dir, "microphone")
	_, err = imp.Import(t.Context(), opts)
	require.NoError(t, err)
	message, err = st.GetMessage(id)
	require.NoError(t, err)
	assert.Len(t, message.Attachments, 2, "identical populated tracks remain separate")
}
