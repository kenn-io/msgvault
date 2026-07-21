package query

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelationshipScore(t *testing.T) {
	tests := []struct {
		name    string
		signals RelationshipSignals
		want    float64
	}{
		{
			name:    "zero signals score zero",
			signals: RelationshipSignals{},
			want:    0,
		},
		{
			name:    "one decayed sent unit scores the sent weight",
			signals: RelationshipSignals{SentToThem: 1, Modalities: 1},
			want:    2.0,
		},
		{
			name:    "one decayed meeting unit scores the meeting weight",
			signals: RelationshipSignals{MeetingsTogether: 1, Modalities: 1},
			want:    3.0,
		},
		{
			name:    "one decayed received unit scores the received weight",
			signals: RelationshipSignals{ReceivedFromThem: 1, Modalities: 1},
			want:    1.0,
		},
		{
			name:    "combined signals sum the weighted contributions",
			signals: RelationshipSignals{SentToThem: 1, MeetingsTogether: 1, ReceivedFromThem: 1, Modalities: 1},
			want:    6.0,
		},
		{
			name:    "three modalities apply a 1.5x breadth boost",
			signals: RelationshipSignals{SentToThem: 1, Modalities: 3},
			want:    3.0,
		},
		{
			name:    "two modalities apply a 1.25x breadth boost",
			signals: RelationshipSignals{SentToThem: 1, Modalities: 2},
			want:    2.5,
		},
		{
			name:    "modalities of zero behave like one (no boost, no penalty)",
			signals: RelationshipSignals{SentToThem: 1, Modalities: 0},
			want:    2.0,
		},
		{
			name: "counts and timestamps do not affect score",
			signals: RelationshipSignals{
				SentToThem: 1, Modalities: 1,
				SentCount: 1000, MeetingCount: 1000,
				LastInteractionAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			want: 2.0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, RelationshipScore(tc.signals), 1e-9)
		})
	}
}

// TestRelationshipsReceivedCreditsOnlyAuthors pins the credit-assignment
// semantics for received_from_them: an incoming message credits only its
// AUTHOR (the 'from' participant), never co-recipients. A mailing-list
// address that merely appears in To on 100 incoming messages authored by
// third parties must accumulate zero received credit; after one
// owner-authored reply it passes the reciprocity gate, but its score is
// sent-only — not inflated by subscription volume.
func TestRelationshipsReceivedCreditsOnlyAuthors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)

	listID := b.AddParticipant("dev@list.example", "list.example", "Dev List")
	authorIDs := make([]int64, 5)
	for i := range authorIDs {
		authorIDs[i] = b.AddParticipant(
			"author"+string(rune('a'+i))+"@example.com", "example.com", "Author")
	}

	// All timestamps are "now" so per-entry decay is ~1.0 (the session's
	// date-diff for the same instant may be off by a day, so decayed sums
	// are asserted with tolerance, and zero sums exactly).
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)

	// 100 incoming list messages: authored by rotating third parties, with
	// the list address and the owner both in To.
	for i := range 100 {
		msgID := b.AddMessage(MessageOpt{SourceID: srcID, SentAt: now})
		b.AddFrom(msgID, authorIDs[i%len(authorIDs)], "Author")
		b.AddTo(msgID, listID, "Dev List")
		b.AddTo(msgID, ownerID, "Owner")
	}

	// One owner-authored reply addressed to the list.
	replyID := b.AddMessage(MessageOpt{SourceID: srcID, IsFromMe: true, SentAt: now})
	b.AddFrom(replyID, ownerID, "Owner")
	b.AddTo(replyID, listID, "Dev List")

	engine := b.BuildEngine()
	result, err := engine.Relationships(context.Background(), RelationshipsRequest{Now: now, Limit: 10})
	require.NoError(err)
	require.Len(result.Rows, 1, "only the list passes the gate; the authors have no sent/meeting signal")

	row := result.Rows[0]
	assert.Equal(listID, row.CanonicalID)
	assert.InDelta(0.0, row.Signals.ReceivedFromThem, 1e-9,
		"co-recipient volume must earn zero received credit")
	assert.Equal(int64(1), row.Signals.SentCount)
	assert.InDelta(1.0, row.Signals.SentToThem, 0.1)
	assert.InEpsilon(relationshipWeightSent*row.Signals.SentToThem, row.Score, 1e-9,
		"score must be sent-only, not inflated by 100 co-recipient messages")
}

// TestRelationshipsReceivedCountsAuthoredMessages verifies the positive side
// of author-only credit: a person who authored 20 messages received by the
// owner earns received_from_them = 20.
func TestRelationshipsReceivedCountsAuthoredMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)

	authorID := b.AddParticipant("carol@example.com", "example.com", "Carol")

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	for range 20 {
		msgID := b.AddMessage(MessageOpt{SourceID: srcID, SentAt: now})
		b.AddFrom(msgID, authorID, "Carol")
		b.AddTo(msgID, ownerID, "Owner")
	}

	engine := b.BuildEngine()
	result, err := engine.Relationships(context.Background(),
		RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err)
	require.Len(result.Rows, 1)

	row := result.Rows[0]
	assert.Equal(authorID, row.CanonicalID)
	assert.InDelta(20.0, row.Signals.ReceivedFromThem, 0.5,
		"every authored message earns one decayed unit of received credit")
	assert.Equal(int64(0), row.Signals.SentCount)
}

// TestRelationshipsChatReceivedCreditUnchanged pins that chat counting is
// untouched by author-only email credit: a grouped chat conversation still
// credits its non-owner members (including silent ones listed only via
// conversation_participants) when the latest message is incoming.
func TestRelationshipsChatReceivedCreditUnchanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b := NewTestDataBuilder(t)
	srcID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	b.AddOwnerParticipant(srcID, ownerID)

	speakerID := b.AddParticipant("dave@chat.example", "chat.example", "Dave")
	silentID := b.AddParticipant("erin@chat.example", "chat.example", "Erin")

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	chatID := b.AddMessage(MessageOpt{SourceID: srcID, MessageType: "imessage", SentAt: now})
	b.AddFrom(chatID, speakerID, "Dave")
	b.AddTo(chatID, ownerID, "Owner")

	var convID int64
	for _, m := range b.messages {
		if m.ID == chatID {
			convID = m.ConversationID
		}
	}
	require.NotZero(convID)
	b.AddConversationParticipant(convID, silentID)

	engine := b.BuildEngine()
	result, err := engine.Relationships(context.Background(),
		RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err)
	require.Len(result.Rows, 2)

	byCanonicalID := make(map[int64]RelationshipRow, len(result.Rows))
	for _, row := range result.Rows {
		byCanonicalID[row.CanonicalID] = row
	}
	require.Contains(byCanonicalID, speakerID)
	require.Contains(byCanonicalID, silentID)
	assert.InDelta(1.0, byCanonicalID[speakerID].Signals.ReceivedFromThem, 0.1)
	assert.InDelta(1.0, byCanonicalID[silentID].Signals.ReceivedFromThem, 0.1,
		"chat conversations credit members without an author requirement")
}
