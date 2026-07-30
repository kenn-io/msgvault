package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestInitSchemaBackfillsLegacyIdentityDerivedAttribution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	senderID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")

	type migratedMessage struct {
		sourceID  int64
		messageID int64
	}
	migrated := make([]migratedMessage, 0, 2)
	for _, sourceType := range []string{"granola", "circleback"} {
		src, err := st.GetOrCreateSource(sourceType, sourceType+"-account")
		require.NoError(err, "create %s source", sourceType)
		convID, err := st.EnsureConversationWithType(
			src.ID,
			sourceType+"-conversation",
			"meeting",
			"Legacy meeting",
		)
		require.NoError(err, "create %s conversation", sourceType)
		messageID, err := st.UpsertMessage(&store.Message{
			SourceID:        src.ID,
			ConversationID:  convID,
			SourceMessageID: sourceType + "-message",
			MessageType:     "meeting",
			SenderID:        sql.NullInt64{Int64: senderID, Valid: true},
			IsFromMe:        true,
		})
		require.NoError(err, "persist legacy %s message", sourceType)
		require.NoError(
			st.AddAccountIdentity(src.ID, "owner@example.com", "account-identifier"),
			"add %s identity",
			sourceType,
		)
		migrated = append(migrated, migratedMessage{sourceID: src.ID, messageID: messageID})
	}

	nativeMessage := f.NewMessage().
		WithSourceMessageID("source-native-control").
		WithIsFromMe(true).
		Build()
	nativeMessage.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	nativeMessageID, err := st.UpsertMessage(nativeMessage)
	require.NoError(err, "persist source-native control")

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages
		SET source_is_from_me = NULL, identity_is_from_me = FALSE
		WHERE id IN (?, ?, ?)
	`), migrated[0].messageID, migrated[1].messageID, nativeMessageID)
	require.NoError(err, "simulate rows written before attribution provenance")
	_, err = st.DB().Exec(st.Rebind(`
		DELETE FROM applied_migrations
		WHERE name = ?
	`), "message_attribution_provenance_v2")
	require.NoError(err, "reset attribution migration sentinel")
	_, err = st.DB().Exec(`
		INSERT INTO applied_migrations (name)
		VALUES ('message_attribution_provenance_v1')
		ON CONFLICT (name) DO NOTHING
	`)
	require.NoError(err, "record prior attribution migration")

	require.NoError(st.InitSchema(), "run production schema migration")

	for _, message := range migrated {
		var sourceDerived sql.NullBool
		var identityDerived bool
		require.NoError(
			st.DB().QueryRow(st.Rebind(`
				SELECT source_is_from_me, identity_is_from_me
				FROM messages
				WHERE id = ?
			`), message.messageID).Scan(&sourceDerived, &identityDerived),
			"read migrated provenance for message %d",
			message.messageID,
		)
		assert.True(sourceDerived.Valid, "migration must assign explicit source provenance")
		assert.False(sourceDerived.Bool, "legacy meeting attribution is not source-native")
		assert.True(identityDerived, "legacy meeting attribution must remain identity-derived")

		removed, removeErr := st.RemoveAccountIdentity(message.sourceID, "owner@example.com")
		require.NoError(removeErr, "remove migrated identity")
		require.Equal(int64(1), removed)
		isFromMe, attributionErr := st.GetMessageIsFromMe(message.messageID)
		require.NoError(attributionErr, "read attribution after identity removal")
		assert.False(isFromMe, "identity removal must clear migrated attribution")
	}

	var nativeSourceDerived sql.NullBool
	require.NoError(
		st.DB().QueryRow(st.Rebind(`
			SELECT source_is_from_me
			FROM messages
			WHERE id = ?
		`), nativeMessageID).Scan(&nativeSourceDerived),
		"read source-native control provenance",
	)
	assert.True(nativeSourceDerived.Valid, "migration must initialize source-native provenance")
	assert.True(nativeSourceDerived.Bool, "migration must preserve source-native attribution")
}

func TestInitSchemaClearsLegacyAttributionWithoutRemainingIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	senderID := f.EnsureParticipant("former-owner@example.com", "Former Owner", "example.com")

	src, err := st.GetOrCreateSource("granola", "removed-identity-account")
	require.NoError(err, "create granola source")
	convID, err := st.EnsureConversationWithType(
		src.ID,
		"removed-identity-conversation",
		"meeting",
		"Legacy meeting",
	)
	require.NoError(err, "create granola conversation")
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID:        src.ID,
		ConversationID:  convID,
		SourceMessageID: "removed-identity-message",
		MessageType:     "meeting",
		SenderID:        sql.NullInt64{Int64: senderID, Valid: true},
		IsFromMe:        true,
	})
	require.NoError(err, "persist stale legacy attribution")

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages
		SET source_is_from_me = NULL, identity_is_from_me = FALSE
		WHERE id = ?
	`), messageID)
	require.NoError(err, "simulate message written before attribution provenance")
	_, err = st.DB().Exec(st.Rebind(`
		DELETE FROM applied_migrations
		WHERE name = ?
	`), "message_attribution_provenance_v2")
	require.NoError(err, "reset attribution migration sentinel")

	require.NoError(st.InitSchema(), "run production schema migration")

	var sourceDerived, identityDerived, isFromMe bool
	require.NoError(
		st.DB().QueryRow(st.Rebind(`
			SELECT source_is_from_me, identity_is_from_me, is_from_me
			FROM messages
			WHERE id = ?
		`), messageID).Scan(&sourceDerived, &identityDerived, &isFromMe),
		"read reconciled attribution",
	)
	assert.False(sourceDerived, "legacy Granola attribution is not source-native")
	assert.False(identityDerived, "removed identity must not remain as provenance")
	assert.False(isFromMe, "migration must clear attribution whose identity was already removed")
}

