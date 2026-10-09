package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestDeliveryEmailEvidenceBackfillUsesMaintenanceTimeout(t *testing.T) {
	dbURL := skipUnlessPostgresInternal(t)
	requirements, assertions := require.New(t), assert.New(t)
	st := newPGStoreInternal(t, dbURL)
	ctx := context.Background()

	source, err := st.GetOrCreateSource("gmail", "slow-evidence-backfill@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "slow-evidence-backfill", "email_thread", "Slow evidence backfill")
	requirements.NoError(err)
	participantID, err := st.EnsureParticipant("slow-evidence-sender@example.test", "Synthetic Sender", "example.test")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "slow-evidence-backfill", MessageType: "email",
		SenderID: sql.NullInt64{Int64: participantID, Valid: true},
	})
	requirements.NoError(err)

	_, err = st.db.ExecContext(ctx, st.Rebind(`
		DELETE FROM delivery_source_email_evidence WHERE participant_id=? AND source_id=?`), participantID, source.ID)
	requirements.NoError(err)
	_, err = st.db.ExecContext(ctx, st.Rebind(
		`DELETE FROM applied_migrations WHERE name=?`), migrationDeliverySourceEmailEvidenceV1)
	requirements.NoError(err)
	_, err = st.db.ExecContext(ctx, `
		CREATE FUNCTION delivery_test_sleep_evidence_backfill() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(1.5); RETURN NEW; END $$;
		CREATE TRIGGER delivery_test_sleep_evidence_backfill BEFORE INSERT ON delivery_source_email_evidence
		FOR EACH ROW EXECUTE FUNCTION delivery_test_sleep_evidence_backfill()`)
	requirements.NoError(err)
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(context.Background(), `
			DROP TRIGGER IF EXISTS delivery_test_sleep_evidence_backfill ON delivery_source_email_evidence;
			DROP FUNCTION IF EXISTS delivery_test_sleep_evidence_backfill()`)
	})

	st.db.SetMaxOpenConns(1)
	st.db.SetMaxIdleConns(1)
	conn, err := st.db.Conn(ctx)
	requirements.NoError(err)
	_, err = conn.ExecContext(ctx, `SET statement_timeout = '1s'`)
	requirements.NoError(err)
	requirements.NoError(conn.Close())

	requirements.NoError(st.installDeliveryEmailEvidenceTracking(ctx), "the evidence backfill must use the maintenance timeout escape hatch")
	applied, err := st.IsMigrationAppliedContext(ctx, migrationDeliverySourceEmailEvidenceV1, 1)
	requirements.NoError(err)
	assertions.True(applied, "the migration ledger is updated after the backfill commits")
	var evidence int
	requirements.NoError(st.db.QueryRowContext(ctx, st.Rebind(`
		SELECT COUNT(*) FROM delivery_source_email_evidence
		WHERE participant_id=? AND source_id=? AND evidence_present=TRUE`), participantID, source.ID).Scan(&evidence))
	assertions.Equal(1, evidence)
}

