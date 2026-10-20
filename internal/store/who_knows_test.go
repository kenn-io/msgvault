package store_test

import (
	"database/sql"
	"maps"
	"slices"
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
	alias := f.EnsureParticipant("owner.work@example.net", "Owner Work", "example.net")
	_, err := f.Store.LinkParticipants(owner, alias)
	require.NoError(err)
	ownerPerson, _, err := f.Store.CreatePersonFromParticipant(owner)
	require.NoError(err)
	anaPerson, _, err := f.Store.CreatePersonFromParticipant(ana)
	require.NoError(err)

	saved := send(f, "saved", ana)
	unsaved := send(f, "unsaved", ben, func(m *store.Message) { m.ListID = sql.NullString{String: "<list.example.net>", Valid: true} })
	outbound := send(f, "outbound", owner)
	aliased := send(f, "aliased", alias)
	byEnvelope := send(f, "envelope", 0)
	require.NoError(f.Store.ReplaceMessageRecipients(byEnvelope, "from", []int64{ben, ana}, []string{"Ben", "Ana"}))
	deleted := send(f, "deleted", ana)
	require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "deleted"))

	got, err := f.Store.TopicAuthorsContext(ctx, []int64{saved, unsaved, outbound, aliased, byEnvelope, deleted, 999999})
	require.NoError(err)
	assert.Len(got, 5, "deleted and missing messages are left out")

	// A saved sender with no activity index built.
	a := got[saved]
	require.NotNil(a.PersonID)
	assert.Equal(anaPerson.ID, *a.PersonID)
	assert.Equal(ana, a.SenderID)
	assert.Equal("Ana Sender", a.SenderName)
	assert.Equal("Re: saved", a.Subject)
	assert.Equal("email", a.MessageType)
	require.NotNil(a.SentAt)
	assert.True(whoKnowsSentAt.Equal(*a.SentAt))
	assert.False(a.SenderAuthenticated, "an email From header is an assertion")

	// A sender bound to no person, writing through a list.
	assert.Nil(got[unsaved].PersonID)
	assert.True(got[unsaved].ListMail)
	assert.False(got[saved].ListMail)

	// The owner's own mail, and mail from an address bound to the owner's
	// person that no account identity lists.
	assert.True(got[outbound].IsFromMe)
	assert.True(got[outbound].OwnerLinked)
	assert.False(got[aliased].IsFromMe)
	assert.True(got[aliased].OwnerLinked)
	require.NotNil(got[aliased].PersonID)
	assert.Equal(ownerPerson.ID, *got[aliased].PersonID)
	assert.False(got[saved].OwnerLinked)

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

	post := func(name, metadata, identifierType, identifier string) int64 {
		sender := f.EnsureParticipant(name+"@example.net", name, "example.net")
		if identifier != "" {
			require.NoError(f.Store.SetParticipantIdentifier(sender, identifierType, identifier))
		}
		m := f.NewMessage().WithSourceMessageID(name).Build()
		m.SourceID, m.ConversationID = slack.ID, conversation
		m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(m)
		require.NoError(err)
		if metadata != "" {
			require.NoError(f.Store.SetMessageMetadata(id, sql.NullString{String: metadata, Valid: true}))
		}
		return id
	}
	automated := map[string]int64{
		"discord":   post("discord", `{"author_kind":"bot","automated":true}`, "", ""),
		"slack":     post("slack", "", "slack", "T1:bot:B1"),
		"agent":     post("agent", "", "chatwoot", "acct/agent_bot/7"),
		"assistant": post("assistant", "", "chatwoot", "acct/captain_assistant/8"),
	}
	humans := map[string]int64{
		"plain":     post("plain", "", "", ""),
		"explicit":  post("explicit", `{"automated":false}`, "", ""),
		"slackuser": post("slackuser", "", "slack", "T1:U1"),
		"agentuser": post("agentuser", "", "chatwoot", "acct/agent/9"),
	}
	ids := slices.Concat(slices.Collect(maps.Values(automated)), slices.Collect(maps.Values(humans)))
	got, err := f.Store.TopicAuthorsContext(t.Context(), ids)
	require.NoError(err)
	for name, id := range automated {
		assert.True(got[id].Automated, name)
	}
	for name, id := range humans {
		assert.False(got[id].Automated, name)
	}
	assert.True(got[humans["plain"]].SenderAuthenticated, "the source sets a Slack sender")
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
	alias := f.EnsureParticipant("owner.work@example.net", "Owner Work", "example.net")
	_, err := f.Store.LinkParticipants(ownerParticipant, alias)
	require.NoError(err)
	setValue(asked, "Straße networking", store.ProvenanceUser)
	setValue(inferred, "kubernetes robotics", store.ProvenanceEnrichment)
	setValue(superseded, "old topic", store.ProvenanceUser)
	setValue(superseded, "new topic", store.ProvenanceUser)
	setValue(owner, "my own topic", store.ProvenanceUser)

	got, truncated, err := f.Store.AskMeAboutValuesContext(ctx)
	require.NoError(err)
	assert.False(truncated)
	assert.Equal([]store.PersonAskMeAbout{
		{PersonID: asked, PersonName: "asked@example.net", Value: "Straße networking"},
		{PersonID: superseded, PersonName: "superseded@example.net", Value: "new topic"},
	}, got, "inferred and superseded values, and the owner's own person, are left out")

	// Copy the first value past the cap: the read returns the cap and says more exist.
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO person_attribute_values (person_id, definition_id, ordinal, value_text, source)
		SELECT v.person_id, v.definition_id, v.ordinal + n.i, v.value_text, v.source
		  FROM person_attribute_values v, n WHERE v.person_id = ? AND v.superseded_at IS NULL`),
		store.DeclaredValueLimit, asked)
	require.NoError(err)
	got, truncated, err = f.Store.AskMeAboutValuesContext(ctx)
	require.NoError(err)
	assert.True(truncated)
	assert.Len(got, store.DeclaredValueLimit)
}
