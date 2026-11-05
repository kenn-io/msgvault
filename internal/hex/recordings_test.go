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
	check := assert.New(t)
	must := require.New(t)

	_, _, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "audio", "complete")
	m := Meeting{Manifest: Manifest{ID: "audio"}}
	writeTrack(t, dir, "system")
	r, err := prepareTrack(t.Context(), opts, m, "system")
	must.NoError(err)
	must.NotNil(r)
	defer func() { _ = r.Close() }()
	must.NoError(os.WriteFile(filepath.Join(dir, "system.wav"), []byte("changed source"), 0600))
	data, err := io.ReadAll(r.File)
	must.NoError(err)
	check.Equal(testutil.RecordingWAV(), data)
	// A symlink cannot redirect retained audio out of the meeting root.
	outside := filepath.Join(t.TempDir(), "outside.wav")
	must.NoError(os.WriteFile(outside, testutil.RecordingWAV(), 0600))
	old := time.Now().Add(-time.Minute)
	must.NoError(os.Chtimes(outside, old, old))
	must.NoError(os.Symlink(outside, filepath.Join(dir, "microphone.wav")))
	deferred, err := prepareTrack(t.Context(), opts, m, "microphone")
	must.NoError(err)
	check.Nil(deferred)
	must.NoError(os.WriteFile(filepath.Join(dir, "system.wav"), testutil.RecordingWAV(), 0600))
	future := time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)
	must.NoError(os.Chtimes(filepath.Join(dir, "system.wav"), future, future))
	deferred, err = prepareTrack(t.Context(), opts, m, "system")
	must.NoError(err)
	check.Nil(deferred, "recent audio waits")
}
func TestRecordingUploadValidationAndSourceMembership(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	st, imp, opts := importFixture(t)
	fixtureMeeting(t, opts.MeetingsDir, "upload", "failed")
	sum, err := imp.Import(t.Context(), opts)
	must.NoError(err)
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
			must := require.New(t)

			u := upload
			tt.mutate(&u)
			r, err := StageRecordingUpload(t.Context(), u, bytes.NewReader(tt.data), tt.cap)
			if tt.name == "valid" {
				must.NoError(err)
				must.NotNil(r)
				must.NoError(r.Close())
			} else {
				must.Error(err)
				assert.Nil(t, r)
			}
		})
	}
	wrong := upload
	wrong.Source.AccountEmail = "other@example.com"
	_, err = imp.ImportRecording(t.Context(), wrong, opts.AttachmentsDir, bytes.NewReader(data), 0)
	must.ErrorIs(err, ErrRemoteValidation)
	other, err := st.GetOrCreateSource(SourceType, "other")
	must.NoError(err)
	must.NoError(st.AddAccountIdentityContext(t.Context(), other.ID, opts.AccountEmail, "account-email"))
	wrong = upload
	wrong.Source.Identifier = "other"
	_, err = imp.ImportRecording(t.Context(), wrong, opts.AttachmentsDir, bytes.NewReader(data), 0)
	must.ErrorIs(err, ErrRemoteRecordValidation)
	_, err = imp.ImportRecording(t.Context(), upload, opts.AttachmentsDir, bytes.NewReader(data[:len(data)-1]), 0)
	must.Error(err)
	refs, err := st.MessageAttachmentsByPartKeyContext(t.Context(), id, "hex:system")
	must.NoError(err)
	check.Empty(refs, "truncated audio publishes no attachment")
}

func TestHeaderOnlyTracksWaitForSamples(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	st, imp, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "empty-tracks", "complete")
	fixtureTranscript(t, dir)
	empty := testutil.RecordingWAV()[:44]
	binary.LittleEndian.PutUint32(empty[4:], 36)
	binary.LittleEndian.PutUint32(empty[40:], 0)
	for _, track := range []string{"system", "microphone"} {
		path := filepath.Join(dir, track+".wav")
		must.NoError(os.WriteFile(path, empty, 0600))
		old := time.Now().Add(-time.Minute)
		must.NoError(os.Chtimes(path, old, old))
	}
	opts.Recordings = true
	sum, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	id := messageID(t, st, sum.SourceID, "empty-tracks")
	message, err := st.GetMessage(id)
	must.NoError(err)
	check.Empty(message.Attachments)
	check.Equal(int64(2), sum.RecordingsSkipped)
	_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}})
	must.NoError(err)
	sum, err = ScanRemote(t.Context(), opts, imp.ImportRemote)
	must.NoError(err, "empty tracks need no recording transport")
	check.Equal(int64(2), sum.RecordingsSkipped)
	writeTrack(t, dir, "system")
	writeTrack(t, dir, "microphone")
	_, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	message, err = st.GetMessage(id)
	must.NoError(err)
	check.Len(message.Attachments, 2, "identical populated tracks remain separate")
}

func TestUnfinalizedWAVHeaderDefersTrack(t *testing.T) {
	must := require.New(t)
	_, _, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "unfinished-audio", "interrupted")
	writeTrack(t, dir, "system")
	path := filepath.Join(dir, "system.wav")
	data, err := os.ReadFile(path)
	must.NoError(err)
	binary.LittleEndian.PutUint32(data[4:], 36) // Header was not updated for the saved samples.
	must.NoError(os.WriteFile(path, data, 0600))
	old := time.Now().Add(-time.Minute)
	must.NoError(os.Chtimes(path, old, old))
	recording, err := prepareTrack(t.Context(), opts, Meeting{Manifest: Manifest{ID: "unfinished-audio"}}, "system")
	must.NoError(err)
	assert.Nil(t, recording, "an inconsistent RIFF size is deferred, not repaired")
}
