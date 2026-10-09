package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func seedDiscoveryChat(tb testing.TB, st *store.Store, sourceID int64, name, kind string, sent time.Time) (int64, int64) {
	tb.Helper()
	participant, err := st.EnsureParticipant(name+"@example.test", name, "example.test")
	require.NoError(tb, err)
	id, err := st.EnsureConversationWithType(sourceID, name, kind, "")
	require.NoError(tb, err)
	message, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, SourceMessageID: name, ConversationID: id,
		MessageType: "beeper", SenderID: sql.NullInt64{Int64: participant, Valid: true},
		SentAt: sql.NullTime{Time: sent, Valid: true},
	})
	require.NoError(tb, err)
	return id, message
}

func BenchmarkChatDiscoveryRepeatedRecipientAlias(b *testing.B) {
	for _, fixture := range []struct{ aliases, unrelated int }{{1, 0}, {1000, 0}, {1, 1000}, {1000, 1000}} {
		b.Run(strconv.Itoa(fixture.aliases)+"/mail-"+strconv.Itoa(fixture.unrelated), func(b *testing.B) {
			st, err := store.OpenForTest(filepath.Join(b.TempDir(), "discovery.db"))
			require.NoError(b, err)
			b.Cleanup(func() { _ = st.Close() })
			require.NoError(b, st.InitSchema())
			source, err := st.GetOrCreateSource("beeper", "repeated-alias-account")
			require.NoError(b, err)
			member, err := st.EnsureParticipant("member@example.test", "Local Alias", "example.test")
			require.NoError(b, err)
			chat, _ := seedDiscoveryChat(b, st, source.ID, "Room", "group_chat", time.Unix(100, 0))
			for i := range fixture.aliases {
				message, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: strconv.Itoa(i), ConversationID: chat, MessageType: "beeper"})
				require.NoError(b, err)
				require.NoError(b, st.ReplaceMessageRecipients(message, "to", []int64{member}, []string{"Remote Alias"}))
			}
			mail, err := st.GetOrCreateSource("imap", "mail@example.test")
			require.NoError(b, err)
			thread, err := st.EnsureConversationWithType(mail.ID, "mail-thread", "email_thread", "")
			require.NoError(b, err)
			for i := range fixture.unrelated {
				_, err := st.UpsertMessage(&store.Message{SourceID: mail.ID, SourceMessageID: strconv.Itoa(i), ConversationID: thread,
					MessageType: "email", SenderID: sql.NullInt64{Int64: member, Valid: true}, IsFromMe: true})
				require.NoError(b, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				page, err := st.SearchChatsContext(b.Context(), store.ChatDiscoveryQuery{Query: "Remote", SourceID: source.ID})
				require.NoError(b, err)
				require.Len(b, page.Results, 1)
				require.Equal(b, []string{"Remote Alias"}, page.Results[0].MatchedNames)
			}
		})
	}
}

func TestChatDiscoveryFindsDifferentNamesAcrossNetworks(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSource("beeper", "social-account")
	requirements.NoError(err)
	second, err := st.GetOrCreateSource("beeper", "native-account")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(first.ID, "Beeper Instagram"))
	requirements.NoError(st.UpdateSourceDisplayName(second.ID, "Beeper iMessage"))
	exact, _ := seedDiscoveryChat(t, st, first.ID, "Jordan Lee Chen", "direct_chat", time.Unix(100, 0))
	partial, _ := seedDiscoveryChat(t, st, second.ID, "Lee Chen", "direct_chat", time.Unix(200, 0))
	seedDiscoveryChat(t, st, second.ID, "Unrelated", "direct_chat", time.Unix(300, 0))
	gmail, err := st.GetOrCreateSource("gmail", "chat@example.test")
	requirements.NoError(err)
	googleChat, err := st.EnsureConversationWithType(gmail.ID, "google-chat", "chat", "Planning Room")
	requirements.NoError(err)
	peer, err := st.EnsureParticipant("google-peer@example.test", "Lee Chen", "example.test")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: gmail.ID, SourceMessageID: "google-message", ConversationID: googleChat,
		MessageType: store.MessageTypeGoogleChat, SenderID: sql.NullInt64{Int64: peer, Valid: true}, SentAt: sql.NullTime{Time: time.Unix(150, 0), Valid: true}})
	requirements.NoError(err)
	page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "JORDAN Lee Chen"})
	requirements.NoError(err)
	requirements.Len(page.Results, 3)
	assertions.Equal(exact, page.Results[0].ConversationID)
	assertions.Equal(partial, page.Results[1].ConversationID)
	assertions.Equal(googleChat, page.Results[2].ConversationID)
	assertions.Equal("gmail", page.Results[2].Network)
	assertions.Equal([]string{"Lee Chen"}, page.Results[2].MatchedNames)
	assertions.Equal("exact", page.Results[0].MatchKind)
	assertions.Equal("partial", page.Results[1].MatchKind)
	assertions.Equal([]string{"lee", "chen"}, page.Results[1].MatchedTokens)
	assertions.Equal("Lee Chen", page.Results[1].SourceConversationID)
	assertions.Equal("iMessage", page.Results[1].Network)
	assertions.False(page.HasMore)
	for _, result := range page.Results {
		assertions.Positive(result.MessageID)
		var visible int64
		requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM messages WHERE id = ? AND conversation_id = ? AND deleted_at IS NULL"), result.MessageID, result.ConversationID).Scan(&visible))
		assertions.Equal(result.MessageID, visible)
	}
}

