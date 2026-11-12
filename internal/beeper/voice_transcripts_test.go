package beeper

import (
	"context"
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
	for _, scenario := range []string{"pending to ready", "provider at gate"} {
		t.Run(scenario, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			world := importVoiceChat(t, voiceSpec{id: "voice", asset: "mxc://beeper.local/voice", mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(1600, 1)})
			docbank := newFakeDocbank(t)
			var messageID int64
			require.NoError(world.st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='voice'`).Scan(&messageID))
			var reads atomic.Int32
			supply := func(ctx context.Context) {
				raw, err := world.st.GetMessageRawContext(ctx, messageID)
				if !assert.NoError(err) {
					return
				}
				var message Message
				if !assert.NoError(json.Unmarshal(raw, &message)) || !assert.Len(message.Attachments, 1) {
					return
				}
				message.Attachments[0].Transcription = &Transcription{Transcription: "provider words"}
				raw, err = json.Marshal(message)
				if assert.NoError(err) {
					assert.NoError(world.st.UpsertMessageRawWithFormat(messageID, raw, "beeper_json"))
				}
			}
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
				_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"ready","coverage_state":"transcribed","transcript":{"origin":"generated","completeness":"complete","units":[{"text":"dinner Thursday"}]}}`)
			}))
			defer server.Close()
			submitter := world.submitter(t, server, "destination")
			submitter.WithASRProfile("asr")
			var supplied bool
			if scenario == "provider at gate" {
				submitter.WithOperationGate(func(ctx context.Context) (func(), bool) {
					if reads.Load() > 0 && !supplied {
						supply(ctx)
						supplied = true
					}
					return func() {}, true
				})
			}
			require.NoError(world.st.SetVoiceTranscriptSearch(t.Context(), true))
			submitter.WithVoiceTranscriptSearch(submitter.client)
			runPasses(t, submitter, 7)
			if scenario == "provider at gate" {
				assert.Positive(reads.Load())
				assert.True(supplied)
				var count int
				require.NoError(world.st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM voice_transcripts`).Scan(&count))
				assert.Zero(count)
				return
			}
			var text string
			require.NoError(world.st.DB().QueryRowContext(t.Context(), `SELECT transcript_text FROM voice_transcripts`).Scan(&text))
			assert.Equal("dinner Thursday", text)
			assert.GreaterOrEqual(reads.Load(), int32(2))
		})
	}
}

func TestGeneratedVoiceTextRejectsSuppliedAndReplacesNUL(t *testing.T) {
	assert := assert.New(t)
	generated := docbankmedia.MediaTranscript{Transcript: &docbankmedia.MediaTranscriptEvidence{Origin: "generated", Completeness: "complete", Units: []docbankmedia.MediaTranscriptUnit{{Text: "dinner\x00 Thursday"}}}}
	assert.Equal("dinner\uFFFD Thursday", generatedVoiceText(generated))
	provider := docbankmedia.MediaTranscript{Transcript: &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "supplied", SuppliedInputID: "input", Completeness: "complete", Units: []docbankmedia.MediaTranscriptUnit{{Text: "provider words"}}}}
	assert.Empty(generatedVoiceText(provider))
}
