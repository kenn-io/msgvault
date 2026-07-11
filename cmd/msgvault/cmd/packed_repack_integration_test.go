package cmd

import (
	"archive/zip"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
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

	assertBlobReadSurfaces(t, f.store, f.blob, f.dir, liveHash, live, "attachment-1.bin")
}

func TestPackedRestoreReadsThroughAPIMCPAndExports(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "restore-reader@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "restore-reader-thread", "Restore Reader")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID,
		SourceMessageID: "restore-reader-message", MessageType: "email",
	})
	require.NoError(err)
	content := []byte("pack-native restored bytes served through every reader")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	attachmentsDir := filepath.Join(dataDir, "attachments")
	loosePath := filepath.Join(attachmentsDir, hash[:2], hash)
	require.NoError(os.MkdirAll(filepath.Dir(loosePath), 0o700))
	require.NoError(os.WriteFile(loosePath, content, 0o600))
	require.NoError(st.UpsertAttachment(messageID, "restored.bin", "application/octet-stream",
		hash[:2]+"/"+hash, hash, len(content)))
	sourceBlobs, err := attachmentstore.New(store.NewPackCatalog(st), attachmentsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = sourceBlobs.Close() })
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	app := backupapp.New("test")
	_, err = backup.Create(context.Background(), repo, app, backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachmentsDir, DataDir: dataDir,
		ContentSource: backupapp.NewContentSource(sourceBlobs, attachmentsDir),
	})
	require.NoError(err)
	target := filepath.Join(t.TempDir(), "restored")
	res, err := backup.Restore(context.Background(), repo, app, backup.RestoreOptions{
		TargetDir: target, PackedContent: backupapp.NewPackedRestoreTarget(packstore.DefaultLimits()),
	})
	require.NoError(err)
	require.Equal(int64(1), res.PackedAttachmentBlobs)
	require.Zero(res.LooseAttachmentBlobs)
	restoredStore, err := store.OpenForTest(res.DBPath)
	require.NoError(err)
	t.Cleanup(func() { _ = restoredStore.Close() })
	restoredBlobs, err := attachmentstore.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "attachments"))
	require.NoError(err)
	t.Cleanup(func() { _ = restoredBlobs.Close() })

	assertBlobReadSurfaces(t, restoredStore, restoredBlobs, target, hash, content, "restored.bin")
}

func assertBlobReadSurfaces(
	t *testing.T,
	st *store.Store,
	blob *attachmentstore.Store,
	dataDir, hash string,
	content []byte,
	filename string,
) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Data: config.DataConfig{DataDir: dataDir}},
		Store: &storeAPIAdapter{
			store: st,
		},
		BlobStore:     blob,
		Logger:        slog.New(slog.DiscardHandler),
		DaemonVersion: Version,
	})
	httpServer := httptest.NewServer(srv.Router())
	t.Cleanup(httpServer.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		httpServer.URL+"/api/v1/cli/attachment?content_hash="+hash, nil)
	require.NoError(err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(err)
	apiData, err := io.ReadAll(resp.Body)
	require.NoError(err)
	require.NoError(resp.Body.Close())
	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal(content, apiData, "real API attachment handler follows packed authority")

	client, err := daemonclient.New(daemonclient.Config{
		URL: httpServer.URL, AllowInsecure: true,
	})
	require.NoError(err)
	configureRemoteDaemonForTest(t, httpServer.URL)
	mcpOpts, err := daemonMCPServeOptions(context.Background(), client)
	require.NoError(err)
	require.NotNil(mcpOpts.AttachmentReader)
	mcpData, err := mcpOpts.AttachmentReader.ReadAttachment(context.Background(), hash)
	require.NoError(err)
	assert.Equal(content, mcpData, "daemon-backed MCP attachment reader follows packed authority")

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
	require.NoError(runExportAttachmentHTTP(cmd, hash))
	single, err := os.ReadFile(exportAttachmentOutput)
	require.NoError(err)
	assert.Equal(content, single)

	directory := t.TempDir()
	dirResult := exportAttachmentsFromHTTP(context.Background(), client, directory,
		[]query.AttachmentInfo{{
			Filename: filename, ContentHash: hash, Size: int64(len(content)),
		}})
	require.Empty(dirResult.Errors)
	require.Len(dirResult.Files, 1)
	directoryData, err := os.ReadFile(dirResult.Files[0].Path)
	require.NoError(err)
	assert.Equal(content, directoryData)

	zipPath := filepath.Join(t.TempDir(), "post-repack.zip")
	zipStats := msgexport.AttachmentsWithOpener(zipPath,
		[]query.AttachmentInfo{{
			Filename: filename, ContentHash: hash, Size: int64(len(content)),
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
	assert.Equal(content, zippedData)
}
