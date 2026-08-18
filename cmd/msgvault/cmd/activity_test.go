package cmd

import (
	"bytes"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRunActivityBuildLocalUsesConfigAndBackstop(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Activity.Timezone = "Pacific/Kiritimati"
	cfg.Activity.BatchSize = 1
	cfg.Activity.MaxDirectCounterparts = 1

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "activity-cli@example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(
		source.ID, "activity-cli@example.com", "manual"))
	conversationID, err := st.EnsureConversation(
		source.ID, "activity-cli-thread", "Activity CLI")
	require.NoError(err)
	ownerID, err := st.EnsureParticipant(
		"activity-cli@example.com", "Activity Owner", "example.com")
	require.NoError(err)
	firstCounterpart, err := st.EnsureParticipant(
		"activity-first@example.com", "Activity First", "example.com")
	require.NoError(err)
	secondCounterpart, err := st.EnsureParticipant(
		"activity-second@example.com", "Activity Second", "example.com")
	require.NoError(err)
	_, created, err := st.CreatePersonFromParticipant(firstCounterpart)
	require.NoError(err)
	require.True(created)
	_, created, err = st.CreatePersonFromParticipant(secondCounterpart)
	require.NoError(err)
	require.True(created)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "activity-cli-message",
		MessageType:     "email",
		SentAt: sql.NullTime{
			Time:  time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC),
			Valid: true,
		},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(
		messageID, "from", []int64{ownerID}, []string{"Activity Owner"}))
	require.NoError(st.ReplaceMessageRecipients(
		messageID, "to",
		[]int64{firstCounterpart, secondCounterpart},
		[]string{"Activity First", "Activity Second"}))
	_, err = st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "activity-cli-second",
		MessageType:     "email",
		SentAt: sql.NullTime{
			Time:  time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC),
			Valid: true,
		},
	})
	require.NoError(err)
	require.NoError(st.Close())

	var output bytes.Buffer
	command := newActivityCommand()
	command.SetContext(t.Context())
	command.SetOut(&output)
	command.SetArgs([]string{"build"})
	require.NoError(command.Execute())
	assert.Contains(output.String(), "Projected 2 event(s) in 2 batch(es)")

	st, err = store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	var timezone, localDate string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT timezone, local_date
		FROM activity_events
		WHERE message_id = ?
	`), messageID).Scan(&timezone, &localDate))
	assert.Equal("Pacific/Kiritimati", timezone)
	assert.Equal("2026-08-01", localDate)
	func() {
		rows, queryErr := st.DB().QueryContext(t.Context(), st.Rebind(`
			SELECT evidence
			FROM activity_event_persons
			WHERE message_id = ?
			ORDER BY person_id
		`), messageID)
		require.NoError(queryErr)
		defer func() { require.NoError(rows.Close()) }()

		var evidence []string
		for rows.Next() {
			var value string
			require.NoError(rows.Scan(&value))
			evidence = append(evidence, value)
		}
		require.NoError(rows.Err())
		assert.Equal([]string{"co_presence", "co_presence"}, evidence,
			"configured max_direct_counterparts=1 must classify two recipients as broadcast")
	}()
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`DELETE FROM activity_events WHERE message_id = ?`), messageID)
	require.NoError(err)
	require.NoError(st.Close())

	output.Reset()
	command = newActivityCommand()
	command.SetContext(t.Context())
	command.SetOut(&output)
	command.SetArgs([]string{"build"})
	require.NoError(command.Execute())
	st, err = store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM activity_events WHERE message_id = ?`),
		messageID).Scan(&count))
	assert.Zero(count, "ordinary build must not force-scan below the watermark")
	require.NoError(st.Close())

	output.Reset()
	command = newActivityCommand()
	command.SetContext(t.Context())
	command.SetOut(&output)
	command.SetArgs([]string{"build", "--backstop"})
	require.NoError(command.Execute())
	assert.Contains(output.String(), "Projected 1 event(s)")
	st, err = store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM activity_events WHERE message_id = ?`),
		messageID).Scan(&count))
	assert.Equal(1, count)
}

func TestActivityBuildProxiesThroughDaemonCLIRunner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal([]string{"activity", "build", "--backstop"}, req.Args, "args")
		},
		`{"type":"stdout","data":"Projected 2 event(s) in 1 batch(es); touched 1 and recomputed 1 person(s); watermark 2.\n"}`,
		`{"type":"complete"}`,
	)
	configureRemoteDaemonForTest(t, server.URL)
	cfg.Data.DataDir = t.TempDir()

	var stdout bytes.Buffer
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newActivityCommand())
	root.SetOut(&stdout)
	root.SetArgs([]string{"activity", "build", "--backstop"})

	require.NoError(root.Execute(), "activity build")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Equal(
		"Projected 2 event(s) in 1 batch(es); touched 1 and recomputed 1 person(s); watermark 2.\n",
		stdout.String(),
		"stdout",
	)
}

func TestRegisterActivityProjectionJobRunsConfiguredProjector(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.NewMessage().
		WithSourceMessageID("activity-job-message").
		WithSentAt(time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)).
		Create(t, f.Store)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	sched := scheduler.New(nil)
	activityConfig := config.ActivityConfig{
		Timezone:              "UTC",
		MaxDirectCounterparts: 25,
		BatchSize:             1,
		Schedule:              "17 * * * *",
	}

	require.NoError(registerActivityProjectionJob(
		sched, f.Store, activityConfig, log))
	assert.True(sched.IsJobScheduled(activityProjectionJob))
	require.NoError(sched.TriggerJob(activityProjectionJob))
	assert.Contains(logs.String(), "activity projection complete")
	assert.Contains(logs.String(), "events_projected=")
	assert.NotContains(logs.String(), "activity-job-message")

	var count int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT COUNT(*) FROM activity_events WHERE message_id = ?`),
		messageID).Scan(&count))
	assert.Equal(1, count)
}

func TestRegisterActivityProjectionJobHonorsDisabledSchedule(t *testing.T) {
	f := storetest.New(t)
	sched := scheduler.New(nil)
	activityConfig := config.NewDefaultConfig().Activity
	activityConfig.Schedule = ""

	require.NoError(t, registerActivityProjectionJob(
		sched, f.Store, activityConfig, slog.Default()))
	assert.False(t, sched.IsJobScheduled(activityProjectionJob))
}
