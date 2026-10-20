package api

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personcard"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The scheduler interface is external to the card read. Only its account
// status methods participate in this regression; archive reads use a real store.
type cardSchedulingScheduler struct {
	SyncScheduler

	scheduled bool
}

func (s *cardSchedulingScheduler) IsScheduled(string) bool { return s.scheduled }
func (s *cardSchedulingScheduler) Status() []AccountStatus { return nil }

func TestPersonCardSourceScheduling(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("scheduled@example.test", "Example Person", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participant)
	requirements.NoError(err)
	source, err := st.GetOrCreateSource("gmail", "scheduled@example.test")
	requirements.NoError(err)
	_, err = st.RecordContactObservationContext(t.Context(), participant, store.ParticipantContactObservationInput{
		SourceID: &source.ID, AddressKind: store.ContactAddressEmail, OriginalValue: "scheduled@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	requirements.NoError(err)
	scheduler := &cardSchedulingScheduler{}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{}, Store: st, Scheduler: scheduler,
		Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	for _, tc := range []struct {
		scheduled bool
		want      string
	}{{false, "manual"}, {true, "scheduled"}} {
		t.Run(tc.want, func(t *testing.T) {
			scheduler.scheduled = tc.scheduled
			in := personcard.Inputs{CollectedAt: time.Now().UTC()}
			srv.personCardSources(t.Context(), person.ID, &in)
			card := personcard.Assemble(in, personcard.Options{})
			require.Len(t, card.Freshness.Sources, 1)
			assert.Equal(t, tc.want, card.Freshness.Sources[0].Scheduling)
		})
	}
}
