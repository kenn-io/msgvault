package cmd

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type xRestoreFixture struct {
	st            *store.Store
	cfg           *config.Config
	source        *store.Source
	message       int64
	repo          *backup.Repo
	repoPath      string
	server        *httptest.Server
	providerCalls atomic.Int32
}

func newXRestoreFixture(t *testing.T) *xRestoreFixture {
	t.Helper()
	f := &xRestoreFixture{cfg: xTestConfig(t)}
	f.cfg.Server.APIKey = "example-owner-key"
	dbPath, err := f.cfg.DatabasePath()
	require.NoError(t, err)
	f.st = testutil.NewSQLiteTestStoreAt(t, dbPath)
	f.source, err = f.st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, 0, store.XControlPatch{OwnerID: "1001", InitializeInstance: true})
	require.NoError(t, err)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
		SourceID: f.source.ID, OwnerID: "1001", PostID: "102", AuthorID: "1002",
		ConversationID: "100", EditHistory: []string{"100", "102"},
		Text: "synthetic aurora payload", Raw: []byte(`{"post":{"id":"102","text":"synthetic aurora payload"}}`),
		CreatedAt: at, FetchedAt: at, Availability: "public",
		Author: &store.XAuthorInput{SourceID: f.source.ID, UserID: "1002", Username: "example_author", DisplayName: "Example Author", FetchedAt: at},
	})
	require.NoError(t, err)
	f.message = saved.MessageID
	require.NoError(t, os.MkdirAll(filepath.Join(f.cfg.Data.DataDir, "attachments"), 0o700))
	f.repoPath = filepath.Join(t.TempDir(), "repository")
	f.repo, err = backup.Init(f.repoPath)
	require.NoError(t, err)
	return f
}

func (f *xRestoreFixture) sync(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	if f.server == nil {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			f.providerCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(provider.Close)
		deps := xCommandDeps{apiBaseURL: provider.URL + "/2", httpClient: provider.Client()}
		cleaner := NewXContentCleanup(f.st, f.cfg, nil)
		adapter := &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler), xDeps: &deps, xCleanup: cleaner}
		server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
		cleaner.SetServer(server)
		mux := http.NewServeMux()
		mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
		mux.Handle("/", server.Router())
		f.server = httptest.NewServer(mux)
		t.Cleanup(f.server.Close)
	}
	bridge := &xCollectionFixture{cfg: f.cfg, server: f.server}
	return bridge.run(t, bridge.clientConfig(t), args...)
}

func (f *xRestoreFixture) snapshot(t *testing.T) string {
	t.Helper()
	dbPath, err := f.cfg.DatabasePath()
	require.NoError(t, err)
	result, err := backup.Create(t.Context(), f.repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: filepath.Join(f.cfg.Data.DataDir, "attachments"), DataDir: f.cfg.Data.DataDir,
	})
	require.NoError(t, err)
	return result.SnapshotID
}

func (f *xRestoreFixture) record(t *testing.T) (string, string) {
	t.Helper()
	record, head, err := f.st.ExportXRemovalRecordContext(t.Context())
	require.NoError(t, err)
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "current-x-removals.json")
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	return path, head
}

func (f *xRestoreFixture) restore(t *testing.T, snapshot, record, head string, overwrite ...bool) (string, error) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "isolated")
	return target, f.restoreInto(t, snapshot, record, head, target, len(overwrite) > 0 && overwrite[0])
}

