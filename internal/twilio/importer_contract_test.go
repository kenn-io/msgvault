package twilio

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImporterRetainedRecordingSurvivesMissingCallMetadata(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	var metadataAvailable atomic.Bool
	var audioRequests atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		user, secret, ok := r.BasicAuth()
		assert.True(ok)
		assert.Equal(testAC, user)
		assert.Equal("test-secret", secret)
		switch r.URL.Path {
		case "/2010-04-01/Accounts/" + testAC + "/Recordings.json":
			writeJSON(t, w, map[string]any{"recordings": []any{map[string]any{"sid": testRE, "account_sid": testAC, "call_sid": testCA, "status": "completed", "start_time": "2020-01-01T10:00:00Z", "duration": "30", "channels": 1}}})
		case "/2010-04-01/Accounts/" + testAC + "/Calls/" + testCA + ".json":
			if !metadataAvailable.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(t, w, map[string]any{"sid": testCA, "account_sid": testAC, "from": "+12025550101", "to": "+12025550102", "duration": "60", "start_time": "2020-01-01T10:00:00Z", "end_time": "2020-01-01T10:01:00Z"})
		case "/2010-04-01/Accounts/" + testAC + "/Calls/" + testCA + "/Recordings.json":
			w.WriteHeader(http.StatusNotFound)
		case "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE + "/Transcriptions.json":
			writeJSON(t, w, map[string]any{"transcriptions": []any{map[string]any{"sid": "TRaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "account_sid": testAC, "recording_sid": testRE, "status": "completed", "transcription_text": "Retained speech"}}})
		case "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE + ".wav":
			audioRequests.Add(1)
			w.Header().Set("Content-Type", "audio/wav")
			_, err := w.Write([]byte("RIFF\x24\x00\x00\x00WAVEfmt synthetic audio"))
			assert.NoError(err)
		case "/v2/Transcripts":
			writeJSON(t, w, map[string]any{"transcripts": []any{}})
		case "/v3/Transcriptions":
			writeJSON(t, w, map[string]any{"transcriptions": []any{}})
		case "/v2/Conversations":
			writeJSON(t, w, map[string]any{"conversations": []any{}})
		default:
			assert.Fail("unexpected endpoint", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}, nil)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	importer := NewImporter(st, client)
	importer.now = func() time.Time { return now }
	opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", AttachmentsDir: t.TempDir()}
	summary, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, summary.MeetingsAdded)
	assert.EqualValues(1, summary.AttachmentsStored)
	assert.Contains(summary.Diagnostics, "call_metadata_unavailable")
	archive, err := loadArchive(t.Context(), st, source.ID, testCA)
	require.NoError(err)
	assert.Empty(archive.Call.From)
	assert.Empty(archive.Call.To)
	assert.Empty(archive.Call.Raw, "no fabricated provider Call response")
	raw, err := archive.snapshot(source.ID, opts.AccountEmail)
	require.NoError(err)
	content := meetingcontent.Decode(RawFormat, raw.Raw, nil)
	assert.Nil(content.DurationSeconds, "recording duration is not whole-call duration")
	assert.Contains(content.Transcript.Text, "Retained speech")
	state, err := loadState(st, source.ID)
	require.NoError(err)
	assert.False(state.Known[testCA].Unverified)
	assert.Equal(now, state.Watermark)
	// Expired metadata absence stops maintenance without holding discovery.
	now = now.Add(49 * time.Hour)
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	state, err = loadState(st, source.ID)
	require.NoError(err)
	assert.True(state.Known[testCA].NextAttempt.IsZero())
	assert.False(state.Known[testCA].Failed)
	assert.Equal(now, state.Watermark)
	// Full refresh revives metadata on the same message and keeps its audio.
	metadataAvailable.Store(true)
	opts.Full = true
	summary, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(0, summary.MeetingsAdded)
	assert.EqualValues(1, summary.MeetingsUpdated)
	archive, err = loadArchive(t.Context(), st, source.ID, testCA)
	require.NoError(err)
	assert.Equal("+12025550101", archive.Call.From)
	assert.Equal("60", archive.Call.Duration)
	assert.NotEmpty(archive.Call.Raw)
	// Later deletion and omission retain the authoritative previous metadata.
	metadataAvailable.Store(false)
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	archive, err = loadArchive(t.Context(), st, source.ID, testCA)
	require.NoError(err)
	assert.Equal("+12025550101", archive.Call.From)
	assert.Equal("60", archive.Call.Duration)
	assert.NotEmpty(archive.Call.Raw)
	assert.EqualValues(1, audioRequests.Load())
	messages, count, err := st.ListMessages(0, 10)
	require.NoError(err)
	require.EqualValues(1, count)
	message, err := st.GetMessage(messages[0].ID)
	require.NoError(err)
	assert.True(message.HasAttachments)
	var attachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT attachment_count FROM messages WHERE id = ?"), messages[0].ID).Scan(&attachmentCount))
	assert.Equal(1, attachmentCount)
	require.Len(message.Attachments, 1)
	assert.True(strings.HasSuffix(message.Attachments[0].Filename, ".wav"))
}
