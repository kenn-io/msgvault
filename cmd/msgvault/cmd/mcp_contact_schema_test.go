package cmd

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestMCPDelegatedContactToolsRequireSchema315(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newDraftReplyFixture(t)
	participantID, err := f.store.EnsureParticipant("synthetic-contact-scope@example.test", "Synthetic Contact Scope", "example.test")
	requirements.NoError(err)
	person, _, err := f.store.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)

	server := mcpDraftTestDaemon(t, f.grantedAdapter(), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/health" && r.Header.Get("X-Msgvault-Agent-Token") != "" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.14.0"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueScopedAgentToken(t.Context(), "Synthetic contact grant", []string{"person.read"}, nil,
		[]int64{person.ID}, nil, nil, time.Time{})
	requirements.NoError(err)
	requirements.NotNil(grant)

	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	requirements.NoError(os.WriteFile(tokenFile, []byte(grant.Secret+"\n"), 0o600))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{
		agentURL: server.URL, agentTokenFile: tokenFile, agentAllowInsecure: true,
		agentURLChanged: true, agentTokenChanged: true,
	})
	session := mcpDraftTestSession(ctx, t)
	names := mcpDraftToolNames(t, session)
	for _, name := range []string{
		"get_person_edit_context", "set_person_display_name", "get_person_structured_profile",
		"patch_person_profile", "get_person_attributes", "set_person_attribute", "clear_person_attribute",
	} {
		assertions.NotContains(names, name, "a 3.14 daemon must not advertise 3.15 contact tools")
	}
}
