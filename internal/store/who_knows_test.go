package store_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/slack"
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

// ownerOf reads the owner's participants, as a who-knows request does.
func ownerOf(f *storetest.Fixture) []int64 {
	f.T.Helper()
	owner, err := f.Store.OwnerFamilyContext(f.T.Context())
	require.NoError(f.T, err)
	return owner
}

// contactsOf stores one message from each sender and returns the contact
// each message is credited to, by sender.
func contactsOf(f *storetest.Fixture, senders ...int64) map[int64]store.TopicContact {
	f.T.Helper()
	ids := map[int64]int64{}
	var messages []int64
	for _, sender := range senders {
		ids[sender] = send(f, fmt.Sprintf("from-%d", sender), sender)
		messages = append(messages, ids[sender])
	}
	got, err := f.Store.TopicAuthorsContext(f.T.Context(), messages, ownerOf(f))
	require.NoError(f.T, err)
	out := map[int64]store.TopicContact{}
	for sender, id := range ids {
		require.True(f.T, got[id].Counts, "sender %d", sender)
		out[sender] = got[id].Contact
	}
	return out
}

func TestTopicAuthorsReadsSendersAtRequestTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()

	ana := f.EnsureParticipant("ana@example.net", "Ana Sender", "example.net")
	ben := f.EnsureParticipant("ben@example.net", "Ben Contractor", "example.net")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")

	saved := send(f, "saved", ana)
	list := send(f, "list", ben, func(m *store.Message) { m.ListID = sql.NullString{String: "<list.example.net>", Valid: true} })
	fromMe := send(f, "from-me", owner, func(m *store.Message) { m.IsFromMe = true })
	senderless := send(f, "senderless", 0)
	byEnvelope := send(f, "envelope", 0)
	require.NoError(f.Store.ReplaceMessageRecipients(byEnvelope, "from", []int64{ben, ana}, []string{"Ben", "Ana"}))
	deleted := send(f, "deleted", ana)
	require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "deleted"))

	got, err := f.Store.TopicAuthorsContext(ctx, []int64{saved, list, fromMe, senderless, byEnvelope, deleted, 999999}, ownerOf(f))
	require.NoError(err)
	assert.Len(got, 5, "deleted and missing messages are left out")

	a := got[saved]
	assert.True(a.Counts)
	assert.Equal(store.TopicContact{Key: ana, Name: "Ana Sender", Email: "ana@example.net", OneWay: true}, a.Contact)
	assert.Equal("Re: saved", a.Subject)
	assert.Equal("email", a.MessageType)
	require.NotNil(a.SentAt)
	assert.True(whoKnowsSentAt.Equal(*a.SentAt))
	assert.False(a.SenderAuthenticated, "an email From header is an assertion")
	assert.False(a.ListMail)

	assert.True(got[list].ListMail)
	assert.False(got[fromMe].Counts, "a message marked from me")
	assert.False(got[senderless].Counts, "a message with no sender")
	// With no sender_id, the lowest 'from' recipient is the sender.
	assert.Equal(ana, got[byEnvelope].Contact.Key)
}

