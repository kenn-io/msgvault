package store_test

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMergePersons_TierAuthorityAndHistorySurviveExactSplit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor := mustPromotedPerson(t, f.Store, "tier-record-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, f.Store, "tier-record-absorbed@example.com", "Absorbed")

	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		INSERT INTO person_tier_state (
			person_id, tier, tier_since, candidate_tier, candidate_since, lanes_json,
			enrichment_eligible, rule, rule_version, config_fingerprint, as_of_local_date,
			run_id, run_started_at, applied_at
		) VALUES (?, 2, '2026-10-17T00:00:00Z', NULL, NULL, '["tracking"]',
			false, 'tier2_two_way', 1, 'absorbed-config', '2026-10-17',
			'absorbed-run', '2026-10-17T00:00:00Z', '2026-10-17T00:00:00Z')`), absorbed.ID)
	require.NoError(err)
	for personID, fingerprint := range map[int64]string{
		survivor.ID: "survivor-config",
		absorbed.ID: "absorbed-config",
	} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
			INSERT INTO person_tier_observations (
				person_id, computed_tier, rule, signals_json, shadow_tier, shadow_since,
				shadow_candidate_tier, shadow_candidate_since, rule_version, config_fingerprint,
				as_of_local_date, activity_ready, evaluated_at
			) VALUES (?, 2, 'tier2_two_way', '{}', 2, '2026-10-17', NULL, NULL, 1, ?,
				'2026-10-17', true, '2026-10-17T00:00:00Z')`), personID, fingerprint)
		require.NoError(err)
	}
	var eventID int64
	err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		INSERT INTO person_tier_events (
			person_id, from_tier, to_tier, reason, mode, lanes_json, signals_json,
			rule, rule_version, actor, run_id
		) VALUES (?, 3, 2, 'promotion', 'apply', '{}', '{}', 'tier2_two_way', 1,
			'test', 'absorbed-run') RETURNING id`), absorbed.ID).Scan(&eventID)
	require.NoError(err)
	var inboundEventID int64
	err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		INSERT INTO person_tier_events (
			person_id, merged_from_person_id, from_tier, to_tier, reason, mode,
			lanes_json, signals_json, rule, rule_version, actor, run_id
		) VALUES (?, ?, 2, 1, 'demotion', 'apply', '{}', '{}', 'tier2_two_way', 1,
			'test', 'survivor-run') RETURNING id`), survivor.ID, absorbed.ID).Scan(&inboundEventID)
	require.NoError(err)

	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "tier-record-merge", Actor: "test",
	})
	require.NoError(err)

	var statePersonID int64
	var stateRunID string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT person_id, run_id FROM person_tier_state WHERE person_id = ?`), survivor.ID).
		Scan(&statePersonID, &stateRunID))
	assert.Equal(survivor.ID, statePersonID)
	assert.Equal("absorbed-run", stateRunID)
	var observationFingerprint string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT config_fingerprint FROM person_tier_observations WHERE person_id = ?`), survivor.ID).
		Scan(&observationFingerprint))
	assert.Equal("survivor-config", observationFingerprint, "survivor observation wins a duplicate")
	var eventPersonID int64
	var mergedFrom sql.NullInt64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT person_id, merged_from_person_id FROM person_tier_events WHERE id = ?`), eventID).
		Scan(&eventPersonID, &mergedFrom))
	assert.Equal(survivor.ID, eventPersonID)
	assert.True(mergedFrom.Valid)
	assert.Equal(absorbed.ID, mergedFrom.Int64)
	var inboundMergedFrom sql.NullInt64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT merged_from_person_id FROM person_tier_events WHERE id = ?`), inboundEventID).
		Scan(&inboundMergedFrom))
	assert.True(inboundMergedFrom.Valid)
	assert.Equal(survivor.ID, inboundMergedFrom.Int64)
	var inboundEventAction string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT action FROM person_merge_rows
		WHERE merge_id = ? AND table_name = 'person_tier_events' AND original_row_id = ?`),
		merged.Merge.ID, inboundEventID).Scan(&inboundEventAction))
	assert.Equal("repointed", inboundEventAction)

	for table, wantAction := range map[string]string{
		"person_tier_state":        "moved",
		"person_tier_observations": "deduplicated",
		"person_tier_events":       "moved",
	} {
		originalRowID := absorbed.ID
		if table == "person_tier_events" {
			originalRowID = eventID
		}
		var action string
		var currentRowID int64
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
			SELECT action, current_row_id FROM person_merge_rows
			WHERE merge_id = ? AND table_name = ? AND original_row_id = ?`),
			merged.Merge.ID, table, originalRowID).Scan(&action, &currentRowID))
		assert.Equal(wantAction, action)
		if table == "person_tier_state" || table == "person_tier_observations" {
			assert.Equal(survivor.ID, currentRowID)
		} else {
			assert.Equal(eventID, currentRowID)
		}
	}

	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "tier-record-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
	var restoredStatePersonID int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT person_id FROM person_tier_state WHERE run_id = 'absorbed-run'`)).
		Scan(&restoredStatePersonID))
	assert.Equal(split.NewPerson.ID, restoredStatePersonID)
	var restoredObservationFingerprint string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT config_fingerprint FROM person_tier_observations WHERE person_id = ?`), split.NewPerson.ID).
		Scan(&restoredObservationFingerprint))
	assert.Equal("absorbed-config", restoredObservationFingerprint)
	var restoredEventPersonID int64
	var restoredMergedFrom sql.NullInt64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT person_id, merged_from_person_id FROM person_tier_events WHERE id = ?`), eventID).
		Scan(&restoredEventPersonID, &restoredMergedFrom))
	assert.Equal(split.NewPerson.ID, restoredEventPersonID)
	assert.False(restoredMergedFrom.Valid)
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT person_id, merged_from_person_id FROM person_tier_events WHERE id = ?`), inboundEventID).
		Scan(&eventPersonID, &inboundMergedFrom))
	assert.Equal(split.SourcePerson.ID, eventPersonID)
	assert.True(inboundMergedFrom.Valid)
	assert.Equal(absorbed.ID, inboundMergedFrom.Int64,
		"historical origin preserves the retired absorbed identity")
}

