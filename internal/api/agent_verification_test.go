package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAgentVerificationReads(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "code", "Your verification code is 482913", "Your verification code is 482913")

	requirements.NoError(err)
	participant, err := st.EnsureParticipant("sender@example.test", "", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{participant}, nil))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET snippet = ? WHERE id = ?"), "…code is 4829", id)
	requirements.NoError(err)
	_, _, err = testutil.CreateIndexedSourceMessage(st, "reader@example.test", "second-code", "Your verification code is 482913", "Your verification code is 482913")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)

	_, revealToken, _, err := srv.agentGrants.Issue("reveal", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead, agentgrant.PermissionVerificationRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, path := range []string{
		fmt.Sprintf("/cli/message?id=%d", id), fmt.Sprintf("/messages/%d", id),
		"/search?q=verification", "/search/fast?q=verification", "/search/deep?q=verification&scope=body", "/cli/search?q=verification&include_snippet=true",
		"/search/deep?q=verification",
		"/search?q=verification&page_size=1&page=2", "/search/fast?q=verification&limit=1&offset=1", "/search/deep?q=verification&limit=1&offset=1", "/search/deep?q=verification&scope=body&limit=1&offset=1", "/cli/search?q=verification&include_snippet=true&limit=1&offset=1",
		"/messages/filter", "/messages", "/search/domains?domains=example.test", fmt.Sprintf("/cli/message/thread?id=%d", id),
	} {
		t.Run(path, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			req := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assertions.Equal(http.StatusOK, w.Code, w.Body.String())
			assertions.NotContains(w.Body.String(), "482913")
			assertions.NotContains(w.Body.String(), "4829")
			assertions.Contains(w.Body.String(), "[verification content masked]")
			var masked map[string]json.RawMessage
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &masked))
			for _, credential := range []string{"owner", revealToken} {
				req := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
				req.RemoteAddr = "127.0.0.1:1234"
				if credential == "owner" {
					req.Header.Set("X-Api-Key", credential)
				} else {
					req.Header.Set(apiprotocol.AgentTokenHeader, credential)
				}
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, req)
				assertions.Equal(http.StatusOK, w.Code, w.Body.String())
				assertions.Contains(w.Body.String(), "482913")
				assertions.NotContains(w.Body.String(), "[verification content masked]")
				if strings.HasPrefix(path, "/search") || strings.HasPrefix(path, "/cli/search?") {
					var revealed map[string]json.RawMessage
					requirements.NoError(json.Unmarshal(w.Body.Bytes(), &revealed))
					for _, field := range []string{"total", "total_count"} {
						if field == "total" && strings.HasPrefix(path, "/search?") || field == "total_count" && (strings.HasPrefix(path, "/search/fast?") || strings.HasPrefix(path, "/search/deep?")) {
							assertions.Contains(revealed, field)
						}
						assertions.Equal(revealed[field], masked[field], field)
					}
				}
			}
		})
	}
}

func TestAgentVerificationDetailAlternatives(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, _, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "seed", "Seed", "")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	requestCount := 0
	tokens := make(map[string]string)
	for _, mode := range []string{"masked", "reveal"} {
		permissions := []agentgrant.Permission{agentgrant.PermissionMessageRead, agentgrant.PermissionSearchRead}
		if mode == "reveal" {
			permissions = append(permissions, agentgrant.PermissionVerificationRead)
		}
		_, token, _, err := srv.agentGrants.Issue(mode, permissions, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
		requirements.NoError(err)
		tokens[mode] = token
	}
	for _, tc := range []struct{ name, subject, text, html string }{
		{"code", "Account", "Your code is 482913", ""},
		{"HTML only", "Sign in", "", "<a href=\" \thttps://example.test/r?token=abcDEF1234567890 \r\n\">Sign in https://example.test/r?token=abcDEF1234567890</a>"},
		{"HTML beside text", "Account", "Ordinary text", "<p>Your code is 482913</p>"},
		{"ordinary", "Project update", "Order 1234567 shipped. Log in to track it. © 2026", "<p>Project update</p>"},
		{"uncapped detail", "Account", strings.Repeat("x", 256*1024) + " Your code is 482913", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			_, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", tc.name, tc.subject, tc.text)
			requirements.NoError(err)
			requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: tc.text, Valid: true}, sql.NullString{String: tc.html, Valid: true}))

			for _, withEngine := range []bool{true, false} {
				paths := []string{fmt.Sprintf("/cli/message?id=%d", id), fmt.Sprintf("/messages/%d", id)}
				if withEngine {
					srv.SetAnalyticsEngine(query.NewEngine(st.DB(), st.IsPostgreSQL()), AnalyticsModeSQL)
				} else {
					srv.SetAnalyticsEngine(nil, "")
					paths = paths[1:]
				}
				for _, path := range paths {
					responses := make(map[string]string)
					for _, mode := range []string{"owner", "masked", "reveal"} {
						req := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
						requestCount++
						req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", requestCount)
						if mode == "owner" {
							req.Header.Set("X-Api-Key", "owner")
						} else {
							req.Header.Set(apiprotocol.AgentTokenHeader, tokens[mode])
						}
						w := httptest.NewRecorder()
						srv.Router().ServeHTTP(w, req)
						assertions.Equal(http.StatusOK, w.Code, w.Body.String())
						responses[mode] = w.Body.String()
						var body struct {
							BodyText string `json:"body_text"`
							BodyHTML string `json:"body_html"`
						}
						requirements.NoError(json.Unmarshal(w.Body.Bytes(), &body))
						if mode == "masked" && tc.name != "ordinary" {
							assertions.Equal("[verification content masked]", body.BodyText)
							assertions.Empty(body.BodyHTML)
							assertions.NotContains(w.Body.String(), "482913")
							assertions.NotContains(w.Body.String(), "abcDEF1234567890")
						} else {
							assertions.Equal(tc.text, body.BodyText)
							assertions.Equal(tc.html, body.BodyHTML)
						}
					}
					assertions.Equal(responses["owner"], responses["reveal"])
					if tc.name == "ordinary" {
						assertions.Equal(responses["owner"], responses["masked"])
					}
				}
			}
		})
	}
}

