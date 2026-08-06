package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type messageIdentityFixture struct {
	Store    *store.Store
	SourceA  int64
	SourceB  int64
	MessageA int64
	MessageB int64
}

func newMessageIdentityFixture(t *testing.T) messageIdentityFixture {
	t.Helper()

	fx := storetest.New(t)
	other, err := fx.Store.GetOrCreateSource("gmail", "other-source@example.test")
	require.NoError(t, err, "create second source")
	otherConversationID, err := fx.Store.EnsureConversation(other.ID, "other-thread", "Other Thread")
	require.NoError(t, err, "create second-source conversation")

	sendAs := fx.EnsureParticipant("send-as@example.test", "Send As", "example.test")
	otherFrom := fx.EnsureParticipant("outside@example.test", "Outside", "example.test")
	maskedRecipient := fx.EnsureParticipant("MASKED-SHOP@EXAMPLE.TEST", "Masked Shop", "example.test")
	ccRecipient := fx.EnsureParticipant("cc-owner@example.test", "CC Owner", "example.test")
	bccRecipient := fx.EnsureParticipant("bcc-owner@example.test", "BCC Owner", "example.test")
	directSender := fx.EnsureParticipant("direct-owner@example.test", "Direct Owner", "example.test")

	messageA := fx.NewMessage().WithSubject("identity intersection").Build()
	messageA.SenderID = sql.NullInt64{Int64: directSender, Valid: true}
	messageAID, err := fx.Store.UpsertMessage(messageA)
	require.NoError(t, err, "create source-A message")
	require.NoError(t, fx.Store.ReplaceMessageRecipients(
		messageAID, "from", []int64{otherFrom, sendAs}, []string{"Outside", "Send As"}),
		"add every From row")
	require.NoError(t, fx.Store.ReplaceMessageRecipients(
		messageAID, "to", []int64{maskedRecipient}, []string{"Masked Shop"}),
		"add To row")
	require.NoError(t, fx.Store.ReplaceMessageRecipients(
		messageAID, "cc", []int64{ccRecipient}, []string{"CC Owner"}),
		"add Cc row")
	require.NoError(t, fx.Store.ReplaceMessageRecipients(
		messageAID, "bcc", []int64{bccRecipient}, []string{"BCC Owner"}),
		"add Bcc row")

	messageB := storetest.NewMessage(other.ID, otherConversationID).
		WithSubject("other source identity").Build()
	messageB.SenderID = sql.NullInt64{Int64: sendAs, Valid: true}
	messageBID, err := fx.Store.UpsertMessage(messageB)
	require.NoError(t, err, "create source-B message")
	require.NoError(t, fx.Store.ReplaceMessageRecipients(
		messageBID, "from", []int64{sendAs}, []string{"Send As"}),
		"add source-B From row")

	return messageIdentityFixture{
		Store:    fx.Store,
		SourceA:  fx.Source.ID,
		SourceB:  other.ID,
		MessageA: messageAID,
		MessageB: messageBID,
	}
}

func TestMatchMessageIdentitiesUsesEveryFromAndRecipientWithinSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := newMessageIdentityFixture(t)
	require.NoError(fx.Store.AddAccountIdentity(fx.SourceA, "Send-As@Example.test", "manual"))
	require.NoError(fx.Store.AddAccountIdentity(fx.SourceA, "masked-shop@example.test", "provider-alias"))
	require.NoError(fx.Store.AddAccountIdentity(fx.SourceA, "cc-owner@example.test", "manual"))
	require.NoError(fx.Store.AddAccountIdentity(fx.SourceA, "bcc-owner@example.test", "manual"))
	require.NoError(fx.Store.AddAccountIdentity(fx.SourceA, "direct-owner@example.test", "manual"))
	require.NoError(fx.Store.AddAccountIdentity(fx.SourceB, "other@example.test", "manual"))

	matches, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{fx.MessageA})
	require.NoError(err)
	match := matches[fx.MessageA]
	assert.Equal(fx.MessageA, match.MessageID)
	assert.Equal(fx.SourceA, match.SourceID)
	assert.Equal([]string{"Send-As@Example.test"}, match.Sender)
	assert.Equal([]string{
		"bcc-owner@example.test",
		"cc-owner@example.test",
		"masked-shop@example.test",
	}, match.Recipients)
	assert.NotContains(match.Sender, "direct-owner@example.test",
		"sender_id must not supplement an existing From row")
}

