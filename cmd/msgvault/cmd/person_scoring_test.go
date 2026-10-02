package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func runPersonScoringCLI(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	localFlag := rootCmd.PersistentFlags().Lookup("local")
	savedChanged := localFlag.Changed
	t.Cleanup(func() { localFlag.Changed = savedChanged })
	root.PersistentFlags().AddFlag(localFlag)
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonScoringCommand())
	root.AddCommand(person)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"person", "scoring"}, args...))
	err := root.ExecuteContext(ctx)
	return output.String(), err
}

func TestPersonScoringCLIUsesDaemonScoringRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	type request struct{ method, path, query, body string }
	requests := make(chan request, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- request{r.Method, r.URL.Path, r.URL.RawQuery, string(body)}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/identity/scoring/status":
			_, _ = io.WriteString(w, `{"enabled":true,"model_id":"jev-1.13.0","minimum_probability":0.8,"disclosure":{"endpoint":"https://api.typesafe.ai/v1/systemone","model_id":"jev-1.13.0","packet_schema":"person-match-packet-v1","retention_declaration":"fixture policy","policy_version":"person-match-policy-v1"},"disclosure_fingerprint":"fixture-fingerprint","credential_available":true,"consent_active":true,"ready":true,"data_fields":"fixture data fields"}`)
		case "/api/v1/identity/scoring/consent", "/api/v1/identity/scoring/revoke":
			_, _ = io.WriteString(w, `{"disclosure_fingerprint":"fixture-fingerprint","consent_active":true,"changed":true}`)
		case "/api/v1/identity/scoring/run":
			_, _ = io.WriteString(w, `{"results":[{"candidate_id":17,"review_token":"token-17","model_id":"jev-1.13.0","packet_schema":"person-match-packet-v1","policy_version":"person-match-policy-v1","evidence_classes":["email"],"probability":0.81,"proposed_action":"needs_review","blockers":["independent_identity_evidence_required"],"status":"scored"}],"processed":1}`)
		case "/api/v1/identity/scoring/history":
			_, _ = io.WriteString(w, `{"judgments":[],"limit":2,"candidate_id":17}`)
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})

	out, err := runPersonScoringCLI(ctx, t, "status", "--json")
	require.NoError(err)
	assert.Contains(out, `"disclosure_fingerprint":"fixture-fingerprint"`)
	assert.Equal("/api/v1/identity/scoring/status", (<-requests).path)

	_, err = runPersonScoringCLI(ctx, t, "consent", "fixture-fingerprint")
	require.NoError(err)
	decision := <-requests
	assert.Equal("/api/v1/identity/scoring/consent", decision.path)
	var body map[string]any
	require.NoError(json.Unmarshal([]byte(decision.body), &body))
	assert.Equal("fixture-fingerprint", body["disclosure_fingerprint"])

	out, err = runPersonScoringCLI(ctx, t, "run", "--limit", "1", "--json")
	require.NoError(err)
	assert.Contains(out, `"candidate_id":17`)
	assert.Equal("/api/v1/identity/scoring/run", (<-requests).path)

	out, err = runPersonScoringCLI(ctx, t, "run")
	require.NoError(err)
	assert.Contains(out, "Candidate 17:")
	assert.Equal("/api/v1/identity/scoring/run", (<-requests).path)

	out, err = runPersonScoringCLI(ctx, t, "history", "--candidate-id", "17", "--limit", "2", "--before-id", "99", "--json")
	require.NoError(err)
	assert.Contains(out, `"judgments":[]`)
	historyRequest := <-requests
	assert.Equal("/api/v1/identity/scoring/history", historyRequest.path)
	assert.Contains(historyRequest.query, "before_id=99")

	_, err = runPersonScoringCLI(ctx, t, "revoke", "fixture-fingerprint")
	require.NoError(err)
	assert.Equal("/api/v1/identity/scoring/revoke", (<-requests).path)
}

func TestPersonScoringConsentHelpDescriptions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	consentHelp, err := runPersonScoringCLI(t.Context(), t, "consent", "--help")
	require.NoError(err)
	assert.Contains(consentHelp, "Consent to the exact current disclosure")
	assert.NotContains(consentHelp, "Consent consent")

	revokeHelp, err := runPersonScoringCLI(t.Context(), t, "revoke", "--help")
	require.NoError(err)
	assert.Contains(revokeHelp, "Revoke consent for the exact current disclosure")
}

func TestPersonScoringCLIRetainsPartialResultsOnBatchError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/identity/scoring/run", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"candidate_id":17,"review_token":"token-17","proposed_action":"needs_review","blockers":[],"status":"scored"}],"processed":1,"error":{"code":"consent_required","message":"Scoring stopped because consent was withdrawn"}}`)
	}))
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	for _, tc := range []struct {
		name   string
		args   []string
		result string
	}{
		{"text", []string{"run"}, "Candidate 17:"},
		{"json", []string{"run", "--json"}, `"candidate_id":17`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runPersonScoringCLI(ctx, t, tc.args...)
			assert.Contains(t, out, tc.result)
			assert.ErrorContains(t, err, "consent_required")
		})
	}
}
