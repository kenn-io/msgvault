package store_test

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"testing"
)

func TestConflictOwnedIntentReopenPreservesEvidenceAndIdentity(t *testing.T) {
	for _, finish := range []string{"commit", "rollback", "mismatch"} {
		t.Run(finish, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, c := approvedStandaloneConflict(t)
			pending, err := st.PrepareCardDAVConflictLocalContext(t.Context(), store.CardDAVConflictLocalPlan{ConflictID: c.ID, ExpectedMappingRevision: c.MappingRevision, RemoteETag: c.RemoteETag, OutgoingSemanticHash: "approved"})
			require.NoError(err)
			require.True(pending.ConflictOwned)
			persisted, err := st.GetCardDAVConflictContext(t.Context(), c.ID)
			require.NoError(err)
			require.Contains(string(persisted.LocalMutationIntent), `"version":1`)
			source, err := st.LoadCardDAVConflictReviewSourceContext(t.Context(), c.ID)
			require.NoError(err)
			// A pull refresh must reject the plan while the operation is ambiguous.
			capture := conflictCapture(source.Resource)
			capture.LocalHash = source.Snapshot.Fingerprint
			capture.RemoteETag = `"newer"`
			_, err = st.RecordCardDAVConflictContext(t.Context(), capture)
			require.ErrorIs(err, store.ErrCardDAVPublicationPending)
			retained, err := st.GetCardDAVConflictContext(t.Context(), c.ID)
			require.NoError(err)
			assert.Equal(persisted.LocalMutationIntent, retained.LocalMutationIntent)
			assert.Equal(persisted.MappingRevision, retained.MappingRevision)
			moved := store.CardDAVRemoteResource{Href: pending.Href + "-moved", PreviousHref: pending.Href, RemoteUID: source.Resource.RemoteUID, RemoteETag: source.Resource.RemoteETag, RemoteBody: source.Resource.RemoteBody, SemanticHash: source.Resource.RemoteSemanticHash}
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: source.Book.ID, ConnectionGeneration: source.ConnectionGeneration, SyncRevision: source.Book.SyncRevision, Upserts: []store.CardDAVRemoteResource{moved}})
			require.ErrorIs(err, store.ErrCardDAVPublicationPending)
			plan := conflictApprovalPlan(t, st, c.ID)
			require.ErrorIs(st.ApproveCardDAVConflictLocalContext(t.Context(), plan), store.ErrCardDAVReviewStale)
			person, err := st.GetPersonContext(t.Context(), pending.PersonID)
			require.NoError(err)
			require.ErrorIs(st.DeletePersonContext(t.Context(), person.ID, person.Revision), store.ErrPersonCardDAVPublished)
			var otherID int64
			require.NoError(st.DB().QueryRow(`INSERT INTO persons(vcard_uid,display_name) VALUES ('intent-survivor','Survivor') RETURNING id`).Scan(&otherID))
			other, err := st.GetPersonContext(t.Context(), otherID)
			require.NoError(err)
			merge := store.PersonMergeRequest{SurvivorID: other.ID, AbsorbedID: person.ID, ExpectedSurvivorRevision: other.Revision, ExpectedAbsorbedRevision: person.Revision, Actor: "test", IdempotencyKey: "intent-merge"}
			_, err = st.MergePersonsContext(t.Context(), merge)
			require.ErrorIs(err, store.ErrPersonCardDAVPublished)
			reopened, err := store.Open(store.DBPathForTest(st))
			require.NoError(err)
			t.Cleanup(func() { _ = reopened.Close() })
			require.NoError(reopened.InitSchema())
			c, err = reopened.GetCardDAVConflictContext(t.Context(), c.ID)
			require.NoError(err)
			restored, err := c.LocalMutationPublication()
			require.NoError(err)
			assert.Equal(pending, restored)
			_, err = reopened.GetCardDAVPublicationContext(t.Context(), person.ID)
			require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
			switch finish {
			case "commit":
				t.Logf("body=%s metadata=%s", pending.OutgoingBody, pending.OutgoingEnvelopeMetadata)
				_, err = st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: person.ID, Text: "Newer inference after intent", Source: store.ProvenanceExtraction})
				require.NoError(err)
				require.NoError(reopened.CommitCardDAVConflictLocalIntentContext(t.Context(), store.CardDAVCanonicalMutation{Publication: *restored, Remote: store.CardDAVRemoteResource{Href: pending.Href, RemoteUID: person.VCardUID, RemoteETag: `"settled"`, RemoteBody: pending.OutgoingBody, SemanticHash: pending.OutgoingSemanticHash}}))
			case "rollback":
				require.NoError(reopened.RollbackCardDAVConflictLocalIntentContext(t.Context(), *restored))
			case "mismatch":
				require.NoError(reopened.ResetCardDAVConflictLocalIntentContext(t.Context(), *restored, store.CardDAVRemoteResource{Href: pending.Href, RemoteETag: `"different"`, RemoteBody: c.RemoteBody, SemanticHash: "different"}, false))
			}
			cleared, err := st.GetCardDAVConflictContext(t.Context(), c.ID)
			require.NoError(err)
			assert.Empty(cleared.LocalMutationIntent)
			person, err = st.GetPersonContext(t.Context(), person.ID)
			require.NoError(err)
			merge.ExpectedAbsorbedRevision = person.Revision
			if finish == "rollback" {
				require.NoError(st.DeletePersonContext(t.Context(), person.ID, person.Revision))
			} else {
				_, err = st.MergePersonsContext(t.Context(), merge)
				require.NoError(err)
			}
		})
	}
}

