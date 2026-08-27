package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/scheduler"
)

func TestPeopleSweepSchedulerRegistersOnlyWhenEnabled(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		want    bool
	}{
		{name: "enabled", enabled: true, want: true},
		{name: "disabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks, must := assert.New(t), require.New(t)
			sched := scheduler.New(nil)
			cfg := peoplesweep.Config{Enabled: test.enabled, Schedule: "15 2 * * *"}
			must.NoError(addPeopleSweepJob(sched, cfg, func(context.Context) error { return nil }))
			checks.Equal(test.want, sched.IsJobScheduled(peopleSweepJobName))
			if test.want {
				must.Len(sched.JobStatus(), 1)
				checks.Equal("15 2 * * *", sched.JobStatus()[0].Schedule)
			}
		})
	}
}

// TestProductionPeopleSweepCodexUsesReleasedIsolationGate catches scheduled
// and manual worker construction passing nil launch dependencies for Codex.
func TestProductionPeopleSweepCodexUsesReleasedIsolationGate(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	var marker string
	var executable string
	if runtime.GOOS == "windows" {
		var err error
		executable, err = os.Executable()
		must.NoError(err)
	} else {
		marker = filepath.Join(t.TempDir(), "app-server-started")
		executable = filepath.Join(t.TempDir(), "codex-production-fixture")
		must.NoError(os.WriteFile(executable, []byte(
			"#!/bin/sh\nif test \"$1\" = '--version'; then printf 'codex-cli 0.149.0\\n'; exit 0; fi\n"+
				"printf started > '"+marker+"'\n",
		), 0o700))
	}
	config := commandCodexConfig()
	config.Provider.Executable = executable

	worker, err := newProductionPersonSweepWorker(config, nil, os.LookupEnv)
	must.ErrorIs(err, peoplesweep.ErrCodexIsolationUnreleased)
	checks.Nil(worker)
	if marker != "" {
		checks.NoFileExists(marker)
	}

	err = newPeopleSweepScheduledRun(config, nil)(t.Context())
	must.ErrorIs(err, peoplesweep.ErrCodexIsolationUnreleased)
	if marker != "" {
		checks.NoFileExists(marker)
	}
}

func TestPeopleSweepSchedulerRejectsInvalidCron(t *testing.T) {
	sched := scheduler.New(nil)
	err := addPeopleSweepJob(sched, peoplesweep.Config{
		Enabled: true, Schedule: "not a cron",
	}, func(context.Context) error { return nil })
	require.ErrorContains(t, err, "invalid cron expression")
	assert.False(t, sched.IsJobScheduled(peopleSweepJobName))
}

func TestPeopleSweepSchedulerRecoversJournalGap(t *testing.T) {
	must := require.New(t)
	st, personID, config := newPersonSweepCommandStore(t, true)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), false)
	must.NoError(err)
	profile, err := config.Profile()
	must.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	must.NoError(err)
	_, err = st.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{{
		PersonID: personID, SourceLane: profile.AllowedSources[0],
		ProgramFingerprint: peoplesweep.ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint,
	}})
	must.NoError(err)
	tx, err := st.DB().BeginTx(t.Context(), nil)
	must.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var journalSequence int64
	err = tx.QueryRowContext(t.Context(), `
		UPDATE person_sweep_change_clock
		SET sequence = sequence + 1
		WHERE singleton = TRUE AND enabled = TRUE
		RETURNING sequence`).Scan(&journalSequence)
	must.NoError(err)
	_, err = tx.ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_sweep_changes
			(sequence, person_id, source_lane, change_kind, evidence_effect, recorded_at)
		VALUES (?, ?, ?, 'upsert', '', CURRENT_TIMESTAMP)`),
		journalSequence, personID, profile.AllowedSources[0])
	must.NoError(err)
	must.NoError(tx.Commit())
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		DELETE FROM person_sweep_work WHERE person_id = ?`), personID)
	must.NoError(err)
	var journalHighWater int64
	must.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT MAX(sequence) FROM person_sweep_changes WHERE person_id = ?`), personID).
		Scan(&journalHighWater))

	run := newPeopleSweepScheduledRun(config, st)
	must.NoError(run(t.Context()))
	var cursorHighWater int64
	must.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT optimistic_sequence FROM person_sweep_cursors
		WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
		  AND catalog_fingerprint = ?`), personID, profile.AllowedSources[0],
		peoplesweep.ProgramFingerprint(), catalog.Fingerprint).Scan(&cursorHighWater))
	assert.Equal(t, journalHighWater, cursorHighWater)
}
