package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
)

func TestTierEnrichmentParticipantRepairStillInvalidatesIneligiblePeople(t *testing.T) {
	for _, p := range []tierEnrichmentPersona{tierEnrichmentPersonas[2], tierEnrichmentPersonas[3]} {
		for _, identity := range []string{"name", "email"} {
			t.Run(p.name+"/"+identity, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := newTierEnrichmentFixture(t)
				setTierEnrichmentPersona(t, f, p)
				var participantID int64
				require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id=?`), f.person.ID).Scan(&participantID))
				before, err := f.store.GetPersonContext(t.Context(), f.person.ID)
				require.NoError(err)
				switch identity {
				case "name":
					err = f.store.RepairParticipantDisplayNames([]store.ParticipantDisplayNameRepair{{ParticipantID: participantID, DisplayName: "Repaired Synthetic Person"}})
				case "email":
					err = f.store.RepairParticipantEmailAddresses([]store.ParticipantEmailRepair{{ParticipantID: participantID, EmailAddress: "repaired-tier@example.test"}})
				}
				require.NoError(err)
				after, err := f.store.GetPersonContext(t.Context(), f.person.ID)
				require.NoError(err)
				assert.Greater(after.Revision, before.Revision)
				assert.Empty(f.work(t))
			})
		}
	}
}

func TestTierEnrichmentConcurrentOptOutCannotLeaveScheduledWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory locks expose the authorization-to-publication interleaving")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	blocker, err := f.store.DB().Conn(ctx)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(blocker.Close()) })
	var blockerPID int
	require.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	_, err = blocker.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended(current_schema() || ':tier-enqueue-test',0))`)
	require.NoError(err)
	defer func() {
		_, _ = blocker.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended(current_schema() || ':tier-enqueue-test',0))`)
	}()
	// The trigger pauses the real INSERT after the preceding authority SELECT.
	// Unlike a Store hook it observes the PostgreSQL write boundary directly.
	_, err = f.store.DB().ExecContext(ctx, `CREATE FUNCTION hold_tier_enqueue_test() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended(TG_TABLE_SCHEMA || ':tier-enqueue-test',0));
  RETURN NEW;
 END $$`)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(ctx, `CREATE TRIGGER hold_tier_enqueue_test BEFORE INSERT ON person_enrichment_work
 FOR EACH ROW EXECUTE FUNCTION hold_tier_enqueue_test()`)
	require.NoError(err)
	enqueueDone := make(chan error, 1)
	go func() {
		enqueueDone <- f.store.EnqueuePersonEnrichmentContext(ctx, store.EnrichmentTriggerInput{PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Kind: personenrichment.TriggerTracked, Generation: "concurrent-opt-out", DueAt: f.now})
	}()
	var enqueuePID int
	var pollErr error
	require.Eventually(func() bool {
		rows, err := f.store.DB().QueryContext(ctx, f.store.Rebind(`SELECT pid FROM pg_stat_activity WHERE ? = ANY(pg_blocking_pids(pid))`), blockerPID)
		if err != nil {
			pollErr = err
			return true
		}
		defer func() { require.NoError(rows.Close()) }()
		if rows.Next() {
			pollErr = rows.Scan(&enqueuePID)
			return true
		}
		pollErr = rows.Err()
		return pollErr != nil
	}, 30*time.Second, 10*time.Millisecond, "enqueue never reached the held insert boundary")
	require.NoError(pollErr)
	optOutDone := make(chan error, 1)
	go func() {
		optOutDone <- f.store.SetPersonTierOptOutContext(ctx, f.person.ID, "enrichment", true, "test", "concurrent cancellation")
	}()
	finishedOptOut := false
	var optOutErr error
	require.Eventually(func() bool {
		select {
		case optOutErr = <-optOutDone:
			finishedOptOut = true
			return true
		default:
		}
		var blocked bool
		pollErr = f.store.DB().QueryRowContext(ctx, f.store.Rebind(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE ? = ANY(pg_blocking_pids(pid)))`), enqueuePID).Scan(&blocked)
		return pollErr != nil || blocked
	}, 30*time.Second, 10*time.Millisecond, "opt-out neither completed nor waited for enqueue authority")
	require.NoError(pollErr)
	_, err = blocker.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended(current_schema() || ':tier-enqueue-test',0))`)
	require.NoError(err)
	require.NoError(<-enqueueDone)
	if !finishedOptOut {
		optOutErr = <-optOutDone
	}
	require.NoError(optOutErr)
	assert.Empty(f.work(t))
}