func TestConflictOwnedCreateRetryUsesCapturedApprovalScope(t *testing.T) {
	for _, change := range []string{"none", "inference", "generation", "book", "body"} {
		t.Run(change, func(t *testing.T) {
			require := require.New(t)
			st, c := approvedStandaloneConflict(t)
			_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_conflicts SET remote_tombstone=TRUE,remote_etag=NULL WHERE id=?`), c.ID)
			require.NoError(err)
			// Approve the current tombstone artifact explicitly before preparing create.
			plan := conflictApprovalPlan(t, st, c.ID)
			require.NoError(st.ApproveCardDAVConflictLocalContext(t.Context(), plan))
			c, err = st.GetCardDAVConflictContext(t.Context(), c.ID)
			require.NoError(err)
			pending, err := st.PrepareCardDAVConflictLocalContext(t.Context(), store.CardDAVConflictLocalPlan{ConflictID: c.ID, ExpectedMappingRevision: c.MappingRevision, RemoteTombstone: true, OutgoingSemanticHash: "approved"})
			require.NoError(err)
			switch change {
			case "inference":
				_, err = st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: pending.PersonID, Text: "Later", Source: store.ProvenanceExtraction})
			case "generation":
				_, err = st.DB().Exec(`UPDATE carddav_accounts SET connection_generation=connection_generation+1`)
			case "book":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET sync_revision=sync_revision+1 WHERE id=?`), pending.AddressBookID)
			case "body":
				pending.OutgoingBody = append(pending.OutgoingBody, '\n')
			}
			require.NoError(err)
			if change != "body" {
				pending, err = st.RefreshCardDAVConflictLocalIntentContext(t.Context(), *pending)
				require.NoError(err)
			}
			err = st.ValidateCardDAVConflictCreateRetryContext(t.Context(), *pending)
			switch change {
			case "none", "inference":
				require.NoError(err)
			case "generation", "book":
				require.ErrorIs(err, store.ErrCardDAVReviewStale)
			case "body":
				require.ErrorIs(err, store.ErrCardDAVStalePlan)
			}
		})
	}
}

func approvedStandaloneConflict(t *testing.T) (*store.Store, *store.CardDAVConflict) {
	t.Helper()
	return standaloneConflict(t, true)
}

