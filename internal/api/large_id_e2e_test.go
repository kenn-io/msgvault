package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

// TestGetMessageLargeIDEndToEnd drives the real API server through the
// daemonclient store adapter with a message ID above ~10^6. It regression-tests
// the client float64 round-trip defect that rendered large IDs in scientific
// notation (e.g. /api/v1/messages/2.4489626e+07), which the server's
// strconv.ParseInt rejected with a 400.
func TestGetMessageLargeIDEndToEnd(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	const largeID = int64(24489626)
	srv, store := newTestServerWithMockStore(t)
	store.messages = []APIMessage{{
		ID:      largeID,
		Subject: "Large ID subject",
		From:    "alice@example.com",
		To:      []string{"bob@example.com"},
		SentAt:  time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Body:    "This is the full message body text.",
	}}

	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	dc, err := daemonclient.New(daemonclient.Config{
		URL:           httpSrv.URL,
		AllowInsecure: true,
		HTTPClient:    httpSrv.Client(),
	})
	require.NoError(err, "daemonclient.New")

	msg, err := dc.GetMessage(largeID)
	require.NoError(err, "GetMessage")
	require.NotNil(msg, "message")
	assert.Equal(largeID, msg.ID, "id")
	assert.Equal("Large ID subject", msg.Subject, "subject")
}
