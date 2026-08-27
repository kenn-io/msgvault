package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func slugify(name string) string { return strings.ReplaceAll(name, " ", "_") }

func mustAttributePerson(t *testing.T, st *store.Store) int64 {
	t.Helper()
	participant, err := st.EnsureParticipant("alice@example.com", "alice", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	return person.ID
}

func TestSetPersonAttributeValueStoresEveryTypedColumn(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	person := mustAttributePerson(t, st)
	timestamp := time.Date(2026, 7, 30, 9, 30, 0, 0, time.UTC)

	tests := []struct {
		name      string
		valueType store.AttributeValueType
		fieldType store.AttributeFieldType
		value     store.AttributeValue
	}{
		{"text", store.AttributeValueText, store.AttributeFieldText,
			store.AttributeValue{Type: store.AttributeValueText, Text: new("board games")}},
		{"integer", store.AttributeValueInteger, store.AttributeFieldDuration,
			store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(30))}},
		{"real", store.AttributeValueReal, store.AttributeFieldText,
			store.AttributeValue{Type: store.AttributeValueReal, Real: new(1.5)}},
		{"boolean", store.AttributeValueBoolean, store.AttributeFieldCheckbox,
			store.AttributeValue{Type: store.AttributeValueBoolean, Boolean: new(true)}},
		{"date", store.AttributeValueDate, store.AttributeFieldDate,
			store.AttributeValue{Type: store.AttributeValueDate, Date: new("2026-07-30")}},
		{"timestamp", store.AttributeValueTimestamp, store.AttributeFieldTimestamp,
			store.AttributeValue{Type: store.AttributeValueTimestamp, Timestamp: &timestamp}},
		{"json", store.AttributeValueJSON, store.AttributeFieldJSON,
			store.AttributeValue{Type: store.AttributeValueJSON,
				JSON: json.RawMessage(`{"kind":"synthetic"}`)}},
		{"record reference", store.AttributeValueRecordReference, store.AttributeFieldPerson,
			store.AttributeValue{Type: store.AttributeValueRecordReference,
				RecordType: new("person"), RecordID: &person}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			definition := personTextDefinition("typed_" + slugify(test.name))
			definition.UniversalID = "test-typed-" + slugify(test.name)
			definition.ValueType = test.valueType
			definition.FieldType = test.fieldType
			if test.valueType == store.AttributeValueRecordReference {
				definition.RecordTarget = new("person")
			}
			created, err := st.CreateAttributeDefinitionContext(ctx, definition)
			require.NoError(err)

			write, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
				PersonID: person, DefinitionSlug: created.Slug,
				Value: test.value, Source: store.ProvenanceUser,
			})
			require.NoError(err)
			require.NotNil(write.Value)
			assert.Equal(test.valueType, write.Value.Value.Type)
			assert.Nil(write.Superseded)

			current, err := st.ListPersonAttributeValuesContext(ctx, person,
				store.PersonAttributeQuery{DefinitionSlug: created.Slug})
			require.NoError(err)
			require.Len(current, 1)
			assert.Equal(write.Value.ID, current[0].ID)
		})
	}
}

func TestDeletePersonRejectsStoredRecordReferences(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	sourceParticipant, err := st.EnsureParticipant(
		"source@example.com", "source", "example.com")
	require.NoError(err)
	source, _, err := st.CreatePersonFromParticipant(sourceParticipant)
	require.NoError(err)
	targetParticipant, err := st.EnsureParticipant(
		"target@example.com", "target", "example.com")
	require.NoError(err)
	target, _, err := st.CreatePersonFromParticipant(targetParticipant)
	require.NoError(err)

	definition := personTextDefinition("related_person")
	definition.UniversalID = "test-related-person"
	definition.ValueType = store.AttributeValueRecordReference
	definition.FieldType = store.AttributeFieldPerson
	definition.RecordTarget = new("person")
	_, err = st.CreateAttributeDefinitionContext(ctx, definition)
	require.NoError(err)
	write, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: source.ID, DefinitionSlug: definition.Slug,
		Value: store.AttributeValue{
			Type: store.AttributeValueRecordReference, RecordType: new("person"),
			RecordID: &target.ID,
		},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)

	err = st.DeletePersonContext(ctx, target.ID, target.Revision)
	require.ErrorIs(err, store.ErrPersonReferenced)
	_, err = st.GetPersonContext(ctx, target.ID)
	require.NoError(err, "referenced person must remain after a rejected delete")

	_, err = st.SupersedePersonAttributeValueContext(ctx,
		store.PersonAttributeSupersedeInput{
			PersonID: source.ID, DefinitionSlug: definition.Slug,
			ExpectedValueID: &write.Value.ID,
		})
	require.NoError(err)
	err = st.DeletePersonContext(ctx, target.ID, target.Revision)
	require.NoError(err,
		"superseded references must not block deletion; only current values do")
}

