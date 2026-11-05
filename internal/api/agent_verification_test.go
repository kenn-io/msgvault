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
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "code", "Your verification code is 482913", "Your verification code is 482913")

	requirements.NoError(err)
	participant, err := st.EnsureParticipant("sender@example.test", "", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{participant}, nil))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET snippet = ? WHERE id = ?"), "…code is 4829", id)
	requirements.NoError(err)
	const link = "https://example.test/reset?token=abcDEF1234567890"
	message, err := st.GetMessage(id)
	requirements.NoError(err)
	chatID, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "chat", ConversationID: message.ConversationID, MessageType: "chat", Subject: sql.NullString{String: "Reset your password " + link, Valid: true}, Snippet: sql.NullString{String: link, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(chatID, sql.NullString{String: "Reset your password " + link, Valid: true}, sql.NullString{}))
	requirements.NoError(st.UpsertFTS(chatID, "Reset your password "+link, "Reset your password "+link, "", "", ""))
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)

	_, revealToken, _, err := srv.agentGrants.Issue("reveal", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead, agentgrant.PermissionVerificationRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, path := range []string{
		fmt.Sprintf("/cli/message?id=%d", id), fmt.Sprintf("/messages/%d", id),
		"/cli/search?q=verification&include_snippet=true", "/search?q=verification", "/messages/filter", "/messages", "/search/domains?domains=example.test",
		"/search/fast?q=verification", "/search/deep?q=verification&scope=body", fmt.Sprintf("/cli/message/thread?id=%d", id),
	} {
		t.Run(path, func(t *testing.T) {
			assertions := assert.New(t)
			req := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assertions.Equal(http.StatusOK, w.Code, w.Body.String())
			assertions.NotContains(w.Body.String(), "482913")
			assertions.NotContains(w.Body.String(), "4829")
			assertions.NotContains(w.Body.String(), "abcDEF1234567890")
			if strings.Contains(path, "q=") {
				assertions.NotContains(w.Body.String(), "[verification content masked]")
			} else {
				assertions.Contains(w.Body.String(), "[verification content masked]")
			}
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
			}
		})
	}
	for _, path := range []string{fmt.Sprintf("/cli/message?id=%d", chatID), fmt.Sprintf("/messages/%d", chatID), "/cli/search?q=reset&include_snippet=true", "/search?q=reset", "/search/fast?q=reset", "/search/deep?q=reset&scope=body", fmt.Sprintf("/cli/message/thread?id=%d", chatID)} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
		req.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assertions.Equal(http.StatusOK, w.Code, path+": "+w.Body.String())
		assertions.NotContains(w.Body.String(), "abcDEF1234567890", path)
		if strings.Contains(path, "q=") {
			assertions.NotContains(w.Body.String(), "[verification content masked]", path)
		} else {
			assertions.Contains(w.Body.String(), "[verification content masked]", path)
		}
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
		{"code", "Your code is 482913", "Your code is 482913", "<p>Your code is 482913</p>"},
		{"HTML only", "Sign in", "", "<a href=\" \thttps://example.test/r?token=abcDEF1234567890 \r\n\">Sign in https://example.test/r?token=abcDEF1234567890</a>"},
		{"HTML beside text", "Account", "Ordinary text", "<p>Your code is 482913</p>"},
		{"cells", "Account", "", "Your verification code is <table><tr><td>4</td><td>8</td><td>2</td><td>9</td><td>1</td><td>3</td></tr></table>"},
		{"entity repeat", "Account", "Your code is 482913", "<p>Your code is 482913 and &#52;&#56;&#50;&#57;&#49;&#51;</p>"},
		{"nbsp repeat", "Account", "", "<p>Your code is 4&nbsp;8&nbsp;2&nbsp;9&nbsp;1&nbsp;3</p>"},
		{"repeated link payload", "Sign in", "Sign in https://example.test/r?token=abcDEF1234567890 repeat abcDEF1234567890", ""},
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
						if mode == "masked" && tc.name != "ordinary" {
							var body struct{ BodyText, BodyHTML string }
							// Both detail APIs expose the same body keys.
							var fields map[string]json.RawMessage
							requirements.NoError(json.Unmarshal(w.Body.Bytes(), &fields))
							requirements.NoError(json.Unmarshal(fields["body_text"], &body.BodyText))
							if value := fields["body_html"]; value != nil {
								requirements.NoError(json.Unmarshal(value, &body.BodyHTML))
							}
							assertions.Equal("[verification content masked]", body.BodyText)
							assertions.Empty(body.BodyHTML)
							assertions.NotContains(w.Body.String(), "482913")
							assertions.NotContains(w.Body.String(), "abcDEF1234567890")
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
	assertions.False(srv.maskMessageSummaries(w, req, []MessageSummary{{ID: id, Subject: "Your code is 482913"}}))
	requireErrorCode(t, w, http.StatusServiceUnavailable, "source_scope_unavailable")
	assertions.NotContains(w.Body.String(), "482913")
}

