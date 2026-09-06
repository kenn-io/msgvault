package store_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func seededCardDAVConflictMapping(
	t *testing.T,
) (*store.Store, store.CardDAVAccount, store.CardDAVAddressBook, *store.CardDAVResource) {
	t.Helper()
	st, account, book := newCardDAVResourceStore(t)
	remote := remoteResource(
		book.CanonicalURL+"alice.vcf", "remote-alice", "Alice", "alice@example.test", `"one"`,
	)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, NextSyncToken: "token-1",
		Upserts: []store.CardDAVRemoteResource{remote},
	})
	require.NoError(t, err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
	require.NoError(t, err)
	return st, account, book, mapping
}

func conflictCapture(mapping *store.CardDAVResource) store.CardDAVConflictCapture {
	return store.CardDAVConflictCapture{
		AddressBookID: mapping.AddressBookID,
		Href:          mapping.Href, ExpectedMappingRevision: mapping.MappingRevision,
		BaseLocalHash: mapping.LocalHash, LocalHash: mapping.LocalHash,
		BaseRemoteHash: mapping.RemoteSemanticHash,
		BaseRemoteETag: mapping.RemoteETag, RemoteETag: `"two"`,
		LocalBody:  []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-alice\r\nFN:Local\r\nEND:VCARD\r\n"),
		RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-alice\r\nFN:Remote\r\nEND:VCARD\r\n"),
	}
}