func TestSetPersonAttributeValueRejectsInvalidShapes(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	person := mustAttributePerson(t, st)

	tests := []store.PersonAttributeValueInput{
		{
			PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(7))},
			Source: store.ProvenanceUser,
		},
		{
			PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText},
			Source: store.ProvenanceUser,
		},
		{
			PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value: store.AttributeValue{Type: store.AttributeValueText,
				Text: new("email"), Integer: new(int64(7))},
			Source: store.ProvenanceUser,
		},
		{
			PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Ordinal: new(int64(1)),
			Value:   store.AttributeValue{Type: store.AttributeValueText, Text: new("email")},
			Source:  store.ProvenanceUser,
		},
		{
			PersonID: person, DefinitionSlug: store.AttributeSlugAskMeAbout,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("sailing")},
			Source: store.ProvenanceUser, Confidence: new(0.9),
		},
	}
	for _, input := range tests {
		_, err := st.SetPersonAttributeValueContext(ctx, input)
		require.ErrorIs(t, err, store.ErrAttributeValueInvalid)
	}

	_, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugLastContacted,
		Value: store.AttributeValue{Type: store.AttributeValueTimestamp,
			Timestamp: new(time.Now())},
		Source: store.ProvenanceSystem,
	})
	require.ErrorIs(t, err, store.ErrAttributeDefinitionNotWritable)
}

func TestPersonAttributeSupersedeHistoryCASAndDryRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	person := mustAttributePerson(t, st)

	first, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("email")},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)

	second, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("chat")},
		Source: store.ProvenanceUser, ExpectedValueID: &first.Value.ID,
	})
	require.NoError(err)
	require.NotNil(second.Superseded)
	assert.Equal(first.Value.ID, second.Superseded.ID)
	require.NotNil(second.Superseded.ActiveUntil)
	assert.True(second.Superseded.ActiveUntil.Equal(second.Value.ActiveFrom))

	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("phone")},
		Source: store.ProvenanceUser, ExpectedValueID: &first.Value.ID,
	})
	require.ErrorIs(err, store.ErrAttributeValueConflict)

	preview, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("sms")},
		Source: store.ProvenanceUser, DryRun: true,
	})
	require.NoError(err)
	assert.True(preview.DryRun)
	assert.Zero(preview.Value.ID)

	history, err := st.ListPersonAttributeValuesContext(ctx, person,
		store.PersonAttributeQuery{
			DefinitionSlug: store.AttributeSlugPrimaryChannel, IncludeHistory: true})
	require.NoError(err)
	require.Len(history, 2)
	assert.Equal(second.Value.ID, history[0].ID)
	assert.Equal(first.Value.ID, history[1].ID)

	cleared, err := st.SupersedePersonAttributeValueContext(ctx,
		store.PersonAttributeSupersedeInput{
			PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			ExpectedValueID: &second.Value.ID,
		})
	require.NoError(err)
	assert.Nil(cleared.Value)
	require.NotNil(cleared.Superseded)
}

