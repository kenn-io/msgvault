package signalandroid

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

type importedMessage struct {
	conversation string
	body         string
	fromMe       bool
	edited       bool
	replyTo      string
}

func runImport(t *testing.T, st *store.Store, dbDir, attachmentsDir string) *ImportSummary {
	t.Helper()
	require := require.New(t)
	opts := DefaultOptions()
	opts.Phone = signalfixture.OwnPhone
	opts.AttachmentFilesDir = dbDir
	opts.AttachmentsDir = attachmentsDir
	summary, err := NewImporter(st, nil).Import(context.Background(), filepath.Join(dbDir, "database.sqlite"), opts)
	require.NoError(err)
	return summary
}

func importedMessages(t *testing.T, st *store.Store) map[string]importedMessage {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT m.source_message_id, c.source_conversation_id, COALESCE(b.body_text, ''),
		       m.is_from_me, COALESCE(m.is_edited, FALSE), COALESCE(p.source_message_id, '')
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN message_bodies b ON b.message_id = m.id
		LEFT JOIN messages p ON p.id = m.reply_to_message_id
		WHERE s.source_type = ? AND m.message_type = ?`), SourceType, MessageType)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	got := make(map[string]importedMessage)
	for rows.Next() {
		var id string
		var m importedMessage
		require.NoError(t, rows.Scan(&id, &m.conversation, &m.body, &m.fromMe, &m.edited, &m.replyTo))
		got[id] = m
	}
	require.NoError(t, rows.Err())
	return got
}

func ms(v int64) string { return strconv.FormatInt(v, 10) }

func expectedMessages() map[string]importedMessage {
	direct := "signal:aci:" + signalfixture.AliceACI
	return map[string]importedMessage{
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:" + signalfixture.AliceACI + ":" + ms(signalfixture.TAlice1): {conversation: direct, body: "Hello from Alice"},
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:" + signalfixture.OwnACI + ":" + ms(signalfixture.TReply): {
			conversation: direct, body: "Replying", fromMe: true, replyTo: "signal:aci:0f0f0f0f-0000-4000-8000-000000000002:" + signalfixture.AliceACI + ":" + ms(signalfixture.TAlice1),
		},
		// The edit keeps the original's identity and carries the newest text.
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:" + signalfixture.OwnACI + ":" + ms(signalfixture.TEditOld): {conversation: direct, body: "First draft", fromMe: true, edited: true},
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:" + signalfixture.AliceACI + ":" + ms(signalfixture.TQuoteDraft): {
			conversation: direct, body: "Nice draft", replyTo: "signal:aci:0f0f0f0f-0000-4000-8000-000000000002:" + signalfixture.OwnACI + ":" + ms(signalfixture.TEditOld),
		},
		"signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:" + signalfixture.BobACI + ":" + ms(signalfixture.TGroup): {
			conversation: "signal:group:" + signalfixture.GroupB64, body: "Hi @Alice Example, look\n\nBeach",
		},
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000004:" + signalfixture.CarolACI + ":" + ms(signalfixture.TCarol): {conversation: "signal:aci:" + signalfixture.CarolACI, body: "Carol here"},
	}
}

func TestImportMapsSignalDatabase(t *testing.T) {
	for name, layout := range map[string]signalfixture.Layout{"current": signalfixture.Current, "v215": signalfixture.V215, "v185": signalfixture.V185} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			dbDir := t.TempDir()
			signalfixture.Write(t, dbDir, layout, 0)
			st := testutil.NewTestStore(t)

			summary := runImport(t, st, dbDir, t.TempDir())

			assert.Equal(expectedMessages(), importedMessages(t, st))
			assert.Equal(int64(6), summary.MessagesAdded)
			assert.Equal(int64(0), summary.MessagesUpdated)
			assert.Equal(int64(4), summary.MessagesSkipped, "SMS, call, group update, and legacy group message")
			assert.Equal(int64(3), summary.ChatsProcessed)
			assert.Equal(int64(1), summary.ChatsSkipped)
			assert.Equal(int64(1), summary.MessagesEdited)
			assert.Equal(int64(2), summary.RepliesLinked)
			assert.Equal(int64(2), summary.ReactionsAdded)
			assert.Equal(int64(2), summary.AttachmentsFound, "quote thumbnails are not attachments")
			assert.Equal(int64(1), summary.AttachmentsStored)
			assert.Equal(int64(1), summary.AttachmentsMissing)
			assert.Zero(summary.Errors)

			var sourceType, identifier string
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT source_type, identifier FROM sources WHERE id = ?`), summary.SourceID).Scan(&sourceType, &identifier))
			assert.Equal("signal", sourceType)
			assert.Equal(signalfixture.OwnPhone, identifier)

			var groupTitle, groupType string
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT COALESCE(title, ''), conversation_type FROM conversations WHERE source_conversation_id = ?`),
				"signal:group:"+signalfixture.GroupB64).Scan(&groupTitle, &groupType))
			assert.Equal("Example Group", groupTitle)
			assert.Equal("group_chat", groupType)
		})
	}
}

func TestImportStoresReactionsParticipantsAndAttachments(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dbDir := t.TempDir()
	signalfixture.Write(t, dbDir, signalfixture.Current, 0)
	st := testutil.NewTestStore(t)
	runImport(t, st, dbDir, t.TempDir())

	type reaction struct{ message, emoji, phone string }
	reactions := func() []reaction {
		rows, err := st.DB().Query(st.Rebind(`
		SELECT m.source_message_id, r.reaction_value, COALESCE(p.phone_number, '')
		FROM reactions r
		JOIN messages m ON m.id = r.message_id
		JOIN participants p ON p.id = r.participant_id
		ORDER BY m.source_message_id`))
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		var got []reaction
		for rows.Next() {
			var r reaction
			require.NoError(rows.Scan(&r.message, &r.emoji, &r.phone))
			got = append(got, r)
		}
		require.NoError(rows.Err())
		return got
	}()
	assert.Equal([]reaction{
		{"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:" + signalfixture.OwnACI + ":" + ms(signalfixture.TEditOld), "\u2764", signalfixture.AlicePhone},
		{"signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:" + signalfixture.BobACI + ":" + ms(signalfixture.TGroup), "\U0001F44D", signalfixture.AlicePhone},
	}, reactions)

	// Bob has no visible number: his participant carries his ACI.
	var bobName string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(p.display_name, '') FROM participants p
		JOIN participant_identifiers pi ON pi.participant_id = p.id
		WHERE pi.identifier_type = ? AND pi.identifier_value = ?`), ACIIdentifierType, signalfixture.BobACI).Scan(&bobName))
	assert.Equal("Bob Example", bobName)

	// Alice is matched by number and also carries her ACI.
	var aliceByACI string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(p.phone_number, '') FROM participants p
		JOIN participant_identifiers pi ON pi.participant_id = p.id
		WHERE pi.identifier_type = ? AND pi.identifier_value = ?`), ACIIdentifierType, signalfixture.AliceACI).Scan(&aliceByACI))
	assert.Equal(signalfixture.AlicePhone, aliceByACI)

	var groupMembers int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN conversations c ON c.id = cp.conversation_id
		WHERE c.source_conversation_id = ?`), "signal:group:"+signalfixture.GroupB64).Scan(&groupMembers))
	assert.Equal(3, groupMembers)

	var mime, mediaType, partKey string
	var size, width int64
	var hasAttachments bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT a.mime_type, COALESCE(a.media_type, ''), a.size, COALESCE(a.width, 0),
		       COALESCE(a.source_part_key, ''), m.has_attachments
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:"+signalfixture.BobACI+":"+ms(signalfixture.TGroup)).Scan(&mime, &mediaType, &size, &width, &partKey, &hasAttachments))
	assert.Equal("image/jpeg", mime)
	assert.Equal("image", mediaType)
	assert.Equal(int64(len("photo bytes")), size)
	assert.Equal(int64(640), width)
	assert.Equal("signal:attachment:0", partKey)
	assert.True(hasAttachments)
}

