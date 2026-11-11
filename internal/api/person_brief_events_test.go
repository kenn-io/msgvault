package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPersonBriefHTTPEventQuestionsAndLegacyHistoryKeepSentenceCitations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonBriefAPIFixture(t)
	legacy := personBriefAPISeedFor(t, personBriefAPIStructure(), personBriefAPINow, []int64{f.evidence[0].ID})
	f.seedBrief(t, legacy)
	// A v1 saved version stays readable after a current v2 event version.
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_briefs
		SET program_version = 'v1' WHERE person_id = ? AND version = 1`), f.personID)
	require.NoError(err)
	for i, state := range []peoplesweep.BriefEventState{
		peoplesweep.BriefEventUpcoming, peoplesweep.BriefEventFollowUp, peoplesweep.BriefEventResolved,
	} {
		output := peoplesweep.BriefOutput{Highlights: []peoplesweep.BriefHighlight{{
			Text: "their interview", Speaker: peoplesweep.BriefSpeakerPerson,
			EvidenceIDs: []string{personBriefAPIEvidenceID},
			LifeEvent: &peoplesweep.BriefLifeEvent{AnchorID: personBriefAPIEvidenceID, Quote: "I have an interview.",
				Question: "How did the interview go?", State: state},
		}}}
		rendered, err := peoplesweep.RenderBrief(peoplesweep.ParsedBrief{Output: output}, peoplesweep.BriefWindow{})
		require.NoError(err)
		structured, err := json.Marshal(output)
		require.NoError(err)
		f.seedBrief(t, personBriefAPISeed{structured: string(structured), rendered: rendered.Text,
			boundary: `{}`, generatedAt: personBriefAPINow.Add(time.Duration(i+1) * time.Hour),
			evidenceIDs: []int64{f.evidence[0].ID}})
		_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_briefs
			SET renderer_policy = ? WHERE person_id = ? AND status = 'current'`),
			peoplesweep.BriefRendererPolicyV2, f.personID)
		require.NoError(err)
		response := doRequest(t, f.server.Router(), http.MethodGet, personBriefPath(f.personID, ""), nil, nil)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var brief PersonBrief
		require.NoError(json.Unmarshal(response.Body.Bytes(), &brief))
		assert.Equal(rendered.Text, brief.RenderedText)
		require.Len(brief.Sentences, 1)
		assert.Equal(rendered.Text, brief.Sentences[0].Text)
		assert.Equal([]int{0}, brief.Sentences[0].EvidenceOrdinals)
		if state == peoplesweep.BriefEventFollowUp {
			assert.Contains(brief.RenderedText, "How did the interview go?")
		} else {
			assert.NotContains(brief.RenderedText, "How did the interview go?")
		}
	}
	response := doRequest(t, f.server.Router(), http.MethodGet, personBriefPath(f.personID, "/versions"), nil, nil)
	require.Equal(http.StatusOK, response.Code)
	var versions PersonBriefVersionsResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &versions))
	require.Len(versions.Versions, 4)
	assert.Equal("v1", versions.Versions[3].ProgramVersion)
	assert.Equal(legacy.rendered, versions.Versions[3].RenderedText)
	assert.NotEmpty(versions.Versions[3].Sentences)
}

func TestPersonBriefHTTPBoundarySanitizesEventProsePreservingReferences(t *testing.T) {
	checks := assert.New(t)
	required := require.New(t)
	f := newPersonBriefAPIFixture(t)
	seed := personBriefAPISeedFor(t, personBriefAPIStructure(), personBriefAPINow, []int64{f.evidence[0].ID})
	seed.boundary = `{"event_memory_version":1,"event_memory":[{"highlight":{"text":"move\u001b[31m soon","evidence_ids":["id\u001b[31m"],"life_event":{"quote":"I \u001b[31mmove","question":"How \u001b[31mwas it?","anchor_id":"id\u001b[31m","resolution":{"quote":"It \u001b[31mwent well","evidence_id":"outcome\u001b[31m"}}},"sources":[{"message_id":9007199254740993,"source_message_id":"source\u001b[31m"}]}],"through_sequence":42,"packet_sha256":"abc123"}`
	f.seedBrief(t, seed)
	for _, suffix := range []string{"", "/versions"} {
		response := doRequest(t, f.server.Router(), http.MethodGet, personBriefPath(f.personID, suffix), nil, nil)
		required.Equal(http.StatusOK, response.Code, response.Body.String())
		var payload map[string]json.RawMessage
		required.NoError(json.Unmarshal(response.Body.Bytes(), &payload))
		if suffix != "" {
			var versions []map[string]json.RawMessage
			required.NoError(json.Unmarshal(payload["versions"], &versions))
			required.Len(versions, 1)
			payload = versions[0]
		}
		var boundary struct {
			EventMemory []struct {
				Highlight struct {
					Text        string   `json:"text"`
					EvidenceIDs []string `json:"evidence_ids"`
					LifeEvent   struct {
						Quote      string `json:"quote"`
						Question   string `json:"question"`
						AnchorID   string `json:"anchor_id"`
						Resolution struct {
							Quote      string `json:"quote"`
							EvidenceID string `json:"evidence_id"`
						} `json:"resolution"`
					} `json:"life_event"`
				} `json:"highlight"`
				Sources []struct {
					MessageID       int64  `json:"message_id"`
					SourceMessageID string `json:"source_message_id"`
				} `json:"sources"`
			} `json:"event_memory"`
			ThroughSequence int64  `json:"through_sequence"`
			PacketSHA256    string `json:"packet_sha256"`
		}
		required.NoError(json.Unmarshal(payload["boundary"], &boundary))
		required.Len(boundary.EventMemory, 1)
		event := boundary.EventMemory[0]
		checks.Equal("move soon", event.Highlight.Text)
		checks.Equal("I move", event.Highlight.LifeEvent.Quote)
		checks.Equal("How was it?", event.Highlight.LifeEvent.Question)
		checks.Equal("It went well", event.Highlight.LifeEvent.Resolution.Quote)
		checks.Equal([]string{"id\x1b[31m"}, event.Highlight.EvidenceIDs)
		checks.Equal("id\x1b[31m", event.Highlight.LifeEvent.AnchorID)
		checks.Equal("outcome\x1b[31m", event.Highlight.LifeEvent.Resolution.EvidenceID)
		required.Len(event.Sources, 1)
		checks.Equal(int64(9007199254740993), event.Sources[0].MessageID)
		checks.Equal("source\x1b[31m", event.Sources[0].SourceMessageID)
		checks.Equal(int64(42), boundary.ThroughSequence)
		checks.Equal("abc123", boundary.PacketSHA256)
	}
}
