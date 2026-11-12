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

func TestVoiceTranscriptSearchReadsReadyEvidence(t *testing.T) {
	for _, scenario := range []string{"pending to ready", "late provider", "supplied origin"} {
		t.Run(scenario, func(t *testing.T) {
			world := importVoiceChat(t, voiceSpec{id: "voice", asset: "mxc://beeper.local/voice", mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(1600, 1)})
			docbank := newFakeDocbank(t)
			var messageID int64
			require.NoError(t, world.st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='voice'`).Scan(&messageID))
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/transcript") {
					docbank.ServeHTTP(w, r)
					return
				}
				n := reads.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if scenario == "pending to ready" && n == 1 {
					_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"pending","coverage_state":"pending"}`)
					return
				}
				if scenario == "late provider" {
					raw, err := world.st.GetMessageRawContext(r.Context(), messageID)
					if !assert.NoError(t, err) {
						return
					}
					var message Message
					if !assert.NoError(t, json.Unmarshal(raw, &message)) || !assert.Len(t, message.Attachments, 1) {
						return
					}
					message.Attachments[0].Transcription = &Transcription{Transcription: "provider words"}
					raw, err = json.Marshal(message)
					if !assert.NoError(t, err) || !assert.NoError(t, world.st.UpsertMessageRawWithFormat(messageID, raw, "beeper_json")) {
						return
					}
				}
				if scenario == "supplied origin" {
					_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"ready","coverage_state":"transcribed","transcript":{"build_id":"build","origin":"supplied","supplied_input_id":"input","completeness":"complete","units":[{"text":"provider words"}]}}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"ready","coverage_state":"transcribed","transcript":{"origin":"generated","completeness":"complete","units":[{"text":"dinner Thursday"}]}}`)
			}))
			defer server.Close()
			require.NoError(t, world.st.SetVoiceTranscriptSearch(t.Context(), true))
			submitter := world.submitter(t, server, "destination")
			submitter.WithASRProfile("asr").WithVoiceTranscriptSearch(submitter.client)
			runPasses(t, submitter, 7)
			if scenario == "late provider" {
				assert.Positive(t, reads.Load())
				var count int
				require.NoError(t, world.st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM voice_transcripts`).Scan(&count))
				assert.Zero(t, count)
				return
			}
			var text string
			require.NoError(t, world.st.DB().QueryRowContext(t.Context(), `SELECT transcript_text FROM voice_transcripts`).Scan(&text))
			if scenario == "pending to ready" {
				assert.Equal(t, "dinner Thursday", text)
				assert.GreaterOrEqual(t, reads.Load(), int32(2))
			} else {
				assert.Empty(t, text)
				assert.Equal(t, int32(1), reads.Load())
			}
		})
	}
}

func TestGeneratedVoiceTextRejectsEmptyAndSuppliedEvidence(t *testing.T) {
	empty := docbankmedia.MediaTranscript{Transcript: &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "generated", Completeness: "complete"}}
	assert.Empty(t, generatedVoiceText(empty))
	provider := docbankmedia.MediaTranscript{Transcript: &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "supplied", SuppliedInputID: "input", Completeness: "complete", Units: []docbankmedia.MediaTranscriptUnit{{Text: "provider words"}}}}
	assert.Empty(t, generatedVoiceText(provider))
}
