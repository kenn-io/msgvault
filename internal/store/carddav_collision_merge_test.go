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
	require := require.New(t)
	assert := assert.New(t)
	st, id, _ := newPersonFactProjectionStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL row-lock scheduling")
	}
	book := inferenceMigrationBook(t, st)
	person, err := st.GetPersonContext(t.Context(), id)
	require.NoError(err)
	var otherID int64
	require.NoError(st.db.QueryRow(`INSERT INTO persons(vcard_uid,display_name) VALUES ('collision-survivor','Collision Survivor') RETURNING id`).Scan(&otherID))
	other, err := st.GetPersonContext(t.Context(), otherID)
	require.NoError(err)
	source, err := st.LoadPersonVCardSnapshotContext(t.Context(), id)
	require.NoError(err)
	body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:%s\r\nFN:Local\r\nEND:VCARD\r\n", person.VCardUID))
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), CardDAVPublicationPlan{PersonID: id, Desired: true, AddressBookID: book.ID, Href: book.CanonicalURL + "collision.vcf", OutgoingBody: body, OutgoingSemanticHash: "local", LocalHash: source.Fingerprint})
	require.NoError(err)
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
	// Merge must reject the pending publication while the collision is still
	// paused. The context bounds a regression that blocks on its person lock.
	mergeErr := <-mergeDone
	close(resume)
	require.NoError(<-collisionDone)
	require.ErrorIs(mergeErr, ErrPersonCardDAVPublished)
	assert.Equal(int64(1), attempts.Load(), "merge must not require a retry after a deadlock")
}
