package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestInitSchemaBackfillsLegacySyncResumeMetadata(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	ctx := t.Context()
	legacySource, err := st.GetOrCreateSource("", "legacy-gmail@example.test")
	requirements.NoError(err)
	mboxSource, err := st.GetOrCreateSource("mbox", "legacy-mbox@example.test")
	requirements.NoError(err)
	emlxSource, err := st.GetOrCreateSource("apple-mail", "legacy-emlx@example.test")
	requirements.NoError(err)
	pstSource, err := st.GetOrCreateSource("pst", "legacy-pst@example.test")
	requirements.NoError(err)
	gcalSource, err := st.GetOrCreateSource("gcal", "legacy-calendar@example.test")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(ctx, `ALTER TABLE sync_runs DROP COLUMN request_fingerprint`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`
		DELETE FROM applied_migrations
		WHERE name = ?
	`), "sync_run_resume_metadata_v1")
	requirements.NoError(err)

	type seededRun struct {
		name            string
		sourceID        int64
		syncType        string
		status          string
		cursorBefore    string
		cursorAfter     sql.NullString
		wantFingerprint sql.NullString
		wantSyncType    string
		wantResumable   bool
	}
	runs := []seededRun{
		{
			name:            "interrupted recovery",
			sourceID:        fixture.Source.ID,
			syncType:        "full",
			status:          store.SyncStatusFailed,
			cursorAfter:     sql.NullString{String: "handoff-cursor", Valid: true},
			wantFingerprint: sql.NullString{String: "gmail-history-recovery:v1", Valid: true},
			wantSyncType:    "full",
		},
		{
			name:            "completed full sync",
			sourceID:        fixture.Source.ID,
			syncType:        "full",
			status:          store.SyncStatusCompleted,
			cursorAfter:     sql.NullString{String: "handoff-cursor", Valid: true},
			wantFingerprint: sql.NullString{},
			wantSyncType:    "full",
		},
		{
			name:            "interrupted legacy Gmail recovery",
			sourceID:        legacySource.ID,
			status:          store.SyncStatusFailed,
			cursorAfter:     sql.NullString{String: "handoff-cursor", Valid: true},
			wantFingerprint: sql.NullString{String: "gmail-history-recovery:v1", Valid: true},
			wantSyncType:    "full",
			wantResumable:   true,
		},
		{
			name:         "ambiguous legacy Gmail checkpoint",
			sourceID:     fixture.Source.ID,
			status:       store.SyncStatusFailed,
			wantSyncType: "",
		},
		{
			name:          "legacy MBOX checkpoint",
			sourceID:      mboxSource.ID,
			status:        store.SyncStatusFailed,
			wantSyncType:  "import-mbox",
			wantResumable: true,
		},
		{
			name:          "legacy EMLX checkpoint",
			sourceID:      emlxSource.ID,
			status:        store.SyncStatusFailed,
			wantSyncType:  "import-emlx",
			wantResumable: true,
		},
		{
			name:          "legacy PST checkpoint",
			sourceID:      pstSource.ID,
			status:        store.SyncStatusFailed,
			wantSyncType:  "import-pst",
			wantResumable: true,
		},
		{
			name:          "legacy Calendar full checkpoint",
			sourceID:      gcalSource.ID,
			status:        store.SyncStatusFailed,
			cursorBefore:  `{"kind":"gcal_full_v1","page_token":"calendar-page"}`,
			wantSyncType:  "full",
			wantResumable: true,
		},
		{
			name:         "legacy Calendar incremental checkpoint",
			sourceID:     gcalSource.ID,
			status:       store.SyncStatusFailed,
			cursorBefore: "incremental-page",
			wantSyncType: "",
		},
	}

	ids := make([]int64, len(runs))
	for index := range runs {
		cursorBefore := runs[index].cursorBefore
		if cursorBefore == "" {
			cursorBefore = "page-token"
		}
		err := st.DB().QueryRowContext(ctx, st.Rebind(`
			INSERT INTO sync_runs (
				source_id, sync_type, started_at, completed_at, status,
				cursor_before, cursor_after
			) VALUES (?, ?, CURRENT_TIMESTAMP, NULL, ?, ?, ?)
			RETURNING id
		`), runs[index].sourceID, runs[index].syncType, runs[index].status,
			cursorBefore, runs[index].cursorAfter).Scan(&ids[index])
		requirements.NoError(err)
	}

	requirements.NoError(st.InitSchemaContext(ctx))

	for index, run := range runs {
		var got sql.NullString
		var gotSyncType string
		requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
			SELECT request_fingerprint, sync_type FROM sync_runs WHERE id = ?
		`), ids[index]).Scan(&got, &gotSyncType), run.name)
		checks.Equal(run.wantFingerprint, got, run.name)
		checks.Equal(run.wantSyncType, gotSyncType, run.name)
		if run.wantResumable {
			resumable, err := st.GetLatestCheckpointedSyncByType(run.sourceID, run.wantSyncType)
			requirements.NoError(err, run.name)
			checks.Equal(ids[index], resumable.ID, run.name)
		}
	}
}