func TestMatchMessageIdentitiesCannotLeakAcrossSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := newMessageIdentityFixture(t)
	require.NoError(fx.Store.AddAccountIdentity(fx.SourceA, "send-as@example.test", "manual"))

	matches, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{fx.MessageB})
	require.NoError(err)
	assert.Empty(matches[fx.MessageB].Sender)
	assert.NotNil(matches[fx.MessageB].Sender)
	assert.NotNil(matches[fx.MessageB].Recipients)
}

func TestMatchMessageIdentitiesPreservesSyntheticExactness(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	synthetic := fx.EnsureParticipant("@me:example.test", "Synthetic", "")
	message := fx.NewMessage().Build()
	message.SenderID = sql.NullInt64{Int64: synthetic, Valid: true}
	messageID, err := fx.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(fx.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{synthetic}, []string{"Synthetic"}))
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "@Me:example.test", "manual"))

	matches, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{messageID})
	require.NoError(err)
	assert.Empty(matches[messageID].Sender)

	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "@me:example.test", "manual"))
	matches, err = fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{messageID})
	require.NoError(err)
	assert.Equal([]string{"@me:example.test"}, matches[messageID].Sender)
}

func TestMatchMessageIdentitiesUsesDirectSenderOnlyWithoutFromRow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	direct := fx.EnsureParticipant("direct@example.test", "Direct", "example.test")
	message := fx.NewMessage().Build()
	message.SenderID = sql.NullInt64{Int64: direct, Valid: true}
	messageID, err := fx.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "Direct@Example.test", "manual"))

	matches, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{messageID})
	require.NoError(err)
	assert.Equal([]string{"Direct@Example.test"}, matches[messageID].Sender)
	assert.NotNil(matches[messageID].Recipients)
}

func TestMatchMessageIdentitiesDoesNotReplaceHeaderAddressWithAlternateIdentifier(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	sender := fx.EnsureParticipant("header-address@example.test", "Sender", "example.test")
	require.NoError(fx.Store.SetParticipantIdentifier(
		sender, "email", "confirmed-alias@example.test"))
	messageID := fx.NewMessage().Create(t, fx.Store)
	require.NoError(fx.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{sender}, []string{"Sender"}))
	require.NoError(fx.Store.AddAccountIdentity(
		fx.Source.ID, "confirmed-alias@example.test", "manual"))

	matches, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{messageID})
	require.NoError(err)
	assert.Empty(matches[messageID].Sender,
		"an alternate participant identifier is not the message's exact From address")
}

func TestMatchMessageIdentitiesChunksAndReturnsStableEmptyMatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	messageIDs := fx.CreateMessages(501)

	matches, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), messageIDs)
	require.NoError(err)
	require.Len(matches, len(messageIDs))
	for _, messageID := range messageIDs {
		match := matches[messageID]
		assert.Equal(fx.Source.ID, match.SourceID)
		assert.NotNil(match.Sender)
		assert.NotNil(match.Recipients)
	}
}

