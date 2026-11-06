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
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:peer:" + ms(signalfixture.TAlice1): {conversation: direct, body: "Hello from Alice"},
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:self:" + ms(signalfixture.TReply): {
			conversation: direct, body: "Replying", fromMe: true, replyTo: "signal:aci:0f0f0f0f-0000-4000-8000-000000000002:peer:" + ms(signalfixture.TAlice1),
		},
		"signal:group-v1:ABEiM0RVZneImaq7zN3u_w:" + signalfixture.CarolACI + ":" + ms(signalfixture.TCarol+10): {conversation: "signal:group-v1:ABEiM0RVZneImaq7zN3u_w", body: "Old group text"},
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:self:" + ms(signalfixture.TEditOld):                   {conversation: direct, body: "First draft", fromMe: true, edited: true},
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:peer:" + ms(signalfixture.TQuoteDraft): {
			conversation: direct, body: "Nice draft", replyTo: "signal:aci:0f0f0f0f-0000-4000-8000-000000000002:self:" + ms(signalfixture.TEditOld),
		},
		"signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:" + signalfixture.BobACI + ":" + ms(signalfixture.TGroup): {
			conversation: "signal:group:" + signalfixture.GroupB64, body: "Hi @Alice Example, look\n\nBeach",
		},
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000004:peer:" + ms(signalfixture.TCarol): {conversation: "signal:aci:" + signalfixture.CarolACI, body: "Carol here"},
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
			assert.Equal(int64(10), summary.MessagesProcessed)
			assert.Equal(int64(7), summary.MessagesAdded)
			assert.Equal(int64(0), summary.MessagesUpdated)
			assert.Equal(int64(3), summary.MessagesSkipped, "SMS, call, and group update")
			assert.Equal(int64(4), summary.ChatsProcessed)
			assert.Zero(summary.ChatsSkipped)
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
				{"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:self:" + ms(signalfixture.TEditOld), "\u2764", signalfixture.AlicePhone},
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
			assert.Equal("signal:attachment:2a6927613ddc903379f3e02145f22074ccf048c7753cfa1ed8ee26c746682997:0", partKey)
			assert.True(hasAttachments)
		})
	}
}

func TestImportDirectChatRecipients(t *testing.T) {
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	st := testutil.NewTestStore(t)
	storage := t.TempDir()
	runImport(t, st, dir, storage)

	for _, tc := range []struct {
		role string
		want []string
	}{
		{"to", []string{
			"signal:aci:" + signalfixture.AliceACI + ":self:" + ms(signalfixture.TReply),
			"signal:aci:" + signalfixture.AliceACI + ":self:" + ms(signalfixture.TEditOld),
		}},
		{"from", []string{
			"signal:aci:" + signalfixture.AliceACI + ":peer:" + ms(signalfixture.TAlice1),
			"signal:aci:" + signalfixture.AliceACI + ":peer:" + ms(signalfixture.TQuoteDraft),
		}},
	} {
		t.Run(tc.role, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, err := st.DB().Query(st.Rebind(`
				SELECT m.source_message_id FROM messages m
				JOIN message_recipients mr ON mr.message_id = m.id
				JOIN participants p ON p.id = mr.participant_id
				JOIN conversations c ON c.id = m.conversation_id
				WHERE mr.recipient_type = ? AND p.phone_number = ? AND c.source_conversation_id = ?`),
				tc.role, signalfixture.AlicePhone, "signal:aci:"+signalfixture.AliceACI)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			var got []string
			for rows.Next() {
				var id string
				require.NoError(rows.Scan(&id))
				got = append(got, id)
			}
			require.NoError(rows.Err())
			assert.ElementsMatch(tc.want, got)
		})
	}

	for _, phase := range []string{"initial group", "changed membership"} {
		t.Run(phase, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			if phase == "changed membership" {
				changeFixture(t, dir, `UPDATE group_membership SET recipient_id = 4 WHERE recipient_id = 2`)
				runImport(t, st, dir, storage)
			}
			rows, err := st.DB().Query(st.Rebind(`
				SELECT p.phone_number FROM message_recipients mr
				JOIN messages m ON m.id = mr.message_id
				JOIN participants p ON p.id = mr.participant_id
				WHERE mr.recipient_type = 'to' AND m.source_message_id = ?`),
				"signal:group:"+signalfixture.GroupB64+":"+signalfixture.BobACI+":"+ms(signalfixture.TGroup))
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			var phones []string
			for rows.Next() {
				var phone string
				require.NoError(rows.Scan(&phone))
				phones = append(phones, phone)
			}
			require.NoError(rows.Err())
			assert.ElementsMatch([]string{signalfixture.OwnPhone, signalfixture.AlicePhone}, phones)
		})
	}
}

