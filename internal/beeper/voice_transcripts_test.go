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
	"go.kenn.io/msgvault/internal/search"
)

func TestVoiceTranscriptSearchReadsReadyEvidence(t *testing.T) {
	for _, scenario := range []string{"pending to ready", "late provider", "supplied origin", "profile cleared"} {
		t.Run(scenario, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			world := importVoiceChat(t, voiceSpec{id: "voice", asset: "mxc://beeper.local/voice", mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(1600, 1)})
			docbank := newFakeDocbank(t)
			var messageID int64
			require.NoError(world.st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='voice'`).Scan(&messageID))
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
					if !assert.NoError(err) {
						return
					}
					var message Message
					if !assert.NoError(json.Unmarshal(raw, &message)) || !assert.Len(message.Attachments, 1) {
						return
					}
					message.Attachments[0].Transcription = &Transcription{Transcription: "provider words"}
					raw, err = json.Marshal(message)
					if !assert.NoError(err) || !assert.NoError(world.st.UpsertMessageRawWithFormat(messageID, raw, "beeper_json")) {
						return
					}
				}
				if scenario == "supplied origin" {
					_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"ready","coverage_state":"transcribed","transcript":{"build_id":"build","origin":"supplied","supplied_input_id":"input","completeness":"complete","units":[{"text":"provider words"}]}}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"vault_uid":"vault-1","source_id":"source-1","source_version_id":"audio-version-1","content_version_id":"audio-content-1","evidence_state":"ready","coverage_state":"transcribed","transcript":{"origin":"generated","completeness":"complete","units":[{"text":"dinner\u0000 Thursday"}]}}`)
			}))
			defer server.Close()
			submitter := world.submitter(t, server, "destination")
			submitter.WithASRProfile("asr")
			if scenario == "profile cleared" {
				runPasses(t, submitter, 6)
				submitter.WithASRProfile("")
				var attachmentID int64
				require.NoError(world.st.DB().QueryRowContext(t.Context(), world.st.Rebind(`SELECT id FROM attachments WHERE message_id=?`), messageID).Scan(&attachmentID))
				candidate, err := world.st.GetBeeperMediaCandidate(t.Context(), attachmentID)
				require.NoError(err)
				archiveUID, err := world.st.ArchiveUIDContext(t.Context())
				require.NoError(err)
				mapping, err := submitter.reconcileCandidate(t.Context(), archiveUID, candidate)
				require.NoError(err)
				assert.Empty(mapping.ProcessingKey)
			}
			require.NoError(world.st.SetVoiceTranscriptSearch(t.Context(), true))
			submitter.WithVoiceTranscriptSearch(submitter.client)
			sender := func() []int64 {
				ids, _, err := world.st.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"Ann"}}, 10)
				require.NoError(err)
				return ids
			}
			assert.Contains(sender(), messageID)
			runPasses(t, submitter, 7)
			if scenario == "late provider" {
				assert.Positive(reads.Load())
				var count int
				require.NoError(world.st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM voice_transcripts`).Scan(&count))
				assert.Zero(count)
				return
			}
			var text string
			require.NoError(world.st.DB().QueryRowContext(t.Context(), `SELECT transcript_text FROM voice_transcripts`).Scan(&text))
			if scenario == "pending to ready" || scenario == "profile cleared" {
				assert.Equal("dinner\uFFFD Thursday", text)
				ids, _, err := world.st.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"dinner", "Thursday"}}, 10)
				require.NoError(err)
				assert.Contains(ids, messageID)
				if scenario == "pending to ready" {
					assert.GreaterOrEqual(reads.Load(), int32(2))
				}
				assert.Contains(sender(), messageID)
				require.NoError(world.st.SetVoiceTranscriptSearch(t.Context(), false))
				assert.Contains(sender(), messageID)
			} else {
				assert.Empty(text)
				assert.Equal(int32(1), reads.Load())
			}
		})
	}
}

func TestGeneratedVoiceTextRejectsEmptyAndSuppliedEvidence(t *testing.T) {
	assert := assert.New(t)
	empty := docbankmedia.MediaTranscript{Transcript: &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "generated", Completeness: "complete"}}
	assert.Empty(generatedVoiceText(empty))
	provider := docbankmedia.MediaTranscript{Transcript: &docbankmedia.MediaTranscriptEvidence{BuildID: "build", Origin: "supplied", SuppliedInputID: "input", Completeness: "complete", Units: []docbankmedia.MediaTranscriptUnit{{Text: "provider words"}}}}
	assert.Empty(generatedVoiceText(provider))
}
