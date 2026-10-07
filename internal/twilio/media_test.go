package twilio

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/export"
)

var testWAV = []byte("RIFF\x10\x00\x00\x00WAVEfmt \x00synthetic")

func TestRecordingDualFallbackAndBounds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	requests := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal("/2010-04-01/Accounts/"+testAC+"/Recordings/"+testRE+".wav", r.URL.Path)
		if r.URL.Query().Get("RequestedChannels") == "2" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, err := w.Write(testWAV)
		assert.NoError(err)
	}, nil)
	reader, err := c.OpenRecording(context.Background(), Recording{SID: testRE, AccountSID: testAC, Status: "completed", Channels: 2}, 100)
	require.NoError(err)
	data, err := io.ReadAll(reader)
	require.NoError(err)
	require.NoError(reader.Close())
	assert.Equal(testWAV, data)
	assert.Equal(2, requests)
	_, err = c.OpenRecording(context.Background(), Recording{SID: testRE, Status: "completed"}, 12)
	require.ErrorIs(err, export.ErrAttachmentTooLarge)

	// A JSON error body labeled as audio is refused.
	jsonBody := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, err := w.Write([]byte(`{"error":"not audio"}`))
		assert.NoError(err)
	}, nil)
	reader, err = jsonBody.OpenRecording(context.Background(), Recording{SID: testRE, Status: "completed"}, 100)
	if reader != nil {
		require.NoError(reader.Close())
	}
	require.Error(err)
}