func TestAgentVerificationFields(t *testing.T) {
	for _, tc := range []struct{ name, subject, snippet, text, html string }{
		{name: "newline", subject: "Account", text: "Your verification code is:\n\n482913"},
		{name: "title cue", subject: "482913", snippet: "verification"},
		{name: "body cue filename", subject: "verification", text: "Your code"},
		{name: "filename cue", subject: "Your code", snippet: "verification"},
		{name: "thread snippet", subject: "482913", snippet: "Your verification code"},
		{name: "metadata", subject: "Your code is 482913", snippet: "verification", text: "verification"},
		{name: "title", subject: "verification", snippet: "verification", text: "verification"},
		{name: "snippet", subject: "verification", snippet: "Your code is 482913"},
		{name: "tail", subject: "verification", snippet: "verification", text: strings.Repeat("ordinary ", 33000) + " Your verification code is 482913"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "message", tc.subject, tc.text)
			requirements.NoError(err)
			requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: tc.text, Valid: true}, sql.NullString{String: tc.html, Valid: true}))
			_, err = st.DB().Exec(st.Rebind("UPDATE messages SET snippet=? WHERE id=?"), tc.snippet, id)
			requirements.NoError(err)
			msg, err := st.GetMessage(id)
			requirements.NoError(err)
			title := tc.subject
			if tc.name == "title cue" {
				title = "Your code"
			}
			if tc.name == "title" {
				title = "Your code is 482913"
			}
			_, err = st.DB().Exec(st.Rebind("UPDATE conversations SET title=? WHERE id=?"), title, msg.ConversationID)
			requirements.NoError(err)
			requirements.NoError(st.UpsertAttachment(id, "482913.png", "image/png", "", strings.Repeat("a", 64), 1))
			cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
			srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
			t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
			_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
			requirements.NoError(err)
			for _, path := range []string{
				fmt.Sprintf("/messages/%d", id), fmt.Sprintf("/cli/message?id=%d", id), fmt.Sprintf("/cli/message/thread?id=%d", id),
				"/cli/search?q=verification&include_snippet=true", "/search?q=verification", "/messages/filter", "/search/fast?q=verification", "/search/deep?q=verification&scope=body", "/messages",
			} {
				if tc.name == "newline" && path == "/search/fast?q=verification" {
					path = "/search/fast?q=Account"
				}
				if (tc.name == "filename cue" || tc.name == "title cue" || tc.name == "body cue filename" || tc.name == "thread snippet") && strings.Contains(path, "q=") {
					continue
				}
				t.Run(path, func(t *testing.T) {
					assertions := assert.New(t)
					req := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
					req.Header.Set(apiprotocol.AgentTokenHeader, token)
					w := httptest.NewRecorder()
					srv.Router().ServeHTTP(w, req)
					assertions.Equal(http.StatusOK, w.Code, w.Body.String())
					assertions.NotContains(w.Body.String(), "482913")
					assertions.NotContains(w.Body.String(), "abcDEF1234567890")
					if path == fmt.Sprintf("/messages/%d", id) || strings.HasPrefix(path, "/cli/message?") {
						assertions.Contains(w.Body.String(), "[verification content masked]")
					} else if !strings.Contains(path, "q=") && (tc.name != "snippet" || !strings.HasPrefix(path, "/search/deep")) && (tc.name != "title" || strings.HasPrefix(path, "/cli/search")) {
						assertions.Contains(w.Body.String(), "[verification content masked]")
					}
				})
			}
			srv.SetAnalyticsEngine(nil, "")
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil)
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assertions.Equal(http.StatusOK, w.Code, w.Body.String())
			assertions.NotContains(w.Body.String(), "482913")
			assertions.NotContains(w.Body.String(), "abcDEF1234567890")
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
		{"ordinary.txt", "text/plain", "Ordinary attachment", true},
		{"mail.eml", "message/rfc822", "Subject: Your code\r\n\r\nYour code is 482913", true},
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
			requirements.NoError(st.UpsertAttachment(id, tc.name, tc.mime, "", hash, len(tc.body)))
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
								if mode == "masked" {
									assertions.Contains(w.Header().Get("Content-Disposition"), "verification content masked")
									assertions.NotContains(w.Header().Get("Content-Disposition"), "482913")
								} else {
									assertions.Contains(w.Header().Get("Content-Disposition"), tc.name)
								}
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
						if mode == "masked" {
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

func TestAgentVerificationDetailUsesLoadedBody(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "message", "Account", "Your code is 482913")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { assertions.NoError(srv.Shutdown(context.Background())) })
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	msg, err := st.GetMessage(id)
	requirements.NoError(err)
	// The loaded response is the classification input even if the database changes.
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Ordinary", Valid: true}, sql.NullString{}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1", nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token)
	srv.maskStoreMessage(req, msg)
	assertions.Equal("[verification content masked]", msg.BodyText)
	detail := &query.MessageDetail{ID: id, Subject: "Account", BodyText: "Your code is 482913"}
	srv.maskMessageDetail(req, detail)
	assertions.Equal("[verification content masked]", detail.BodyText)
}

