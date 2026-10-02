package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCardDAVCrossAccountTargetSwitchRacingPublication(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	st, _, oldBook := newCardDAVResourceStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL lock interleaving")
	}
	input := cardDAVConcurrentInput("work@example.com", "work")
	input.ConnectionName = "work"
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name) VALUES ('review-target-race','Example Person') RETURNING id`).Scan(&personID))
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	reached, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(resume) })
	st.SetCardDAVPublicationReviewBeforePersonLockHookForTest(func() {
		close(reached)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	})
	defer st.SetCardDAVPublicationReviewBeforePersonLockHookForTest(nil)
	publicationDone := make(chan error, 1)
	go func() {
		_, err := st.PrepareCardDAVPublicationContext(ctx, store.CardDAVPublicationPlan{PersonID: personID, Desired: true, AddressBookID: oldBook.ID, Href: oldBook.CanonicalURL + "review-target-race.vcf", OutgoingBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:review-target-race\r\nFN:Example Person\r\nEND:VCARD\r\n"), OutgoingSemanticHash: "example-hash", LocalHash: snapshot.Fingerprint})
		publicationDone <- err
	}()
	select {
	case <-reached:
	case <-ctx.Done():
		require.NoError(ctx.Err())
	}
	roleDone := make(chan error, 1)
	go func() {
		roleDone <- st.SetCardDAVBookRolesContext(ctx, books[0].ID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true})
	}()
	// Wait for the role change to reach an actual database lock held by the
	// old target publication before releasing that publication.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := st.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND (query LIKE '%carddav_accounts%' OR query LIKE '%carddav_address_books%'))`).Scan(&blocked)
		require.NoError(err)
		if blocked {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			require.NoError(ctx.Err())
		}
	}
	release.Do(func() { close(resume) })
	require.NoError(<-publicationDone)
	require.ErrorIs(<-roleDone, store.ErrCardDAVRoleChangePending, "cross-account target swap must not pass its ownership guard before waiting for old-owner publication")
	pending, err := st.GetCardDAVPublicationContext(ctx, personID)
	require.NoError(err)
	assertions.Equal(store.CardDAVMutationCreate, pending.PendingOperation)
	all, err := st.ListCardDAVAddressBooksContext(ctx, store.AllCardDAVAccounts)
	require.NoError(err)
	for _, book := range all {
		if book.ID == oldBook.ID {
			assertions.True(book.IsWriteTarget, "the book with newly pending work must remain target")
		}
	}
}
