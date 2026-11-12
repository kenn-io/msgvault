package beeper

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
)

func TestVoiceTranscriptSearchPendingToReady(t *testing.T) {
	world := importVoiceChat(t, voiceSpec{id: "voice", asset: "mxc://beeper.local/voice", mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(1600, 1)})
	docbank := newFakeDocbank(t)
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript") {
			w.Header().Set("Content-Type", "application/json")
			if reads.Add(1) == 1 {
				_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"pending","coverage_state":"pending"}`)
			} else {
				_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"ready","coverage_state":"complete","transcript":{"build_id":"build","origin":"generated","completeness":"complete","units":[{"text":"dinner Thursday"}]}}`)
			}
			return
		}
		docbank.ServeHTTP(w, r)
	}))
	defer server.Close()
	require.NoError(t, world.st.SetVoiceTranscriptSearch(t.Context(), true))
	submitter := world.submitter(t, server, "destination")
	submitter.WithASRProfile("asr").WithVoiceTranscriptSearch(submitter.client)
	runPasses(t, submitter, 6)
	var text string
	require.NoError(t, world.st.DB().QueryRowContext(t.Context(), `SELECT transcript_text FROM voice_transcripts`).Scan(&text))
	assert.Equal(t, "dinner Thursday", text)
	assert.GreaterOrEqual(t, reads.Load(), int32(2))
}

func TestGeneratedVoiceTextRequiresReadyGeneratedEvidence(t *testing.T) {
	ready := docbankmedia.MediaTranscript{EvidenceState: "ready", CoverageState: "transcribed", Transcript: &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "generated", Completeness: "complete", Units: []docbankmedia.MediaTranscriptUnit{{Text: "dinner Thursday"}}}}
	assert.Equal(t, "dinner Thursday", generatedVoiceText(ready))
	empty := ready
	empty.Transcript = &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "generated", Completeness: "complete"}
	assert.Empty(t, generatedVoiceText(empty))
	provider := ready
	provider.Transcript = &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "supplied", Units: []docbankmedia.MediaTranscriptUnit{{Text: "provider words"}}}
	assert.Empty(t, generatedVoiceText(provider))
}

func TestVoiceTranscriptSearchLateProviderTranscript(t *testing.T) {
	world := importVoiceChat(t, voiceSpec{id: "voice", asset: "mxc://beeper.local/voice", mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(1600, 1)})
	docbank := newFakeDocbank(t)
	var messageID int64
	require.NoError(t, world.st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='voice'`).Scan(&messageID))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/transcript") {
			docbank.ServeHTTP(w, r)
			return
		}
		raw, err := world.st.GetMessageRawContext(r.Context(), messageID)
		if !assert.NoError(t, err) {
			return
		}
		var message Message
		if !assert.NoError(t, json.Unmarshal(raw, &message)) {
			return
		}
		if !assert.Len(t, message.Attachments, 1) {
			return
		}
		message.Attachments[0].Transcription = &Transcription{Transcription: "provider words"}
		raw, err = json.Marshal(message)
		if !assert.NoError(t, err) {
			return
		}
		if !assert.NoError(t, world.st.UpsertMessageRawWithFormat(messageID, raw, "beeper_json")) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"ready","coverage_state":"transcribed","transcript":{"build_id":"build","origin":"generated","completeness":"complete","units":[{"text":"generated dinner"}]}}`)
	}))
	defer server.Close()
	require.NoError(t, world.st.SetVoiceTranscriptSearch(t.Context(), true))
	submitter := world.submitter(t, server, "destination")
	submitter.WithASRProfile("asr").WithVoiceTranscriptSearch(submitter.client)
	runPasses(t, submitter, 5)
	var count int
	require.NoError(t, world.st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM voice_transcripts`).Scan(&count))
	assert.Zero(t, count)
}

func TestVoiceTranscriptSearchUnusableReadyStopsReading(t *testing.T) {
	world := importVoiceChat(t, voiceSpec{id: "voice", asset: "mxc://beeper.local/voice", mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(1600, 1)})
	docbank := newFakeDocbank(t)
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/transcript") {
			docbank.ServeHTTP(w, r)
			return
		}
		reads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"ready","coverage_state":"transcribed","transcript":{"build_id":"build","origin":"supplied","supplied_input_id":"input","completeness":"complete","units":[{"text":"provider words"}]}}`)
	}))
	defer server.Close()
	require.NoError(t, world.st.SetVoiceTranscriptSearch(t.Context(), true))
	submitter := world.submitter(t, server, "destination")
	submitter.WithASRProfile("asr").WithVoiceTranscriptSearch(submitter.client)
	runPasses(t, submitter, 7)
	assert.Equal(t, int32(1), reads.Load())
	var text string
	require.NoError(t, world.st.DB().QueryRowContext(t.Context(), `SELECT transcript_text FROM voice_transcripts`).Scan(&text))
	assert.Empty(t, text)
}