func TestInactiveDefinitionBlocksSetButAllowsSupersede(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	person := mustAttributePerson(t, st)

	definition := personTextDefinition("retired_field")
	definition.UniversalID = "test-retired-field"
	created, err := st.CreateAttributeDefinitionContext(ctx, definition)
	require.NoError(err)
	write, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: created.Slug,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("stale")},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)

	inactive := false
	_, err = st.UpdateAttributeDefinitionContext(ctx, created.ID, created.Revision,
		store.AttributeDefinitionUpdate{IsActive: &inactive})
	require.NoError(err)

	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: created.Slug,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("new")},
		Source: store.ProvenanceUser,
	})
	require.ErrorIs(err, store.ErrAttributeDefinitionInactive)

	cleared, err := st.SupersedePersonAttributeValueContext(ctx,
		store.PersonAttributeSupersedeInput{
			PersonID: person, DefinitionSlug: created.Slug,
			ExpectedValueID: &write.Value.ID,
		})
	require.NoError(err, "retracting a value from a retired definition must stay possible")
	require.NotNil(cleared.Superseded)
}

func TestBackdatedReplacementSeparatesValidityAndAuditTimes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	person := mustAttributePerson(t, st)
	originalActiveFrom := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	replacementActiveFrom := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)

	first, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:      store.AttributeValue{Type: store.AttributeValueText, Text: new("email")},
		ActiveFrom: &originalActiveFrom, Source: store.ProvenanceUser,
	})
	require.NoError(err)

	beforeWrite := time.Now().UTC().Add(-time.Second)
	replacement, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:      store.AttributeValue{Type: store.AttributeValueText, Text: new("chat")},
		ActiveFrom: &replacementActiveFrom, Source: store.ProvenanceUser,
		ExpectedValueID: &first.Value.ID,
	})
	afterWrite := time.Now().UTC().Add(time.Second)
	require.NoError(err)
	require.NotNil(replacement.Superseded)
	require.NotNil(replacement.Superseded.ActiveUntil)
	require.NotNil(replacement.Superseded.SupersededAt)
	assert.Equal(replacementActiveFrom, *replacement.Superseded.ActiveUntil)
	assert.True(replacement.Superseded.SupersededAt.After(beforeWrite))
	assert.True(replacement.Superseded.SupersededAt.Before(afterWrite))
	assert.Equal(replacement.Value.CreatedAt, *replacement.Superseded.SupersededAt,
		"one transaction timestamp must bind the old audit close and new audit open")
}

func TestPersonAttributePublicDryRunAndCASDoNotPersistPins(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	person := mustAttributePerson(t, st)

	preview, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("email")},
		Source: store.ProvenanceUser, DryRun: true,
	})
	require.NoError(err)
	assert.Zero(preview.Value.ID)
	pins, err := st.ListPersonFactPinsContext(ctx, person)
	require.NoError(err)
	assert.Empty(pins)

	committed, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("email")},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	stale := committed.Value.ID + 1000
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("chat")},
		Source: store.ProvenanceUser, ExpectedValueID: &stale,
	})
	require.ErrorIs(err, store.ErrAttributeValueConflict)
	pins, err = st.ListPersonFactPinsContext(ctx, person)
	require.NoError(err)
	require.Len(pins, 1)
}

func TestMultiCardinalityAppendsPerOrdinalAndDerivedConfidenceIsAllowed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	person := mustAttributePerson(t, st)

	first, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugAskMeAbout,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("sailing")},
		Source: store.ProvenanceExtraction, Confidence: new(0.62),
	})
	require.NoError(err)
	second, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugAskMeAbout,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("ceramics")},
		Source: store.ProvenanceSystem, Confidence: new(0.5),
	})
	require.NoError(err)
	assert.Equal(int64(0), first.Value.Ordinal)
	assert.Equal(int64(1), second.Value.Ordinal)
	assert.InDelta(0.5, *second.Value.Confidence, 0.0001)

	_, err = st.SupersedePersonAttributeValueContext(ctx,
		store.PersonAttributeSupersedeInput{
			PersonID: person, DefinitionSlug: store.AttributeSlugAskMeAbout,
			Ordinal: &second.Value.Ordinal,
		})
	require.NoError(err)
	third, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person, DefinitionSlug: store.AttributeSlugAskMeAbout,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("poetry")},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	assert.Equal(int64(2), third.Value.Ordinal,
		"appending must not reuse a superseded slot's ordinal and adopt its history")
}
