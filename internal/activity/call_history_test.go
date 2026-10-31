package activity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestProjectorNativeCallObservation(t *testing.T) {
	for _, mode := range []string{"incremental", "backstop", "exact"} {
		t.Run(mode, func(t *testing.T) {
			st := storetest.New(t).Store
			observation := callhistory.Observation{
				RecordID: "native-call", Version: 1, Provider: "signal", Account: "example",
				NativeCallID: "native-call", Kind: "call", Direction: "incoming",
				Outcome: "answered", OwnerParticipation: "joined", Precision: "instant",
				OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
				Raw:        []byte(`{"status":"Accepted"}`),
			}
			source, err := st.GetOrCreateSource(callhistory.SourceType, observation.SourceIdentifier())
			require.NoError(t, err)
			peer, err := st.EnsureParticipantByPhone("+12025550101", "Example", "signal")
			require.NoError(t, err)
			person, _, err := st.CreatePersonFromParticipant(peer)
			require.NoError(t, err)
			observation.Participants = []callhistory.Participant{{ID: peer, Participation: "joined"}}
			projector, err := NewProjector(st, Options{Timezone: "UTC", BatchSize: 1})
			require.NoError(t, err)
			_, err = projector.RunBackstop(t.Context())
			require.NoError(t, err)
			messageID, err := st.PutCallObservationContext(t.Context(), source.ID, observation)
			require.NoError(t, err)
			switch mode {
			case "incremental":
				_, err = projector.RunOnce(t.Context())
			case "backstop":
				_, err = projector.RunBackstop(t.Context())
			case "exact":
				_, err = projector.ProjectMessages(t.Context(), []int64{messageID})
			}
			require.NoError(t, err)
			var channel, direction string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
				`SELECT channel, direction FROM activity_events WHERE message_id = ?`), messageID).Scan(&channel, &direction))
			assert.Equal(t, string(store.ChannelOther), channel)
			assert.Equal(t, string(store.DirectionObserved), direction)
			var links int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
				`SELECT COUNT(*) FROM activity_event_persons WHERE message_id = ?`), messageID).Scan(&links))
			assert.Zero(t, links)
			state, err := st.ContactStateContext(t.Context(), person.ID, observation.OccurredAt.Add(time.Hour))
			require.NoError(t, err)
			assert.Zero(t, state.InteractionCount)
			assert.Empty(t, state.LastContactRef)
			calls, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{})
			require.NoError(t, err)
			require.Len(t, calls, 1)
			assert.Equal(t, messageID, calls[0].MessageID)
			if mode != "exact" {
				queue, err := st.ListActivityProjectionQueueContext(t.Context(), 10)
				require.NoError(t, err)
				assert.Empty(t, queue)
			}
		})
	}
}
