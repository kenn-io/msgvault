package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPeopleRolesForMessages(t *testing.T) {
	must := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	author := f.EnsureParticipant("author@example.net", "Author", "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(author)
	must.NoError(err)
	must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	for _, kind := range []string{"email", "direct_chat", "group_chat", "meeting"} {
		t.Run(kind, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			conv, err := f.Store.EnsureConversationWithType(f.Source.ID, kind, kind, kind)
			must.NoError(err)
			for _, id := range []int64{author, owner} {
				must.NoError(f.Store.EnsureConversationParticipant(conv, id, "member"))
			}
			typ := "email"
			if kind == "meeting" {
				typ = "meeting_transcript"
			}
			id, err := f.Store.UpsertMessage(&store.Message{ConversationID: conv, SourceID: f.Source.ID, SourceMessageID: kind, MessageType: typ, SenderID: sql.NullInt64{Int64: author, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}})
			must.NoError(err)
			if kind == "email" {
				must.NoError(f.Store.ReplaceMessageRecipients(id, "cc", []int64{owner}, []string{"Owner"}))
			}
			got, err := f.Store.PeopleRolesForMessages(t.Context(), []int64{id, id})
			must.NoError(err)
			must.Len(got.Messages, 1)
			msg := got.Messages[id]
			must.NotNil(msg.SentAt)
			check.True(at.Equal(*msg.SentAt))
			must.Len(msg.Edges, 2)
			byRole := map[personscope.Role][]store.RoleEdge{}
			for _, edge := range msg.Edges {
				byRole[edge.Role] = append(byRole[edge.Role], edge)
			}
			must.Len(byRole[personscope.RoleFrom], 1)
			check.Equal(&person.ID, byRole[personscope.RoleFrom][0].PersonID)
			role := personscope.RoleConversationMember
			if kind == "email" {
				role = personscope.RoleCC
			}
			if kind == "direct_chat" {
				role = personscope.RoleTo
			}
			must.Len(byRole[role], 1)
			check.True(byRole[role][0].IsOwner)
			check.Nil(byRole[role][0].PersonID)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE messages SET deleted_at = ? WHERE id = ?"), at, id)
			must.NoError(err)
			gone, err := f.Store.PeopleRolesForMessages(t.Context(), []int64{id})
			must.NoError(err)
			check.Empty(gone.Messages)
		})
	}
	tooManyIDs := make([]int64, 1001)
	for i := range tooManyIDs {
		tooManyIDs[i] = int64(i + 1)
	}
	_, err = f.Store.PeopleRolesForMessages(t.Context(), tooManyIDs)
	must.Error(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.PeopleRolesForMessages(ctx, []int64{1})
	must.ErrorIs(err, context.Canceled)
}

func TestPeopleRolesNativeSenderOwnsFromEdgeBeforeIdentityFallback(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	sender := f.EnsureParticipant("sender@example.net", "Sender", "example.net")
	ownerAlias := f.EnsureParticipant("owner@example.net", "Owner alias", "example.net")
	must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	id, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: "native-multiauthor",
		MessageType:     "email",
		SenderID:        sql.NullInt64{Int64: sender, Valid: true},
		IsFromMe:        true,
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender, ownerAlias}, []string{"Sender", "Owner alias"}))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_recipients SET email_address = CASE participant_id WHEN ? THEN ? ELSE ? END WHERE message_id = ? AND recipient_type = 'from'`), sender, "sender@example.net", "owner@example.net", id)
	must.NoError(err)

	got, err := f.Store.PeopleRolesForMessages(t.Context(), []int64{id})
	must.NoError(err)
	byParticipant := map[int64]store.RoleEdge{}
	for _, edge := range got.Messages[id].Edges {
		byParticipant[edge.ParticipantID] = edge
	}
	must.Contains(byParticipant, sender)
	must.Contains(byParticipant, ownerAlias)
	check.True(byParticipant[sender].IsOwner)
	check.False(byParticipant[ownerAlias].IsOwner)
}

func TestPeopleRolesExplicitRecipientEnvelopePreventsRosterOwnerFallback(t *testing.T) {
	for _, conversationType := range []string{"group_chat", "meeting"} {
		t.Run(conversationType, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			f := storetest.New(t)
			sender := f.EnsureParticipant("sender@example.net", "Sender", "example.net")
			recipient := f.EnsureParticipant("owner@example.net", "Merged contact", "example.net")
			must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
			conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, conversationType, conversationType, conversationType)
			must.NoError(err)
			must.NoError(f.Store.EnsureConversationParticipant(conversation, sender, "member"))
			must.NoError(f.Store.EnsureConversationParticipant(conversation, recipient, "member"))
			messageType := "chat"
			if conversationType == "meeting" {
				messageType = "meeting_transcript"
			}
			id, err := f.Store.UpsertMessage(&store.Message{
				ConversationID:  conversation,
				SourceID:        f.Source.ID,
				SourceMessageID: "explicit-external-recipient",
				MessageType:     messageType,
				SenderID:        sql.NullInt64{Int64: sender, Valid: true},
			})
			must.NoError(err)
			must.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{recipient}, []string{"Merged contact"}))
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE message_recipients SET email_address = ? WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'"), "external@example.net", id, recipient)
			must.NoError(err)

			got, err := f.Store.PeopleRolesForMessages(t.Context(), []int64{id})
			must.NoError(err)
			var recipientEdges []store.RoleEdge
			for _, edge := range got.Messages[id].Edges {
				if edge.ParticipantID == recipient {
					recipientEdges = append(recipientEdges, edge)
				}
			}
			must.Len(recipientEdges, 1)
			check.Equal(personscope.RoleTo, recipientEdges[0].Role)
			check.False(recipientEdges[0].IsOwner)
		})
	}
}

func TestPeopleRolesLargeRoomBound(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	conv, err := f.Store.EnsureConversationWithType(f.Source.ID, "large", "group_chat", "Large")
	must.NoError(err)
	var sender int64
	for i := range store.MaxRosterEdges + 1 {
		id := f.EnsureParticipant(fmt.Sprintf("member%d@example.net", i), "Member", "example.net")
		if i == 0 {
			sender = id
		}
		must.NoError(f.Store.EnsureConversationParticipant(conv, id, "member"))
	}
	id, err := f.Store.UpsertMessage(&store.Message{ConversationID: conv, SourceID: f.Source.ID, SourceMessageID: "large", MessageType: "chat", SenderID: sql.NullInt64{Int64: sender, Valid: true}})
	must.NoError(err)
	got, err := f.Store.PeopleRolesForMessages(t.Context(), []int64{id})
	must.NoError(err)
	check.Equal([]int64{id}, got.LargeRoomMessageIDs)
	must.Len(got.Messages[id].Edges, 1)
	check.Equal(personscope.RoleFrom, got.Messages[id].Edges[0].Role)

	limited, err := f.Store.PeopleRolesForMessagesWithRosterLimit(t.Context(), []int64{id}, 1)
	must.NoError(err)
	check.Equal([]int64{id}, limited.LargeRoomMessageIDs)
	check.Len(limited.Messages[id].Edges, 1)
	_, err = f.Store.PeopleRolesForMessagesWithRosterLimit(t.Context(), []int64{id}, 0)
	must.Error(err)
}

func TestPeopleRolesDateFallbackUsesInternalDateAndNeverArchiveTime(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("dates@example.net", "Dates", "example.net")
	internalDate := time.Date(2025, 4, 3, 2, 1, 0, 0, time.UTC)
	archivedAt := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	withInternalDate, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: "internal-date-only",
		MessageType:     "email",
		SenderID:        sql.NullInt64{Int64: participant, Valid: true},
	})
	must.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE messages SET sent_at=NULL,received_at=NULL,internal_date=?,archived_at=? WHERE id=?"), internalDate, archivedAt, withInternalDate)
	must.NoError(err)
	archiveTimeOnly, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: "archive-time-only",
		MessageType:     "email",
		SenderID:        sql.NullInt64{Int64: participant, Valid: true},
	})
	must.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE messages SET sent_at=NULL,received_at=NULL,internal_date=NULL,archived_at=? WHERE id=?"), archivedAt, archiveTimeOnly)
	must.NoError(err)

	got, err := f.Store.PeopleRolesForMessages(t.Context(), []int64{withInternalDate, archiveTimeOnly})
	must.NoError(err)
	must.NotNil(got.Messages[withInternalDate].SentAt)
	check.True(internalDate.Equal(*got.Messages[withInternalDate].SentAt))
	check.Nil(got.Messages[archiveTimeOnly].SentAt)
}

func TestPeopleRolesOwnerIdentityAndExplicitEdgeBounds(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	id, err := f.Store.UpsertMessage(&store.Message{ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "envelope", MessageType: "email", SenderID: sql.NullInt64{Int64: owner, Valid: true}})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{"Owner"}))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE message_recipients SET email_address = ? WHERE message_id = ?"), "other@example.net", id)
	must.NoError(err)
	got, err := f.Store.PeopleRolesForMessages(t.Context(), []int64{id})
	must.NoError(err)
	must.Len(got.Messages[id].Edges, 1)
	check.False(got.Messages[id].Edges[0].IsOwner, "the envelope is authoritative over a participant alias")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE messages SET source_is_from_me = TRUE WHERE id = ?"), id)
	must.NoError(err)
	got, err = f.Store.PeopleRolesForMessages(t.Context(), []int64{id})
	must.NoError(err)
	check.True(got.Messages[id].Edges[0].IsOwner)
	ids := []int64{}
	names := []string{}
	for i := range 70 {
		ids = append(ids, f.EnsureParticipant(fmt.Sprintf("explicit%d@example.net", i), "Explicit", "example.net"))
		names = append(names, "Explicit")
	}
	must.NoError(f.Store.ReplaceMessageRecipients(id, "to", ids, names))
	got, err = f.Store.PeopleRolesForMessages(t.Context(), []int64{id})
	must.NoError(err)
	check.Len(got.Messages[id].Edges, store.MaxRosterEdges)
	// An identifier-only participant uses the same identity policy as activity.
	var identifierOwner int64
	must.NoError(f.Store.DB().QueryRowContext(t.Context(), "INSERT INTO participants(display_name) VALUES ('Identifier Owner') RETURNING id").Scan(&identifierOwner))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("INSERT INTO participant_identifiers(participant_id,identifier_type,identifier_value) VALUES (?, 'email', ?)"), identifierOwner, "OWNER@example.net")
	must.NoError(err)
	id2, err := f.Store.UpsertMessage(&store.Message{ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "identifier", MessageType: "email", SenderID: sql.NullInt64{Int64: identifierOwner, Valid: true}})
	must.NoError(err)
	got, err = f.Store.PeopleRolesForMessages(t.Context(), []int64{id2})
	must.NoError(err)
	must.Len(got.Messages[id2].Edges, 1)
	check.True(got.Messages[id2].Edges[0].IsOwner)
}

func TestPeopleRolesMergedEnvelopeAliasesPreserveOwner(t *testing.T) {
	must := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("z@example.net", "Merged Contact", "example.net")
	must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	id, err := f.Store.UpsertMessage(&store.Message{ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "aliases", MessageType: "email"})
	must.NoError(err)
	for _, address := range []string{"owner@example.net", "z@example.net"} {
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES (?,?, 'to',?)"), id, participant, address)
		must.NoError(err)
	}
	got, err := f.Store.PeopleRolesForMessages(t.Context(), []int64{id})
	must.NoError(err)
	must.Len(got.Messages[id].Edges, 1)
	assert.True(t, got.Messages[id].Edges[0].IsOwner)
}
