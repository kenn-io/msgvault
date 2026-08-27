package query

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func BenchmarkRelationshipCalendarWarm(b *testing.B) {
	fixture := NewTestDataBuilder(b)
	sourceID := fixture.AddSourceWithType("owner@example.test", "imessage")
	ownerID := fixture.AddParticipant("owner@example.test", "example.test", "Owner")
	personID := fixture.AddParticipant("person@example.test", "example.test", "Person")
	fixture.AddOwnerParticipant(sourceID, ownerID)
	for index := range 365 * 4 {
		isFromMe := index%2 == 0
		messageID := fixture.AddMessage(MessageOpt{
			SourceID: sourceID, MessageType: "imessage", ConversationType: "direct_chat",
			IsFromMe: isFromMe,
			SentAt:   time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(index) * 6 * time.Hour),
		})
		if isFromMe {
			fixture.AddFrom(messageID, ownerID, "Owner")
			fixture.AddTo(messageID, personID, "Person")
		} else {
			fixture.AddFrom(messageID, personID, "Person")
			fixture.AddTo(messageID, ownerID, "Owner")
		}
	}
	engine := fixture.BuildEngine()
	request := RelationshipCalendarRequest{
		CanonicalID: personID, Year: 2025, Timezone: "America/New_York",
	}
	response, err := engine.RelationshipCalendar(b.Context(), request)
	require.NoError(b, err)
	require.Len(b, response.Days, 365)

	b.ResetTimer()
	for range b.N {
		_, err := engine.RelationshipCalendar(b.Context(), request)
		require.NoError(b, err)
	}
}