func TestReimportAddsNothingAfterRestore(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	attachmentsDir := t.TempDir()
	first := t.TempDir()
	signalfixture.Write(t, first, signalfixture.Current, 0)
	runImport(t, st, first, attachmentsDir)

	// The same history restored on another phone gets new local row IDs.
	restored := t.TempDir()
	signalfixture.Write(t, restored, signalfixture.Current, 7000)
	summary := runImport(t, st, restored, attachmentsDir)

	assert.Equal(int64(0), summary.MessagesAdded)
	assert.Equal(int64(6), summary.MessagesUpdated)
	assert.Equal(expectedMessages(), importedMessages(t, st))

	var reactions, attachments, conversations int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachments))
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM conversations WHERE source_id = ?`), summary.SourceID).Scan(&conversations))
	assert.Equal(2, reactions)
	assert.Equal(1, attachments)
	assert.Equal(3, conversations)
}

func TestImportRejectsAnotherAccountsBackup(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dbDir := t.TempDir()
	path := signalfixture.Write(t, dbDir, signalfixture.Current, 0)
	opts := DefaultOptions()
	opts.Phone = "+15555550199"
	_, err := NewImporter(testutil.NewTestStore(t), nil).Import(context.Background(), path, opts)
	require.Error(err)
	assert.Contains(err.Error(), "--phone does not match")
}

func TestImportRejectsUnsupportedDatabases(t *testing.T) {
	tests := []struct {
		name string
		ddl  []string
		want string
	}{
		{
			name: "not signal",
			ddl:  []string{`CREATE TABLE message (_id INTEGER PRIMARY KEY)`},
			want: "recipient and thread tables are missing",
		},
		{
			name: "before the single message table",
			ddl: []string{
				`CREATE TABLE recipient (_id INTEGER PRIMARY KEY, uuid TEXT, phone TEXT, group_id TEXT)`,
				`CREATE TABLE thread (_id INTEGER PRIMARY KEY, thread_recipient_id INTEGER)`,
				`CREATE TABLE sms (_id INTEGER PRIMARY KEY)`,
				`CREATE TABLE mms (_id INTEGER PRIMARY KEY)`,
			},
			want: "predates Signal Android database version 185",
		},
		{
			name: "before message authorship columns",
			ddl: []string{
				`CREATE TABLE recipient (_id INTEGER PRIMARY KEY, uuid TEXT, phone TEXT, group_id TEXT)`,
				`CREATE TABLE thread (_id INTEGER PRIMARY KEY, recipient_id INTEGER)`,
				`CREATE TABLE message (_id INTEGER PRIMARY KEY, date_sent INTEGER, date_received INTEGER,
					thread_id INTEGER, recipient_id INTEGER, type INTEGER, body TEXT,
					quote_id INTEGER, quote_author INTEGER)`,
			},
			want: "message.from_recipient_id is missing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			path := writeTablesFixture(t, tt.ddl...)
			opts := DefaultOptions()
			opts.Phone = signalfixture.OwnPhone
			_, err := NewImporter(testutil.NewTestStore(t), nil).Import(context.Background(), path, opts)
			require.ErrorIs(err, ErrUnsupportedDatabase)
			assert.Contains(err.Error(), tt.want)
		})
	}
}

func TestImportLimitStopsAfterWrites(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dbDir := t.TempDir()
	path := signalfixture.Write(t, dbDir, signalfixture.Current, 0)
	opts := DefaultOptions()
	opts.Phone = signalfixture.OwnPhone
	opts.Limit = 2
	st := testutil.NewTestStore(t)
	summary, err := NewImporter(st, nil).Import(context.Background(), path, opts)
	require.NoError(err)
	assert.Equal(int64(2), summary.MessagesAdded)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`),
		summary.SourceID).Scan(&count))
	assert.Equal(2, count)
}