func TestTopicAuthorsReadLegacyEmailWithNullType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// Only SQLite archives from before the NOT NULL constraint hold a NULL
	// type, and the vector index still finds them as email.
	st, sourceID, conversationID := newLegacyNullableMessageTypeStore(t)
	ana, err := st.EnsureParticipant("ana@example.net", "Ana Sender", "example.net")
	require.NoError(err)
	legacy, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: conversationID, SourceMessageID: "legacy",
		MessageType: store.MessageTypeEmail, SenderID: sql.NullInt64{Int64: ana, Valid: true},
	})
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE messages SET message_type = NULL WHERE id = ?`, legacy)
	require.NoError(err)

	got, err := st.TopicAuthorsContext(t.Context(), []int64{legacy}, nil)
	require.NoError(err)
	a := got[legacy]
	assert.True(a.Counts)
	assert.Equal(ana, a.Contact.Key)
	assert.True(store.IsEmailMessageType(a.MessageType), "a legacy row with no type is email")
}

func TestTopicAuthorsLeaveOutAutomatedSenders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	post := func(kind, email, metadata, identifierType, identifier string) int64 {
		chat, err := f.Store.GetOrCreateSource(kind, "workspace")
		require.NoError(err)
		conversation, err := f.Store.EnsureConversation(chat.ID, "channel", "Channel")
		require.NoError(err)
		sender := f.EnsureParticipant(email, email, "example.net")
		if identifier != "" {
			require.NoError(f.Store.SetParticipantIdentifier(sender, identifierType, identifier))
		}
		m := f.NewMessage().WithSourceMessageID(email).Build()
		m.SourceID, m.ConversationID, m.MessageType = chat.ID, conversation, kind
		m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(m)
		require.NoError(err)
		if metadata != "" {
			require.NoError(f.Store.SetMessageMetadata(id, sql.NullString{String: metadata, Valid: true}))
		}
		return id
	}
	automated := map[string]int64{
		"discord flag":       post("discord", "discord@example.net", `{"author_kind":"bot","automated":true}`, "", ""),
		"slack bot":          post("slack", "slack@example.net", "", "slack", "T1:bot:B1"),
		"chatwoot agent bot": post("chatwoot", "agent@example.net", "", "chatwoot", "acct/agent_bot/7"),
		"chatwoot assistant": post("chatwoot", "assistant@example.net", "", "chatwoot", "acct/captain_assistant/8"),
	}
	for _, address := range []string{
		"noreply@example.net", "no-reply+tag@example.net", "notifications@example.net",
		"alerts-jira@example.net", "newsletter@example.net", "No_Reply@example.net", "mailer_daemon@example.net",
	} {
		automated[address] = post("slack", address, "", "", "")
	}
	humans := map[string]int64{
		"slack user":     post("slack", "slackuser@example.net", "", "slack", "T1:U1"),
		"chatwoot agent": post("chatwoot", "agentuser@example.net", "", "chatwoot", "acct/agent/9"),
		"newsome":        post("slack", "newsome@example.net", "", "", ""),
		"botanist":       post("slack", "botanist@example.net", "", "", ""),
		"alert.sam":      post("slack", "alert.sam@example.net", "", "", ""),
		"bot.lee":        post("slack", "bot.lee@example.net", "", "", ""),
		"digest.ann":     post("slack", "digest.ann@example.net", "", "", ""),
		"jira":           post("slack", "jira@example.net", "", "", ""),
		"plain":          post("slack", "plain@example.net", "", "", ""),
	}

	var ids []int64
	for _, id := range automated {
		ids = append(ids, id)
	}
	for _, id := range humans {
		ids = append(ids, id)
	}
	got, err := f.Store.TopicAuthorsContext(t.Context(), ids, ownerOf(f))
	require.NoError(err)
	for name, id := range automated {
		assert.False(got[id].Counts, name)
	}
	for name, id := range humans {
		assert.True(got[id].Counts, name)
	}
	assert.True(got[humans["slack user"]].SenderAuthenticated, "the source sets a Slack sender")
}

// A Discord system message, such as a channel rename or a thread start, names
// the member who caused it but is not something they wrote.
func TestTopicAuthorsLeaveOutDiscordSystemMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	chat, err := f.Store.GetOrCreateSource("discord", "guild")
	require.NoError(err)
	channel, err := f.Store.EnsureConversation(chat.ID, "channel", "Channel")
	require.NoError(err)
	member := f.EnsureParticipant("member@example.net", "Member", "example.net")
	post := func(name string, discordType int) int64 {
		id := send(f, name, member, func(m *store.Message) {
			m.SourceID, m.ConversationID, m.MessageType = chat.ID, channel, "discord"
		})
		require.NoError(f.Store.SetMessageMetadata(id, sql.NullString{
			String: fmt.Sprintf(`{"discord_message_type":%d}`, discordType), Valid: true}))
		return id
	}
	authored := map[string]int64{"default": post("default", 0), "reply": post("reply", 19)}
	system := map[string]int64{"channel rename": post("rename", 4), "thread created": post("thread", 18)}

	var ids []int64
	for _, id := range authored {
		ids = append(ids, id)
	}
	for _, id := range system {
		ids = append(ids, id)
	}
	got, err := f.Store.TopicAuthorsContext(t.Context(), ids, ownerOf(f))
	require.NoError(err)
	for name, id := range authored {
		assert.True(got[id].Counts, name)
	}
	for name, id := range system {
		assert.False(got[id].Counts, name)
	}
}

// Slack credits only authored messages, excluding generated events and bots
// even when their payload names a user.
func TestTopicAuthorsLeaveOutSlackBotsPostingAsAUser(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	export := t.TempDir()
	write := func(name, body string) {
		path := filepath.Join(export, name)
		require.NoError(os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(os.WriteFile(path, []byte(body), 0o600))
	}
	write("users.json", `[
		{"id":"UOWNER","team_id":"T1","name":"owner","profile":{"email":"owner@example.net"}},
		{"id":"UANA","team_id":"T1","name":"ana","profile":{"email":"ana@example.net"}}]`)
	write("channels.json", `[{"id":"C1","name":"general","is_channel":true,"is_member":true,"members":["UOWNER","UANA"]}]`)
	cases := []struct {
		subtype string
		counts  bool
	}{
		{"channel_join", false},
		{"", true},
		{"thread_broadcast", true},
		{"reply_broadcast", true},
		{"file_share", true},
		{"me_message", true},
	}
	var messages []string
	for i, tc := range cases {
		messages = append(messages, fmt.Sprintf(`{"type":"message","ts":"1790000000.%06d","user":"UANA","subtype":%q,"text":"pod networking"}`, i+4, tc.subtype))
	}
	write("general/2026-10-01.json", `[
		{"type":"message","ts":"1790000000.000001","user":"UANA","text":"pod networking notes"},
		{"type":"message","ts":"1790000000.000002","user":"UANA","bot_id":"B1","text":"deploy finished"},`+strings.Join(messages, ",")+"]")
	summary, err := slack.ImportSlackdump(ctx, f.Store, export, slack.SlackdumpImportOptions{Me: "owner@example.net"})
	require.NoError(err)

	ids := map[string]int64{}
	for _, ts := range []string{"1790000000.000001", "1790000000.000002"} {
		var id int64
		require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
			`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`),
			summary.SourceID, "C1:"+ts).Scan(&id))
		ids[ts] = id
	}
	anaOwn, botAsAna := ids["1790000000.000001"], ids["1790000000.000002"]
	got, err := f.Store.TopicAuthorsContext(ctx, []int64{botAsAna, anaOwn}, ownerOf(f))
	require.NoError(err)
	assert.False(got[botAsAna].Counts, "a bot posting as Ana")
	assert.True(got[anaOwn].Counts, "Ana's own message")
	for i, tc := range cases {
		var id int64
		require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
			`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`),
			summary.SourceID, fmt.Sprintf("C1:1790000000.%06d", i+4)).Scan(&id))
		got, err := f.Store.TopicAuthorsContext(ctx, []int64{id}, ownerOf(f))
		require.NoError(err)
		assert.Equal(tc.counts, got[id].Counts, "subtype %q", tc.subtype)
	}
}

func TestTopicAuthorsLeaveOutAddressesLinkedToTheOwner(t *testing.T) {
	for _, accountIdentity := range []bool{true, false} {
		t.Run(fmt.Sprintf("account_identity=%t", accountIdentity), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
			if accountIdentity {
				require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
			} else {
				send(f, "outbound", owner, func(m *store.Message) { m.IsFromMe = true })
				require.Empty(ownerOf(f), "no account identity names the owner")
			}
			alias := f.EnsureParticipant("owner.work@example.net", "Owner Work", "example.net")
			_, err := f.Store.LinkParticipants(owner, alias)
			require.NoError(err)
			fromAlias := send(f, "from-alias", alias)

			got, err := f.Store.TopicAuthorsContext(t.Context(), []int64{fromAlias}, ownerOf(f))
			require.NoError(err)
			assert.False(got[fromAlias].Counts, "an address linked to the owner's is the owner before anyone is saved")

			_, _, err = f.Store.CreatePersonFromParticipant(owner)
			require.NoError(err)
			got, err = f.Store.TopicAuthorsContext(t.Context(), []int64{fromAlias}, ownerOf(f))
			require.NoError(err)
			assert.False(got[fromAlias].Counts, "and after the owner is saved")
		})
	}
}

func TestTopicAuthorsLeaveOutOwnerChatIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	// A chat account identity names the owner by identifier.
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "T1:UOWNER", "manual"))
	chatOwner, err := f.Store.EnsureParticipantByIdentifier("slack", "T1:UOWNER", "Owner")
	require.NoError(err)
	chatOther, err := f.Store.EnsureParticipantByIdentifier("slack", "T1:UANA", "Ana")
	require.NoError(err)
	fromChatOwner, fromChatOther := send(f, "from-chat-owner", chatOwner), send(f, "from-chat-other", chatOther)
	got, err := f.Store.TopicAuthorsContext(t.Context(), []int64{fromChatOwner, fromChatOther}, ownerOf(f))
	require.NoError(err)
	assert.False(got[fromChatOwner].Counts, "the owner's chat identity")
	assert.True(got[fromChatOther].Counts)
}

func TestTopicContactsGroupLinkedAndBoundSenders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	home := f.EnsureParticipant("dee@example.net", "Dee", "example.net")
	work := f.EnsureParticipant("dee@work.example.net", "Dee Work", "work.example.net")
	_, err := f.Store.LinkParticipants(home, work)
	require.NoError(err)
	ana := f.EnsureParticipant("ana@example.net", "Ana", "example.net")
	anaOther := f.EnsureParticipant("ana@other.example.net", "Ana Other", "other.example.net")

	got := contactsOf(f, home, work, ana, anaOther)
	assert.Equal(store.TopicContact{Key: home, Name: "Dee", Email: "dee@example.net", OneWay: true}, got[work],
		"linked unsaved addresses are one sender")
	assert.Equal(got[home], got[work])
	assert.NotEqual(got[ana].Key, got[anaOther].Key)

	// Merging Ana's two people makes one person that spans two clusters.
	anaPerson, _, err := f.Store.CreatePersonFromParticipant(ana)
	require.NoError(err)
	otherPerson, _, err := f.Store.CreatePersonFromParticipant(anaOther)
	require.NoError(err)
	_, err = f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: anaPerson.ID, AbsorbedID: otherPerson.ID,
		ExpectedSurvivorRevision: anaPerson.Revision, ExpectedAbsorbedRevision: otherPerson.Revision,
		IdempotencyKey: "who-knows-merge", Actor: "test",
	})
	require.NoError(err)
	got = contactsOf(f, ana, anaOther)
	assert.Equal(store.TopicContact{Key: ana, PersonID: anaPerson.ID, Name: "Ana", Email: "ana@example.net", OneWay: true}, got[anaOther])
	assert.Equal(got[ana], got[anaOther])
}

func TestTopicContactsUseNamedAliasWhenKeyHasNoName(t *testing.T) {
	f := storetest.New(t)
	key := f.EnsureParticipant("unnamed@example.net", "", "example.net")
	alias := f.EnsureParticipant("alias@example.net", "Alias", "example.net")
	_, err := f.Store.LinkParticipants(key, alias)
	require.NoError(t, err)
	got := contactsOf(f, key, alias)
	want := store.TopicContact{Key: key, Name: "Alias", Email: "alias@example.net", OneWay: true}
	assert.Equal(t, want, got[key])
	assert.Equal(t, want, got[alias])
}

func TestTopicContactsReportWhomTheOwnerWroteTo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	ana := f.EnsureParticipant("ana@example.net", "Ana", "example.net")
	anaOther := f.EnsureParticipant("ana.work@example.net", "Ana Work", "example.net")
	cc := f.EnsureParticipant("cc@example.net", "Copied", "example.net")
	vendor := f.EnsureParticipant("vendor@example.net", "Vendor", "example.net")
	_, err := f.Store.LinkParticipants(ana, anaOther)
	require.NoError(err)

	mail := send(f, "to-ana", owner)
	require.NoError(f.Store.ReplaceMessageRecipients(mail, "to", []int64{anaOther}, []string{"Ana Work"}))
	require.NoError(f.Store.ReplaceMessageRecipients(mail, "cc", []int64{cc}, []string{"Copied"}))
	// Mail the vendor sent to the owner does not count.
	inbound := send(f, "from-vendor", vendor)
	require.NoError(f.Store.ReplaceMessageRecipients(inbound, "to", []int64{owner}, []string{"Owner"}))

	got := contactsOf(f, ana, cc, vendor)
	assert.False(got[ana].OneWay, "the owner wrote to Ana's linked address")
	assert.False(got[cc].OneWay)
	assert.True(got[vendor].OneWay, "the vendor was never written to")
}

func TestTopicContactsRecognizeOwnerReplies(t *testing.T) {
	for _, accountIdentity := range []bool{true, false} {
		for _, bound := range []bool{false, true} {
			t.Run(fmt.Sprintf("account_identity=%t/bound=%t", accountIdentity, bound), func(t *testing.T) {
				require := require.New(t)
				f := storetest.New(t)
				owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
				alias := f.EnsureParticipant("owner.work@example.net", "Owner Work", "example.net")
				ana := f.EnsureParticipant("ana@example.net", "Ana", "example.net")
				if accountIdentity {
					require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
				} else {
					send(f, "outbound", owner, func(m *store.Message) { m.IsFromMe = true })
					require.Empty(ownerOf(f), "no account identity names the owner")
				}
				if bound {
					person, _, err := f.Store.CreatePersonFromParticipant(owner)
					require.NoError(err)
					other, _, err := f.Store.CreatePersonFromParticipant(alias)
					require.NoError(err)
					_, err = f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
						SurvivorID: person.ID, AbsorbedID: other.ID,
						ExpectedSurvivorRevision: person.Revision, ExpectedAbsorbedRevision: other.Revision,
						IdempotencyKey: "owner-reply-merge", Actor: "test",
					})
					require.NoError(err)
				} else {
					_, err := f.Store.LinkParticipants(owner, alias)
					require.NoError(err)
				}
				reply := send(f, "reply", alias)
				require.NoError(f.Store.ReplaceMessageRecipients(reply, "to", []int64{ana}, []string{"Ana"}))
				fromMe, err := f.Store.GetMessageIsFromMe(reply)
				require.NoError(err)
				require.False(fromMe)

				got := contactsOf(f, ana)
				require.Contains(got, ana)
				assert.False(t, got[ana].OneWay, "the owner's alias replied to Ana")
			})
		}
	}
}

func TestAskMeAboutValuesReadsUserValuesForPeopleWhoCount(t *testing.T) {
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
	ownerAlias := f.EnsureParticipant("owner.work@example.net", "Owner Work", "example.net")
	_, err := f.Store.LinkParticipants(f.EnsureParticipant("owner@example.net", "Owner", "example.net"), ownerAlias)
	require.NoError(err)
	ownerPerson, _, err := f.Store.CreatePersonFromParticipant(ownerAlias)
	require.NoError(err)
	_, noreplyPerson := newPerson("noreply@example.net")
	webhook, webhookPerson := newPerson("builds@example.net")
	require.NoError(f.Store.SetParticipantIdentifier(webhook, "discord_webhook_id", "W1"))
	setValue(asked, "Straße networking", store.ProvenanceUser)
	setValue(inferred, "kubernetes robotics", store.ProvenanceEnrichment)
	setValue(superseded, "old topic", store.ProvenanceUser)
	setValue(superseded, "new topic", store.ProvenanceUser)
	setValue(ownerPerson.ID, "my own topic", store.ProvenanceUser)
	setValue(noreplyPerson, "receipts", store.ProvenanceUser)
	setValue(webhookPerson, "release builds", store.ProvenanceUser)

	got, truncated, err := f.Store.AskMeAboutValuesContext(ctx, ownerOf(f))
	require.NoError(err)
	assert.False(truncated)
	assert.Equal([]store.PersonAskMeAbout{
		{PersonID: asked, PersonName: "asked@example.net", Value: "Straße networking"},
		{PersonID: superseded, PersonName: "superseded@example.net", Value: "new topic"},
	}, got, "inferred and superseded values, the owner's own person, and automated senders are left out")

	// Copy the first value past the cap: the read returns the cap and says more exist.
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO person_attribute_values (person_id, definition_id, ordinal, value_text, source)
		SELECT v.person_id, v.definition_id, v.ordinal + n.i, v.value_text, v.source
		  FROM person_attribute_values v, n WHERE v.person_id = ? AND v.superseded_at IS NULL`),
		store.DeclaredValueLimit, asked)
	require.NoError(err)
	got, truncated, err = f.Store.AskMeAboutValuesContext(ctx, ownerOf(f))
	require.NoError(err)
	assert.True(truncated)
	assert.Len(got, store.DeclaredValueLimit)
}

