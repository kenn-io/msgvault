package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Ignoring Handler's admitted caller context would make daemon credentials
// necessary for these real archived reads, contrary to the library contract.
func TestInboxReadHandlerPreservesCallerAdmission(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	mid := f.CreateMessage("caller-admitted-message")
	target := inboxcontrol.Target{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "caller-admitted-message"}
	_, err := f.Store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()})
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, "Synthetic caller body")
	requirements.NoError(err)
	query := url.Values{"source_id": {strconv.FormatInt(f.Source.ID, 10)}, "source_type": {"gmail"}, "source_identifier": {f.Source.Identifier}, "account_id": {f.Source.Identifier}, "scope": {"message"}}
	body, err := json.Marshal(inboxcontrol.ContextRequest{Target: target})
	requirements.NoError(err)
	for _, call := range []struct{ name, method, path, body string }{
		{"candidates", http.MethodGet, "/api/v1/inbox/candidates?" + query.Encode(), ""},
		{"context", http.MethodPost, "/api/v1/inbox/context", string(body)},
	} {
		for _, headers := range []struct{ name, daemon, agent string }{
			{name: "missing headers"},
			{name: "invalid daemon header", daemon: "Bearer invalid-synthetic-key"},
			{name: "invalid agent header", agent: "invalid-synthetic-agent"},
		} {
			for _, admission := range []struct {
				name    string
				allowed bool
				status  int
				reads   int
			}{
				{"admitted", true, http.StatusOK, 1},
				{"rejected", false, http.StatusForbidden, 0},
			} {
				t.Run(call.name+"/"+headers.name+"/"+admission.name, func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					reads := 0
					archive := &inboxCandidateHTTPStore{archive: f.Store, afterRead: func() { reads++ }}
					srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-daemon-key", AgentAccess: true}}, archive, nil, testLogger())
					handler := srv.Handler(func(w http.ResponseWriter, _ *http.Request, _ Operation) bool {
						if !admission.allowed {
							w.WriteHeader(http.StatusForbidden)
						}
						return admission.allowed
					})
					req := httptest.NewRequest(call.method, call.path, strings.NewReader(call.body))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", headers.daemon)
					req.Header.Set(apiprotocol.AgentTokenHeader, headers.agent)
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, req)
					assertions.Equal(admission.status, response.Code)
					assertions.Equal(admission.reads, reads)
					assertions.Equal("no-store", response.Header().Get("Cache-Control"))
					if !admission.allowed {
						assertions.NotContains(response.Body.String(), "caller-admitted-message")
						assertions.NotContains(response.Body.String(), "Synthetic caller body")
						return
					}
					if call.method == http.MethodGet {
						var page inboxcontrol.CandidatePage
						requirements.NoError(json.Unmarshal(response.Body.Bytes(), &page))
						requirements.Len(page.Candidates, 1)
						assertions.Equal(target, page.Candidates[0].State.Target)
					} else {
						var content inboxcontrol.Context
						requirements.NoError(json.Unmarshal(response.Body.Bytes(), &content))
						assertions.Equal(target, content.Target)
						assertions.Equal("Synthetic caller body", content.Text)
					}
				})
			}
		}
	}
}
