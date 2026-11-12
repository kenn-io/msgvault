package beeper

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/search"
)

func TestVoiceTranscriptSearchPendingToReady(t *testing.T) {
	world := importVoiceChat(t, voiceSpec{id: "voice", asset: "mxc://beeper.local/voice", mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(1600, 1)})
	docbank := newFakeDocbank(t)
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript") {
			assert.Equal(t, "/api/v1/media/sources/source-1/versions/audio-version-1/transcript", r.URL.Path)
			assert.Equal(t, "audio-content-1", r.URL.Query().Get("content_version_id"))
			w.Header().Set("Content-Type", "application/json")
			if reads.Add(1) == 1 {
				_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"pending","coverage_state":"pending"}`)
			} else {
				_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"ready","coverage_state":"transcribed","transcript":{"build_id":"build","origin":"generated","completeness":"complete","units":[{"text":"dinner Thursday"}]}}`)
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
	ids, _, err := world.st.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"dinner", "Thursday"}}, 10)
	require.NoError(t, err)
	assert.Len(t, ids, 1)
	assert.GreaterOrEqual(t, reads.Load(), int32(2))
}

func TestVoiceTranscriptSearchDisabledDuringRead(t *testing.T) {
	world := importVoiceChat(t, voiceSpec{id: "voice", asset: "mxc://beeper.local/voice", mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(1600, 1)})
	docbank := newFakeDocbank(t)
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript") {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"ready","coverage_state":"transcribed","transcript":{"build_id":"build","origin":"generated","completeness":"complete","units":[{"text":"dinner Thursday"}]}}`)
			return
		}
		docbank.ServeHTTP(w, r)
	}))
	defer server.Close()
	require.NoError(t, world.st.SetVoiceTranscriptSearch(t.Context(), true))
	submitter := world.submitter(t, server, "destination")
	submitter.WithASRProfile("asr")
	runPasses(t, submitter, 5)
	deliveries := deliveryRows(t, world.st, "destination")
	require.Len(t, deliveries, 1)
	require.Equal(t, "done", deliveries[0].Phase)
	require.Equal(t, "transcribed", deliveries[0].Coverage)
	submitter.WithVoiceTranscriptSearch(submitter.client)
	done := make(chan error, 1)
	go func() {
		_, err := submitter.RunBatch(t.Context())
		done <- err
	}()
	finished := false
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		if !finished {
			<-done
		}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "transcript request did not start")
	}
	require.NoError(t, world.st.SetVoiceTranscriptSearch(t.Context(), false))
	close(release)
	select {
	case err := <-done:
		finished = true
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "transcript read did not finish")
	}
	ids, _, err := world.st.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"dinner", "Thursday"}}, 10)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestGeneratedVoiceTextRequiresReadyGeneratedEvidence(t *testing.T) {
	ready := docbankmedia.MediaTranscript{EvidenceState: "ready", CoverageState: "transcribed", Transcript: &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "generated", Completeness: "complete", Units: []docbankmedia.MediaTranscriptUnit{{Text: "dinner Thursday"}}}}
	text, valid := generatedVoiceText(ready)
	assert.Equal(t, "dinner Thursday", text)
	assert.True(t, valid)
	empty := ready
	empty.Transcript = &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "generated", Completeness: "complete"}
	text, valid = generatedVoiceText(empty)
	assert.True(t, valid)
	assert.Empty(t, text)
	provider := ready
	provider.Transcript = &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "supplied", Units: []docbankmedia.MediaTranscriptUnit{{Text: "provider words"}}}
	_, valid = generatedVoiceText(provider)
	assert.False(t, valid)
	pending := ready
	pending.EvidenceState = "pending"
	pending.Transcript = nil
	_, valid = generatedVoiceText(pending)
	assert.False(t, valid)
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
	ids, _, err := world.st.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"generated", "dinner"}}, 10)
	require.NoError(t, err)
	assert.Empty(t, ids)
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
