package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMergeParticipantsPreservesLoopMuteAndCounterpart(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	absorbed := f.EnsureParticipant("absorbed@example.test", "Absorbed", "example.test")
	survivor := f.EnsureParticipant("survivor@example.test", "Survivor", "example.test")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	messageID := f.CreateMessage("participant-merge-loop")
	open := loopOpen(t, f, messageID, messageID, now)
	open.CounterpartyParticipantID = &absorbed
	applyLoop(t, f, open, now)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)

	absorbedCreatedAt := now.Add(-time.Hour)
	survivorCreatedAt := now
	for _, row := range []struct {
		participantID int64
		note          string
		createdAt     time.Time
	}{
		{participantID: absorbed, note: "absorbed mute", createdAt: absorbedCreatedAt},
		{participantID: survivor, note: "survivor mute", createdAt: survivorCreatedAt},
	} {
		_, err := f.Store.CreateLoopSuppressionContext(t.Context(), store.LoopSuppressionRequest{
			TargetKind: "participant", ParticipantID: &row.participantID, Note: row.note,
		}, row.createdAt)
		requirements.NoError(err)
	}

	requirements.NoError(f.Store.MergeParticipantsContext(t.Context(), absorbed, survivor))
	got, err := f.Store.GetOpenLoopContext(t.Context(), rows[0].ID)
	requirements.NoError(err)
	assertions.Equal("closed", got.Status)
	requirements.NotNil(got.ClosedReason)
	assertions.Equal("muted", *got.ClosedReason)
	requirements.NotNil(got.CounterpartyParticipantID)
	assertions.Equal(survivor, *got.CounterpartyParticipantID)

	suppressions, err := f.Store.LoadLoopSuppressionsContext(t.Context())
	requirements.NoError(err)
	requirements.Len(suppressions, 1)
	requirements.NotNil(suppressions[0].ParticipantID)
	assertions.Equal(survivor, *suppressions[0].ParticipantID)
	assertions.Equal("survivor mute\nabsorbed mute", suppressions[0].Note)
	assertions.True(absorbedCreatedAt.Equal(suppressions[0].CreatedAt))
}

func TestListLiveConversationLoopsResolvesCurrentCounterpartyPerson(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	participantID := f.EnsureParticipant("departed@example.test", "Departed", "example.test")
	messageID := f.CreateMessage("live-counterparty-person")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	open := loopOpen(t, f, messageID, messageID, now)
	open.CounterpartyParticipantID = &participantID
	applyLoop(t, f, open, now)

	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)

	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 1)
	requirements.NotNil(live[0].CounterpartyPersonID)
	assertions.Equal(person.ID, *live[0].CounterpartyPersonID)
}
