package recordingref

import (
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRecordingReferenceFeedHTMLProse(t *testing.T) {
	for _, tc := range []struct {
		name, bodyText string
	}{
		{"HTML only", ""},
		{"plain text without URL", "Watch the recording"},
		{"URL in both alternatives", "https://loom.com/share/html-prose"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("html-prose")
			require.NoError(f.Store.UpsertMessageBody(id,
				sql.NullString{String: tc.bodyText, Valid: tc.bodyText != ""},
				sql.NullString{String: `<p>https://loom.com/share/html-prose</p>`, Valid: true}))
			requests := make(chan string, 2)
			client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
				var req docbankmedia.ReferenceRequest
				if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
					return
				}
				requests <- req.ReferenceURL
				writeReceipt(w, req.OperationID)
			})
			w := NewWorker(f.Store, client, "destination", nil, nil)
			waitForRecordingChange(t, w, id)
			require.Len(requests, 1)
			assert.Equal("https://loom.com/share/html-prose", <-requests)
		})
	}
}
