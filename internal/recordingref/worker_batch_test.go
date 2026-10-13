package recordingref

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRecordingReferenceBatchContinuesAfterInvalidOccurrence(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	bad := f.CreateMessage("bad")
	good := f.CreateMessage("good")
	recordingBody(t, f, bad, "https://loom.com/share/bad")
	recordingBody(t, f, good, "https://loom.com/share/good")
	_, err := f.Store.DB().Exec(`UPDATE messages SET content_changed_at='2000-01-01 00:00:00.000'`)
	require.NoError(err)
	requests := make(chan string, 2)
	client := recordingURLClient(t, requests)
	worker := NewWorker(f.Store, client, "destination", nil, nil)
	// Discover both rows without sending, then corrupt the first saved occurrence.
	unavailable, err := docbankmedia.NewClient("http://127.0.0.1:1", func() (string, error) {
		return "", docbankmedia.ErrCredentialUnavailable
	})
	require.NoError(err)
	require.NoError(NewWorker(f.Store, unavailable, "destination", nil, nil).RunBatch(t.Context()))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE recording_references SET occurrence_json='{' WHERE message_id=?`), bad)
	require.NoError(err)
	due(t, f)
	err = worker.RunBatch(t.Context())
	require.Error(err)
	assert.Contains(err.Error(), "message "+strconv.FormatInt(bad, 10))
	var route string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT route_key FROM recording_references WHERE message_id=?`), bad).Scan(&route))
	assert.Contains(err.Error(), route)
	require.Len(requests, 1, "an invalid earlier row must not stop later deliveries")
	assert.Equal("https://loom.com/share/good", <-requests)
	state, _, _ := recordingState(t, f, good)
	assert.Equal("retained", state)
}
