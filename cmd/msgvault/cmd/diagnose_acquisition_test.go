package cmd

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type acquisitionArchive struct {
	t      *testing.T
	dir    string
	cfg    *config.Config
	st     *store.Store
	source int64
}

// newAcquisitionArchive uses the configured test database unless the case
// inspects the SQLite file itself.
func newAcquisitionArchive(t *testing.T, sourceType string, sqliteOnly bool) *acquisitionArchive {
	t.Helper()
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	var st *store.Store
	if sqliteOnly {
		st = testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	} else {
		st = testutil.NewTestStore(t)
		setMatrixTestDatabaseURL(t, st, cfg)
	}
	source, err := st.GetOrCreateSource(sourceType, "source@example.com")
	require.NoError(t, err)
	return &acquisitionArchive{t: t, dir: dir, cfg: cfg, st: st, source: source.ID}
}

func acquisitionOutcome(id, phase, status, kind string) store.SyncRunItem {
	return store.SyncRunItem{SourceMessageID: id, Phase: phase, Status: status, ErrorKind: kind, ErrorMessage: kind}
}

// record writes one run's outcomes. Completed runs are incremental.
func (a *acquisitionArchive) record(source int64, completed bool, items ...store.SyncRunItem) {
	a.t.Helper()
	syncType := "full"
	if completed {
		syncType = "incremental"
	}
	run, err := a.st.StartSync(source, syncType)
	require.NoError(a.t, err)
	for _, item := range items {
		item.SyncRunID = run
		require.NoError(a.t, a.st.RecordSyncRunItem(item))
	}
	if completed {
		require.NoError(a.t, a.st.CompleteSync(run, ""))
	} else {
		require.NoError(a.t, a.st.FailSync(run, "interrupted"))
	}
}

func (a *acquisitionArchive) diagnose(configText string, args ...string) (string, error) {
	a.t.Helper()
	if a.st != nil {
		require.NoError(a.t, a.st.Close())
		a.st = nil
	}
	configText = fmt.Sprintf("[data]\ndata_dir = %q\ndatabase_url = %q\n", a.dir, a.cfg.Data.DatabaseURL) + configText
	require.NoError(a.t, os.WriteFile(filepath.Join(a.dir, "config.toml"), []byte(configText), 0o600))
	root := newRootCommand()
	root.AddCommand(newDiagnoseAcquisitionCmd())
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{"--home", a.dir, "diagnose-acquisition", "--source-id", strconv.FormatInt(a.source, 10)}, args...))
	err := executeRootContext(a.t.Context(), root)
	return output.String(), err
}

func (a *acquisitionArchive) diagnoseJSON(args ...string) []map[string]string {
	a.t.Helper()
	output, err := a.diagnose("", append(args, "--json")...)
	require.NoError(a.t, err)
	assert.True(a.t, bytes.HasSuffix([]byte(output), []byte("\n")))
	items := []map[string]string{}
	require.NoError(a.t, json.Unmarshal([]byte(output), &items))
	return items
}

func acquisitionJSON(id, phase, kind, state string) map[string]string {
	return map[string]string{"source_message_id": id, "phase": phase, "error_kind": kind, "state": state}
}

