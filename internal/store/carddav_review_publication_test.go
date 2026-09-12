package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func reviewedCurrentPlan(t *testing.T, st *store.Store, personID int64) store.CardDAVReviewedPublicationPlan {
	t.Helper()
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(t, err)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:review-person\r\nFN:Review Person\r\nEND:VCARD\r\n")
	plan := store.CardDAVPublicationPlan{PersonID: personID, Desired: true,
		AddressBookID: source.Book.ID, Href: source.Book.CanonicalURL + source.Person.VCardUID + ".vcf",
		OutgoingBody: body, OutgoingSemanticHash: "semantic", LocalHash: source.Snapshot.Fingerprint}
	fence := store.CardDAVCurrentReviewFence(source, body, plan.Href)
	return store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)}
}

func TestReviewedPublicationStoreRejectsChangedArtifactFences(t *testing.T) {
	for _, field := range []string{"body", "person", "inference", "generation", "book_revision", "write_target", "href", "token", "mapping", "mutation", "conflict", "etag"} {
		t.Run(field, func(t *testing.T) {
			require := require.New(t)
			st, _, book := newCardDAVResourceStore(t)
			personID := inferenceReviewPerson(t, st)
			_, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: personID, Text: "Inferred", Source: store.ProvenanceExtraction})
			require.NoError(err)
			plan := reviewedCurrentPlan(t, st, personID)
			switch field {
			case "body":
				plan.Publication.OutgoingBody = append(plan.Publication.OutgoingBody, '\n')
			case "person":
				_, err = st.DB().Exec(st.Rebind(`UPDATE persons SET display_name = ? WHERE id = ?`), "Changed Person", personID)
			case "inference":
				_, err = st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: personID, Text: "Later", Source: store.ProvenanceExtraction})
			case "generation":
				_, err = st.DB().Exec(`UPDATE carddav_accounts SET connection_generation = connection_generation + 1`)
			case "book_revision":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET sync_revision = sync_revision + 1 WHERE id = ?`), book.ID)
			case "write_target":
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET is_write_target = FALSE WHERE id = ?`), book.ID)
			case "href":
				plan.Publication.Href += "-changed"
			case "token":
				plan.ApprovalToken = "wrong"
			case "mapping":
				n := int64(1)
				plan.Fence.MappingRevision = &n
			case "mutation":
				n := int64(1)
				plan.Fence.MutationRevision = &n
			case "conflict":
				n := int64(1)
				plan.Fence.ConflictID = &n
			case "etag":
				etag := "changed"
				plan.Fence.RemoteETag = &etag
			}
			require.NoError(err)
			_, err = st.PrepareReviewedCardDAVPublicationContext(t.Context(), plan)
			require.ErrorIs(err, store.ErrCardDAVReviewStale)
			var approvedRevision int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT approved_revision FROM person_carddav_inference_state WHERE person_id = ?`), personID).Scan(&approvedRevision))
			assert.Zero(t, approvedRevision)
			_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
			require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
		})
	}
}

func TestCardDAVPersonCoordinatorSharesScopedViewsAndCancelsWaiters(t *testing.T) {
	require := require.New(t)
	st, _, _ := newCardDAVResourceStore(t)
	personID := inferenceReviewPerson(t, st)
	release, err := st.AcquireCardDAVPersonOperation(t.Context(), personID)
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	canceled := make(chan error, 1)
	go func() {
		unlock, err := st.ScopedToSync(1, 1).AcquireCardDAVPersonOperation(ctx, personID)
		if unlock != nil {
			unlock()
		}
		canceled <- err
	}()
	cancel()
	require.ErrorIs(<-canceled, context.Canceled)
	other, err := st.AcquireCardDAVPersonOperation(t.Context(), personID+1)
	require.NoError(err)
	other()
	release()
	release()
	final, err := st.ScopedToSync(1, 1).AcquireCardDAVPersonOperation(t.Context(), personID)
	require.NoError(err)
	final()
}

func TestAppendNotesDoesNotAdvanceDebtForDeclaredBlankOrDryRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, _, _ := newCardDAVResourceStore(t)
	personID := inferenceReviewPerson(t, st)
	_, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: personID, Text: "Inferred", Source: store.ProvenanceExtraction})
	require.NoError(err)
	for _, input := range []store.PersonNoteAppendInput{
		{PersonID: personID, Text: "Declared", Source: store.ProvenanceUser},
		{PersonID: personID, Text: "Preview", Source: store.ProvenanceExtraction, DryRun: true},
		{PersonID: personID, Text: " ", Source: store.ProvenanceExtraction},
	} {
		_, err := st.AppendPersonNoteContext(t.Context(), input)
		if input.Text == " " {
			require.Error(err)
		} else {
			require.NoError(err)
		}
		source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
		require.NoError(err)
		assert.Equal(int64(1), source.Inference.InferenceRevision)
		assert.Zero(source.Inference.ApprovedRevision)
	}
}

func TestReviewedPublicationZeroStateRemainsSparseAndIntentAuthorized(t *testing.T) {
	require := require.New(t)
	st, _, _ := newCardDAVResourceStore(t)
	personID := inferenceReviewPerson(t, st)
	plan := reviewedCurrentPlan(t, st, personID)
	pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), plan)
	require.NoError(err)
	require.NotNil(pending.ApprovedInferenceRevision)
	assert.Zero(t, *pending.ApprovedInferenceRevision)
	var revision int64
	err = st.DB().QueryRow(st.Rebind(`SELECT inference_revision FROM person_carddav_inference_state WHERE person_id = ?`), personID).Scan(&revision)
	require.ErrorIs(err, sql.ErrNoRows)
	require.NoError(st.RollbackCardDAVPublicationContext(t.Context(), pending))
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
}

func TestReviewedPublicationFirstInvalidationRejectsStalePostgresSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, _, _ := newCardDAVResourceStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL repeatable-read row version conflicts")
	}
	personID := inferenceReviewPerson(t, st)
	initial := reviewedCurrentPlan(t, st, personID)
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), initial.Publication)
	require.NoError(err)
	require.NoError(st.CommitCardDAVPublicationContext(t.Context(), store.CardDAVCanonicalMutation{
		Publication: *pending, Remote: store.CardDAVRemoteResource{Href: pending.Href, RemoteUID: "review-person",
			RemoteETag: `"one"`, RemoteBody: pending.OutgoingBody, SemanticHash: pending.OutgoingSemanticHash},
	}))
	plan := reviewedCurrentPlan(t, st, personID)
	lockedBook, resume := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	st.SetCardDAVPublicationReviewBeforePersonLockHookForTest(func() {
		close(lockedBook)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	})
	defer st.SetCardDAVPublicationReviewBeforePersonLockHookForTest(nil)
	done := make(chan error, 1)
	go func() { _, err := st.PrepareReviewedCardDAVPublicationContext(ctx, plan); done <- err }()
	select {
	case <-lockedBook:
	case <-ctx.Done():
		require.NoError(ctx.Err())
	}
	_, err = st.AppendPersonNoteContext(ctx, store.PersonNoteAppendInput{PersonID: personID, Text: "First inferred detail", Source: store.ProvenanceExtraction})
	require.NoError(err)
	close(resume)
	require.ErrorIs(<-done, store.ErrCardDAVReviewStale)
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(int64(1), source.Inference.InferenceRevision)
	assert.Zero(source.Inference.ApprovedRevision)
	require.NotNil(source.Publication)
	assert.Empty(source.Publication.PendingOperation)
}

func TestReviewedPublicationUpgradeLeavesLegacyIntentUnapproved(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, _, _ := newCardDAVResourceStore(t)
	personID := inferenceReviewPerson(t, st)
	plan := reviewedCurrentPlan(t, st, personID)
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), plan.Publication)
	require.NoError(err)
	// Exercise the production legacy column migrations by reopening a schema
	// with a real old pending intent and without any new proof columns.
	for _, column := range []string{"approved_body_sha256", "approved_inference_revision", "approved_mutation_revision", "outgoing_envelope_metadata"} {
		_, err := st.DB().Exec(`ALTER TABLE carddav_publications DROP COLUMN ` + column)
		require.NoError(err)
	}
	for range 2 {
		require.NoError(st.InitSchema())
	}
	upgraded, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(pending.OutgoingBody, upgraded.OutgoingBody)
	assert.Equal(pending.MutationRevision, upgraded.MutationRevision)
	assert.Nil(upgraded.ApprovedBodySHA256)
	assert.Nil(upgraded.ApprovedInferenceRevision)
	assert.Nil(upgraded.ApprovedMutationRevision)
	assert.Empty(upgraded.OutgoingEnvelopeMetadata)
}

func TestReviewedPublicationReopenPreservesExactIntentProof(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, _, _ := newCardDAVResourceStore(t)
	personID := inferenceReviewPerson(t, st)
	plan := reviewedCurrentPlan(t, st, personID)
	pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), plan)
	require.NoError(err)
	reopened, err := store.Open(store.DBPathForTest(st))
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(reopened.InitSchema())
	after, err := reopened.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(pending.OutgoingBody, after.OutgoingBody)
	assert.Equal(pending.ApprovedBodySHA256, after.ApprovedBodySHA256)
	assert.Equal(pending.ApprovedInferenceRevision, after.ApprovedInferenceRevision)
	assert.Equal(pending.ApprovedMutationRevision, after.ApprovedMutationRevision)
}

func TestCurrentPublicationOrdinaryPreparationRejectsChangedSourceFence(t *testing.T) {
	st, _, book := newCardDAVResourceStore(t)
	personID := inferenceReviewPerson(t, st)
	reviewed := reviewedCurrentPlan(t, st, personID)
	reviewed.Publication.SourceFence = &reviewed.Fence
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET sync_revision = sync_revision + 1 WHERE id = ?`), book.ID)
	require.NoError(t, err)
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), reviewed.Publication)
	require.ErrorIs(t, err, store.ErrCardDAVStalePlan)
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(t, err, store.ErrCardDAVPublicationNotFound)
}
