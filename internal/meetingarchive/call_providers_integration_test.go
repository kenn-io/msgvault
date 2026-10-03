package meetingarchive_test

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/bland"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/twilio"
)

// Exercise both production HTTP clients through the shared meeting and blob
// stores: source identity must keep calls apart even if labels and IDs collide,
// while identical audio bytes deduplicate without losing either occurrence.
func TestCallProvidersArchiveRecordingsAndEnrichMeetings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	account := "AC" + strings.Repeat("a", 32)
	callID := "CA" + strings.Repeat("b", 32)
	recordingID := "RE" + strings.Repeat("c", 32)
	transcriptionID := "TR" + strings.Repeat("d", 32)
	var transcriptReady atomic.Bool
	var downloads atomic.Int32
	audio := syntheticCallWAV()
	call := map[string]any{"sid": callID, "account_sid": account, "from": "+12025550100", "to": "+12025550101", "status": "completed", "direction": "outbound-api", "start_time": "2026-10-01T12:00:00Z", "end_time": "2026-10-01T12:01:00Z", "duration": "60"}
	recording := map[string]any{"sid": recordingID, "account_sid": account, "call_sid": callID, "status": "completed", "date_created": "2026-10-01T12:00:00Z", "start_time": "2026-10-01T12:00:00Z", "duration": "60", "channels": 1}
	writeJSON := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(value))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			assert.Equal("synthetic-bland-key", r.Header.Get("Authorization"))
		} else {
			user, password, ok := r.BasicAuth()
			assert.True(ok)
			assert.Equal(account, user)
			assert.Equal("synthetic-twilio-token", password)
		}
		voice := "/2010-04-01/Accounts/" + account
		switch r.URL.Path {
		case voice + "/Recordings.json", voice + "/Calls/" + callID + "/Recordings.json":
			writeJSON(w, map[string]any{"recordings": []any{recording}, "next_page_uri": nil})
		case voice + "/Calls/" + callID + ".json":
			writeJSON(w, call)
		case voice + "/Recordings/" + recordingID + "/Transcriptions.json":
			items := []any{}
			if transcriptReady.Load() {
				items = append(items, map[string]any{"sid": transcriptionID, "account_sid": account, "recording_sid": recordingID, "status": "completed", "transcription_text": "Watermelon delivery confirmed."})
			}
			writeJSON(w, map[string]any{"transcriptions": items, "next_page_uri": nil})
		case "/v2/Transcripts":
			writeJSON(w, map[string]any{"transcripts": []any{}, "meta": map[string]any{"next_page_url": nil}})
		case "/v3/Transcriptions":
			writeJSON(w, map[string]any{"transcriptions": []any{}, "meta": map[string]any{}})
		case "/v2/Conversations":
			writeJSON(w, map[string]any{"conversations": []any{}, "meta": map[string]any{}})
		case "/v1/calls":
			items := []any{}
			if r.URL.Query().Get("from") == "" || r.URL.Query().Get("from") == "0" {
				items = append(items, map[string]any{"call_id": callID, "updated_at": "2026-10-01T12:02:00Z"})
			}
			writeJSON(w, map[string]any{"calls": items, "count": len(items), "total_count": 1})
		case "/v1/calls/" + callID:
			value := map[string]any{"call_id": callID, "from": "+12025550100", "to": "+12025550101", "completed": true, "status": "completed", "record": true, "started_at": "2026-10-01T12:00:00Z", "created_at": "2026-10-01T12:00:00Z", "corrected_duration": "60"}
			if transcriptReady.Load() {
				value["transcripts"] = []any{map[string]any{"user": "user", "text": "Watermelon delivery confirmed."}}
			}
			writeJSON(w, value)
		case "/v1/postcall/webhooks/" + callID:
			w.WriteHeader(http.StatusNotFound)
		case voice + "/Recordings/" + recordingID + ".wav", "/v1/recordings/" + callID:
			downloads.Add(1)
			w.Header().Set("Content-Type", "audio/wav")
			w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
			_, err := w.Write(audio)
			assert.NoError(err)
		default:
			assert.Fail("unexpected provider request", "%s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	_, err := st.GetOrCreateSource(twilio.SourceType, "calls")
	require.NoError(err)
	_, err = st.GetOrCreateSource(bland.SourceType, "calls")
	require.NoError(err)
	client, err := twilio.NewClient(twilio.Options{AccountSID: account, AuthToken: "synthetic-twilio-token", Endpoints: map[string]string{
		"voice": server.URL, "intelligence": server.URL, "batch": server.URL, "insights": server.URL, "orchestrator": server.URL,
	}})
	require.NoError(err)
	twilioImport := twilio.NewImporter(st, client)
	blandImport := bland.NewImporter(st, bland.NewClient(server.URL+"/v1", "synthetic-bland-key"))
	syncBoth := func(full bool) {
		_, err := twilioImport.Import(t.Context(), twilio.ImportOptions{Identifier: "calls", AccountEmail: "owner@example.com", AttachmentsDir: root, Full: full})
		require.NoError(err)
		_, err = blandImport.Import(t.Context(), bland.ImportOptions{Identifier: "calls", AccountEmail: "owner@example.com", AttachmentsDir: root, Full: full})
		require.NoError(err)
	}
	syncBoth(false)
	messages, total, err := st.ListMessages(0, 10)
	require.NoError(err)
	require.EqualValues(2, total)
	require.Len(messages, 2)
	ids := []int64{messages[0].ID, messages[1].ID}
	assert.NotEqual(messages[0].SourceID, messages[1].SourceID)
	for _, id := range ids {
		message, err := st.GetMessage(id)
		require.NoError(err)
		assert.True(message.HasAttachments, "initial import must expose the recording")
		require.Len(message.Attachments, 1)
	}
	transcriptReady.Store(true)
	syncBoth(true)
	syncBoth(false)
	// An upstream omission on a later full refresh must not erase speech or audio.
	transcriptReady.Store(false)
	syncBoth(true)
	_, total, err = st.ListMessages(0, 10)
	require.NoError(err)
	assert.EqualValues(2, total)
	assert.EqualValues(2, downloads.Load(), "unchanged audio should be downloaded once per provider")
	var sharedHash string
	for _, id := range ids {
		message, err := st.GetMessage(id)
		require.NoError(err)
		assert.Equal("meeting_transcript", message.MessageType)
		assert.True(message.HasAttachments)
		require.Len(message.Attachments, 1)
		assert.Contains(message.Body, "Watermelon")
		raw, err := st.GetMessageRaw(id)
		require.NoError(err)
		source, err := st.GetSourceByID(message.SourceID)
		require.NoError(err)
		format := twilio.RawFormat
		if source.SourceType == bland.SourceType {
			format = bland.RawFormat
		}
		assert.Equal(meetingcontent.StateAvailable, meetingcontent.Decode(format, raw, nil).Transcript.State)
		hash := message.Attachments[0].ContentHash
		require.NotEmpty(hash)
		if sharedHash == "" {
			sharedHash = hash
		} else {
			assert.Equal(sharedHash, hash)
		}
	}
	results, count, err := st.SearchMessages("Watermelon", 0, 10)
	require.NoError(err)
	assert.EqualValues(2, count)
	assert.Len(results, 2)
	blobs, err := attachmentstore.New(store.NewPackCatalog(st), root)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(blobs.Close()) })
	reader, size, err := blobs.OpenStream(t.Context(), sharedHash)
	require.NoError(err)
	got, err := io.ReadAll(reader)
	require.NoError(err)
	require.NoError(reader.Close())
	assert.EqualValues(len(audio), size)
	assert.Equal(audio, got)
}

func syntheticCallWAV() []byte {
	data := make([]byte, 44+16000)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 8000)
	binary.LittleEndian.PutUint32(data[28:], 16000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], 16000)
	return data
}
