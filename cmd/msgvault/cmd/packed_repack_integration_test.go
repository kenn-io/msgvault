package cmd

import (
	"archive/zip"
	"context"
	crand "crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
)

func TestPostRepackBlobReadsThroughAPIMCPAndExports(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	live := []byte("post-repack bytes served through every daemon-backed reader")
	liveHash := f.addLoose(live)
	dead := make([]byte, (8<<20)+(256<<10))
	_, err := crand.Read(dead)
	require.NoError(err)
	deadHash := f.addLoose(dead)
	deadSmallHash := f.addLoose([]byte("second dead entry makes the source pack sparse"))

	_, err = f.maintenance.pack(context.Background(), 0)
	require.NoError(err)
	oldEntry := f.packedEntry(liveHash)
	require.NotNil(oldEntry)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		DELETE FROM attachments WHERE content_hash IN (?, ?)`), deadHash, deadSmallHash)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339), oldEntry.PackID)
	require.NoError(err)

	stats, err := f.maintenance.repack(context.Background(), 0)
	require.NoError(err)
	assert.Equal(1, stats.PacksRewritten)
	newEntry := f.packedEntry(liveHash)
	require.NotNil(newEntry)
	assert.NotEqual(oldEntry.PackID, newEntry.PackID)

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Data: config.DataConfig{DataDir: f.dir}},
		Store: &storeAPIAdapter{
			store: f.store, attachmentMaintenance: f.maintenance,
		},
		BlobStore:     f.blob,
		Logger:        slog.New(slog.DiscardHandler),
		DaemonVersion: Version,
	})
	httpServer := httptest.NewServer(srv.Router())
	t.Cleanup(httpServer.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		httpServer.URL+"/api/v1/cli/attachment?content_hash="+liveHash, nil)
	require.NoError(err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(err)
	apiData, err := io.ReadAll(resp.Body)
	require.NoError(err)
	require.NoError(resp.Body.Close())
	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal(live, apiData, "real API attachment handler follows the swapped mapping")

	client, err := daemonclient.New(daemonclient.Config{
		URL: httpServer.URL, AllowInsecure: true,
	})
	require.NoError(err)
	configureRemoteDaemonForTest(t, httpServer.URL)
	mcpOpts, err := daemonMCPServeOptions(context.Background(), client)
	require.NoError(err)
	require.NotNil(mcpOpts.AttachmentReader)
	mcpData, err := mcpOpts.AttachmentReader.ReadAttachment(context.Background(), liveHash)
	require.NoError(err)
	assert.Equal(live, mcpData, "daemon-backed MCP attachment reader follows the swapped mapping")

	savedOutput := exportAttachmentOutput
	savedJSON := exportAttachmentJSON
	savedBase64 := exportAttachmentBase64
	t.Cleanup(func() {
		exportAttachmentOutput = savedOutput
		exportAttachmentJSON = savedJSON
		exportAttachmentBase64 = savedBase64
	})
	exportAttachmentJSON = false
	exportAttachmentBase64 = false
	exportAttachmentOutput = filepath.Join(t.TempDir(), "single-export.bin")
	cmd := &cobra.Command{Use: "export-attachment"}
	cmd.SetContext(context.Background())
	require.NoError(runExportAttachmentHTTP(cmd, liveHash))
	single, err := os.ReadFile(exportAttachmentOutput)
	require.NoError(err)
	assert.Equal(live, single)

	directory := t.TempDir()
	dirResult := exportAttachmentsFromHTTP(context.Background(), client, directory,
		[]query.AttachmentInfo{{
			Filename: "post-repack.bin", ContentHash: liveHash, Size: int64(len(live)),
		}})
	require.Empty(dirResult.Errors)
	require.Len(dirResult.Files, 1)
	directoryData, err := os.ReadFile(dirResult.Files[0].Path)
	require.NoError(err)
	assert.Equal(live, directoryData)

	zipPath := filepath.Join(t.TempDir(), "post-repack.zip")
	zipStats := msgexport.AttachmentsWithOpener(zipPath,
		[]query.AttachmentInfo{{
			Filename: "post-repack.bin", ContentHash: liveHash, Size: int64(len(live)),
		}},
		func(hash string) (io.ReadCloser, error) {
			return client.OpenCLIAttachment(context.Background(), hash)
		})
	require.Empty(zipStats.Errors)
	assert.Equal(1, zipStats.Count)
	zipReader, err := zip.OpenReader(zipPath)
	require.NoError(err)
	require.Len(zipReader.File, 1)
	zipped, err := zipReader.File[0].Open()
	require.NoError(err)
	zippedData, err := io.ReadAll(zipped)
	require.NoError(err)
	require.NoError(zipped.Close())
	require.NoError(zipReader.Close())
	assert.Equal(live, zippedData)
}
