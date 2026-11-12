package beeper

import (
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
