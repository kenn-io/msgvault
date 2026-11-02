package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func coldPolicyTier(t *testing.T, st *store.Store, providerID string) conversationpolicy.Tier {
	t.Helper()
	rows, err := st.ListConversationPolicies(t.Context())
	require.NoError(t, err)
	for _, row := range rows {
		if row.ProviderID == providerID {
			return row.EffectiveTier
		}
	}
	require.FailNow(t, "missing policy", providerID)
	return ""
}

func coldPolicyGraph(t *testing.T, count int, signal string) *httptest.Server {
	t.Helper()
	var serverURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if selfChatAbsent(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"cold-chat","chatType":"group"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			if count < 0 {
				http.Error(w, "unavailable", http.StatusBadRequest)
				return
			}
			members := []map[string]any{}
			for i := range count {
				members = append(members, map[string]any{"id": strconv.Itoa(i), "userId": fmt.Sprintf("user-%d", i), "email": fmt.Sprintf("user-%d@example.test", i)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": members})
		case strings.Contains(r.URL.Path, "/messages"):
			msg := map[string]any{"id": "recent", "createdDateTime": time.Now().Add(-time.Hour).Format(time.RFC3339), "lastModifiedDateTime": time.Now().Add(-time.Hour).Format(time.RFC3339), "from": map[string]any{"user": map[string]any{"id": "user-1@example.test", "userIdentityType": "emailUser"}}, "body": map[string]any{"contentType": "html", "content": `retained text<img src="` + serverURL + `/chats/cold-chat/messages/recent/hostedContents/image/$value">`}}
			if signal == "own" {
				msg["from"] = map[string]any{"user": map[string]any{"id": "user-0@example.test", "userIdentityType": "emailUser"}}
			}
			if signal == "mention" {
				msg["mentions"] = []any{map[string]any{"id": 0, "mentionText": "self", "mentioned": map[string]any{"user": map[string]any{"id": "user-0", "userIdentityType": "aadUser"}}}}
			}
			msgs := []any{msg}
			if signal == "reply" {
				msg["replyToId"] = "parent"
				msgs = append(msgs, map[string]any{"id": "parent", "createdDateTime": time.Now().Add(-181 * 24 * time.Hour).Format(time.RFC3339), "lastModifiedDateTime": time.Now().Add(-181 * 24 * time.Hour).Format(time.RFC3339), "from": map[string]any{"user": map[string]any{"id": "user-0@example.test", "userIdentityType": "emailUser"}}, "body": map[string]any{"contentType": "text", "content": "old own"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": msgs})
		default:
			http.Error(w, "absent", http.StatusNotFound)
		}
	}))
	serverURL = srv.URL
	return srv
}
func TestImporterColdPolicyRetainsTextAndMediaPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		count    int
		disabled bool
		override conversationpolicy.Tier
		want     conversationpolicy.Tier
	}{
		{name: "twenty", count: 20, want: conversationpolicy.Full}, {name: "twenty-one", count: 21, want: conversationpolicy.Cold}, {name: "unknown", count: -1, want: conversationpolicy.Cold}, {name: "disabled", count: 21, disabled: true, want: conversationpolicy.Full}, {name: "explicit-full", count: 21, override: conversationpolicy.Full, want: conversationpolicy.Full}, {name: "explicit-cold", count: 20, override: conversationpolicy.Cold, want: conversationpolicy.Cold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			srv := coldPolicyGraph(t, tc.count, "")
			defer srv.Close()
			st := testutil.NewTestStore(t)
			settings := conversationpolicy.RecommendedSettings()
			if tc.disabled {
				settings.MaxParticipants["teams"] = 0
			}
			if tc.override != "" {
				settings.Overrides = []conversationpolicy.Override{{SourceType: "teams", SourceIdentifier: "user-0@example.test", ConversationID: "cold-chat", Tier: tc.override}}
			}
			_, err := st.ConfigureConversationPolicy(t.Context(), settings, time.Now())
			requirements.NoError(err)
			imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50))
			sum, err := imp.Import(t.Context(), ImportOptions{Email: "user-0@example.test", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{MaxParticipants: 1}})
			requirements.NoError(err)
			assertions.EqualValues(1, sum.MessagesAdded)
			assertions.Equal(tc.want, coldPolicyTier(t, st, "cold-chat"))
			assertions.Zero(sum.InlineImagesCopied)
			assertions.EqualValues(1, sum.InlineImagesSkipped)
			var mediaReason string
			requirements.NoError(st.DB().QueryRow(`SELECT attachment_skip_reason FROM attachments`).Scan(&mediaReason))
			assertions.Equal(string(attachmentpolicy.SkipParticipantThreshold), mediaReason)
			run, err := st.GetLatestSync(sum.SourceID)
			requirements.NoError(err)
			state, err := LoadSyncState(run.CursorAfter.String)
			requirements.NoError(err)
			if tc.count < 0 {
				assertions.Empty(state.ChatCursor("cold-chat"))
			} else {
				assertions.NotEmpty(state.ChatCursor("cold-chat"))
			}
		})
	}
}
func TestImporterColdPolicySameSyncActivityPromotion(t *testing.T) {
	for _, signal := range []string{"own", "mention", "reply"} {
		t.Run(signal, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			srv := coldPolicyGraph(t, 21, signal)
			defer srv.Close()
			st := testutil.NewTestStore(t)
			_, policyErr := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
			require.NoError(t, policyErr)
			source, err := st.GetOrCreateSource("teams", "user-0@example.test")
			requirements.NoError(err)
			requirements.NoError(st.AddAccountIdentity(source.ID, "user-0@example.test", "manual"))
			sum, importErr := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)).Import(t.Context(), ImportOptions{Email: "user-0@example.test"})
			requirements.NoError(importErr)
			wantMessages := 1
			if signal == "reply" {
				wantMessages = 2
			}
			assertions.EqualValues(wantMessages, sum.MessagesProcessed)
			assertions.Equal(conversationpolicy.Full, coldPolicyTier(t, st, "cold-chat"))
		})
	}
}

