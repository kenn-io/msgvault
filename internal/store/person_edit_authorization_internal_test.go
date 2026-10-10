package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonEditAuthorizationFencesConcurrentRelationshipProjection(t *testing.T) {
	dbURL := skipUnlessPostgresInternal(t)
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newPGStoreInternal(t, dbURL)
	firstParticipant, err := st.EnsureParticipant("first@example.test", "First Example", "example.test")
	requirements.NoError(err)
	secondParticipant, err := st.EnsureParticipant("second@example.test", "Second Example", "example.test")
	requirements.NoError(err)
	first, _, err := st.CreatePersonFromParticipant(firstParticipant)
	requirements.NoError(err)
	second, _, err := st.CreatePersonFromParticipant(secondParticipant)
	requirements.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	authorized := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	editDone := make(chan error, 1)
	go func() {
		_, err := st.UpdatePersonDisplayNameAuthorizedContext(ctx, first.ID, first.Revision, new("Changed Example"), func(_ context.Context, scope *IdentityGrantSelection) error {
			if len(scope.Persons) != 1 || scope.Persons[0].ID != first.ID {
				return errors.New("unexpected synthetic affected-person scope")
			}
			close(authorized)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		editDone <- err
	}()
	select {
	case <-authorized:
	case <-ctx.Done():
		requirements.FailNow("person edit did not reach authorization")
	}
	relationshipDone := make(chan error, 1)
	go func() {
		_, err := st.AddPersonRelationshipContext(ctx, PersonRelationshipInput{SourcePersonID: first.ID, TargetPersonID: second.ID, TypeSlug: "friend", Source: ProvenanceUser, Actor: "synthetic-owner"})
		relationshipDone <- err
	}()
	assertions.Eventually(func() bool {
		var waiting bool
		err := st.db.QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM pg_stat_activity writer WHERE writer.datname = current_database()
 AND writer.wait_event_type = 'Lock'
 AND EXISTS (SELECT 1 FROM pg_locks relation_lock WHERE relation_lock.pid = writer.pid
 AND relation_lock.relation = 'persons'::regclass)
 AND EXISTS (SELECT 1 FROM pg_locks blocker_lock
 WHERE blocker_lock.pid = ANY(pg_blocking_pids(writer.pid))
 AND blocker_lock.relation = 'persons'::regclass
 AND blocker_lock.granted AND blocker_lock.mode = 'RowShareLock'))`).Scan(&waiting)
		return err == nil && waiting
	}, 15*time.Second, 10*time.Millisecond)
	close(release)
	requirements.NoError(<-editDone)
	requirements.NoError(<-relationshipDone)
	after, err := st.PersonEditScopeContext(ctx, first.ID)
	requirements.NoError(err)
	ids := make([]int64, len(after.Persons))
	for i, person := range after.Persons {
		ids[i] = person.ID
	}
	assertions.ElementsMatch([]int64{first.ID, second.ID}, ids)
}
