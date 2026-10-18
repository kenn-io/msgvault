package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type demotingUserLookup struct {
	store  *store.Store
	source int64
	calls  int
}

func (l *demotingUserLookup) GetUser(ctx context.Context, id string) (*GraphUser, error) {
	l.calls++
	if err := l.store.SetConversationPolicyOverride(ctx, l.source, "origin-room", conversationpolicy.Cold); err != nil {
		return nil, err
	}
	return &GraphUser{ID: id, Mail: "teams-origin@example.test"}, nil
}

func TestTeamsOriginDemotionDuringDirectoryLookup(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(1)
	src, err := st.GetOrCreateSource("teams", "owner@example.test")
	requirements.NoError(err)
	room, err := st.EnsureConversation(src.ID, "origin-room", "Synthetic")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), src.ID, "origin-room", conversationpolicy.Full))
	full, err := st.ConversationFullEligibleContext(t.Context(), room)
	requirements.NoError(err)
	requirements.True(full)
	lookup := &demotingUserLookup{store: st, source: src.ID}
	resolver := newParticipantResolver(st, lookup)
	pid, err := resolver.resolve(t.Context(), &Identity{ID: "synthetic-object", UserIdentityType: "aadUser", DisplayName: "Fetched directory name"}, room)
	requirements.NoError(err)
	assertions.NotZero(pid)
	assertions.Equal(1, lookup.calls)
	assertions.Empty(resolver.mail, "rejected directory data must not enter the shared rich cache")
	var rich int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='teams-origin@example.test' OR display_name='Fetched directory name'`).Scan(&rich))
	assertions.Zero(rich, "directory completion must not authorize rich writes after demotion")
}

func TestTeamsTeamRosterFetchDoesNotAdmitRichIdentity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[{"id":"synthetic-member","userId":"synthetic-object","email":"teams-roster@example.test","displayName":"Fetched team name"}]}`))
	}))
	defer srv.Close()
	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic", nil }, 50))
	roster := imp.resolveTeamRoster(t.Context(), "synthetic-team")
	requirements.NoError(roster.err)
	assertions.Equal(1, roster.memberCount)
	var rich int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='teams-roster@example.test' OR display_name='Fetched team name'`).Scan(&rich))
	assertions.Zero(rich, "team membership fetch has no originating conversation")
	assertions.Empty(imp.res.cache)
	assertions.Empty(imp.res.mail)

	src, err := st.GetOrCreateSource("teams", "owner@example.test")
	requirements.NoError(err)
	room, err := st.EnsureConversation(src.ID, "retained-room", "Synthetic")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), src.ID, "retained-room", conversationpolicy.Full))
	full, err := st.ConversationFullEligibleContext(t.Context(), room)
	requirements.NoError(err)
	requirements.True(full)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), src.ID, "retained-room", conversationpolicy.Cold))
	imp.client = nil // Retained roster replay needs no new Graph request.
	ids := imp.resolveRosterMembers(t.Context(), room, roster.members)
	requirements.Len(ids, 1)
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='teams-roster@example.test' OR display_name='Fetched team name'`).Scan(&rich))
	assertions.Zero(rich)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), src.ID, "retained-room", conversationpolicy.Full))
	ids = imp.resolveRosterMembers(t.Context(), room, roster.members)
	requirements.Len(ids, 1)
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='teams-roster@example.test'`).Scan(&rich))
	assertions.Equal(1, rich, "retained profile and bare cache promote against current Full")
	cached := imp.res.cache["synthetic-object"]
	roster.members[0].Email = "changed-cold@example.test"
	roster.members[0].DisplayName = "Changed cold name"
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), src.ID, "retained-room", conversationpolicy.Cold))
	ids = imp.resolveRosterMembers(t.Context(), room, roster.members)
	requirements.Len(ids, 1)
	assertions.Equal(cached, imp.res.cache["synthetic-object"])
	assertions.Equal("teams-roster@example.test", imp.res.mail["synthetic-object"], "Cold replay cannot replace admitted mail cache")
	now := time.Now().UTC()
	message := &ChatMessage{ID: "retained-message", CreatedDateTime: now, LastModifiedDateTime: now, From: &IdentitySet{User: &Identity{ID: "cold-sender@example.test", DisplayName: "Cold sender", UserIdentityType: "emailUser"}}, Body: MessageBody{ContentType: "text", Content: "Retained Teams text"}, Mentions: []Mention{{Mentioned: &IdentitySet{User: &Identity{ID: "synthetic-object", DisplayName: "Cold mention", UserIdentityType: "aadUser"}}}}, Reactions: []Reaction{{ReactionType: "like", CreatedDateTime: now, User: &IdentitySet{User: &Identity{ID: "cold-reactor@example.test", DisplayName: "Cold reactor", UserIdentityType: "emailUser"}}}}, Raw: []byte(`{"id":"retained-message","body":{"contentType":"text","content":"Retained Teams text"}}`)}
	_, persisted, err := imp.persistMessage(t.Context(), room, src.ID, "retained-message", message, ImportOptions{}, &ImportSummary{}, nil)
	requirements.NoError(err)
	requirements.True(persisted)
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address IN ('changed-cold@example.test','cold-sender@example.test','cold-reactor@example.test') OR display_name IN ('Changed cold name','Cold sender','Cold mention','Cold reactor')`).Scan(&rich))
	assertions.Zero(rich, "cached roster and every message identity still require current origin admission")
	archived, err := st.InspectMessage("retained-message")
	requirements.NoError(err)
	assertions.Equal("Retained Teams text", archived.BodyText)
	assertions.Equal(1, archived.RecipientCounts["mention"])
	var reactions int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	assertions.Equal(1, reactions)
}