func (f *xRestoreFixture) restoreInto(t *testing.T, snapshot, record, head, target string, overwrite bool) error {
	t.Helper()
	savedRepo, savedTarget := backupRestoreRepo, backupRestoreTarget
	savedOverwrite, savedForce := backupRestoreOverwrite, backupRestoreForceUnlock
	savedJobs, savedLoose, savedIntegrity := backupRestoreJobs, backupRestoreLooseAttachments, backupRestoreIntegrityCheck
	savedRecord, savedHead := backupRestoreXRemovals, backupRestoreXRemovalsHead
	t.Cleanup(func() {
		backupRestoreRepo, backupRestoreTarget = savedRepo, savedTarget
		backupRestoreOverwrite, backupRestoreForceUnlock = savedOverwrite, savedForce
		backupRestoreJobs, backupRestoreLooseAttachments, backupRestoreIntegrityCheck = savedJobs, savedLoose, savedIntegrity
		backupRestoreXRemovals, backupRestoreXRemovalsHead = savedRecord, savedHead
	})
	backupRestoreRepo = f.repoPath
	backupRestoreTarget = target
	backupRestoreOverwrite, backupRestoreForceUnlock = overwrite, false
	backupRestoreJobs, backupRestoreLooseAttachments, backupRestoreIntegrityCheck = 1, false, true
	backupRestoreXRemovals, backupRestoreXRemovalsHead = record, head
	command := &cobra.Command{Use: "restore"}
	command.SetContext(testInvocationContext(t.Context(), f.cfg, invocationOptions{}))
	var output bytes.Buffer
	command.SetOut(&output)
	return runBackupRestore(command, []string{snapshot})
}

func TestXRestoreAcceptsCompleteCurrentEmptyRemovalRecord(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRestoreFixture(t)
	snapshot := f.snapshot(t)
	record, head := f.record(t)
	target, err := f.restore(t, snapshot, record, head)
	_testRequire.NoError(err, "a bound, independently current empty removal set still requires proof")
	restored, err := store.OpenReadOnly(filepath.Join(target, "msgvault.db"))
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, restored.Close()) }()
	body, err := restored.GetMessageBodyText(t.Context(), f.message)
	_testRequire.NoError(err)
	_testAssert.Equal("synthetic aurora payload", body)
	_testAssert.NoDirExists(filepath.Join(target, "analytics"))
	_testAssert.NoFileExists(filepath.Join(target, "vectors.db"))
}

