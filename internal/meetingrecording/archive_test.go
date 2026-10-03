package meetingrecording

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreAudioPublishesVerifiedCAS(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	root := t.TempDir()
	wav := []byte("RIFF\x24\x00\x00\x00WAVEfmt synthetic recording")
	blob, err := StoreAudio(t.Context(), root, bytes.NewReader(wav), 100)
	require.NoError(err)
	assert.Equal(int64(len(wav)), blob.Size)
	stored, err := os.ReadFile(filepath.Join(root, blob.Path))
	require.NoError(err)
	assert.Equal(wav, stored)
	again, err := StoreAudio(t.Context(), root, bytes.NewReader(wav), 100)
	require.NoError(err)
	assert.Equal(blob, again)
}
func TestStoreAudioRejectsErrorsAndOverLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		cap  int64
		want error
	}{
		{"json", []byte(`{"error":"unavailable"}`), 100, ErrNotAudio},
		{"html", []byte("<html>failure</html>"), 100, ErrNotAudio},
		{"cap", []byte("RIFF\x24\x00\x00\x00WAVEfmt too long"), 15, ErrSizeLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			root := t.TempDir()
			_, err := StoreAudio(t.Context(), root, bytes.NewReader(tc.body), tc.cap)
			require.ErrorIs(err, tc.want)
			entries, e := os.ReadDir(root)
			require.NoError(e)
			for _, entry := range entries {
				assert.NotEqual(2, len(entry.Name()), "no published hash directory")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := StoreAudio(ctx, t.TempDir(), bytes.NewReader([]byte("ID3 synthetic mp3")), 100)
	assert.ErrorIs(t, err, context.Canceled)
}
