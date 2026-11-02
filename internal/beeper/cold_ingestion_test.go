package beeper

import (
	"encoding/json/v2"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"net/http"
	"net/http/httptest"
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

func coldPolicyChat(count int) *fakeChat {
	ch := mediaChat()
	ch.Type = "group"
	ch.Msgs[0].Text = "retained text"
	ch.Participants = nil
	for i := range count {
		ch.Participants = append(ch.Participants, map[string]any{"id": fmt.Sprintf("user-%d", i), "fullName": fmt.Sprintf("Synthetic %d", i), "isSelf": i == 0})
	}
	return ch
}
func TestImporterColdPolicyRetainsTextAndMediaPolicy(t *testing.T) {
	for _, tc := range []struct {
		name              string
		count             int
		unknown, disabled bool
		override          conversationpolicy.Tier
		want              conversationpolicy.Tier
	}{
		{name: "twenty", count: 20, want: conversationpolicy.Full}, {name: "twenty-one", count: 21, want: conversationpolicy.Cold}, {name: "unknown", count: 21, unknown: true, want: conversationpolicy.Cold},
		{name: "disabled", count: 21, disabled: true, want: conversationpolicy.Full}, {name: "explicit-full", count: 21, override: conversationpolicy.Full, want: conversationpolicy.Full}, {name: "explicit-cold", count: 20, override: conversationpolicy.Cold, want: conversationpolicy.Cold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := newFakeBeeper(t)
			ch := coldPolicyChat(tc.count)
			if tc.unknown {
				ch.ParticipantsTruncated = true
				ch.ParticipantsTotalUnknown = true
				f.failChatGets[ch.ID] = true
			}
			f.addChat(ch)
			imp, st, done := newTestImporter(t, f)
			defer done()
			settings := conversationpolicy.RecommendedSettings()
			if tc.disabled {
				settings.MaxParticipants["beeper"] = 0
			}
			if tc.override != "" {
				settings.Overrides = []conversationpolicy.Override{{SourceType: "beeper", SourceIdentifier: "signal", ConversationID: ch.ID, Tier: tc.override}}
			}
			_, err := st.ConfigureConversationPolicy(t.Context(), settings, time.Now())
			requirements.NoError(err)
			f.setAsset("mxc://beeper.local/photo1", []byte("image bytes"))
			opts := ImportOptions{AccountID: "signal", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{MaxParticipants: 20}}
			sum, err := imp.Import(t.Context(), opts)
			if tc.unknown {
				requirements.ErrorContains(err, "partial Beeper sync")
			} else {
				requirements.NoError(err)
			}
			assertions.EqualValues(1, sum.MessagesProcessed)
			var count int
			requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assertions.Equal(1, count)
			assertions.Equal(tc.want, coldPolicyTier(t, st, ch.ID))
			if tc.count > 20 || tc.unknown {
				assertions.EqualValues(1, sum.AttachmentsSkipped)
				assertions.Zero(sum.AttachmentsDownloaded)
				var mediaReason string
				requirements.NoError(st.DB().QueryRow(`SELECT attachment_skip_reason FROM attachments`).Scan(&mediaReason))
				assertions.Equal(string(attachmentpolicy.SkipParticipantThreshold), mediaReason)
			} else {
				assertions.EqualValues(1, sum.AttachmentsDownloaded)
			}
			state := latestBeeperState(t, imp)
			assertions.True(state.Chats[ch.ID].Done)
			assertions.NotEmpty(state.Chats[ch.ID].Newest)
			if tc.override == conversationpolicy.Cold {
				assertColdPolicyAccountScope(t, st, "beeper", ch.ID)
			}
		})
	}
}
func TestImporterColdPolicySameSyncActivityPromotion(t *testing.T) {
	for _, signal := range []string{"own", "mention", "reply"} {
		t.Run(signal, func(t *testing.T) {
			f := newFakeBeeper(t)
			f.accounts = []map[string]any{{"accountID": "signal", "status": "connected", "bridge": map[string]any{"type": "signal"}}}
			f.pageSize = 1
			ch := coldPolicyChat(21)
			old := time.Now().Add(-181 * 24 * time.Hour)
			recent := time.Now().Add(-time.Hour)
			ch.Msgs = []fakeMsg{{ID: "own", SortKey: 0, Timestamp: old, Text: "old own", SenderID: "user-0", IsSender: true}, {ID: "recent", SortKey: 1, Timestamp: recent, Text: "recent", SenderID: "user-1"}}
			ch.LastActivity = recent
			switch signal {
			case "own":
				ch.Msgs[1].IsSender = true
				ch.Msgs[1].SenderID = "user-0"
			case "mention":
				ch.Msgs[1].Mentions = []string{"user-0"}
			case "reply":
				ch.Msgs[1].LinkedMessageID = "own"
			}
			f.addChat(ch)
			imp, st, done := coldPolicyImporter(t, f)
			defer done()
			sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
			require.NoError(t, err)
			assert.EqualValues(t, 2, sum.MessagesProcessed)
			assert.Equal(t, conversationpolicy.Full, coldPolicyTier(t, st, ch.ID))
		})
	}
}
func TestImporterColdPolicyNewRoomBeforeObservations(t *testing.T) {
	requirements := require.New(t)
	f := newFakeBeeper(t)
	ch := coldPolicyChat(21)
	ch.ParticipantsTruncated = true
	ch.ParticipantsTotalUnknown = true
	f.failChatGets[ch.ID] = true
	f.addChat(ch)
	srv := f.server()
	defer srv.Close()
	st := testutil.NewSQLiteTestStore(t)
	_, err := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER observe_first_insert BEFORE INSERT ON messages BEGIN SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM conversation_policy WHERE conversation_id=NEW.conversation_id AND effective_tier='cold') THEN RAISE(ABORT,'first insert exposed pending room') END; END`)
	requirements.NoError(err)
	_, err = NewImporter(st, NewClient(srv.URL, testToken, 10000)).Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	requirements.ErrorContains(err, "partial Beeper sync")
	assert.Equal(t, conversationpolicy.Cold, coldPolicyTier(t, st, ch.ID))
}
func TestImporterColdPolicyLateNotFoundDoesNotRestoreRoute(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newFakeBeeper(t)
	f.accounts = []map[string]any{{"accountID": "signal", "status": "connected", "bridge": map[string]any{"type": "signal"}}}
	f.pageSize = 1
	ch := coldPolicyChat(21)
	ch.Msgs[0].IsSender = true
	ch.Msgs[0].SenderID = "user-0"
	ch.Msgs[0].Timestamp = time.Now().Add(-time.Hour)
	ch.LastActivity = ch.Msgs[0].Timestamp
	f.addChat(ch)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/accounts/signal" {
			_, _ = w.Write([]byte(`{"accountID":"signal","status":"connected","bridge":{"type":"signal"}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/messages") && r.URL.Query().Get("cursor") != "" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		f.handler()(w, r)
	}))
	defer server.Close()
	st := testutil.NewTestStore(t)
	// Classification starts the 21-member room Cold, so only the imported
	// own message's promotion can make it Full and replay its route.
	_, err := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	_, err = NewImporter(st, NewClient(server.URL, testToken, 10000)).Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	requirements.ErrorContains(err, "partial Beeper sync")
	rows, err := st.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier)
	metadata, err := st.GetConversationMetadataContext(t.Context(), rows[0].ConversationID)
	requirements.NoError(err)
	var m struct {
		Route store.MessagingRouteEvidence `json:"messaging_route"`
	}
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &m))
	assertions.False(m.Route.MembershipComplete, "late missing-chat proof must survive promotion replay")
}

