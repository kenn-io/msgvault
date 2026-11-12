package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/testutil/email"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderAuthMessageReads(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	root := t.TempDir()
	email.Maildir(t, root)
	raw := email.NewMessage().
		From(`Sender <"a@b c"@example.test>`).
		To("reader@example.test").
		Subject("Authentication").
		Date("Tue, 03 Nov 2026 12:00:00 +0000").
		Header("Message-ID", "<auth@example.test>").
		Header("Authentication-Results", "mx.google.com; spf=pass; dkim=pass; dmarc=pass header.from=example.test").
		Body("Archived body.").
		CRLF().
		Bytes()
	requirements.NoError(os.WriteFile(filepath.Join(root, "new", "message"), raw, 0o600))
	summary, err := importer.ImportMaildir(t.Context(), f.Store, root, importer.MaildirImportOptions{Identifier: "reader@example.test", AttachmentsDir: t.TempDir()})
	requirements.NoError(err)
	requirements.Equal(int64(1), summary.MessagesAdded)
	var id int64
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT id FROM messages WHERE source_id = ?`), summary.SourceID).Scan(&id))
	noRaw := f.CreateMessage("no-raw")
	const want = `{"verified":true,"domain":"example.test","authserv_id":"mx.google.com","spf":"pass","dkim":"pass","dmarc":"pass"}`
	for _, withEngine := range []bool{false, true} {
		t.Run(fmt.Sprintf("engine=%t", withEngine), func(t *testing.T) {
			for _, pinned := range []bool{false, true} {
				t.Run(fmt.Sprintf("pinned=%t", pinned), func(t *testing.T) {
					cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
					if pinned {
						cfg.SenderAuth.Sources = []config.SenderAuthSource{{SourceID: summary.SourceID, TrustedAuthservIDs: []string{"mx.google.com"}}, {SourceID: f.Source.ID, TrustedAuthservIDs: []string{"mx.google.com"}}}
					}
					opts := ServerOptions{Config: cfg, Store: f.Store, Logger: testLogger()}
					if withEngine {
						opts.Engine = query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL())
						t.Cleanup(func() { require.NoError(t, opts.Engine.Close()) })
					}
					srv := NewServerWithOptions(opts)
					for _, messageType := range []string{"email", "google_chat"} {
						_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type = ? WHERE id = ?`), messageType, id)
						require.NoError(t, err)
						paths := []string{fmt.Sprintf("/api/v1/messages/%d", id)}
						if withEngine {
							paths = append(paths, fmt.Sprintf("/api/v1/cli/message?id=%d", id))
						}
						for _, path := range paths {
							w := httptest.NewRecorder()
							srv.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
							require.Equal(t, http.StatusOK, w.Code, w.Body.String())
							var got map[string]json.RawMessage
							require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
							if pinned && messageType == "email" {
								assert.JSONEq(t, want, string(got["sender_auth"]))
							} else {
								assert.NotContains(t, got, "sender_auth")
							}
						}
					}
					if pinned {
						w := httptest.NewRecorder()
						srv.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", noRaw), nil))
						require.Equal(t, http.StatusOK, w.Code, w.Body.String())
						var got struct {
							SenderAuth *senderauth.Verdict `json:"sender_auth"`
						}
						require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
						assert.Equal(t, &senderauth.Verdict{}, got.SenderAuth)
					}
				})
			}
		})
	}
}

func TestSenderAuthReadFailureLogs(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("corrupt-raw")
	require.NoError(f.Store.UpsertMessageRaw(id, email.NewMessage().From("sender@example.test").Bytes()))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_raw SET raw_data = ? WHERE message_id = ?`), []byte("corrupt zlib"), id)
	require.NoError(err)
	var logs bytes.Buffer
	cfg := &config.Config{SenderAuth: config.SenderAuthConfig{Sources: []config.SenderAuthSource{{SourceID: f.Source.ID, TrustedAuthservIDs: []string{"mx.google.com"}}}}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: f.Store, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	got := srv.senderAuth(t.Context(), id, f.Source.ID, "email", "sender@example.test")
	assert.Equal(&senderauth.Verdict{}, got)
	assert.Contains(logs.String(), "failed to read sender authentication headers")
	assert.Contains(logs.String(), fmt.Sprintf("message_id=%d source_id=%d", id, f.Source.ID))
	assert.Contains(logs.String(), "zlib")
}
