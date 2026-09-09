package store_test

import (
	"context"
	"database/sql"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type imapRelocationFixture struct {
	Store          *store.Store
	Scoped         *store.Store
	SourceID       int64
	ConversationID int64
	TargetID       int64
	Guard          store.MessageIdentityGuard
	OldLabelID     int64
	SentLabelID    int64
	SyncID         int64
}

func seedIMAPRelocationFixture(t *testing.T) imapRelocationFixture {
	t.Helper()
	return seedRelocationFixtureWithSourceType(t, "imap")
}

func seedRelocationFixtureWithSourceType(
	t *testing.T, sourceType string,
) imapRelocationFixture {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceType, "imap-relocation@example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(
		source.ID, "imap-relocation-thread", "Draft conversation")
	require.NoError(err)
	oldLabelID, err := st.EnsureLabel(source.ID, "DRAFT", "Drafts", "system")
	require.NoError(err)
	sentLabelID, err := st.EnsureLabel(source.ID, "SENT", "Sent", "system")
	require.NoError(err)

	targetID, err := st.PersistMessageWithParticipantsContext(
		t.Context(),
		[]store.ParticipantPersistData{
			{EmailAddress: "draft-author@example.test", DisplayName: "Draft Author", Domain: "example.test"},
			{EmailAddress: "draft-to@example.test", DisplayName: "Draft To", Domain: "example.test"},
			{EmailAddress: "draft-cc@example.test", DisplayName: "Draft Cc", Domain: "example.test"},
			{EmailAddress: "draft-bcc@example.test", DisplayName: "Draft Bcc", Domain: "example.test"},
		},
		func(participantIDs []int64) *store.MessagePersistData {
			return &store.MessagePersistData{
				Message: &store.Message{
					ConversationID:  conversationID,
					SourceID:        source.ID,
					SourceMessageID: "Drafts|1",
					RFC822MessageID: sql.NullString{String: "<relocation@example.test>", Valid: true},
					MessageType:     "email",
					SenderID:        sql.NullInt64{Int64: participantIDs[0], Valid: true},
					Subject:         sql.NullString{String: "Draft subject", Valid: true},
					Snippet:         sql.NullString{String: "Draft snippet", Valid: true},
					SizeEstimate:    101,
				},
				BodyText: sql.NullString{String: "draftword", Valid: true},
				BodyHTML: sql.NullString{String: "<p>draftword</p>", Valid: true},
				RawMIME:  []byte("literal draft MIME"),
				Recipients: []store.RecipientSet{
					{Type: "from", ParticipantIDs: participantIDs[0:1], DisplayNames: []string{"Draft Author"}, EmailAddresses: []string{"draft-author@example.test"}},
					{Type: "to", ParticipantIDs: participantIDs[1:2], DisplayNames: []string{"Draft To"}, EmailAddresses: []string{"draft-to@example.test"}},
					{Type: "cc", ParticipantIDs: participantIDs[2:3], DisplayNames: []string{"Draft Cc"}, EmailAddresses: []string{"draft-cc@example.test"}},
					{Type: "bcc", ParticipantIDs: participantIDs[3:4], DisplayNames: []string{"Draft Bcc"}, EmailAddresses: []string{"draft-bcc@example.test"}},
				},
				LabelIDs: []int64{oldLabelID},
				FTS: &store.FTSDoc{
					Subject:  "Draft subject",
					Body:     "draftword",
					FromAddr: "draft-author@example.test",
					ToAddrs:  "draft-to@example.test",
					CcAddrs:  "draft-cc@example.test",
				},
			}
		},
	)
	require.NoError(err)
	require.NoError(st.UpsertAttachmentRecord(t.Context(), targetID, store.AttachmentWrite{
		Filename: "draft.txt", MIMEType: "text/plain", StoragePath: "draft.txt",
		ContentHash: "draft-hash", Size: 11, SourcePartKey: "mime:part:1",
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
	}))
	require.NoError(st.UpsertAttachmentRecord(t.Context(), targetID, store.AttachmentWrite{
		Filename: "provider.bin", MIMEType: "application/octet-stream", StoragePath: "provider.bin",
		ContentHash: "provider-hash", Size: 12, SourceAttachmentID: "provider:attachment:1",
		SourcePartKey: "provider:part:1", Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceProviderExplicit,
	}))
	require.NoError(st.RecomputeMessageAttachmentStats(targetID))
	require.NoError(st.SetEmbedGen(t.Context(), []int64{targetID}, 19))
	syncID, err := st.StartSync(source.ID, "full")
	require.NoError(err)

	return imapRelocationFixture{
		Store: st, Scoped: st.ScopedToSync(source.ID, syncID),
		SourceID: source.ID, ConversationID: conversationID, TargetID: targetID,
		Guard: store.MessageIdentityGuard{
			ID: targetID, SourceID: source.ID, SourceMessageID: "Drafts|1",
		},
		OldLabelID: oldLabelID, SentLabelID: sentLabelID, SyncID: syncID,
	}
}