func TestImportSenderSharedName(t *testing.T) {
	for _, tc := range []struct{ name, given, family, system, profile, nickname, want string }{
		{name: "given only", given: "Bob", want: "Bob"},
		{name: "family only", family: "Example", want: "Example"},
		{name: "full name", given: "Bob", family: "Example", want: "Bob Example"},
		{name: "profile takes precedence", given: "Shared", family: "Name", profile: "Bob Example", want: "Bob Example"},
		{name: "nickname takes precedence", given: "Shared", family: "Name", system: "System name", profile: "Bob Example", nickname: "Example nickname", want: "Example nickname"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			dir := t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir,
				`ALTER TABLE recipient ADD COLUMN shared_given_name TEXT DEFAULT NULL`,
				`ALTER TABLE recipient ADD COLUMN shared_family_name TEXT DEFAULT NULL`,
				`UPDATE recipient SET system_joined_name = '`+tc.system+`', profile_joined_name = '`+tc.profile+`', nickname_joined_name = '`+tc.nickname+`', shared_given_name = '`+tc.given+`', shared_family_name = '`+tc.family+`' WHERE aci = '`+signalfixture.BobACI+`'`,
			)
			st := testutil.NewTestStore(t)
			runImport(t, st, dir, t.TempDir())

			var name string
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT COALESCE(p.display_name, '') FROM messages m
				JOIN participants p ON p.id = m.sender_id
				WHERE m.source_message_id = ?`), "signal:group:"+signalfixture.GroupB64+":"+signalfixture.BobACI+":"+ms(signalfixture.TGroup)).Scan(&name))
			assert.Equal(tc.want, name)
		})
	}
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
	assert.Equal(int64(7), summary.MessagesUpdated)
	assert.Equal(expectedMessages(), importedMessages(t, st))

	var reactions, attachments, conversations int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachments))
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM conversations WHERE source_id = ?`), summary.SourceID).Scan(&conversations))
	assert.Equal(2, reactions)
	assert.Equal(1, attachments)
	assert.Equal(4, conversations)
}

func TestImportRejectsUnsupportedDatabases(t *testing.T) {
	tests := []struct {
		name    string
		version int
		ddl     []string
		want    string
	}{
		{
			name: "not signal", version: 215,
			ddl:  nil,
			want: "chat details are missing; use database.sqlite from a Signal Android backup",
		},
		{
			name: "before the single message table", version: 184,
			ddl: []string{
				`CREATE TABLE recipient (_id INTEGER PRIMARY KEY, uuid TEXT, phone TEXT, group_id TEXT)`,
				`CREATE TABLE thread (_id INTEGER PRIMARY KEY, thread_recipient_id INTEGER)`,
				`CREATE TABLE sms (_id INTEGER PRIMARY KEY)`,
				`CREATE TABLE mms (_id INTEGER PRIMARY KEY)`,
			},
			want: "predates Signal Android database version 185",
		},
		{
			// Missing authorship must fail schema validation before an import starts.
			name: "damaged message authorship columns", version: 215,
			ddl: []string{
				`CREATE TABLE recipient (_id INTEGER PRIMARY KEY, uuid TEXT, phone TEXT, group_id TEXT)`,
				`CREATE TABLE thread (_id INTEGER PRIMARY KEY, recipient_id INTEGER)`,
				`CREATE TABLE message (_id INTEGER PRIMARY KEY, date_sent INTEGER, date_received INTEGER,
					thread_id INTEGER, recipient_id INTEGER, type INTEGER, body TEXT,
					quote_id INTEGER, quote_author INTEGER)`,
			},
			want: "message details are missing; msgvault reads Signal Android database versions 185 through 330",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			path := writeTablesFixture(t, tt.version, tt.ddl...)
			opts := DefaultOptions()
			opts.Phone = signalfixture.OwnPhone
			st := testutil.NewTestStore(t)
			_, err := NewImporter(st, nil).Import(context.Background(), path, opts)
			require.ErrorIs(err, ErrUnsupportedDatabase)
			assert.Contains(err.Error(), tt.want)
			assert.Zero(countRows(t, st, `SELECT COUNT(*) FROM sources`))
		})
	}
}

