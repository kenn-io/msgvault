package store

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

// observeIdentitySupportWriter exercises an actual native writer while a
// separate transaction holds the authorization fence. PostgreSQL lock state
// proves ordering without relying on how quickly the runner schedules work.
func observeIdentitySupportWriter(t *testing.T, st *Store, write func() error) {
	t.Helper()
	tx, err := st.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, st.lockIdentityMutationTxContext(t.Context(), tx))
	var blockerPID int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPID))
	done := make(chan error, 1)
	go func() { done <- write() }()
	completed := false
	var writeErr error
	const transitionBudget = 15 * time.Second
	assert.Eventually(t, func() bool {
		select {
		case writeErr = <-done:
			completed = true
			return true
		default:
		}
		var waiting bool
		err := st.db.QueryRowContext(t.Context(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND ? = ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&waiting)
		return err == nil && waiting
	}, transitionBudget, 10*time.Millisecond)
	assert.False(t, completed, "native source-support writer must wait for the authorization fence")
	if !completed {
		var writerPID int
		require.NoError(t, st.db.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND ? = ANY(pg_blocking_pids(pid)) LIMIT 1`, blockerPID).Scan(&writerPID))
		var priorLocks int
		require.NoError(t, st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pg_locks l
			JOIN pg_class c ON c.oid = l.relation
			WHERE l.pid = ? AND l.granted AND l.mode IN ('RowExclusiveLock', 'RowShareLock')
			AND c.relname IN ('sources', 'carddav_accounts', 'carddav_address_books',
			'carddav_resources', 'carddav_publications', 'carddav_conflicts', 'persons', 'sync_runs',
			'messages', 'message_recipients', 'participant_identifiers', 'participant_contact_observations')`, writerPID).Scan(&priorLocks))
		assert.Zero(t, priorLocks, "identity fence must precede source/account/book/person row locks and writes")
		var advisoryLocks int
		require.NoError(t, st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pg_locks
			WHERE pid = ? AND locktype = 'advisory' AND granted`, writerPID).Scan(&advisoryLocks))
		assert.Zero(t, advisoryLocks, "identity fence must precede person-fact generation and target locks")
	}
	require.NoError(t, tx.Commit())
	if !completed {
		select {
		case writeErr = <-done:
		case <-time.After(transitionBudget):
			require.FailNow(t, "source-support writer did not finish after releasing the fence")
		}
	}
	require.NoError(t, writeErr)
}

func TestIdentityScopeBoundNativeWritersSerialize(t *testing.T) {
	requirements := require.New(t)

	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	source, err := st.GetOrCreateSource("carddav", "bound-owner@example.test")
	requirements.NoError(err)
	run, err := st.StartSync(source.ID, "full")
	requirements.NoError(err)
	bound := st.ScopedToSync(source.ID, run)
	t.Run("source", func(t *testing.T) {
		observeIdentitySupportWriter(t, st, func() error {
			_, err := bound.GetOrCreateSourceContext(t.Context(), source.SourceType, source.Identifier)
			return err
		})
	})
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), CardDAVDiscoveryInput{
		BaseURL: "https://bound.example.test/dav", Username: "synthetic-owner",
		PrincipalURL: "https://bound.example.test/principal/", HomeURL: "https://bound.example.test/books/",
		Books: []CardDAVDiscoveredBook{{CanonicalURL: "https://bound.example.test/books/synthetic/", DisplayName: "Synthetic Book", CanCreate: &allowed}},
	})
	requirements.NoError(err)
	requirements.Len(books, 1)
	requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[0].ID, CardDAVBookRoles{IsSubscribed: true}))
	books, err = st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
	requirements.NoError(err)
	requirements.Len(books, 1)
	t.Run("CardDAV sync", func(t *testing.T) {
		observeIdentitySupportWriter(t, st, func() error {
			_, err := bound.ApplyCardDAVSyncPlanContext(t.Context(), CardDAVSyncPlan{
				AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration,
				SyncRevision: books[0].SyncRevision, NextSyncToken: "synthetic-bound-token",
			})
			return err
		})
	})
	participant, err := st.EnsureParticipant("bound-person@example.test", "Synthetic Bound Person", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), AttributeDefinitionInput{
		UniversalID: "synthetic-bound-text", ObjectType: AttributeObjectPerson, Slug: "synthetic_bound_text", Label: "Synthetic Bound Text",
		ValueType: AttributeValueText, FieldType: AttributeFieldText, Cardinality: AttributeCardinalitySingle,
		Ownership: AttributeOwnershipUser, APIMutable: true, IsDeletable: true,
	})
	requirements.NoError(err)
	t.Run("person attribute", func(t *testing.T) {
		observeIdentitySupportWriter(t, st, func() error {
			_, err := bound.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
				PersonID: person.ID, DefinitionSlug: definition.Slug, Source: ProvenanceUser,
				Value: AttributeValue{Type: AttributeValueText, Text: new("Synthetic Bound Value")},
			})
			return err
		})
	})
}

// Ordinary and delegated writes share the same identity-before-facts order.
// Holding the identity row exposes earlier advisory locks directly, without
// requiring a deadlock victim or depending on retry scheduling.
func TestIdentityScopeOrdinaryPersonAttributeWritersSerialize(t *testing.T) {
	requirements := require.New(t)

	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	participant, err := st.EnsureParticipant("fact-owner@example.test", "Synthetic Fact Owner", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), AttributeDefinitionInput{
		UniversalID: "synthetic-fence-related-person", ObjectType: AttributeObjectPerson,
		Slug: "synthetic_fence_related_person", Label: "Synthetic Related Person",
		ValueType: AttributeValueRecordReference, FieldType: AttributeFieldPerson,
		RecordTarget: new("person"), Cardinality: AttributeCardinalitySingle,
		Ownership: AttributeOwnershipUser, APIMutable: true, IsAudited: true, IsDeletable: true,
	})
	requirements.NoError(err)
	var valueID int64
	t.Run("set", func(t *testing.T) {
		observeIdentitySupportWriter(t, st, func() error {
			write, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
				PersonID: person.ID, DefinitionSlug: definition.Slug, Source: ProvenanceUser,
				Value: AttributeValue{Type: AttributeValueRecordReference, RecordType: new("person"), RecordID: &person.ID},
			})
			if err == nil {
				valueID = write.Value.ID
			}
			return err
		})
	})
	requirements.Positive(valueID)
	t.Run("supersede", func(t *testing.T) {
		observeIdentitySupportWriter(t, st, func() error {
			_, err := st.SupersedePersonAttributeValueContext(t.Context(), PersonAttributeSupersedeInput{
				PersonID: person.ID, DefinitionSlug: definition.Slug, ExpectedValueID: &valueID,
			})
			return err
		})
	})
}

func TestIdentityScopeSourceOwnershipWritersSerialize(t *testing.T) {
	if !IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("requires PostgreSQL to observe independent transaction row locks")
	}
	st := newPGStoreInternal(t, os.Getenv("MSGVAULT_TEST_DB"))
	require.True(t, st.IsPostgreSQL())
	for _, operation := range []string{"create", "identifier", "config", "oauth app", "meeting owner"} {
		t.Run(operation, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			source, err := st.GetOrCreateSource("meeting", strings.ReplaceAll(operation, " ", "-")+"@example.test")
			requirements.NoError(err)
			before, err := st.IdentityRevision()
			requirements.NoError(err)
			var created *Source
			observeIdentitySupportWriter(t, st, func() error {
				switch operation {
				case "create":
					var err error
					created, err = st.GetOrCreateSource("meeting", "new-source@example.test")
					return err
				case "identifier":
					return st.UpdateSourceIdentifier(source.ID, "renamed-source@example.test")
				case "config":
					return st.UpdateSourceSyncConfig(source.ID, `{"account_email":"owner@example.test"}`)
				case "oauth app":
					return st.UpdateSourceOAuthApp(source.ID, sql.NullString{String: "synthetic-app", Valid: true})
				case "meeting owner":
					return st.BindMeetingSourceOwner(source.ID, "owner@example.test")
				}
				return nil
			})
			if operation == "create" {
				requirements.NotNil(created)
				assertions.Equal("new-source@example.test", created.Identifier)
			} else {
				got, err := st.GetSourceByID(source.ID)
				requirements.NoError(err)
				requirements.NotNil(got)
				switch operation {
				case "identifier":
					assertions.Equal("renamed-source@example.test", got.Identifier)
				case "config", "meeting owner":
					assertions.JSONEq(`{"account_email":"owner@example.test"}`, got.SyncConfig.String)
				case "oauth app":
					assertions.Equal(sql.NullString{String: "synthetic-app", Valid: true}, got.OAuthApp)
				}
			}
			after, err := st.IdentityRevision()
			requirements.NoError(err)
			assertions.Equal(before, after, "support synchronization must not change semantic identity revision")
		})
	}
}

func TestIdentityScopeMessageMigrationSerializes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	if !IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("requires PostgreSQL to observe independent transaction row locks")
	}
	st := newPGStoreInternal(t, os.Getenv("MSGVAULT_TEST_DB"))
	requirements.True(st.IsPostgreSQL())
	source, err := st.GetOrCreateSource("gmail", "migration@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "synthetic-migration-thread", "Synthetic Thread")
	requirements.NoError(err)
	for _, id := range []string{"legacy-message", "canonical-message"} {
		senderID, err := st.EnsureParticipant(id+"@example.test", "Synthetic Sender", "example.test")
		requirements.NoError(err)
		_, err = st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversationID, SourceMessageID: id, MessageType: "email", SenderID: sql.NullInt64{Int64: senderID, Valid: true}})
		requirements.NoError(err)
	}
	observeIdentitySupportWriter(t, st, func() error {
		return st.MigrateSourceMessageID(source.ID, conversationID, "legacy-message", "canonical-message")
	})
	var remaining int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`, source.ID, "legacy-message").Scan(&remaining))
	assertions.Zero(remaining)
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`, source.ID, "canonical-message").Scan(&remaining))
	assertions.Equal(1, remaining)
}

func TestIdentityScopeSenderRepairSerializes(t *testing.T) {
	requirements := require.New(t)

	if !IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("requires PostgreSQL to observe independent transaction row locks")
	}
	st := newPGStoreInternal(t, os.Getenv("MSGVAULT_TEST_DB"))
	requirements.True(st.IsPostgreSQL())
	source, err := st.GetOrCreateSource("gmail", "repair@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "synthetic-repair-thread", "Synthetic Thread")
	requirements.NoError(err)
	messageID, err := st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversationID, SourceMessageID: "synthetic-repair-message", MessageType: "email"})
	requirements.NoError(err)
	raw := []byte("From: Synthetic Sender <sender@example.test>\r\nSubject: Synthetic repair\r\n\r\nSynthetic body\r\n")
	requirements.NoError(st.UpsertMessageRaw(messageID, raw))
	candidates, err := st.ListMissingMIMESendersPageContext(t.Context(), 0, 1)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	requirements.NoError(candidates[0].DecodeError)
	observeIdentitySupportWriter(t, st, func() error {
		return st.ApplySenderRepairContext(t.Context(), messageID, candidates[0].RawMIMEFingerprint, []mime.Address{{Email: "sender@example.test", Name: "Synthetic Sender", Domain: "example.test"}})
	})
	var sender string
	requirements.NoError(st.db.QueryRow(`SELECT p.email_address FROM participants p
		WHERE EXISTS (SELECT 1 FROM messages m WHERE m.id = ? AND m.sender_id = p.id)`, messageID).Scan(&sender))
	assert.Equal(t, "sender@example.test", sender)
}

func TestIdentityScopeContactObservationSerializes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	if !IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("requires PostgreSQL to observe independent transaction row locks")
	}
	st := newPGStoreInternal(t, os.Getenv("MSGVAULT_TEST_DB"))
	requirements.True(st.IsPostgreSQL())
	source, err := st.GetOrCreateSource("beeper", "synthetic-contact-source")
	requirements.NoError(err)
	participantID, err := st.EnsureParticipant("observed@example.test", "Synthetic Contact", "example.test")
	requirements.NoError(err)
	var result *RecordContactObservationResult
	observeIdentitySupportWriter(t, st, func() error {
		var err error
		result, err = st.RecordContactObservationContext(t.Context(), participantID, ParticipantContactObservationInput{
			SourceID: &source.ID, AddressKind: ContactAddressEmail, OriginalValue: "contact@example.test",
			Envelope: ValueEnvelopeInput{Source: ProvenanceArchiveObservation},
		})
		return err
	})
	requirements.NotNil(result)
	requirements.NotNil(result.Observation)
	assertions.Equal(&source.ID, result.Observation.SourceID)
	assertions.Equal(participantID, result.Observation.ParticipantID)
	assertions.Equal("contact@example.test", result.Observation.NormalizedValue)
}

func TestIdentityScopeCardDAVOwnershipWritersSerialize(t *testing.T) {
	requirements := require.New(t)

	if !IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("requires PostgreSQL to observe independent transaction row locks")
	}
	st := newPGStoreInternal(t, os.Getenv("MSGVAULT_TEST_DB"))
	requirements.True(st.IsPostgreSQL())
	allowed := true
	input := CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "synthetic",
		PrincipalURL: "https://contacts.example.test/principal/synthetic/",
		HomeURL:      "https://contacts.example.test/books/synthetic/",
		Books: []CardDAVDiscoveredBook{{
			CanonicalURL: "https://contacts.example.test/books/synthetic/personal/",
			DisplayName:  "Synthetic Contacts", CanCreate: &allowed, CanUpdate: &allowed, CanDelete: &allowed,
		}},
	}
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	requirements.NoError(err)
	requirements.Len(books, 1)
	bookID := books[0].ID
	t.Run("discovery", func(t *testing.T) {
		input.Books[0].DisplayName = "Updated Synthetic Contacts"
		observeIdentitySupportWriter(t, st, func() error {
			_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			return err
		})
		got, err := st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, input.Books[0].DisplayName, got[0].DisplayName)
	})
	t.Run("roles", func(t *testing.T) {
		observeIdentitySupportWriter(t, st, func() error {
			return st.SetCardDAVBookRolesContext(t.Context(), bookID, CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true})
		})
		got, err := st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.True(t, got[0].IsLookupSource)
	})
	t.Run("sync plan", func(t *testing.T) {
		requirements := require.New(t)

		got, err := st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
		requirements.NoError(err)
		requirements.Len(got, 1)
		plan := CardDAVSyncPlan{AddressBookID: bookID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: got[0].SyncRevision, NextSyncToken: "synthetic-next-token"}
		observeIdentitySupportWriter(t, st, func() error {
			_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), plan)
			return err
		})
		got, err = st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
		requirements.NoError(err)
		requirements.Len(got, 1)
		assert.Equal(t, plan.NextSyncToken, got[0].SyncToken)
	})
	t.Run("publication preparation", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)

		var personID int64
		requirements.NoError(st.db.QueryRow(`INSERT INTO persons (vcard_uid, display_name)
			VALUES ('synthetic-fence-person', 'Synthetic Person') RETURNING id`).Scan(&personID))
		snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
		requirements.NoError(err)
		plan := CardDAVPublicationPlan{
			PersonID: personID, Desired: true, AddressBookID: bookID,
			Href:                 input.Books[0].CanonicalURL + "synthetic-fence-person.vcf",
			OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic-fence-person\r\nFN:Synthetic Person\r\nEND:VCARD\r\n"),
			OutgoingSemanticHash: "synthetic-semantic", LocalHash: snapshot.Fingerprint,
		}
		var prepared *CardDAVPublication
		observeIdentitySupportWriter(t, st, func() error {
			var err error
			prepared, err = st.PrepareCardDAVPublicationContext(t.Context(), plan)
			return err
		})
		requirements.NotNil(prepared)
		assertions.Equal(CardDAVMutationCreate, prepared.PendingOperation)
		got, err := st.GetCardDAVPublicationContext(t.Context(), personID)
		requirements.NoError(err)
		assertions.Equal(plan.OutgoingBody, got.OutgoingBody)
		assertions.Equal(bookID, got.AddressBookID)
		t.Run("publication refresh", func(t *testing.T) {
			observeIdentitySupportWriter(t, st, func() error {
				var err error
				prepared, err = st.RefreshCardDAVPublicationFenceContext(t.Context(), personID)
				return err
			})
			require.NotNil(t, prepared)
			assert.Equal(t, CardDAVMutationCreate, prepared.PendingOperation)
		})
		t.Run("publication rollback", func(t *testing.T) {
			observeIdentitySupportWriter(t, st, func() error { return st.RollbackCardDAVPublicationContext(t.Context(), prepared) })
			current, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.ErrorIs(t, err, ErrCardDAVPublicationNotFound)
			assert.Nil(t, current)
			prepared, err = st.PrepareCardDAVPublicationContext(t.Context(), plan)
			require.NoError(t, err)
		})
		t.Run("publication commit", func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			remote := CardDAVRemoteResource{
				Href: plan.Href, RemoteUID: "synthetic-fence-person", RemoteETag: `"synthetic-one"`,
				RemoteBody: plan.OutgoingBody, SemanticHash: plan.OutgoingSemanticHash,
				DisplayName: "Synthetic Person",
			}
			observeIdentitySupportWriter(t, st, func() error {
				return st.CommitCardDAVPublicationContext(t.Context(), CardDAVCanonicalMutation{
					Publication: *prepared, Remote: remote,
				})
			})
			resource, err := st.GetCardDAVResourceContext(t.Context(), bookID, plan.Href)
			requirements.NoError(err)
			assertions.Equal(&personID, resource.PersonID)
			assertions.Equal(CardDAVGovernanceLocal, resource.Governance)
			assertions.Equal(remote.RemoteETag, resource.RemoteETag)
			got, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			requirements.NoError(err)
			assertions.Empty(got.PendingOperation)
			assertions.True(got.Desired)
			changed := remote
			changed.RemoteETag = `"synthetic-two"`
			changed.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic-fence-person\r\nFN:Changed Synthetic Person\r\nEND:VCARD\r\n")
			changed.SemanticHash = "changed-synthetic-semantic"
			changed.DisplayName = "Changed Synthetic Person"
			var conflict *CardDAVConflict
			t.Run("conflict capture", func(t *testing.T) {
				observeIdentitySupportWriter(t, st, func() error {
					var err error
					conflict, err = st.RecordCardDAVConflictContext(t.Context(), CardDAVConflictCapture{
						AddressBookID: bookID, Href: plan.Href, ExpectedMappingRevision: resource.MappingRevision,
						BaseLocalHash: resource.LocalHash, LocalHash: resource.LocalHash,
						BaseRemoteHash: resource.RemoteSemanticHash, BaseRemoteETag: resource.RemoteETag,
						RemoteETag: changed.RemoteETag, LocalBody: remote.RemoteBody, RemoteBody: changed.RemoteBody,
					})
					return err
				})
				require.NotNil(t, conflict)
				assert.Equal(t, CardDAVConflictUnresolved, conflict.Status)
				assert.Equal(t, changed.RemoteBody, conflict.RemoteBody)
			})
			requirements.NotNil(conflict)
			t.Run("conflict remote resolution", func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)

				var resolved *CardDAVConflict
				observeIdentitySupportWriter(t, st, func() error {
					var err error
					resolved, err = st.ResolveCardDAVConflictRemoteContext(t.Context(), CardDAVConflictRemoteResolution{
						ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision,
						ExpectedPersonID: personID, Remote: changed,
					})
					return err
				})
				requirements.NotNil(resolved)
				assertions.Equal(CardDAVConflictResolved, resolved.Status)
				resource, err := st.GetCardDAVResourceContext(t.Context(), bookID, plan.Href)
				requirements.NoError(err)
				assertions.Equal(changed.RemoteETag, resource.RemoteETag)
				assertions.Equal(changed.RemoteBody, resource.RemoteBody)
			})
			t.Run("pending publication conflict capture", func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)

				person, err := st.GetPersonContext(t.Context(), personID)
				requirements.NoError(err)
				_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Synthetic Local Change"))
				requirements.NoError(err)
				local, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
				requirements.NoError(err)
				update := plan
				update.LocalHash = local.Fingerprint
				update.OutgoingBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic-fence-person\r\nFN:Synthetic Local Change\r\nEND:VCARD\r\n")
				update.OutgoingSemanticHash = "synthetic-local-change"
				pending, err := st.PrepareCardDAVPublicationContext(t.Context(), update)
				requirements.NoError(err)
				requirements.Equal(CardDAVMutationUpdate, pending.PendingOperation)
				mapping, err := st.GetCardDAVResourceContext(t.Context(), bookID, plan.Href)
				requirements.NoError(err)
				capture := CardDAVConflictCapture{
					AddressBookID: bookID, Href: plan.Href, ExpectedMappingRevision: mapping.MappingRevision,
					BaseLocalHash: mapping.LocalHash, LocalHash: local.Fingerprint,
					BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
					RemoteETag: `"synthetic-three"`, LocalBody: update.OutgoingBody, RemoteBody: changed.RemoteBody,
				}
				t.Run("oversize rollback", func(t *testing.T) {
					oversize := capture
					oversize.LocalBody = make([]byte, MaxCardDAVConflictSnapshotBytes+1)
					var rejected error
					observeIdentitySupportWriter(t, st, func() error {
						_, rejected = st.RecordCardDAVPublicationConflictContext(t.Context(), *pending, oversize)
						if errors.Is(rejected, ErrCardDAVConflictTooLarge) {
							return nil
						}
						return rejected
					})
					require.ErrorIs(t, rejected, ErrCardDAVConflictTooLarge)
					pending, err = st.PrepareCardDAVPublicationContext(t.Context(), update)
					require.NoError(t, err)
					mapping, err = st.GetCardDAVResourceContext(t.Context(), bookID, plan.Href)
					require.NoError(t, err)
					capture.ExpectedMappingRevision = mapping.MappingRevision
				})
				var recorded *CardDAVConflict
				observeIdentitySupportWriter(t, st, func() error {
					var err error
					recorded, err = st.RecordCardDAVPublicationConflictContext(t.Context(), *pending, capture)
					return err
				})
				requirements.NotNil(recorded)
				assertions.Equal(CardDAVConflictUnresolved, recorded.Status)
			})
		})
	})
}
