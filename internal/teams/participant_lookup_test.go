package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func lookupMessage(id string) *ChatMessage {
	now := time.Now().UTC()
	identity := &IdentitySet{User: &Identity{ID: "lookup-object", DisplayName: "Fetched name", UserIdentityType: "aadUser"}}
	return &ChatMessage{ID: id, CreatedDateTime: now, LastModifiedDateTime: now, From: identity,
		Body:      MessageBody{ContentType: "text", Content: "Retained lookup text"},
		Mentions:  []Mention{{Mentioned: identity}},
		Reactions: []Reaction{{ReactionType: "like", CreatedDateTime: now, User: identity}},
		Raw:       []byte(`{"body":{"content":"Retained lookup text"}}`)}
}

func lookupRoom(t *testing.T, st *store.Store) (int64, int64) {
	t.Helper()
	st.DB().SetMaxOpenConns(1)
	source, err := st.GetOrCreateSource("teams", "owner@example.test")
	require.NoError(t, err)
	room, err := st.EnsureConversation(source.ID, "lookup-room", "Synthetic")
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), source.ID, "lookup-room", conversationpolicy.Cold))
	return source.ID, room
}

func TestTeamsColdLookupMemoPreservesAdmission(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	source, room := lookupRoom(t, st)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source, "lookup-room", conversationpolicy.Full))
	work, err := st.CaptureConversationPolicyWork(t.Context())
	requirements.NoError(err)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := st.SetConversationPolicyOverride(r.Context(), source, "lookup-room", conversationpolicy.Cold); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"lookup-object","mail":"lookup@example.test"}`))
	}))
	defer srv.Close()
	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic", nil }, 1000))
	for _, id := range []string{"lookup-1", "lookup-2"} {
		_, persisted, err := imp.persistMessage(t.Context(), room, source, id, lookupMessage(id), ImportOptions{}, &ImportSummary{}, nil)
		requirements.NoError(err)
		requirements.True(persisted)
		archived, err := st.InspectMessage(id)
		requirements.NoError(err)
		assertions.Equal("Retained lookup text", archived.BodyText)
		assertions.Equal(1, archived.RecipientCounts["mention"])
	}
	assertions.EqualValues(1, calls.Load(), "sender, mention and reaction reuse the raw lookup after HTTP demotion")
	assertions.Empty(imp.res.mail)
	var rich, reactions int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address IS NOT NULL OR COALESCE(display_name,'')<>''`).Scan(&rich))
	assertions.Zero(rich)
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	assertions.Equal(2, reactions)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source, "lookup-room", conversationpolicy.Full))
	identity := lookupMessage("unused").From.User
	_, err = imp.res.resolve(work, identity, room)
	requirements.NoError(err)
	assertions.Empty(imp.res.mail, "memo cannot refresh original captured authority")
	_, err = imp.res.resolve(t.Context(), identity, room)
	requirements.NoError(err)
	assertions.Equal("lookup@example.test", imp.res.mail["lookup-object"])
	assertions.EqualValues(1, calls.Load(), "Full promotion reuses fetched data but rechecks admission")
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source, "lookup-room", conversationpolicy.Cold))
	identity.DisplayName = "Rejected cold name"
	_, err = imp.res.resolve(t.Context(), identity, room)
	requirements.NoError(err)
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE display_name='Rejected cold name'`).Scan(&rich))
	assertions.Zero(rich)
	assertions.Equal("lookup@example.test", imp.res.mail["lookup-object"])
	assertions.EqualValues(1, calls.Load())
}

func TestTeamsColdRosterSeedsRawLookup(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	source, room := lookupRoom(t, st)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/teams/synthetic/members" {
			_, _ = w.Write([]byte(`{"value":[{"id":"member","userId":"lookup-object","email":"roster@example.test","displayName":"Roster name"}]}`))
			return
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"id":"lookup-object","mail":"unexpected@example.test"}`))
	}))
	defer srv.Close()
	imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic", nil }, 1000))
	roster := imp.resolveTeamRoster(t.Context(), "synthetic")
	requirements.NoError(roster.err)
	assertions.Empty(imp.res.mail)
	requirements.Len(imp.resolveRosterMembers(t.Context(), room, roster.members), 1)
	for _, id := range []string{"roster-1", "roster-2"} {
		_, persisted, err := imp.persistMessage(t.Context(), room, source, id, lookupMessage(id), ImportOptions{}, &ImportSummary{}, nil)
		requirements.NoError(err)
		requirements.True(persisted)
	}
	assertions.Zero(calls.Load(), "fetched roster email supplies repeated Cold message identities")
	assertions.Empty(imp.res.mail)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source, "lookup-room", conversationpolicy.Full))
	_, err := imp.res.resolve(t.Context(), lookupMessage("unused").From.User, room)
	requirements.NoError(err)
	assertions.Equal("roster@example.test", imp.res.mail["lookup-object"])
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source, "lookup-room", conversationpolicy.Cold))
	roster.members[0].Email = "new-roster@example.test"
	requirements.Len(imp.resolveRosterMembers(t.Context(), room, roster.members), 1)
	_, err = imp.res.resolve(t.Context(), lookupMessage("unused").From.User, room)
	requirements.NoError(err)
	assertions.Equal("roster@example.test", imp.res.mail["lookup-object"], "raw observation cannot replace admitted mail")
	var rich int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='new-roster@example.test'`).Scan(&rich))
	assertions.Zero(rich)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source, "lookup-room", conversationpolicy.Full))
	_, err = imp.res.resolve(t.Context(), lookupMessage("unused").From.User, room)
	requirements.NoError(err)
	assertions.Equal("new-roster@example.test", imp.res.mail["lookup-object"])
	assertions.Zero(calls.Load())
}

func TestTeamsNegativeLookupMemoExpiresWithResolver(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			st := testutil.NewTestStore(t)
			source, room := lookupRoom(t, st)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"id":"lookup-object","userPrincipalName":"guest#EXT#@example.test"}`))
			}))
			defer srv.Close()
			imp := NewImporter(st, NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic", nil }, 1000))
			for _, id := range []string{"negative-1", "negative-2"} {
				_, persisted, err := imp.persistMessage(t.Context(), room, source, id, lookupMessage(id), ImportOptions{}, &ImportSummary{}, nil)
				requirements.NoError(err)
				requirements.True(persisted)
			}
			assertions.EqualValues(1, calls.Load(), "empty and failed directory results are memo hits")
			assertions.Empty(imp.res.mail)
			// PostgreSQL holds a connection for the sync lock and needs another
			// connection for the source lifecycle check.
			st.DB().SetMaxOpenConns(2)
			run, err := st.StartSync(source, "teams")
			requirements.NoError(err)
			fresh := imp.scopedToSync(source, run)
			_, err = fresh.res.resolve(t.Context(), lookupMessage("unused").From.User, room)
			requirements.NoError(err)
			assertions.EqualValues(2, calls.Load(), "fresh sync retries prior negative result")
			_, err = imp.res.resolveMember(t.Context(), ChatMember{UserID: "lookup-object", Email: "recovered@example.test"}, room)
			requirements.NoError(err)
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source, "lookup-room", conversationpolicy.Full))
			_, err = imp.res.resolve(t.Context(), lookupMessage("unused").From.User, room)
			requirements.NoError(err)
			assertions.Equal("recovered@example.test", imp.res.mail["lookup-object"])
			assertions.EqualValues(2, calls.Load(), "new roster proof replaces the negative memo without HTTP")
		})
	}
}
