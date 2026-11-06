package activity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestProjectorNativeCallObservation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := storetest.New(t).Store
	observation := callhistory.Observation{
		RecordID: "native-call", Version: 1, Provider: "signal", Account: "example",
		NativeCallID: "native-call", Direction: "incoming",
		Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		Raw:        []byte(`{"status":"Accepted"}`),
	}
	source, err := st.GetOrCreateSource(callhistory.SourceType, observation.SourceIdentifier())
	requirements.NoError(err)
	peer, err := st.EnsureParticipantByPhone("+12025550101", "Example", "signal")
	requirements.NoError(err)
	_, _, err = st.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	observation.Participants = []callhistory.Participant{{ID: peer, Participation: "joined"}}
	projector, err := NewProjector(st, Options{Timezone: "UTC", BatchSize: 1})
	requirements.NoError(err)
	_, err = projector.RunBackstop(t.Context())
	requirements.NoError(err)
	messageID, err := st.PutCallObservationContext(t.Context(), source.ID, observation)
	requirements.NoError(err)
	_, err = projector.RunOnce(t.Context())
	requirements.NoError(err)
	var links int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM activity_event_persons WHERE message_id = ?`), messageID).Scan(&links))
	assertions.Zero(links)
}
