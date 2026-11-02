package daemonclient_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/verification"
)

// This uses the production router and generated client: dropping an optional
// verification field in any adapter would otherwise silently erase the policy.
func TestVerificationMetadataThroughDaemon(t *testing.T) {
	for _, long := range []bool{false, true} {
		t.Run(strconv.FormatBool(long), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("beeper", "synthetic-source")
			requirements.NoError(err)
			conv, err := st.EnsureConversationWithType(src.ID, "direct-chat", "direct_chat", "Synthetic chat")
			requirements.NoError(err)
			id, err := st.UpsertMessage(&store.Message{
				SourceID: src.ID, ConversationID: conv, SourceMessageID: "provider-message", MessageType: "beeper",
				Subject: sql.NullString{String: "glacier 482913", Valid: true},
				Snippet: sql.NullString{String: "482913", Valid: true},
				SentAt:  sql.NullTime{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true},
			})
			requirements.NoError(err)
			body := "Your verification code is 482913. Expires in 10 minutes."
			if long {
				body = strings.Repeat("Long ordinary context. ", 40) + body
			}
			requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{String: "<p>" + body + "</p>", Valid: true}))
			requirements.NoError(st.UpsertFTS(id, "glacier 482913", body, "", "", ""))
			htmlConv, err := st.EnsureConversationWithType(src.ID, "html-only-chat", "direct_chat", "HTML-only")
			requirements.NoError(err)
			htmlID, err := st.UpsertMessage(&store.Message{
				SourceID: src.ID, ConversationID: htmlConv, SourceMessageID: "html-only-message", MessageType: "beeper",
				Subject: sql.NullString{String: "HTML-only verification", Valid: true},
				SentAt:  sql.NullTime{Time: time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC), Valid: true},
			})
			requirements.NoError(err)
			htmlBody := `<p>Your verification code is <b>AB4</b><span>C9D</span></p>`
			requirements.NoError(st.UpsertMessageBody(htmlID, sql.NullString{}, sql.NullString{String: htmlBody, Valid: true}))
			hash := strings.Repeat("d", 64)
			requirements.NoError(st.UpsertAttachment(id, "482913.txt", "text/plain", "", hash, 32))
			engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
			t.Cleanup(func() { requirements.NoError(engine.Close()) })
			attachments, err := engine.GetAttachmentsByHash(t.Context(), hash)
			requirements.NoError(err)
			requirements.Len(attachments, 1)
			cfg := &config.Config{
				HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()},
				Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true},
			}
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
			t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
			server := httptest.NewServer(srv.Router())
			t.Cleanup(server.Close)
			owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true, HTTPClient: server.Client()})
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(owner.Close()) })
			for _, reveal := range []bool{false, true} {
				permissions := []string{"message.read", "search.read", "attachment.read"}
				if reveal {
					permissions = append(permissions, "verification.read")
				}
				grant, err := owner.IssueAgentToken(t.Context(), "synthetic reader", permissions, []int64{src.ID}, nil, time.Time{})
				requirements.NoError(err)
				client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true, HTTPClient: server.Client()})
				requirements.NoError(err)
				t.Cleanup(func() { requirements.NoError(client.Close()) })
				adapter := daemonclient.NewEngineAdapter(client)
				check := func(value any, metadata *verification.Metadata) {
					t.Helper()
					requirements.NotNil(metadata)
					assertions.Equal(!reveal, metadata.Masked)
					assertions.Equal("otp_code", metadata.Kind)
					requirements.NotNil(metadata.ExpiresAt)
					assertions.True(metadata.Expired, "expiry does not authorize reveal")
					encoded, err := json.Marshal(value)
					requirements.NoError(err)
					assertions.Equal(reveal, strings.Contains(string(encoded), "482913"))
					assertions.NotContains(string(encoded), `"values"`)
				}
				detail, err := adapter.GetMessage(t.Context(), id)
				requirements.NoError(err)
				check(detail, detail.Verification)
				assertions.Equal(src.ID, detail.SourceID)
				assertions.Equal("provider-message", detail.SourceMessageID)
				htmlDetail, err := adapter.GetMessage(t.Context(), htmlID)
				requirements.NoError(err)
				requirements.NotNil(htmlDetail.Verification)
				assertions.Equal(!reveal, htmlDetail.Verification.Masked)
				assertions.Empty(htmlDetail.BodyText)
				if reveal {
					assertions.Equal(htmlBody, htmlDetail.BodyHTML)
				} else {
					assertions.Equal(verification.ContentPlaceholder, htmlDetail.BodyHTML)
				}
				cli, err := client.GetCLIMessage(t.Context(), strconv.FormatInt(id, 10))
				requirements.NoError(err)
				check(cli, cli.Verification)
				assertions.Equal(src.ID, cli.SourceID)
				rows, err := adapter.ListMessages(t.Context(), query.MessageFilter{})
				requirements.NoError(err)
				requirements.Len(rows, 2)
				for _, row := range rows {
					if row.ID == id {
						check(row, row.Verification)
					}
				}
				page, err := adapter.ListThread(t.Context(), query.ThreadQuery{MessageRef: query.MessageRef{ID: id}, Limit: 10})
				requirements.NoError(err)
				requirements.Len(page.Messages, 1)
				check(page.Messages[0], page.Messages[0].Verification)
				attachment, err := adapter.GetAttachment(t.Context(), attachments[0].ID)
				requirements.NoError(err)
				requirements.NotNil(attachment)
				assertions.Equal(id, attachment.MessageID)
				assertions.Equal(src.ID, attachment.SourceID)
				assertions.Equal(reveal, strings.Contains(attachment.Filename, "482913"))
			}
			// Owner HTTP reads remain complete and carry no delegated policy decision.
			detail, err := daemonclient.NewEngineAdapter(owner).GetMessage(t.Context(), id)
			requirements.NoError(err)
			assertions.Contains(detail.BodyText, "482913")
			assertions.Nil(detail.Verification)
			htmlDetail, err := daemonclient.NewEngineAdapter(owner).GetMessage(t.Context(), htmlID)
			requirements.NoError(err)
			assertions.Empty(htmlDetail.BodyText)
			assertions.Equal(htmlBody, htmlDetail.BodyHTML)
			maskedHTML := query.PrepareVerification(htmlDetail, false, time.Time{})
			requirements.NotNil(maskedHTML.Verification)
			assertions.Equal(verification.ContentPlaceholder, maskedHTML.BodyHTML)
		})
	}
}
