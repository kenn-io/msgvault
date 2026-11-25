package hex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRecordingUploadValidationAndSourceMembership(t *testing.T) {
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
		name    string
		mutate  func(*RecordingUpload)
		data    []byte
		cap     int64
		wantErr error
	}{
		{"valid", func(*RecordingUpload) {}, data, 0, nil},
		{"track", func(u *RecordingUpload) { u.Track = "../../escape" }, data, 0, ErrRemoteRecordValidation},
		{"media", func(u *RecordingUpload) { u.MIMEType = "audio/mp4" }, data, 0, ErrRemoteRecordValidation},
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
				must.ErrorIs(err, tt.wantErr)
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
}
