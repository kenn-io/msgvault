package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	activitypkg "go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestRepairDatesAlwaysProxiesThroughDaemonCLIRunner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal([]string{"repair-dates", "--apply"}, req.Args)
		},
		`{"type":"stdout","data":"Repaired 1 message(s).\n"}`,
		`{"type":"complete"}`,
	)
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")

	var apply bool
	var stdout bytes.Buffer
	cmd := &cobra.Command{
		Use: repairDatesCmd.Use, Args: repairDatesCmd.Args, RunE: repairDatesCmd.RunE,
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write repaired dates")
	cmd.SetArgs([]string{"--apply"})
	cmd.SetOut(&stdout)

	require.NoError(cmd.Execute())
	assert.Equal(1, int(requests.Load()))
	assert.Contains(stdout.String(), "Repaired 1 message(s)")
}

func TestRunRepairDatesLocalDryRunApplyAndIdempotency(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	t.Cleanup(func() { cfg = savedCfg })

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(
		source.ID,
		"date-repair-thread",
		"Date repair",
	)
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "date-repair-message",
		MessageType:     "email",
		SentAt: sql.NullTime{
			Time:  time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
			Valid: true,
		},
		InternalDate: sql.NullTime{
			Time:  time.Date(2015, 5, 5, 12, 0, 0, 0, time.UTC),
			Valid: true,
		},
	})
	require.NoError(err)
	raw := []byte("From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Date: Thu, 1 Jan 1970 00:00:00 +0000\r\n" +
		"Received: by edge.example.com; Wed, 3 Jan 2007 16:05:06 +0000\r\n" +
		"Received: by mx.example.com; Tue, 2 Jan 2007 15:04:05 +0000\r\n" +
		"Subject: Synthetic date repair\r\n" +
		"\r\nBody\r\n")
	require.NoError(st.UpsertMessageRaw(messageID, raw))
	fallbackDate := time.Date(2012, 4, 3, 10, 0, 0, 0, time.UTC)
	fallbackMessageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "date-repair-fallback-message",
		MessageType:     "email",
		SentAt: sql.NullTime{
			Time:  time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC),
			Valid: true,
		},
		InternalDate: sql.NullTime{Time: fallbackDate, Valid: true},
	})
	require.NoError(err)
	participantID, err := st.EnsureParticipant(
		"contact-state@example.com", "Contact State", "example.com")
	require.NoError(err)
	person, created, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	require.True(created)
	prepareDateRepairContactState(t, st, person.ID)
	require.NoError(st.Close())

	now := time.Date(2026, 7, 23, 12, 0, 0, 123, time.UTC)
	var dryRunOut bytes.Buffer
	dryRunCmd := &cobra.Command{}
	dryRunCmd.SetContext(context.Background())
	dryRunCmd.SetOut(&dryRunOut)
	require.NoError(runRepairDatesLocal(dryRunCmd, false, now))
	assert.Contains(dryRunOut.String(), "Repairable: 2")
	assert.Contains(
		dryRunOut.String(),
		"Candidates lacking usable raw MIME: 1 "+
			"(source metadata may still resolve them)",
	)
	assert.Contains(dryRunOut.String(), "Dry run: no rows were modified")
	assert.Equal(
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		readMessageSentAt(t, cfg.DatabaseDSN(), messageID),
	)
	assert.Equal(
		time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC),
		readMessageSentAt(t, cfg.DatabaseDSN(), fallbackMessageID),
	)
	_, err = os.Stat(filepath.Join(dataDir, "repairs"))
	require.ErrorIs(err, os.ErrNotExist)
	assert.False(readContactDirtyAt(
		t, cfg.DatabaseDSN(), person.ID).Valid,
		"dry run must not dirty contact state")

	var applyOut bytes.Buffer
	applyCmd := &cobra.Command{}
	applyCmd.SetContext(context.Background())
	applyCmd.SetOut(&applyOut)
	require.NoError(runRepairDatesLocal(applyCmd, true, now))
	assert.Contains(applyOut.String(), "Repaired 2 message(s)")
	assert.Contains(applyOut.String(), "Analytics cache rebuilt")
	assert.Equal(
		time.Date(2007, 1, 2, 15, 4, 5, 0, time.UTC),
		readMessageSentAt(t, cfg.DatabaseDSN(), messageID),
	)
	assert.Equal(
		fallbackDate,
		readMessageSentAt(t, cfg.DatabaseDSN(), fallbackMessageID),
	)
	dirtyAfterApply := readContactDirtyAt(t, cfg.DatabaseDSN(), person.ID)
	assert.True(dirtyAfterApply.Valid,
		"committed date repairs must dirty contact state")

	ledgerPaths, err := filepath.Glob(filepath.Join(dataDir, "repairs", "dates-*.json"))
	require.NoError(err)
	require.Len(ledgerPaths, 1)
	ledgerBytes, err := os.ReadFile(ledgerPaths[0])
	require.NoError(err)
	var ledger dateRepairLedger
	require.NoError(json.Unmarshal(ledgerBytes, &ledger))
	assert.Equal("complete", ledger.Status)
	assert.True(ledger.CacheRebuilt)
	require.Len(ledger.Repairs, 2)
	assert.Equal(messageID, ledger.Repairs[0].MessageID)
	assert.Equal("received-oldest-plausible", string(ledger.Repairs[0].Source))
	assert.Equal(fallbackMessageID, ledger.Repairs[1].MessageID)
	assert.Equal("source-fallback", string(ledger.Repairs[1].Source))

	st, err = store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	pending, err := st.ListActivityProjectionQueueContext(t.Context(), 10)
	require.NoError(err)
	assert.Len(pending, 2,
		"date repair must leave both changed messages queued for real projection")
	projectDateRepairActivity(t, st)
	pending, err = st.ListActivityProjectionQueueContext(t.Context(), 10)
	require.NoError(err)
	assert.Empty(pending)
	require.NoError(st.Close())
	assert.False(readContactDirtyAt(t, cfg.DatabaseDSN(), person.ID).Valid)

	var secondApplyOut bytes.Buffer
	secondApplyCmd := &cobra.Command{}
	secondApplyCmd.SetContext(context.Background())
	secondApplyCmd.SetOut(&secondApplyOut)
	require.NoError(runRepairDatesLocal(secondApplyCmd, true, now))
	assert.Contains(secondApplyOut.String(), "Nothing to repair")
	ledgerPaths, err = filepath.Glob(filepath.Join(dataDir, "repairs", "dates-*.json"))
	require.NoError(err)
	assert.Len(ledgerPaths, 1)
	assert.False(readContactDirtyAt(t, cfg.DatabaseDSN(), person.ID).Valid,
		"no-op apply must not dirty contact state")
}