func standaloneConflict(t *testing.T, inferred bool) (*store.Store, *store.CardDAVConflict) {
	t.Helper()
	st, account, book := newCardDAVResourceStore(t)
	_, seedErr := st.DB().Exec(`INSERT INTO persons(vcard_uid,display_name) VALUES ('standalone','Standalone')`)
	require.NoError(t, seedErr)
	remote := remoteResource(book.CanonicalURL+"standalone.vcf", "standalone", "Standalone", "", `"base"`)
	remote.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:standalone\r\nFN:Standalone\r\nEND:VCARD\r\n")
	remote.Emails = nil
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote}})
	require.NoError(t, err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
	require.NoError(t, err)
	if inferred {
		_, err = st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: *mapping.PersonID, Text: "Approved inference", Source: store.ProvenanceExtraction})
		require.NoError(t, err)
	}
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *mapping.PersonID)
	require.NoError(t, err)
	capture := conflictCapture(mapping)
	capture.LocalHash = snapshot.Fingerprint
	c, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(t, err)
	plan := conflictApprovalPlan(t, st, c.ID)
	require.NoError(t, st.ApproveCardDAVConflictLocalContext(t.Context(), plan))
	c, err = st.GetCardDAVConflictContext(t.Context(), c.ID)
	require.NoError(t, err)
	return st, c
}

