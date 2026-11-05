package hex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
)

func TestMissingRootAndHiddenFolders(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	_, imp, opts := importFixture(t)
	opts.MeetingsDir = filepath.Join(t.TempDir(), "not-created", "meetings")
	reader, err := Open(t.Context(), opts.MeetingsDir)
	must.NoError(err)
	scan, err := reader.ListMeetings(t.Context())
	must.NoError(err)
	check.Empty(scan.Meetings)
	check.Zero(scan.Warnings)
	must.NoError(reader.Close())
	must.NoError(ForgetRemoteUploads(t.Context(), opts))
	sum, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Zero(sum.MeetingsProcessed)
	sum, err = ScanRemote(t.Context(), opts, nil)
	must.NoError(err)
	check.Zero(sum.MeetingsProcessed)
	_, err = os.Stat(opts.MeetingsDir)
	must.ErrorIs(err, os.ErrNotExist, "setup and scans never create Hex storage")
	must.NoError(os.MkdirAll(filepath.Join(opts.MeetingsDir, ".hidden"), 0700))
	dir := fixtureMeeting(t, opts.MeetingsDir, "visible", "complete")
	fixtureTranscript(t, dir)
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.MeetingsAdded, "a later scan sees the root Hex creates")
	check.Zero(sum.Warnings, "hidden folders are ignored without reading their contents")
}

func TestRetainedRemoteReceiptKeepsRecordingRetries(t *testing.T) {
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
}