func TestImportRejectsIncompleteBackupDetails(t *testing.T) {
	// Each missing column guards its own schema check before creating a source.
	for _, tc := range []struct{ name, change, want string }{
		{"account IDs", `ALTER TABLE recipient DROP COLUMN aci`, "account IDs are missing"},
		{"phone numbers", `ALTER TABLE recipient DROP COLUMN e164`, "phone numbers are missing"},
		{"chat contacts", `ALTER TABLE thread DROP COLUMN recipient_id`, "chat contacts are missing"},
		{"contacts", `ALTER TABLE recipient DROP COLUMN group_id`, "contact details are missing"},
		{"attachments", `ALTER TABLE attachment DROP COLUMN content_type`, "attachment details are missing"},
		{"reaction message", `ALTER TABLE reaction DROP COLUMN message_id`, "reaction details are missing"},
		{"reaction author", `ALTER TABLE reaction DROP COLUMN author_id`, "reaction details are missing"},
		{"reaction emoji", `ALTER TABLE reaction DROP COLUMN emoji`, "reaction details are missing"},
		{"reaction sent date", `ALTER TABLE reaction DROP COLUMN date_sent`, "reaction details are missing"},
		{"mention message", `ALTER TABLE mention DROP COLUMN message_id`, "mention details are missing"},
		{"mention recipient", `ALTER TABLE mention DROP COLUMN recipient_id`, "mention details are missing"},
		{"mention range", `ALTER TABLE mention DROP COLUMN range_start`, "mention details are missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			dir := t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, tc.change)
			opts := DefaultOptions()
			opts.Phone = signalfixture.OwnPhone
			st := testutil.NewSQLiteTestStore(t)
			_, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.ErrorIs(err, ErrUnsupportedDatabase)
			assert.Contains(err.Error(), tc.want)
			assert.Zero(countRows(t, st, `SELECT COUNT(*) FROM sources`))
		})
	}

	// Schema rejection must also leave an existing archive untouched.
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	st, storage := testutil.NewSQLiteTestStore(t), t.TempDir()
	runImport(t, st, dir, storage)
	before := importedMessages(t, st)
	reactions := countRows(t, st, `SELECT COUNT(*) FROM reactions`)
	changeFixture(t, dir, `ALTER TABLE recipient DROP COLUMN aci`, `UPDATE message SET body = 'Changed text' WHERE _id = 300`)
	opts := DefaultOptions()
	opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, storage
	_, err := NewImporter(st, nil).Import(t.Context(), path, opts)
	require.ErrorIs(err, ErrUnsupportedDatabase)
	assert.Equal(before, importedMessages(t, st))
	assert.Equal(reactions, countRows(t, st, `SELECT COUNT(*) FROM reactions`))
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
	// Existing messages precede the next new messages in the same backup.
	summary, err = NewImporter(st, nil).Import(context.Background(), path, opts)
	require.NoError(err)
	assert.Equal(int64(2), summary.MessagesAdded)
	assert.Len(importedMessages(t, st), 4)
}