func TestXRestoreCurrentRecordPurgesOldAndFreshSnapshots(t *testing.T) {
	f := newXRestoreFixture(t)
	old := f.snapshot(t)
	f.cfg.X.Accounts = []config.XAccountConfig{{UserID: "1001", MonthlyBudgetMicros: 0}}
	_, _, err := f.sync(t, "--source-id", strconv.FormatInt(f.source.ID, 10), "--remove-post", "100", "--json")
	require.NoError(t, err)
	record, head := f.record(t)
	fresh := f.snapshot(t)
	for _, test := range []struct{ name, snapshot string }{{"pre-cleanup", old}, {"post-cleanup", fresh}} {
		t.Run(test.name, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			target, err := f.restore(t, test.snapshot, record, head)
			_testRequire.NoError(err)
			dbPath := filepath.Join(target, "msgvault.db")
			restored, err := store.Open(dbPath)
			_testRequire.NoError(err)
			defer func() { assert.NoError(t, restored.Close()) }()
			body, err := restored.GetMessageBodyText(t.Context(), f.message)
			_testRequire.NoError(err)
			_testAssert.Empty(body)
			_, err = restored.GetMessageRaw(f.message)
			_testRequire.ErrorIs(err, sql.ErrNoRows)
			_, count, err := restored.SearchMessages("aurora", 0, 10)
			_testRequire.NoError(err)
			_testAssert.Zero(count)
			pending, err := restored.XCleanupPendingContext(t.Context())
			_testRequire.NoError(err)
			_testAssert.False(pending)
			_testAssert.NoFileExists(filepath.Join(target, "vectors.db"))
			analytics := filepath.Join(target, "analytics")
			_, err = buildCache(t.Context(), dbPath, analytics, true)
			_testRequire.NoError(err)
			duck, err := query.NewDuckDBEngine(t.Context(), analytics, dbPath, restored.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
			_testRequire.NoError(err)
			t.Cleanup(func() { assert.NoError(t, duck.Close()) })
			rows, err := duck.QuerySQL(t.Context(), "SELECT * FROM messages")
			_testRequire.NoError(err)
			encoded, err := json.Marshal(rows)
			_testRequire.NoError(err)
			for _, removed := range []string{"aurora payload", "example_author", "Example Author"} {
				_testAssert.NotContains(string(encoded), removed)
			}
			// Readers are started only after real rebuild and derivative checks.
			cfg := xTestConfig(t)
			cfg.HomeDir, cfg.Data.DataDir = target, target
			cleaner := NewXContentCleanup(restored, cfg, nil)
			adapter := &storeAPIAdapter{store: restored, config: cfg, logger: slog.New(slog.DiscardHandler)}
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Engine: duck, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
			cleaner.SetServer(server)
			host := httptest.NewServer(server.Router())
			t.Cleanup(host.Close)
			response, err := xTestHTTPGet(t, host.Client(), fmt.Sprintf("%s/api/v1/messages/%d", host.URL, f.message))
			_testRequire.NoError(err)
			data, err := io.ReadAll(response.Body)
			_testRequire.NoError(err)
			_testRequire.NoError(response.Body.Close())
			_testAssert.Equal(http.StatusOK, response.StatusCode)
			_testAssert.NotContains(string(data), "aurora")
			host.Close()
			_testRequire.NoError(duck.Close())
			at := time.Now().UTC().Add(time.Hour)
			replayed, err := restored.PersistXPostContext(t.Context(), store.XPersistPostInput{
				SourceID: f.source.ID, OwnerID: "1001", PostID: "104", AuthorID: "1002", ConversationID: "100",
				EditHistory: []string{"100", "102", "104"}, Text: "must never return", CreatedAt: at, FetchedAt: at, Availability: "public",
				Author: &store.XAuthorInput{SourceID: f.source.ID, UserID: "1002", Username: "example_author", DisplayName: "Example Author", FetchedAt: at},
			})
			_testRequire.NoError(err)
			_testAssert.True(replayed.Skipped)
			_testRequire.NoError(cleaner.Resume(t.Context()))
			body, err = restored.GetMessageBodyText(t.Context(), f.message)
			_testRequire.NoError(err)
			_testAssert.Empty(body)
			latest, err := restored.ResolveXPostContext(t.Context(), f.source.ID, "104")
			_testRequire.NoError(err)
			_testAssert.Equal("removed", latest.Availability)
		})
	}
	assert.Zero(t, f.providerCalls.Load())
}

func TestSyncXExportRemovalRecordUsesOwnerDaemonWithoutProvider(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRestoreFixture(t)
	f.cfg.X.Accounts = []config.XAccountConfig{{UserID: "1001", MonthlyBudgetMicros: 0}}
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	credential, model := "invalid_grant", store.XModelErrorResourceShape
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", CredentialError: &credential, ModelError: &model})
	_testRequire.NoError(err)
	other, err := f.st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	_, err = f.st.UpdateXControlContext(t.Context(), other.ID, 0, store.XControlPatch{OwnerID: "2001", InitializeInstance: true})
	_testRequire.NoError(err)
	_, _, err = f.sync(t, "--source-id", strconv.FormatInt(f.source.ID, 10), "--remove-post", "100", "--json")
	_testRequire.NoError(err)
	expected, head, err := f.st.ExportXRemovalRecordContext(t.Context())
	_testRequire.NoError(err)
	before, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	out, diagnostics, err := f.sync(t, "--export-removals", "--json")
	_testRequire.NoError(err)
	var record store.XRemovalRecord
	_testRequire.NoError(json.Unmarshal([]byte(out), &record))
	_testAssert.Equal(expected, record)
	_testRequire.Len(record.Sources, 2)
	_testAssert.Contains(diagnostics, head)
	after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(before, after)
	_testAssert.Zero(f.providerCalls.Load())
	_testAssert.NoDirExists(f.cfg.TokensDir())
}

