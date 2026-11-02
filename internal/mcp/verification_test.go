package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/verification"
)

func TestVerificationOwnerChannelsAndPaging(t *testing.T) {
	for _, channel := range []struct{ sourceType, messageType string }{
		{"gmail", "email"}, {"beeper", "beeper"},
		{"imessage", "sms"}, {"imessage", "imessage"},
		{"imazing_csv", "sms"}, {"imazing_csv", "imessage"}, {"imazing_csv", "rcs"},
		{"synctech_sms", "sms"}, {"synctech_sms", "mms"},
		{"whatsapp", "whatsapp"}, {"teams", "teams"}, {"slack", "slack"},
		{"discord", "discord"}, {"matrix", "matrix"}, {"inline", "inline"},
		{"facebook_messenger", "fbmessenger"}, {"chatwoot", "chatwoot"},
	} {
		for _, long := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/long=%t", channel.sourceType, channel.messageType, long), func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)

				st := testutil.NewTestStore(t)
				src, err := st.GetOrCreateSource(channel.sourceType, "synthetic-source")
				requirements.NoError(err)
				conv, err := st.EnsureConversationWithType(src.ID, "direct-chat", "direct_chat", "Synthetic direct chat")
				requirements.NoError(err)
				body := "界 Your verification code is 482913."
				if long {
					body = strings.Repeat("ordinary text ", 40) + body
				}
				body += " Magic link: https://example.com/login?token=abcdefghijklmnop"
				id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "provider-message", MessageType: channel.messageType, Subject: sql.NullString{String: "glacier 482913", Valid: true}, Snippet: sql.NullString{String: "482913", Valid: true}})
				requirements.NoError(err)
				requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{String: "<p>" + body + "</p>", Valid: true}))
				requirements.NoError(st.UpsertFTS(id, "glacier 482913", body, "", "", ""))
				requirements.NoError(st.UpsertMessageRaw(id, []byte("Subject: code\r\n\r\n"+body)))
				h := newTestHandlers(query.NewEngine(st.DB(), st.IsPostgreSQL()))
				h.unmaskVerification = false
				t.Cleanup(h.downloads.close)
				for _, format := range []string{"text", "html"} {
					full := runTool[getMessageResponse](t, ToolGetMessage, h.getMessage, map[string]any{"id": float64(id), "body_format": format, "full_body": true})
					requirements.NotNil(full.Verification)
					assertions.True(full.Verification.Masked)
					assertions.Equal(src.ID, full.SourceID)
					assertions.Equal(conv, full.ConversationID)
					assertions.Equal("provider-message", full.SourceMessageID)
					expected := full.BodyText + full.BodyHTML
					assertions.NotContains(expected, "482913")
					assertions.NotContains(expected, "abcdefghijklmnop")
					var reconstructed strings.Builder
					for offset := 0; offset < full.BodyLength; {
						page := runTool[getMessageResponse](t, ToolGetMessage, h.getMessage, map[string]any{"id": float64(id), "body_format": format, "max_chars": float64(7), "offset": float64(offset)})
						text := page.BodyText + page.BodyHTML
						requirements.NotEmpty(text)
						assertions.True(utf8.ValidString(text))
						assertions.Equal(offset, page.Offset)
						reconstructed.WriteString(text)
						offset = page.Offset + page.BodyReturned
					}
					assertions.Equal(expected, reconstructed.String())
					center := runTool[getMessageResponse](t, ToolGetMessage, h.getMessage, map[string]any{"id": float64(id), "body_format": format, "max_chars": float64(7), "center_at": float64(full.BodyLength - 15)})
					assertions.Equal(expected[center.Offset:center.Offset+center.BodyReturned], center.BodyText+center.BodyHTML)
				}
				for _, tc := range []struct {
					name string
					fn   toolHandler
					args map[string]any
				}{
					{ToolListMessages, h.listMessages, nil},
					{ToolSearchMetadata, h.searchMetadata, map[string]any{"query": "glacier"}},
					{ToolListThread, h.listThread, map[string]any{"id": float64(id)}},
				} {
					result := callToolDirect(t, tc.name, tc.fn, tc.args)
					requirements.False(result.isError, result.text)
					assertions.NotContains(result.text, "482913")
					assertions.NotContains(result.text, "abcdefghijklmnop")
					assertions.Contains(result.text, `"masked":true`)
				}
				raw := runToolExpectError(t, ToolExportEML, h.exportEML, map[string]any{"id": float64(id)})
				assertions.Contains(raw.text, "verification_masked")
				h.unmaskVerification = true
				revealed := runTool[getMessageResponse](t, ToolGetMessage, h.getMessage, map[string]any{"id": float64(id), "full_body": true})
				assertions.Contains(revealed.BodyText, "482913")
				assertions.False(revealed.Verification.Masked)
				_ = runTool[exportEMLResponse](t, ToolExportEML, h.exportEML, map[string]any{"id": float64(id)})
			})
		}
	}
}