func TestSplitPersonMerge_KeepsRewrittenTierSingletonsWithSurvivor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor := mustPromotedPerson(t, f.Store, "rewritten-tier-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, f.Store, "rewritten-tier-absorbed@example.com", "Absorbed")

	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		INSERT INTO person_tier_state (
			person_id, tier, tier_since, candidate_tier, candidate_since, lanes_json,
			enrichment_eligible, rule, rule_version, config_fingerprint, as_of_local_date,
			run_id, run_started_at, applied_at
		) VALUES (?, 2, '2026-10-17T00:00:00Z', NULL, NULL, '["tracking"]',
			false, 'tier2_two_way', 1, 'absorbed-config', '2026-10-17',
			'absorbed-run', '2026-10-17T00:00:00Z', '2026-10-17T00:00:00Z')`), absorbed.ID)
	require.NoError(err)
	for personID, fingerprint := range map[int64]string{
		survivor.ID: "survivor-config",
		absorbed.ID: "absorbed-config",
	} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
			INSERT INTO person_tier_observations (
				person_id, computed_tier, rule, signals_json, shadow_tier, shadow_since,
				shadow_candidate_tier, shadow_candidate_since, rule_version, config_fingerprint,
				as_of_local_date, activity_ready, evaluated_at
			) VALUES (?, 2, 'tier2_two_way', '{}', 2, '2026-10-17', NULL, NULL, 1, ?,
				'2026-10-17', true, '2026-10-17T00:00:00Z')`), personID, fingerprint)
		require.NoError(err)
	}
	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "rewritten-tier-merge", Actor: "test",
	})
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE person_tier_state SET tier = 1, run_id = 'survivor-after-merge'
		WHERE person_id = ?`), merged.Person.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE person_tier_observations SET computed_tier = 1,
			config_fingerprint = 'survivor-after-merge'
		WHERE person_id = ?`), merged.Person.ID)
	require.NoError(err)

	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "rewritten-tier-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
	var sourceStateRun, restoredStateRun string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT run_id FROM person_tier_state WHERE person_id = ?`), split.SourcePerson.ID).
		Scan(&sourceStateRun))
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT run_id FROM person_tier_state WHERE person_id = ?`), split.NewPerson.ID).
		Scan(&restoredStateRun))
	assert.Equal("survivor-after-merge", sourceStateRun)
	assert.Equal("absorbed-run", restoredStateRun)
	var sourceObservation, restoredObservation string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT config_fingerprint FROM person_tier_observations WHERE person_id = ?`), split.SourcePerson.ID).
		Scan(&sourceObservation))
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT config_fingerprint FROM person_tier_observations WHERE person_id = ?`), split.NewPerson.ID).
		Scan(&restoredObservation))
	assert.Equal("survivor-after-merge", sourceObservation)
	assert.Equal("absorbed-config", restoredObservation)
}

