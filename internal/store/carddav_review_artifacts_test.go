package store_test

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
	"go.kenn.io/msgvault/internal/vcardmap"
	"sync"
	"testing"
	"time"
)

func conflictApprovalPlan(t *testing.T, st *store.Store, id int64) store.CardDAVConflictLocalApprovalPlan {
	t.Helper()
	source, err := st.LoadCardDAVConflictReviewSourceContext(t.Context(), id)
	require.NoError(t, err)
	envelope, err := vcardmap.ProjectPersonEnvelope(*source.Snapshot, source.Envelope.ResourceEnvelope)
	require.NoError(t, err)
	envelope, err = envelope.PrepareWireRender(vcard.Version30)
	require.NoError(t, err)
	metadata, err := vcard.MarshalResourceMetadata(envelope)
	require.NoError(t, err)
	fence := store.CardDAVConflictReviewFence(source, envelope.StoredBody)
	return store.CardDAVConflictLocalApprovalPlan{Fence: fence, Body: envelope.StoredBody, EnvelopeMetadata: metadata, ApprovalToken: store.CardDAVReviewToken(fence)}
}

func TestConflictReviewRejectsStaleArtifactWithoutRefreshing(t *testing.T) {
	for _, field := range []string{"body", "person", "inference", "generation", "book", "mapping", "etag", "conflict", "mutation", "token"} {
		t.Run(field, func(t *testing.T) {
			require := require.New(t)
			st, _, book, mapping := seededCardDAVConflictMapping(t)
			conflict, err := st.RecordCardDAVConflictContext(t.Context(), conflictCapture(mapping))
			require.NoError(err)
			plan := conflictApprovalPlan(t, st, conflict.ID)
			switch field {
			case "body":
				plan.Body = append(plan.Body, '\n')
			case "person":
				_, err = st.DB().Exec(st.Rebind(`UPDATE persons SET display_name='Changed' WHERE id=?`), *mapping.PersonID)
			case "inference":
				_, err = st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: *mapping.PersonID, Text: "New inference", Source: store.ProvenanceExtraction})
			case "generation":
				_, err = st.DB().Exec(`UPDATE carddav_accounts SET connection_generation=connection_generation+1`)
			case "book":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET sync_revision=sync_revision+1 WHERE id=?`), book.ID)
			case "mapping":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_resources SET mapping_revision=mapping_revision+1 WHERE id=?`), mapping.ID)
			case "etag":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_conflicts SET remote_etag='changed' WHERE id=?`), conflict.ID)
			case "conflict":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_conflicts SET review_revision=review_revision+1 WHERE id=?`), conflict.ID)
			case "mutation":
				_, err = st.DB().Exec(st.Rebind(`INSERT INTO carddav_publications(person_id,desired,address_book_id,href) VALUES (?,TRUE,?,?)`), *mapping.PersonID, book.ID, mapping.Href)
			case "token":
				plan.ApprovalToken = "invalid"
			}
			require.NoError(err)
			before, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
			require.NoError(err)
			require.ErrorIs(st.ApproveCardDAVConflictLocalContext(t.Context(), plan), store.ErrCardDAVReviewStale)
			after, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
			require.NoError(err)
			assert.Equal(t, before, after)
		})
	}
}

func TestPendingReviewRejectsStaleArtifactWithoutAuthorizing(t *testing.T) {
	for _, field := range []string{"body", "person", "inference", "generation", "book", "mutation", "href", "token"} {
		t.Run(field, func(t *testing.T) {
			require := require.New(t)
			st, _, book := newCardDAVResourceStore(t)
			personID := inferenceReviewPerson(t, st)
			initial := reviewedCurrentPlan(t, st, personID)
			_, err := st.PrepareCardDAVPublicationContext(t.Context(), initial.Publication)
			require.NoError(err)
			_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_publications SET approved_body_sha256=NULL,approved_inference_revision=NULL,approved_mutation_revision=NULL WHERE person_id=?`), personID)
			require.NoError(err)
			source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			fence := store.CardDAVPendingReviewFence(source)
			plan := store.CardDAVPendingCreateApprovalPlan{Fence: fence, Body: source.Publication.OutgoingBody, ApprovalToken: store.CardDAVReviewToken(fence)}
			switch field {
			case "body":
				plan.Body = append(plan.Body, '\n')
			case "person":
				_, err = st.DB().Exec(st.Rebind(`UPDATE persons SET display_name='Changed' WHERE id=?`), personID)
			case "inference":
				_, err = st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: personID, Text: "New inference", Source: store.ProvenanceExtraction})
			case "generation":
				_, err = st.DB().Exec(`UPDATE carddav_accounts SET connection_generation=connection_generation+1`)
			case "book":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET sync_revision=sync_revision+1 WHERE id=?`), book.ID)
			case "mutation":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_publications SET mutation_revision=mutation_revision+1 WHERE person_id=?`), personID)
			case "href":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_publications SET href=href || '-changed' WHERE person_id=?`), personID)
			case "token":
				plan.ApprovalToken = "invalid"
			}
			require.NoError(err)
			before, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.ApprovePendingCardDAVCreateContext(t.Context(), plan)
			require.ErrorIs(err, store.ErrCardDAVReviewStale)
			after, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			assert.Equal(t, before, after)
		})
	}
}

