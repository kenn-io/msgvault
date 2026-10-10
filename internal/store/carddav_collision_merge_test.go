package store

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sync/atomic"
	"testing"
	"time"
)

func TestCardDAVCreateCollisionAndMergeUseCompatibleLocks(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, id, _ := newPersonFactProjectionStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL row-lock scheduling")
	}
	book := inferenceMigrationBook(t, st)
	person, err := st.GetPersonContext(t.Context(), id)
	requirements.NoError(err)
	var otherID int64
	requirements.NoError(st.db.QueryRow(`INSERT INTO persons(vcard_uid,display_name) VALUES ('collision-survivor','Collision Survivor') RETURNING id`).Scan(&otherID))
	other, err := st.GetPersonContext(t.Context(), otherID)
	requirements.NoError(err)
	source, err := st.LoadPersonVCardSnapshotContext(t.Context(), id)
	requirements.NoError(err)
	body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:%s\r\nFN:Local\r\nEND:VCARD\r\n", person.VCardUID))
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), CardDAVPublicationPlan{PersonID: id, Desired: true, AddressBookID: book.ID, Href: book.CanonicalURL + "collision.vcf", OutgoingBody: body, OutgoingSemanticHash: "local", LocalHash: source.Fingerprint})
	requirements.NoError(err)
	remote := CardDAVRemoteResource{Href: pending.Href, RemoteUID: "remote", RemoteETag: `"remote"`, RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:remote\r\nFN:Remote\r\nEND:VCARD\r\n"), SemanticHash: "remote"}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	reached, resume := make(chan struct{}), make(chan struct{})
	st.cardDAVCollisionIdentityLockHook = func() {
		close(reached)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	}
	defer func() { st.cardDAVCollisionIdentityLockHook = nil }()
	collisionDone := make(chan error, 1)
	go func() { _, err := st.FenceCardDAVCreateCollisionContext(ctx, *pending, remote); collisionDone <- err }()
	<-reached
	var attempts atomic.Int64
	st.personOperationBeforeIdentityLockHook = func() { attempts.Add(1) }
	defer func() { st.personOperationBeforeIdentityLockHook = nil }()
	mergeDone := make(chan error, 1)
	go func() {
		_, err := st.MergePersonsContext(ctx, PersonMergeRequest{SurvivorID: otherID, AbsorbedID: id, ExpectedSurvivorRevision: other.Revision, ExpectedAbsorbedRevision: person.Revision, Actor: "test", IdempotencyKey: "collision-merge"})
		mergeDone <- err
	}()
	// Both operations take the identity fence before account or person locks.
	// Observe the merge waiting on that fence, then let collision commit so
	// merge can recheck the pending publication and reject it.
	assertions.Eventually(func() bool {
		var blocked bool
		err := st.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND query LIKE '%archive_metadata%' AND cardinality(pg_blocking_pids(pid)) > 0)`).Scan(&blocked)
		return err == nil && blocked
	}, 10*time.Second, 10*time.Millisecond)
	close(resume)
	requirements.NoError(<-collisionDone)
	mergeErr := <-mergeDone
	requirements.ErrorIs(mergeErr, ErrPersonCardDAVPublished)
	assertions.Equal(int64(1), attempts.Load(), "merge must not require a retry after a deadlock")
}