func TestInitSchemaReconcilesConfirmedIdentityForEverySource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	senderID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(
		st.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"),
		"confirm Gmail identity",
	)
	message := f.NewMessage().WithSourceMessageID("legacy-gmail-identity").Build()
	message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	messageID, err := st.UpsertMessage(message)
	require.NoError(err, "persist matching Gmail message")

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages
		SET is_from_me = FALSE,
		    source_is_from_me = NULL,
		    identity_is_from_me = FALSE
		WHERE id = ?
	`), messageID)
	require.NoError(err, "simulate stale legacy Gmail attribution")
	_, err = st.DB().Exec(st.Rebind(`
		DELETE FROM applied_migrations
		WHERE name = ?
	`), "message_attribution_provenance_v2")
	require.NoError(err, "reset attribution migration sentinel")

	revisionBefore, err := st.AccountIdentityRevision()
	require.NoError(err, "read account identity revision before migration")
	require.NoError(st.InitSchema(), "run production schema migration")

	var sourceDerived, identityDerived, isFromMe bool
	require.NoError(
		st.DB().QueryRow(st.Rebind(`
			SELECT source_is_from_me, identity_is_from_me, is_from_me
			FROM messages
			WHERE id = ?
		`), messageID).Scan(&sourceDerived, &identityDerived, &isFromMe),
		"read reconciled Gmail attribution",
	)
	assert.False(sourceDerived, "incoming Gmail message was not source-native")
	assert.True(identityDerived, "confirmed identity must be reconciled for Gmail")
	assert.True(isFromMe, "effective attribution must include the confirmed identity")

	revisionAfter, err := st.AccountIdentityRevision()
	require.NoError(err, "read account identity revision after migration")
	assert.Equal(
		revisionBefore+1,
		revisionAfter,
		"provenance migration must invalidate caches atomically",
	)
}

func TestInitSchemaBackfillsLegacyCalendarAttributionProvenance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	src, err := st.GetOrCreateSource(gcal.SourceType, "owner@example.com/primary")
	require.NoError(err, "create calendar source")
	require.NoError(
		st.AddAccountIdentity(src.ID, "owner@example.com", "account-email"),
		"confirm configured calendar account",
	)
	senderID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")

	persistLegacyEvent := func(sourceMessageID, raw string) int64 {
		convID, ensureErr := st.EnsureConversationWithType(
			src.ID,
			sourceMessageID+"-conversation",
			gcal.ConversationType,
			"Legacy calendar event",
		)
		require.NoError(ensureErr, "create %s conversation", sourceMessageID)
		messageID, upsertErr := st.UpsertMessage(&store.Message{
			SourceID:        src.ID,
			ConversationID:  convID,
			SourceMessageID: sourceMessageID,
			MessageType:     gcal.MessageTypeCalendarEvent,
			SenderID:        sql.NullInt64{Int64: senderID, Valid: true},
			IsFromMe:        true,
		})
		require.NoError(upsertErr, "persist legacy %s message", sourceMessageID)
		require.NoError(
			st.UpsertMessageRawWithFormat(messageID, []byte(raw), gcal.RawFormat),
			"persist legacy %s raw event",
			sourceMessageID,
		)
		return messageID
	}

	identityMessageID := persistLegacyEvent(
		"configured-account-organizer",
		`{"organizer":{"email":"owner@example.com"}}`,
	)
	nativeMessageID := persistLegacyEvent(
		"google-self-organizer",
		`{"organizer":{"email":"owner@example.com","self":true}}`,
	)

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages
		SET source_is_from_me = NULL,
		    identity_is_from_me = FALSE,
		    is_from_me = TRUE
		WHERE id IN (?, ?)
	`), identityMessageID, nativeMessageID)
	require.NoError(err, "simulate calendar rows written before attribution provenance")
	_, err = st.DB().Exec(st.Rebind(`
		DELETE FROM applied_migrations
		WHERE name = ?
	`), "message_attribution_provenance_v2")
	require.NoError(err, "reset attribution migration sentinel")

	require.NoError(st.InitSchema(), "run production schema migration")

	var identitySourceDerived, identityDerived bool
	require.NoError(
		st.DB().QueryRow(st.Rebind(`
			SELECT source_is_from_me, identity_is_from_me
			FROM messages
			WHERE id = ?
		`), identityMessageID).Scan(&identitySourceDerived, &identityDerived),
		"read configured-account provenance",
	)
	assert.False(identitySourceDerived, "configured-account match is not source-native")
	assert.True(identityDerived, "configured-account match must remain identity-derived")

	var nativeSourceDerived bool
	require.NoError(
		st.DB().QueryRow(st.Rebind(`
			SELECT source_is_from_me
			FROM messages
			WHERE id = ?
		`), nativeMessageID).Scan(&nativeSourceDerived),
		"read Organizer.Self provenance",
	)
	assert.True(nativeSourceDerived, "Organizer.Self must remain source-native")

	removed, err := st.RemoveAccountIdentity(src.ID, "owner@example.com")
	require.NoError(err, "remove configured calendar identity")
	require.Equal(int64(1), removed)

	identityIsFromMe, err := st.GetMessageIsFromMe(identityMessageID)
	require.NoError(err, "read configured-account attribution after identity removal")
	assert.False(identityIsFromMe, "identity removal must clear configured-account attribution")
	nativeIsFromMe, err := st.GetMessageIsFromMe(nativeMessageID)
	require.NoError(err, "read Organizer.Self attribution after identity removal")
	assert.True(nativeIsFromMe, "identity removal must preserve Organizer.Self attribution")
}