func TestConflictArtifactUpgradeAndReopen(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, _, _, mapping := seededCardDAVConflictMapping(t)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), conflictCapture(mapping))
	require.NoError(err)
	for _, column := range []string{"review_revision", "local_inference_revision", "approved_local_body_sha256", "approved_local_inference_revision", "approved_conflict_revision", "local_envelope_metadata", "local_mutation_intent"} {
		_, err = st.DB().Exec(`ALTER TABLE carddav_conflicts DROP COLUMN ` + column)
		require.NoError(err)
	}
	reopened, err := store.Open(store.DBPathForTest(st))
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	for range 2 {
		require.NoError(reopened.InitSchema())
	}
	legacy, err := reopened.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(int64(1), legacy.ReviewRevision)
	assert.Equal(conflict.LocalBody, legacy.LocalBody)
	assert.Nil(legacy.ApprovedConflictRevision)
	plan := conflictApprovalPlan(t, reopened, conflict.ID)
	require.NoError(reopened.ApproveCardDAVConflictLocalContext(t.Context(), plan))
	require.NoError(reopened.InitSchema())
	approved, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(int64(2), approved.ReviewRevision)
	assert.Equal(plan.Body, approved.LocalBody)
	assert.Equal(plan.EnvelopeMetadata, approved.LocalEnvelopeMetadata)
	require.NotNil(approved.ApprovedConflictRevision)
	assert.Equal(approved.ReviewRevision, *approved.ApprovedConflictRevision)
}

func approvedInferenceConflict(t *testing.T) (*store.Store, *store.CardDAVConflict) {
	t.Helper()
	require := require.New(t)
	st, _, _, mapping := seededCardDAVConflictMapping(t)
	_, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: *mapping.PersonID, Text: "Inferred local", Source: store.ProvenanceExtraction})
	require.NoError(err)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	capture := conflictCapture(mapping)
	capture.LocalHash = snapshot.Fingerprint
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	plan := conflictApprovalPlan(t, st, conflict.ID)
	require.NoError(st.ApproveCardDAVConflictLocalContext(t.Context(), plan))
	conflict, err = st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	return st, conflict
}

