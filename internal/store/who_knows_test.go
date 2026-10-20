package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestTopicAuthorsReadsSendersAtRequestTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))

	ana := f.EnsureParticipant("ana@example.net", "Ana Sender", "example.net")
	ben := f.EnsureParticipant("ben@example.net", "Ben Contractor", "example.net")
	owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	alias := f.EnsureParticipant("owner.work@example.net", "Owner Work", "example.net")
	_, err := f.Store.LinkParticipants(owner, alias)
	require.NoError(err)
	ownerPerson, _, err := f.Store.CreatePersonFromParticipant(owner)
	require.NoError(err)
	anaPerson, _, err := f.Store.CreatePersonFromParticipant(ana)
	require.NoError(err)

	sentAt := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	send := func(name string, sender int64, opts ...func(*store.Message)) int64 {
		m := f.NewMessage().WithSourceMessageID(name).WithSubject("Re: " + name).
			WithSnippet("snippet " + name).WithSentAt(sentAt).Build()
		m.SenderID = sql.NullInt64{Int64: sender, Valid: sender != 0}
		for _, opt := range opts {
			opt(m)
		}
		id, err := f.Store.UpsertMessage(m)
		require.NoError(err)
		return id
	}
	saved := send("saved", ana)
	unsaved := send("unsaved", ben)
	outbound := send("outbound", owner)
	aliased := send("aliased", alias)
	byEnvelope := send("envelope", 0)
	require.NoError(f.Store.ReplaceMessageRecipients(byEnvelope, "from", []int64{ben, ana}, []string{"Ben", "Ana"}))
	deleted := send("deleted", ana)
	require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "deleted"))
	nonexistent := int64(999999)

	got, err := f.Store.TopicAuthorsContext(ctx, []int64{saved, unsaved, outbound, aliased, byEnvelope, deleted, nonexistent})
	require.NoError(err)
	assert.Len(got, 5, "deleted and missing messages are left out")

	// Saved sender, with no activity index built.
	a := got[saved]
	require.NotNil(a.PersonID)
	assert.Equal(anaPerson.ID, *a.PersonID)
	assert.Equal(ana, a.SenderID)
	assert.Equal("Ana Sender", a.SenderName)
	assert.Equal("ana@example.net", a.SenderEmail)
	assert.Equal("Re: saved", a.Subject)
	assert.Equal("snippet saved", a.Snippet)
	assert.Equal("email", a.MessageType)
	require.NotNil(a.SentAt)
	assert.True(sentAt.Equal(*a.SentAt))
	assert.False(a.IsFromMe)
	assert.False(a.OwnerLinked)

	// Sender bound to no person.
	b := got[unsaved]
	assert.Nil(b.PersonID)
	assert.Equal(ben, b.SenderID)
	assert.False(b.OwnerLinked)

	// The owner's own message is flagged, and its person is owner-linked.
	o := got[outbound]
	assert.True(o.IsFromMe)
	assert.True(o.OwnerLinked)
	require.NotNil(o.PersonID)
	assert.Equal(ownerPerson.ID, *o.PersonID)

	// An address the owner never registered, but bound to the owner's person.
	l := got[aliased]
	assert.False(l.IsFromMe)
	assert.True(l.OwnerLinked)
	require.NotNil(l.PersonID)
	assert.Equal(ownerPerson.ID, *l.PersonID)

	// With no sender_id, the lowest 'from' recipient is the sender.
	e := got[byEnvelope]
	assert.Equal(min(ben, ana), e.SenderID)
}

func TestTopicAuthorsBound(t *testing.T) {
	f := storetest.New(t)
	ids := make([]int64, store.MaxTopicAuthorMessages+1)
	_, err := f.Store.TopicAuthorsContext(t.Context(), ids)
	require.Error(t, err)
	got, err := f.Store.TopicAuthorsContext(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestAskMeAboutValuesReadsUserValuesForRequestedPeople(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	setValue := func(personID int64, text string, source store.Provenance) {
		_, err := f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID:       personID,
			DefinitionSlug: store.AttributeSlugAskMeAbout,
			Ordinal:        new(int64),
			Value:          store.AttributeValue{Type: store.AttributeValueText, Text: &text},
			Source:         source,
		})
		require.NoError(err)
	}
	newPerson := func(email string) int64 {
		participant := f.EnsureParticipant(email, email, "example.net")
		person, _, err := f.Store.CreatePersonFromParticipant(participant)
		require.NoError(err)
		return person.ID
	}
	asked := newPerson("asked@example.net")
	other := newPerson("other@example.net")
	inferred := newPerson("inferred@example.net")
	superseded := newPerson("superseded@example.net")
	setValue(asked, "kubernetes", store.ProvenanceUser)
	setValue(other, "billing", store.ProvenanceUser)
	setValue(inferred, "robotics", store.ProvenanceEnrichment)
	setValue(superseded, "old topic", store.ProvenanceUser)
	setValue(superseded, "new topic", store.ProvenanceUser)

	got, err := f.Store.AskMeAboutValuesContext(ctx, []int64{asked, inferred, superseded})
	require.NoError(err)
	assert.Equal([]store.PersonAskMeAbout{
		{PersonID: asked, Value: "kubernetes"},
		{PersonID: superseded, Value: "new topic"},
	}, got, "other people, inferred values, and superseded values are left out")

	none, err := f.Store.AskMeAboutValuesContext(ctx, nil)
	require.NoError(err)
	assert.Empty(none)
	_, err = f.Store.AskMeAboutValuesContext(ctx, make([]int64, store.MaxAskMeAboutPeople+1))
	assert.Error(err)
}
