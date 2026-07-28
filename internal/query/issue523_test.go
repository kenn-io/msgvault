package query

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRelationshipsDirectJoinHandlesDuplicateEmailAndMixedSources is a
// regression test for issue #523: the rewritten relationships query (which
// joins logical_entries directly to message_recipients, messages.sender_id,
// and conversation_participants instead of unnesting pre-built participant
// lists) must produce identical results to the old fan-out approach for
// archives with duplicate email and mixed-source conversations.
//
// The fixture exercises:
//   - Duplicate email: multiple messages with the same recipients
//   - Mixed sources: email + iMessage chat
//   - Chat conversations with conversation participants who never sent a
//     message (silent members)
//   - Clustered identities (linked aliases)
//   - Owner exclusion
//   - Meetings with and without the owner
func TestRelationshipsDirectJoinHandlesDuplicateEmailAndMixedSources(t *testing.T) {
	b := NewTestDataBuilder(t)
	emailSrc := b.AddSource("owner@example.com")
	chatSrc := b.AddSourceWithType("+15550001111", "imessage")

	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(emailSrc, ownerID)

	// Alice: clustered with a second identity, receives email and chat.
	aliceID := b.AddParticipant("alice@example.com", "example.com", "Alice")
	aliceAliasID := b.AddParticipant("alice@other.example", "other.example", "Alice Other")
	b.LinkCluster(aliceID, aliceAliasID)

	// Bob: unlinked, receives email only.
	bobID := b.AddParticipant("bob@example.com", "example.com", "Bob")

	// Carol: chat-only, clustered with a silent alias.
	carolID := b.AddParticipant("carol@chat.example", "chat.example", "Carol")
	carolAliasID := b.AddParticipant("carol@other.chat", "other.chat", "")
	b.LinkCluster(carolID, carolAliasID)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// --- Duplicate email: 5 messages from owner to Alice+Bob ---
	for i := range 5 {
		msgID := b.AddMessage(MessageOpt{
			SourceID: emailSrc, IsFromMe: true,
			SentAt: now.AddDate(0, 0, -(5 - i)),
		})
		b.AddFrom(msgID, ownerID, "Owner")
		b.AddTo(msgID, aliceID, "Alice")
		b.AddTo(msgID, bobID, "Bob")
	}

	// --- Chat conversation: Carol (alias) sends to owner, with silent member ---
	chatID := b.AddMessage(MessageOpt{
		SourceID: chatSrc, MessageType: "imessage",
		SenderID: &carolAliasID, SentAt: now.AddDate(0, 0, -1),
	})
	b.AddFrom(chatID, carolAliasID, "Carol Alias")
	b.AddTo(chatID, ownerID, "Owner")
	var chatConvID int64
	for _, m := range b.messages {
		if m.ID == chatID {
			chatConvID = m.ConversationID
		}
	}
	require.NotZero(t, chatConvID)
	b.AddConversationParticipant(chatConvID, carolID)
	b.AddConversationParticipant(chatConvID, carolAliasID)
	b.AddConversationParticipant(chatConvID, ownerID)

	// --- Group chat: owner sends to Alice + Carol ---
	groupID := b.AddMessage(MessageOpt{
		SourceID: chatSrc, MessageType: "imessage",
		SenderID: &ownerID, IsFromMe: true,
		SentAt: now.AddDate(0, 0, -2),
	})
	b.AddFrom(groupID, ownerID, "Owner")
	b.AddTo(groupID, aliceID, "Alice")
	b.AddTo(groupID, carolID, "Carol")
	var groupConvID int64
	for _, m := range b.messages {
		if m.ID == groupID {
			groupConvID = m.ConversationID
		}
	}
	require.NotZero(t, groupConvID)
	b.AddConversationParticipant(groupConvID, aliceID)
	b.AddConversationParticipant(groupConvID, carolID)
	b.AddConversationParticipant(groupConvID, ownerID)

	// --- Meeting with owner + Alice ---
	meetingID := b.AddMessage(MessageOpt{
		SourceID: emailSrc, MessageType: "calendar_event",
		SentAt: now.AddDate(0, 0, -3),
	})
	b.AddFrom(meetingID, ownerID, "Owner")
	b.AddTo(meetingID, aliceID, "Alice")

	engine := b.BuildEngine()
	ctx := context.Background()

	t.Run("relationships ranks all counterparts correctly", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)

		result, err := engine.Relationships(ctx, RelationshipsRequest{
			Now: now, Limit: 100, ShowAll: true,
		})
		requirements.NoError(err)

		byCanonicalID := make(map[int64]RelationshipRow, len(result.Rows))
		for _, row := range result.Rows {
			byCanonicalID[row.CanonicalID] = row
		}

		// Alice should be present with clustered identity.
		requirements.Contains(byCanonicalID, aliceID)
		alice := byCanonicalID[aliceID]
		assertions.Equal([]int64{aliceID, aliceAliasID}, alice.MemberIDs)
		// 5 emails + 1 group chat from owner = 6 sent.
		assertions.Equal(int64(6), alice.Signals.SentCount, "owner sent 5 emails + 1 group chat to Alice")
		// Alice never sent to the owner.
		assertions.Zero(alice.Signals.ReceivedFromThem, "Alice never sent to owner")
		assertions.Equal(int64(1), alice.Signals.MeetingCount, "one meeting with owner")
		assertions.Equal(3, alice.Signals.Modalities, "email + meeting + chat")

		// Bob should be present with only email activity.
		requirements.Contains(byCanonicalID, bobID)
		bob := byCanonicalID[bobID]
		assertions.Equal(int64(5), bob.Signals.SentCount, "owner sent 5 emails to Bob")
		assertions.Zero(bob.Signals.ReceivedFromThem, "Bob never sent to owner")
		assertions.Equal(1, bob.Signals.Modalities, "email only")

		// Carol should be present with chat activity (including silent alias).
		requirements.Contains(byCanonicalID, carolID)
		carol := byCanonicalID[carolID]
		assertions.Equal([]int64{carolID, carolAliasID}, carol.MemberIDs)
		// Carol sent 1 direct chat to the owner.
		assertions.Positive(carol.Signals.ReceivedFromThem, "Carol receives chat credit")
		// Owner sent 1 group chat to Carol.
		assertions.Equal(int64(1), carol.Signals.SentCount, "owner sent 1 group chat to Carol")
		assertions.Equal(1, carol.Signals.Modalities, "chat only")

		// Owner should NOT be in the results.
		assertions.NotContains(byCanonicalID, ownerID, "owner is never ranked")
	})

	t.Run("people listing aggregates all participants correctly", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)

		result, err := engine.SearchPeople(ctx, PersonSearchRequest{
			Page: PageSpec{Limit: 100},
		})
		requirements.NoError(err)

		byID := make(map[int64]PersonSummary, len(result.Rows))
		for _, row := range result.Rows {
			byID[row.ID] = row
		}

		// Alice (canonical) should aggregate activity across both identities.
		// 5 emails + 1 group chat + 1 meeting = 7 (Alice is NOT in the direct chat).
		requirements.Contains(byID, aliceID)
		alice := byID[aliceID]
		assertions.Equal(int64(7), alice.ActivityCount, "5 emails + 1 group chat + 1 meeting")
		assertions.Equal("Alice", alice.DisplayLabel, "best name across cluster")

		// Carol (canonical) should aggregate chat activity.
		// 1 direct chat + 1 group chat = 2.
		requirements.Contains(byID, carolID)
		carol := byID[carolID]
		assertions.Equal(int64(2), carol.ActivityCount, "1 direct chat + 1 group chat")

		// Bob should have only email activity.
		requirements.Contains(byID, bobID)
		bob := byID[bobID]
		assertions.Equal(int64(5), bob.ActivityCount, "5 emails")
	})

	t.Run("participant grouping counts all participants", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)

		result, err := engine.ExploreGroups(ctx, ExploreGroupRequest{
			Dimension: "participant",
			Page:      PageSpec{Limit: 100},
		})
		requirements.NoError(err)

		byKey := make(map[string]int64, len(result.Rows))
		for _, row := range result.Rows {
			byKey[row.Key] = row.Count
		}

		// Alice (canonical ID = aliceID) should have 7 entries.
		assertions.Equal(int64(7), byKey[strconv.FormatInt(aliceID, 10)], "Alice canonical ID has 7 entries")

		// Carol (canonical ID = carolID) should have 2 entries.
		assertions.Equal(int64(2), byKey[strconv.FormatInt(carolID, 10)], "Carol canonical ID has 2 entries")

		// Bob should have 5 entries.
		assertions.Equal(int64(5), byKey[strconv.FormatInt(bobID, 10)], "Bob has 5 entries")
	})
}

