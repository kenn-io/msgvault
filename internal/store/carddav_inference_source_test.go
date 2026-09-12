package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func inferenceReviewPerson(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name) VALUES ('review-person', 'Review Person') RETURNING id`).Scan(&id))
	return id
}

func TestCardDAVReviewSourceUnpublishedTarget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, account, book := newCardDAVResourceStore(t)
	id := inferenceReviewPerson(t, st)
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(book, source.Book)
	assert.Equal(account.ConnectionGeneration, source.ConnectionGeneration)
	assert.Equal(source.Snapshot.Profile.Person, source.Person)
	assert.Equal(id, source.Inference.PersonID)
	assert.Nil(source.Publication)
	assert.Nil(source.Resource)
	assert.Nil(source.Envelope)
	assert.Nil(source.Conflict)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{}))
	_, err = st.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
	assert.ErrorIs(err, store.ErrCardDAVNoWriteTarget)
}

func TestCardDAVReviewSourceMissingAccount(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := inferenceReviewPerson(t, st)
	_, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
	require.ErrorIs(t, err, store.ErrCardDAVNoWriteTarget)
	_, err = st.LoadCardDAVPublicationReviewSourceContext(t.Context(), id+1)
	require.ErrorIs(t, err, store.ErrPersonNotFound)
}

func TestCardDAVReviewSourceMappedEnvelopeAndConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, account, book, mapping := seededCardDAVConflictMapping(t)
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	require.NotNil(source.Resource)
	require.NotNil(source.Envelope)
	assert.Equal(mapping, source.Resource)
	assert.Equal(mapping.RemoteBody, source.Envelope.StoredBody)
	assert.Equal(account.ConnectionGeneration, source.ConnectionGeneration)
	assert.Equal(book.ID, source.Book.ID)
	assert.Nil(source.Conflict)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), conflictCapture(mapping))
	require.NoError(err)
	source, err = st.LoadCardDAVPublicationReviewSourceContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.Equal(conflict, source.Conflict)
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_conflicts SET status = 'resolved', resolution = 'keep_local', resolved_at = CURRENT_TIMESTAMP WHERE id = ?`), conflict.ID)
	require.NoError(err)
	source, err = st.LoadCardDAVPublicationReviewSourceContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.Nil(source.Conflict)
}

func TestCardDAVReviewSourcePendingRetainsCapturedTarget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, _, book := newCardDAVResourceStore(t)
	id := inferenceReviewPerson(t, st)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), id)
	require.NoError(err)
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: id, Desired: true, AddressBookID: book.ID, Href: book.CanonicalURL + "review-person.vcf",
		OutgoingBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:review-person\r\nFN:Review Person\r\nEND:VCARD\r\n"), OutgoingSemanticHash: "pending", LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	// Legacy artifacts can outlive a target role change; review retains the exact target.
	_, err = st.DB().Exec(`UPDATE carddav_address_books SET is_write_target = FALSE, is_subscribed = FALSE`)
	require.NoError(err)
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(pending, source.Publication)
	assert.Equal(book.ID, source.Book.ID)
	assert.Nil(source.Resource)
}

func TestCardDAVReviewSourcePropagatesBrokenOptionalRecords(t *testing.T) {
	for _, broken := range []string{"resource", "envelope", "conflict", "missing envelope"} {
		t.Run(broken, func(t *testing.T) {
			st, _, book, mapping := seededCardDAVConflictMapping(t)
			var err error
			switch broken {
			case "resource":
				_, err = st.DB().Exec(`ALTER TABLE carddav_resources RENAME COLUMN remote_etag TO broken_etag`)
			case "envelope":
				_, err = st.DB().Exec(`UPDATE vcard_resource_envelopes SET resource_metadata = '{}'`)
			case "conflict":
				_, err = st.DB().Exec(`ALTER TABLE carddav_conflicts RENAME COLUMN status TO broken_status`)
			case "missing envelope":
				_, err = st.DB().Exec(st.Rebind(`DELETE FROM vcard_resource_envelopes WHERE source_ref = ?`), fmt.Sprintf("carddav:%d", book.ID))
			}
			require.NoError(t, err)
			_, err = st.LoadCardDAVPublicationReviewSourceContext(t.Context(), *mapping.PersonID)
			require.Error(t, err)
			if broken == "missing envelope" {
				assert.ErrorIs(t, err, store.ErrVCardResourceNotFound)
			}
		})
	}
}

func TestCardDAVInferenceApprovalClearedWhenDiscoveryDeletesBook(t *testing.T) {
	for _, identityChange := range []bool{false, true} {
		t.Run(fmt.Sprintf("identity_change_%t", identityChange), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, account, book := newCardDAVResourceStore(t)
			id := inferenceReviewPerson(t, st)
			_, err := st.DB().Exec(st.Rebind(`INSERT INTO person_carddav_inference_state
 (person_id,inference_revision,approved_revision,approved_connection_generation,approved_address_book_id)
 VALUES (?,7,7,?,?)`), id, account.ConnectionGeneration, book.ID)
			require.NoError(err)
			input := cardDAVRediscoveryForBook(account, book)
			if identityChange {
				input.Username = "replacement"
			} else {
				input.Books = nil
			}
			_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err, "book deletion must clear both nullable approval fields before FK cleanup")
			if !identityChange {
				require.Empty(books)
			}
			var revision, approved int64
			var generation, bookID *int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT inference_revision, approved_revision,
 approved_connection_generation, approved_address_book_id FROM person_carddav_inference_state WHERE person_id=?`), id).Scan(&revision, &approved, &generation, &bookID))
			assert.Equal(int64(7), revision)
			assert.Zero(approved)
			assert.Nil(generation)
			assert.Nil(bookID)
			// Reuse the canonical book URL under discovery without inheriting approval.
			if !identityChange {
				input = cardDAVRediscoveryForBook(account, book)
			}
			_, books, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			require.Len(books, 1)
			assert.NotEqual(book.ID, books[0].ID)
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT approved_revision, approved_address_book_id FROM person_carddav_inference_state WHERE person_id=?`), id).Scan(&approved, &bookID))
			assert.Zero(approved)
			assert.Nil(bookID)
		})
	}
}

func TestCardDAVReviewSourceSettledPublicationRequiresUsableTarget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, _, book := newCardDAVResourceStore(t)
	id, _ := settleCardDAVTestPublication(t, st, book, "review-settled")
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
	require.NoError(err)
	require.NotNil(source.Publication)
	require.NotNil(source.Resource)
	require.NotNil(source.Envelope)
	assert.True(source.Publication.Desired)
	// Model role drift from a legacy writer. Only captured pending artifacts may
	// still be reviewed at an unavailable ordinary publication target.
	_, err = st.DB().Exec(`UPDATE carddav_address_books SET is_write_target=FALSE`)
	require.NoError(err)
	_, err = st.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
	assert.ErrorIs(err, store.ErrCardDAVNoWriteTarget)
}

func TestCardDAVReviewSourceRejectsPublicationMissingCapturedBook(t *testing.T) {
	st, _, book := newCardDAVResourceStore(t)
	id, _ := settleCardDAVTestPublication(t, st, book, "review-missing-book")
	// Settled legacy rows allow NULL scope, but cannot authorize choosing a new
	// target on behalf of a publication that still claims to be desired.
	_, err := st.DB().Exec(`UPDATE carddav_publications SET address_book_id=NULL`)
	require.NoError(t, err)
	_, err = st.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
	assert.ErrorIs(t, err, store.ErrCardDAVAddressBookNotFound)
}
