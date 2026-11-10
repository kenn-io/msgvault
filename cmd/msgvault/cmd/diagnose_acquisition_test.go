package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDiagnoseAcquisitionCommandReadOnly(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := st.GetOrCreateSource("gmail", "source@example.com")
	require.NoError(err)
	run, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	require.NoError(st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run,
		SourceMessageID: "synthetic-id", Phase: "ingest", Status: "error", ErrorKind: "remote_image_error",
		ErrorMessage: store.SyncRunItemFilesPendingMessage}))
	require.NoError(st.FailSync(run, "interrupted"))
	require.NoError(st.Close())
	var requests atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer remote.Close()
	// Enabled file logging and a monitored remote exercise the real root
	// lifecycle. The diagnostic must bypass both of those write/network paths.
	require.NoError(os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf(
		"[data]\ndata_dir = %q\n[log]\nenabled = true\n[remote]\nurl = %q\nallow_insecure = true\n", dir, remote.URL)), 0o600))
	snapshot := func() map[string][32]byte {
		t.Helper()
		files := map[string][32]byte{}
		require.NoError(filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				files[path+"/"] = [32]byte{}
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = sha256.Sum256(data)
			return nil
		}))
		return files
	}
	before := snapshot()
	root := newRootCommand()
	root.AddCommand(newDiagnoseAcquisitionCmd())
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--home", dir, "diagnose-acquisition", "--source-id", strconv.FormatInt(source.ID, 10), "--json"})
	require.NoError(executeRootContext(t.Context(), root))
	var report store.AcquisitionDiagnostic
	require.NoError(json.Unmarshal(output.Bytes(), &report))
	assert.Equal(1, report.Version)
	assert.Equal(source.ID, report.SourceID)
	assert.Equal(int64(1), report.Summary.ActiveObligations)
	assert.Zero(report.Summary.ActiveErrors)
	require.Len(report.Items, 1)
	assert.Equal([]string{"remote_image_archiving_disabled"}, report.Items[0].BlockedBy)
	assert.Equal(before, snapshot(), "configuration, archive files, and directory contents must remain unchanged")
	root = newRootCommand()
	root.AddCommand(newDiagnoseAcquisitionCmd())
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--home", dir, "diagnose-acquisition", "--source-id", strconv.FormatInt(source.ID, 10), "--state", "resolved"})
	output.Reset()
	require.NoError(executeRootContext(t.Context(), root))
	assert.Contains(output.String(), "Messages with pending work: 1")
	assert.Contains(output.String(), "Details: resolved work\n")
	assert.Contains(output.String(), "does not prove archive completeness")
	assert.Equal(before, snapshot())
	root = newRootCommand()
	root.AddCommand(newDiagnoseAcquisitionCmd())
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--home", dir, "diagnose-acquisition", "--source-id", strconv.FormatInt(source.ID, 10)})
	output.Reset()
	require.NoError(executeRootContext(t.Context(), root))
	assert.Contains(output.String(), `"synthetic-id": file-saving reminder`)
	assert.Contains(output.String(), "synthetic=true, error=false")
	assert.Contains(output.String(), `latest="needs attention"`)
	assert.Contains(output.String(), "remote image archiving disabled")
	assert.NotContains(output.String(), "ingest/remote_image_error")
	assert.NotContains(output.String(), "ingest/error")
	assert.Equal(before, snapshot())
	assert.Zero(requests.Load(), "the command must not contact the configured remote")
}

func TestDiagnoseAcquisitionCommandHelp(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	root := newRootCommand()
	root.AddCommand(newDiagnoseAcquisitionCmd())
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"diagnose-acquisition", "--help"})
	require.NoError(executeRootContext(t.Context(), root))
	assert.Contains(output.String(), "saved sync records")
	assert.Contains(output.String(), "Finding no pending work does not prove archive completeness.")
	assert.Contains(output.String(), "Continue with the --after-id value from the previous page")
	assert.NotContains(output.String(), "ledger")
	assert.NotContains(output.String(), "obligations")
	assert.NotContains(output.String(), "outcome ID")
}

func TestDiagnoseAcquisitionCommandMissingArchive(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := filepath.Join(t.TempDir(), "absent")
	root := newRootCommand()
	root.AddCommand(newDiagnoseAcquisitionCmd())
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--home", dir, "diagnose-acquisition", "--source-id", "1", "--json"})
	require.Error(executeRootContext(t.Context(), root))
	assert.NoDirExists(dir)
	assert.NotContains(output.String(), `"active_obligations":0`)
}