func relocatedIMAPMessageData(
	fixture imapRelocationFixture, participantIDs []int64,
) *store.MessagePersistData {
	emptyReplacement := []store.AttachmentWrite{}
	return &store.MessagePersistData{
		Message: &store.Message{
			ConversationID:  fixture.ConversationID,
			SourceID:        fixture.SourceID,
			SourceMessageID: "Sent|2",
			RFC822MessageID: sql.NullString{String: "relocation@example.test", Valid: true},
			MessageType:     "email",
			SenderID:        sql.NullInt64{Int64: participantIDs[0], Valid: true},
			Subject:         sql.NullString{String: "Sent subject", Valid: true},
			Snippet:         sql.NullString{String: "Sent snippet", Valid: true},
			SizeEstimate:    202,
		},
		BodyText: sql.NullString{String: "finalword", Valid: true},
		BodyHTML: sql.NullString{String: "<p>finalword</p>", Valid: true},
		RawMIME:  []byte("literal sent MIME"),
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: participantIDs[0:1], DisplayNames: []string{"Sent Author"}, EmailAddresses: []string{"sent-author@example.test"}},
			{Type: "to", ParticipantIDs: participantIDs[1:2], DisplayNames: []string{"Sent To"}, EmailAddresses: []string{"sent-to@example.test"}},
			{Type: "cc", ParticipantIDs: []int64{}, DisplayNames: []string{}, EmailAddresses: []string{}},
			{Type: "bcc", ParticipantIDs: []int64{}, DisplayNames: []string{}, EmailAddresses: []string{}},
		},
		LabelIDs:                  []int64{fixture.SentLabelID},
		MIMEAttachmentReplacement: &emptyReplacement,
		FTS: &store.FTSDoc{
			Subject:  "Sent subject",
			Body:     "finalword",
			FromAddr: "sent-author@example.test",
			ToAddrs:  "sent-to@example.test",
		},
	}
}

func imapRelocationParticipants() []store.ParticipantPersistData {
	return []store.ParticipantPersistData{
		{EmailAddress: "sent-author@example.test", DisplayName: "Sent Author", Domain: "example.test"},
		{EmailAddress: "sent-to@example.test", DisplayName: "Sent To", Domain: "example.test"},
	}
}

func TestPersistIMAPRelocationReplacesCompleteSnapshotAtomically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := seedIMAPRelocationFixture(t)

	gotID, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
		t.Context(), fixture.Guard, imapRelocationParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			return relocatedIMAPMessageData(fixture, participantIDs)
		}, true,
	)
	require.NoError(err)
	assert.Equal(fixture.Guard.ID, gotID)

	after := readSiblingMessageSnapshot(t, fixture.Store, gotID)
	assert.Equal("Sent|2", after.SourceMessageID)
	assert.Equal("Sent subject", after.Subject.String)
	assert.Equal("Sent snippet", after.Snippet.String)
	assert.Equal("finalword", after.BodyText.String)
	assert.Equal("<p>finalword</p>", after.BodyHTML.String)
	assert.Equal([]byte("literal sent MIME"), after.RawMIME)
	assert.Equal([]string{"provider.bin"}, attachmentFilenames(after))
	assert.True(after.HasAttachments)
	assert.Equal(1, after.AttachmentCount)
	assert.False(after.EmbedGen.Valid)
	assert.Equal([]int64{fixture.SentLabelID}, after.LabelIDs)
	require.Len(after.Recipients, 2)
	assert.Equal("from", after.Recipients[0].Type)
	assert.Equal("sent-author@example.test", after.Recipients[0].EnvelopeEmail.String)
	assert.Equal("to", after.Recipients[1].Type)
	assert.Equal("sent-to@example.test", after.Recipients[1].EnvelopeEmail.String)

	_, finalHits, err := fixture.Store.SearchMessages("finalword", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), finalHits)
	_, draftHits, err := fixture.Store.SearchMessages("draftword", 0, 10)
	require.NoError(err)
	assert.Zero(draftHits)
}