func TestSkipReason(t *testing.T) {
	tests := []struct {
		name string
		row  sigMessage
		want string
	}{
		{"incoming signal message", sigMessage{Type: signalfixture.TypeIncoming}, ""},
		{"outgoing signal message", sigMessage{Type: signalfixture.TypeOutgoing}, ""},
		{"plain sms", sigMessage{Type: signalfixture.TypeSMSIncoming}, "sms"},
		{"call", sigMessage{Type: signalfixture.TypeCallIncoming}, "event"},
		{"group update", sigMessage{Type: signalfixture.TypeGroupUpdate}, "event"},
		{"expiration timer", sigMessage{Type: signalfixture.TypeIncoming | expirationTimerBit}, "event"},
		{"payment notification", sigMessage{Type: signalfixture.TypeIncoming | 0x300000000}, "event"},
		{"draft", sigMessage{Type: 27 | secureMessageBit}, "event"},
		{"story", sigMessage{Type: signalfixture.TypeIncoming, StoryType: 1}, "story"},
		{"story reply", sigMessage{Type: signalfixture.TypeIncoming, ParentStoryID: 9}, "story"},
		{"deleted for everyone", sigMessage{Type: signalfixture.TypeIncoming, RemoteDeleted: 1}, "deleted"},
		{"scheduled", sigMessage{Type: signalfixture.TypeOutgoing, ScheduledDate: 1}, "scheduled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			assert.Equal(tt.want, skipReason(tt.row))
		})
	}
}

func TestIdentifiers(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	id, ok := groupV2Identifier(groupV2IDPrefix + signalfixture.GroupHex)
	require.True(ok)
	assert.Equal(signalfixture.GroupB64, id)

	_, ok = groupV2Identifier("__textsecure_group__!00ff")
	assert.False(ok)

	assert.Equal(signalfixture.AliceACI, normalizeACI("ACI:"+"0F0F0F0F-0000-4000-8000-000000000002"))
	assert.Empty(normalizeACI("PNI:" + signalfixture.AliceACI))
	assert.Empty(normalizeACI("00000000-0000-0000-0000-000000000000"))

	_, _, ok = conversationIdentity(sigRecipient{})
	assert.False(ok, "a distribution list or call link has no identity")
}

func TestApplyMentions(t *testing.T) {
	assert := assert.New(t)
	names := map[int64]string{1: "Ann", 2: "Ben"}
	got := applyMentions("\uFFFC and \uFFFC", []sigMention{{RecipientID: 2, RangeStart: 8}, {RecipientID: 1, RangeStart: 0}},
		func(id int64) string { return names[id] })
	assert.Equal("@Ann and @Ben", got)
	assert.Equal("no mention", applyMentions("no mention", nil, nil))
}
