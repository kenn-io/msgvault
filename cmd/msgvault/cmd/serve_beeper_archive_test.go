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
		{name: "unsupported state", desired: true, whole: true, missingState: true, wantCode: "archive_unsupported"},
		{name: "readback failure", desired: true, whole: true, failReadback: true, writes: 1, wantCode: "remote_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("beeper", "fixture-account")
			require.NoError(t, err)
			chatID := "!fixture:beeper.test"
			conv, err := st.EnsureConversation(t.Context(), source.ID, chatID, "Fixture")
			require.NoError(t, err)
			id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "message", MessageType: "chat"})
			require.NoError(t, err)
			var mu sync.Mutex
			archived, writes, reads := tc.before, 0, 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				assert.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
				if r.Method == http.MethodPost {
					assert.Equal(t, "/v1/chats/"+chatID+"/archive", r.URL.Path)
					var payload map[string]bool
					require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
					assert.Equal(t, map[string]bool{"archived": tc.desired}, payload)
					archived = payload["archived"]
					writes++
					w.WriteHeader(http.StatusNoContent)
					return
				}
				reads++
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/v1/chats/"+chatID, r.URL.Path)
				if writes > 0 && tc.failReadback {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				account := source.Identifier
				if tc.wrongAccount {
					account = "other"
				}
				state := map[string]any{"id": chatID, "accountID": account}
				if !tc.missingState {
					state["isArchived"] = archived
				}
				assert.NoError(t, json.NewEncoder(w).Encode(state))
			}))
			t.Cleanup(provider.Close)
			cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
			cfg.Beeper.URL = provider.URL
			require.NoError(t, beeper.SaveToken(cfg.TokensDir(), "fixture"))
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
			assert.Zero(t, gates)
			if tc.wantCode != "" {
				var failure *emailtags.MessageTagError
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, tc.wantCode, failure.Code)
			} else {
				require.NoError(t, err)
				assert.Equal(t, chatID, result.ChatID)
				assert.Equal(t, tc.desired, result.Archived)
				assert.Equal(t, tc.before != tc.desired, result.Changed)
				assert.Equal(t, !tc.dry, result.Verified)
			}
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, tc.writes, writes)
			if tc.restricted || tc.wrongSource || !tc.whole {
				assert.Zero(t, reads)
			}
			if tc.dry {
				assert.Equal(t, tc.before, archived)
			}
		})
	}
}