func TestCardDAVConflictSafeReadModelsUseCompactHeadersAndExactBaseFence(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(*testing.T, *store.Store, *store.CardDAVResource, *store.CardDAVConflict)
		baseAvailable bool
	}{
		{name: "exact tuple", baseAvailable: true},
		{name: "wrong revision", mutate: func(t *testing.T, st *store.Store, mapping *store.CardDAVResource, _ *store.CardDAVConflict) {
			t.Helper()
			_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_resources SET mapping_revision = mapping_revision + 1 WHERE id = ?`), mapping.ID)
			require.NoError(t, err)
		}},
		{name: "wrong semantic hash", mutate: func(t *testing.T, st *store.Store, mapping *store.CardDAVResource, _ *store.CardDAVConflict) {
			t.Helper()
			_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_resources SET remote_semantic_hash = ? WHERE id = ?`), "different", mapping.ID)
			require.NoError(t, err)
		}},
		{name: "wrong etag", mutate: func(t *testing.T, st *store.Store, mapping *store.CardDAVResource, _ *store.CardDAVConflict) {
			t.Helper()
			_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_resources SET remote_etag = ? WHERE id = ?`), `"different"`, mapping.ID)
			require.NoError(t, err)
		}},
		{name: "missing mapping", mutate: func(t *testing.T, st *store.Store, mapping *store.CardDAVResource, _ *store.CardDAVConflict) {
			t.Helper()
			_, err := st.DB().Exec(st.Rebind(`DELETE FROM carddav_resources WHERE id = ?`), mapping.ID)
			require.NoError(t, err)
		}},
		{name: "resolved and remapped", mutate: func(t *testing.T, st *store.Store, mapping *store.CardDAVResource, conflict *store.CardDAVConflict) {
			t.Helper()
			_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_conflicts SET status = 'resolved', resolution = 'keep_local', resolved_at = CURRENT_TIMESTAMP WHERE id = ?`), conflict.ID)
			require.NoError(t, err)
			_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_resources SET mapping_revision = mapping_revision + 1 WHERE id = ?`), mapping.ID)
			require.NoError(t, err)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, _, book, mapping := seededCardDAVConflictMapping(t)
			capture := conflictCapture(mapping)
			capture.LocalBody = append(capture.LocalBody, bytes.Repeat([]byte("private-local-body"), 1024)...)
			capture.RemoteBody = append(capture.RemoteBody, bytes.Repeat([]byte("private-remote-body"), 1024)...)
			conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
			require.NoError(err)

			headers, err := st.ListCardDAVConflictHeadersContext(t.Context())
			require.NoError(err)
			require.Len(headers, 1)
			assert.Equal(conflict.ID, headers[0].ID)
			assert.Equal(book.DisplayName, headers[0].AddressBookName)

			if tt.mutate != nil {
				tt.mutate(t, st, mapping, conflict)
			}
			detail, err := st.GetCardDAVConflictDetailSourceContext(t.Context(), conflict.ID)
			require.NoError(err)
			assert.Equal(capture.LocalBody, detail.LocalBody)
			assert.Equal(capture.RemoteBody, detail.RemoteBody)
			assert.Equal(tt.baseAvailable, detail.BaseAvailable)
			if tt.baseAvailable {
				assert.Equal(mapping.RemoteBody, detail.BaseBody)
			} else {
				assert.Empty(detail.BaseBody)
			}
		})
	}
}

func seededCardDAVTombstoneConflict(
	t *testing.T, mappedPerson bool,
) (*store.Store, store.CardDAVAccount, store.CardDAVAddressBook, *store.CardDAVResource, *store.CardDAVConflict, store.CardDAVRemoteResource) {
	t.Helper()
	st, account, book, mapping := seededCardDAVConflictMapping(t)
	if mappedPerson {
		require.NotNil(t, mapping.PersonID)
		snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *mapping.PersonID)
		require.NoError(t, err)
		_, err = st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
			PersonID: *mapping.PersonID, Desired: true, AddressBookID: book.ID, Href: mapping.Href,
			OutgoingBody: mapping.RemoteBody, OutgoingSemanticHash: mapping.RemoteSemanticHash,
			LocalHash: snapshot.Fingerprint,
		})
		require.NoError(t, err)
		pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
			PersonID: *mapping.PersonID, Desired: false, AddressBookID: book.ID, Href: mapping.Href,
		})
		require.NoError(t, err)
		mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
		require.NoError(t, err)
		remote := remoteResource(mapping.Href, "remote-alice", "Alice Remote", "remote@example.test", `"two"`)
		remote.SemanticHash = "semantic-remote-two"
		conflict, err := st.RecordCardDAVPublicationConflictContext(t.Context(), *pending,
			store.CardDAVConflictCapture{
				AddressBookID: book.ID, Href: mapping.Href,
				ExpectedMappingRevision: mapping.MappingRevision,
				BaseLocalHash:           mapping.LocalHash, LocalHash: pending.LocalHash,
				BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
				RemoteETag: remote.RemoteETag, RemoteBody: remote.RemoteBody,
				LocalTombstone: true,
			})
		require.NoError(t, err)
		mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
		require.NoError(t, err)
		return st, account, book, mapping, conflict, remote
	}

	require.NotNil(t, mapping.PersonID)
	person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
	require.NoError(t, err)
	require.NoError(t, st.DeletePersonContext(t.Context(), person.ID, person.Revision))
	mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(t, err)
	remote := remoteResource(mapping.Href, "remote-alice", "Alice Remote", "remote@example.test", `"two"`)
	remote.SemanticHash = "semantic-remote-two"
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), store.CardDAVConflictCapture{
		AddressBookID: book.ID, Href: mapping.Href,
		ExpectedMappingRevision: mapping.MappingRevision,
		BaseLocalHash:           mapping.LocalHash, LocalHash: mapping.LocalHash,
		BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
		RemoteETag: remote.RemoteETag, RemoteBody: remote.RemoteBody,
		LocalTombstone: true,
	})
	require.NoError(t, err)
	mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(t, err)
	return st, account, book, mapping, conflict, remote
}

func refreshedCardDAVTombstonePlan(
	t *testing.T, st *store.Store, account store.CardDAVAccount, book store.CardDAVAddressBook,
	mapping *store.CardDAVResource,
) (store.CardDAVSyncPlan, store.CardDAVRemoteResource) {
	t.Helper()
	books, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(t, err)
	require.Len(t, books, 1)
	latest := remoteResource(mapping.Href, "remote-alice", "Alice Latest", "latest@example.test", `"latest"`)
	latest.SemanticHash = "semantic-remote-latest"
	return store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, NextSyncToken: "latest-token",
		Upserts: []store.CardDAVRemoteResource{latest},
		Conflicts: []store.CardDAVConflictCapture{{
			AddressBookID: book.ID, Href: mapping.Href,
			ExpectedMappingRevision: mapping.MappingRevision,
			BaseLocalHash:           mapping.LocalHash, LocalHash: mapping.LocalHash,
			BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
			RemoteETag: latest.RemoteETag, RemoteBody: latest.RemoteBody,
			LocalTombstone: true,
		}},
	}, latest
}

func TestCardDAVOversizedConflictDoesNotAdvanceMapping(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, _, mapping := seededCardDAVConflictMapping(t)
	capture := conflictCapture(mapping)
	capture.LocalBody = bytes.Repeat([]byte("x"), store.MaxCardDAVConflictSnapshotBytes)
	capture.RemoteBody = []byte("y")

	_, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.ErrorIs(err, store.ErrCardDAVConflictTooLarge)

	after, err := st.GetCardDAVResourceContext(t.Context(), mapping.AddressBookID, mapping.Href)
	require.NoError(err)
	assert.Equal(mapping.MappingRevision, after.MappingRevision)
	conflicts, err := st.ListCardDAVConflictsContext(t.Context(), true)
	require.NoError(err)
	assert.Empty(conflicts)
}

func TestCardDAVConflictRefreshKeepsOneUnresolvedRowPerMapping(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, _, mapping := seededCardDAVConflictMapping(t)
	first, err := st.RecordCardDAVConflictContext(t.Context(), conflictCapture(mapping))
	require.NoError(err)

	refreshedCapture := conflictCapture(mapping)
	refreshedCapture.ExpectedMappingRevision = first.MappingRevision
	refreshedCapture.RemoteETag = `"three"`
	refreshedCapture.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-alice\r\nFN:Latest Remote\r\nEND:VCARD\r\n")
	second, err := st.RecordCardDAVConflictContext(t.Context(), refreshedCapture)
	require.NoError(err)

	assert.Equal(first.ID, second.ID)
	assert.Equal(first.MappingRevision+1, second.MappingRevision)
	assert.Equal(refreshedCapture.RemoteETag, second.RemoteETag)
	assert.Equal(refreshedCapture.RemoteBody, second.RemoteBody)
	conflicts, err := st.ListCardDAVConflictsContext(t.Context(), true)
	require.NoError(err)
	require.Len(conflicts, 1)
}

func TestCardDAVKeepRemoteResolutionAppliesRetainedSnapshotAndAuditsChoice(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, _, mapping := seededCardDAVConflictMapping(t)
	capture := conflictCapture(mapping)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	retained := parseCardDAVRemoteForStoreTest(mapping.Href, capture.RemoteETag, capture.RemoteBody)

	resolved, err := st.ResolveCardDAVConflictRemoteContext(t.Context(), store.CardDAVConflictRemoteResolution{
		ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision,
		Remote: retained,
	})
	require.NoError(err)
	assert.Equal(store.CardDAVConflictResolved, resolved.Status)
	assert.Equal(store.CardDAVResolutionKeepRemote, resolved.Resolution)
	assert.NotNil(resolved.ResolvedAt)

	after, err := st.GetCardDAVResourceContext(t.Context(), mapping.AddressBookID, mapping.Href)
	require.NoError(err)
	assert.Equal(capture.RemoteBody, after.RemoteBody)
	assert.Equal(capture.RemoteETag, after.RemoteETag)
	require.NotNil(after.PersonID)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *after.PersonID)
	require.NoError(err)
	assert.Equal(snapshot.Fingerprint, after.LocalHash)
}

func TestCardDAVKeepLocalUsesCapableSubscribedSourceBook(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, book, mapping := seededCardDAVConflictMapping(t)
	capture := conflictCapture(mapping)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET
		is_write_target = FALSE, is_subscribed = TRUE, can_update = FALSE WHERE id = ?`), book.ID)
	require.NoError(err)

	plan := store.CardDAVConflictLocalPlan{
		ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision,
		RemoteETag: conflict.RemoteETag, OutgoingSemanticHash: "local-semantic",
	}
	_, err = st.PrepareCardDAVConflictLocalContext(t.Context(), plan)
	require.ErrorIs(err, store.ErrCardDAVNoWriteTarget)
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET can_update = TRUE WHERE id = ?`), book.ID)
	require.NoError(err)

	prepared, err := st.PrepareCardDAVConflictLocalContext(t.Context(), plan)
	require.NoError(err)
	require.NotNil(prepared)
	assert.Equal(book.ID, prepared.AddressBookID)
	assert.Equal(mapping.Href, prepared.Href)
	assert.Equal(store.CardDAVMutationUpdate, prepared.PendingOperation)
}

func TestCardDAVKeepRemoteRevalidatesAfterUnlockedConflictIdentityRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, _, mapping := seededCardDAVConflictMapping(t)
	firstCapture := conflictCapture(mapping)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), firstCapture)
	require.NoError(err)
	retained := parseCardDAVRemoteForStoreTest(mapping.Href, firstCapture.RemoteETag, firstCapture.RemoteBody)

	snapshotRead := make(chan struct{})
	resume := make(chan struct{})
	st.SetCardDAVConflictResolutionSnapshotHookForTest(func() {
		close(snapshotRead)
		<-resume
	})
	t.Cleanup(func() { st.SetCardDAVConflictResolutionSnapshotHookForTest(nil) })
	resolved := make(chan error, 1)
	go func() {
		_, resolveErr := st.ResolveCardDAVConflictRemoteContext(context.Background(), store.CardDAVConflictRemoteResolution{
			ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision,
			Remote: retained,
		})
		resolved <- resolveErr
	}()
	select {
	case <-snapshotRead:
	case <-time.After(5 * time.Second):
		require.FailNow("keep-remote did not reach the unlocked identity snapshot")
	}

	current, err := st.GetCardDAVResourceContext(t.Context(), mapping.AddressBookID, mapping.Href)
	require.NoError(err)
	latestCapture := conflictCapture(current)
	latestCapture.RemoteETag = `"latest"`
	latestCapture.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-alice\r\nFN:Latest Remote\r\nEND:VCARD\r\n")
	latest, err := st.RecordCardDAVConflictContext(t.Context(), latestCapture)
	require.NoError(err)
	close(resume)

	select {
	case resolveErr := <-resolved:
		require.ErrorIs(resolveErr, store.ErrCardDAVConflictStale)
	case <-time.After(5 * time.Second):
		require.FailNow("keep-remote did not finish after conflict refresh")
	}
	after, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictUnresolved, after.Status)
	assert.Equal(latest.MappingRevision, after.MappingRevision)
	assert.Equal(latestCapture.RemoteBody, after.RemoteBody)
}

func TestCardDAVTombstonePreparationRevalidatesAfterUnlockedConflictIdentityRead(t *testing.T) {
	for _, test := range []struct {
		name         string
		mappedPerson bool
	}{
		{name: "deleted person", mappedPerson: false},
		{name: "mapped unpublish", mappedPerson: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, account, book, mapping, conflict, remote := seededCardDAVTombstoneConflict(t, test.mappedPerson)
			snapshotRead := make(chan struct{})
			resume := make(chan struct{})
			st.SetCardDAVConflictTombstonePreparationSnapshotHookForTest(func() {
				close(snapshotRead)
				<-resume
			})
			t.Cleanup(func() { st.SetCardDAVConflictTombstonePreparationSnapshotHookForTest(nil) })
			prepared := make(chan error, 1)
			go func() {
				_, prepareErr := st.PrepareCardDAVConflictLocalTombstoneContext(
					context.Background(), conflict.ID, conflict.MappingRevision, remote)
				prepared <- prepareErr
			}()
			select {
			case <-snapshotRead:
			case <-time.After(5 * time.Second):
				require.FailNow("tombstone preparation did not reach the unlocked identity snapshot")
			}

			plan, latestRemote := refreshedCardDAVTombstonePlan(t, st, account, book, mapping)
			_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), plan)
			require.NoError(err)
			close(resume)

			select {
			case prepareErr := <-prepared:
				require.ErrorIs(prepareErr, store.ErrCardDAVConflictStale)
			case <-time.After(5 * time.Second):
				require.FailNow("tombstone preparation did not finish after conflict refresh")
			}
			after, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
			require.NoError(err)
			assert.Equal(store.CardDAVConflictUnresolved, after.Status)
			assert.Empty(after.PendingOperation)
			assert.Equal(latestRemote.RemoteBody, after.RemoteBody)
			afterMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
			require.NoError(err)
			assert.Equal(after.MappingRevision, afterMapping.MappingRevision)
		})
	}
}

func TestCardDAVTombstonePreparationAndPullUseCanonicalPostgresLockOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book, mapping, conflict, remote := seededCardDAVTombstoneConflict(t, true)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks are required for the CardDAV tombstone preparation/pull deadlock regression")
	}
	plan, latestRemote := refreshedCardDAVTombstonePlan(t, st, account, book, mapping)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	var lockedBookID int64
	require.NoError(blocker.QueryRowContext(ctx,
		`SELECT id FROM carddav_address_books WHERE id = $1 FOR UPDATE`, book.ID).Scan(&lockedBookID))
	waitingBefore := postgreSQLWaitingLockCount(t, st)

	pullDone := make(chan error, 1)
	go func() {
		_, pullErr := st.ApplyCardDAVSyncPlanContext(ctx, plan)
		pullDone <- pullErr
	}()
	require.Eventually(func() bool {
		return postgreSQLWaitingLockCount(t, st) >= waitingBefore+1
	}, 5*time.Second, 10*time.Millisecond, "pull did not wait on the held book lock")

	prepareDone := make(chan error, 1)
	go func() {
		_, prepareErr := st.PrepareCardDAVConflictLocalTombstoneContext(
			ctx, conflict.ID, conflict.MappingRevision, remote)
		prepareDone <- prepareErr
	}()
	require.Eventually(func() bool {
		return postgreSQLWaitingLockCount(t, st) >= waitingBefore+2
	}, 5*time.Second, 10*time.Millisecond, "preparation did not wait behind the pull lock order")
	require.NoError(blocker.Commit())

	select {
	case pullErr := <-pullDone:
		require.NoError(pullErr, "pull must win the canonical lock order without a deadlock")
	case <-ctx.Done():
		require.FailNow("pull did not finish", ctx.Err())
	}
	select {
	case prepareErr := <-prepareDone:
		require.ErrorIs(prepareErr, store.ErrCardDAVConflictStale)
	case <-ctx.Done():
		require.FailNow("tombstone preparation did not finish", ctx.Err())
	}

	after, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(latestRemote.RemoteBody, after.RemoteBody)
	afterMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
	assert.Equal(after.MappingRevision, afterMapping.MappingRevision)
}

func TestCardDAVKeepRemoteAndPullUseCanonicalPostgresLockOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book, mapping := seededCardDAVConflictMapping(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks are required for the CardDAV resolution/pull deadlock regression")
	}
	firstCapture := conflictCapture(mapping)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), firstCapture)
	require.NoError(err)
	retained := parseCardDAVRemoteForStoreTest(mapping.Href, firstCapture.RemoteETag, firstCapture.RemoteBody)
	current, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
	latestRemote := remoteResource(mapping.Href, "remote-alice", "Latest Remote", "latest@example.test", `"latest"`)
	latestRemote.SemanticHash = "latest-semantic"
	latestCapture := conflictCapture(current)
	latestCapture.RemoteETag = latestRemote.RemoteETag
	latestCapture.RemoteBody = latestRemote.RemoteBody
	books, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	require.Len(books, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	var lockedBookID int64
	require.NoError(blocker.QueryRowContext(ctx,
		`SELECT id FROM carddav_address_books WHERE id = $1 FOR UPDATE`, book.ID).Scan(&lockedBookID))
	waitingBefore := postgreSQLWaitingLockCount(t, st)

	pullDone := make(chan error, 1)
	go func() {
		_, pullErr := st.ApplyCardDAVSyncPlanContext(ctx, store.CardDAVSyncPlan{
			AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
			SyncRevision: books[0].SyncRevision, NextSyncToken: "latest-token",
			Upserts:   []store.CardDAVRemoteResource{latestRemote},
			Conflicts: []store.CardDAVConflictCapture{latestCapture},
		})
		pullDone <- pullErr
	}()
	require.Eventually(func() bool {
		return postgreSQLWaitingLockCount(t, st) >= waitingBefore+1
	}, 5*time.Second, 10*time.Millisecond, "pull did not wait on the held book lock")

	resolveDone := make(chan error, 1)
	go func() {
		_, resolveErr := st.ResolveCardDAVConflictRemoteContext(ctx, store.CardDAVConflictRemoteResolution{
			ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision,
			Remote: retained,
		})
		resolveDone <- resolveErr
	}()
	require.Eventually(func() bool {
		return postgreSQLWaitingLockCount(t, st) >= waitingBefore+2
	}, 5*time.Second, 10*time.Millisecond, "resolution did not wait behind the pull book lock")
	require.NoError(blocker.Commit())

	select {
	case pullErr := <-pullDone:
		require.NoError(pullErr, "pull must win the canonical lock order without a deadlock")
	case <-ctx.Done():
		require.FailNow("pull did not finish", ctx.Err())
	}
	select {
	case resolveErr := <-resolveDone:
		require.ErrorIs(resolveErr, store.ErrCardDAVConflictStale,
			"resolution must revalidate to stale, got %v", resolveErr)
	case <-ctx.Done():
		require.FailNow("resolution did not finish", ctx.Err())
	}

	after, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictUnresolved, after.Status)
	assert.Equal(latestRemote.RemoteBody, after.RemoteBody)
	afterMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
	assert.Equal(after.MappingRevision, afterMapping.MappingRevision)
}

func TestCardDAVKeepRemoteReconcilesUnboundMappingByBookRole(t *testing.T) {
	for _, test := range []struct {
		name       string
		subscribed bool
	}{
		{name: "subscribed reimports", subscribed: true},
		{name: "lookup-only stays unbound", subscribed: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, account, book := newCardDAVResourceStore(t)
			base := remoteResource(book.CanonicalURL+"alice.vcf", "remote-alice", "Alice", "alice@example.test", `"one"`)
			_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
				AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
				SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{base},
			})
			require.NoError(err)
			mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, base.Href)
			require.NoError(err)
			require.NotNil(mapping.PersonID)
			person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
			require.NoError(err)
			require.NoError(st.DeletePersonContext(t.Context(), person.ID, person.Revision))
			if !test.subscribed {
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET
					is_write_target = FALSE, is_subscribed = FALSE,
					is_lookup_source = TRUE WHERE id = ?`), book.ID)
				require.NoError(err)
			}
			unbound, err := st.GetCardDAVResourceContext(t.Context(), book.ID, base.Href)
			require.NoError(err)
			require.Nil(unbound.PersonID)
			latest := remoteResource(base.Href, "remote-alice", "Alice Retained", "alice-new@example.test", `"two"`)
			latest.SemanticHash = "semantic-latest"
			conflict, err := st.RecordCardDAVConflictContext(t.Context(), store.CardDAVConflictCapture{
				AddressBookID: book.ID, Href: unbound.Href,
				ExpectedMappingRevision: unbound.MappingRevision,
				BaseLocalHash:           unbound.LocalHash, LocalHash: unbound.LocalHash,
				BaseRemoteHash: unbound.RemoteSemanticHash, BaseRemoteETag: unbound.RemoteETag,
				RemoteETag: latest.RemoteETag, RemoteBody: latest.RemoteBody,
				LocalTombstone: true,
			})
			require.NoError(err)

			_, err = st.ResolveCardDAVConflictRemoteContext(t.Context(), store.CardDAVConflictRemoteResolution{
				ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision,
				Remote: latest,
			})
			require.NoError(err)
			after, err := st.GetCardDAVResourceContext(t.Context(), book.ID, base.Href)
			require.NoError(err)
			assert.Equal(latest.RemoteBody, after.RemoteBody)
			if test.subscribed {
				require.NotNil(after.PersonID)
				assert.NotEqual(person.ID, *after.PersonID)
				assert.Equal(store.CardDAVGovernanceRemote, after.Governance)
				envelope, envelopeErr := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", base.Href)
				require.NoError(envelopeErr)
				assert.Equal(latest.RemoteBody, envelope.StoredBody)
				assert.Equal(*after.PersonID, envelope.PersonID)
			} else {
				assert.Nil(after.PersonID)
				assert.Equal(store.CardDAVMappingUnbound, after.MappingStatus)
			}
			publicationIDs, err := st.ListCardDAVPublicationPersonIDsContext(t.Context())
			require.NoError(err)
			assert.Empty(publicationIDs, "keep-remote must not synthesize publication replay state")
		})
	}
}

