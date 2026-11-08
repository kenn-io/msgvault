package meetingarchive

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRecordingM4ADetectionAndIncompleteContainers(t *testing.T) {
	data, err := os.ReadFile("../muesli/testdata/synthetic.m4a")
	require.NoError(t, err)
	compatible := append([]byte{}, data...)
	copy(compatible[8:12], "isom")
	for _, tc := range []struct {
		name      string
		data      []byte
		available bool
		mime      string
		immutable bool
	}{
		{"m4a", data, true, "audio/mp4", false},
		{"compatible m4a", compatible, true, "audio/mp4", false},
		{"short atom", data[:len(data)-1], false, "", false},
		{"unfinished wav", testutil.RecordingWAV()[:46], false, "", false},
		{"unrelated", []byte("arbitrary non-audio file"), false, "", false},
		{"immutable wav snapshot", testutil.RecordingWAV(), true, "audio/wav", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must, check := require.New(t), assert.New(t)
			base := t.TempDir()
			path := filepath.Join(base, "recording.wav")
			must.NoError(os.WriteFile(path, tc.data, 0600))
			old := time.Now().Add(-time.Minute)
			must.NoError(os.Chtimes(path, old, old))
			recording, err := PrepareRecording(t.Context(), base, path, 0)
			must.NoError(err)
			if !tc.available {
				check.Nil(recording)
				return
			}
			must.NotNil(recording)
			t.Cleanup(func() { must.NoError(recording.Close()) })
			check.Equal(tc.mime, recording.MIMEType)
			if tc.immutable {
				must.NoError(os.WriteFile(path, []byte("changed source"), 0600))
				got, err := io.ReadAll(recording.File)
				must.NoError(err)
				check.Equal(tc.data, got, "the immutable snapshot survives later source writes")
			}
		})
	}
}

func TestRecordingDefaultCapRejectsSparseFileAboveFourGiB(t *testing.T) {
	must, check := require.New(t), assert.New(t)
	base := t.TempDir()
	path := filepath.Join(base, "large.wav")
	must.NoError(os.WriteFile(path, testutil.RecordingWAV(), 0600))
	file, err := os.OpenFile(path, os.O_WRONLY, 0600)
	must.NoError(err)
	must.NoError(file.Truncate(DefaultRecordingMaxBytes + 1))
	must.NoError(file.Close())
	old := time.Now().Add(-time.Minute)
	must.NoError(os.Chtimes(path, old, old))
	recording, err := PrepareRecording(t.Context(), base, path, 0)
	must.ErrorIs(err, ErrRecordingTooLarge)
	check.Nil(recording)
}
