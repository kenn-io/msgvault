package cmd

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// A real Cobra command must use the scoped content endpoint and preserve the
// native empty-versus-unavailable distinction through its JSON output.
func TestInboxCLIContextRealDaemon(t *testing.T) {
	for _, mode := range []string{"bounded", "empty", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f := storetest.New(t)
			mid := f.CreateMessage("synthetic-context")
			_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET is_read=FALSE WHERE id=?`), mid)
			requirements.NoError(err)
			target := inboxcontrol.Target{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "synthetic-context"}
			if mode != "unavailable" {
				body := ""
				if mode == "bounded" {
					body = "Synthetic untrusted body"
				}
				_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, body)
				requirements.NoError(err)
			}
			server := httptest.NewServer(api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler)).Router())
			defer server.Close()
			ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true}})
			payload, err := json.Marshal(inboxcontrol.ContextRequest{Target: target, MaxBytes: 9})
			requirements.NoError(err)
			command := newInboxCmd()
			command.SetContext(ctx)
			command.SetArgs([]string{"context", "--request", "-"})
			command.SetIn(strings.NewReader(string(payload)))
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			requirements.NoError(command.Execute())
			var result inboxcontrol.Context
			requirements.NoError(json.Unmarshal(output.Bytes(), &result))
			assertions.Equal(target, result.Target)
			assertions.Equal(mid, result.MessageID)
			if mode == "bounded" {
				assertions.Equal("Synthetic", result.Text)
			} else {
				assertions.Empty(result.Text)
			}
			assertions.Equal(mode == "bounded", result.Truncated)
			assertions.Equal(mode == "unavailable", result.Unavailable)
			var isRead bool
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT is_read FROM messages WHERE id=?`), mid).Scan(&isRead))
			assertions.False(isRead, "archive context must not mark the message read")
		})
	}
}

func TestInboxCLIContextRejectsInvalidRequest(t *testing.T) {
	for _, body := range []string{"null", "{}", `{"unexpected":true}`, strings.Repeat(" ", (1<<20)+1)} {
		command := newInboxCmd()
		command.SetArgs([]string{"context", "--request", "-"})
		command.SetIn(strings.NewReader(body))
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		require.ErrorIs(t, command.Execute(), inboxcontrol.ErrInvalid)
	}
	command := newInboxCmd()
	command.SetArgs([]string{"context", "--request", "-", "--apply"})
	command.SetIn(strings.NewReader("{}"))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	assert.Error(t, command.Execute(), "read-only context has no apply mode")
}
