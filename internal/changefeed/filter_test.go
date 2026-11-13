package changefeed

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveFilterTracksNewPersonLinks(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	first, err := st.EnsureParticipant("first@example.com", "First", "example.com")
	require.NoError(err)
	second, err := st.EnsureParticipant("second@example.com", "Second", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(err)
	filter, err := ResolveFilter(t.Context(), st, Filter{PersonID: person.ID})
	require.NoError(err)
	assert.True(filter.Matches(store.ChangedMessage{SenderID: first}))
	assert.False(filter.Matches(store.ChangedMessage{SenderID: second}))
	_, err = st.LinkParticipants(first, second)
	require.NoError(err)
	filter, err = ResolveFilter(t.Context(), st, Filter{PersonID: person.ID})
	require.NoError(err)
	assert.True(filter.Matches(store.ChangedMessage{SenderID: second}))
	_, err = ResolveFilter(t.Context(), st, Filter{PersonID: 99999999})
	require.ErrorIs(err, store.ErrPersonNotFound)
}
func TestChangesFilterMatchesAllDimensions(t *testing.T) {
	f := Filter{PersonID: 4, Participants: []int64{10, 11}, SourceIDs: []int64{1, 2}, ConversationIDs: []int64{8}, IncomingOnly: true}
	for _, tc := range []struct {
		name string
		row  store.ChangedMessage
		want bool
	}{
		{"first identity", store.ChangedMessage{SenderID: 10, SourceID: 1, ConversationID: 8}, true},
		{"second identity", store.ChangedMessage{SenderID: 11, SourceID: 2, ConversationID: 8}, true},
		{"other sender", store.ChangedMessage{SenderID: 12, SourceID: 1, ConversationID: 8}, false},
		{"other source", store.ChangedMessage{SenderID: 10, SourceID: 3, ConversationID: 8}, false},
		{"other conversation", store.ChangedMessage{SenderID: 10, SourceID: 1, ConversationID: 9}, false},
		{"outgoing", store.ChangedMessage{SenderID: 10, SourceID: 1, ConversationID: 8, IsFromMe: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) { Assert.Equal(t, tc.want, f.Matches(tc.row)) })
	}
}

func TestChangesScanPages(t *testing.T) {
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	bound := at.Add(time.Hour)
	rows := make([]store.ChangedMessage, 2001)
	for i := range rows {
		rows[i] = store.ChangedMessage{ID: int64(i + 1), SourceID: 1, ContentChangedAt: at}
	}
	from := store.ChangedMessagesFrom(at)
	after := store.ChangedMessagesAfter(at, 0)
	for _, tc := range []struct {
		name         string
		page         store.ChangedMessagePage
		since        store.ChangedMessagesCursor
		source       int64
		limit, count int
		next         store.ChangedMessagesCursor
		more         bool
	}{
		{"limit", store.ChangedMessagePage{Messages: rows, ServerTime: bound.Add(time.Second), CompleteThrough: bound}, from, 1, 1, 1, store.ChangedMessagesAfter(at, 1), true},
		{"sentinel", store.ChangedMessagePage{Messages: rows, ServerTime: bound.Add(time.Second), CompleteThrough: bound}, from, 2, 100, 0, store.ChangedMessagesAfter(at, 2000), true},
		{"exhaustion", store.ChangedMessagePage{Messages: rows[:2000], ServerTime: bound.Add(time.Second), CompleteThrough: bound}, from, 2, 100, 0, store.ChangedMessagesFrom(bound), false},
		{"no bound", store.ChangedMessagePage{ServerTime: bound}, after, 1, 1, 0, after, false},
		{"earlier bound", store.ChangedMessagePage{ServerTime: bound, CompleteThrough: at.Add(-time.Second)}, after, 1, 1, 0, after, false},
		{"equal bound", store.ChangedMessagePage{ServerTime: bound, CompleteThrough: at}, after, 1, 1, 0, after, false},
		{"above clock", store.ChangedMessagePage{ServerTime: at.Add(-time.Second), CompleteThrough: at.Add(-2 * time.Second)}, after, 1, 1, 0, store.ChangedMessagesFrom(at.Add(-2 * time.Second)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, next, more := Scan(tc.page, tc.since, Filter{SourceIDs: []int64{tc.source}}, tc.limit)
			Assert.Len(t, got, tc.count)
			Assert.Equal(t, tc.next, next)
			Assert.Equal(t, tc.more, more)
		})
	}
}

func TestGrantDigestUsesStableReferences(t *testing.T) {
	assert := Assert.New(t)
	original := Filter{GrantRefs: []GrantRef{{Type: "gmail", Identifier: "a@example.com"}, {Type: "gmail", Identifier: "b@example.com"}}}
	same := Filter{GrantRefs: []GrantRef{{Type: "gmail", Identifier: "a@example.com"}, {Type: "gmail", Identifier: "b@example.com"}}}
	assert.NotEmpty(original.Digest())
	assert.Equal(original.Digest(), same.Digest())
	assert.NotEqual(original.Digest(), (Filter{GrantRefs: same.GrantRefs[:1]}).Digest())
	assert.NotEqual(original.Digest(), (Filter{}).Digest())
	empty := Filter{GrantRefs: []GrantRef{}}
	assert.NotEmpty(empty.Digest())
	assert.NotEqual(empty.Digest(), (Filter{}).Digest())
	assert.NotEqual(empty.Digest(), original.Digest())
}