func TestImporterColdPolicyChatStandardPrivateChannels(t *testing.T) {
	for _, signal := range []string{"quiet", "mention", "reply"} {
		t.Run(signal, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
			requirements.NoError(err)
			source, err := st.GetOrCreateSource("teams", "user-0@example.test")
			requirements.NoError(err)
			requirements.NoError(st.AddAccountIdentity(source.ID, "user-0@example.test", "manual"))
			serverURL := ""
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if selfChatAbsent(w, r) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/me/chats":
					_, _ = w.Write([]byte(`{"value":[]}`))
				case r.URL.Path == "/me/joinedTeams":
					_, _ = w.Write([]byte(`{"value":[{"id":"team","displayName":"Synthetic team"}]}`))
				case r.URL.Path == "/teams/team/channels":
					_, _ = w.Write([]byte(`{"value":[{"id":"standard","membershipType":"standard"},{"id":"private","membershipType":"private"}]}`))
				case strings.HasSuffix(r.URL.Path, "/members"):
					count := 21
					if strings.Contains(r.URL.Path, "/private/") {
						count = 20
					}
					members := []any{}
					for i := range count {
						members = append(members, map[string]any{"id": strconv.Itoa(i), "userId": fmt.Sprintf("user-%d", i), "email": fmt.Sprintf("user-%d@example.test", i)})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"value": members})
				case strings.HasSuffix(r.URL.Path, "/replies"):
					// Graph serves channel replies only through the parent's
					// replies collection, never through /messages.
					replies := []any{}
					if signal == "reply" && strings.HasSuffix(r.URL.Path, "/messages/parent/replies") {
						now := time.Now().Add(-time.Hour).Format(time.RFC3339)
						replies = append(replies, map[string]any{"id": "recent", "replyToId": "parent", "createdDateTime": now, "lastModifiedDateTime": now, "from": map[string]any{"user": map[string]any{"id": "user-1@example.test", "userIdentityType": "emailUser"}}, "body": map[string]any{"contentType": "text", "content": "retained text"}})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"value": replies})
				case strings.HasSuffix(r.URL.Path, "/delta"):
					_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}, "@odata.deltaLink": serverURL + "/delta"})
				case strings.HasSuffix(r.URL.Path, "/messages"):
					now := time.Now().Add(-time.Hour).Format(time.RFC3339)
					old := time.Now().Add(-181 * 24 * time.Hour).Format(time.RFC3339)
					msg := map[string]any{"id": "recent", "createdDateTime": now, "lastModifiedDateTime": now, "from": map[string]any{"user": map[string]any{"id": "user-1@example.test", "userIdentityType": "emailUser"}}, "body": map[string]any{"contentType": "text", "content": "retained text"}}
					msgs := []any{msg}
					if signal == "mention" {
						msg["mentions"] = []any{map[string]any{"id": 0, "mentioned": map[string]any{"user": map[string]any{"id": "user-0", "userIdentityType": "aadUser"}}}}
					}
					if signal == "reply" {
						// Only the old own parent is a top-level post; the recent
						// reply arrives through its replies collection.
						msgs = []any{map[string]any{"id": "parent", "createdDateTime": old, "lastModifiedDateTime": old, "from": map[string]any{"user": map[string]any{"id": "user-0@example.test", "userIdentityType": "emailUser"}}, "body": map[string]any{"contentType": "text", "content": "old own"}}}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"value": msgs})
				default:
					http.Error(w, "absent", http.StatusNotFound)
				}
			}))
			defer srv.Close()
			serverURL = srv.URL
			sum, err := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)).Import(t.Context(), ImportOptions{Email: "user-0@example.test", IncludeChannels: true})
			requirements.NoError(err)
			assertions.EqualValues(2, sum.ChannelsProcessed)
			want := conversationpolicy.Cold
			if signal != "quiet" {
				want = conversationpolicy.Full
			}
			assertions.Equal(want, coldPolicyTier(t, st, "team/standard"))
			assertions.Equal(conversationpolicy.Full, coldPolicyTier(t, st, "team/private"))
		})
	}
}

// A roster the archive failed to record must not feed the policy refresh: the
// refresh would classify the room from stale membership.
func TestImporterStopsWhenRosterCannotBeRecorded(t *testing.T) {
	requirements := require.New(t)
	srv := coldPolicyGraph(t, 21, "")
	defer srv.Close()
	st := testutil.NewSQLiteTestStore(t)
	_, err := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER fail_roster_write BEFORE UPDATE OF metadata ON conversations
		BEGIN SELECT RAISE(ABORT, 'synthetic roster write failure'); END`)
	requirements.NoError(err)
	sum, err := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)).Import(t.Context(), ImportOptions{Email: "user-0@example.test"})
	requirements.ErrorContains(err, "synthetic roster write failure")
	assert.Zero(t, sum.MessagesProcessed, "messages are not imported against unrecorded membership")
}