func TestAgentVerificationModifierGrantsNoRead(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	registry := agentgrant.NewRegistry()
	t.Cleanup(registry.Close)
	_, _, grant, err := registry.Issue("modifier", []agentgrant.Permission{agentgrant.PermissionVerificationRead}, []agentgrant.SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}}, time.Time{})
	requirements.NoError(err)
	for operation := range agentReadPermissions {
		_, denied := agentReadPermission(operation, &grant)
		requirements.NotNil(denied, operation)
		assertions.Equal(http.StatusForbidden, denied.GetStatus(), operation)
	}
}

func TestAgentVerificationUnavailableLoader(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "code", "Your code is 482913", "")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	// Hide only the optional loader capability.
	srv.store = struct{ MessageStore }{st}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	assertions.False(srv.maskMessageSummaries(w, req, []MessageSummary{{ID: id, Subject: "Your code is 482913"}}, nil))
	requireErrorCode(t, w, http.StatusServiceUnavailable, "source_scope_unavailable")
	assertions.NotContains(w.Body.String(), "482913")
}

func TestAgentVerificationFields(t *testing.T) {
	for _, tc := range []struct{ name, subject, snippet, title, text, filename string }{
		{name: "title cue", subject: "482913", title: "Your code"},
		{name: "body cue", subject: "482913", text: "Your code"},
		{name: "filename cue", subject: "Your code", filename: "482913.png"},
		{name: "thread snippet", subject: "482913", snippet: "Your verification code"},
		{name: "metadata", subject: "Your code is 482913"},
		{name: "title", title: "Your code is 482913"},
		{name: "snippet", snippet: "Your code is 482913"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := testutil.NewTestStore(t)
			source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "message", tc.subject, tc.text)
			requirements.NoError(err)
			_, err = st.DB().Exec(st.Rebind("UPDATE messages SET snippet=? WHERE id=?"), tc.snippet, id)
			requirements.NoError(err)
			msg, err := st.GetMessage(id)
			requirements.NoError(err)
			_, err = st.DB().Exec(st.Rebind("UPDATE conversations SET title=? WHERE id=?"), tc.title, msg.ConversationID)
			requirements.NoError(err)
			if tc.filename != "" {
				requirements.NoError(st.UpsertAttachment(id, tc.filename, "image/png", "", strings.Repeat("a", 64), 1))
			}
			cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
			srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
			t.Cleanup(func() { assertions.NoError(srv.Shutdown(context.Background())) })
			_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
			requirements.NoError(err)
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/cli/message/thread?id=%d", id), nil)
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assertions.Equal(http.StatusOK, w.Code, w.Body.String())
			assertions.NotContains(w.Body.String(), "482913")
			assertions.Contains(w.Body.String(), "[verification content masked]")
			for _, withEngine := range []bool{true, false} {
				if !withEngine {
					srv.SetAnalyticsEngine(nil, "")
				}
				req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil)
				req.Header.Set(apiprotocol.AgentTokenHeader, token)
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, req)
				assertions.Equal(http.StatusOK, w.Code, w.Body.String())
				var body struct {
					BodyText string `json:"body_text"`
				}
				requirements.NoError(json.Unmarshal(w.Body.Bytes(), &body))
				assertions.Equal("[verification content masked]", body.BodyText)
				assertions.NotContains(w.Body.String(), "482913")
			}
		})
	}
}