func TestResolveAccountIdentityContextReturnsStoredIdentityAndAllParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	primary := fx.EnsureParticipant("owner@example.test", "Primary", "example.test")
	alias := fx.EnsureParticipant("alias@example.test", "Alias", "example.test")
	require.NoError(fx.Store.SetParticipantIdentifier(alias, "email", "OWNER@EXAMPLE.TEST"))
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "Owner@Example.test", "manual"))

	resolved, err := fx.Store.ResolveAccountIdentityContext(
		t.Context(), fx.Source.ID, "owner@example.test")
	require.NoError(err)
	assert.Equal(fx.Source.ID, resolved.SourceID)
	assert.Equal("Owner@Example.test", resolved.Identifier)
	assert.Equal([]int64{primary, alias}, resolved.ParticipantIDs)
}

func TestResolveAccountIdentityContextRequiresExactSourceConfirmation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	other, err := fx.Store.GetOrCreateSource("gmail", "identity-other@example.test")
	require.NoError(err)
	require.NoError(fx.Store.AddAccountIdentity(other.ID, "owner@example.test", "manual"))

	_, err = fx.Store.ResolveAccountIdentityContext(
		context.Background(), fx.Source.ID, "OWNER@example.test")
	require.ErrorIs(err, store.ErrAccountIdentityNotFound)

	_, err = fx.Store.ResolveAccountIdentityContext(
		context.Background(), other.ID, "missing@example.test")
	require.ErrorIs(err, store.ErrAccountIdentityNotFound)

	require.NoError(fx.Store.AddAccountIdentity(other.ID, "@Me:example.test", "manual"))
	_, err = fx.Store.ResolveAccountIdentityContext(
		context.Background(), other.ID, "@me:example.test")
	require.ErrorIs(err, store.ErrAccountIdentityNotFound)
	resolved, err := fx.Store.ResolveAccountIdentityContext(
		context.Background(), other.ID, "@Me:example.test")
	require.NoError(err)
	assert.Equal("@Me:example.test", resolved.Identifier)
	assert.NotNil(resolved.ParticipantIDs)
}

// TestMatchMessageIdentitiesMatchesAttributionForPhoneIdentity guards
// hydration parity with messageIdentityAttributionMatch: a phone identity
// badges through the participant_identifiers row EnsureParticipantByPhone
// creates, never through participants.phone_number directly. If that row
// disappears (the MergeParticipants edge, simulated here with a direct
// delete) the badge must vanish, exactly as attribution stops matching.
func TestMatchMessageIdentitiesMatchesAttributionForPhoneIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	const phone = "+15550100001"

	participantID, err := fx.Store.EnsureParticipantByPhone(phone, "Phone Owner", "whatsapp")
	require.NoError(err, "EnsureParticipantByPhone")
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, phone, "manual"))

	message := fx.NewMessage().WithSubject("phone identity parity").Build()
	message.SenderID = sql.NullInt64{Int64: participantID, Valid: true}
	messageID, err := fx.Store.UpsertMessage(message)
	require.NoError(err, "create phone-sender message")
	require.NoError(fx.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{participantID}, []string{"Phone Owner"}),
		"add phone From row")

	matches, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{messageID})
	require.NoError(err)
	assert.Equal([]string{phone}, matches[messageID].Sender,
		"identifier-backed phone identity must badge the sender")

	_, err = fx.Store.DB().Exec(
		fx.Store.Rebind("DELETE FROM participant_identifiers WHERE participant_id = ?"),
		participantID,
	)
	require.NoError(err, "simulate MergeParticipants edge: identifier row gone, phone_number still set")

	matchesAfter, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{messageID})
	require.NoError(err)
	assert.Empty(matchesAfter[messageID].Sender,
		"participants.phone_number alone must not badge; attribution never consults it")
}

