package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestInitSchemaBackfillsMicrosoftSentEnvelopeRecipients(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	st := f.Store
	ctx := t.Context()
	source, err := st.GetOrCreateSource("msmail", "owner@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "sent-envelope-migration", "Sent envelope migration")
	requirements.NoError(err)
	ownerID := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	toID := f.EnsureParticipant("to@example.test", "To recipient", "example.test")
	bccID := f.EnsureParticipant("bcc@example.test", "Bcc recipient", "example.test")

	newMessage := func(sourceMessageID string) int64 {
		record := storetest.NewMessage(source.ID, conversationID).WithSourceMessageID(sourceMessageID).Build()
		record.SenderID = sql.NullInt64{Int64: ownerID, Valid: true}
		record.IsSent = sql.NullBool{Bool: true, Valid: true}
		id, persistErr := st.PersistMessage(&store.MessagePersistData{
			Message: record,
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: []int64{ownerID}, EmailAddresses: []string{"owner@example.test"}},
				{Type: "to", ParticipantIDs: []int64{toID}, EmailAddresses: []string{"to@example.test"}},
				{Type: "bcc", ParticipantIDs: []int64{bccID}, EmailAddresses: []string{"bcc@example.test"}},
			},
		})
		requirements.NoError(persistErr)
		return id
	}

	metadataMessageID := newMessage("previously-sent-archive")
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE messages SET metadata=? WHERE id=?`),
		`{"msmail_sent_items_observed":true}`, metadataMessageID)
	requirements.NoError(err)

	labelMessageID := newMessage("currently-sent-label")
	rawLabelMessageID := newMessage("source-merged-import")
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO source_merge_archive_only_messages
		(message_id, original_source_message_id) VALUES (?, ?)`), rawLabelMessageID, "eml-"+strings.Repeat("a", 64))
	requirements.NoError(err)
	folders, err := st.EnsureMicrosoftMailFoldersContext(ctx, source.ID, map[string]store.LabelInfo{
		"sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	requirements.NoError(err)
	requirements.NoError(st.AddMessageLabels(labelMessageID, []int64{folders["sent"]}))
	requirements.NoError(st.AddMessageLabels(rawLabelMessageID, []int64{folders["sent"]}))

	rawObservedMessageID := newMessage("eml-" + strings.Repeat("b", 64))
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE messages SET metadata=? WHERE id=?`),
		`{"msmail_sent_items_observed":true}`, rawObservedMessageID)
	requirements.NoError(err)

	ordinaryMessageID := newMessage("not-sent")
	_, err = st.DB().ExecContext(ctx, `DELETE FROM msmail_sent_envelope_recipients`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM applied_migrations WHERE name=?`), "msmail_sent_envelope_recipients_v1")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(ctx, `UPDATE archive_metadata SET value='20' WHERE key='schema_version'`)
	requirements.NoError(err)

	requirements.NoError(st.InitSchemaContext(ctx))
	var applied bool
	requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		SELECT EXISTS(SELECT 1 FROM applied_migrations WHERE name=?)
	`), "msmail_sent_envelope_recipients_v1").Scan(&applied))
	assertions.True(applied)

	rows, err := st.DB().QueryContext(ctx, st.Rebind(`
		SELECT message_id, email_address FROM msmail_sent_envelope_recipients
		ORDER BY message_id, email_address
	`))
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	type envelope struct {
		messageID int64
		address   string
	}
	var got []envelope
	for rows.Next() {
		var item envelope
		requirements.NoError(rows.Scan(&item.messageID, &item.address))
		got = append(got, item)
	}
	requirements.NoError(rows.Err())
	assertions.Equal([]envelope{
		{messageID: metadataMessageID, address: "bcc@example.test"},
		{messageID: metadataMessageID, address: "to@example.test"},
		{messageID: labelMessageID, address: "bcc@example.test"},
		{messageID: labelMessageID, address: "to@example.test"},
		{messageID: rawObservedMessageID, address: "bcc@example.test"},
		{messageID: rawObservedMessageID, address: "to@example.test"},
	}, got)
	assertions.NotContains(got, envelope{messageID: ordinaryMessageID, address: "to@example.test"})
	assertions.NotContains(got, envelope{messageID: rawLabelMessageID, address: "to@example.test"},
		"the migration must not infer provider Sent provenance from a raw import's Sent label")
}
