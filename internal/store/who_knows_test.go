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

var whoKnowsSentAt = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// send stores a message from sender and returns its ID.
func send(f *storetest.Fixture, name string, sender int64, change ...func(*store.Message)) int64 {
	f.T.Helper()
	m := f.NewMessage().WithSourceMessageID(name).WithSubject("Re: " + name).
		WithSnippet("snippet " + name).WithSentAt(whoKnowsSentAt).Build()
	m.SenderID = sql.NullInt64{Int64: sender, Valid: sender != 0}
	for _, c := range change {
		c(m)
	}
	id, err := f.Store.UpsertMessage(m)
	require.NoError(f.T, err)
	return id
}

func TestTopicAuthorsReadsSendersAtRequestTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))

	ana := f.EnsureParticipant("ana@example.net", "Ana Sender", "example.net")
	ben := f.EnsureParticipant("ben@example.net", "Ben Contractor", "example.net")
	owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	anaPerson, _, err := f.Store.CreatePersonFromParticipant(ana)
	require.NoError(err)

	saved := send(f, "saved", ana)
	unsaved := send(f, "unsaved", ben, func(m *store.Message) { m.ListID = sql.NullString{String: "<list.example.net>", Valid: true} })
	outbound := send(f, "outbound", owner)
	byEnvelope := send(f, "envelope", 0)
	require.NoError(f.Store.ReplaceMessageRecipients(byEnvelope, "from", []int64{ben, ana}, []string{"Ben", "Ana"}))
	deleted := send(f, "deleted", ana)
	require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "deleted"))

	got, err := f.Store.TopicAuthorsContext(ctx, []int64{saved, unsaved, outbound, byEnvelope, deleted, 999999})
	require.NoError(err)
	assert.Len(got, 4, "deleted and missing messages are left out")

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
	assert.True(whoKnowsSentAt.Equal(*a.SentAt))
	assert.False(a.IsFromMe)
	assert.False(a.ListMail)
	assert.False(a.Automated)
	assert.False(a.SenderAuthenticated, "an email From header is an assertion")

	// Sender bound to no person, writing through a list.
	b := got[unsaved]
	assert.Nil(b.PersonID)
	assert.Equal(ben, b.SenderID)
	assert.True(b.ListMail)

	assert.True(got[outbound].IsFromMe)

	// With no sender_id, the lowest 'from' recipient is the sender.
	assert.Equal(min(ben, ana), got[byEnvelope].SenderID)
}

func TestTopicAuthorsReadsProducerAutomationAndAuthentication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	slack, err := f.Store.GetOrCreateSource("slack", "workspace")
	require.NoError(err)
	conversation, err := f.Store.EnsureConversation(slack.ID, "channel", "Channel")
	require.NoError(err)
	user := f.EnsureParticipant("user@example.net", "User", "example.net")

	post := func(name, metadata string) int64 {
		m := f.NewMessage().WithSourceMessageID(name).Build()
		m.SourceID, m.ConversationID = slack.ID, conversation
		m.SenderID = sql.NullInt64{Int64: user, Valid: true}
		id, err := f.Store.UpsertMessage(m)
		require.NoError(err)
		if metadata != "" {
			require.NoError(f.Store.SetMessageMetadata(id, sql.NullString{String: metadata, Valid: true}))
		}
		return id
	}
	human := post("human", `{"author_kind":"user"}`)
	bot := post("bot", `{"author_kind":"bot","automated":true}`)
	hook := post("hook", `{"author_kind":"webhook","automated":true}`)
	plain := post("plain", "")
	notAutomated := post("explicit", `{"automated":false}`)

	got, err := f.Store.TopicAuthorsContext(t.Context(), []int64{human, bot, hook, plain, notAutomated})
	require.NoError(err)
	assert.False(got[human].Automated)
	assert.True(got[bot].Automated)
	assert.True(got[hook].Automated)
	assert.False(got[plain].Automated)
	assert.False(got[notAutomated].Automated)
	assert.True(got[human].SenderAuthenticated, "the source sets a Slack sender")
}

func TestOwnerAddressedFindsParticipantsTheOwnerWroteTo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	ana := f.EnsureParticipant("ana@example.net", "Ana", "example.net")
	anaOther := f.EnsureParticipant("ana.work@example.net", "Ana Work", "example.net")
	cc := f.EnsureParticipant("cc@example.net", "Copied", "example.net")
	vendor := f.EnsureParticipant("vendor@example.net", "Vendor", "example.net")
	chatty := f.EnsureParticipant("chatty@example.net", "Chatty", "example.net")
	_, err := f.Store.LinkParticipants(ana, anaOther)
	require.NoError(err)
	_, _, err = f.Store.CreatePersonFromParticipant(ana)
	require.NoError(err)

	mail := send(f, "to-ana", owner)
	require.NoError(f.Store.ReplaceMessageRecipients(mail, "to", []int64{anaOther}, []string{"Ana Work"}))
	require.NoError(f.Store.ReplaceMessageRecipients(mail, "cc", []int64{cc}, []string{"Copied"}))
	// Mail the vendor sent to the owner does not count.
	inbound := send(f, "from-vendor", vendor)
	require.NoError(f.Store.ReplaceMessageRecipients(inbound, "to", []int64{owner}, []string{"Owner"}))
	// A chat the owner posted in counts for its members.
	chat, err := f.Store.EnsureConversationWithType(f.Source.ID, "room", "group_chat", "Room")
	require.NoError(err)
	require.NoError(f.Store.EnsureConversationParticipant(chat, chatty, "member"))
	send(f, "chat-post", owner, func(m *store.Message) { m.ConversationID = chat; m.MessageType = "chat" })

	got, err := f.Store.OwnerAddressedContext(t.Context(), []int64{ana, cc, vendor, chatty})
	require.NoError(err)
	assert.Equal(map[int64]bool{ana: true, cc: true, chatty: true}, got,
		"ana counts through her bound second address; the vendor was never written to")
	none, err := f.Store.OwnerAddressedContext(t.Context(), nil)
	require.NoError(err)
	assert.Empty(none)
}

func TestAskMeAboutValuesReadsUserValuesForNonOwners(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
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
	newPerson := func(email string) (participant, person int64) {
		participant = f.EnsureParticipant(email, email, "example.net")
		created, _, err := f.Store.CreatePersonFromParticipant(participant)
		require.NoError(err)
		return participant, created.ID
	}
	_, asked := newPerson("asked@example.net")
	_, inferred := newPerson("inferred@example.net")
	_, superseded := newPerson("superseded@example.net")
	ownerParticipant, owner := newPerson("owner@example.net")
	setValue(asked, "kubernetes", store.ProvenanceUser)
	setValue(inferred, "robotics", store.ProvenanceEnrichment)
	setValue(superseded, "old topic", store.ProvenanceUser)
	setValue(superseded, "new topic", store.ProvenanceUser)
	setValue(owner, "my own topic", store.ProvenanceUser)
	send(f, "mine", ownerParticipant)

	got, err := f.Store.AskMeAboutValuesContext(ctx)
	require.NoError(err)
	assert.Equal([]store.PersonAskMeAbout{
		{PersonID: asked, PersonName: "asked@example.net", Value: "kubernetes"},
		{PersonID: superseded, PersonName: "superseded@example.net", Value: "new topic"},
	}, got, "inferred values, superseded values, and the owner are left out")
}
