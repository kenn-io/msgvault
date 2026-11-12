package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntroductionReadsWithoutCapability(t *testing.T) {
	srv, _ := newTestServerWithAgentGrants(t)
	for _, path := range []string{"/api/v1/intros", "/api/v1/intros/unknown"} {
		response := doRequest(t, srv.Router(), http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer owner-key"})
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		assert.Contains(t, response.Body.String(), "local_store_failed")
	}
}

func TestIntroductionOpenAPI(t *testing.T) {
	doc := OpenAPIDocument()
	for path, id := range map[string]string{"/api/v1/intros": "listIntroductions", "/api/v1/intros/{id}": "getIntroduction"} {
		require.NotNil(t, doc.Paths[path])
		op := doc.Paths[path].Get
		require.NotNil(t, op)
		assert.Equal(t, id, op.OperationID)
		require.NotNil(t, op.Responses["200"])
		assert.Contains(t, op.Responses["200"].Headers, "Cache-Control")
	}
	schemas := doc.Components.Schemas.Map()
	require.Contains(t, schemas, "IntroductionRecord")
	require.Contains(t, schemas, "IntroductionPage")
	for _, name := range []string{"intro_id", "revision", "status", "source_id", "person_a_id", "person_b_id", "title", "body", "created_by", "cancelled_at", "created_at", "updated_at", "members", "blockers"} {
		assert.Contains(t, schemas["IntroductionRecord"].Properties, name)
	}
	for _, name := range []string{"items", "has_more", "next_after_id"} {
		assert.Contains(t, schemas["IntroductionPage"].Properties, name)
	}
	assert.Equal(t, []any{"prepared", "cancelled"}, schemas["IntroductionRecord"].Properties["status"].Enum)
}