// TestMatchMessageIdentitiesNonEmailIdentifierIsCaseSensitive guards
// hydration parity with messageIdentityAttributionMatch's per-row rule: a
// participant_identifiers row whose type is not "email" compares
// case-sensitively based on identifier_type, not on whether the value looks
// email-shaped — the old shape-based normalization would case-fold an
// email-looking bridge handle and badge a differently cased identity that
// attribution rejects.
func TestMatchMessageIdentitiesNonEmailIdentifierIsCaseSensitive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)

	// Neither participant carries an email of its own, so hydration falls
	// back to the identifier rows; the values are deliberately email-shaped.
	newBridgeSender := func(name, identifierValue, sourceMessageID string) int64 {
		t.Helper()
		var senderID int64
		require.NoError(fx.Store.DB().QueryRow(fx.Store.Rebind(`
			INSERT INTO participants (display_name, created_at, updated_at)
			VALUES (?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			RETURNING id
		`), name).Scan(&senderID), "create email-less participant")
		require.NoError(fx.Store.SetParticipantIdentifier(senderID, "matrix", identifierValue))
		message := fx.NewMessage().WithSubject("bridge identifier parity").Build()
		message.SourceMessageID = sourceMessageID
		message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
		messageID, err := fx.Store.UpsertMessage(message)
		require.NoError(err, "create bridge-sender message")
		require.NoError(fx.Store.ReplaceMessageRecipients(
			messageID, "from", []int64{senderID}, []string{name}),
			"add bridge From row")
		return messageID
	}
	exactCaseMessage := newBridgeSender("Bridge Exact", "User@Example.Test", "bridge-exact")
	differentCaseMessage := newBridgeSender("Bridge Diff", "user@example.test", "bridge-diff")

	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "User@Example.Test", "manual"))

	matches, err := fx.Store.MatchMessageIdentitiesContext(
		t.Context(), []int64{exactCaseMessage, differentCaseMessage})
	require.NoError(err)
	assert.Equal([]string{"User@Example.Test"}, matches[exactCaseMessage].Sender,
		"exact-case match against a non-email identifier row must badge")
	assert.Empty(matches[differentCaseMessage].Sender,
		"non-email identifier types must compare case-sensitively even for email-shaped values")
}

// TestMatchMessageIdentitiesEmailTypedIdentifierIsCaseInsensitive locks in
// the email-typed half of the per-row rule: identifier rows of type "email"
// keep badging case-insensitively, same as attribution.
func TestMatchMessageIdentitiesEmailTypedIdentifierIsCaseInsensitive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)

	var senderID int64
	require.NoError(fx.Store.DB().QueryRow(fx.Store.Rebind(`
		INSERT INTO participants (display_name, created_at, updated_at)
		VALUES ('Alias User', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		RETURNING id
	`)).Scan(&senderID), "create email-less participant")
	require.NoError(fx.Store.SetParticipantIdentifier(senderID, "email", "Send-As@Example.Test"))

	message := fx.NewMessage().WithSubject("email identifier parity").Build()
	message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	messageID, err := fx.Store.UpsertMessage(message)
	require.NoError(err, "create alias-sender message")
	require.NoError(fx.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{senderID}, []string{"Alias User"}),
		"add alias From row")

	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "send-as@example.test", "manual"))
	matches, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{messageID})
	require.NoError(err)
	assert.Equal([]string{"send-as@example.test"}, matches[messageID].Sender,
		"email-typed identifier rows must badge case-insensitively")
}

// TestResolveAccountIdentityMatchesAttributionForPhoneIdentity guards parity
// with messageIdentityAttributionMatch: a phone identity resolves through the
// participant_identifiers row EnsureParticipantByPhone creates, not through
// participants.phone_number directly. If that row disappears (the
// MergeParticipants edge, simulated here with a direct delete) the resolver
// must stop matching, exactly as attribution would.
func TestResolveAccountIdentityMatchesAttributionForPhoneIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	const phone = "+15550100001"

	participantID, err := fx.Store.EnsureParticipantByPhone(phone, "Phone Owner", "whatsapp")
	require.NoError(err, "EnsureParticipantByPhone")
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, phone, "manual"))

	resolved, err := fx.Store.ResolveAccountIdentityContext(t.Context(), fx.Source.ID, phone)
	require.NoError(err)
	assert.Contains(resolved.ParticipantIDs, participantID,
		"participant_identifiers row for the phone must resolve, matching attribution")

	_, err = fx.Store.DB().Exec(
		fx.Store.Rebind("DELETE FROM participant_identifiers WHERE participant_id = ?"),
		participantID,
	)
	require.NoError(err, "simulate MergeParticipants edge: identifier row gone, phone_number still set")

	resolvedAfter, err := fx.Store.ResolveAccountIdentityContext(t.Context(), fx.Source.ID, phone)
	require.NoError(err)
	assert.NotContains(resolvedAfter.ParticipantIDs, participantID,
		"participants.phone_number alone must not resolve; attribution never consults it")
}