func TestCardDAVResolvedConflictSweepKeepsThirtyDayAuditWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, _, mapping := seededCardDAVConflictMapping(t)
	capture := conflictCapture(mapping)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	retained := parseCardDAVRemoteForStoreTest(mapping.Href, capture.RemoteETag, capture.RemoteBody)
	resolved, err := st.ResolveCardDAVConflictRemoteContext(t.Context(), store.CardDAVConflictRemoteResolution{
		ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision, Remote: retained,
	})
	require.NoError(err)
	require.NotNil(resolved.ResolvedAt)

	removed, err := st.SweepResolvedCardDAVConflictsContext(t.Context(), resolved.ResolvedAt.Add(30*24*time.Hour))
	require.NoError(err)
	assert.Equal(int64(0), removed)
	removed, err = st.SweepResolvedCardDAVConflictsContext(t.Context(), resolved.ResolvedAt.Add(30*24*time.Hour+time.Second))
	require.NoError(err)
	assert.Equal(int64(1), removed)
}

func parseCardDAVRemoteForStoreTest(href, etag string, body []byte) store.CardDAVRemoteResource {
	return store.CardDAVRemoteResource{
		Href: href, RemoteUID: "remote-alice", RemoteETag: etag,
		RemoteBody: body, SemanticHash: "retained-remote-hash", DisplayName: "Remote",
	}
}
