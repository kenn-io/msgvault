package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBeeperArchive(t *testing.T) {
	for _, tc := range []struct {
		name                                                                                           string
		before, desired, dry, whole, restricted, wrongSource, wrongAccount, missingState, failReadback bool
		merged, ignoreWrite                                                                            bool
		wantCode                                                                                       string
		writes                                                                                         int
	}{
		{name: "archive", desired: true, whole: true, writes: 1},
		{name: "unarchive", before: true, whole: true, writes: 1},
		{name: "already archived", before: true, desired: true, whole: true},
		{name: "already inbox", whole: true},
		{name: "preview", desired: true, dry: true, whole: true},
		{name: "explicit scope required", desired: true, wantCode: "invalid_request"},
		{name: "sender restricted", desired: true, whole: true, restricted: true, wantCode: "message_not_found"},
		{name: "wrong source", desired: true, whole: true, wrongSource: true, wantCode: "message_not_found"},
		{name: "account mismatch", desired: true, whole: true, wrongAccount: true, wantCode: "stale_identity"},
		{name: "merged chat", desired: true, whole: true, merged: true, wantCode: "stale_identity"},
		{name: "readback mismatch", desired: true, whole: true, ignoreWrite: true, writes: 1, wantCode: "remote_unknown"},
		{name: "unsupported state", desired: true, whole: true, missingState: true, wantCode: "archive_unsupported"},
		{name: "readback failure", desired: true, whole: true, failReadback: true, writes: 1, wantCode: "remote_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("beeper", "fixture-account")
			requirements.NoError(err)
			chatID := "!fixture:beeper.test"
			conv, err := st.EnsureConversation(t.Context(), source.ID, chatID, "Fixture")
			requirements.NoError(err)
			id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "message", MessageType: "chat"})
			requirements.NoError(err)
			var mu sync.Mutex
			archived, writes, reads := tc.before, 0, 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				assertions.Equal("Bearer fixture", r.Header.Get("Authorization"))
				if r.Method == http.MethodPost {
					assertions.Equal("/v1/chats/"+chatID+"/archive", r.URL.Path)
					var payload map[string]bool
					if !assertions.NoError(json.NewDecoder(r.Body).Decode(&payload)) {
						http.Error(w, "invalid fixture request", http.StatusBadRequest)
						return
					}
					assertions.Equal(map[string]bool{"archived": tc.desired}, payload)
					if !tc.ignoreWrite {
						archived = payload["archived"]
					}
					writes++
					w.WriteHeader(http.StatusNoContent)
					return
				}
				reads++
				assertions.Equal(http.MethodGet, r.Method)
				assertions.Equal("/v1/chats/"+chatID, r.URL.Path)
				if writes > 0 && tc.failReadback {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				account := source.Identifier
				if tc.wrongAccount {
					account = "other"
				}
				state := map[string]any{"id": chatID, "accountID": account}
				if tc.merged {
					state["mergedIntoChatID"] = "!another:example.test"
				}
				if !tc.missingState {
					state["isArchived"] = archived
				}
				assertions.NoError(json.NewEncoder(w).Encode(state))
			}))
			t.Cleanup(provider.Close)
			cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
			cfg.Beeper.URL = provider.URL
			requirements.NoError(beeper.SaveToken(cfg.TokensDir(), "fixture"))
			adapter := &storeAPIAdapter{store: st, config: cfg}
			ref := agentgrant.SourceRef{Type: "beeper", Identifier: source.Identifier}
			if tc.restricted {
				ref.IncomingSenderKeys = []string{"codes@example.test"}
			}
			if tc.wrongSource {
				ref.Identifier = "other"
			}
			grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionMessageArchive}, Sources: []agentgrant.SourceRef{ref}}
			gates := 0
			result, err := adapter.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &tc.desired, DryRun: tc.dry, WholeChat: tc.whole}, grant, func(context.Context) (func(), error) { gates++; return func() { gates-- }, nil })
			assertions.Zero(gates)
			if tc.wantCode != "" {
				var failure *emailtags.MessageTagError
				requirements.ErrorAs(err, &failure)
				assertions.Equal(tc.wantCode, failure.Code)
			} else {
				requirements.NoError(err)
				assertions.Equal(chatID, result.ChatID)
				assertions.Equal(tc.desired, result.Archived)
				assertions.Equal(tc.before != tc.desired, result.Changed)
				assertions.Equal(!tc.dry, result.Verified)
			}
			mu.Lock()
			defer mu.Unlock()
			assertions.Equal(tc.writes, writes)
			if tc.restricted || tc.wrongSource || !tc.whole {
				assertions.Zero(reads)
			}
			if tc.dry {
				assertions.Equal(tc.before, archived)
			}
		})
	}
}