func TestRunRepairDatesLocalReportsUnresolvedReasons(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	t.Cleanup(func() { cfg = savedCfg })

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("apple-mail", "archive@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(
		source.ID,
		"unresolved-date-thread",
		"Unresolved dates",
	)
	require.NoError(err)

	newCandidate := func(sourceMessageID string, raw []byte) int64 {
		id, err := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: sourceMessageID,
			MessageType:     "email",
			SentAt: sql.NullTime{
				Time:  time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
				Valid: true,
			},
		})
		require.NoError(err)
		require.NoError(st.UpsertMessageRaw(id, raw))
		return id
	}

	// Date header present but unparseable.
	headerUnusableID := newCandidate("header-unusable", []byte(
		"From: sender@example.com\r\n"+
			"To: recipient@example.com\r\n"+
			"Date: not a real date\r\n"+
			"Subject: A\r\n\r\nBody\r\n"))

	// Received headers present, but every hop is outside the plausible range.
	receivedUnusableID := newCandidate("received-unusable", []byte(
		"From: sender@example.com\r\n"+
			"To: recipient@example.com\r\n"+
			"Received: by mx.example.com; Mon, 02 Jan 1980 15:04:05 +0000\r\n"+
			"Subject: B\r\n\r\nBody\r\n"))

	// No Date header and no Received headers at all.
	noSignalID := newCandidate("no-signal", []byte(
		"From: sender@example.com\r\n"+
			"To: recipient@example.com\r\n"+
			"Subject: C\r\n\r\nBody\r\n"))

	require.NoError(st.Close())

	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	var dryRunOut bytes.Buffer
	dryRunCmd := &cobra.Command{}
	dryRunCmd.SetContext(context.Background())
	dryRunCmd.SetOut(&dryRunOut)
	require.NoError(runRepairDatesLocal(dryRunCmd, false, now))

	out := dryRunOut.String()
	assert.Contains(out, "Repairable: 0")
	assert.Contains(out, "No plausible replacement: 3")
	assert.Contains(out, "date-header-unusable: 1 (Date header present but not a plausible date)")
	assert.Contains(out, fmt.Sprintf("message %d", headerUnusableID))
	assert.Contains(out, "received-headers-unusable: 1 (Received headers present but none plausible)")
	assert.Contains(out, fmt.Sprintf("message %d", receivedUnusableID))
	assert.Contains(out, "no-date-signal: 1 (no Date header, no Received headers)")
	assert.Contains(out, fmt.Sprintf("message %d", noSignalID))
}