// An iMessage-only archive has no account identities, so only the owner's
// is_from_me messages show which participant is the owner.
func TestAskMeAboutValuesLeaveOutOwnerWithoutAccountIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	newPerson := func(handle string, isFromMe bool) int64 {
		participant := f.EnsureParticipant(handle, handle, "example.net")
		send(f, handle, participant, func(m *store.Message) { m.IsFromMe = isFromMe })
		person, _, err := f.Store.CreatePersonFromParticipant(participant)
		require.NoError(err)
		topic := "pod networking"
		_, err = f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID:       person.ID,
			DefinitionSlug: store.AttributeSlugAskMeAbout,
			Ordinal:        new(int64),
			Value:          store.AttributeValue{Type: store.AttributeValueText, Text: &topic},
			Source:         store.ProvenanceUser,
		})
		require.NoError(err)
		return person.ID
	}
	newPerson("owner@example.net", true)
	ana := newPerson("ana@example.net", false)

	owner := ownerOf(f)
	require.Empty(owner, "no account identity names the owner")
	got, _, err := f.Store.AskMeAboutValuesContext(ctx, owner)
	require.NoError(err)
	assert.Equal([]store.PersonAskMeAbout{{PersonID: ana, PersonName: "ana@example.net", Value: "pod networking"}}, got,
		"the person who sent the owner's messages is left out")
}