func TestSplitPersonMerge_PreservesSameActorReenabledMovedGrants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor := mustPromotedPerson(t, f.Store, "same-actor-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, f.Store, "same-actor-absorbed@example.com", "Absorbed")
	ctx := t.Context()

	_, err := f.Store.SetPersonTrackingContext(ctx, absorbed.ID, true)
	require.NoError(err)
	_, err = f.Store.SetPersonBriefEnrollmentContext(ctx, absorbed.ID, true, "owner", false)
	require.NoError(err)
	originalAt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`
		UPDATE person_tracking SET tracked_at = ? WHERE person_id = ?`), originalAt, absorbed.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`
		UPDATE person_brief_enrollments SET enabled_at = ? WHERE person_id = ?`), originalAt, absorbed.ID)
	require.NoError(err)

	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "same-actor-reenabled-merge", Actor: "test",
	})
	require.NoError(err)
	_, err = f.Store.SetPersonTrackingContext(ctx, merged.Person.ID, false)
	require.NoError(err)
	replacementTracking, err := f.Store.SetPersonTrackingContext(ctx, merged.Person.ID, true)
	require.NoError(err)
	_, err = f.Store.SetPersonBriefEnrollmentContext(ctx, merged.Person.ID, false, "owner", false)
	require.NoError(err)
	replacementEnrollment, err := f.Store.SetPersonBriefEnrollmentContext(
		ctx, merged.Person.ID, true, "owner", false,
	)
	require.NoError(err)
	require.NotNil(replacementTracking.TrackedAt)
	require.NotNil(replacementEnrollment.EnabledAt)

	split, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "same-actor-reenabled-split", Actor: "test",
	})
	require.NoError(err)
	sourceTracking, err := f.Store.GetPersonTrackingContext(ctx, split.SourcePerson.ID)
	require.NoError(err)
	newTracking, err := f.Store.GetPersonTrackingContext(ctx, split.NewPerson.ID)
	require.NoError(err)
	assert.True(sourceTracking.Tracked)
	require.NotNil(sourceTracking.TrackedAt)
	assert.True(replacementTracking.TrackedAt.Equal(*sourceTracking.TrackedAt))
	assert.True(newTracking.Tracked)
	require.NotNil(newTracking.TrackedAt)
	assert.True(originalAt.Equal(*newTracking.TrackedAt))

	sourceEnrollment, err := f.Store.GetPersonBriefEnrollmentContext(ctx, split.SourcePerson.ID)
	require.NoError(err)
	newEnrollment, err := f.Store.GetPersonBriefEnrollmentContext(ctx, split.NewPerson.ID)
	require.NoError(err)
	assert.True(sourceEnrollment.Enrolled)
	assert.Equal("owner", sourceEnrollment.Actor)
	require.NotNil(sourceEnrollment.EnabledAt)
	assert.True(replacementEnrollment.EnabledAt.Equal(*sourceEnrollment.EnabledAt))
	assert.True(newEnrollment.Enrolled)
	assert.Equal("owner", newEnrollment.Actor)
	require.NotNil(newEnrollment.EnabledAt)
	assert.True(originalAt.Equal(*newEnrollment.EnabledAt))
}

