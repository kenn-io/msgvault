package imessage

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestImportImessageGeneratedTitleUsesCanonicalNamesAndNativeOrder(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path, db := newChatDB(t)
	_, err := db.Exec(`INSERT INTO handle(id) VALUES('+15550000002'),('alpha@example.test'),('+15550000003'),('beta@example.test'); INSERT INTO message(date,text) VALUES(757425600,'Synthetic native group'); INSERT INTO chat(guid,display_name,chat_identifier) VALUES('any;+;synthetic-group','','synthetic-group'); INSERT INTO chat_message_join(chat_id,message_id) VALUES(1,1); INSERT INTO chat_handle_join(chat_id,handle_id) VALUES(1,1),(1,2),(1,3),(1,4),(1,5)`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	_, err = st.EnsureParticipantByPhone("+15550000002", "Synthetic Gamma", "imessage")
	require.NoError(err)
	_, err = st.EnsureParticipantsBatch([]mime.Address{{Email: "alpha@example.test", Name: "Synthetic Alpha"}})
	require.NoError(err)
	source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { Require.NoError(t, client.Close()) })
	for range 2 {
		summary, err := client.Import(t.Context(), st, source.ID)
		require.NoError(err)
		require.Equal(1, summary.MessagesImported)
		var title string
		var members int
		require.NoError(st.DB().QueryRow(`SELECT title FROM conversations`).Scan(&title))
		assert.Equal("Synthetic Gamma, Synthetic Alpha, peer@example.test +2 more", title)
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM conversation_participants`).Scan(&members))
		assert.Equal(5, members)
	}
	var namespace string
	require.NoError(st.DB().QueryRow(`SELECT identifier_type FROM participant_identifiers WHERE identifier_value='+15550000003'`).Scan(&namespace))
	assert.Equal("imessage", namespace)
}

func TestImportImessageOmittedBodyRetainsNativeContent(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path, db := newChatDB(t)
	_, err := db.Exec(`INSERT INTO message(date,text) VALUES(757425600,'Synthetic retained body')`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { Require.NoError(t, client.Close()) })
	first, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, first.MessagesImported)
	_, err = db.Exec(`UPDATE message SET text=NULL,attributedBody=NULL,service='iMessage'`)
	require.NoError(err)
	retry, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, retry.MessagesImported)
	var id int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&id))
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("Synthetic retained body", body)
	var labels int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_labels`).Scan(&labels))
	assert.Equal(2, labels, "native service labels are additive")
}

func TestImportImessageEmptyPlaceholderHasReadyBodyRow(t *testing.T) {
	require := Require.New(t)

	path, db := newChatDB(t)
	_, err := db.Exec(`INSERT INTO message(date,text) VALUES(0,NULL)`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { Require.NoError(t, client.Close()) })
	summary, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, summary.MessagesImported)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_bodies WHERE body_text IS NULL AND body_html IS NULL`).Scan(&count))
	Assert.Equal(t, 1, count)
}