func TestXRestoreRejectsInvalidCurrentAuthorityBeforePublication(t *testing.T) {
	_testRequire := require.New(t)

	f := newXRestoreFixture(t)
	snapshot := f.snapshot(t)
	_, _, err := f.sync(t, "--source-id", strconv.FormatInt(f.source.ID, 10), "--remove-post", "100", "--json")
	_testRequire.NoError(err)
	record, currentHead, err := f.st.ExportXRemovalRecordContext(t.Context())
	_testRequire.NoError(err)
	original, err := json.Marshal(record)
	_testRequire.NoError(err)
	for _, tc := range []struct {
		name, diagnostic string
		edit             func(*store.XRemovalRecord)
		data             func([]byte) []byte
		rehash           bool
	}{
		{name: "shortened suppression set", diagnostic: "independently trusted current head", edit: func(r *store.XRemovalRecord) { r.Sources[0].RemovedIDs = r.Sources[0].RemovedIDs[:1] }},
		{name: "missing source", diagnostic: "independently trusted current head", edit: func(r *store.XRemovalRecord) { r.Sources = nil }},
		{name: "wrong archive", diagnostic: "current archive authority", rehash: true, edit: func(r *store.XRemovalRecord) { r.ArchiveUID = strings.Repeat("a", 64) }},
		{name: "different source incarnation", diagnostic: "restored source incarnation", rehash: true, edit: func(r *store.XRemovalRecord) { r.Sources[0].InstanceID = strings.Repeat("b", 64) }},
		{name: "different numeric owner", diagnostic: "restored source incarnation", rehash: true, edit: func(r *store.XRemovalRecord) { r.Sources[0].OwnerID = "9999" }},
		{name: "unknown field", diagnostic: "malformed X removal record", data: func(b []byte) []byte { return append(b[:len(b)-1], []byte(`,"unexpected":true}`)...) }},
		{name: "duplicate field", diagnostic: "malformed X removal record", data: func(b []byte) []byte { return append(b[:len(b)-1], []byte(`,"version":1}`)...) }},
		{name: "invalid JSON", diagnostic: "malformed X removal record", data: func([]byte) []byte { return []byte(`{"private-content":`) }},
		{name: "oversized record", diagnostic: "no larger than 8 MiB", data: func([]byte) []byte { return bytes.Repeat([]byte(" "), store.XMaxRemovalRecordBytes+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			var changed store.XRemovalRecord
			_testRequire.NoError(json.Unmarshal(original, &changed))
			if tc.edit != nil {
				tc.edit(&changed)
			}
			data, err := json.Marshal(changed)
			_testRequire.NoError(err)
			head := currentHead
			if tc.rehash {
				digest := sha256.Sum256(data)
				head = hex.EncodeToString(digest[:])
			}
			if tc.data != nil {
				data = tc.data(data)
			}
			path := filepath.Join(t.TempDir(), "record.json")
			_testRequire.NoError(os.WriteFile(path, data, 0600))
			target, err := f.restore(t, snapshot, path, head)
			_testRequire.ErrorContains(err, tc.diagnostic)
			_testAssert.NotContains(err.Error(), "private-content")
			_testAssert.NoFileExists(filepath.Join(target, "msgvault.db"))
		})
	}
	assert.Zero(t, f.providerCalls.Load())
}

func TestXRestoreProofPairAndIsolatedTargetRequired(t *testing.T) {
	f := newXRestoreFixture(t)
	snapshot := f.snapshot(t)
	record, head := f.record(t)
	for _, tc := range []struct {
		name, record, head, diagnostic string
		overwrite                      bool
	}{
		{"missing both", "", "", "complete current removal record", false},
		{"missing head", record, "", "must be supplied together", false},
		{"missing record", "", head, "must be supplied together", false},
		{"overwrite", record, head, "fresh isolated target", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := f.restore(t, snapshot, tc.record, tc.head, tc.overwrite)
			require.ErrorContains(t, err, tc.diagnostic)
			assert.NoFileExists(t, filepath.Join(target, "msgvault.db"))
		})
	}
}

