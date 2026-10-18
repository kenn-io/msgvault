package cmd

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/whoknows"
)

type fixedWhoKnowsRetriever struct {
	messageID int64
}

func (r fixedWhoKnowsRetriever) Search(context.Context, hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error) {
	return []vector.FusedHit{{MessageID: r.messageID, RRFScore: 1, VectorScore: .9}}, hybrid.ResultMeta{}, nil
}

func TestStoreAPIAdapterPassesRosterLimit(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	sender := f.EnsureParticipant("sender@example.net", "Sender", "example.net")
	member := f.EnsureParticipant("member@example.net", "Member", "example.net")
	senderPerson, _, err := f.Store.CreatePersonFromParticipant(sender)
	must.NoError(err)
	memberPerson, _, err := f.Store.CreatePersonFromParticipant(member)
	must.NoError(err)
	conversationID, err := f.Store.EnsureConversationWithType(f.Source.ID, "bounded-room", "group_chat", "Bounded room")
	must.NoError(err)
	for _, participantID := range []int64{sender, member} {
		must.NoError(f.Store.EnsureConversationParticipant(conversationID, participantID, "member"))
	}
	at := time.Date(2026, time.October, 17, 9, 0, 0, 0, time.UTC)
	messageID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        f.Source.ID,
		SourceMessageID: "bounded-room-message",
		MessageType:     "chat",
		SenderID:        sql.NullInt64{Int64: sender, Valid: true},
		SentAt:          sql.NullTime{Time: at, Valid: true},
	})
	must.NoError(err)

	service := whoknows.Service{
		Retriever:  fixedWhoKnowsRetriever{messageID: messageID},
		Data:       &storeAPIAdapter{store: f.Store},
		KPerSignal: 100,
		Now:        func() time.Time { return at },
	}
	params := whoknows.DefaultParams()
	params.MaxRosterEdges = 1
	limited, err := service.Find(t.Context(), "topic", params)
	must.NoError(err)
	check.Equal(1, limited.Excluded.LargeRooms)
	var senderEvidence []whoknows.Evidence
	for _, person := range limited.People {
		check.NotEqual(memberPerson.ID, person.PersonID)
		if person.PersonID == senderPerson.ID {
			senderEvidence = person.Evidence
		}
	}
	must.Len(senderEvidence, 1)
	check.Equal("from", senderEvidence[0].Role)

	params.MaxRosterEdges = 2
	unlimited, err := service.Find(t.Context(), "topic", params)
	must.NoError(err)
	check.Zero(unlimited.Excluded.LargeRooms)
	var memberEvidence []whoknows.Evidence
	for _, person := range unlimited.People {
		if person.PersonID == memberPerson.ID {
			memberEvidence = person.Evidence
		}
	}
	must.Len(memberEvidence, 1)
	check.Equal("conversation_member", memberEvidence[0].Role)
}