func TestImportPre191DuplicateMessage(t *testing.T) {
	for name, limit := range map[string]int{"unlimited": 0, "limit_seven": 7} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			dir := t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.V185, 0)
			changeFixture(t, dir, `INSERT INTO message
				(_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body)
				SELECT 99, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body
				FROM message WHERE _id = 100`)
			opts := DefaultOptions()
			opts.Phone = signalfixture.OwnPhone
			opts.Limit = limit
			opts.AttachmentFilesDir = dir
			opts.AttachmentsDir = t.TempDir()
			st := testutil.NewTestStore(t)
			summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.NoError(t, err)
			assert.Zero(summary.Errors)
			assert.Equal(int64(7), summary.MessagesAdded)
			assert.Equal(int64(1), summary.MessagesUpdated)
			assert.Equal(7, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE source_id = ?`, summary.SourceID))
		})
	}
}

func TestSkipReason(t *testing.T) {
	tests := []struct {
		name string
		row  sigMessage
		want string
	}{
		{"incoming signal message", sigMessage{Type: signalfixture.TypeIncoming}, ""},
		{"outgoing signal message", sigMessage{Type: signalfixture.TypeOutgoing}, ""},
		{"sending skipped", sigMessage{Type: 28 | pushMessageBit | secureMessageBit}, ""},
		{"plain sms", sigMessage{Type: signalfixture.TypeSMSIncoming}, "sms"},
		{"call", sigMessage{Type: signalfixture.TypeCallIncoming}, "event"},
		{"group update", sigMessage{Type: signalfixture.TypeGroupUpdate}, "event"},
		{"expiration timer", sigMessage{Type: signalfixture.TypeIncoming | expirationTimerBit}, "event"},
		{"payment notification", sigMessage{Type: signalfixture.TypeIncoming | 0x300000000}, "event"},
		{"draft", sigMessage{Type: 27 | secureMessageBit}, "event"},
		{"story", sigMessage{Type: signalfixture.TypeIncoming, StoryType: 1}, "story"},
		{"group story reply", sigMessage{Type: signalfixture.TypeIncoming, ParentStoryID: 9}, "story"},
		{"direct story reply", sigMessage{Type: signalfixture.TypeIncoming, ParentStoryID: -9}, ""},
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
	id, ok := groupIdentifier(groupV2IDPrefix+signalfixture.GroupHex, groupV2IDPrefix)
	require.True(ok)
	assert.Equal(signalfixture.GroupB64, id)

	_, ok = groupIdentifier("__textsecure_group__!00ff", groupV2IDPrefix)
	assert.False(ok)

	assert.Equal(signalfixture.AliceACI, normalizeACI("ACI:"+"0F0F0F0F-0000-4000-8000-000000000002"))
	assert.Empty(normalizeACI("PNI:" + signalfixture.AliceACI))
	assert.Empty(normalizeACI("00000000-0000-0000-0000-000000000000"))
	assert.Equal(signalfixture.AliceACI, normalizePNI(" PNI:0F0F0F0F-0000-4000-8000-000000000002 "))
	assert.Equal(signalfixture.AliceACI, normalizePNI(signalfixture.AliceACI))
	assert.Empty(normalizePNI("ACI:" + signalfixture.AliceACI))
	assert.Empty(normalizePNI("00000000-0000-0000-0000-000000000000"))
	assert.Empty(normalizePNI("invalid"))

	_, _, ok = conversationIdentity(sigRecipient{}, nil)
	assert.False(ok, "a distribution list or call link has no identity")
}

func TestApplyMentions(t *testing.T) {
	assert := assert.New(t)
	names := map[int64]string{1: "Ann", 2: "Ben"}
	got := applyMentions("\uFFFC and \uFFFC", []sigMention{{RecipientID: 1, RangeStart: 0}, {RecipientID: 2, RangeStart: 6}},
		func(id int64) string { return names[id] })
	assert.Equal("@Ann and @Ben", got)
	assert.Equal("no mention", applyMentions("no mention", nil, nil))
	// The emoji takes two UTF-16 code units; the first placeholder is literal text.
	got = applyMentions("😀 \uFFFC then \uFFFC and \uFFFC", []sigMention{{RecipientID: 1, RangeStart: 10}, {RecipientID: 2, RangeStart: 16}},
		func(id int64) string { return names[id] })
	assert.Equal("😀 \uFFFC then @Ann and @Ben", got)
}
