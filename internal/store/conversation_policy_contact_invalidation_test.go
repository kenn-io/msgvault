package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCuratedContactPointMutationRefreshesParticipantProjectionAndContext(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := testutil.NewTestStore(t)
	ctx := t.Context()
	const email = "curated-contact@example.test"

	source, err := s.GetOrCreateSource("beeper", "curated-contact-invalidation")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "full-room", "Full room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(ctx, source.ID, "full-room", conversationpolicy.Full))
	participantID, err := s.EnsureParticipantContext(ctx, email, "", "example.test")
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipantContext(ctx, room, participantID, "member"))
	person, _, err := s.CreatePersonFromParticipantContext(ctx, participantID)
	requirements.NoError(err)
	messageID, err := s.UpsertMessageContext(ctx, &store.Message{
		SourceID: source.ID, ConversationID: room,
		SourceMessageID: "curated-contact-context", MessageType: "beeper",
	})
	requirements.NoError(err)
	requirements.NoError(s.SetEmbedGen(ctx, []int64{messageID}, 73))

	eligible := func() bool {
		var value bool
		err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT `+
			store.FullParticipantValueWhere("p.id", "'email'", "p.email_address")+
			` FROM participants p WHERE p.id=?`), participantID).Scan(&value)
		requirements.NoError(err)
		return value
	}
	display := func() string {
		var value string
		err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT `+
			store.FullParticipantDisplaySQL("p")+
			` FROM participants p WHERE p.id=?`), participantID).Scan(&value)
		requirements.NoError(err)
		return value
	}
	embedding := func() (sql.NullInt64, int64) {
		var generation sql.NullInt64
		var revision int64
		err := s.DB().QueryRowContext(ctx, s.Rebind(
			`SELECT embed_gen,embedding_revision FROM messages WHERE id=?`), messageID).Scan(&generation, &revision)
		requirements.NoError(err)
		return generation, revision
	}

	assertions.False(eligible(), "the chat-derived email has no Full or curated support yet")
	assertions.Empty(display(), "the unsupported email must stay out of participant projections")
	participantRevisionBefore, err := s.ParticipantDisplayNameRevisionContext(ctx)
	requirements.NoError(err)
	_, embeddingRevisionBefore := embedding()

	point, err := s.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: email,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	assertions.True(eligible(), "curated contact evidence makes the linked participant email eligible")
	assertions.Equal(email, display(), "the newly eligible email must reach participant projections")
	participantRevisionAfterAdd, err := s.ParticipantDisplayNameRevisionContext(ctx)
	requirements.NoError(err)
	assertions.Greater(participantRevisionAfterAdd, participantRevisionBefore,
		"derived participant projections must observe curated contact support changes")
	generation, embeddingRevisionAfterAdd := embedding()
	assertions.False(generation.Valid, "cached contextual embeddings with changed participant values must be withdrawn")
	assertions.Greater(embeddingRevisionAfterAdd, embeddingRevisionBefore,
		"in-flight contextual publication must fail after curated contact support changes")

	requirements.NoError(s.SetEmbedGen(ctx, []int64{messageID}, 74))
	_, embeddingRevisionBefore = embedding()
	participantRevisionBefore = participantRevisionAfterAdd
	requirements.NoError(s.SupersedePersonContactPointContext(ctx, person.ID, point.Envelope.ID, nil))
	assertions.False(eligible(), "superseding curated support must make the chat-only email ineligible again")
	assertions.Empty(display(), "removed curated support must disappear from participant projections")
	participantRevisionAfterSupersede, err := s.ParticipantDisplayNameRevisionContext(ctx)
	requirements.NoError(err)
	assertions.Greater(participantRevisionAfterSupersede, participantRevisionBefore,
		"participant projection revisions must advance when curated support is withdrawn")
	generation, embeddingRevisionAfterSupersede := embedding()
	assertions.False(generation.Valid, "cached contextual embeddings with withdrawn values must be withdrawn")
	assertions.Greater(embeddingRevisionAfterSupersede, embeddingRevisionBefore,
		"in-flight contextual publication must fail after curated support is withdrawn")
}