func TestPersistIMAPRelocationRequiresGuardedIMAPSyncScope(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (imapRelocationFixture, *store.Store, store.MessageIdentityGuard)
		want  string
	}{
		{
			name: "wrong old source identity",
			setup: func(t *testing.T) (imapRelocationFixture, *store.Store, store.MessageIdentityGuard) {
				t.Helper()
				fixture := seedIMAPRelocationFixture(t)
				guard := fixture.Guard
				guard.SourceMessageID = "Drafts|wrong"
				return fixture, fixture.Scoped, guard
			},
			want: "identity guard",
		},
		{
			name: "wrong sync source owner",
			setup: func(t *testing.T) (imapRelocationFixture, *store.Store, store.MessageIdentityGuard) {
				t.Helper()
				fixture := seedIMAPRelocationFixture(t)
				other, err := fixture.Store.GetOrCreateSource("imap", "other-relocation@example.test")
				require.NoError(t, err)
				runID, err := fixture.Store.StartSync(other.ID, "full")
				require.NoError(t, err)
				return fixture, fixture.Store.ScopedToSync(other.ID, runID), fixture.Guard
			},
			want: "scoped to source",
		},
		{
			name: "unscoped store",
			setup: func(t *testing.T) (imapRelocationFixture, *store.Store, store.MessageIdentityGuard) {
				t.Helper()
				fixture := seedIMAPRelocationFixture(t)
				return fixture, fixture.Store, fixture.Guard
			},
			want: "requires a sync generation",
		},
		{
			name: "non IMAP source",
			setup: func(t *testing.T) (imapRelocationFixture, *store.Store, store.MessageIdentityGuard) {
				t.Helper()
				fixture := seedRelocationFixtureWithSourceType(t, "gmail")
				return fixture, fixture.Scoped, fixture.Guard
			},
			want: "requires an IMAP source",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture, scoped, guard := test.setup(t)
			before := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
			var built atomic.Bool
			_, err := scoped.PersistIMAPRelocationWithParticipantsContext(
				t.Context(), guard, imapRelocationParticipants(),
				func(participantIDs []int64) *store.MessagePersistData {
					built.Store(true)
					return relocatedIMAPMessageData(fixture, participantIDs)
				}, true,
			)
			require.ErrorContains(t, err, test.want)
			assert.False(t, built.Load(), "preflight rejection must precede participant resolution and building")
			assert.Equal(t, before, readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID))
		})
	}
}

func TestPersistIMAPRelocationRejectsSupersededSyncGeneration(t *testing.T) {
	fixture := seedIMAPRelocationFixture(t)
	require.NoError(t, fixture.Store.CompleteSync(fixture.SyncID, "complete"))
	var built atomic.Bool

	_, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
		t.Context(), fixture.Guard, imapRelocationParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			built.Store(true)
			return relocatedIMAPMessageData(fixture, participantIDs)
		}, true,
	)
	require.ErrorIs(t, err, store.ErrSyncRunSuperseded)
	assert.False(t, built.Load())
}

func TestPersistIMAPRelocationRejectsDifferentRFC822Identity(t *testing.T) {
	fixture := seedIMAPRelocationFixture(t)
	before := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)

	_, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
		t.Context(), fixture.Guard, imapRelocationParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			data := relocatedIMAPMessageData(fixture, participantIDs)
			data.Message.RFC822MessageID = sql.NullString{String: "<different@example.test>", Valid: true}
			return data
		}, true,
	)
	require.ErrorContains(t, err, "RFC822 identity mismatch")
	assert.Equal(t, before, readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID))
}

