package daemonclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// A qualified ref's '#' must reach the daemon escaped, not as a URL fragment.
func TestLinkKataEvidenceEscapesQualifiedRef(t *testing.T) {
	t.Parallel()
	var linkedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		linkedPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"issue": map[string]any{"uid": "01ISSUE", "ref": "abcd", "qualified_ref": "example#abcd", "project": "example", "title": "Send the budget", "status": "open", "revision": "1"}, "replayed": false}))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)

	_, err = client.LinkKataEvidence(t.Context(), "example#abcd", generated.KataEvidenceLinkRequest{})
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/integrations/kata/issues/example%23abcd/evidence", linkedPath)
}
