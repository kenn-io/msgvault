package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func importSyntheticMuesliAudio(t *testing.T) (*store.Store, *attachmentstore.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("muesli", "recorder")
	require.NoError(t, err)
	dir := t.TempDir()
	path := filepath.Join(dir, "muesli.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY, title TEXT, start_time TEXT, created_at TEXT, meeting_status TEXT, raw_transcript TEXT, saved_recording_path TEXT)`)
	require.NoError(t, err)
	audio := filepath.Join(dir, "meeting-recordings", "synthetic.wav")
	require.NoError(t, os.MkdirAll(filepath.Dir(audio), 0700))
	require.NoError(t, os.WriteFile(audio, testWAV(), 0600))
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(audio, old, old))
	_, err = db.Exec(`INSERT INTO meetings VALUES (42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','completed','Synthetic transcript',?)`, audio)
	require.NoError(t, err)
	blobsDir := t.TempDir()
	_, err = muesli.NewImporter(st).Import(t.Context(), muesli.ImportOptions{Identifier: "recorder", AccountEmail: "user@example.com", DBPath: path, Recordings: true, AttachmentsDir: blobsDir})
	require.NoError(t, err)
	blobs, err := attachmentstore.New(store.NewPackCatalog(st), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, blobs.Close()) })
	return st, blobs
}

func TestMuesliRecordingUsesSharedDocbankAudioConsent(t *testing.T) {
	for _, tt := range []struct {
		name     string
		consent  bool
		requests int
	}{
		{"without consent", false, 0}, {"enabled with consent", true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, blobs := importSyntheticMuesliAudio(t)
			remote, server := newRetentionServer(t)
			cfg := config.DocbankIntegrationConfig{Enabled: true, URL: server.URL, APIKey: "synthetic-key", AllSourcesUploadConsent: tt.consent}
			sched := scheduler.New(nil)
			t.Cleanup(func() { <-sched.Stop().Done() })
			require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), t.TempDir(), cfg, nil))
			assert.True(sched.IsJobScheduled(beeperMediaSubmitJob))
			require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
			require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
			assert.Equal(tt.requests, remote.requestCount(), "ordinary audio retention deduplicates reprocessing")
		})
	}
}