func TestAvailableDateRepairLedgerPathDoesNotOverwriteExistingLedger(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	now := time.Date(2026, 7, 23, 12, 0, 0, 123, time.UTC)
	first, err := availableDateRepairLedgerPath(dataDir, now)
	require.NoError(err)
	require.NoError(os.MkdirAll(filepath.Dir(first), 0o700))
	require.NoError(os.WriteFile(first, []byte("{}\n"), 0o600))

	second, err := availableDateRepairLedgerPath(dataDir, now)
	require.NoError(err)
	assert.NotEqual(t, first, second)
	assert.Equal(t, "dates-20260723T120000.000000123Z-1.json", filepath.Base(second))
}

func TestApplyPlannedDateRepairsRecordsGuardFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	st, err := store.OpenForTest(filepath.Join(dataDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(
		source.ID,
		"guarded-date-repair",
		"Guarded date repair",
	)
	require.NoError(err)
	currentSentAt := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "guarded-date-repair-message",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: currentSentAt, Valid: true},
	})
	require.NoError(err)

	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	lowerBound, upperBound := mime.PlausibleDateBounds(now)
	plan := &dateRepairPlan{
		candidateCount: 1,
		repairs: []plannedDateRepair{{
			MessageDateRepair: store.MessageDateRepair{
				ID:     messageID,
				SentAt: time.Date(2007, 1, 2, 15, 4, 5, 0, time.UTC),
			},
			source: mime.DateSourceReceived,
			oldSentAt: sql.NullTime{
				Time:  time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
				Valid: true,
			},
		}},
	}
	ledger := newDateRepairLedger(plan, now)
	ledgerPath := filepath.Join(dataDir, "repairs", "guard-failure.json")
	require.NoError(writeDateRepairLedger(ledgerPath, ledger))

	err = applyPlannedDateRepairs(
		context.Background(),
		st,
		plan,
		lowerBound,
		upperBound,
		ledgerPath,
		ledger,
	)
	require.ErrorContains(err, "changed after date repair planning")
	var persistedSentAt time.Time
	require.NoError(st.DB().QueryRow(
		st.Rebind("SELECT sent_at FROM messages WHERE id = ?"),
		messageID,
	).Scan(&persistedSentAt))
	assert.Equal(currentSentAt, persistedSentAt.UTC())

	ledgerBytes, err := os.ReadFile(ledgerPath)
	require.NoError(err)
	var persisted dateRepairLedger
	require.NoError(json.Unmarshal(ledgerBytes, &persisted))
	assert.Equal("failed", persisted.Status)
	assert.Contains(persisted.Error, "changed after date repair planning")
	require.Len(persisted.Repairs, 1)
	assert.Equal(messageID, persisted.Repairs[0].MessageID)
}

func TestDateRepairCacheRefreshErrorProvidesRecoveryCommand(t *testing.T) {
	cause := errors.New("synthetic cache failure")

	err := dateRepairCacheRefreshError(cause)

	require.ErrorIs(t, err, cause)
	require.ErrorContains(t, err, "msgvault build-cache --full-rebuild")
}

func TestDateRepairPostApplyLedgerErrorProvidesRecoveryCommand(t *testing.T) {
	cause := errors.New("synthetic ledger failure")

	err := dateRepairPostApplyRecoveryError(
		"audit ledger finalization failed",
		cause,
	)

	require.ErrorIs(t, err, cause)
	require.ErrorContains(t, err, "audit ledger finalization failed")
	require.ErrorContains(t, err, "msgvault build-cache --full-rebuild")
}

func TestDateRepairUsesAnalyticsCacheOnlyForSQLite(t *testing.T) {
	assert.True(t, dateRepairUsesAnalyticsCache("/tmp/msgvault.db"))
	assert.False(t, dateRepairUsesAnalyticsCache("postgres://db.example.com/msgvault"))
	assert.False(t, dateRepairUsesAnalyticsCache("postgresql://db.example.com/msgvault"))
}

func TestRunRepairDatesLocalInvalidatesAndUnlocksCacheWhenApplyFails(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	t.Cleanup(func() { cfg = savedCfg })

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(
		source.ID,
		"failed-date-repair",
		"Failed date repair",
	)
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "failed-date-repair-message",
		MessageType:     "email",
		SentAt: sql.NullTime{
			Time:  time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
			Valid: true,
		},
		InternalDate: sql.NullTime{
			Time:  time.Date(2015, 5, 5, 12, 0, 0, 0, time.UTC),
			Valid: true,
		},
	})
	require.NoError(err)
	_, err = st.DB().Exec(`
		CREATE TRIGGER reject_date_repair
		BEFORE UPDATE OF sent_at ON messages
		BEGIN
			SELECT RAISE(ABORT, 'synthetic date repair failure');
		END
	`)
	require.NoError(err)
	require.NoError(st.Close())

	require.NoError(os.MkdirAll(cfg.AnalyticsDir(), 0o755))
	statePath := query.CacheStatePath(cfg.AnalyticsDir())
	require.NoError(os.WriteFile(statePath, []byte("{}\n"), 0o600))

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	err = runRepairDatesLocal(
		cmd,
		true,
		time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
	)
	require.ErrorContains(err, "synthetic date repair failure")
	_, statErr := os.Stat(statePath)
	require.ErrorIs(statErr, os.ErrNotExist)

	cacheLock, err := cacheBuildFileLock(cfg.AnalyticsDir())
	require.NoError(err)
	locked, err := cacheLock.TryLock()
	require.NoError(err)
	require.True(locked, "date repair must release the cache lock on failure")
	require.NoError(cacheLock.Unlock())
}