func TestMergePersons_ManualGrantsOverrideAutomaticGrants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor := mustPromotedPerson(t, f.Store, "tier-grant-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, f.Store, "tier-grant-absorbed@example.com", "Absorbed")

	for personID, grant := range map[int64]struct {
		provenance string
		actor      string
	}{
		survivor.ID: {provenance: "auto", actor: "tier-run-survivor"},
		absorbed.ID: {provenance: "manual", actor: "operator-absorbed"},
	} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
			INSERT INTO person_tracking (person_id, tracked_at, provenance, actor)
			VALUES (?, '2026-10-16T00:00:00Z', ?, ?)`), personID, grant.provenance, grant.actor)
		require.NoError(err)
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
			INSERT INTO person_brief_enrollments (person_id, enabled_at, actor, provenance)
			VALUES (?, '2026-10-16T00:00:00Z', ?, ?)`), personID, grant.actor, grant.provenance)
		require.NoError(err)
	}

	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "tier-manual-grant-merge", Actor: "test",
	})
	require.NoError(err)

	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		var provenance, actor string
		query := "SELECT provenance, actor FROM " + table + " WHERE person_id = ?"
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(query), survivor.ID).
			Scan(&provenance, &actor))
		assert.Equal("manual", provenance, table)
		assert.Equal("operator-absorbed", actor, table)

		var survivorPostState string
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
			SELECT post_merge_row_json FROM person_merge_rows
			WHERE merge_id = ? AND table_name = ? AND origin_side = 'survivor'`),
			merged.Merge.ID, table).Scan(&survivorPostState))
		assert.NotEmpty(survivorPostState, table+" survivor change must be journaled for split")
	}

	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "tier-manual-grant-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
	for _, test := range []struct {
		table string
	}{
		{table: "person_tracking"},
		{table: "person_brief_enrollments"},
	} {
		var provenance, actor string
		query := "SELECT provenance, actor FROM " + test.table + " WHERE person_id = ?"
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(query), split.SourcePerson.ID).
			Scan(&provenance, &actor))
		assert.Equal("auto", provenance, test.table+" survivor provenance")
		assert.Equal("tier-run-survivor", actor, test.table+" survivor actor")

		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(query), split.NewPerson.ID).
			Scan(&provenance, &actor))
		assert.Equal("manual", provenance, test.table+" absorbed provenance")
		assert.Equal("operator-absorbed", actor, test.table+" absorbed actor")
	}
}

func TestSplitPersonMerge_PreservesReenabledSurvivorBriefEnrollment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor := mustPromotedPerson(t, f.Store, "reenabled-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, f.Store, "reenabled-absorbed@example.com", "Absorbed")

	for _, grant := range []struct {
		personID   int64
		actor      string
		provenance string
	}{
		{personID: survivor.ID, actor: "tier-run-survivor", provenance: "auto"},
		{personID: absorbed.ID, actor: "operator-absorbed", provenance: "manual"},
	} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
			INSERT INTO person_brief_enrollments (person_id, enabled_at, actor, provenance)
			VALUES (?, '2026-10-16T00:00:00Z', ?, ?)`), grant.personID, grant.actor, grant.provenance)
		require.NoError(err)
	}

	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "reenabled-brief-merge", Actor: "test",
	})
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		DELETE FROM person_brief_enrollments WHERE person_id = ?`), survivor.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		INSERT INTO person_brief_enrollments (person_id, enabled_at, actor, provenance)
		VALUES (?, '2026-10-18T00:00:00Z', 'operator-reenabled', 'manual')`), survivor.ID)
	require.NoError(err)

	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "reenabled-brief-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
	var actor, provenance string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT actor, provenance FROM person_brief_enrollments WHERE person_id = ?`),
		split.SourcePerson.ID).Scan(&actor, &provenance))
	assert.Equal("operator-reenabled", actor)
	assert.Equal("manual", provenance)
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT actor, provenance FROM person_brief_enrollments WHERE person_id = ?`),
		split.NewPerson.ID).Scan(&actor, &provenance))
	assert.Equal("operator-absorbed", actor)
	assert.Equal("manual", provenance)
}

