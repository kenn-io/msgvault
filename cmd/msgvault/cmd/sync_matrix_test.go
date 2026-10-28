package cmd

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	matrixsource "go.kenn.io/msgvault/internal/matrix"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMatrixSyncSummaryBackupFailureAndKeyHint(t *testing.T) {
	setup := func(t *testing.T) (*store.Store, *config.Config, matrixsource.Credentials, *bool, *int) {
		t.Helper()
		req := require.New(t)
		newPlaceholder := new(bool)
		backupRequests := new(int)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
			if *newPlaceholder {
				_, _ = w.Write([]byte(`{"next_batch":"next","rooms":{"join":{"!room:example.org":{"timeline":{"events":[{"type":"m.room.encrypted","event_id":"$new","sender":"@member:example.org","content":{"algorithm":"m.megolm.v1.aes-sha2","session_id":"unknown","ciphertext":"unknown"}}]}}}}}`))
			} else {
				_, _ = w.Write([]byte(`{"next_batch":"next","rooms":{"join":{}}}`))
			}
		})
		mux.HandleFunc("GET /_matrix/client/v3/user/{user}/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		})
		mux.HandleFunc("GET /_matrix/client/v3/room_keys/version", func(w http.ResponseWriter, _ *http.Request) {
			*backupRequests++
			w.WriteHeader(http.StatusBadRequest)
		})
		mux.HandleFunc("GET /_matrix/client/v3/rooms/{room}/joined_members", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"joined":{}}`))
		})
		mux.HandleFunc("GET /_matrix/client/v3/rooms/{room}/messages", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"chunk":[]}`))
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		s := testutil.NewTestStore(t)
		source, err := s.GetOrCreateSource(sourceTypeMatrix, "@archive:example.org")
		req.NoError(err)
		convID, err := s.EnsureConversation(source.ID, "!room:example.org", "Room")
		req.NoError(err)
		_, err = s.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: convID, SourceMessageID: "$pending", MessageType: "matrix", Snippet: sql.NullString{String: store.MatrixEncryptedPlaceholder, Valid: true}}})
		req.NoError(err)
		ids, err := s.MessageExistsBatch(source.ID, []string{"$pending"})
		req.NoError(err)
		req.NoError(s.UpsertMessageRawWithFormat(ids["$pending"], []byte(`{"type":"m.room.encrypted"}`), "matrix_json"))
		home := t.TempDir()
		cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
		creds := matrixsource.Credentials{Homeserver: server.URL, UserID: source.Identifier, DeviceID: "ARCHIVE", AccessToken: "token"}
		return s, cfg, creds, newPlaceholder, backupRequests
	}
	t.Run("failure", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		s, cfg, creds, _, _ := setup(t)
		creds.BackupKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), creds))
		var out bytes.Buffer
		req.ErrorContains(runMatrixSync(t.Context(), s, cfg, "", false, false, nil, nil, &out), "key backup unavailable, 1 encrypted events pending: load Matrix key backup")
		check.NotContains(out.String(), "key backup unavailable")
	})
	t.Run("key hint", func(t *testing.T) {
		for _, scenario := range []string{"single account", "second account", "new placeholder", "excluded room"} {
			t.Run(scenario, func(t *testing.T) {
				req, check := require.New(t), assert.New(t)
				s, cfg, creds, newPlaceholder, _ := setup(t)
				req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), creds))
				if scenario != "single account" {
					second, err := s.GetOrCreateSource(sourceTypeMatrix, "@second:example.org")
					req.NoError(err)
					secondRoom, err := s.EnsureConversation(second.ID, "!room:example.org", "Room")
					req.NoError(err)
					_, err = s.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: second.ID, ConversationID: secondRoom, SourceMessageID: "$second", MessageType: "matrix", Snippet: sql.NullString{String: store.MatrixEncryptedPlaceholder, Valid: true}}, RawMIME: []byte(`{"type":"m.room.encrypted"}`), RawFormat: "matrix_json"})
					req.NoError(err)
					req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), matrixsource.Credentials{Homeserver: creds.Homeserver, UserID: second.Identifier, DeviceID: "ARCHIVE", AccessToken: "token"}))
				}
				*newPlaceholder = scenario == "new placeholder"
				if scenario == "excluded room" {
					cfg.Matrix.ExcludeRooms = []string{"!room:example.org"}
				}
				var out bytes.Buffer
				req.NoError(runMatrixSync(t.Context(), s, cfg, "", false, false, nil, nil, &out))
				if scenario == "excluded room" {
					check.NotContains(out.String(), "--recovery-file")
					return
				}
				check.Equal(1, strings.Count(out.String(), "run msgvault add-matrix --homeserver"))
				check.Contains(out.String(), fmt.Sprintf("--homeserver %q --user-id %q --recovery-file <path>", creds.Homeserver, creds.UserID))
				if scenario == "new placeholder" {
					check.Equal(2, strings.Count(out.String(), "2 encrypted placeholders"))
				}
			})
		}
	})
	for _, syncConfig := range []string{`"invalid JSON"`, `{"skip_key_backup":"invalid"}`, `{"skip_key_backup":true,"skip_key_backup":"invalid"}`} {
		t.Run("unreadable preference/"+syncConfig, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			s, cfg, creds, newPlaceholder, _ := setup(t)
			*newPlaceholder = true
			sources, err := s.ListSources(sourceTypeMatrix)
			req.NoError(err)
			req.NoError(s.UpdateSourceSyncConfig(sources[0].ID, syncConfig))
			req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), creds))
			var out bytes.Buffer
			req.NoError(runMatrixSync(t.Context(), s, cfg, "", false, false, nil, nil, &out))
			check.Contains(out.String(), "2 encrypted placeholders")
			check.Contains(out.String(), "run msgvault add-matrix --homeserver")
		})
	}
	t.Run("skip key backup", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		s, cfg, creds, _, backupRequests := setup(t)
		sources, err := s.ListSources(sourceTypeMatrix)
		req.NoError(err)
		req.NoError(s.UpdateSourceSyncConfig(sources[0].ID, `{"skip_key_backup":true}`))
		creds.BackupKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), creds))
		var out bytes.Buffer
		req.NoError(runMatrixSync(t.Context(), s, cfg, "", false, false, nil, nil, &out))
		check.Contains(out.String(), "1 encrypted placeholders")
		check.NotContains(out.String(), "--recovery-file")
		check.Zero(*backupRequests)
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
	room, err := s.EnsureConversation(source.ID, "!room:example.org", "Media")
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
