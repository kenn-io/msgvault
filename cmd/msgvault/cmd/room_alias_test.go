package cmd

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestRoomAliasDetailUsesLiveMembershipBeforeCacheRebuild(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newRelationshipIndexHTTPFixture(t)
	room, err := f.store.EnsureParticipant("room@resource.calendar.google.com", "Room", "resource.calendar.google.com")
	requirements.NoError(err)
	message := insertRelationshipIndexMessage(t, f.store, store.Message{
		SourceID: f.sourceID, ConversationID: f.conversationID, SourceMessageID: "room-message", MessageType: "email",
		SentAt: sql.NullTime{Time: f.lastAt.Add(time.Hour), Valid: true},
	})
	requirements.NoError(f.store.ReplaceMessageRecipients(message, "to", []int64{room}, []string{"Room"}))
	_, err = buildCache(f.dbPath, f.analyticsDir, true)
	requirements.NoError(err)
	engine, err := query.NewDuckDBEngine(f.analyticsDir, "", nil)
	requirements.NoError(err)
	t.Cleanup(func() {
		if engine != nil {
			requirements.NoError(engine.Close())
		}
	})
	_, err = f.store.LinkParticipants(room, f.personID)
	requirements.NoError(err)
	members, err := f.store.ClusterMembers(room)
	requirements.NoError(err)
	detail, err := engine.GetPerson(t.Context(), room, query.Context{}, members)
	requirements.NoError(err)
	requirements.NotNil(detail)
	assertions.Equal(room, detail.ID)
	assertions.Equal(int64(3), detail.ActivityCount)
	summary, err := engine.GetPersonSummary(t.Context(), room, query.ExploreRequest{Context: query.Context{SourceIDs: []int64{f.sourceID}}}, members)
	requirements.NoError(err)
	requirements.Len(summary.Rows, 1)
	assertions.Equal(int64(3), summary.Rows[0].ActivityCount)

	// Retain the linked cache while the live store removes the link.
	requirements.NoError(engine.Close())
	engine = nil
	_, err = buildCache(f.dbPath, f.analyticsDir, true)
	requirements.NoError(err)
	engine, err = query.NewDuckDBEngine(f.analyticsDir, "", nil)
	requirements.NoError(err)
	_, err = f.store.UnlinkParticipants(room, f.personID)
	requirements.NoError(err)
	members, err = f.store.ClusterMembers(room)
	requirements.NoError(err)
	detail, err = engine.GetPerson(t.Context(), room, query.Context{}, members)
	requirements.NoError(err)
	assertions.Nil(detail)
	summary, err = engine.GetPersonSummary(t.Context(), room, query.ExploreRequest{Context: query.Context{SourceIDs: []int64{f.sourceID}}}, members)
	requirements.NoError(err)
	assertions.Empty(summary.Rows)
}
