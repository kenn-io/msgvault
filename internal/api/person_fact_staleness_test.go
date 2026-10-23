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
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonFactStalenessHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st, personID, targets := newPersonFactAPIFixtureWithoutClaims(t)
	now := time.Now().UTC()
	subject := personID
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID:      personID,
		SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "stale", End: "stale-end"}},
		ProgramID:     "fixture-program", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64),
		CatalogFingerprint: "fixture-catalog", Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1",
		ResolvedAt: now.Add(-100 * 24 * time.Hour), Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture-policy"},
		Claims: []personfacts.ProposedClaim{{
			Target: targets[store.AttributeSlugInterestsFunNow], Relation: personfacts.RelationSupport,
			SubmittedValue: json.RawMessage(`"climbing"`), Origin: personfacts.OriginExtraction,
			Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
			Evidence: []personfacts.EvidenceInput{{
				PersonID: personID, SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative,
				SourceURL: "https://example.com/stale", SubjectPersonID: &subject, SubjectRef: "synthetic-person", Excerpt: "Synthetic evidence",
				SourceVersion: "source-v1", EventTime: now.Add(-200 * 24 * time.Hour), RecordedTime: now.Add(-100 * 24 * time.Hour), IdentityScore: 990,
			}},
		}},
	}, nil)
	require.NoError(err)
	response := doRequest(t, srv.Router(), http.MethodGet, fmt.Sprintf("/api/v1/people/%d/fact-staleness", personID), nil, nil)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal("no-store", response.Header().Get("Cache-Control"))
	var body PersonFactStalenessResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assert.False(body.AsOf.IsZero())
	require.Len(body.Items, 1)
	assert.True(body.Items[0].Stale)
	assert.Equal(personfacts.StabilityVolatile, body.Items[0].Stability)
	assert.JSONEq(`"climbing"`, string(body.Items[0].Value))
	unknown := doRequest(t, srv.Router(), http.MethodGet, "/api/v1/people/999999/fact-staleness", nil, nil)
	assert.Equal(http.StatusNotFound, unknown.Code, unknown.Body.String())
	invalid := doRequest(t, srv.Router(), http.MethodGet, "/api/v1/people/invalid/fact-staleness", nil, nil)
	assert.Equal(http.StatusBadRequest, invalid.Code, invalid.Body.String())
	delegated, registry := newTestServerWithAgentGrants(t)
	_, secret, _, err := registry.Issue("stale-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "alice@example.com"}}, time.Time{})
	require.NoError(err)
	refused := doRequest(t, delegated.Router(), http.MethodGet, fmt.Sprintf("/api/v1/people/%d/fact-staleness", personID), nil, map[string]string{apiprotocol.AgentTokenHeader: secret})
	assert.Equal(http.StatusUnauthorized, refused.Code, refused.Body.String())
}

func TestOpenAPIPersonFactStalenessUsesLosslessValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	base := OpenAPIDocument().Components.Schemas.Map()["PersonFactStaleness"]
	require.NotNil(base)
	require.NotNil(base.Properties["value"])
	assert.Empty(base.Properties["value"].Type)
	schema := openAPIClientDocument().Components.Schemas.Map()["PersonFactStaleness"]
	require.NotNil(schema)
	value := schema.Properties["value"]
	require.NotNil(value)
	assert.Equal("jsontext.Value", value.Extensions["x-go-type"])
	assert.Equal(map[string]any{"path": "encoding/json/jsontext"}, value.Extensions["x-go-type-import"])
}
