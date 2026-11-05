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
	"strconv"
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
			assertions.NotContains(w.Body.String(), "abcDEF1234567890")
			assertions.Contains(w.Body.String(), "[verification content masked]")
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
		assertions.Contains(w.Body.String(), "[verification content masked]", path)
	}
}

func TestAgentVerificationDetailAlternatives(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, _, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "seed", "Seed", "")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	requestCount := 0
	tokens := make(map[string]string)
	for _, mode := range []string{"masked", "reveal", "modifier only"} {
		permissions := []agentgrant.Permission{agentgrant.PermissionMessageRead, agentgrant.PermissionSearchRead}
		if mode == "reveal" {
			permissions = append(permissions, agentgrant.PermissionVerificationRead)
		}
		if mode == "modifier only" {
			permissions = []agentgrant.Permission{agentgrant.PermissionVerificationRead}
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
					for _, mode := range []string{"owner", "masked", "reveal", "modifier only"} {
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
						if mode == "modifier only" {
							assertions.Equal(http.StatusForbidden, w.Code)
							continue
						}
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
	srv.SetAnalyticsEngine(query.NewEngine(st.DB(), st.IsPostgreSQL()), AnalyticsModeSQL)
	for _, path := range []string{"/search?q=code", "/messages", "/cli/message/thread?id=1", "/cli/accounts"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
		requestCount++
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", requestCount)
		req.Header.Set(apiprotocol.AgentTokenHeader, tokens["modifier only"])
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assertions.Equal(http.StatusForbidden, w.Code, path)
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
	for _, tc := range []struct{ name, subject, snippet, text, html string }{
		{name: "title cue", subject: "482913", snippet: "verification"},
		{name: "body cue filename", subject: "verification", text: "Your code"},
		{name: "filename cue", subject: "Your code", snippet: "verification"},
		{name: "thread snippet", subject: "482913", snippet: "Your verification code"},
		{name: "metadata", subject: "Your code is 482913", snippet: "verification", text: "verification"},
		{name: "title", subject: "verification", snippet: "verification", text: "verification"},
		{name: "snippet", subject: "verification", snippet: "Your code is 482913"},
		{name: "tail", subject: "verification", snippet: "verification", text: strings.Repeat("ordinary ", 33000) + " Your verification code is 482913"},
		{name: "entity repeated", subject: "verification", text: "Your verification code is 482913", html: "Your verification code is 482913 and &#52;&#56;&#50;&#57;&#49;&#51;"},
		{name: "nbsp separated", subject: "verification", text: "Your verification code is 4\u00a08\u00a02\u00a09\u00a01\u00a03", html: "Your verification code is 4&nbsp;8&nbsp;2&nbsp;9&nbsp;1&nbsp;3"},
		{name: "repeated link", subject: "verification", text: "Verification: Sign in https://example.test/r?token=abcDEF1234567890 repeat abcDEF1234567890"},
		{name: "stored cells", subject: "verification", snippet: "verification", text: "Your verification code is 4 8 2 9 1 3", html: "Your verification code is <td>4</td><td>8</td><td>2</td><td>9</td><td>1</td><td>3</td>"},
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
					assertions.NotContains(w.Body.String(), "4 8 2 9 1 3")
					assertions.NotContains(w.Body.String(), "abcDEF1234567890")
					if path == fmt.Sprintf("/messages/%d", id) || strings.HasPrefix(path, "/cli/message?") {
						assertions.Contains(w.Body.String(), "[verification content masked]")
					} else if (tc.name != "snippet" || !strings.HasPrefix(path, "/search/deep")) && (tc.name != "title" || strings.HasPrefix(path, "/cli/search")) {
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
			assertions.NotContains(w.Body.String(), "4 8 2 9 1 3")
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
		permissions := []agentgrant.Permission{agentgrant.PermissionAttachmentRead}
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
		{"code.txt", "text/plain", "Your code is 482913", false},
		{"parent.txt", "text/plain", "\x00Ordinary text file", true},
		{"parent.png", "image/png", "\x89PNG\r\n\x1a\n\x00Your code is 482913", false},
		{"parent.eml", "application/octet-stream", "\x00Ordinary email", true},
		{"parent.msg", "message/rfc822", "\x00Ordinary message", true},
		{"parent.bin", "application/octet-stream", "Ordinary text", true},
		{"mail.eml", "message/rfc822", "Subject: Your code\r\n\r\nYour code is 482913", false},
		{"large.txt", "text/plain", strings.Repeat("a", 1024*1024+1), false},
		{"ordinary.txt", "text/plain", "Ordinary attachment", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			subject := "Ordinary"
			if strings.HasPrefix(tc.name, "parent.") {
				subject = "Your code is 482913"
			}
			_, err := st.DB().Exec(st.Rebind("UPDATE messages SET subject=? WHERE id=?"), subject, id)
			requirements.NoError(err)
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
						}
					}
				}
			}
		})
	}
}