func TestPersistIMAPRelocationRequiresCompleteSnapshotPayload(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.MessagePersistData)
		want   string
	}{
		{
			name: "snapshot source",
			mutate: func(data *store.MessagePersistData) {
				data.Message.SourceID++
			},
			want: "snapshot source mismatch",
		},
		{
			name: "new source message ID",
			mutate: func(data *store.MessagePersistData) {
				data.Message.SourceMessageID = ""
			},
			want: "requires a source message ID",
		},
		{
			name: "changed source message ID",
			mutate: func(data *store.MessagePersistData) {
				data.Message.SourceMessageID = "Drafts|1"
			},
			want: "must change",
		},
		{
			name: "raw MIME",
			mutate: func(data *store.MessagePersistData) {
				data.RawMIME = nil
			},
			want: "requires raw MIME",
		},
		{
			name: "FTS",
			mutate: func(data *store.MessagePersistData) {
				data.FTS = nil
			},
			want: "requires FTS content",
		},
		{
			name: "MIME attachment replacement",
			mutate: func(data *store.MessagePersistData) {
				data.MIMEAttachmentReplacement = nil
			},
			want: "requires MIME attachment replacement",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := seedIMAPRelocationFixture(t)
			before := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
			_, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
				t.Context(), fixture.Guard, imapRelocationParticipants(),
				func(participantIDs []int64) *store.MessagePersistData {
					data := relocatedIMAPMessageData(fixture, participantIDs)
					test.mutate(data)
					return data
				}, true,
			)
			require.ErrorContains(t, err, test.want)
			assert.Equal(t, before, readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID))
		})
	}
}

func TestPersistIMAPRelocationRequiresExactlyOneCompleteRecipientSetPerRole(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.MessagePersistData)
		want   string
	}{
		{
			name: "missing role",
			mutate: func(data *store.MessagePersistData) {
				data.Recipients = data.Recipients[:3]
			},
			want: "exactly one bcc recipient set",
		},
		{
			name: "duplicate role",
			mutate: func(data *store.MessagePersistData) {
				data.Recipients = append(data.Recipients, store.RecipientSet{Type: "from"})
			},
			want: "exactly one from recipient set",
		},
		{
			name: "unexpected role",
			mutate: func(data *store.MessagePersistData) {
				data.Recipients[3].Type = "reply-to"
			},
			want: "unexpected recipient role",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := seedIMAPRelocationFixture(t)
			before := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)
			_, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
				t.Context(), fixture.Guard, imapRelocationParticipants(),
				func(participantIDs []int64) *store.MessagePersistData {
					data := relocatedIMAPMessageData(fixture, participantIDs)
					test.mutate(data)
					return data
				}, true,
			)
			require.ErrorContains(t, err, test.want)
			assert.Equal(t, before, readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID))
		})
	}
}

func TestPersistIMAPRelocationAppliesLabelPolicy(t *testing.T) {
	tests := []struct {
		name           string
		replaceLabels  bool
		preserveLabels bool
		want           func(imapRelocationFixture) []int64
	}{
		{
			name: "complete replaces", replaceLabels: true,
			want: func(fixture imapRelocationFixture) []int64 { return []int64{fixture.SentLabelID} },
		},
		{
			name: "partial merges", replaceLabels: false,
			want: func(fixture imapRelocationFixture) []int64 {
				return []int64{fixture.OldLabelID, fixture.SentLabelID}
			},
		},
		{
			name: "deferred preserves", replaceLabels: true, preserveLabels: true,
			want: func(fixture imapRelocationFixture) []int64 { return []int64{fixture.OldLabelID} },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := seedIMAPRelocationFixture(t)
			_, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
				t.Context(), fixture.Guard, imapRelocationParticipants(),
				func(participantIDs []int64) *store.MessagePersistData {
					data := relocatedIMAPMessageData(fixture, participantIDs)
					data.PreserveLabels = test.preserveLabels
					return data
				}, test.replaceLabels,
			)
			require.NoError(t, err)
			assert.Equal(t, test.want(fixture),
				readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID).LabelIDs)
		})
	}
}