func TestObservationEligibilityChangeInvalidatesContextualParticipantMessages(t *testing.T) {
	for _, scenario := range []string{"first Full room support", "native name evidence"} {
		t.Run(scenario, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s := testutil.NewTestStore(t)
			ctx := t.Context()
			chatSource, err := s.GetOrCreateSource("beeper", "observation-context-invalidation")
			requirements.NoError(err)
			contextRoom, err := s.EnsureConversation(chatSource.ID, "context", "Context")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(ctx, chatSource.ID, "context", conversationpolicy.Full))
			participantID, err := s.EnsureParticipantByIdentifier(
				"beeper", "synthetic-observation-context-user", "Synthetic Observation Context")
			requirements.NoError(err)
			messageID, err := s.UpsertMessageContext(ctx, &store.Message{
				SourceID: chatSource.ID, ConversationID: contextRoom,
				SourceMessageID: "observation-context", MessageType: "beeper",
				SenderID: sql.NullInt64{Int64: participantID, Valid: true},
			})
			requirements.NoError(err)
			requirements.NoError(s.SetEmbedGen(ctx, []int64{messageID}, 73))

			var beforeRevision int64
			requirements.NoError(s.DB().QueryRowContext(ctx, s.Rebind(
				`SELECT embedding_revision FROM messages WHERE id=?`), messageID).Scan(&beforeRevision))
			var eligible bool
			requirements.NoError(s.DB().QueryRowContext(ctx, s.Rebind(`SELECT `+
				store.FullParticipantNameWhere("p.id", "p.display_name")+
				` FROM participants p WHERE p.id=?`), participantID).Scan(&eligible))
			assertions.False(eligible, "the participant name has no support before the observation change")

			input := store.ParticipantContactObservationInput{
				SourceID: &chatSource.ID, AddressKind: store.ContactAddressDisplayName,
				OriginalValue: "Synthetic Observation Context",
				Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
			}
			if scenario == "first Full room support" {
				coldRoom, err := s.EnsureConversation(chatSource.ID, "cold-support", "Cold support")
				requirements.NoError(err)
				requirements.NoError(s.SetConversationPolicyOverride(ctx, chatSource.ID, "cold-support", conversationpolicy.Cold))
				input.ConversationID = &coldRoom
				_, err = s.RecordContactObservationContext(ctx, participantID, input)
				requirements.NoError(err)
				input.ConversationID = &contextRoom
			} else {
				nativeSource, err := s.GetOrCreateSource("whatsapp", "native-observation-context")
				requirements.NoError(err)
				input.SourceID = &nativeSource.ID
			}
			_, err = s.RecordContactObservationContext(ctx, participantID, input)
			requirements.NoError(err)

			requirements.NoError(s.DB().QueryRowContext(ctx, s.Rebind(`SELECT `+
				store.FullParticipantNameWhere("p.id", "p.display_name")+
				` FROM participants p WHERE p.id=?`), participantID).Scan(&eligible))
			assertions.True(eligible, "the new Full or native observation makes the display name eligible")
			var generation sql.NullInt64
			var afterRevision int64
			requirements.NoError(s.DB().QueryRowContext(ctx, s.Rebind(
				`SELECT embed_gen,embedding_revision FROM messages WHERE id=?`), messageID).Scan(&generation, &afterRevision))
			assertions.False(generation.Valid, "cached context must be withdrawn when its participant inputs change")
			assertions.Greater(afterRevision, beforeRevision, "in-flight contextual publication must fail its revision check")
		})
	}
}

func TestPersonProfileContactPointPatchRefreshesParticipantProjectionAndContext(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := testutil.NewTestStore(t)
	ctx := t.Context()
	const email = "profile-patch-contact@example.test"

	source, err := s.GetOrCreateSource("beeper", "profile-patch-contact-invalidation")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "full-room", "Full room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(ctx, source.ID, "full-room", conversationpolicy.Full))
	participantID, err := s.EnsureParticipantContext(ctx, email, "", "example.test")
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipantContext(ctx, room, participantID, "member"))
	person, _, err := s.CreatePersonFromParticipantContext(ctx, participantID)
	requirements.NoError(err)
	messageID, err := s.UpsertMessageContext(ctx, &store.Message{
		SourceID: source.ID, ConversationID: room,
		SourceMessageID: "profile-patch-contact-context", MessageType: "beeper",
	})
	requirements.NoError(err)
	requirements.NoError(s.SetEmbedGen(ctx, []int64{messageID}, 73))

	profile, err := s.GetPersonProfileContext(ctx, person.ID)
	requirements.NoError(err)
	participantRevisionBefore, err := s.ParticipantDisplayNameRevisionContext(ctx)
	requirements.NoError(err)
	var embeddingRevisionBefore int64
	requirements.NoError(s.DB().QueryRowContext(ctx, s.Rebind(
		`SELECT embedding_revision FROM messages WHERE id=?`), messageID).Scan(&embeddingRevisionBefore))

	_, err = s.ApplyPersonProfilePatchContext(ctx, person.ID, profile.Person.Revision,
		store.PersonProfilePatch{ContactPoints: &store.PersonContactPointPatch{Add: []store.PersonContactPointInput{{
			AddressKind: store.ContactAddressEmail, OriginalValue: email,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		}}}})
	requirements.NoError(err)

	participantRevisionAfter, err := s.ParticipantDisplayNameRevisionContext(ctx)
	requirements.NoError(err)
	assertions.Greater(participantRevisionAfter, participantRevisionBefore,
		"profile patch contact changes must refresh linked participant projections")
	var generation sql.NullInt64
	var embeddingRevisionAfter int64
	requirements.NoError(s.DB().QueryRowContext(ctx, s.Rebind(
		`SELECT embed_gen,embedding_revision FROM messages WHERE id=?`), messageID).Scan(&generation, &embeddingRevisionAfter))
	assertions.False(generation.Valid, "profile patch contact changes must withdraw cached contextual embeddings")
	assertions.Greater(embeddingRevisionAfter, embeddingRevisionBefore,
		"in-flight contextual publication must fail after profile patch contact changes")
}