// TestResolveAccountIdentityNonEmailIdentifierIsCaseSensitive guards parity
// with messageIdentityAttributionMatch's per-row rule: a participant_identifiers
// row whose type is not "email" compares case-sensitively, based on the
// row's identifier_type column — not on whether the confirmed identity
// address happens to look email-shaped. The confirmed identity here is
// deliberately email-shaped (e.g. a chat bridge that issues email-looking
// handles) so the old global "does the searched string look like an email"
// rule would apply LOWER() everywhere and over-match a differently cased,
// non-email-typed row; the per-row rule must not.
func TestResolveAccountIdentityNonEmailIdentifierIsCaseSensitive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	exactCase := fx.EnsureParticipant("matrix-exact@example.test", "Matrix Exact", "example.test")
	require.NoError(fx.Store.SetParticipantIdentifier(exactCase, "matrix", "User@Example.Test"))
	differentCase := fx.EnsureParticipant("matrix-diff@example.test", "Matrix Diff", "example.test")
	require.NoError(fx.Store.SetParticipantIdentifier(differentCase, "matrix", "user@example.test"))

	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "User@Example.Test", "manual"))

	resolved, err := fx.Store.ResolveAccountIdentityContext(
		t.Context(), fx.Source.ID, "User@Example.Test")
	require.NoError(err)
	assert.Contains(resolved.ParticipantIDs, exactCase,
		"exact-case match against a non-email identifier row must resolve")
	assert.NotContains(resolved.ParticipantIDs, differentCase,
		"non-email identifier types compare case-sensitively even when the value looks email-shaped")
}

// TestResolveAccountIdentityEmailRemainsCaseInsensitive locks in existing
// behavior: participant_identifiers rows of type "email" keep comparing
// case-insensitively, same as attribution.
func TestResolveAccountIdentityEmailRemainsCaseInsensitive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	owner := fx.EnsureParticipant("owner@example.test", "Owner", "example.test")
	alias := fx.EnsureParticipant("alias@example.test", "Alias", "example.test")
	require.NoError(fx.Store.SetParticipantIdentifier(alias, "email", "User@Example.Com"))
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "user@example.com", "manual"))

	resolved, err := fx.Store.ResolveAccountIdentityContext(
		t.Context(), fx.Source.ID, "user@example.com")
	require.NoError(err)
	assert.Contains(resolved.ParticipantIDs, alias,
		"email identifier type must match case-insensitively")
	assert.NotContains(resolved.ParticipantIDs, owner,
		"unrelated participant must not resolve")
}

// persistEnvelopeMessage stores one email message through the production
// PersistMessage path with an envelope address snapshot on its single
// recipient row, mirroring what email ingest writes.
func persistEnvelopeMessage(
	t *testing.T,
	fx *storetest.Fixture,
	sourceMessageID, recipientType string,
	participantID int64,
	envelopeAddress string,
) int64 {
	t.Helper()
	message := &store.Message{
		ConversationID:  fx.ConvID,
		SourceID:        fx.Source.ID,
		SourceMessageID: sourceMessageID,
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC), Valid: true},
		SizeEstimate:    100,
	}
	if recipientType == "from" {
		message.SenderID = sql.NullInt64{Int64: participantID, Valid: true}
	}
	messageID, err := fx.Store.PersistMessage(&store.MessagePersistData{
		Message: message,
		Recipients: []store.RecipientSet{{
			Type:           recipientType,
			ParticipantIDs: []int64{participantID},
			DisplayNames:   []string{"Envelope Participant"},
			EmailAddresses: []string{envelopeAddress},
		}},
	})
	require.NoError(t, err, "persist envelope message %s", sourceMessageID)
	return messageID
}