func TestConflictOwnedIntentSurvivesLegacyTableUpgrade(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, c := approvedStandaloneConflict(t)
	pending, err := st.PrepareCardDAVConflictLocalContext(t.Context(), store.CardDAVConflictLocalPlan{ConflictID: c.ID, ExpectedMappingRevision: c.MappingRevision, RemoteETag: c.RemoteETag, OutgoingSemanticHash: "approved"})
	require.NoError(err)
	c, err = st.GetCardDAVConflictContext(t.Context(), c.ID)
	require.NoError(err)
	require.NoError(recreateE7CardDAVConflicts(t, st))
	binary := "BLOB"
	if st.IsPostgreSQL() {
		binary = "BYTEA"
	}
	// A partially upgraded database already has durable evidence, but lacks the
	// e7 pending constraint. The SQLite rebuild must copy these bytes as well.
	_, err = st.DB().Exec(`ALTER TABLE carddav_conflicts ADD COLUMN review_revision INTEGER NOT NULL DEFAULT 1`)
	require.NoError(err)
	_, err = st.DB().Exec(`ALTER TABLE carddav_conflicts ADD COLUMN local_mutation_intent ` + binary)
	require.NoError(err)
	_, err = st.DB().Exec(`ALTER TABLE carddav_conflicts ADD COLUMN local_envelope_metadata ` + binary)
	require.NoError(err)
	var id int64
	require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO carddav_conflicts(address_book_id,href,base_local_hash,local_hash,base_remote_hash,base_remote_etag,remote_etag,mapping_revision,local_body,remote_body,local_mutation_intent,local_envelope_metadata,review_revision) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`), c.AddressBookID, c.Href, c.BaseLocalHash, c.LocalHash, c.BaseRemoteHash, c.BaseRemoteETag, c.RemoteETag, c.MappingRevision, c.LocalBody, c.RemoteBody, c.LocalMutationIntent, c.LocalEnvelopeMetadata, c.ReviewRevision).Scan(&id))
	require.NoError(st.InitSchema())
	require.NoError(st.InitSchema())
	reopened, err := store.Open(store.DBPathForTest(st))
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(reopened.InitSchema())
	upgraded, err := reopened.GetCardDAVConflictContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(c.LocalMutationIntent, upgraded.LocalMutationIntent)
	assert.Equal(c.LocalEnvelopeMetadata, upgraded.LocalEnvelopeMetadata)
	restored, err := upgraded.LocalMutationPublication()
	require.NoError(err)
	pending.ResolutionConflictID = id
	assert.Equal(pending, restored)
	require.NoError(reopened.RollbackCardDAVConflictLocalIntentContext(t.Context(), *restored))
}

func TestConflictOwnedReprepareRejectsOldCanonicalResult(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, c := standaloneConflict(t, false)
	plan := store.CardDAVConflictLocalPlan{ConflictID: c.ID, ExpectedMappingRevision: c.MappingRevision, RemoteETag: c.RemoteETag, OutgoingSemanticHash: "same-body"}
	first, err := st.PrepareCardDAVConflictLocalContext(t.Context(), plan)
	require.NoError(err)
	require.NoError(st.RollbackCardDAVConflictLocalIntentContext(t.Context(), *first))
	second, err := st.PrepareCardDAVConflictLocalContext(t.Context(), plan)
	require.NoError(err)
	assert.Equal(first.OutgoingBody, second.OutgoingBody)
	assert.Greater(second.MutationRevision, first.MutationRevision)
	remote := store.CardDAVRemoteResource{Href: first.Href, RemoteUID: "standalone", RemoteETag: `"canonical"`, RemoteBody: first.OutgoingBody, SemanticHash: first.OutgoingSemanticHash}
	require.ErrorIs(st.CommitCardDAVConflictLocalIntentContext(t.Context(), store.CardDAVCanonicalMutation{Publication: *first, Remote: remote}), store.ErrCardDAVStalePlan)
	require.NoError(st.CommitCardDAVConflictLocalIntentContext(t.Context(), store.CardDAVCanonicalMutation{Publication: *second, Remote: remote}))
}

func TestConflictOwnedIntentProtectsConnectionChangeUntilRollback(t *testing.T) {
	require := require.New(t)
	st, c := approvedStandaloneConflict(t)
	pending, err := st.PrepareCardDAVConflictLocalContext(t.Context(), store.CardDAVConflictLocalPlan{ConflictID: c.ID, ExpectedMappingRevision: c.MappingRevision, RemoteETag: c.RemoteETag, OutgoingSemanticHash: "approved"})
	require.NoError(err)
	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	require.ErrorIs(st.ValidateCardDAVConnectionChangeContext(t.Context(), account.BaseURL, account.Username, true), store.ErrCardDAVCredentialChangePending)
	require.NoError(st.RollbackCardDAVConflictLocalIntentContext(t.Context(), *pending))
	require.NoError(st.ValidateCardDAVConnectionChangeContext(t.Context(), account.BaseURL, account.Username, true))
}

func TestConflictOwnedIntentRejectsPullBeforeAdvancingSyncToken(t *testing.T) {
	for _, tombstone := range []bool{false, true} {
		t.Run(fmt.Sprintf("tombstone_%t", tombstone), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, c := approvedStandaloneConflict(t)
			pending, err := st.PrepareCardDAVConflictLocalContext(t.Context(), store.CardDAVConflictLocalPlan{ConflictID: c.ID, ExpectedMappingRevision: c.MappingRevision, RemoteETag: c.RemoteETag, OutgoingSemanticHash: "approved"})
			require.NoError(err)
			source, err := st.LoadCardDAVConflictReviewSourceContext(t.Context(), c.ID)
			require.NoError(err)
			capture := conflictCapture(source.Resource)
			capture.LocalHash = source.Snapshot.Fingerprint
			capture.RemoteETag = `"newer"`
			capture.RemoteTombstone = tombstone
			plan := store.CardDAVSyncPlan{AddressBookID: source.Book.ID, ConnectionGeneration: source.ConnectionGeneration, SyncRevision: source.Book.SyncRevision, NextSyncToken: "newer-token"}
			if tombstone {
				capture.RemoteBody, capture.RemoteETag = nil, ""
				plan.RemovedHrefs = []string{capture.Href}
			} else {
				plan.Upserts = []store.CardDAVRemoteResource{{Href: capture.Href, RemoteUID: source.Resource.RemoteUID, RemoteETag: capture.RemoteETag, RemoteBody: capture.RemoteBody, SemanticHash: "newer"}}
			}
			plan.Conflicts = []store.CardDAVConflictCapture{capture}
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), plan)
			require.ErrorIs(err, store.ErrCardDAVPublicationPending)
			books, err := st.ListCardDAVAddressBooksContext(t.Context())
			require.NoError(err)
			assert.Equal(source.Book.SyncToken, books[0].SyncToken)
			assert.Equal(source.Book.SyncRevision, books[0].SyncRevision)
			// After recovery releases the intent, the same delta is still usable.
			require.NoError(st.RollbackCardDAVConflictLocalIntentContext(t.Context(), *pending))
			plan.Conflicts[0].ExpectedMappingRevision = pending.PreviousMappingRevision
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), plan)
			require.NoError(err)
			updated, err := st.GetCardDAVConflictContext(t.Context(), c.ID)
			require.NoError(err)
			assert.Equal(capture.RemoteBody, updated.RemoteBody)
			assert.Equal(tombstone, updated.RemoteTombstone)
			assert.Equal(capture.RemoteETag, updated.RemoteETag)
		})
	}
}