func TestRunRepairDatesLocalReportsCommittedRepairWhenContactInvalidationFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	t.Cleanup(func() { cfg = savedCfg })

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(
		source.ID, "invalidation-failure", "Invalidation failure")
	require.NoError(err)
	repairedAt := time.Date(2015, 5, 5, 12, 0, 0, 0, time.UTC)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "invalidation-failure-message",
		MessageType:     "email",
		SentAt: sql.NullTime{
			Time:  time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
			Valid: true,
		},
		InternalDate: sql.NullTime{Time: repairedAt, Valid: true},
	})
	require.NoError(err)
	participantID, err := st.EnsureParticipant(
		"invalidation@example.com", "Invalidation", "example.com")
	require.NoError(err)
	person, created, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	require.True(created)
	prepareDateRepairContactState(t, st, person.ID)
	_, err = st.DB().Exec(`
		CREATE TRIGGER reject_contact_invalidation
		BEFORE UPDATE OF dirty_at ON person_contact_state
		BEGIN
			SELECT RAISE(ABORT, 'synthetic contact invalidation failure');
		END
	`)
	require.NoError(err)
	require.NoError(st.Close())

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	err = runRepairDatesLocal(
		cmd,
		true,
		time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
	)
	require.ErrorContains(err, "contact state invalidation failed")
	require.ErrorContains(err, "full activity/contact-state rebuild")
	require.ErrorContains(err, "synthetic contact invalidation failure")
	assert.Equal(repairedAt, readMessageSentAt(t, cfg.DatabaseDSN(), messageID),
		"the repair committed before invalidation failed")
	st, err = store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	pending, err := st.ListActivityProjectionQueueContext(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(messageID, pending[0].MessageID,
		"failed invalidation must not acknowledge the repair queue")
	require.NoError(st.Close())

	ledgerPaths, err := filepath.Glob(filepath.Join(dataDir, "repairs", "dates-*.json"))
	require.NoError(err)
	require.Len(ledgerPaths, 1)
	ledgerBytes, err := os.ReadFile(ledgerPaths[0])
	require.NoError(err)
	var ledger dateRepairLedger
	require.NoError(json.Unmarshal(ledgerBytes, &ledger))
	assert.Equal("applied-invalidation-failed", ledger.Status)
	assert.Contains(ledger.Error, "synthetic contact invalidation failure")
	require.NotNil(ledger.AppliedAt,
		"a committed repair must retain its applied timestamp")
	assert.Nil(ledger.CompletedAt)
}

func prepareDateRepairContactState(
	t *testing.T,
	st *store.Store,
	personID int64,
) {
	t.Helper()
	projectDateRepairActivity(t, st)
	revisions, err := st.ContactRevisionsContext(t.Context())
	require.NoError(t, err)
	require.NoError(t, st.RecomputeContactStateContext(
		t.Context(), []int64{personID}, revisions))
}

func projectDateRepairActivity(t *testing.T, st *store.Store) {
	t.Helper()
	projector, err := activitypkg.NewProjector(st, activitypkg.Options{
		Timezone: "UTC", BatchSize: 10,
	})
	require.NoError(t, err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(t, err)
}

func readMessageSentAt(t *testing.T, dsn string, messageID int64) time.Time {
	t.Helper()
	st, err := store.OpenForTest(dsn)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	var sentAt time.Time
	require.NoError(t, st.DB().QueryRow(
		st.Rebind("SELECT sent_at FROM messages WHERE id = ?"),
		messageID,
	).Scan(&sentAt))
	return sentAt.UTC()
}

func readContactDirtyAt(t *testing.T, dsn string, personID int64) sql.NullTime {
	t.Helper()
	st, err := store.OpenForTest(dsn)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	var dirty sql.NullTime
	require.NoError(t, st.DB().QueryRow(
		st.Rebind("SELECT dirty_at FROM person_contact_state WHERE person_id = ?"),
		personID,
	).Scan(&dirty))
	return dirty
}
