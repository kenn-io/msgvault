package daemonclient_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
				cli, err := client.GetCLIMessage(t.Context(), strconv.FormatInt(id, 10))
				requirements.NoError(err)
				check(cli, cli.Verification)
				assertions.Equal(src.ID, cli.SourceID)
				rows, err := adapter.ListMessages(t.Context(), query.MessageFilter{})
				requirements.NoError(err)
				requirements.Len(rows, 1)
				check(rows[0], rows[0].Verification)
				messageRef := query.MessageRef{ID: id}
				thread := query.ThreadQuery{MessageRef: messageRef, Limit: 10}
				page, err := adapter.ListThread(t.Context(), thread)
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

			imageURL := "http://images.example/chart.png"
			imageDigest := sha256.Sum256([]byte(imageURL))
			imageKey := "remote-image:" + hex.EncodeToString(imageDigest[:])
			htmlBody := `<p>Your verification code is 482<!-- > -->913.</p><img src="http://images.example/chart.png">`
			htmlOnlyID, err := st.UpsertMessage(&store.Message{
				SourceID: src.ID, ConversationID: conv, SourceMessageID: "html-only-provider-message", MessageType: "beeper",
				Subject: sql.NullString{String: "HTML-only notice", Valid: true},
				Snippet: sql.NullString{String: "Ordinary notice", Valid: true},
			})
			requirements.NoError(err)
			requirements.NoError(st.UpsertMessageBody(htmlOnlyID, sql.NullString{}, sql.NullString{String: htmlBody, Valid: true}))
			requirements.NoError(st.UpsertRemoteImageAttachment(t.Context(), htmlOnlyID, store.AttachmentWrite{
				Filename: "chart.png", MIMEType: "image/png", StoragePath: "remote-images/chart.png",
				ContentHash: strings.Repeat("e", 64), Size: 1, SourceAttachmentID: imageKey,
				SourcePartKey: imageKey, ContentID: imageKey, MediaType: "image",
			}))
			requirements.NoError(st.UpsertFTS(htmlOnlyID, "HTML-only notice", "Ordinary notice", "", "", ""))
			htmlDetail, err := daemonclient.NewEngineAdapter(owner).GetMessage(t.Context(), htmlOnlyID)
			requirements.NoError(err)
			assertions.Contains(htmlDetail.BodyHTML, "cid:"+imageKey)
			assertions.NotContains(htmlDetail.BodyHTML, imageURL)
			assertions.Empty(htmlDetail.BodyText)
			assertions.NotNil(verification.Detect(verification.Input{
				Subject: htmlDetail.Subject, BodyText: htmlDetail.BodyText, BodyHTML: htmlDetail.BodyHTML,
			}))
		})
	}
}

func TestEngineGetMessageDoesNotFallBackToProviderID(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-source")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "direct-chat", "direct_chat", "Synthetic chat")
	requirements.NoError(err)
	providerMessageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID, SourceMessageID: "42", MessageType: "beeper",
		Subject: sql.NullString{String: "Unrelated provider message", Valid: true},
	})
	requirements.NoError(err)
	assertions.NotEqual(int64(42), providerMessageID, "the provider ID is not the archive's internal ID")

	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	cfg := &config.Config{
		HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()},
		Server: config.ServerConfig{APIKey: "synthetic-owner-key"},
	}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(server.Shutdown(context.Background())) })
	httpServer := httptest.NewServer(server.Router())
	t.Cleanup(httpServer.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL: httpServer.URL, APIKey: "synthetic-owner-key", AllowInsecure: true, HTTPClient: httpServer.Client(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	resolved, err := client.GetCLIMessage(t.Context(), "42")
	requirements.NoError(err)
	requirements.NotNil(resolved)
	assertions.Equal(providerMessageID, resolved.ID, "the CLI resolver accepts provider IDs")

	got, err := daemonclient.NewEngineAdapter(client).GetMessage(t.Context(), 42)
	requirements.NoError(err)
	assertions.Nil(got, "an internal-ID lookup must not return a provider-ID match")
}
