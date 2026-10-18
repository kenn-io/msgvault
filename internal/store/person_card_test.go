package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// storetest uses disposable SQLite stores, or disposable PostgreSQL databases
// when MSGVAULT_TEST_DB is configured. Both backends exercise the same reads.
func TestPersonCardResolveEmail(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := storetest.New(t).Store
	ctx := t.Context()
	email := "shared@example.test"
	var want []int64
	for index := range 8 {
		person := cardPerson(t, st, fmt.Sprintf("person-%d@example.test", index))
		want = append(want, person.ID)
		_, err := st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "Shared@Example.Test",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		requirements.NoError(err)
		// Duplicate contact values on one profile cannot create ambiguity.
		_, err = st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: email,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		requirements.NoError(err)
	}
	ids, err := st.ResolvePersonByEmailContext(ctx, "  SHARED@EXAMPLE.TEST  ")
	requirements.NoError(err)
	assertions.Equal(want[:6], ids, "five candidates plus an ambiguity sentinel")

	unique, err := st.ResolvePersonByEmailContext(ctx, "person-0@example.test")
	requirements.NoError(err)
	assertions.Equal(want[:1], unique)
	for _, email := range []string{"missing@example.test", "", " \t"} {
		ids, err := st.ResolvePersonByEmailContext(ctx, email)
		requirements.NoError(err)
		assertions.Empty(ids)
	}
}

func TestPersonCardResolveEmailUsesCurrentDurableContactPoints(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := storetest.New(t).Store
	ctx := t.Context()
	person := cardPerson(t, st, "current@example.test")
	point, err := st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "old@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	requirements.NoError(st.SupersedePersonContactPointContext(ctx, person.ID, point.Envelope.ID, nil))
	_, err = st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressUsername, OriginalValue: "username@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	participant, err := st.EnsureParticipantByIdentifier("email", "observed@example.test", "Observed Example")
	requirements.NoError(err)
	_, err = st.RecordContactObservationContext(ctx, participant, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "observed@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	requirements.NoError(err)
	for _, email := range []string{"old@example.test", "username@example.test", "observed@example.test"} {
		ids, err := st.ResolvePersonByEmailContext(ctx, email)
		requirements.NoError(err)
		assertions.Empty(ids, email)
	}
	// A value closed without superseding is historical too.
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE person_contact_points SET active_until = ? WHERE person_id = ?`),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), person.ID)
	requirements.NoError(err)
	ids, err := st.ResolvePersonByEmailContext(ctx, "current@example.test")
	requirements.NoError(err)
	assertions.Empty(ids)
}

func TestPersonCardSourcesAggregateCurrentBindings(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := storetest.New(t).Store
	ctx := t.Context()
	person := cardPerson(t, st, "person@example.test")
	other := cardPerson(t, st, "other@example.test")
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	var sourceIDs []int64
	for index := range 12 {
		source, err := st.GetOrCreateSource("gmail", fmt.Sprintf("source-%02d@example.test", index))
		requirements.NoError(err)
		sourceIDs = append(sourceIDs, source.ID)
		cardObservation(t, st, person.ParticipantIDs[0], source.ID, at.Add(-time.Duration(index)*time.Hour))
	}
	// A second bound participant contributes to the same source, once.
	participant, err := st.EnsureParticipantByIdentifier("email", "alias@example.test", "Alias Example")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_participants (person_id, participant_id) VALUES (?, ?)`), person.ID, participant)
	requirements.NoError(err)
	cardObservation(t, st, participant, sourceIDs[1], at)
	// Source 0 and 1 now tie; source ID resolves the tie deterministically.
	cardObservation(t, st, other.ParticipantIDs[0], sourceIDs[11], at.Add(time.Hour))
	items, err := st.ListPersonSourcesContext(ctx, person.ID, 2)
	requirements.NoError(err)
	requirements.Len(items, 2)
	assertions.Equal([]int64{items[0].SourceID, items[1].SourceID}, sourceIDs[:2])
	for _, item := range items {
		assertions.Equal(at, item.LastActivityAt)
		assertions.Equal(12, item.TotalCount)
	}
	for _, limit := range []int{0, -1, 10, 11, 100} {
		items, err := st.ListPersonSourcesContext(ctx, person.ID, limit)
		requirements.NoError(err)
		assertions.Len(items, 10)
		assertions.Equal(12, items[0].TotalCount)
	}
	// Binding changes are reflected immediately, without a materialized view.
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM person_participants WHERE participant_id = ?`), participant)
	requirements.NoError(err)
	items, err = st.ListPersonSourcesContext(ctx, person.ID, 2)
	requirements.NoError(err)
	requirements.Len(items, 2)
	assertions.Equal(at.Add(-time.Hour), items[1].LastActivityAt)
	items, err = st.ListPersonSourcesContext(ctx, 999999, 10)
	requirements.NoError(err)
	assertions.Empty(items)
}

func TestPersonCardSourcesExcludeHistoricalAndSourcelessObservations(t *testing.T) {
	requirements := require.New(t)

	st := storetest.New(t).Store
	ctx := t.Context()
	person := cardPerson(t, st, "person@example.test")
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for index := range 2 {
		source, err := st.GetOrCreateSource("gmail", fmt.Sprintf("closed-%d@example.test", index))
		requirements.NoError(err)
		cardObservation(t, st, person.ParticipantIDs[0], source.ID, at)
		column := "active_until"
		if index == 1 {
			column = "superseded_at"
		}
		_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE participant_contact_observations SET `+column+` = ? WHERE source_id = ?`), at, source.ID)
		requirements.NoError(err)
	}
	_, err := st.RecordContactObservationContext(ctx, person.ParticipantIDs[0], store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "sourceless@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	requirements.NoError(err)
	items, err := st.ListPersonSourcesContext(ctx, person.ID, 10)
	requirements.NoError(err)
	assert.Empty(t, items)
}

