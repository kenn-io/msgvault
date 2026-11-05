package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvePersonReferenceKeepsMembershipAcrossPromotion(t *testing.T) {
	gate := newGmailAuditSnapshotGate(0)
	gate.query = "WITH RECURSIVE visible_members AS"
	var st *Store
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		st = newGmailAuditSnapshotPostgresStore(t, gate)
	} else {
		st = newGmailAuditSnapshotSQLiteStore(t, gate)
	}
	peer, err := st.EnsureParticipant("peer@example.com", "Peer", "example.com")
	require.NoError(t, err)
	writer, err := OpenForTest(st.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, writer.Close()) })
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), gmailAuditSnapshotGateKey{}, gate), 30*time.Second)
	defer cancel()
	type result struct {
		candidates []PersonCandidate
		err        error
	}
	done := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		candidates, err := st.ResolvePersonReferenceContext(ctx, "email", "peer@example.com", nil, 10)
		done <- result{candidates, err}
	}()
	defer func() {
		gate.resume()
		cancel()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			assert.Fail(t, "reference reader did not finish")
		}
	}()
	select {
	case <-gate.paused:
	case <-ctx.Done():
		require.FailNow(t, "reference membership query did not reach the gate")
	}
	person, _, err := writer.CreatePersonFromParticipant(peer)
	require.NoError(t, err)
	gate.resume()
	got := <-done
	require.NoError(t, got.err)
	require.Len(t, got.candidates, 1)
	assert.Zero(t, got.candidates[0].PersonID)
	assert.Equal(t, []int64{peer}, got.candidates[0].ParticipantIDs)
	fresh, err := st.ResolvePersonReferenceContext(t.Context(), "email", "peer@example.com", nil, 10)
	require.NoError(t, err)
	require.Len(t, fresh, 1)
	assert.Equal(t, person.ID, fresh[0].PersonID)
	assert.Equal(t, []int64{peer}, fresh[0].ParticipantIDs)
}
