package query

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExploreProviderMailboxState(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("owner@example.test", "gmail")
	id := b.AddMessage(MessageOpt{SourceID: source, Subject: "Waiting", SentAt: time.Now()})
	unknown := b.AddMessage(MessageOpt{SourceID: source, Subject: "Unknown", SentAt: time.Now().Add(-time.Hour)})
	alice := b.AddParticipant("alice@example.test", "example.test", "Alice Fixture")
	bob := b.AddParticipant("bob@example.test", "example.test", "Bob Fixture")
	b.AddFrom(id, alice, "Alice Fixture")
	b.AddFrom(unknown, bob, "Bob Fixture")
	b.mailboxStates = []string{fmt.Sprintf("(%d::BIGINT,'inbox','unread')", id)}
	engine := b.BuildEngine()
	for _, search := range []SearchSpec{{}, {Mode: SearchFullText, Query: "Waiting", CandidateMessageIDs: []int64{id}, LexicalIndexRevision: "fixture"}} {
		page, err := engine.Explore(t.Context(), ExploreRequest{Context: Context{InboxStates: []string{"inbox"}, ReadStates: []string{"unread"}}, Search: search})
		requirements.NoError(err)
		requirements.Len(page.Rows, 1)
		requirements.NotNil(page.Rows[0].AnchorMessageID)
		assertions.Equal(id, *page.Rows[0].AnchorMessageID)
		assertions.Equal("inbox", page.Rows[0].InboxState)
		assertions.Equal("unread", page.Rows[0].ReadState)
		assertions.Equal("owner@example.test", page.Rows[0].SourceIdentifier)
	}
	page, err := engine.Explore(t.Context(), ExploreRequest{Context: Context{InboxStates: []string{"unknown"}}})
	requirements.NoError(err)
	requirements.Len(page.Rows, 1)
	requirements.NotNil(page.Rows[0].AnchorMessageID)
	assertions.Equal(unknown, *page.Rows[0].AnchorMessageID)
	assertions.Equal("unknown", page.Rows[0].ReadState)
	groups, err := engine.ExploreGroups(t.Context(), ExploreGroupRequest{Explore: ExploreRequest{Context: Context{InboxStates: []string{"inbox"}, ReadStates: []string{"unread"}}}, Dimension: "participant"})
	requirements.NoError(err)
	requirements.Len(groups.Rows, 1)
	assertions.Equal(strconv.FormatInt(alice, 10), groups.Rows[0].Key)

	people, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Explore: ExploreRequest{Context: Context{InboxStates: []string{"inbox"}, ReadStates: []string{"unread"}}}})
	requirements.NoError(err)
	requirements.Len(people.Rows, 1)
	assertions.Equal(alice, people.Rows[0].ID)
}