func TestAgentVerificationAttachments(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "message", "Ordinary", "Ordinary")
	requirements.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	requestCount := 0
	tokens := map[string]string{"owner": "owner"}
	for _, mode := range []string{"masked", "reveal"} {
		permissions := []agentgrant.Permission{agentgrant.PermissionAttachmentRead, agentgrant.PermissionMessageRead}
		if mode == "reveal" {
			permissions = append(permissions, agentgrant.PermissionVerificationRead)
		}
		_, token, _, err := srv.agentGrants.Issue(mode, permissions, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
		requirements.NoError(err)
		tokens[mode] = token
	}
	for _, tc := range []struct {
		name, mime, body string
		blocked          bool
	}{
		{"ordinary.txt", "text/plain", "\x00Ordinary attachment", true},
		{"mail.bin", "message/rfc822", "\x00Ordinary mail", true},
		{"x.eml", "application/octet-stream", "\x00Ordinary email", true},
		{"data.bin", "application/octet-stream", "Ordinary text", true},
		{"image.png", "image/png", "\x89PNG\r\n\x1a\n\x00Ordinary image", false},
		{"code-482913.png", "image/png", "\x89PNG\r\n\x1a\n\x00Named image", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.body)))
			path, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
			requirements.NoError(err)
			requirements.NoError(os.MkdirAll(filepath.Dir(path), 0700))
			requirements.NoError(os.WriteFile(path, []byte(tc.body), 0600))
			messageID := id
			if tc.name == "x.eml" {
				_, messageID, err = testutil.CreateIndexedSourceMessage(st, "reader@example.test", "withheld-eml", "Your verification code is 482913", "")
				requirements.NoError(err)
			}
			requirements.NoError(st.UpsertAttachment(messageID, tc.name, tc.mime, "", hash, len(tc.body)))
			for _, blob := range []bool{false, true} {
				srv.blobStore = nil
				if blob {
					bs, err := attachmentstore.New(store.NewPackCatalog(st), cfg.AttachmentsDir())
					requirements.NoError(err)
					srv.blobStore = bs
					t.Cleanup(func() { requirements.NoError(bs.Close()) })
				}
				for _, endpoint := range []string{"/attachments/" + hash + "/content", "/cli/attachment?content_hash=" + hash} {
					for _, mode := range []string{"masked", "reveal", "owner"} {
						req := httptest.NewRequest(http.MethodGet, "/api/v1"+endpoint, nil)
						requestCount++
						req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", requestCount)
						if mode == "owner" {
							req.Header.Set("X-Api-Key", tokens[mode])
						} else {
							req.Header.Set(apiprotocol.AgentTokenHeader, tokens[mode])
						}
						w := httptest.NewRecorder()
						srv.Router().ServeHTTP(w, req)
						if mode == "masked" && tc.blocked {
							requireErrorCode(t, w, http.StatusForbidden, "verification_masked")
							assertions.NotContains(w.Body.String(), "482913")
						} else {
							assertions.Equal(http.StatusOK, w.Code, w.Body.String())
							assertions.Equal(tc.body, w.Body.String())
							if strings.HasPrefix(endpoint, "/cli/attachment?") {
								assertions.Empty(w.Header().Get("Content-Disposition"))
							} else if tc.name == "code-482913.png" {
								assertions.Contains(w.Header().Get("Content-Disposition"), tc.name)
							}
						}
					}
				}
			}
			if tc.name == "code-482913.png" {
				attachments, err := srv.queryEngineForContext(context.Background()).GetAttachmentsByHash(context.Background(), hash)
				requirements.NoError(err)
				requirements.Len(attachments, 1)
				for _, endpoint := range []string{fmt.Sprintf("/attachments/%d", attachments[0].ID), fmt.Sprintf("/cli/message?id=%d", id)} {
					for _, mode := range []string{"masked", "reveal", "owner"} {
						req := httptest.NewRequest(http.MethodGet, "/api/v1"+endpoint, nil)
						if mode == "owner" {
							req.Header.Set("X-Api-Key", "owner")
						} else {
							req.Header.Set(apiprotocol.AgentTokenHeader, tokens[mode])
						}
						w := httptest.NewRecorder()
						srv.Router().ServeHTTP(w, req)
						assertions.Equal(http.StatusOK, w.Code, w.Body.String())
						if mode == "masked" && strings.HasPrefix(endpoint, "/cli/message?") {
							assertions.NotContains(w.Body.String(), "482913")
							assertions.Contains(w.Body.String(), "verification content masked")
						} else {
							assertions.Contains(w.Body.String(), tc.name)
						}
					}
				}
			}
		})
	}
}
