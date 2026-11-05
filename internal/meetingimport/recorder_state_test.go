package meetingimport

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestRecorderUploadsMigratesIDOnlyRecordingAcknowledgements(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "uploads.json")
	legacy := `{"version":1,"meetings":{"1":"meeting-digest:recording-hash"},"recordings":{"1":"recording-hash"}}`
	require.NoError(os.WriteFile(path, []byte(legacy), 0600))

	uploads, err := LoadRecorderUploads[int64](path)
	require.NoError(err)
	assert.True(uploads.Dirty)
	assert.Equal("meeting-digest:recording-hash", uploads.Meetings[1], "meeting acknowledgements remain usable")
	assert.False(uploads.RecordingAcknowledged("meeting:1:20260901T140003Z", "recording-hash"), "row-ID-only recording acknowledgements are ambiguous")
	require.NoError(uploads.Save())

	data, err := os.ReadFile(path)
	require.NoError(err)
	var migrated RecorderUploads[int64]
	require.NoError(json.Unmarshal(data, &migrated))
	assert.Equal(2, migrated.Version)
	assert.Empty(migrated.Recordings)
}

func TestRecorderUploadsRefusesFutureVersionOnReset(t *testing.T) {
	must := require.New(t)
	root, state := t.TempDir(), t.TempDir()
	canonical, dir, err := RecorderSyncPaths(root, state)
	must.NoError(err)
	path := RecorderUploadsPath(dir, canonical, "recorder", "https://archive.example.com")
	original := []byte(`{"version":3,"meetings":["future-shape"],"recordings":{},"future_state":{"retain":true}}`)
	must.NoError(os.WriteFile(path, original, 0600))
	receipts, err := LoadRecorderUploads[string](path)
	must.ErrorContains(err, "unsupported meeting upload record version 3")
	must.Nil(receipts)
	err = ForgetRecorderUploads[string](t.Context(), root, state, "recorder", "https://archive.example.com")
	must.ErrorContains(err, "unsupported meeting upload record version 3")
	after, err := os.ReadFile(path)
	must.NoError(err)
	assert.Equal(t, string(original), string(after), "reset must not delete future receipt state")
}