// TestMatchMessageIdentitiesPrefersEnvelopeAddressAfterParticipantMerge is
// the badge-hydration half of envelope-accurate identity matching: after a
// participant merge repoints recipient rows onto the survivor, each message
// must keep badging the alias that actually appeared in its envelope, not
// every alias the merged participant now carries.
func TestMatchMessageIdentitiesPrefersEnvelopeAddressAfterParticipantMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	aliasA := fx.EnsureParticipant("alias-a@example.test", "Alias A", "example.test")
	aliasB := fx.EnsureParticipant("alias-b@example.test", "Alias B", "example.test")

	sentViaA := persistEnvelopeMessage(t, fx, "sent-via-a", "from", aliasA, "alias-a@example.test")
	sentViaB := persistEnvelopeMessage(t, fx, "sent-via-b", "from", aliasB, "alias-b@example.test")
	receivedAtA := persistEnvelopeMessage(t, fx, "received-at-a", "to", aliasA, "alias-a@example.test")

	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "alias-a@example.test", "manual"))
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "alias-b@example.test", "manual"))
	require.NoError(fx.Store.MergeParticipants(aliasA, aliasB), "merge alias A into alias B")

	matches, err := fx.Store.MatchMessageIdentitiesContext(
		t.Context(), []int64{sentViaA, sentViaB, receivedAtA})
	require.NoError(err)
	assert.Equal([]string{"alias-a@example.test"}, matches[sentViaA].Sender,
		"a merge must not rebadge alias A's sent mail as alias B")
	assert.Equal([]string{"alias-b@example.test"}, matches[sentViaB].Sender,
		"alias B's own sent mail keeps its envelope badge")
	assert.Equal([]string{"alias-a@example.test"}, matches[receivedAtA].Recipients,
		"a merge must not rebadge alias A's received mail as alias B")
}

// TestMatchMessageIdentitiesFallsBackToParticipantEmailWithoutEnvelope locks
// in the legacy contract: recipient rows written before the envelope snapshot
// existed (email_address NULL) keep matching through the current participant
// fields.
func TestMatchMessageIdentitiesFallsBackToParticipantEmailWithoutEnvelope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	legacy := fx.EnsureParticipant("legacy-owner@example.test", "Legacy", "example.test")
	message := fx.NewMessage().Build()
	message.SenderID = sql.NullInt64{Int64: legacy, Valid: true}
	messageID, err := fx.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(fx.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{legacy}, []string{"Legacy"}),
		"legacy write path leaves the envelope snapshot NULL")
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "Legacy-Owner@example.test", "manual"))

	matches, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{messageID})
	require.NoError(err)
	assert.Equal([]string{"Legacy-Owner@example.test"}, matches[messageID].Sender,
		"NULL-envelope rows must keep matching via the participant email")
}