func TestDiagnoseAcquisition(t *testing.T) {
	t.Run("still failing", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		a := newAcquisitionArchive(t, "gmail", false)
		other, err := a.st.GetOrCreateSource("gmail", "other@example.com")
		require.NoError(err)
		a.record(other.ID, false, acquisitionOutcome("other", "fetch", "error", "fetch_error"))
		pending := acquisitionOutcome("pending", "ingest", "error", "attachment_error")
		pending.ErrorMessage = store.SyncRunItemFilesPendingMessage
		a.record(a.source, false,
			acquisitionOutcome("download", "fetch", "error", "fetch_error"),
		)
		a.record(a.source, false,
			acquisitionOutcome("download", "fetch", "error", "batch_fetch_error"),
			acquisitionOutcome("ingest", "ingest", "error", "ingest_error"),
			acquisitionOutcome("attachment", "ingest", "error", "attachment_error"),
			acquisitionOutcome("image", "ingest", "error", "remote_image_error"),
			acquisitionOutcome("both", "ingest", "error", "attachment_remote_image_error"),
			pending,
		)
		output, err := a.diagnose("")
		require.NoError(err)
		assert.Equal("download\tstill failing\tmessage download failed\n"+
			"ingest\tstill failing\tmessage processing failed\n"+
			"attachment\tstill failing\tattachment saving failed\n"+
			"image\tstill failing\tremote image saving failed\n"+
			"both\tstill failing\tattachment and remote image saving failed\n"+
			"pending\tstill failing\tattachment saving failed\n", output)
		assert.Equal([]map[string]string{
			acquisitionJSON("download", "fetch", "batch_fetch_error", "active"),
			acquisitionJSON("ingest", "ingest", "ingest_error", "active"),
			acquisitionJSON("attachment", "ingest", "attachment_error", "active"),
			acquisitionJSON("image", "ingest", "remote_image_error", "active"),
			acquisitionJSON("both", "ingest", "attachment_remote_image_error", "active"),
			acquisitionJSON("pending", "ingest", "attachment_error", "active"),
		}, a.diagnoseJSON())
	})

	t.Run("fixed", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		a := newAcquisitionArchive(t, "gmail", false)
		pending := acquisitionOutcome("saved", "ingest", "error", "attachment_error")
		pending.ErrorMessage = store.SyncRunItemFilesPendingMessage
		a.record(a.source, false, pending, acquisitionOutcome("older", "fetch", "error", "fetch_error"))
		pending.SourceMessageID = "older"
		a.record(a.source, false, pending)
		a.record(a.source, false, acquisitionOutcome("saved", "fetch", "resolved", "acquired"), acquisitionOutcome("older", "fetch", "resolved", "acquired"))
		a.record(a.source, false, acquisitionOutcome("reopened", "fetch", "error", "fetch_error"))
		a.record(a.source, false, acquisitionOutcome("reopened", "fetch", "resolved", "acquired"))
		a.record(a.source, false, acquisitionOutcome("reopened", "ingest", "error", "ingest_error"))
		a.record(a.source, false, acquisitionOutcome("retired-first", "fetch", "error", "fetch_error"))
		a.record(a.source, false, acquisitionOutcome("retired-first", "fetch", "skipped", "gmail_not_found"))
		a.record(a.source, false, acquisitionOutcome("retired-first", "fetch", "error", "fetch_error"))
		a.record(a.source, false, acquisitionOutcome("retired-first", "fetch", "resolved", "acquired"))
		output, err := a.diagnose("")
		require.NoError(err)
		assert.Equal("reopened\tstill failing\tmessage processing failed\n", output)
		output, err = a.diagnose("", "--all")
		require.NoError(err)
		assert.Equal("older\tfixed\tmessage download failed\nreopened\tfixed\tmessage download failed\nreopened\tstill failing\tmessage processing failed\nretired-first\tretired\tmessage download failed\nretired-first\tfixed\tmessage download failed\n", output)
		assert.Equal([]map[string]string{
			acquisitionJSON("older", "fetch", "fetch_error", "resolved"),
			acquisitionJSON("reopened", "fetch", "fetch_error", "resolved"),
			acquisitionJSON("reopened", "ingest", "ingest_error", "active"),
			acquisitionJSON("retired-first", "fetch", "fetch_error", "historical"),
			acquisitionJSON("retired-first", "fetch", "fetch_error", "resolved"),
		}, a.diagnoseJSON("--all"))
	})

	t.Run("retired", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		a := newAcquisitionArchive(t, "gmail", false)
		pending := acquisitionOutcome("saved", "ingest", "error", "attachment_error")
		pending.ErrorMessage = store.SyncRunItemFilesPendingMessage
		a.record(a.source, false, pending)
		a.record(a.source, false, acquisitionOutcome("saved", "fetch", "resolved", "acquired"))
		_, err := a.st.DB().ExecContext(t.Context(), a.st.Rebind("UPDATE sync_run_items SET status = ? WHERE source_message_id = ? AND status = ?"), "skipped", "saved", "error")
		require.NoError(err)
		pending.SourceMessageID, pending.Status = "unfinished", "skipped"
		a.record(a.source, false, pending)
		a.record(a.source, false,
			acquisitionOutcome("cleaned", "ingest", "skipped", "attachment_error"),
			acquisitionOutcome("gone", "fetch", "error", "fetch_error"),
			acquisitionOutcome("files", "ingest", "error", "attachment_error"),
		)
		a.record(a.source, false,
			acquisitionOutcome("gone", "fetch", "skipped", "gmail_not_found"),
			acquisitionOutcome("files", "fetch", "skipped", "gmail_not_found"),
		)
		output, err := a.diagnose("")
		require.NoError(err)
		assert.Equal("No pending acquisition failures.\n", output)
		assert.Equal([]map[string]string{
			acquisitionJSON("unfinished", "ingest", "attachment_error", "historical"),
			acquisitionJSON("cleaned", "ingest", "attachment_error", "historical"),
			acquisitionJSON("gone", "fetch", "fetch_error", "historical"),
			acquisitionJSON("files", "ingest", "attachment_error", "indeterminate"),
		}, a.diagnoseJSON("--all"))
	})

	t.Run("cleanup preserves fixed failures", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		a := newAcquisitionArchive(t, "gmail", true)
		conversation, err := a.st.EnsureConversation(t.Context(), a.source, "thread", "Thread")
		require.NoError(err)
		_, err = a.st.UpsertMessage(&store.Message{ConversationID: conversation, SourceID: a.source, SourceMessageID: "cleaned", MessageType: "email"})
		require.NoError(err)
		a.record(a.source, false, acquisitionOutcome("cleaned", "ingest", "error", "attachment_error"))
		a.record(a.source, false, acquisitionOutcome("cleaned", "fetch", "resolved", "acquired"))
		a.record(a.source, false, acquisitionOutcome("cleaned", "fetch", "error", "fetch_error"))
		require.NoError(a.st.MarkMessageDeleted(a.source, "cleaned"))
		plan, err := a.st.PlanGCContext(t.Context())
		require.NoError(err)
		_, err = a.st.ExecuteGCContext(t.Context(), plan)
		require.NoError(err)
		assert.Equal([]map[string]string{
			acquisitionJSON("cleaned", "ingest", "attachment_error", "resolved"),
			acquisitionJSON("cleaned", "fetch", "fetch_error", "historical"),
		}, a.diagnoseJSON("--all"))
	})

	t.Run("unknown", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		a := newAcquisitionArchive(t, "gmail", false)
		a.record(a.source, false,
			acquisitionOutcome("stale", "fetch", "error", "fetch_error"),
			acquisitionOutcome("(unknown)", "fetch", "error", "batch_fetch_error"),
		)
		a.record(a.source, true)
		a.record(a.source, false,
			acquisitionOutcome("odd", "fetch", "error", "label_error"),
			acquisitionOutcome("odd", "fetch", "resolved", "acquired"),
		)
		output, err := a.diagnose("")
		require.NoError(err)
		assert.Equal("No pending acquisition failures.\n", output)
		assert.Equal([]map[string]string{}, a.diagnoseJSON())
		output, err = a.diagnose("", "--all")
		require.NoError(err)
		assert.Equal("stale\tunknown\tmessage download failed\n(unknown)\tunknown\tmessage download failed\nodd\tunknown\tmessage acquisition failed\n", output)
		assert.Equal([]map[string]string{
			acquisitionJSON("stale", "fetch", "fetch_error", "indeterminate"),
			acquisitionJSON("(unknown)", "fetch", "batch_fetch_error", "indeterminate"),
			acquisitionJSON("odd", "fetch", "label_error", "indeterminate"),
		}, a.diagnoseJSON("--all"))
	})

	t.Run("newer archive", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		a := newAcquisitionArchive(t, "gmail", true)
		a.record(a.source, false, acquisitionOutcome("newer-failure", "fetch", "error", "fetch_error"))
		_, err := a.st.DB().ExecContext(t.Context(), "UPDATE archive_metadata SET value = ? WHERE key = 'schema_version'", strconv.Itoa(store.SchemaVersion+1))
		require.NoError(err)
		output, err := a.diagnose("", "--all")
		require.ErrorContains(err, "run setup with the matching release")
		assert.NotContains(output, "newer-failure")
		assert.NotContains(output, "acquisition failures.")
	})

	t.Run("deletion", func(t *testing.T) {
		assert := assert.New(t)
		a := newAcquisitionArchive(t, "gmail", false)
		a.record(a.source, false,
			acquisitionOutcome("deleted", "delete", "error", "delete_error"),
			acquisitionOutcome("download", "fetch", "error", "fetch_error"),
			acquisitionOutcome("download", "delete", "error", "delete_error"),
		)
		want := []map[string]string{acquisitionJSON("download", "fetch", "fetch_error", "active")}
		assert.Equal(want, a.diagnoseJSON())
		assert.Equal(want, a.diagnoseJSON("--all"))
	})

	t.Run("local", func(t *testing.T) {
		require := require.New(t)
		a := newAcquisitionArchive(t, "gmail", false)
		remote := "[remote]\nurl = 'https://example.com'\n"
		_, err := a.diagnose(remote)
		require.ErrorContains(err, "use --local")
		_, err = a.diagnose(remote, "--local")
		require.NoError(err)
	})

	t.Run("merged source", func(t *testing.T) {
		require := require.New(t)
		a := newAcquisitionArchive(t, "gmail", true)
		into, err := a.st.GetOrCreateSource("gmail", "into@example.com")
		require.NoError(err)
		_, err = a.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: a.source, IntoSourceID: into.ID})
		require.NoError(err)
		_, err = a.diagnose("")
		require.ErrorContains(err, fmt.Sprintf("merged into source %d", into.ID))
	})
}