func TestSplitPersonMerge_PreservesReenabledSurvivorTracking(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor := mustPromotedPerson(t, f.Store, "reenabled-tracking-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, f.Store, "reenabled-tracking-absorbed@example.com", "Absorbed")

	for _, grant := range []struct {
		personID   int64
		actor      string
		provenance string
	}{
		{personID: survivor.ID, actor: "tier-run-survivor", provenance: "auto"},
		{personID: absorbed.ID, actor: "operator-absorbed", provenance: "manual"},
	} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
			INSERT INTO person_tracking (person_id, tracked_at, actor, provenance)
			VALUES (?, '2026-10-16T00:00:00Z', ?, ?)`), grant.personID, grant.actor, grant.provenance)
		require.NoError(err)
	}

	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "reenabled-tracking-merge", Actor: "test",
	})
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		DELETE FROM person_tracking WHERE person_id = ?`), survivor.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		INSERT INTO person_tracking (person_id, tracked_at, actor, provenance)
		VALUES (?, '2026-10-18T00:00:00Z', 'operator-reenabled', 'manual')`), survivor.ID)
	require.NoError(err)

	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "reenabled-tracking-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
	var actor, provenance string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT actor, provenance FROM person_tracking WHERE person_id = ?`),
		split.SourcePerson.ID).Scan(&actor, &provenance))
	assert.Equal("operator-reenabled", actor)
	assert.Equal("manual", provenance)
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT actor, provenance FROM person_tracking WHERE person_id = ?`),
		split.NewPerson.ID).Scan(&actor, &provenance))
	assert.Equal("operator-absorbed", actor)
	assert.Equal("manual", provenance)
}

func TestSplitPersonMerge_LegacyTierGrantSnapshotsUseMigrationDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor := mustPromotedPerson(t, f.Store, "legacy-tier-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, f.Store, "legacy-tier-absorbed@example.com", "Absorbed")

	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		INSERT INTO person_tracking (person_id, tracked_at, provenance, actor)
		VALUES (?, '2026-10-16T00:00:00Z', 'manual', 'legacy-operator')`), absorbed.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		INSERT INTO person_brief_enrollments (person_id, enabled_at, actor, provenance)
		VALUES (?, '2026-10-16T00:00:00Z', 'legacy-operator', 'manual')`), absorbed.ID)
	require.NoError(err)

	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "legacy-tier-grant-merge", Actor: "test",
	})
	require.NoError(err)

	// A v10 merge journal did not capture the columns added in v11. The
	// migration fills those columns with their defaults on the live row.
	removePersonMergePostColumns(t, f.Store, merged.Merge.ID, "person_tracking", absorbed.ID,
		"provenance", "actor")
	removePersonMergePostColumns(t, f.Store, merged.Merge.ID, "person_brief_enrollments", absorbed.ID,
		"provenance")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE person_tracking SET provenance = 'manual', actor = '' WHERE person_id = ?`), survivor.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		UPDATE person_brief_enrollments SET provenance = 'manual' WHERE person_id = ?`), survivor.ID)
	require.NoError(err)

	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "legacy-tier-grant-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		query := "SELECT COUNT(*) FROM " + table + " WHERE person_id = ?"
		var survivorCount, restoredCount int
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(query), split.SourcePerson.ID).
			Scan(&survivorCount))
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(query), split.NewPerson.ID).
			Scan(&restoredCount))
		assert.Equal(0, survivorCount, table+" must leave the absorbed-only grant off the survivor")
		assert.Equal(1, restoredCount, table+" must restore the absorbed-only grant")
	}
}

func TestSplitPersonMerge_ChainedTierEventJournalRestoresOriginalOrigin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	original := mustPromotedPerson(t, f.Store, "tier-chain-original@example.com", "Original")
	middle := mustPromotedPerson(t, f.Store, "tier-chain-middle@example.com", "Middle")
	final := mustPromotedPerson(t, f.Store, "tier-chain-final@example.com", "Final")
	observer := mustPromotedPerson(t, f.Store, "tier-chain-observer@example.com", "Observer")
	eventID := insertPersonTierEvent(t, f.Store, original.ID, nil, "tier-chain-original-run")
	inboundEventID := insertPersonTierEvent(
		t, f.Store, observer.ID, original.ID, "tier-chain-inbound-run",
	)

	firstMerge, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: middle.ID, AbsorbedID: original.ID,
		ExpectedSurvivorRevision: middle.Revision,
		ExpectedAbsorbedRevision: original.Revision,
		IdempotencyKey:           "tier-chain-first-merge", Actor: "test",
	})
	require.NoError(err)
	secondMerge, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: final.ID, AbsorbedID: firstMerge.Person.ID,
		ExpectedSurvivorRevision: final.Revision,
		ExpectedAbsorbedRevision: firstMerge.Person.Revision,
		IdempotencyKey:           "tier-chain-second-merge", Actor: "test",
	})
	require.NoError(err)

	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: firstMerge.Merge.ID,
		ParticipantIDs:         original.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "tier-chain-first-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
	var eventPersonID int64
	var mergedFrom sql.NullInt64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT person_id, merged_from_person_id FROM person_tier_events WHERE id = ?`), eventID).
		Scan(&eventPersonID, &mergedFrom))
	assert.Equal(split.NewPerson.ID, eventPersonID)
	assert.False(mergedFrom.Valid, "reversing the first merge restores the original NULL origin")
	var inboundPersonID int64
	var inboundMergedFrom sql.NullInt64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT person_id, merged_from_person_id FROM person_tier_events WHERE id = ?`), inboundEventID).
		Scan(&inboundPersonID, &inboundMergedFrom))
	assert.Equal(observer.ID, inboundPersonID, "the inbound event keeps its live owner")
	assert.True(inboundMergedFrom.Valid)
	assert.Equal(original.ID, inboundMergedFrom.Int64,
		"the inbound event restores its original historical origin")
}

func TestSplitPersonMerge_ChainedTierEventsRestoreNullOriginNewestFirst(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	original := mustPromotedPerson(t, f.Store, "tier-null-chain-original@example.com", "Original")
	middle := mustPromotedPerson(t, f.Store, "tier-null-chain-middle@example.com", "Middle")
	final := mustPromotedPerson(t, f.Store, "tier-null-chain-final@example.com", "Final")
	eventID := insertPersonTierEvent(t, f.Store, original.ID, nil, "tier-null-chain-run")

	firstMerge, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: middle.ID, AbsorbedID: original.ID,
		ExpectedSurvivorRevision: middle.Revision,
		ExpectedAbsorbedRevision: original.Revision,
		IdempotencyKey:           "tier-null-chain-first-merge", Actor: "test",
	})
	require.NoError(err)
	secondMerge, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: final.ID, AbsorbedID: firstMerge.Person.ID,
		ExpectedSurvivorRevision: final.Revision,
		ExpectedAbsorbedRevision: firstMerge.Person.Revision,
		IdempotencyKey:           "tier-null-chain-second-merge", Actor: "test",
	})
	require.NoError(err)

	secondSplit, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: secondMerge.Merge.ID,
		ParticipantIDs:         firstMerge.Person.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "tier-null-chain-second-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(secondSplit.ExactReversal)
	assert.Empty(secondSplit.UnrestoredRows)

	firstSplit, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: secondSplit.NewPerson.ID, MergeID: firstMerge.Merge.ID,
		ParticipantIDs:         original.ParticipantIDs,
		ExpectedSourceRevision: secondSplit.NewPerson.Revision,
		IdempotencyKey:         "tier-null-chain-first-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(firstSplit.ExactReversal)
	assert.Empty(firstSplit.UnrestoredRows)
	var eventPersonID int64
	var mergedFrom sql.NullInt64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT person_id, merged_from_person_id FROM person_tier_events WHERE id = ?`), eventID).
		Scan(&eventPersonID, &mergedFrom))
	assert.Equal(firstSplit.NewPerson.ID, eventPersonID)
	assert.False(mergedFrom.Valid, "reversing both merges restores the initial NULL origin")
}