func TestAgentVerificationSearchOmitsMatches(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, hidden, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "otp", "Your verification code is 482913", "Your verification code is 482913")
	requirements.NoError(err)
	_, ordinary, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "ordinary", "Order 482913 shipped", "Order 482913 shipped")
	requirements.NoError(err)
	participant, err := st.EnsureParticipant("sender@example.test", "Sender", "example.test")
	requirements.NoError(err)
	for _, id := range []int64{hidden, ordinary} {
		requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{participant}, nil))
	}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	requestCount := 0
	for _, mode := range []string{"delegate", "reveal", "owner"} {
		permissions := []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionStatsRead}
		if mode == "reveal" {
			permissions = append(permissions, agentgrant.PermissionVerificationRead)
		}
		_, token, _, err := srv.agentGrants.Issue(mode, permissions, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
		requirements.NoError(err)
		for _, route := range []string{"/search?q=", "/search/fast?q=", "/search/fast?view_type=senders&q=", "/search/deep?q=", "/search/deep?view_type=senders&q=", "/search/deep?scope=body&q=", "/cli/search?q="} {
			for _, term := range []string{"482913", "48291", "4829", "shipped", "verification"} {
				t.Run(mode+route+term, func(t *testing.T) {
					requirements, assertions := require.New(t), assert.New(t)
					req := httptest.NewRequest(http.MethodGet, "/api/v1"+route+term, nil)
					requestCount++
					req.RemoteAddr = fmt.Sprintf("127.0.0.%d:1234", requestCount)
					if mode == "owner" {
						req.Header.Set("X-Api-Key", "owner")
					} else {
						req.Header.Set(apiprotocol.AgentTokenHeader, token)
					}
					w := httptest.NewRecorder()
					srv.Router().ServeHTTP(w, req)
					requirements.Equal(http.StatusOK, w.Code, w.Body.String())
					type searchRow struct {
						ID int64 `json:"id"`
					}
					var response struct {
						Messages     []searchRow         `json:"messages"`
						Results      []searchRow         `json:"results"`
						Total        *int64              `json:"total"`
						TotalCount   *int64              `json:"total_count"`
						Stats        *TotalStatsResponse `json:"stats"`
						BodyContexts []BodySearchContext `json:"body_contexts"`
					}
					requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
					ids := make([]int64, 0)
					for _, row := range response.Messages {
						ids = append(ids, row.ID)
					}
					for _, row := range response.Results {
						ids = append(ids, row.ID)
					}
					if term != "verification" {
						assertions.Contains(ids, ordinary)
					} else {
						assertions.NotContains(ids, ordinary)
					}
					if mode == "delegate" || term == "shipped" {
						assertions.NotContains(ids, hidden)
						if term != "verification" {
							assertions.Len(ids, 1)
						} else {
							assertions.Empty(ids)
						}
					} else {
						assertions.Contains(ids, hidden)
					}
					for _, count := range []*int64{response.Total, response.TotalCount} {
						if count != nil {
							assertions.Equal(int64(len(ids)), *count)
						}
					}
					if mode == "delegate" {
						assertions.Nil(response.Stats)
						for _, context := range response.BodyContexts {
							assertions.NotEqual(hidden, context.MessageID)
						}
					}
				})
			}
		}
		for _, route := range []string{"/aggregates?view_type=senders&search_query=482913", "/aggregates/sub?view_type=senders&search_query=482913", "/stats/total?search_query=482913", "/search/fast?limit=0&q=482913"} {
			t.Run(mode+route, func(t *testing.T) {
				requirements, assertions := require.New(t), assert.New(t)
				req := httptest.NewRequest(http.MethodGet, "/api/v1"+route, nil)
				requestCount++
				req.RemoteAddr = fmt.Sprintf("127.0.0.%d:1234", requestCount)
				if mode == "owner" {
					req.Header.Set("X-Api-Key", "owner")
				} else {
					req.Header.Set(apiprotocol.AgentTokenHeader, token)
				}
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, req)
				requirements.Equal(http.StatusOK, w.Code, w.Body.String())
				var response struct {
					Rows         []AggregateRowJSON `json:"rows"`
					MessageCount int64              `json:"message_count"`
					TotalCount   int64              `json:"total_count"`
				}
				requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
				if mode == "delegate" {
					assertions.Empty(response.Rows)
					assertions.Zero(response.MessageCount)
					assertions.Zero(response.TotalCount)
				} else if strings.HasPrefix(route, "/search/") {
					assertions.Equal(int64(2), response.TotalCount)
				} else if strings.HasPrefix(route, "/stats/") {
					assertions.Equal(int64(2), response.MessageCount)
				} else {
					requirements.Len(response.Rows, 1)
					assertions.Equal(int64(2), response.Rows[0].Count)
				}
			})
		}
	}
}
