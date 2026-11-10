package cmd

import (
	"bytes"
	"database/sql"
	"fmt"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	matrixsource "go.kenn.io/msgvault/internal/matrix"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMatrixSyncSummaryBackupFailureAndKeyHint(t *testing.T) {
	setup := func(t *testing.T) (*store.Store, *config.Config, matrixsource.Credentials) {
		t.Helper()
		req := require.New(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"next_batch":"next","rooms":{"join":{}}}`))
		})
		mux.HandleFunc("GET /_matrix/client/v3/user/{user}/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		})
		mux.HandleFunc("GET /_matrix/client/v3/room_keys/version", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		s := testutil.NewTestStore(t)
		source, err := s.GetOrCreateSource(sourceTypeMatrix, "@archive:example.org")
		req.NoError(err)
		convID, err := s.EnsureConversation(t.Context(), source.ID, "!room:example.org", "Room")
		req.NoError(err)
		_, err = s.PersistMatrixMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{SourceID: source.ID, ConversationID: convID, SourceMessageID: "$pending", MessageType: "matrix", Snippet: sql.NullString{String: store.MatrixEncryptedPlaceholder, Valid: true}},
			RawMIME: []byte(`{"type":"m.room.encrypted"}`), RawFormat: "matrix_json",
		}, store.MatrixPersist{SessionID: "session"})
		req.NoError(err)
		home := t.TempDir()
		cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
		creds := matrixsource.Credentials{Homeserver: server.URL, UserID: source.Identifier, DeviceID: "ARCHIVE", AccessToken: "token"}
		return s, cfg, creds
	}
	t.Run("saved backup key overrides skip preference", func(t *testing.T) {
		req := require.New(t)
		s, cfg, creds := setup(t)
		source, err := s.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeMatrix, creds.UserID)
		req.NoError(err)
		req.NoError(s.UpdateSourceSyncConfig(t.Context(), source.ID, `{"skip_key_backup":true}`))
		creds.BackupKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), creds))
		var out bytes.Buffer
		req.ErrorContains(runMatrixSync(t.Context(), s, cfg, "", false, false, nil, nil, &out), "key backup unavailable: ")
		assert.Contains(t, out.String(), "1 encrypted placeholders", "the summary reports the pending count")
	})
	t.Run("single hint across sources", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		s, cfg, creds := setup(t)
		req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), creds))
		var out bytes.Buffer
		second, err := s.GetOrCreateSource(sourceTypeMatrix, "@second:example.org")
		req.NoError(err)
		secondRoom, err := s.EnsureConversation(t.Context(), second.ID, "!room:example.org", "Room")
		req.NoError(err)
		_, err = s.PersistMatrixMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: second.ID, ConversationID: secondRoom, SourceMessageID: "$second", MessageType: "matrix", Snippet: sql.NullString{String: store.MatrixEncryptedPlaceholder, Valid: true}}, RawMIME: []byte(`{"type":"m.room.encrypted"}`), RawFormat: "matrix_json"}, store.MatrixPersist{SessionID: "session"})
		req.NoError(err)
		req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), matrixsource.Credentials{Homeserver: creds.Homeserver, UserID: second.Identifier, DeviceID: "ARCHIVE", AccessToken: "token"}))
		req.NoError(runMatrixSync(t.Context(), s, cfg, "", false, false, nil, nil, &out))
		check.Equal(1, strings.Count(out.String(), "run msgvault add-matrix --homeserver"))
	})
	t.Run("unreadable preference", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		s, cfg, creds := setup(t)
		sources, err := s.ListSources(sourceTypeMatrix)
		req.NoError(err)
		req.NoError(s.UpdateSourceSyncConfig(t.Context(), sources[0].ID, `"invalid JSON"`))
		req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), creds))
		var out bytes.Buffer
		req.NoError(runMatrixSync(t.Context(), s, cfg, "", false, false, nil, nil, &out))
		check.Contains(out.String(), "run msgvault add-matrix --homeserver")
	})
	t.Run("skip key backup", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		s, cfg, creds := setup(t)
		sources, err := s.ListSources(sourceTypeMatrix)
		req.NoError(err)
		req.NoError(s.UpdateSourceSyncConfig(t.Context(), sources[0].ID, `{"skip_key_backup":true}`))
		req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), creds))
		var out bytes.Buffer
		req.NoError(runMatrixSync(t.Context(), s, cfg, "", false, false, nil, nil, &out))
		check.NotContains(out.String(), "--recovery-file")
	})
}

func TestMatrixSyncSummaryMediaOutput(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"next_batch":"next","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/{user}/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	s := testutil.NewTestStore(t)
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	creds := matrixsource.Credentials{Homeserver: server.URL, UserID: "@archive:example.org", DeviceID: "ARCHIVE", AccessToken: "token"}
	req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), creds))
	source, err := s.GetOrCreateSource(sourceTypeMatrix, creds.UserID)
	req.NoError(err)
	room, err := s.EnsureConversation(t.Context(), source.ID, "!room:example.org", "Media")
	req.NoError(err)
	for i, state := range []attachmentpolicy.DownloadState{attachmentpolicy.StatePending, attachmentpolicy.StateFailed} {
		messageID, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: fmt.Sprintf("$media%d", i), MessageType: "matrix"})
		req.NoError(err)
		req.NoError(s.ReplaceMessageMatrixAttachments(t.Context(), messageID, []store.AttachmentRef{{SourceAttachmentID: "matrix:mxc://example.org/file", StoragePath: "mxc://example.org/file", State: state}}))
	}
	var out bytes.Buffer
	req.NoError(runMatrixSync(t.Context(), s, cfg, creds.UserID, false, true, nil, nil, &out))
	check.Contains(out.String(), "1 media pending, 1 media failed")
}