func TestSplitPersonMerge_TierEventPreservesRetiredMergedFromIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	original := mustPromotedPerson(t, f.Store, "tier-history-original@example.com", "Original")
	middle := mustPromotedPerson(t, f.Store, "tier-history-middle@example.com", "Middle")
	final := mustPromotedPerson(t, f.Store, "tier-history-final@example.com", "Final")
	eventID := insertPersonTierEvent(t, f.Store, original.ID, nil, "tier-history-original-run")

	firstMerge, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: middle.ID, AbsorbedID: original.ID,
		ExpectedSurvivorRevision: middle.Revision,
		ExpectedAbsorbedRevision: original.Revision,
		IdempotencyKey:           "tier-history-first-merge", Actor: "test",
	})
	require.NoError(err)
	secondMerge, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: final.ID, AbsorbedID: firstMerge.Person.ID,
		ExpectedSurvivorRevision: final.Revision,
		ExpectedAbsorbedRevision: firstMerge.Person.Revision,
		IdempotencyKey:           "tier-history-second-merge", Actor: "test",
	})
	require.NoError(err)

	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: secondMerge.Person.ID, MergeID: secondMerge.Merge.ID,
		ParticipantIDs:         firstMerge.Person.ParticipantIDs,
		ExpectedSourceRevision: secondMerge.Person.Revision,
		IdempotencyKey:         "tier-history-second-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
	var eventPersonID int64
	var mergedFrom sql.NullInt64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT person_id, merged_from_person_id FROM person_tier_events WHERE id = ?`), eventID).
		Scan(&eventPersonID, &mergedFrom))
	assert.Equal(split.NewPerson.ID, eventPersonID)
	assert.True(mergedFrom.Valid)
	assert.Equal(original.ID, mergedFrom.Int64,
		"the journal must retain the retired historical origin instead of resolving its alias")
}

func insertPersonTierEvent(
	t *testing.T, st *store.Store, personID int64, mergedFrom any, runID string,
) int64 {
	t.Helper()
	var eventID int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_tier_events (
			person_id, merged_from_person_id, from_tier, to_tier, reason, mode,
			lanes_json, signals_json, rule, rule_version, actor, run_id
		) VALUES (?, ?, 3, 2, 'promotion', 'apply', '{}', '{}', 'tier2_two_way', 1,
			'test', ?) RETURNING id`), personID, mergedFrom, runID).Scan(&eventID)
	require.NoError(t, err)
	return eventID
}

