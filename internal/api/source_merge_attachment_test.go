package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSourceMergePreservesLegacyAttachmentDownloads(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		partKey        string
		livePath       string
		writeCAS       bool
		wantLiveStatus int
		hidden         bool
	}{
		{name: "missing_live_storage", wantLiveStatus: http.StatusNotFound},
		{name: "both_stored_different_paths", livePath: "legacy/current.bin", wantLiveStatus: http.StatusOK, hidden: true},
		{name: "hash_only_live_with_cas", writeCAS: true, wantLiveStatus: http.StatusOK},
		{name: "keyed_missing_live_storage", partKey: "mime:1", wantLiveStatus: http.StatusNotFound},
		{name: "keyed_both_stored_different_paths", partKey: "mime:1", livePath: "legacy/current.bin", wantLiveStatus: http.StatusOK, hidden: true},
		{name: "keyed_hash_only_live_with_cas", partKey: "mime:1", writeCAS: true, wantLiveStatus: http.StatusOK},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("beeper", "history-example")
			require.NoError(err)
			into, err := st.GetOrCreateSource("beeper", "live-example")
			require.NoError(err)
			sender, err := st.EnsureParticipant("sender@example.test", "Example Sender", "example.test")
			require.NoError(err)
			cfg := &config.Config{Server: config.ServerConfig{APIPort: 8080}, Data: config.DataConfig{DataDir: t.TempDir()}}
			content := []byte("synthetic legacy attachment bytes")
			digest := sha256.Sum256(content)
			hash := hex.EncodeToString(digest[:])
			var attachmentIDs []int64
			for _, source := range []*store.Source{into, from} {
				conversation, err := st.EnsureConversation(source.ID, "!shared:example.test", "Example conversation")
				require.NoError(err)
				id, err := st.UpsertMessage(&store.Message{
					SourceID: source.ID, ConversationID: conversation, SourceMessageID: "shared-message", MessageType: "beeper",
					SenderID: sql.NullInt64{Int64: sender, Valid: true},
					SentAt:   sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
				})
				require.NoError(err)
				require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "same body", Valid: true}, sql.NullString{}))
				path := scenario.livePath
				if source.ID == from.ID {
					path = "legacy/historical.bin"
				}
				if path != "" {
					filename := filepath.Join(cfg.AttachmentsDir(), filepath.FromSlash(path))
					require.NoError(os.MkdirAll(filepath.Dir(filename), 0o700))
					require.NoError(os.WriteFile(filename, content, 0o600))
				}
				require.NoError(st.UpsertAttachment(id, "shared.bin", "application/octet-stream", path, hash, len(content)))
				if scenario.partKey != "" {
					_, err = st.DB().Exec(st.Rebind(`UPDATE attachments SET source_part_key = ? WHERE message_id = ?`), scenario.partKey, id)
					require.NoError(err)
				}
				var attachmentID int64
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM attachments WHERE message_id = ?`), id).Scan(&attachmentID))
				attachmentIDs = append(attachmentIDs, attachmentID)
			}
			if scenario.writeCAS {
				filename, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
				require.NoError(err)
				require.NoError(os.MkdirAll(filepath.Dir(filename), 0o700))
				require.NoError(os.WriteFile(filename, content, 0o600))
			}
			srv := NewServerWithOptions(ServerOptions{
				Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger(),
			})
			t.Cleanup(func() { require.NoError(srv.Shutdown(context.Background())) })
			download := func(id int64) *httptest.ResponseRecorder {
				response := httptest.NewRecorder()
				srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/files/%d/content", id), nil))
				return response
			}
			before := download(attachmentIDs[1])
			require.Equal(http.StatusOK, before.Code, before.Body.String())
			require.Equal(content, before.Body.Bytes())
			req := store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true}
			preview, err := st.MergeSourcesContext(t.Context(), req)
			require.NoError(err)
			req.DryRun = false
			result, err := st.MergeSourcesContext(t.Context(), req)
			require.NoError(err)
			preview.DryRun = false
			assert.Equal(preview, result)
			wantHidden := int64(0)
			if scenario.hidden {
				wantHidden = 1
			}
			assert.Equal(wantHidden, result.DuplicatesHidden)
			assert.Zero(result.AttachmentsCopied)
			live, history := download(attachmentIDs[0]), download(attachmentIDs[1])
			assert.Equal(scenario.wantLiveStatus, live.Code, live.Body.String())
			if scenario.hidden {
				assert.Equal(http.StatusNotFound, history.Code, history.Body.String())
			} else {
				require.Equal(http.StatusOK, history.Code, history.Body.String())
				assert.Equal(content, history.Body.Bytes())
			}
			if live.Code == http.StatusOK {
				assert.Equal(content, live.Body.Bytes())
			}
			byHash := httptest.NewRecorder()
			srv.Router().ServeHTTP(byHash, httptest.NewRequest(http.MethodGet, "/api/v1/attachments/"+hash+"/content", nil))
			require.Equal(http.StatusOK, byHash.Code, byHash.Body.String())
			assert.Equal(content, byHash.Body.Bytes())
		})
	}
}