func coldPolicyImporter(t *testing.T, f *fakeBeeper) (*Importer, *store.Store, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/accounts/signal" {
			_, _ = w.Write([]byte(`{"accountID":"signal","status":"connected","bridge":{"type":"signal"}}`))
			return
		}
		f.handler()(w, r)
	}))
	st := testutil.NewTestStore(t)
	// Turn classification on so a large room needs activity to stay full.
	_, err := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	require.NoError(t, err)
	return NewImporter(st, NewClient(srv.URL, testToken, 10000)), st, srv.Close
}

func assertColdPolicyAccountScope(t *testing.T, st *store.Store, sourceType, providerID string) {
	t.Helper()
	other, err := st.GetOrCreateSource(sourceType, "other-account")
	require.NoError(t, err)
	room, err := st.EnsureConversation(other.ID, providerID, "Independent account room")
	require.NoError(t, err)
	require.NoError(t, st.SetConversationMemberCount(room, 20))
	_, err = st.RefreshConversationPolicy(t.Context(), []int64{room}, time.Now())
	require.NoError(t, err)
	policies, err := st.ListConversationPolicies(t.Context())
	require.NoError(t, err)
	for _, p := range policies {
		if p.ConversationID == room {
			assert.Empty(t, p.Override)
			assert.Equal(t, conversationpolicy.Full, p.EffectiveTier)
			return
		}
	}
	require.FailNow(t, "missing other-account policy")
}