func TestPersonCardReadsHonorCancellation(t *testing.T) {
	st := storetest.New(t).Store
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := st.ResolvePersonByEmailContext(ctx, "person@example.test")
	require.ErrorIs(t, err, context.Canceled)
	_, err = st.ListPersonSourcesContext(ctx, 1, 10)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPersonCardSourcesOrderByInstantAndHandleMissingObservationTime(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := storetest.New(t).Store
	ctx := t.Context()
	person := cardPerson(t, st, "person@example.test")
	first, err := st.GetOrCreateSource("gmail", "first@example.test")
	requirements.NoError(err)
	second, err := st.GetOrCreateSource("gmail", "second@example.test")
	requirements.NoError(err)
	at := time.Date(2026, 10, 17, 12, 0, 0, 123456789, time.UTC)
	cardObservation(t, st, person.ParticipantIDs[0], first.ID, at)
	delta := time.Nanosecond
	if st.IsPostgreSQL() {
		delta = time.Microsecond
	}
	// The later instant has an earlier wall-clock hour in its own zone.
	cardObservation(t, st, person.ParticipantIDs[0], second.ID, at.Add(delta).In(time.FixedZone("offset", -4*60*60)))
	items, err := st.ListPersonSourcesContext(ctx, person.ID, 10)
	requirements.NoError(err)
	requirements.Len(items, 2)
	assertions.Equal(second.ID, items[0].SourceID)
	assertions.WithinDuration(at.Add(delta), items[0].LastActivityAt, delta)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE participant_contact_observations
		SET observed_at = NULL, created_at = ? WHERE source_id = ?`), at.Add(time.Hour), first.ID)
	requirements.NoError(err)
	items, err = st.ListPersonSourcesContext(ctx, person.ID, 1)
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Equal(first.ID, items[0].SourceID)
	assertions.WithinDuration(at.Add(time.Hour), items[0].LastActivityAt, time.Microsecond)
}

func cardPerson(t *testing.T, st *store.Store, email string) *store.Person {
	t.Helper()
	participant, err := st.EnsureParticipantByIdentifier("email", email, "Example Person")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participant)
	require.NoError(t, err)
	_, err = st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: email,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	return person
}

func cardObservation(t *testing.T, st *store.Store, participant, source int64, at time.Time) {
	t.Helper()
	_, err := st.RecordContactObservationContext(t.Context(), participant, store.ParticipantContactObservationInput{
		SourceID: &source, AddressKind: store.ContactAddressEmail,
		OriginalValue: fmt.Sprintf("participant-%d@example.test", participant), ObservedAt: &at,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	require.NoError(t, err)
}