func TestCardDAVContactPointRetirementRefreshesParticipantProjectionAndContext(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, account, book := newCardDAVResourceStore(t)
	ctx := t.Context()
	href := book.CanonicalURL + "contact-invalidation.vcf"
	const email = "carddav-retired-contact@example.test"

	initial := remoteResource(href, "remote-contact-invalidation", "Synthetic Contact", email, `"one"`)
	_, err := s.ApplyCardDAVSyncPlanContext(ctx, store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{initial},
	})
	requirements.NoError(err)
	resource, err := s.GetCardDAVResourceContext(ctx, book.ID, href)
	requirements.NoError(err)
	requirements.NotNil(resource.PersonID)

	source, err := s.GetOrCreateSource("beeper", "carddav-contact-invalidation")
	requirements.NoError(err)
	participantID, err := s.EnsureParticipantContext(ctx, email, "", "example.test")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "contact-room", "Contact room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(ctx, source.ID, "contact-room", conversationpolicy.Full))
	requirements.NoError(s.EnsureConversationParticipantContext(ctx, room, participantID, "member"))
	_, err = s.DB().ExecContext(ctx, s.Rebind(
		`INSERT INTO person_participants (person_id, participant_id) VALUES (?, ?)`),
		*resource.PersonID, participantID)
	requirements.NoError(err)
	messageID, err := s.UpsertMessageContext(ctx, &store.Message{
		SourceID: source.ID, ConversationID: room,
		SourceMessageID: "carddav-retired-contact-context", MessageType: "beeper",
	})
	requirements.NoError(err)
	requirements.NoError(s.SetEmbedGen(ctx, []int64{messageID}, 73))

	eligible := func() bool {
		var value bool
		err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT `+
			store.FullParticipantValueWhere("p.id", "'email'", "p.email_address")+
			` FROM participants p WHERE p.id=?`), participantID).Scan(&value)
		requirements.NoError(err)
		return value
	}
	assertions.True(eligible(), "the imported contact currently supports the participant email")
	participantRevisionBefore, err := s.ParticipantDisplayNameRevisionContext(ctx)
	requirements.NoError(err)
	var embeddingRevisionBefore int64
	requirements.NoError(s.DB().QueryRowContext(ctx, s.Rebind(
		`SELECT embedding_revision FROM messages WHERE id=?`), messageID).Scan(&embeddingRevisionBefore))
	_, _, err = s.ReplaceCardDAVDiscoveryContext(ctx, store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts-new.example/dav", Username: "alice",
		PrincipalURL: "https://contacts-new.example/principal/alice/",
		HomeURL:      "https://contacts-new.example/books/alice/",
	})
	requirements.NoError(err)
	assertions.False(eligible(), "retiring the imported contact must withdraw participant support")
	participantRevisionAfter, err := s.ParticipantDisplayNameRevisionContext(ctx)
	requirements.NoError(err)
	assertions.Greater(participantRevisionAfter, participantRevisionBefore,
		"CardDAV contact retirement must refresh linked participant projections")
	var generation sql.NullInt64
	var embeddingRevisionAfter int64
	requirements.NoError(s.DB().QueryRowContext(ctx, s.Rebind(
		`SELECT embed_gen,embedding_revision FROM messages WHERE id=?`), messageID).Scan(&generation, &embeddingRevisionAfter))
	assertions.False(generation.Valid, "CardDAV contact retirement must withdraw cached contextual embeddings")
	assertions.Greater(embeddingRevisionAfter, embeddingRevisionBefore,
		"CardDAV contact retirement must invalidate in-flight contextual publication")
}