// TestRelationshipsDirectJoinOwnerAbsentMeetingExcluded verifies that the
// rewritten with_owner computation (using EXISTS subqueries on raw tables
// instead of list_has_any on participant_ids) correctly excludes meetings
// the owner did not attend and includes meetings the owner did attend.
func TestRelationshipsDirectJoinOwnerAbsentMeetingExcluded(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)

	attendeeID := b.AddParticipant("attendee@example.com", "example.com", "Attendee")
	absentID := b.AddParticipant("absent@example.com", "example.com", "Absent")

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)

	// Meeting WITH owner: owner + attendee
	attendedMeeting := b.AddMessage(MessageOpt{
		SourceID: srcID, MessageType: "calendar_event",
		SentAt: now.AddDate(0, 0, -1),
	})
	b.AddFrom(attendedMeeting, ownerID, "Owner")
	b.AddTo(attendedMeeting, attendeeID, "Attendee")

	// Meeting WITHOUT owner: absent + attendee (no owner)
	unattendedMeeting := b.AddMessage(MessageOpt{
		SourceID: srcID, MessageType: "calendar_event",
		SentAt: now.AddDate(0, 0, -2),
	})
	b.AddFrom(unattendedMeeting, absentID, "Absent")
	b.AddTo(unattendedMeeting, attendeeID, "Attendee")

	engine := b.BuildEngine()
	result, err := engine.Relationships(context.Background(),
		RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	requirements.NoError(err)

	byCanonicalID := make(map[int64]RelationshipRow, len(result.Rows))
	for _, row := range result.Rows {
		byCanonicalID[row.CanonicalID] = row
	}

	// Attendee should have 1 meeting (the attended one), not 2.
	requirements.Contains(byCanonicalID, attendeeID)
	assertions.Equal(int64(1), byCanonicalID[attendeeID].Signals.MeetingCount,
		"only the owner-attended meeting counts")

	// Absent should NOT appear (their only entry is a meeting without the owner).
	assertions.NotContains(byCanonicalID, absentID,
		"a meeting without the owner contributes no signal for any attendee")
}