func TestAgentVerificationAttachmentNames(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "message", "Ordinary", "Ordinary")
	requirements.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	const body = "\x89PNG\r\n\x1a\n\x00Ordinary attachment"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	path, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
	requirements.NoError(err)
	requirements.NoError(os.MkdirAll(filepath.Dir(path), 0700))
	requirements.NoError(os.WriteFile(path, []byte(body), 0600))
	requirements.NoError(st.UpsertAttachment(id, "code-482913.png", "image/png", "", hash, len(body)))
	attachments, err := srv.queryEngineForContext(context.Background()).GetAttachmentsByHash(context.Background(), hash)
	requirements.NoError(err)
	requirements.Len(attachments, 1)
	for _, reveal := range []bool{false, true} {
		permissions := []agentgrant.Permission{agentgrant.PermissionAttachmentRead, agentgrant.PermissionMessageRead}
		if reveal {
			permissions = append(permissions, agentgrant.PermissionVerificationRead)
		}
		_, token, _, err := srv.agentGrants.Issue(strconv.FormatBool(reveal), permissions, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
		requirements.NoError(err)
		for _, endpoint := range []string{fmt.Sprintf("/attachments/%d", attachments[0].ID), "/attachments/" + hash + "/content", "/cli/attachment?content_hash=" + hash, fmt.Sprintf("/cli/message?id=%d", id)} {
			req := httptest.NewRequest(http.MethodGet, "/api/v1"+endpoint, nil)
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assertions.Equal(http.StatusOK, w.Code, w.Body.String())
			output := w.Body.String()
			if strings.Contains(endpoint, "/content") || strings.HasPrefix(endpoint, "/cli/attachment?") {
				assertions.Equal(body, output)
				output = w.Header().Get("Content-Disposition")
				assertions.NotEmpty(output)
			}
			if reveal {
				assertions.Contains(output, "482913", endpoint)
			} else {
				assertions.NotContains(output, "482913", endpoint)
				assertions.Contains(output, "verification content masked", endpoint)
			}
		}
	}
}

func TestAgentVerificationShortExcerpts(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	text := strings.Repeat("a", 145) + " Your code is 482913"
	source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "message", "Verification", text)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET snippet=? WHERE id=?"), "Your code is 48…", id)
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, endpoint := range []string{"/cli/search?q=code&include_snippet=true", "/messages", "/search/deep?q=code&scope=body"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1"+endpoint, nil)
		req.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assertions.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), "[verification content masked]", endpoint)
		assertions.NotContains(w.Body.String(), "Your code is 48", endpoint)
		var response struct {
			Results []struct {
				Snippet      string `json:"snippet"`
				MatchSnippet string `json:"match_snippet"`
				BodyText     string `json:"body_text"`
			} `json:"results"`
			Messages     []MessageSummary    `json:"messages"`
			BodyContexts []BodySearchContext `json:"body_contexts"`
		}
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
		if strings.HasPrefix(endpoint, "/cli/search") {
			requirements.Len(response.Results, 1)
			assertions.Equal("[verification content masked]", response.Results[0].Snippet)
			assertions.Equal("[verification content masked]", response.Results[0].MatchSnippet)
			assertions.Empty(response.Results[0].BodyText)
		} else {
			requirements.Len(response.Messages, 1)
			assertions.Equal("[verification content masked]", response.Messages[0].Snippet)
		}
		if strings.HasPrefix(endpoint, "/search/deep") {
			requirements.Len(response.BodyContexts, 1)
			assertions.Equal([]string{"[verification content masked]"}, response.BodyContexts[0].ContextSnippets)
		}
	}
}

func TestAgentVerificationSharedAttachment(t *testing.T) {
	for _, mode := range []string{"text", "binary", "other source"} {
		binary := mode == "binary"
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			source, first, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "first", "Ordinary", "")
			requirements.NoError(err)
			identifier := "reader@example.test"
			if mode == "other source" {
				identifier = "other@example.test"
			}
			_, second, err := testutil.CreateIndexedSourceMessage(st, identifier, "second", "Your code is 482913", "")
			requirements.NoError(err)
			cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
			srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
			t.Cleanup(func() { assertions.NoError(srv.Shutdown(context.Background())) })
			body, mimeType, filename := "Ordinary", "text/plain", "482913.txt"
			if binary {
				body, mimeType, filename = "\x89PNG\r\n\x1a\n\x00Ordinary", "image/png", "482913.png"
			}
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
			requirements.NoError(os.MkdirAll(cfg.AttachmentsDir(), 0700))
			requirements.NoError(os.WriteFile(filepath.Join(cfg.AttachmentsDir(), "present"), []byte(body), 0600))
			requirements.NoError(st.UpsertAttachment(first, "ordinary", mimeType, "missing", hash, len(body)))
			requirements.NoError(st.UpsertAttachment(second, filename, mimeType, "present", hash, len(body)))
			// CLI uses the content-addressed file; HTTP exercises the recorded-file fallback.
			path, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
			requirements.NoError(err)
			requirements.NoError(os.MkdirAll(filepath.Dir(path), 0700))
			requirements.NoError(os.WriteFile(path, []byte(body), 0600))
			_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionAttachmentRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
			requirements.NoError(err)
			for _, endpoint := range []string{"/attachments/" + hash + "/content", "/cli/attachment?content_hash=" + hash} {
				if strings.HasPrefix(endpoint, "/attachments/") && mode != "other source" {
					requirements.NoError(os.Remove(path))
				} else {
					requirements.NoError(os.WriteFile(path, []byte(body), 0600))
				}
				req := httptest.NewRequest(http.MethodGet, "/api/v1"+endpoint, nil)
				req.Header.Set(apiprotocol.AgentTokenHeader, token)
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, req)
				if binary {
					assertions.Equal(http.StatusOK, w.Code, w.Body.String())
					assertions.Equal(body, w.Body.String())
					assertions.NotContains(w.Header().Get("Content-Disposition"), "482913")
					if strings.HasPrefix(endpoint, "/attachments/") {
						assertions.Contains(w.Header().Get("Content-Disposition"), "verification content masked")
					}
				} else {
					requireErrorCode(t, w, http.StatusForbidden, "verification_masked")
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