func TestVerificationOwnerFailsClosedOnHydration(t *testing.T) {
	assertions := assert.New(t)

	engine := &querytest.MockEngine{
		ListMessagesFunc: func(context.Context, query.MessageFilter) ([]query.MessageSummary, error) {
			return []query.MessageSummary{{ID: 17, Snippet: "482913"}}, nil
		},
		GetMessageFunc: func(context.Context, int64) (*query.MessageDetail, error) {
			return nil, errors.New("synthetic failure")
		},
	}
	h := newTestHandlers(engine)
	h.unmaskVerification = false
	result := runToolExpectError(t, ToolListMessages, h.listMessages, nil)
	assertions.NotContains(result.text, "482913")
	assertions.Contains(result.text, "verification_unavailable")
}

func TestVerificationOwnerSemanticContextsAndCapabilityGates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	canonical := &query.MessageDetail{ID: 17, BodyText: "Your verification code is 482913"}
	row := query.MessageSummary{ID: 17, Subject: "482913", Snippet: "482913"}
	h := newTestHandlers(&querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{17: canonical},
		GetMessageSummariesByIDsFunc: func(context.Context, []int64) ([]query.MessageSummary, error) {
			return []query.MessageSummary{row}, nil
		},
	})
	h.unmaskVerification = false
	h.hybridSearcher = hybridSearcherFunc(func(context.Context, HybridSearchRequest) (*HybridSearchResult, error) {
		return &HybridSearchResult{Hits: []HybridSearchHit{{ID: 17, Matches: []HybridSearchMatch{{Snippet: "482"}}}}}, nil
	})
	h.similarSearcher = similarSearcherFunc(func(context.Context, SimilarSearchRequest) (*SimilarSearchResult, error) {
		return &SimilarSearchResult{Messages: []query.MessageSummary{row}}, nil
	})
	semantic := runTool[searchMessageBodiesResponse](t, ToolSemanticSearchMessages, h.semanticSearchMessages, map[string]any{"query": "glacier"})
	requirements.Len(semantic.Data, 1)
	assertions.NotContains(semantic.Data[0].Subject, "482913")
	assertions.Empty(semantic.Data[0].Matches, "cropped credentials cannot be safely replaced")
	requirements.NotNil(semantic.Data[0].Verification)
	assertions.True(semantic.Data[0].Verification.Masked)
	similar := callToolDirect(t, ToolFindSimilarMessages, h.findSimilarMessages, map[string]any{"message_id": float64(9)})
	requirements.False(similar.isError, similar.text)
	assertions.NotContains(similar.text, "482913")
	for _, tc := range []struct {
		name string
		fn   toolHandler
		args map[string]any
	}{
		{ToolQuerySQL, h.querySQL, map[string]any{"query": "SELECT subject FROM messages"}},
		{ToolAggregate, h.aggregate, map[string]any{"group_by": "sender"}},
		{ToolSearchInMessage, h.searchInMessage, map[string]any{"id": float64(17), "query": "glacier", "mode": "vector"}},
	} {
		denied := runToolExpectError(t, tc.name, tc.fn, tc.args)
		assertions.Contains(denied.text, "verification_masked")
	}
}

func TestVerificationDelegatedMCPIgnoresOwnerOverride(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "synthetic-source")
	requirements.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "synthetic-chat", "direct_chat", "Synthetic")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "synthetic-message", MessageType: "beeper"})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Your verification code is 482913", Valid: true}, sql.NullString{}))
	hash := strings.Repeat("e", 64)
	requirements.NoError(st.UpsertAttachment(id, "482913.txt", "text/plain", "", hash, 32))
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	attachments, err := engine.GetAttachmentsByHash(t.Context(), hash)
	requirements.NoError(err)
	requirements.Len(attachments, 1)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(owner.Close()) })
	grant, err := owner.IssueAgentToken(t.Context(), "synthetic reader", []string{"message.read", "attachment.read"}, []int64{src.ID}, nil, time.Time{})
	requirements.NoError(err)
	agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(agent.Close()) })
	opts := agentServeOptions(t, agent)
	opts.UnmaskVerification, opts.AttachmentReader = true, agent
	message := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": ToolGetMessage, "arguments": map[string]any{"id": id, "full_body": true}})
	encoded, err := json.Marshal(message.Result)
	requirements.NoError(err)
	assertions.NotContains(string(encoded), "482913")
	assertions.Contains(string(encoded), `\"masked\":true`)
	attachment := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": ToolGetAttachment, "arguments": map[string]any{"attachment_id": attachments[0].ID}})
	assertions.Equal(true, attachment.Result["isError"])
	encoded, err = json.Marshal(attachment.Result)
	requirements.NoError(err)
	assertions.Contains(string(encoded), "verification_masked")
	assertions.NotContains(string(encoded), "482913")
}

