package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonFactDecisionsHTTPExplainsStabilityWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st, personID, targets := newPersonFactAPIFixtureWithoutClaims(t)
	target := targets[store.AttributeSlugInterestsFunNow]
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID:      personID,
		SourceCursors: []personfacts.SourceCursor{{Lane: "stability", Start: "one", End: "two"}},
		ProgramID:     "fixture-program", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64),
		CatalogFingerprint: "fixture-catalog", Provider: "fixture-provider", ProviderVersion: "v1",
		ResolvedAt: personFactAPINow, Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture-policy"},
		Claims: []personfacts.ProposedClaim{{
			Target: target, Relation: personfacts.RelationSupport, SubmittedValue: json.RawMessage(`"climbing"`),
			Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
			Evidence: []personfacts.EvidenceInput{{
				PersonID: personID, SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf,
				Authority: personfacts.AuthorityAuthoritative, SourceURL: "https://example.test/stability",
				SubjectPersonID: &personID, SubjectRef: "synthetic-person", Excerpt: "Synthetic profile evidence",
				SourceVersion: "v1", EventTime: personFactAPINow.Add(-200 * 24 * time.Hour),
				RecordedTime: personFactAPINow, IdentityScore: 990,
			}},
		}},
	}, nil)
	require.NoError(err)
	response := doRequest(t, srv.Router(), http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/fact-decisions?target=%s", personID, personFactAPITargetQuery(target)), nil, nil)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var result struct {
		Decisions []struct {
			Reason string `json:"reason"`
			Score  struct {
				Total           int  `json:"total"`
				StabilityWindow bool `json:"stability_window"`
			} `json:"score"`
		} `json:"decisions"`
	}
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(result.Decisions, 1)
	assert.Equal("below-threshold", result.Decisions[0].Reason)
	assert.Greater(result.Decisions[0].Score.Total, 750)
	assert.True(result.Decisions[0].Score.StabilityWindow)
}
