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
