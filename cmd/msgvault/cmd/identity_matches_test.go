package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
)

const identityMatchesCLICandidate = `{"id":17,"left_kind":"participant","left_id":7,"right_kind":"participant","right_id":8,"basis":"email","source":"archive_observation","state":"candidate","review_token":"review-token-17","actionable":true,"application_pending":false,"evidence":[{"id":4,"candidate_id":17,"evidence_kind":"email","source":"archive_observation","created_at":"2026-01-01T00:00:00Z"}],"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`

func runIdentityMatchesCLI(ctx context.Context, t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	localFlag := rootCmd.PersistentFlags().Lookup("local")
	savedChanged := localFlag.Changed
	t.Cleanup(func() { localFlag.Changed = savedChanged })
	root.PersistentFlags().AddFlag(localFlag)
	identity := &cobra.Command{Use: "identity"}
	identity.AddCommand(newIdentityMatchesCommand())
	root.AddCommand(identity)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(io.Discard)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"identity", "matches"}, args...))
	err := root.ExecuteContext(ctx)
	return output.String(), err
}

func TestIdentityMatchesCLIReviewUsesDaemonRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	type recorded struct {
		method, path, query, body string
	}
	requests := make(chan recorded, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(body)}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/identity/match-candidates":
			_, _ = fmt.Fprintf(w, `{"candidates":[%s],"limit":2,"offset":3}`, identityMatchesCLICandidate)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/identity/match-candidates/17":
			_, _ = io.WriteString(w, identityMatchesCLICandidate)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/review/accept"):
			if strings.Contains(string(body), `"stale-token"`) {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":"identity_match_review_stale","message":"The match changed"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"candidate":%s,"identity_revision":9,"cache_state":"ready"}`,
				strings.Replace(identityMatchesCLICandidate, `"state":"candidate"`, `"state":"accepted"`, 1))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/review/reject"):
			_, _ = fmt.Fprintf(w, `{"candidate":%s,"identity_revision":9,"cache_state":"ready"}`,
				strings.Replace(identityMatchesCLICandidate, `"state":"candidate"`, `"state":"rejected"`, 1))
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})

	out, err := runIdentityMatchesCLI(t.Context(), t, "", "list", "--state", "candidate", "--limit", "2", "--offset", "3", "--json")
	require.NoError(err)
	assert.Contains(out, `"review_token":"review-token-17"`)
	list := <-requests
	assert.Equal("/api/v1/identity/match-candidates", list.path)
	assert.Contains(list.query, "offset=3")
	assert.Contains(list.query, "limit=2")

	out, err = runIdentityMatchesCLI(t.Context(), t, "", "show", "17", "--json")
	require.NoError(err)
	assert.Contains(out, `"evidence"`)
	assert.Equal("/api/v1/identity/match-candidates/17", (<-requests).path)
	out, err = runIdentityMatchesCLI(t.Context(), t, "", "show", "17")
	require.NoError(err)
	assert.Contains(out, "Review token: review-token-17")
	assert.Contains(out, "- email")
	assert.Equal("/api/v1/identity/match-candidates/17", (<-requests).path)

	out, err = runIdentityMatchesCLI(t.Context(), t, "", "accept", "17", "--review-token", "review-token-17", "--json")
	require.NoError(err)
	assert.Contains(out, `"identity_revision":9`)
	accepted := <-requests
	assert.Equal("/api/v1/identity/match-candidates/17/review/accept", accepted.path)
	var acceptedBody map[string]any
	require.NoError(json.Unmarshal([]byte(accepted.body), &acceptedBody))
	assert.Equal("review-token-17", acceptedBody["review_token"])

	_, err = runIdentityMatchesCLI(t.Context(), t, "Different people", "reject", "17",
		"--review-token", "review-token-17", "--notes-stdin")
	require.NoError(err)
	rejected := <-requests
	assert.Equal("/api/v1/identity/match-candidates/17/review/reject", rejected.path)
	var rejectedBody map[string]any
	require.NoError(json.Unmarshal([]byte(rejected.body), &rejectedBody))
	assert.Equal("Different people", rejectedBody["notes"])

	notesPath := filepath.Join(t.TempDir(), "review-notes.txt")
	require.NoError(os.WriteFile(notesPath, []byte("Confirmed from another source\n"), 0o600))
	out, err = runIdentityMatchesCLI(t.Context(), t, "", "accept", "17",
		"--review-token", "review-token-17", "--notes-file", notesPath)
	require.NoError(err)
	assert.Contains(out, "Next: Inspect the resulting person and CardDAV publication.")
	fromFile := <-requests
	var fileBody map[string]any
	require.NoError(json.Unmarshal([]byte(fromFile.body), &fileBody))
	assert.Equal("Confirmed from another source", fileBody["notes"])

	_, err = runIdentityMatchesCLI(t.Context(), t, "", "accept", "17",
		"--review-token", "stale-token")
	require.Error(err)
	var apiErr *daemonclient.APIError
	require.ErrorAs(err, &apiErr)
	assert.Equal(http.StatusConflict, apiErr.Status)
	assert.Equal("identity_match_review_stale", apiErr.Code)
	assert.Equal("/api/v1/identity/match-candidates/17/review/accept", (<-requests).path)

	_, err = runIdentityMatchesCLI(t.Context(), t, "", "accept", "17")
	require.Error(err)
	require.ErrorContains(err, "--review-token")

	_, err = runIdentityMatchesCLI(t.Context(), t, "", "show", "0")
	require.Error(err)
	require.ErrorContains(err, "positive")
}
