package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
)

func TestCallObservationDoesNotProjectMessageContact(t *testing.T) {
	assertions := assert.New(t)
	personID := int64(7)
	got := store.ClassifyActivityCandidate(store.ActivityCandidate{
		MessageType: callhistory.MessageType, ConversationType: "call_history",
		SourceIsFromMe: true,
		Counterparts:   []store.ActivityCounterpart{{ParticipantID: 1, PersonID: &personID, RecipientType: "call_joined"}},
	}, 10)
	assertions.Empty(got.Persons)
	assertions.Equal(store.DirectionObserved, got.Direction)
	assertions.Equal(store.ChannelOther, got.Channel)
}
