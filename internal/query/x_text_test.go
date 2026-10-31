package query_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/x"
)

func TestXWrittenRepliesAppearInTextSearchChatAndPeople(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	// Reply-first input exercises the real writer's deferred parent resolution.
	page := x.Page{Posts: []x.Post{
		{ID: "102", AuthorID: "1002", ConversationID: "101", Text: "Synthetic constellation reply", CreatedAt: at.Add(time.Minute), References: []x.Reference{{Type: "replied_to", ID: "101"}}},
		{ID: "101", AuthorID: "1001", ConversationID: "101", Text: "Synthetic constellation root", CreatedAt: at},
	}, Users: []x.User{
		{ID: "1001", Username: "example_owner", Name: "Example Owner", ProtectedKnown: true},
		{ID: "1002", Username: "example_reply", Name: "Example Reply", ProtectedKnown: true},
	}}
	require.NoError(t, st.AddAccountIdentityContext(t.Context(), source.ID, "1001", "x_owner"))
	require.NoError(t, x.WritePage(t.Context(), st, source.ID, "1001", page, at))
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	results, err := engine.SearchFast(t.Context(), search.Parse("constellation"), query.MessageFilter{SourceID: &source.ID, MessageType: "x"}, 10, 0)
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, row := range results {
		assert.Equal(t, source.ID, row.SourceID)
		detail, err := engine.GetMessage(t.Context(), row.ID)
		require.NoError(t, err)
		assert.Equal(t, row.SourceMessageID == "101", detail.IsFromMe)
	}
	assert.True(t, query.IsKnownMessageType("x"), "the real search CLI validator must admit the stored type")
	assert.False(t, query.IsKnownMessageType("x_typo"))
	if texts, ok := engine.(query.TextEngine); ok {
		conversations, err := texts.ListConversations(t.Context(), query.TextFilter{SourceID: &source.ID})
		require.NoError(t, err)
		require.Len(t, conversations, 1)
		assert.Equal(t, int64(2), conversations[0].MessageCount)
		messages, err := texts.ListConversationMessages(t.Context(), conversations[0].ConversationID, query.TextFilter{})
		require.NoError(t, err)
		assert.Len(t, messages, 2)
		hits, err := texts.TextSearch(t.Context(), "constellation", &source.ID, 10, 0)
		require.NoError(t, err)
		assert.Len(t, hits, 2)
	}
	chats, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "example_reply", SourceID: source.ID})
	require.NoError(t, err)
	require.Len(t, chats.Results, 1)
	assert.Equal(t, "x", chats.Results[0].SourceType)
	observations, err := st.FindObservationsByAddressContext(t.Context(), store.ContactPointQuery{AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"), NormalizedValue: "example_reply"})
	require.NoError(t, err)
	require.Len(t, observations, 1)
	person, _, err := st.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), observations[0].ParticipantID, new("Curated Reply"))
	require.NoError(t, err)
	directory, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "Curated Reply"})
	require.NoError(t, err)
	require.Len(t, directory.People, 1)
	assert.Equal(t, person.ID, directory.People[0].ID)
}

func TestXParquetTextViewsPreserveSourceAndPeople(t *testing.T) {
	b := query.NewTestDataBuilder(t)
	xSource := b.AddSourceWithType("1001", "x")
	otherSource := b.AddSourceWithType("1001", "discord")
	owner := b.AddParticipant("", "", "Example Owner")
	author := b.AddParticipant("", "", "Example Reply")
	b.AddParticipantIdentifier(owner, "x_user_id", "1001", "@example_owner", true)
	b.AddParticipantIdentifier(author, "x_user_id", "1002", "@example_reply", true)
	b.AddOwnerParticipant(xSource, owner)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for i, sender := range []int64{owner, author} {
		message := b.AddMessage(query.MessageOpt{SourceID: xSource, ConversationID: 100, ConversationType: "channel", ConversationTitle: "X conversation 101", MessageType: "x", SenderID: &sender, OwnerParticipantID: &owner, IsFromMe: sender == owner, SentAt: at.Add(time.Duration(i) * time.Minute)})
		b.AddFrom(message, sender, "")
		b.AddConversationParticipant(100, sender)
	}
	b.AddMessage(query.MessageOpt{SourceID: otherSource, ConversationID: 200, ConversationType: "channel", MessageType: "discord", SentAt: at})
	engine := b.BuildEngine()
	conversations, err := engine.ListConversations(t.Context(), query.TextFilter{SourceID: &xSource})
	require.NoError(t, err)
	require.Len(t, conversations, 1)
	assert.Equal(t, int64(100), conversations[0].ConversationID)
	assert.Equal(t, int64(2), conversations[0].MessageCount)
	sources, err := engine.TextAggregate(t.Context(), query.TextViewSources, query.TextAggregateOptions{})
	require.NoError(t, err)
	assert.Len(t, sources, 2, "X and existing Discord must both remain visible")
	people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{})
	require.NoError(t, err)
	var found bool
	for _, person := range people.Rows {
		if person.ID == author {
			found = true
		}
	}
	assert.True(t, found, "the reply author must remain discoverable through actual Parquet people queries")
}