func TestVerificationKeywordOffsetsAddressMaskedBody(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	canonical := &query.MessageDetail{ID: 17, BodyText: "İİ 界\nYour verification code is 482913. İ verification details."}
	h := newTestHandlers(&querytest.MockEngine{Messages: map[int64]*query.MessageDetail{17: canonical}})
	h.unmaskVerification = false
	served := runTool[getMessageResponse](t, ToolGetMessage, h.getMessage, map[string]any{"id": float64(17), "full_body": true})
	result := runTool[searchInMessageResponse](t, ToolSearchInMessage, h.searchInMessage, map[string]any{"id": float64(17), "query": "VERIFICATION"})
	requirements.Len(result.Data, 3, "the fixed masking placeholder is also searchable")
	for _, match := range result.Data {
		requirements.NotNil(match.CharOffset)
		assertions.Equal("verification", served.BodyText[*match.CharOffset:*match.CharOffset+len("verification")])
		assertions.NotContains(match.Snippet, "482913")
		assertions.True(utf8.ValidString(match.Snippet))
	}
	secret := runTool[searchInMessageResponse](t, ToolSearchInMessage, h.searchInMessage, map[string]any{"id": float64(17), "query": "482913"})
	assertions.Empty(secret.Data)
}

func TestVerificationCanonicalContextMasksSummary(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	canonical := &query.MessageDetail{ID: 17, BodyText: "Your verification code is 482913"}
	engine := &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{17: canonical},
		SearchFastWithStatsFunc: func(context.Context, *search.Query, string, query.MessageFilter, query.ViewType, int, int) (*query.SearchFastResult, error) {
			return &query.SearchFastResult{Messages: []query.MessageSummary{{ID: 17, Subject: "482913", Snippet: "482913"}}, TotalCount: 1}, nil
		},
	}
	h := newTestHandlers(engine)
	h.unmaskVerification = false
	result := callToolDirect(t, ToolSearchMetadata, h.searchMetadata, map[string]any{"query": "glacier"})
	requirements.False(result.isError, result.text)
	assertions.NotContains(result.text, "482913")
	assertions.Contains(result.text, `"masked":true`)
	assertions.Equal("Your verification code is 482913", canonical.BodyText)
	h.delegatedOnly = true
	masked := query.PrepareVerification(canonical, false, time.Now())
	assertions.Contains(masked.BodyText, verification.CodePlaceholder)
	assertions.Same(masked, h.prepareVerificationDetail(masked))
	assertions.Equal(fmt.Sprint(canonical.ID), "17")
}

func TestVerificationAttachmentAndRawContinuation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	x := newExportFixture(t)
	x.h.unmaskVerification = false
	data := []byte("synthetic attachment bytes")
	hash := strings.Repeat("a", 64)
	requirements.NoError(x.f.Store.UpsertAttachment(x.withRaw, "ordinary.txt", "text/plain", "", hash, len(data)))
	var attachmentID int64
	requirements.NoError(x.f.Store.DB().QueryRow(x.f.Store.Rebind("SELECT id FROM attachments WHERE message_id=?"), x.withRaw).Scan(&attachmentID))
	attachment, err := x.engine.GetAttachment(t.Context(), attachmentID)
	requirements.NoError(err)
	requirements.NotNil(attachment)
	assertions.Equal(x.withRaw, attachment.MessageID)
	assertions.Equal(x.f.Source.ID, attachment.SourceID)
	reads := 0
	x.h.attachmentReader = attachmentReaderFunc(func(context.Context, string) ([]byte, error) {
		reads++
		return data, nil
	})
	first := callToolDirect(t, ToolGetAttachment, x.h.getAttachment, map[string]any{"attachment_id": float64(attachmentID), "offset": float64(0), "length": float64(4)})
	requirements.False(first.isError, first.text)
	var chunk struct {
		SHA256 string `json:"sha256"`
	}
	requirements.NoError(json.Unmarshal([]byte(first.text), &chunk))
	requirements.NotEmpty(chunk.SHA256)
	raw := runTool[exportEMLResponse](t, ToolExportEML, x.h.exportEML, map[string]any{"id": float64(x.withRaw), "length": float64(4)})
	// A later canonical update must invalidate protection even when the initial
	// byte snapshot and digest still exist in the download cache.
	requirements.NoError(x.f.Store.UpsertMessageBody(x.withRaw, sql.NullString{String: "Your verification code is 482913", Valid: true}, sql.NullString{}))
	for _, args := range []map[string]any{
		{"attachment_id": float64(attachmentID)},
		{"attachment_id": float64(attachmentID), "offset": float64(4), "length": float64(4), "sha256": chunk.SHA256},
	} {
		denied := runToolExpectError(t, ToolGetAttachment, x.h.getAttachment, args)
		assertions.Contains(denied.text, "verification_masked")
		assertions.Nil(denied.embeddedResource)
		assertions.NotContains(denied.text, "data_base64")
	}
	_, err = x.h.attachmentService().load(t.Context(), attachmentID)
	requirements.ErrorContains(err, "verification_masked")
	assertions.Equal(1, reads, "denied reads must not touch attachment bytes")
	denied := runToolExpectError(t, ToolExportEML, x.h.exportEML, map[string]any{"id": float64(x.withRaw), "offset": float64(4), "length": float64(4), "sha256": raw.SHA256})
	assertions.Contains(denied.text, "verification_masked")
	assertions.NotContains(denied.text, "data_base64")
	// Parent lookup failure is a refusal even if a caller has metadata.
	missing := *attachment
	missing.MessageID = 0
	assertions.ErrorContains(x.h.protectVerificationAttachment(t.Context(), &missing), "verification_unavailable")
}

