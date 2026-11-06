package store_test

import (
	"database/sql"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
	"time"
)

func TestKeepInTouchPeoplePageAndCountedCadence(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	counterpart := f.EnsureParticipant("counterpart@example.com", "Example Person", "example.com")
	person, _, err := f.Store.CreatePersonFromParticipant(counterpart)
	require.NoError(err)
	id := person.ID
	rows, err := f.Store.ListKeepInTouchPeopleContext(t.Context(), 0, 1)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(id, rows[0].ID)
	rows, err = f.Store.ListKeepInTouchPeopleContext(t.Context(), id, 1)
	require.NoError(err)
	assert.Empty(rows)
	last := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	messageID := f.NewMessage().WithSourceMessageID("synthetic-outreach-contact").WithSentAt(last).Create(t, f.Store)
	require.NoError(f.Store.ReplaceMessageRecipients(messageID, "from", []int64{counterpart}, []string{"Example Person"}))
	require.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", []int64{owner}, []string{"Owner"}))
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 2, MaxDirectCounterparts: 25})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	options := store.PersonTierOptions{Config: persontiers.DefaultConfig(), ActivityTimezone: "UTC", ActivityMaxDirect: 25}
	missing, err := f.Store.KeepInTouchContactStateContext(t.Context(), id, last.AddDate(0, 0, 20), options)
	require.NoError(err)
	assert.Nil(missing.LastContactAt, "people without a saved cadence do not need a history scan")
	assert.Nil(missing.CadenceDueAt)
	days := int64(21)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: id, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser})
	require.NoError(err)
	state, err := f.Store.ContactStateContext(t.Context(), id, last.AddDate(0, 0, 20))
	require.NoError(err)
	require.NotNil(state.CadenceDueAt)
	assert.Equal(last.AddDate(0, 0, 21), *state.CadenceDueAt)
	require.NoError(f.Store.MarkContactStateDirtyContext(t.Context(), id))
	state, err = f.Store.ContactStateContext(t.Context(), id, last.AddDate(0, 0, 20))
	require.NoError(err)
	assert.True(state.Stale)
}

func TestKeepInTouchIgnoresNewsletterAndOldImportedContact(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	other := f.EnsureParticipant("person@example.com", "Example", "example.com")
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	person, _, err := f.Store.CreatePersonFromParticipant(other)
	require.NoError(err)
	last := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	insert := func(key string, at time.Time, list bool) int64 {
		m := f.NewMessage().WithSourceMessageID(key).WithSentAt(at).Build()
		if list {
			m.ListID = sql.NullString{String: "news.example.com", Valid: true}
		}
		id, err := f.Store.UpsertMessage(m)
		Require.NoError(t, err)
		Require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{other}, []string{"Example"}))
		Require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, []string{"Owner"}))
		return id
	}
	_ = insert("meaningful", last, false)
	_ = insert("newsletter", last.AddDate(0, 0, 10), true)
	_ = insert("old-import", last.AddDate(0, 0, -50), false)
	days := int64(21)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser})
	require.NoError(err)
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	options := store.PersonTierOptions{Config: persontiers.DefaultConfig(), ActivityTimezone: "UTC", ActivityMaxDirect: 25}
	state, err := f.Store.KeepInTouchContactStateContext(t.Context(), person.ID, last.AddDate(0, 0, 30), options)
	require.NoError(err)
	require.NotNil(state.LastContactAt)
	assert.Equal(last, *state.LastContactAt)
	require.NotNil(state.CadenceDueAt)
	assert.Equal(last.AddDate(0, 0, 21), *state.CadenceDueAt)
	recent := last.AddDate(0, 0, 25)
	_ = insert("real-contact", recent, false)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	state, err = f.Store.KeepInTouchContactStateContext(t.Context(), person.ID, recent, options)
	require.NoError(err)
	assert.Equal(recent, *state.LastContactAt)
	assert.Equal(recent.AddDate(0, 0, 21), *state.CadenceDueAt)
}

func TestKeepInTouchCheckpointCompareAndSwap(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	f := storetest.New(t)
	cursor, err := f.Store.KeepInTouchCursorContext(t.Context())
	require.NoError(err)
	assert.Zero(cursor)
	advanced, err := f.Store.AdvanceKeepInTouchCursorContext(t.Context(), 0, 42)
	require.NoError(err)
	assert.True(advanced)
	advanced, err = f.Store.AdvanceKeepInTouchCursorContext(t.Context(), 0, 7)
	require.NoError(err)
	assert.False(advanced)
	advanced, err = f.Store.AdvanceKeepInTouchCursorContext(t.Context(), 42, 0)
	require.NoError(err)
	assert.True(advanced)
	cursor, err = f.Store.KeepInTouchCursorContext(t.Context())
	require.NoError(err)
	assert.Zero(cursor)
	_, err = f.Store.AdvanceKeepInTouchCursorContext(t.Context(), -1, 3)
	require.Error(err)
}