func TestPersistIMAPRelocationRejectsNewSourceIDCollisionWithoutChangingEitherRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := seedIMAPRelocationFixture(t)
	collisionID, err := fixture.Store.UpsertMessage(&store.Message{
		ConversationID: fixture.ConversationID, SourceID: fixture.SourceID,
		SourceMessageID: "Sent|2", MessageType: "email",
		Subject: sql.NullString{String: "Collision", Valid: true},
	})
	require.NoError(err)
	require.NotEqual(fixture.TargetID, collisionID)
	beforeTarget := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)

	_, err = fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
		t.Context(), fixture.Guard, imapRelocationParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			return relocatedIMAPMessageData(fixture, participantIDs)
		}, true,
	)
	require.ErrorContains(err, "rekey IMAP relocation target")
	assert.Equal(beforeTarget, readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID))
	collisionSourceID, sourceErr := fixture.Store.GetMessageSourceID(collisionID)
	require.NoError(sourceErr)
	assert.Equal("Sent|2", collisionSourceID)
}

func TestPersistIMAPRelocationHonorsCanceledContext(t *testing.T) {
	fixture := seedIMAPRelocationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var built atomic.Bool

	_, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
		ctx, fixture.Guard, imapRelocationParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			built.Store(true)
			return relocatedIMAPMessageData(fixture, participantIDs)
		}, true,
	)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, built.Load())
}

func TestPersistIMAPRelocationRollsBackEveryMutationOnLateLabelFailure(t *testing.T) {
	fixture := seedIMAPRelocationFixture(t)
	before := readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID)

	_, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
		t.Context(), fixture.Guard, imapRelocationParticipants(),
		func(participantIDs []int64) *store.MessagePersistData {
			data := relocatedIMAPMessageData(fixture, participantIDs)
			data.LabelIDs = []int64{fixture.SentLabelID, 1 << 60}
			return data
		}, true,
	)
	require.ErrorContains(t, err, "reconcile IMAP relocation labels")
	assert.Equal(t, before, readSiblingMessageSnapshot(t, fixture.Store, fixture.TargetID),
		"late label failure must roll back source, content, FTS, recipients, attachments, and labels")
}

func TestPersistIMAPRelocationDoesNotMutateCallerData(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := seedIMAPRelocationFixture(t)
	participantIDs := make([]int64, 2)
	for index, participant := range imapRelocationParticipants() {
		id, err := fixture.Store.EnsureParticipant(
			participant.EmailAddress, participant.DisplayName, participant.Domain)
		require.NoError(err)
		participantIDs[index] = id
	}
	data := relocatedIMAPMessageData(fixture, participantIDs)
	data.Message.ID = 987654
	wantMessage := *data.Message
	wantRaw := slices.Clone(data.RawMIME)
	wantRecipients := make([]store.RecipientSet, len(data.Recipients))
	for index, recipient := range data.Recipients {
		wantRecipients[index] = recipient
		wantRecipients[index].ParticipantIDs = slices.Clone(recipient.ParticipantIDs)
		wantRecipients[index].DisplayNames = slices.Clone(recipient.DisplayNames)
		wantRecipients[index].EmailAddresses = slices.Clone(recipient.EmailAddresses)
	}
	wantLabels := slices.Clone(data.LabelIDs)
	wantAttachments := slices.Clone(*data.MIMEAttachmentReplacement)
	wantFTS := *data.FTS

	gotID, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
		t.Context(), fixture.Guard, nil,
		func([]int64) *store.MessagePersistData { return data }, true,
	)
	require.NoError(err)
	assert.Equal(fixture.TargetID, gotID)
	assert.Equal(wantMessage, *data.Message)
	assert.Equal(wantRaw, data.RawMIME)
	assert.Equal(wantRecipients, data.Recipients)
	assert.Equal(wantLabels, data.LabelIDs)
	assert.Equal(wantAttachments, *data.MIMEAttachmentReplacement)
	assert.Equal(wantFTS, *data.FTS)
}

func TestPersistIMAPRelocationRequiresBuilder(t *testing.T) {
	fixture := seedIMAPRelocationFixture(t)
	_, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
		t.Context(), fixture.Guard, nil, nil, true)
	require.ErrorContains(t, err, "requires a participant builder")
}
