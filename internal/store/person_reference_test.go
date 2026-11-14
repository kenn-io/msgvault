package store_test

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func TestResolvePersonReferenceForms(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f, person := newActivityQueryFixture(t)
	seedProjectedActivity(t, f, person, []time.Time{time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)})
	var participant int64
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id=?`), person).Scan(&participant))
	requirements.NoError(f.Store.SetParticipantIdentifier(participant, "phone", "+14155550123"))
	for _, tc := range []struct {
		kind  store.PersonRefKind
		value string
	}{{"person", strconv.FormatInt(person, 10)}, {"participant", strconv.FormatInt(participant, 10)}, {"email", "COUNTERPART@example.com"}, {"phone", "+1 (415) 555-0123"}, {"name", "Counterpart"}} {
		got, err := f.Store.ResolvePersonReferenceContext(t.Context(), tc.kind, tc.value, nil, 10)
		requirements.NoError(err)
		requirements.Len(got, 1)
		assertions.Equal(person, got[0].PersonID)
		assertions.NotEmpty(got[0].PersonUID)
	}
	got, err := f.Store.ResolvePersonReferenceContext(t.Context(), "name", "Counterpart", []int64{}, 10)
	requirements.NoError(err)
	assertions.Empty(got)
}
func TestResolvePersonReferenceHiddenMatchesDoNotChangeResult(t *testing.T) {
	requirements := require.New(t)
	f, person := newActivityQueryFixture(t)
	seedProjectedActivity(t, f, person, []time.Time{time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)})
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	before, err := f.Store.ResolvePersonReferenceContext(ctx, "name", "Counterpart", nil, 10)
	requirements.NoError(err)
	hidden := f.EnsureParticipant("hidden@example.com", "Counterpart", "example.com")
	_, _, err = f.Store.CreatePersonFromParticipant(hidden)
	requirements.NoError(err)
	hiddenSource, err := f.Store.GetOrCreateSource("gmail", "hidden-source@example.com")
	requirements.NoError(err)
	hiddenConv, err := f.Store.EnsureConversation(hiddenSource.ID, "hidden-thread", "Hidden")
	requirements.NoError(err)
	message := f.NewMessage().Build()
	message.SourceID = hiddenSource.ID
	message.ConversationID = hiddenConv
	message.SenderID = sql.NullInt64{Int64: hidden, Valid: true}
	_, err = f.Store.UpsertMessage(message)
	requirements.NoError(err)
	after, err := f.Store.ResolvePersonReferenceContext(ctx, "name", "Counterpart", nil, 10)
	requirements.NoError(err)
	assert.Equal(t, before, after)
}
func TestResolvePersonReferenceAmbiguousName(t *testing.T) {
	f, person := newActivityQueryFixture(t)
	seedProjectedActivity(t, f, person, []time.Time{time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)})
	for i := range 3 {
		p := f.EnsureParticipant(fmt.Sprintf("alex%d@example.com", i), "Alex", "example.com")
		_, _, err := f.Store.CreatePersonFromParticipant(p)
		require.NoError(t, err)
	}
	got, err := f.Store.ResolvePersonReferenceContext(t.Context(), "name", "Alex", nil, 2)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}
func TestResolvePersonReferenceNeverCreates(t *testing.T) {
	f, _ := newActivityQueryFixture(t)
	counts := func() [2]int {
		var c [2]int
		require.NoError(t, f.Store.DB().QueryRow(`SELECT COUNT(*) FROM participants`).Scan(&c[0]))
		require.NoError(t, f.Store.DB().QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&c[1]))
		return c
	}
	before := counts()
	got, err := f.Store.ResolvePersonReferenceContext(t.Context(), "email", "absent@example.com", nil, 10)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, before, counts())
}

func TestResolvePersonReferenceUnsavedCluster(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	other := f.EnsureParticipant("alias@example.com", "Alice alias", "example.com")
	_, err := f.Store.LinkParticipants(f.peer, other)
	requirements.NoError(err)
	got, err := f.Store.ResolvePersonReferenceContext(t.Context(), "participant", strconv.FormatInt(other, 10), nil, 10)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Zero(got[0].PersonID)
	members, err := f.Store.ClusterMembers(other)
	requirements.NoError(err)
	assertions.Equal(members, got[0].ParticipantIDs)
}

func TestResolvePersonReferenceGrantBeforeLimit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	var wanted int64
	for i := range 12 {
		participant := f.EnsureParticipant(fmt.Sprintf("candidate%d@example.com", i), "Alex", "example.com")
		person, _, err := f.Store.CreatePersonFromParticipant(participant)
		requirements.NoError(err)
		wanted = person.ID
	}
	got, err := f.Store.ResolvePersonReferenceContext(t.Context(), "name", "Alex", []int64{wanted}, 1)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(wanted, got[0].PersonID)
}

func TestPersonReferenceRejectsMalformedEmail(t *testing.T) {
	f := newTimelineActivityFixture(t)
	for _, value := range []string{"", "alice", "alice@", "@example.com", "Alice <alice@example.com>", "alice@example.com,other@example.com"} {
		t.Run(value, func(t *testing.T) {
			_, err := f.Store.ResolvePersonReferenceContext(t.Context(), "email", value, nil, 10)
			assert.ErrorIs(t, err, store.ErrInvalidActivityRequest)
		})
	}
}

func TestResolvePersonReferenceUnicodeCase(t *testing.T) {
	for _, field := range []string{"email", "identifier", "participant name", "saved name"} {
		t.Run(field, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineActivityFixture(t)
			kind, value := store.PersonRefKind("name"), "büro"
			var personID int64
			switch field {
			case "email":
				kind, value = "email", "büro@example.com"
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address='BÜRO@example.com' WHERE id=?`), f.peer)
				requirements.NoError(err)
			case "identifier":
				kind, value = "email", "büro@example.com"
				requirements.NoError(f.Store.SetParticipantIdentifier(f.peer, "email", "BÜRO@example.com"))
			case "participant name":
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET display_name='BÜRO' WHERE id=?`), f.peer)
				requirements.NoError(err)
			case "saved name":
				person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				requirements.NoError(err)
				personID = person.ID
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE persons SET display_name='BÜRO' WHERE id=?`), personID)
				requirements.NoError(err)
			}
			got, err := f.Store.ResolvePersonReferenceContext(t.Context(), kind, value, nil, 10)
			requirements.NoError(err)
			requirements.Len(got, 1)
			assertions.Equal(personID, got[0].PersonID)
			assertions.Equal([]int64{f.peer}, got[0].ParticipantIDs)
		})
	}
}