func TestConflictLocalPreparationChecksDurableApproval(t *testing.T) {
	for _, field := range []string{"body", "inference", "generation", "book", "mapping", "approval_revision", "approval_inference", "remote_etag"} {
		t.Run(field, func(t *testing.T) {
			require := require.New(t)
			st, conflict := approvedInferenceConflict(t)
			source, err := st.LoadCardDAVConflictReviewSourceContext(t.Context(), conflict.ID)
			require.NoError(err)
			plan := store.CardDAVConflictLocalPlan{ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision, RemoteETag: conflict.RemoteETag, OutgoingSemanticHash: "local-hash"}
			switch field {
			case "body":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_conflicts SET local_body=? WHERE id=?`), append(conflict.LocalBody, '\n'), conflict.ID)
			case "inference":
				_, err = st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: source.Person.ID, Text: "Later inferred", Source: store.ProvenanceExtraction})
			case "generation":
				_, err = st.DB().Exec(`UPDATE carddav_accounts SET connection_generation=connection_generation+1`)
			case "book":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET sync_revision=sync_revision+1 WHERE id=?`), source.Book.ID)
			case "mapping":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_resources SET mapping_revision=mapping_revision+1 WHERE id=?`), source.Resource.ID)
			case "approval_revision":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_conflicts SET approved_conflict_revision=approved_conflict_revision+1 WHERE id=?`), conflict.ID)
			case "approval_inference":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_conflicts SET approved_local_inference_revision=0 WHERE id=?`), conflict.ID)
			case "remote_etag":
				plan.RemoteETag = `"later"`
			}
			require.NoError(err)
			_, err = st.PrepareCardDAVConflictLocalContext(t.Context(), plan)
			require.Error(err)
			_, err = st.GetCardDAVPublicationContext(t.Context(), source.Person.ID)
			require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
		})
	}
}

func TestConflictRefreshClearsApprovedArtifact(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, approved := approvedInferenceConflict(t)
	source, err := st.LoadCardDAVConflictReviewSourceContext(t.Context(), approved.ID)
	require.NoError(err)
	capture := conflictCapture(source.Resource)
	capture.LocalHash = source.Snapshot.Fingerprint
	capture.RemoteETag = `"new"`
	refreshed, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	assert.Equal(approved.ReviewRevision+1, refreshed.ReviewRevision)
	assert.Nil(refreshed.ApprovedLocalBodySHA256)
	assert.Nil(refreshed.ApprovedLocalInferenceRevision)
	assert.Nil(refreshed.ApprovedConflictRevision)
	assert.Empty(refreshed.LocalEnvelopeMetadata)
}

func TestConflictIntentReopenAndRollbackPreserveExactOwnership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, conflict := approvedInferenceConflict(t)
	source, err := st.LoadCardDAVConflictReviewSourceContext(t.Context(), conflict.ID)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO carddav_publications(person_id,desired,address_book_id,href) VALUES (?,TRUE,?,?)`), source.Person.ID, source.Book.ID, source.Resource.Href)
	require.NoError(err)
	pending, err := st.PrepareCardDAVConflictLocalContext(t.Context(), store.CardDAVConflictLocalPlan{ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision, RemoteETag: conflict.RemoteETag, OutgoingSemanticHash: "local-hash"})
	require.NoError(err)
	require.True(pending.HasExactBodyApproval())
	assert.Equal(conflict.LocalBody, pending.OutgoingBody)
	assert.Equal(conflict.LocalEnvelopeMetadata, pending.OutgoingEnvelopeMetadata)
	reopened, err := store.Open(store.DBPathForTest(st))
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(reopened.InitSchema())
	persisted, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	require.NoError(err)
	assert.Equal(pending.OutgoingBody, persisted.OutgoingBody)
	assert.Equal(pending.OutgoingEnvelopeMetadata, persisted.OutgoingEnvelopeMetadata)
	require.True(persisted.HasExactBodyApproval())
	require.NoError(reopened.RollbackCardDAVPublicationContext(t.Context(), persisted))
	rolledBack, err := st.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	require.NoError(err)
	assert.Empty(rolledBack.OutgoingBody)
	assert.Empty(rolledBack.OutgoingEnvelopeMetadata)
	assert.Nil(rolledBack.ApprovedMutationRevision)
	assert.Nil(rolledBack.ApprovedBodySHA256)
	assert.Nil(rolledBack.ApprovedInferenceRevision)
}

func TestArtifactApprovalRejectsConcurrentPostgresInference(t *testing.T) {
	for _, kind := range []string{"pending", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			st, _, book := newCardDAVResourceStore(t)
			if !st.IsPostgreSQL() {
				t.Skip("requires PostgreSQL repeatable-read row conflicts")
			}
			var personID int64
			var approve func(context.Context) error
			if kind == "pending" {
				personID = inferenceReviewPerson(t, st)
				initial := reviewedCurrentPlan(t, st, personID)
				_, err := st.PrepareCardDAVPublicationContext(t.Context(), initial.Publication)
				require.NoError(err)
				source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				require.NoError(err)
				fence := store.CardDAVPendingReviewFence(source)
				plan := store.CardDAVPendingCreateApprovalPlan{Fence: fence, Body: source.Publication.OutgoingBody, ApprovalToken: store.CardDAVReviewToken(fence)}
				approve = func(ctx context.Context) error {
					_, err := st.ApprovePendingCardDAVCreateContext(ctx, plan)
					return err
				}
			} else {
				account, err := st.GetCardDAVAccountContext(t.Context())
				require.NoError(err)
				remote := remoteResource(book.CanonicalURL+"race.vcf", "race", "Race Person", "race@example.test", `"base"`)
				_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote}})
				require.NoError(err)
				mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
				require.NoError(err)
				personID = *mapping.PersonID
				conflict, err := st.RecordCardDAVConflictContext(t.Context(), conflictCapture(mapping))
				require.NoError(err)
				plan := conflictApprovalPlan(t, st, conflict.ID)
				approve = func(ctx context.Context) error { return st.ApproveCardDAVConflictLocalContext(ctx, plan) }
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			reached, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			st.SetCardDAVPublicationReviewBeforePersonLockHookForTest(func() {
				once.Do(func() {
					close(reached)
					select {
					case <-resume:
					case <-ctx.Done():
					}
				})
			})
			defer st.SetCardDAVPublicationReviewBeforePersonLockHookForTest(nil)
			done := make(chan error, 1)
			go func() { done <- approve(ctx) }()
			select {
			case <-reached:
			case <-ctx.Done():
				require.NoError(ctx.Err())
			}
			_, err := st.AppendPersonNoteContext(ctx, store.PersonNoteAppendInput{PersonID: personID, Text: "First inference during approval", Source: store.ProvenanceExtraction})
			require.NoError(err)
			close(resume)
			require.ErrorIs(<-done, store.ErrCardDAVReviewStale)
		})
	}
}
