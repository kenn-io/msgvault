package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCardDAVLocalPublicationOriginMigrationUsesOrderingEvidence(t *testing.T) {
	st, _, _ := newPersonFactProjectionStore(t)
	book := inferenceMigrationBook(t, st)
	cases := []struct {
		name, publicationCreatedAt, resourceCreatedAt string
		cardUID                                       string
		publicationDesired                            bool
		pendingOperation                              string
		want                                          bool
	}{
		{name: "publication predates resource", publicationCreatedAt: "2020-01-01 00:00:00", resourceCreatedAt: "2020-01-02 00:00:00", publicationDesired: true, want: true},
		{name: "remote match predates publication", publicationCreatedAt: "2020-01-02 00:00:00", resourceCreatedAt: "2020-01-01 00:00:00", publicationDesired: true},
		{name: "same-second UID-matched remote card is ambiguous", publicationCreatedAt: "2020-01-01 00:00:00", resourceCreatedAt: "2020-01-01 00:00:00", cardUID: "origin-same-second UID-matched remote card is ambiguous", publicationDesired: true},
		{name: "same-second remote match", publicationCreatedAt: "2020-01-01 00:00:00", resourceCreatedAt: "2020-01-01 00:00:00", cardUID: "remote-client-uid", publicationDesired: true},
		{name: "same-second card without UID", publicationCreatedAt: "2020-01-01 00:00:00", resourceCreatedAt: "2020-01-01 00:00:00", publicationDesired: true},
		{name: "pending local publication", publicationCreatedAt: "2020-01-01 00:00:00", resourceCreatedAt: "2020-01-02 00:00:00", publicationDesired: true, pendingOperation: "update", want: true},
		{name: "undesired local publication", publicationCreatedAt: "2020-01-01 00:00:00", resourceCreatedAt: "2020-01-02 00:00:00", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			var personID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
				VALUES (?, ?) RETURNING id`), "origin-"+tc.name, "Origin Example").Scan(&personID))
			href := fmt.Sprintf("%s%s.vcf", book.CanonicalURL, tc.name)
			_, err := st.DB().Exec(st.Rebind(`INSERT INTO carddav_resources (
				address_book_id, href, remote_uid, remote_etag, remote_body, remote_semantic_hash,
				local_hash, mapping_status, governance, person_id, created_at, updated_at
			) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?)`), book.ID, href, tc.cardUID, `"etag"`, []byte("card"), "semantic",
				"local", CardDAVMappingMapped, CardDAVGovernanceLocal, personID, tc.resourceCreatedAt, tc.resourceCreatedAt)
			require.NoError(err)
			_, err = st.DB().Exec(st.Rebind(`INSERT INTO carddav_publications (
				person_id, desired, address_book_id, href, pending_operation, local_hash,
				connection_generation, book_sync_revision, mapping_revision, created_at, updated_at
			) VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), NULLIF(?, 0), NULLIF(?, 0), ?, ?)`),
				personID, tc.publicationDesired, book.ID, href, tc.pendingOperation,
				"local", 1, 1, 1, tc.publicationCreatedAt, tc.publicationCreatedAt)
			require.NoError(err)
		})
	}

	require.NoError(t, rerunCardDAVLocalPublicationOriginMigration(t.Context(), st))

	for _, tc := range cases {
		var locallyPublished bool
		href := fmt.Sprintf("%s%s.vcf", book.CanonicalURL, tc.name)
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT locally_published FROM carddav_resources
			WHERE address_book_id = ? AND href = ?`), book.ID, href).Scan(&locallyPublished))
		assert.Equal(t, tc.want, locallyPublished, tc.name)
	}
}

func rerunCardDAVLocalPublicationOriginMigration(ctx context.Context, st *Store) error {
	_, err := st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), migrationCardDAVLocalPublicationOrigin)
	if err != nil {
		return err
	}
	return st.InitSchemaContext(ctx)
}