func TestPostgreSQLCrossedEmailTombstonesSerializeEvidenceRefreshes(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	dbURL := skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t, dbURL)
	ctx := context.Background()

	source, err := st.GetOrCreateSource("gmail", "crossed-evidence@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID,
		"crossed-evidence", "email_thread", "Crossed evidence")
	requirements.NoError(err)
	participantA, err := st.EnsureParticipantContext(ctx,
		"crossed-a@example.test", "Synthetic A", "example.test")
	requirements.NoError(err)
	participantB, err := st.EnsureParticipantContext(ctx,
		"crossed-b@example.test", "Synthetic B", "example.test")
	requirements.NoError(err)
	insertMessage := func(id string, senderID, recipientID int64) int64 {
		t.Helper()
		messageID, err := st.UpsertMessage(&Message{
			ConversationID: conversationID, SourceID: source.ID,
			SourceMessageID: id, MessageType: "email",
			SenderID: sql.NullInt64{Int64: senderID, Valid: true},
		})
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(messageID, "to",
			[]int64{recipientID}, []string{"Synthetic recipient"}))
		return messageID
	}
	messageAB := insertMessage("crossed-a-to-b", participantA, participantB)
	messageBA := insertMessage("crossed-b-to-a", participantB, participantA)

	first, err := st.DB().BeginTx(ctx, nil)
	requirements.NoError(err)
	firstOpen := true
	second, err := st.DB().BeginTx(ctx, nil)
	requirements.NoError(err)
	secondOpen := true
	secondRefreshStarted := false
	secondRefreshDone := make(chan error, 1)
	t.Cleanup(func() {
		if firstOpen {
			_ = first.Rollback()
		}
		if secondRefreshStarted {
			select {
			case <-secondRefreshDone:
			case <-time.After(5 * time.Second):
			}
		}
		if secondOpen {
			_ = second.Rollback()
		}
	})
	var firstPID, secondPID int
	requirements.NoError(first.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&firstPID))
	requirements.NoError(second.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&secondPID))
	refresh := st.Rebind(`SELECT delivery_refresh_source_email_evidence(?, ?, NULL)`)
	_, err = first.ExecContext(ctx, refresh, participantA, source.ID)
	requirements.NoError(err, "the first sender refresh holds its evidence serialization lock")
	secondRefreshStarted = true
	go func() {
		_, refreshErr := second.ExecContext(ctx, refresh, participantB, source.ID)
		secondRefreshDone <- refreshErr
	}()
	requirements.Eventually(func() bool {
		var waiting bool
		err := st.DB().QueryRowContext(ctx, st.Rebind(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity activity
				WHERE activity.pid=? AND activity.wait_event_type='Lock'
				  AND ?=ANY(pg_blocking_pids(activity.pid))
				  AND EXISTS (
					SELECT 1 FROM pg_locks waiting_lock
					JOIN pg_locks blocking_lock
					  ON blocking_lock.locktype=waiting_lock.locktype
					 AND blocking_lock.classid=waiting_lock.classid
					 AND blocking_lock.objid=waiting_lock.objid
					 AND blocking_lock.objsubid=waiting_lock.objsubid
					 AND blocking_lock.pid=?
					WHERE waiting_lock.pid=activity.pid
					  AND waiting_lock.locktype='advisory'
					  AND NOT waiting_lock.granted
					  AND blocking_lock.granted
				  )
			)`), secondPID, firstPID, firstPID).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond,
		"crossed sender/recipient refreshes must serialize before acquiring opposite participant locks")

	_, err = first.ExecContext(ctx, st.Rebind(`
		UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), messageAB)
	requirements.NoError(err)
	requirements.NoError(first.Commit())
	firstOpen = false
	requirements.NoError(<-secondRefreshDone)
	secondRefreshStarted = false
	_, err = second.ExecContext(ctx, st.Rebind(`
		UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), messageBA)
	requirements.NoError(err)
	requirements.NoError(second.Commit())
	secondOpen = false

	var deletedMessages, missingEvidence int
	requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		SELECT COUNT(*) FROM messages
		WHERE id IN (?, ?) AND deleted_from_source_at IS NOT NULL`), messageAB, messageBA).Scan(&deletedMessages))
	requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		SELECT COUNT(*) FROM delivery_source_email_evidence
		WHERE source_id=? AND participant_id IN (?, ?) AND evidence_present=FALSE`),
		source.ID, participantA, participantB).Scan(&missingEvidence))
	assertions.Equal(2, deletedMessages)
	assertions.Equal(2, missingEvidence)
}

func TestPostgreSQLParticipantEnsureAndDisplayNameBackfillUseGenerationBeforeMetadata(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	dbURL := skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t, dbURL)
	ctx := context.Background()

	participantID, err := st.EnsureParticipantContext(ctx,
		"tracked-backfill@example.test", "", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participantID)
	requirements.NoError(err)
	profile, err := (personenrichment.ProviderConfig{
		Name: "delivery-lock-order", Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: "https://api.example.test/search", APIKeyEnv: "PROVIDER_API_KEY",
		Mode: "deep", NumResults: 1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
		TargetKeys:         []string{"attribute:bio"}, RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", RefreshInterval: 24 * time.Hour,
		RequestTimeout: time.Minute, PollInterval: 30 * time.Second,
		MaxJobAge: 15 * time.Minute, MaxRetries: 5,
		MaxRequestsPerRun: 10, MaxRequestsPerDay: 100,
	}).Profile(personfacts.Catalog{
		Version: "fixture-v1",
		Targets: []personfacts.TargetDescriptor{{
			Kind: personfacts.TargetAttribute, Key: "attribute:bio", Revision: "revision-1",
			UniversalID: "attribute:bio", Slug: "bio", Description: "Synthetic biography",
			ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
			Choices: []personfacts.ChoiceDescriptor{}, Fields: []personfacts.FieldDescriptor{},
		}},
	})
	requirements.NoError(err)
	_, err = st.EnsurePersonEnrichmentProfile(ctx, profile)
	requirements.NoError(err)
	_, _, err = st.GrantPersonEnrichmentConsent(ctx, profile.Fingerprint, "test")
	requirements.NoError(err)
	_, err = st.SetPersonTrackingContext(ctx, person.ID, true)
	requirements.NoError(err)

	const barrierSQL = `
		CREATE FUNCTION delivery_test_pause_participant_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(hashtextextended(
				'msgvault.test.participant_display_order:' || current_schema(), 0));
			RETURN NEW;
		END $$;
		CREATE TRIGGER zzz_delivery_test_pause_participant_insert AFTER INSERT ON participants
		FOR EACH ROW EXECUTE FUNCTION delivery_test_pause_participant_insert()`
	_, err = st.DB().ExecContext(ctx, barrierSQL)
	requirements.NoError(err)
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(context.Background(), `
			DROP TRIGGER IF EXISTS zzz_delivery_test_pause_participant_insert ON participants;
			DROP FUNCTION IF EXISTS delivery_test_pause_participant_insert()`)
	})

	barrier, err := st.DB().Conn(ctx)
	requirements.NoError(err)
	barrierHeld := true
	ensureStarted, updateStarted := false, false
	ensureFinished, updateFinished := false, false
	type ensureResult struct {
		id  int64
		err error
	}
	type updateResult struct {
		updated bool
		err     error
	}
	ensureDone := make(chan ensureResult, 1)
	updateDone := make(chan updateResult, 1)
	var ensurePID int
	defer func() {
		if barrierHeld {
			_, _ = barrier.ExecContext(context.Background(), `SELECT pg_advisory_unlock(
				hashtextextended('msgvault.test.participant_display_order:' || current_schema(), 0))`)
		}
		if ensureStarted && !ensureFinished {
			select {
			case <-ensureDone:
			case <-time.After(5 * time.Second):
			}
		}
		if updateStarted && !updateFinished {
			select {
			case <-updateDone:
			case <-time.After(5 * time.Second):
			}
		}
		_ = barrier.Close()
	}()
	_, err = barrier.ExecContext(ctx, `SELECT pg_advisory_lock(
		hashtextextended('msgvault.test.participant_display_order:' || current_schema(), 0))`)
	requirements.NoError(err)
	var barrierPID int
	requirements.NoError(barrier.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&barrierPID))

	ensureStarted = true
	go func() {
		id, ensureErr := st.EnsureParticipantContext(ctx,
			"new-participant@example.test", "Synthetic New", "example.test")
		ensureDone <- ensureResult{id: id, err: ensureErr}
	}()
	requirements.Eventually(func() bool {
		err := st.DB().QueryRowContext(ctx, st.Rebind(`
			SELECT activity.pid FROM pg_stat_activity activity
			WHERE activity.datname=current_database()
			  AND ?=ANY(pg_blocking_pids(activity.pid))
			  AND activity.wait_event_type='Lock'
			LIMIT 1`), barrierPID).Scan(&ensurePID)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond,
		"participant insertion must pause after the generation trigger runs")

	updateStarted = true
	go func() {
		updated, updateErr := st.UpdateParticipantDisplayNameByEmail(
			"tracked-backfill@example.test", "Synthetic Backfill")
		updateDone <- updateResult{updated: updated, err: updateErr}
	}()
	requirements.Eventually(func() bool {
		var waiting bool
		err := st.DB().QueryRowContext(ctx, st.Rebind(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity activity
				WHERE activity.wait_event_type='Lock'
				  AND ?=ANY(pg_blocking_pids(activity.pid))
			)`), ensurePID).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond,
		"the display-name backfill must wait on a lock held by participant insertion")

	_, err = barrier.ExecContext(ctx, `SELECT pg_advisory_unlock(
		hashtextextended('msgvault.test.participant_display_order:' || current_schema(), 0))`)
	requirements.NoError(err)
	barrierHeld = false
	var ensured ensureResult
	select {
	case ensured = <-ensureDone:
		ensureFinished = true
	case <-time.After(10 * time.Second):
		requirements.FailNow("participant insertion did not finish after releasing the test barrier")
	}
	requirements.NoError(ensured.err)
	assertions.Positive(ensured.id)
	var updated updateResult
	select {
	case updated = <-updateDone:
		updateFinished = true
	case <-time.After(10 * time.Second):
		requirements.FailNow("display-name backfill did not finish after participant insertion")
	}
	requirements.NoError(updated.err)
	assertions.True(updated.updated)
}
