package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestDaemonInboxBeeperNativeSignedArchive(t *testing.T) {
	for _, loseResponse := range []bool{false, true} {
		name := "verified"
		if loseResponse {
			name = "recover without redispatch"
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			a, target, _, _ := inboxBindingFixture(t, "beeper")
			a.inboxProviderFactory = nil
			archived, posts := false, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/chats/"+target.ProviderID+"/messages":
					_, _ = w.Write([]byte(`{"items":[],"hasMore":false}`))
				case r.Method == http.MethodGet && r.URL.Path == "/v1/accounts":
					_ = json.NewEncoder(w).Encode([]map[string]string{{"accountID": target.AccountID}})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/chats/"+target.ProviderID:
					assert.Equal(t, "1", r.URL.Query().Get("maxParticipantCount"))
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": target.ProviderID, "accountID": target.AccountID,
						"isArchived": archived, "isMarkedUnread": true, "unreadCount": 0,
						"capabilities": map[string]bool{"archive": true, "markAsUnread": true},
						"draft":        map[string]string{"text": "keep this composer"},
					})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/chats/"+target.ProviderID+"/archive":
					posts++
					var body map[string]bool
					if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
						return
					}
					assert.Equal(t, map[string]bool{"archived": true}, body)
					archived = true
					if loseResponse {
						w.WriteHeader(http.StatusServiceUnavailable)
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
				default:
					assert.Fail(t, "unexpected provider access", "%s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			a.config = &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
			a.config.Beeper.URL = server.URL
			requirements.NoError(beeper.SaveToken(a.config.TokensDir(), "fixture-token"))
			owner := inboxcontrol.Principal{ID: "owner", Owner: true}
			authorize := func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }
			gates, releases := 0, 0
			gate := func(context.Context) (func(), error) {
				gates++
				return func() { releases++ }, nil
			}
			request := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}
			preview, err := a.ControlInbox(t.Context(), request, owner, authorize, gate)
			requirements.NoError(err)
			requirements.NotNil(preview.Before)
			requirements.NotNil(preview.Projected)
			assertions.True(*preview.Before.Inbox)
			assertions.False(*preview.Projected.Inbox)
			assertions.Equal(0, posts)
			assertions.Equal(0, gates)
			request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "beeper-archive"
			result, err := a.ControlInbox(t.Context(), request, owner, authorize, gate)
			if loseResponse {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
			} else {
				requirements.NoError(err)
			}
			requirements.NotNil(result.Receipt)
			if loseResponse {
				assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
				observation, err := a.store.GetInboxProviderState(t.Context(), target)
				requirements.NoError(err)
				assertions.Nil(observation)
				result, err = a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, owner, authorize, gate)
				requirements.NoError(err)
			}
			assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
			observation, err := a.store.GetInboxProviderState(t.Context(), target)
			requirements.NoError(err)
			requirements.NotNil(observation)
			assertions.False(*observation.Inbox)
			assertions.False(*observation.Read)
			assertions.True(*observation.MarkedUnread)
			replay, err := a.ControlInbox(t.Context(), request, owner, authorize, gate)
			requirements.NoError(err)
			assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
			assertions.Equal(1, posts)
			assertions.Equal(gates, releases)
			source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
			capabilities, err := a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source}, owner, authorize, gate)
			requirements.NoError(err)
			requirements.NotNil(capabilities.Capabilities)
			for _, capability := range capabilities.Capabilities.Operations {
				if capability.Operation.IsMutation() {
					assertions.NotEqual(inboxcontrol.CapabilitySupported, capability.Status, "source discovery cannot assert chat-specific native support")
				}
			}
			assertions.Equal(1, posts)
		})
	}
}