func TestSyncXExportRemovalRecordRefusesActiveSource(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRestoreFixture(t)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	t.Cleanup(func() { assert.NoError(t, execution.Release()) })
	output, _, err := f.sync(t, "--export-removals", "--json")
	_testRequire.Error(err)
	_testAssert.Empty(output, "an incomplete inventory must not produce usable authority")
	_testAssert.Zero(f.providerCalls.Load())
}

func TestXRestorePreservesCurrentAliasesFromPostSnapshotSource(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRestoreFixture(t)
	old := f.snapshot(t)
	other, err := f.st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	_, err = f.st.UpdateXControlContext(t.Context(), other.ID, 0, store.XControlPatch{OwnerID: "2001", InitializeInstance: true})
	_testRequire.NoError(err)
	at := time.Now().UTC()
	_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
		SourceID: other.ID, OwnerID: "2001", PostID: "501", AuthorID: "1002", ConversationID: "500",
		EditHistory: []string{"500", "501"}, Text: "later source native content", CreatedAt: at, FetchedAt: at, Availability: "public",
	})
	_testRequire.NoError(err)
	// Source A never fetched this chain. The owner requests removal through A;
	// the current daemon also erases B's copy and records its additional alias.
	_, _, err = f.sync(t, "--source-id", strconv.FormatInt(f.source.ID, 10), "--remove-post", "500", "--json")
	_testRequire.NoError(err)
	record, head := f.record(t)
	target, err := f.restore(t, old, record, head)
	_testRequire.NoError(err)
	dbPath := filepath.Join(target, "msgvault.db")
	restored, err := store.Open(dbPath)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, restored.Close()) }()
	sources, err := restored.ListSourcesContext(t.Context(), "x")
	_testRequire.NoError(err)
	_testAssert.Len(sources, 1, "replaying current authority must preserve the old snapshot's source population")
	body, err := restored.GetMessageBodyText(t.Context(), f.message)
	_testRequire.NoError(err)
	_testAssert.Equal("synthetic aurora payload", body, "an independent post stays supported")
	// A shorter returned history cannot forget the alias known only through B.
	replayed, err := restored.PersistXPostContext(t.Context(), store.XPersistPostInput{
		SourceID: f.source.ID, OwnerID: "1001", PostID: "501", AuthorID: "1002", ConversationID: "500",
		EditHistory: []string{"501"}, Text: "forbidden lunar resurrection", CreatedAt: at, FetchedAt: at.Add(time.Hour), Availability: "public",
	})
	_testRequire.NoError(err)
	_testAssert.True(replayed.Skipped, "every currently removed native alias must survive even if its original source is absent")
	_, count, err := restored.SearchMessages("resurrection", 0, 10)
	_testRequire.NoError(err)
	_testAssert.Zero(count)

	// Arrange the independent repeat-restore case through real local cleanup,
	// even if the preceding suppression assertion fails. The complete current
	// record covers 501 globally, while A's own live record held only 500.
	cfg := xTestConfig(t)
	cfg.HomeDir, cfg.Data.DataDir = target, target
	_, err = NewXContentCleanup(restored, cfg, nil).RemovePost(t.Context(), f.source.ID, "501", "removed")
	_testRequire.NoError(err)
	clean, err := backup.Create(t.Context(), f.repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: filepath.Join(target, "attachments"), DataDir: target,
	})
	_testRequire.NoError(err)
	repeatedTarget, err := f.restore(t, clean.SnapshotID, record, head)
	_testRequire.NoError(err, "the same complete global authority covers a suppression retained by another restored source")
	repeated, err := store.OpenReadOnly(filepath.Join(repeatedTarget, "msgvault.db"))
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, repeated.Close()) }()
	_, count, err = repeated.SearchMessages("resurrection", 0, 10)
	_testRequire.NoError(err)
	_testAssert.Zero(count)
	_testAssert.Zero(f.providerCalls.Load())
}
