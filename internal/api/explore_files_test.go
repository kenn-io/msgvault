package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExploreFilesRejectsUnboundedLimit(t *testing.T) {
	srv := newTestServerWithEngine(t, newExploreDuckDBFixture(t))
	response := postExploreJSON(t, srv, "/api/v1/explore/files", `{"predicate":{},"limit":101}`)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), "limit must be between")
}
