package twilio

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestHTTPImporterEnrichmentAndUnchangedDownloadRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var withText atomic.Bool
	var downloadOK atomic.Bool
	var downloads atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2010-04-01/Accounts/" + testAC + "/Recordings.json", "/2010-04-01/Accounts/" + testAC + "/Calls/" + testCA + "/Recordings.json":
			writeJSON(t, w, map[string]any{"recordings": []any{map[string]any{"sid": testRE, "account_sid": testAC, "call_sid": testCA, "status": "completed", "channels": 2, "date_created": "Sat, 03 Oct 2026 10:00:00 +0000", "duration": "60"}}, "next_page_uri": nil})
		case "/2010-04-01/Accounts/" + testAC + "/Calls/" + testCA + ".json":
			writeJSON(t, w, map[string]any{"sid": testCA, "account_sid": testAC, "from": "+12025550101", "to": "+12025550102", "status": "completed", "duration": "60", "start_time": "Sat, 03 Oct 2026 10:00:00 +0000", "end_time": "Sat, 03 Oct 2026 10:01:00 +0000"})
		case "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE + "/Transcriptions.json":
			values := []any{}
			if withText.Load() {
				values = append(values, map[string]any{"sid": "TR" + strings.Repeat("a", 32), "account_sid": testAC, "recording_sid": testRE, "status": "completed", "transcription_text": "Retained synthetic budget"})
			}
			writeJSON(t, w, map[string]any{"transcriptions": values, "next_page_uri": nil})
		case "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE + ".wav":
			downloads.Add(1)
			if !downloadOK.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			_, err := w.Write([]byte("ID3\x04\x00\x00\x00\x00\x00\x00synthetic real HTTP audio"))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}, nil)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "http-account")
	require.NoError(err)
	imp := NewImporter(st, client)
	current := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	imp.now = func() time.Time { return current }
	opts := ImportOptions{Identifier: "http-account", AccountEmail: "owner@example.com", AttachmentsDir: t.TempDir()}
	first, err := imp.Import(t.Context(), opts)
	require.Error(err)
	assert.EqualValues(1, first.MeetingsAdded)
	state, err := loadState(st, source.ID)
	require.NoError(err)
	assert.True(state.Watermark.IsZero())
	current = current.Add(7 * time.Hour)
	withText.Store(true)
	downloadOK.Store(true)
	second, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, second.AttachmentsStored)
	messages, count, err := st.ListMessages(0, 10)
	require.NoError(err)
	require.EqualValues(1, count)
	msg, err := st.GetMessage(messages[0].ID)
	require.NoError(err)
	require.Len(msg.Attachments, 1)
	assert.Equal(testRE+".mp3", msg.Attachments[0].Filename)
	assert.Equal("audio/mpeg", msg.Attachments[0].MimeType)
	assert.Contains(msg.Body, "budget")
	blobs, err := attachmentstore.New(store.NewPackCatalog(st), opts.AttachmentsDir)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(blobs.Close()) })
	data, _, err := blobs.ReadBounded(msg.Attachments[0].ContentHash, 1024)
	require.NoError(err)
	assert.Contains(string(data), "synthetic real HTTP audio")
	current = current.Add(7 * time.Hour)
	withText.Store(false)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	msg, err = st.GetMessage(msg.ID)
	require.NoError(err)
	assert.Contains(msg.Body, "budget")
	assert.Len(msg.Attachments, 1)
	assert.True(msg.HasAttachments)
	assert.EqualValues(2, downloads.Load())
	found, n, err := st.SearchMessages("budget", 0, 10)
	require.NoError(err)
	assert.EqualValues(1, n)
	assert.Len(found, 1)
}