func TestChatDiscoveryRanksBestIndividualName(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "individual-name-account")
	requirements.NoError(err)
	direct, _ := seedDiscoveryChat(t, st, source.ID, "Lee Chen", "direct_chat", time.Unix(100, 0))
	group, _ := seedDiscoveryChat(t, st, source.ID, "Unrelated Room", "group_chat", time.Unix(200, 0))
	refs := []store.ConversationParticipantRef{}
	for _, name := range []string{"Jordan Park", "Amy Lee", "Sam Chen"} {
		participant, err := st.EnsureParticipant(strings.ReplaceAll(name, " ", "-")+"@example.test", name, "example.test")
		requirements.NoError(err)
		refs = append(refs, store.ConversationParticipantRef{ParticipantID: participant})
	}
	requirements.NoError(st.ReplaceConversationParticipants(group, refs))
	for _, limit := range []int{1, 20} {
		page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Jordan Lee Chen", Limit: limit})
		requirements.NoError(err)
		requirements.Len(page.Results, min(limit, 2))
		assertions.Equal(direct, page.Results[0].ConversationID)
		assertions.Equal(limit == 1, page.HasMore)
		if limit > 1 {
			assertions.Equal(group, page.Results[1].ConversationID)
			assertions.Equal([]string{"jordan", "lee", "chen"}, page.Results[1].MatchedTokens)
		}
	}
}

func TestChatDiscoveryUnicodeLimitsAndArchiveScope(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("apple_messages", "archive@example.test")
	requirements.NoError(err)
	first, firstHidden := seedDiscoveryChat(t, st, source.ID, "Émile Example", "direct_chat", time.Unix(100, 0))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), firstHidden)
	requirements.NoError(err)
	peer, err := st.EnsureParticipant("Émile Example@example.test", "Émile Example", "example.test")
	requirements.NoError(err)
	firstVisible, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "first-live", ConversationID: first,
		MessageType: "beeper", SenderID: sql.NullInt64{Int64: peer, Valid: true}, SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}})
	requirements.NoError(err)
	second, _ := seedDiscoveryChat(t, st, source.ID, "Émile Other", "group_chat", time.Unix(200, 0))
	seedDiscoveryChat(t, st, source.ID, "Émile Email", "email_thread", time.Unix(300, 0))
	_, hidden := seedDiscoveryChat(t, st, source.ID, "Émile Hidden", "channel", time.Unix(400, 0))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), hidden)
	requirements.NoError(err)
	// Source tombstones preserve archived content; dedup tombstones hide it.
	requirements.NoError(st.MarkMessageDeleted(source.ID, "first-live"))
	_, err = st.EnsureConversationWithType(source.ID, "empty", "direct_chat", "Émile Empty")
	requirements.NoError(err)
	page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "E\u0301MILE émile", Limit: 1})
	requirements.NoError(err)
	requirements.Len(page.Results, 1)
	assertions.Equal(second, page.Results[0].ConversationID)
	assertions.True(page.HasMore)
	page, err = st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "émile", SourceID: source.ID})
	requirements.NoError(err)
	requirements.Len(page.Results, 2)
	assertions.Equal(first, page.Results[1].ConversationID)
	assertions.Equal(firstVisible, page.Results[1].MessageID)
	assertions.NotEqual(firstHidden, page.Results[1].MessageID)
	assertions.Equal("apple_messages", page.Results[1].Network)
	page, err = st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "émile", SourceID: source.ID + 100})
	requirements.NoError(err)
	assertions.Empty(page.Results)
}

