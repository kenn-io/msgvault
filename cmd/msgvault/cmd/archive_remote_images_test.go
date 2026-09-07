package cmd

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestArchiveRemoteImagesRequiresTrackingConsentBeforeDispatch(t *testing.T) {
	require := require.New(t)
	command := newArchiveRemoteImagesCmd()
	require.ErrorContains(command.RunE(command, nil), "--allow-tracking")
	require.NoError(command.Flags().Set("allow-tracking", "true"))
	require.NoError(command.Flags().Set("limit", "-1"))
	require.ErrorContains(command.RunE(command, nil), "limit")
}

func TestArchivedRemoteImagesThroughDaemonAdapter(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	assert.True(attachmentProducingCommand([]string{"archive-remote-images", "--allow-tracking"}))
	st := testutil.NewTestStore(t)
	configuration := config.NewDefaultConfig()
	configuration.Data.DataDir = t.TempDir()
	src, err := st.GetOrCreateSource("eml", "user@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "remote-images", "Images")
	require.NoError(err)
	target := "https://images.example/chart.png"
	key := fmt.Sprintf("remote-image:%x", sha256.Sum256([]byte(target)))
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message:  &store.Message{SourceID: src.ID, SourceMessageID: "remote-image", ConversationID: conv, MessageType: "email"},
		BodyHTML: sql.NullString{String: `<img src="` + target + `">`, Valid: true},
	})
	require.NoError(err)
	content := []byte("\x89PNG\r\n\x1a\nimage")
	receipt, err := export.StoreAttachmentFileDurable(configuration.AttachmentsDir(), &mime.Attachment{ContentType: "image/png", Content: content})
	require.NoError(err)
	require.NoError(st.UpsertAttachmentRecord(t.Context(), id, store.AttachmentWrite{
		Filename: "image.png", MIMEType: "image/png", StoragePath: receipt.StoragePath, ContentHash: receipt.ContentHash,
		Size: int64(len(content)), SourceAttachmentID: key, SourcePartKey: key, ContentID: key,
		Role: store.AttachmentRoleInline, RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: configuration, Store: &storeAPIAdapter{store: st}, Logger: slog.Default()})
	response := httptest.NewRecorder()
	daemon.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil))
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "cid:"+key)
	response = httptest.NewRecorder()
	daemon.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d/inline?cid=%s", id, key), nil))
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal(content, response.Body.Bytes())
}

func TestConfiguredRemoteImageFetcherDefaultsOff(t *testing.T) {
	assert := assert.New(t)
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = config.NewDefaultConfig()
	assert.Nil(configuredRemoteImageFetcher())
	cfg.Sync.ArchiveRemoteImages = true
	assert.NotNil(configuredRemoteImageFetcher())
	cfg.Sync.ArchiveRemoteImages = false
	assert.Nil(configuredRemoteImageFetcher())
}
