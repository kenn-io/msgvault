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

	a := got[saved]
	assert.Equal(ana, a.SenderID)
	assert.Equal("Ana Sender", a.SenderName)
	assert.Equal("ana@example.net", a.SenderEmail)
	assert.Equal("Re: saved", a.Subject)
	assert.Equal("email", a.MessageType)
	require.NotNil(a.SentAt)
	assert.True(whoKnowsSentAt.Equal(*a.SentAt))
	assert.False(a.SenderAuthenticated, "an email From header is an assertion")
	assert.False(a.ListMail)
	assert.False(a.IsFromMe)

	assert.True(got[unsaved].ListMail)
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

	post := func(name, metadata, identifierType, identifier string) int64 {
		sender := f.EnsureParticipant(name+"@example.net", name, "example.net")
		if identifier != "" {
			require.NoError(f.Store.SetParticipantIdentifier(sender, identifierType, identifier))
		}
		m := f.NewMessage().WithSourceMessageID(name).Build()
		m.SourceID, m.ConversationID, m.MessageType = slack.ID, conversation, "slack"
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
	// The Slack importer files a bot relaying under a user's ID by the user,
	// and keeps the payload that shows the bot. A payload that can't be read
	// leaves the message counted.
	payload := func(id int64, raw string) {
		require.NoError(f.Store.UpsertMessageRawWithFormatContext(t.Context(), id, []byte(raw), "slack_json"))
	}
	automated["relayed"] = post("relayed", "", "slack", "T1:U2")
	payload(automated["relayed"], `{"type":"message","user":"U2","bot_id":"B2","text":"deploy finished"}`)
	automated["bot message"] = post("botmessage", "", "", "")
	payload(automated["bot message"], `{"type":"message","subtype":"bot_message","text":"alert"}`)
	humans["user payload"] = post("userpayload", "", "slack", "T1:U3")
	payload(humans["user payload"], `{"type":"message","user":"U3","text":"hello"}`)
	humans["unreadable"] = post("unreadable", "", "", "")
	payload(humans["unreadable"], `not json`)

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

func TestAuthorIdentitiesGroupLinkedAndBoundSenders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	ownerAlias := f.EnsureParticipant("owner.work@example.net", "Owner Work", "example.net")
	_, err := f.Store.LinkParticipants(owner, ownerAlias)
	require.NoError(err)
	home := f.EnsureParticipant("dee@example.net", "Dee", "example.net")
	work := f.EnsureParticipant("dee@work.example.net", "Dee Work", "work.example.net")
	_, err = f.Store.LinkParticipants(home, work)
	require.NoError(err)
	ana := f.EnsureParticipant("ana@example.net", "Ana", "example.net")
	anaOther := f.EnsureParticipant("ana@other.example.net", "Ana Other", "other.example.net")

	senders := []int64{ownerAlias, home, work, ana, anaOther}
	got, err := f.Store.AuthorIdentitiesContext(ctx, senders)
	require.NoError(err)
	assert.True(got[ownerAlias].Owner, "an address linked to the owner's is the owner before anyone is saved")
	assert.Equal(got[home], got[work], "linked unsaved addresses are one sender")
	assert.Equal(min(home, work), got[work].Key)
	assert.Zero(got[home].PersonID)
	assert.False(got[home].Owner)
	assert.NotEqual(got[ana].Key, got[anaOther].Key)

	// Saving the owner keeps the alias an owner identity. Merging Ana's two
	// people makes one person that spans two clusters, so one sender.
	_, _, err = f.Store.CreatePersonFromParticipant(owner)
	require.NoError(err)
	anaPerson, _, err := f.Store.CreatePersonFromParticipant(ana)
	require.NoError(err)
	otherPerson, _, err := f.Store.CreatePersonFromParticipant(anaOther)
	require.NoError(err)
	_, err = f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: anaPerson.ID, AbsorbedID: otherPerson.ID,
		ExpectedSurvivorRevision: anaPerson.Revision, ExpectedAbsorbedRevision: otherPerson.Revision,
		IdempotencyKey: "who-knows-merge", Actor: "test",
	})
	require.NoError(err)
	got, err = f.Store.AuthorIdentitiesContext(ctx, senders)
	require.NoError(err)
	assert.True(got[ownerAlias].Owner)
	assert.Equal(got[ana], got[anaOther])
	assert.Equal(min(ana, anaOther), got[anaOther].Key)
	assert.Equal(anaPerson.ID, got[anaOther].PersonID)

	none, err := f.Store.AuthorIdentitiesContext(ctx, nil)
	require.NoError(err)
	assert.Empty(none)
}

func TestAuthorIdentitiesReportWhomTheOwnerWroteTo(t *testing.T) {
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

	got, err := f.Store.AuthorIdentitiesContext(t.Context(), []int64{ana, cc, vendor, chatty})
	require.NoError(err)
	assert.True(got[ana].Addressed, "the owner wrote to Ana's linked address")
	assert.True(got[cc].Addressed)
	assert.True(got[chatty].Addressed)
	assert.False(got[vendor].Addressed, "the vendor was never written to")
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