func TestChatDiscoveryRejectsInvalidQueryAndCancellation(t *testing.T) {
	st := testutil.NewTestStore(t)
	for _, q := range []store.ChatDiscoveryQuery{
		{Query: " "}, {Query: "%_"}, {Query: strings.Repeat("a", 257)},
		{Query: "a b c d e f g h i j k l m n o p q"}, {Query: "name", Limit: -1},
		{Query: "name", Limit: 101}, {Query: "name", SourceID: -1},
	} {
		_, err := st.SearchChatsContext(t.Context(), q)
		require.ErrorIs(t, err, store.ErrInvalidChatDiscoveryQuery)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := st.SearchChatsContext(ctx, store.ChatDiscoveryQuery{Query: "name"})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestChatDiscoveryUsesMembersRecipientAliasesAndCuratedNames(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "alias-account")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(source.ID, "Beeper alias-account"))
	member, err := st.EnsureParticipant("member@example.test", "Local Alias", "example.test")
	requirements.NoError(err)
	chat, message := seedDiscoveryChat(t, st, source.ID, "Room Sender", "group_chat", time.Unix(100, 0))
	_, mention := seedDiscoveryChat(t, st, source.ID, "Mention Sender", "channel", time.Unix(200, 0))
	requirements.NoError(st.ReplaceMessageRecipients(message, "to", []int64{member}, []string{"Remote Alias"}))
	for i, alias := range []string{"Remote Alias", "Remote Alias", "Changed Alias"} {
		id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "alias-" + strconv.Itoa(i), ConversationID: chat, MessageType: "beeper"})
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{member}, []string{alias}))
	}
	requirements.NoError(st.ReplaceMessageRecipients(mention, "mention", []int64{member}, []string{"Remote Alias"}))
	label := "Curated Friend"
	person, _, err := st.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), member, &label)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO person_names
		(person_id, name_kind, formatted, original_value, source) VALUES (?, 'nickname', ?, ?, 'user')`),
		person.ID, "Alternate Friend", "Alternate Friend")
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO person_names
		(person_id, name_kind, original_value, source, active_until) VALUES (?, 'nickname', 'Retired Friend', 'user', CURRENT_TIMESTAMP)`), person.ID)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO participant_identifiers
		(participant_id, identifier_type, identifier_value) VALUES (?, 'username', 'HandleToken')`), member)
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("beeper", "other-account")
	requirements.NoError(err)
	for _, fixture := range []struct {
		name    string
		source  int64
		retired bool
	}{{"CurrentAlias", source.ID, false}, {"WrongSourceAlias", other.ID, false}, {"RetiredAlias", source.ID, true}} {
		envelope := store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}
		if fixture.retired {
			retired := time.Unix(100, 0)
			envelope.ActiveUntil = &retired
		}
		_, err = st.RecordContactObservationContext(t.Context(), member, store.ParticipantContactObservationInput{
			SourceID: &fixture.source, AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"), OriginalValue: fixture.name, Envelope: envelope,
		})
		requirements.NoError(err)
	}
	_, err = st.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
		NameKind: store.PersonNameStructured, GivenName: new("Jordan"), FamilyName: new("Example"), AdditionalNames: new("Lee"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	for _, query := range []string{"Remote", "Changed", "Local", "Curated", "Alternate", "HandleToken", "CurrentAlias", "Jordan Lee Example"} {
		page, searchErr := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: query})
		requirements.NoError(searchErr)
		if query == "Jordan Lee Example" {
			requirements.Len(page.Results, 2, query)
		} else {
			requirements.Len(page.Results, 1, query)
		}
		assertions.Equal(chat, page.Results[0].ConversationID, query)
		assertions.Equal("unknown", page.Results[0].Network)
		if query == "Jordan Lee Example" {
			assertions.Equal([]string{"jordan", "lee", "example"}, page.Results[0].MatchedTokens)
			assertions.Equal("exact", page.Results[0].MatchKind)
		}
		if query == "Remote" {
			assertions.Equal([]string{"Remote Alias"}, page.Results[0].MatchedNames)
		}
	}
	for _, name := range []string{"Retired", "WrongSourceAlias", "RetiredAlias"} {
		page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: name})
		requirements.NoError(err)
		assertions.Empty(page.Results, name)
	}
	memberChat, _ := seedDiscoveryChat(t, st, source.ID, "Other Sender", "direct_chat", time.Unix(300, 0))
	requirements.NoError(st.ReplaceConversationParticipants(memberChat, []store.ConversationParticipantRef{{ParticipantID: member}}))
	page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Curated"})
	requirements.NoError(err)
	requirements.Len(page.Results, 2)
	assertions.Equal(memberChat, page.Results[0].ConversationID)
}

func TestChatDiscoveryFiltersSourceOwners(t *testing.T) {
	for _, tc := range []struct {
		name, membership, title                              string
		account, scopedUser                                  string
		confirmed, provider, removed, otherSource, ownerOnly bool
	}{
		{name: "confirmed roster", membership: "roster", confirmed: true},
		{name: "confirmed sender", membership: "sender", confirmed: true},
		{name: "confirmed recipient", membership: "recipient", confirmed: true},
		{name: "provider sender", membership: "sender", provider: true},
		{name: "provider recipient", membership: "recipient", provider: true},
		{name: "confirmed other source", membership: "roster", confirmed: true, otherSource: true},
		{name: "provider other source", membership: "roster", provider: true, otherSource: true},
		{name: "unknown identity", membership: "roster"},
		{name: "removed identity", membership: "roster", confirmed: true, removed: true},
		{name: "removed identity with provider", membership: "roster", confirmed: true, provider: true, removed: true},
		{name: "matching title", membership: "roster", confirmed: true, title: "OwnerName"},
		{name: "self chat title", membership: "sender", confirmed: true, ownerOnly: true, title: "OwnerName"},
		{name: "owner only without title", membership: "sender", confirmed: true, ownerOnly: true},
		{name: "scoped incoming only", membership: "roster", confirmed: true, account: "account-a", scopedUser: "user-a"},
		{name: "scoped other account", membership: "roster", confirmed: true, otherSource: true, account: "account-a", scopedUser: "user-a"},
		{name: "scoped removed identity", membership: "roster", confirmed: true, removed: true, account: "account-a", scopedUser: "user-a"},
		{name: "scoped matching title", membership: "roster", confirmed: true, title: "OwnerName", account: "account-a", scopedUser: "user-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			account := tc.account
			if account == "" {
				account = "owner-account"
			}
			source, err := st.GetOrCreateSource("beeper", account)
			requirements.NoError(err)
			chatSource := source.ID
			if tc.otherSource {
				other, err := st.GetOrCreateSource("beeper", "other-account")
				requirements.NoError(err)
				chatSource = other.ID
			}
			identity := "owneraddress@example.test"
			var owner int64
			if tc.scopedUser != "" {
				identity = tc.scopedUser
				if tc.otherSource {
					firstOwner, err := st.EnsureParticipantByIdentifier("beeper", beeperidentity.Fallback(account, identity), "OwnerName")
					requirements.NoError(err)
					firstChat, _ := seedDiscoveryChat(t, st, source.ID, "First Sender", "direct_chat", time.Unix(100, 0))
					requirements.NoError(st.ReplaceConversationParticipants(firstChat, []store.ConversationParticipantRef{{ParticipantID: firstOwner}}))
					account = "other-account"
				}
				owner, err = st.EnsureParticipantByIdentifier("beeper", beeperidentity.Fallback(account, identity), "OwnerName")
			} else {
				owner, err = st.EnsureParticipant(identity, "OwnerName", "example.test")
			}
			requirements.NoError(err)
			requirements.NoError(st.SetParticipantIdentifier(owner, "username", "OwnerHandle"))
			if tc.scopedUser == "" {
				_, err = st.RecordContactObservationContext(t.Context(), owner, store.ParticipantContactObservationInput{
					AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"), OriginalValue: "OwnerObservation",
					Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
				})
				requirements.NoError(err)
			}
			person, _, err := st.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), owner, new("OwnerCurated"))
			requirements.NoError(err)
			_, err = st.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
				NameKind: store.PersonNameStructured, Formatted: new("OwnerFormatted"), GivenName: new("OwnerGiven"),
				OriginalValue: "OwnerOriginal", SortAs: new("OwnerSort"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			})
			requirements.NoError(err)
			if tc.confirmed {
				requirements.NoError(st.AddAccountIdentity(source.ID, identity, "test"))
			}
			if tc.provider {
				proofChat, err := st.EnsureConversationWithType(source.ID, "proof", "direct_chat", "")
				requirements.NoError(err)
				proof, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "proof", ConversationID: proofChat,
					MessageType: "beeper", SenderID: sql.NullInt64{Int64: owner, Valid: true}, IsFromMe: true})
				requirements.NoError(err)
				_, err = st.DB().Exec(st.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), proof)
				requirements.NoError(err)
			}
			if tc.removed {
				_, err = st.RemoveAccountIdentity(source.ID, identity)
				requirements.NoError(err)
			}
			chat, err := st.EnsureConversationWithType(chatSource, "target", "direct_chat", tc.title)
			requirements.NoError(err)
			peer, err := st.EnsureParticipant("peer@example.test", "PeerName", "example.test")
			requirements.NoError(err)
			sender := peer
			if tc.membership == "sender" {
				sender = owner
			}
			message, err := st.UpsertMessage(&store.Message{SourceID: chatSource, SourceMessageID: "target", ConversationID: chat,
				MessageType: "beeper", SenderID: sql.NullInt64{Int64: sender, Valid: true}, SentAt: sql.NullTime{Time: time.Unix(200, 0), Valid: true}})
			requirements.NoError(err)
			if tc.membership == "roster" {
				requirements.NoError(st.ReplaceConversationParticipants(chat, []store.ConversationParticipantRef{{ParticipantID: owner}}))
			}
			if tc.membership == "recipient" {
				requirements.NoError(st.ReplaceMessageRecipients(message, "to", []int64{owner}, []string{"OwnerRecipient"}))
			} else if !tc.ownerOnly {
				requirements.NoError(st.ReplaceMessageRecipients(message, "to", []int64{peer}, []string{"PeerAlias"}))
			}
			other, _ := seedDiscoveryChat(t, st, chatSource, "PeerOther", "direct_chat", time.Unix(100, 0))
			excluded := !tc.otherSource && (tc.provider || tc.confirmed && !tc.removed)
			queries := []string{"OwnerName", "OwnerHandle", "OwnerCurated", "OwnerFormatted", "OwnerGiven", "OwnerOriginal", "OwnerSort"}
			if tc.scopedUser == "" {
				queries = append(queries, "owneraddress", "OwnerObservation")
			}
			if tc.membership == "recipient" {
				queries = append(queries, "OwnerRecipient")
			}
			for _, query := range queries {
				page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: query})
				requirements.NoError(err)
				if excluded && query != tc.title {
					assertions.Empty(page.Results, query)
				} else {
					requirements.Len(page.Results, 1, query)
					assertions.Equal(chat, page.Results[0].ConversationID, query)
					assertions.Contains(page.Results[0].MatchedTokens, strings.ToLower(query))
					if excluded {
						assertions.Equal([]string{tc.title}, page.Results[0].MatchedNames)
					}
				}
			}
			page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "PeerName PeerOther"})
			requirements.NoError(err)
			if tc.ownerOnly {
				requirements.Len(page.Results, 1)
				assertions.Equal(other, page.Results[0].ConversationID)
			} else {
				requirements.Len(page.Results, 2)
				assertions.Equal(chat, page.Results[0].ConversationID)
				assertions.Equal(other, page.Results[1].ConversationID)
				assertions.Equal([]string{"PeerName"}, page.Results[0].MatchedNames)
			}
		})
	}
}

func TestBeeperFallbackIDSQLMatchesProducer(t *testing.T) {
	st := testutil.NewTestStore(t)
	driver := "sqlite3"
	if st.IsPostgreSQL() {
		driver = "pgx"
	}
	var spaces strings.Builder
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsSpace(r) {
			spaces.WriteRune(r)
		}
	}
	whitespace := spaces.String()
	for _, tc := range []struct{ account, user string }{
		{"account-a", "user-a"}, {"account:a", "user:a"}, {"账户:é", "用户:é"},
		{"", " user "}, {" account ", ""}, {"", ""}, {whitespace, whitespace},
		{whitespace + "a" + whitespace, whitespace + "u" + whitespace},
		{"a" + whitespace + "b", "u" + whitespace + "v"}, {"\u200b", "\u200b"},
	} {
		t.Run(strconv.Quote(tc.account)+"/"+strconv.Quote(tc.user), func(t *testing.T) {
			var got string
			statement := `WITH input AS (SELECT CAST(? AS TEXT) AS account, CAST(? AS TEXT) AS user_id) SELECT ` +
				beeperidentity.FallbackSQL(driver, "account", "user_id") + ` FROM input`
			require.NoError(t, st.DB().QueryRow(st.Rebind(statement), tc.account, tc.user).Scan(&got))
			assert.Equal(t, beeperidentity.Fallback(tc.account, tc.user), got)
		})
	}
}

func TestChatDiscoveryLiteralTokensTitlesAndBoundedEvidence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "evidence-account")
	requirements.NoError(err)
	chat, _ := seedDiscoveryChat(t, st, source.ID, "Unrelated Sender", "group_chat", time.Unix(100, 0))
	_, err = st.EnsureConversationWithType(source.ID, "Unrelated Sender", "group_chat", "Planning Room")
	requirements.NoError(err)
	page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "%planning_"})
	requirements.NoError(err)
	requirements.Len(page.Results, 1)
	assertions.Equal(chat, page.Results[0].ConversationID)
	assertions.Equal([]string{"Planning Room"}, page.Results[0].MatchedNames)
	assertions.False(page.Results[0].EvidenceTruncated)
	refs := []store.ConversationParticipantRef{}
	for _, name := range []string{"Member Alpha", "Member Beta", "Member Gamma", "Member Delta", "Member Epsilon", "Member Zeta", "Member Eta", "Member Theta", "Member Iota", "Member Kappa"} {
		id, ensureErr := st.EnsureParticipant(strings.ReplaceAll(name, " ", "-")+"@example.test", name, "example.test")
		requirements.NoError(ensureErr)
		refs = append(refs, store.ConversationParticipantRef{ParticipantID: id})
	}
	requirements.NoError(st.ReplaceConversationParticipants(chat, refs))
	page, err = st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Member Member"})
	requirements.NoError(err)
	requirements.Len(page.Results, 1)
	assertions.Equal([]string{"member"}, page.Results[0].MatchedTokens)
	assertions.Len(page.Results[0].MatchedNames, 8)
	assertions.True(page.Results[0].EvidenceTruncated)
	assertions.Equal("Member Alpha", page.Results[0].MatchedNames[0])
	refs = nil
	for _, name := range []string{"Aa0 Lee", "Aa1 Lee", "Aa2 Lee", "Aa3 Lee", "Aa4 Lee", "Aa5 Lee", "Aa6 Lee", "Aa7 Lee", "Aa8 Lee", "Zz Lee Chen", "Lee Chen"} {
		id, err := st.EnsureParticipant(strings.ReplaceAll(name, " ", "-")+"@example.test", name, "example.test")
		requirements.NoError(err)
		refs = append(refs, store.ConversationParticipantRef{ParticipantID: id})
	}
	requirements.NoError(st.ReplaceConversationParticipants(chat, refs))
	other, _ := seedDiscoveryChat(t, st, source.ID, "Lee Other", "direct_chat", time.Unix(200, 0))
	for _, query := range []string{"Lee Chen", "Jordan Lee Chen"} {
		page, err = st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: query})
		requirements.NoError(err)
		requirements.Len(page.Results, 2)
		assertions.Equal(chat, page.Results[0].ConversationID)
		assertions.Equal(other, page.Results[1].ConversationID)
		assertions.Len(page.Results[0].MatchedNames, 8)
		assertions.Contains(page.Results[0].MatchedNames, "Lee Chen")
		assertions.True(page.Results[0].EvidenceTruncated)
		assertions.Equal(query == "Lee Chen", page.Results[0].MatchKind == "exact")
		assertions.True(slices.IsSorted(page.Results[0].MatchedNames))
		assertions.Equal(page.Results[0].MatchedNames, slices.Compact(slices.Clone(page.Results[0].MatchedNames)))
	}
}

func TestChatDiscoveryExactMatchPreservesTokenOrder(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "token-order-account")
	require.NoError(t, err)
	older, _ := seedDiscoveryChat(t, st, source.ID, "Lee Chen", "direct_chat", time.Unix(100, 0))
	newer, _ := seedDiscoveryChat(t, st, source.ID, "Chen Lee", "direct_chat", time.Unix(200, 0))
	exact, _ := seedDiscoveryChat(t, st, source.ID, "Bora Bora", "direct_chat", time.Unix(100, 0))
	_, _ = seedDiscoveryChat(t, st, source.ID, "Bora", "direct_chat", time.Unix(300, 0))
	for _, fixture := range []struct {
		query string
		first int64
		kind  string
		limit int
	}{{"Lee Chen", older, "exact", 0}, {"Chen Lee", newer, "exact", 0}, {"Chen", newer, "partial", 0}, {"Bora Bora", exact, "exact", 1}} {
		t.Run(fixture.query, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: fixture.query, Limit: fixture.limit})
			requirements.NoError(err)
			if fixture.limit == 1 {
				requirements.Len(page.Results, 1)
				assertions.True(page.HasMore)
			} else {
				requirements.Len(page.Results, 2, "reversed names remain discoverable")
				assertions.Equal("partial", page.Results[1].MatchKind)
			}
			assertions.Equal(fixture.first, page.Results[0].ConversationID)
			assertions.Equal(fixture.kind, page.Results[0].MatchKind)
		})
	}
}

func TestChatDiscoveryRanksLiveActivityAndStableTies(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "ranking-account")
	requirements.NoError(err)
	first, _ := seedDiscoveryChat(t, st, source.ID, "Taylor One", "direct_chat", time.Unix(100, 0))
	second, _ := seedDiscoveryChat(t, st, source.ID, "Taylor Two", "direct_chat", time.Unix(100, 0))
	hidden, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "hidden-newest", ConversationID: first, MessageType: "beeper", SentAt: sql.NullTime{Time: time.Unix(500, 0), Valid: true}})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), hidden)
	requirements.NoError(err)
	recent, _ := seedDiscoveryChat(t, st, source.ID, "Taylor Three", "direct_chat", time.Unix(200, 0))
	page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Taylor", Limit: 2})
	requirements.NoError(err)
	requirements.Len(page.Results, 2)
	assertions.Equal(recent, page.Results[0].ConversationID)
	assertions.Equal(first, page.Results[1].ConversationID)
	assertions.True(page.HasMore)
	page, err = st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Taylor"})
	requirements.NoError(err)
	requirements.Len(page.Results, 3)
	assertions.Equal(second, page.Results[2].ConversationID)
	page, err = st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Tay"})
	requirements.NoError(err)
	assertions.Empty(page.Results, "whole-token discovery does not match substrings")
}

func TestChatDiscoveryAnchorsNewestVisibleMessage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "anchor-account")
	requirements.NoError(err)
	insert := func(chat int64, id string, sent *time.Time) int64 {
		message := &store.Message{SourceID: source.ID, SourceMessageID: id, ConversationID: chat, MessageType: "beeper"}
		if sent != nil {
			message.SentAt = sql.NullTime{Time: *sent, Valid: true}
		}
		messageID, err := st.UpsertMessage(message)
		requirements.NoError(err)
		return messageID
	}
	at := func(seconds int64) *time.Time {
		value := time.Unix(seconds, 0)
		return &value
	}
	dated, err := st.EnsureConversationWithType(source.ID, "dated", "direct_chat", "Anchor Dated")
	requirements.NoError(err)
	insert(dated, "older", at(100))
	insert(dated, "tied-first", at(300))
	newest := insert(dated, "tied-later", at(300))
	insert(dated, "middle", at(200))
	insert(dated, "undated", nil)
	hidden := insert(dated, "hidden", at(400))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), hidden)
	requirements.NoError(err)
	undated, err := st.EnsureConversationWithType(source.ID, "undated", "direct_chat", "Anchor Undated")
	requirements.NoError(err)
	insert(undated, "first-undated", nil)
	latestUndated := insert(undated, "later-undated", nil)

	page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Anchor"})
	requirements.NoError(err)
	requirements.Len(page.Results, 2)
	assertions.Equal(dated, page.Results[0].ConversationID, "dated activity ranks above an undated chat")
	assertions.Equal(newest, page.Results[0].MessageID, "a timestamp tie picks the later message")
	assertions.Equal(undated, page.Results[1].ConversationID)
	assertions.Equal(latestUndated, page.Results[1].MessageID, "an undated chat picks its latest message")
}