func TestDelegatedAttachmentContinuationRechecksVerificationPolicy(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const attachmentID = int64(19)
	const messageID = int64(81)
	data := []byte("synthetic attachment content")
	message := &query.MessageDetail{ID: messageID, SourceID: 3, BodyText: "ordinary message"}
	engine := &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{messageID: message},
		Attachments: map[int64]*query.AttachmentInfo{attachmentID: {
			ID: attachmentID, MessageID: messageID, SourceID: 3, Filename: "proof.txt",
			MimeType: "text/plain", StoragePath: "synthetic/path", ContentHash: strings.Repeat("a", 64), Size: int64(len(data)),
		}},
	}
	reads := 0
	h := &handlers{
		engine: engine, downloads: &downloadCache{}, delegatedOnly: true,
		attachmentReader: attachmentReaderFunc(func(context.Context, string) ([]byte, error) {
			reads++
			return data, nil
		}),
	}

	first := callToolDirect(t, ToolGetAttachment, h.getAttachment, map[string]any{
		"attachment_id": float64(attachmentID), "offset": float64(0), "length": float64(4),
	})
	requirements.False(first.isError, first.text)
	var chunk struct {
		SHA256 string `json:"sha256"`
	}
	requirements.NoError(json.Unmarshal([]byte(first.text), &chunk))
	requirements.NotEmpty(chunk.SHA256)

	message.Verification = &verification.Metadata{Masked: true}
	denied := runToolExpectError(t, ToolGetAttachment, h.getAttachment, map[string]any{
		"attachment_id": float64(attachmentID), "offset": float64(4), "length": float64(4), "sha256": chunk.SHA256,
	})
	assertions.Contains(denied.text, "verification_masked")
	assertions.Nil(denied.embeddedResource)
	assertions.NotContains(denied.text, "data_base64")
	assertions.Equal(1, reads, "a denied continuation must reuse no attachment bytes")
}

func TestVerificationBinaryAttachmentMetadata(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	x := newExportFixture(t)
	x.h.unmaskVerification = false
	requirements.NoError(x.f.Store.UpsertMessageBody(x.withRaw, sql.NullString{String: "Your verification code is 482913", Valid: true}, sql.NullString{}))
	hash := strings.Repeat("f", 64)
	requirements.NoError(x.f.Store.UpsertAttachment(x.withRaw, "482913.pdf", "application/pdf", "", hash, 4))
	attachments, err := x.engine.GetAttachmentsByHash(t.Context(), hash)
	requirements.NoError(err)
	requirements.Len(attachments, 1)
	x.h.attachmentReader = attachmentReaderFunc(func(context.Context, string) ([]byte, error) { return []byte("data"), nil })
	args := map[string]any{"attachment_id": float64(attachments[0].ID)}
	result := callToolDirect(t, ToolGetAttachment, x.h.getAttachment, args)
	requirements.False(result.isError, result.text)
	assertions.NotContains(result.text, "482913")
	requirements.NotNil(result.embeddedResource)
	args["offset"], args["length"] = float64(0), float64(1)
	chunk := callToolDirect(t, ToolGetAttachment, x.h.getAttachment, args)
	requirements.False(chunk.isError, chunk.text)
	assertions.NotContains(chunk.text, "482913")
	args = map[string]any{"attachment_id": float64(attachments[0].ID), "destination": t.TempDir()}
	exported := runTool[exportAttachmentResponse](t, ToolExportAttachment, x.h.exportAttachment, args)
	assertions.NotContains(exported.Filename, "482913")
	assertions.NotContains(exported.Path, "482913")
	assertions.Equal("482913.pdf", attachments[0].Filename, "shared metadata remains unchanged")
}