// TestMatchMessageIdentitiesKeepsNonEmailIdentifierAfterParticipantMerge
// guards legacy and non-email recipient rows after an email participant
// absorbs a phone participant. The survivor's primary email suppresses
// alternate email aliases, but must not hide the preserved phone identifier.
func TestMatchMessageIdentitiesKeepsNonEmailIdentifierAfterParticipantMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	const (
		email = "owner@example.test"
		phone = "+15550100003"
	)

	emailParticipant := fx.EnsureParticipant(email, "Email Owner", "example.test")
	phoneParticipant, err := fx.Store.EnsureParticipantByPhone(phone, "Phone Owner", "whatsapp")
	require.NoError(err, "create phone participant")

	message := fx.NewMessage().WithSubject("merged email and phone identity").Build()
	message.SenderID = sql.NullInt64{Int64: emailParticipant, Valid: true}
	messageID, err := fx.Store.UpsertMessage(message)
	require.NoError(err, "create email-sender message")
	require.NoError(fx.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{emailParticipant}, []string{"Email Owner"}),
		"add legacy From row without an envelope snapshot")

	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, email, "manual"))
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, phone, "manual"))
	require.NoError(fx.Store.MergeParticipants(phoneParticipant, emailParticipant),
		"merge phone participant into email participant")

	matches, err := fx.Store.MatchMessageIdentitiesContext(t.Context(), []int64{messageID})
	require.NoError(err)
	assert.Equal([]string{phone, email}, matches[messageID].Sender,
		"the survivor must badge both its primary email and preserved phone identifier")
}

// TestResolveAccountIdentityContextReportsEmailIdentifierShape verifies the
// resolver classifies the stored identifier's shape so consumers know whether
// the envelope snapshot column applies (emails) or participant matching is
// the only surface (phones, matrix IDs, handles).
func TestResolveAccountIdentityContextReportsEmailIdentifierShape(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "shape-owner@example.test", "manual"))
	require.NoError(fx.Store.AddAccountIdentity(fx.Source.ID, "+15550100002", "manual"))

	email, err := fx.Store.ResolveAccountIdentityContext(
		t.Context(), fx.Source.ID, "shape-owner@example.test")
	require.NoError(err)
	assert.True(email.IdentifierIsEmail, "email-shaped identifier must be classified as email")

	phone, err := fx.Store.ResolveAccountIdentityContext(
		t.Context(), fx.Source.ID, "+15550100002")
	require.NoError(err)
	assert.False(phone.IdentifierIsEmail, "phone identifier must not be classified as email")
}

func TestGenericAPIMessagePathsDoNotHydrateIdentityMatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	from := fx.EnsureParticipant("STORED-SENDER@EXAMPLE.TEST", "Header Sender", "example.test")
	to := fx.EnsureParticipant("STORED-RECIPIENT@EXAMPLE.TEST", "Header To", "example.test")
	message := fx.NewMessage().WithSubject("identity hydration subject").Build()
	message.SenderID = sql.NullInt64{Int64: from, Valid: true}
	messageID, err := fx.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(fx.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{from}, []string{"Header Sender"}))
	require.NoError(fx.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{to}, []string{"Header To"}))
	require.NoError(fx.Store.AddAccountIdentity(
		fx.Source.ID, "Stored-Sender@Example.test", "manual"))
	require.NoError(fx.Store.AddAccountIdentity(
		fx.Source.ID, "stored-recipient@example.test", "manual"))
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true, MaxStmtChars: 10_000})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	var logBuffer bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	detail, err := fx.Store.GetMessage(messageID)
	require.NoError(err)
	assert.Equal("Header Sender <STORED-SENDER@EXAMPLE.TEST>", detail.From)
	assert.Equal([]string{"Header To <STORED-RECIPIENT@EXAMPLE.TEST>"}, detail.To)

	listed, total, err := fx.Store.ListMessages(0, 10)
	require.NoError(err)
	require.Equal(int64(1), total)
	require.Len(listed, 1)

	summaries, err := fx.Store.GetMessagesSummariesByIDs([]int64{messageID})
	require.NoError(err)
	require.Len(summaries, 1)

	searched, total, err := fx.Store.SearchMessagesQuery(
		&search.Query{SubjectTerms: []string{"identity hydration"}}, 0, 10)
	require.NoError(err)
	require.Equal(int64(1), total)
	require.Len(searched, 1)

	trace := strings.ToLower(logBuffer.String())
	assert.NotContains(trace, "account_identities",
		"generic message paths must leave identity matching to explicit API consumers")
}