func TestSplitPersonMerge_PreservesDeletionOfTierEventOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor := mustPromotedPerson(t, f.Store, "deleted-tier-event-survivor@example.com", "Survivor")
	absorbed := mustPromotedPerson(t, f.Store, "deleted-tier-event-absorbed@example.com", "Absorbed")
	eventOwner := mustPromotedPerson(t, f.Store, "deleted-tier-event-owner@example.com", "Event owner")
	eventID := insertPersonTierEvent(t, f.Store, eventOwner.ID, absorbed.ID, "deleted-tier-event-run")

	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "deleted-tier-event-merge", Actor: "test",
	})
	require.NoError(err)
	require.NoError(f.Store.DeletePersonContext(t.Context(), eventOwner.ID, eventOwner.Revision))

	var eventCount int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT COUNT(*) FROM person_tier_events WHERE id = ?`), eventID).Scan(&eventCount))
	assert.Zero(eventCount, "deleting the owner cascades the tier event")

	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         absorbed.ParticipantIDs,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "deleted-tier-event-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Empty(split.UnrestoredRows)
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT COUNT(*) FROM person_tier_events WHERE id = ?`), eventID).Scan(&eventCount))
	assert.Zero(eventCount, "splitting preserves the post-merge event deletion")
}

func removePersonMergePostColumns(
	t *testing.T, st *store.Store, mergeID int64, table string, originalRowID int64, removed ...string,
) {
	t.Helper()
	require := require.New(t)
	var postState string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT post_merge_row_json FROM person_merge_rows
		WHERE merge_id = ? AND table_name = ? AND original_row_id = ?`),
		mergeID, table, originalRowID).Scan(&postState))
	var encoded map[string]json.RawMessage
	require.NoError(json.Unmarshal([]byte(postState), &encoded))
	var columns []map[string]json.RawMessage
	require.NoError(json.Unmarshal(encoded["columns"], &columns))
	removedNames := make(map[string]struct{}, len(removed))
	for _, name := range removed {
		removedNames[name] = struct{}{}
	}
	kept := make([]map[string]json.RawMessage, 0, len(columns))
	for _, column := range columns {
		var name string
		require.NoError(json.Unmarshal(column["name"], &name))
		if _, omit := removedNames[name]; !omit {
			kept = append(kept, column)
		}
	}
	columnsJSON, err := json.Marshal(kept)
	require.NoError(err)
	encoded["columns"] = columnsJSON
	updated, err := json.Marshal(encoded)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_merge_rows SET post_merge_row_json = ?
		WHERE merge_id = ? AND table_name = ? AND original_row_id = ?`),
		string(updated), mergeID, table, originalRowID)
	require.NoError(err)
}
