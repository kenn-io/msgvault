package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestVerificationDelegatedChannels(t *testing.T) {
	// Source namespaces can differ from message types; reveal follows the source.
	for _, channel := range []struct{ sourceType, messageType string }{
		{"gmail", "email"}, {"beeper", "beeper"},
		{"imessage", "sms"}, {"imessage", "imessage"},
		{"imazing_csv", "sms"}, {"imazing_csv", "imessage"}, {"imazing_csv", "rcs"},
		{"synctech_sms", "sms"}, {"synctech_sms", "mms"},
		{"whatsapp", "whatsapp"}, {"teams", "teams"}, {"slack", "slack"},
		{"discord", "discord"}, {"matrix", "matrix"}, {"inline", "inline"},
		{"facebook_messenger", "fbmessenger"}, {"chatwoot", "chatwoot"},
	} {
		for _, mode := range []string{"short", "long", "dense"} {
			t.Run(channel.sourceType+"/"+channel.messageType+"/"+mode, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)

				st := testutil.NewTestStore(t)
				src, err := st.GetOrCreateSource(channel.sourceType, "synthetic-source")
				requirements.NoError(err)
				conv, err := st.EnsureConversationWithType(src.ID, "direct-chat", "direct_chat", "Synthetic direct chat")
				requirements.NoError(err)
				body := "Your verification code is 482913."
				if mode == "long" {
					body = strings.Repeat("Long ordinary context. ", 40) + body
				}
				if mode == "dense" {
					var bodyBuilder strings.Builder
					bodyBuilder.WriteString(body)
					for i := range 65 {
						_, _ = fmt.Fprintf(&bodyBuilder, " Verification code: %d.", 10000000+i)
					}
					body = bodyBuilder.String()
				}
				link := "https://example.com/login?token=abcdefghijklmnop"
				body += " Magic link: " + link
				id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "provider-message", MessageType: channel.messageType, Subject: sql.NullString{String: "glacier 482913", Valid: true}, Snippet: sql.NullString{String: "482913", Valid: true}, SentAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}})
				requirements.NoError(err)
				requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{String: "<p>" + body + "</p>", Valid: true}))
				requirements.NoError(st.UpsertFTS(id, "glacier 482913", body, "", "", ""))
				cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}
				srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
				t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
				issue := func(permissions []agentgrant.Permission, sources []agentgrant.SourceRef) string {
					_, token, _, err := srv.agentGrants.Issue("synthetic reader", permissions, sources, time.Time{})
					requirements.NoError(err)
					return token
				}
				scope := []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}
				read := []agentgrant.Permission{agentgrant.PermissionMessageRead, agentgrant.PermissionSearchRead}
				masked := issue(read, scope)
				reveal := issue(append(read, agentgrant.PermissionVerificationRead), scope)
				searchOnly := issue([]agentgrant.Permission{agentgrant.PermissionSearchRead}, scope)
				modifierOnly := issue([]agentgrant.Permission{agentgrant.PermissionVerificationRead}, scope)
				other, err := st.GetOrCreateSource(channel.sourceType, "other-source")
				requirements.NoError(err)
				wrongSource := issue(append(read, agentgrant.PermissionVerificationRead), []agentgrant.SourceRef{{ID: other.ID, Type: other.SourceType, Identifier: other.Identifier}})
				request := func(path, token string, owner bool) *httptest.ResponseRecorder {
					r := httptest.NewRequest(http.MethodGet, path, nil)
					if owner {
						r.Header.Set("X-Api-Key", "owner-key")
					} else {
						r.Header.Set(apiprotocol.AgentTokenHeader, token)
					}
					w := httptest.NewRecorder()
					srv.Router().ServeHTTP(w, r)
					return w
				}
				for _, path := range []string{fmt.Sprintf("/api/v1/messages/%d", id), fmt.Sprintf("/api/v1/cli/message?id=%d", id), fmt.Sprintf("/api/v1/cli/message/thread?id=%d", id), "/api/v1/messages", "/api/v1/messages/filter", "/api/v1/cli/search?q=glacier", "/api/v1/search?q=glacier", "/api/v1/search/fast?q=glacier", "/api/v1/search/deep?q=glacier"} {
					w := request(path, masked, false)
					requirements.Equal(http.StatusOK, w.Code, "%s: %s", path, w.Body.String())
					assertions.NotContains(w.Body.String(), "482913", path)
					assertions.NotContains(w.Body.String(), "abcdefghijklmnop", path)
					assertions.Contains(w.Body.String(), `"masked":true`, path)
					assertions.Contains(w.Body.String(), "provider-message", path)
					if mode == "dense" {
						assertions.Contains(w.Body.String(), "[verification content masked]", path)
					}
				}
				for _, path := range []string{fmt.Sprintf("/api/v1/messages/%d", id), fmt.Sprintf("/api/v1/cli/message?id=%d", id), "/api/v1/search?q=glacier"} {
					w := request(path, reveal, false)
					requirements.Equal(http.StatusOK, w.Code, w.Body.String())
					assertions.Contains(w.Body.String(), "482913")
					assertions.Contains(w.Body.String(), `"masked":false`)
					w = request(path, "", true)
					requirements.Equal(http.StatusOK, w.Code, w.Body.String())
					assertions.Contains(w.Body.String(), "482913")
					assertions.NotContains(w.Body.String(), `"verification"`)
				}
				w := request("/api/v1/search?q=glacier", searchOnly, false)
				requirements.Equal(http.StatusOK, w.Code, w.Body.String())
				assertions.NotContains(w.Body.String(), "482913")
				assertions.NotContains(w.Body.String(), "Long ordinary context")
				w = request(fmt.Sprintf("/api/v1/messages/%d", id), modifierOnly, false)
				assertions.Equal(http.StatusForbidden, w.Code)
				w = request(fmt.Sprintf("/api/v1/messages/%d", id), wrongSource, false)
				assertions.Equal(http.StatusNotFound, w.Code)
				w = request("/api/v1/aggregates", masked, false)
				assertions.Equal(http.StatusForbidden, w.Code)
				// Canonical identity and content remain unchanged in storage.
				canonical, err := query.NewEngine(st.DB(), st.IsPostgreSQL()).GetMessage(t.Context(), id)
				requirements.NoError(err)
				assertions.Equal(src.ID, canonical.SourceID)
				assertions.Equal(conv, canonical.ConversationID)
				assertions.Equal("provider-message", canonical.SourceMessageID)
				assertions.Equal(body, canonical.BodyText)
				assertions.Nil(canonical.Verification)
			})
		}
	}
}

func TestVerificationHTMLOnlyAttachmentRefusal(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "synthetic-source")
	requirements.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "synthetic-thread", "Synthetic")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "synthetic-message", MessageType: "email", Subject: sql.NullString{String: "482913", Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{}, sql.NullString{String: "<p>Your verification code is <b>482</b><span>913</span></p>", Valid: true}))
	hash := strings.Repeat("c", 64)
	data := []byte("Your verification code is 482913")
	requirements.NoError(st.UpsertAttachment(id, "482913.eml", "message/rfc822", "cc/"+hash, hash, len(data)))
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	attachments, err := engine.GetAttachmentsByHash(t.Context(), hash)
	requirements.NoError(err)
	requirements.Len(attachments, 1)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}
	path, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
	requirements.NoError(err)
	requirements.NoError(os.MkdirAll(filepath.Dir(path), 0700))
	requirements.NoError(os.WriteFile(path, data, 0600))
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	request := func(method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	for _, reveal := range []bool{false, true} {
		permissions := []agentgrant.Permission{agentgrant.PermissionAttachmentRead, agentgrant.PermissionMessageRead}
		if reveal {
			permissions = append(permissions, agentgrant.PermissionVerificationRead)
		}
		_, token, _, err := srv.agentGrants.Issue("synthetic reader", permissions, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
		requirements.NoError(err)
		for _, path := range []string{fmt.Sprintf("/api/v1/messages/%d", id), fmt.Sprintf("/api/v1/attachments/%d", attachments[0].ID)} {
			w := request(http.MethodGet, path, token)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			assertions.Equal(reveal, strings.Contains(w.Body.String(), "482913"), path)
			if path == fmt.Sprintf("/api/v1/messages/%d", id) && !reveal {
				var detail MessageDetail
				requirements.NoError(json.Unmarshal(w.Body.Bytes(), &detail))
				assertions.Equal("[verification content masked]", detail.BodyHTML)
				assertions.Equal("[verification content masked]", detail.Body)
			}
		}
		for _, path := range []string{"/api/v1/attachments/" + hash + "/content", "/api/v1/cli/attachment?content_hash=" + hash} {
			w := request(http.MethodGet, path, token)
			if reveal {
				assertions.Equal(http.StatusOK, w.Code, w.Body.String())
				assertions.Equal(data, w.Body.Bytes())
			} else {
				assertions.Equal(http.StatusForbidden, w.Code, w.Body.String())
				assertions.Contains(w.Body.String(), "verification_masked")
				assertions.NotContains(w.Body.String(), "482913")
			}
		}
		w := request(http.MethodHead, "/api/v1/attachments/"+hash+"/content", token)
		if reveal {
			assertions.Equal(http.StatusOK, w.Code, w.Body.String())
			assertions.Empty(w.Body.Bytes(), "permission checks must not transfer attachment bytes")
		} else {
			assertions.Equal(http.StatusForbidden, w.Code, w.Body.String())
			assertions.Contains(w.Body.String(), "verification_masked")
			assertions.NotEqual(data, w.Body.Bytes(), "permission checks must not transfer attachment bytes")
			assertions.NotContains(w.Body.String(), "482913")
		}
	}
}

func TestVerificationExpiredRevealGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)

		cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}
		srv := NewServerWithOptions(ServerOptions{Config: cfg, Logger: testLogger()})
		t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
		_, token, _, err := srv.agentGrants.Issue("expiring reader", []agentgrant.Permission{agentgrant.PermissionMessageRead, agentgrant.PermissionVerificationRead}, []agentgrant.SourceRef{{ID: 1, Type: "beeper", Identifier: "synthetic-source"}}, time.Now().Add(time.Hour))
		requirements.NoError(err)
		time.Sleep(2 * time.Hour)
		r := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1", nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assertions.Equal(http.StatusUnauthorized, w.Code)
	})
}

func TestVerificationBinaryAttachmentFilename(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "reader@example.test", "glacier", "Your verification code is AB4C9D")
	requirements.NoError(err)
	hash := strings.Repeat("d", 64)
	data := []byte{0, 1, 2, 3}
	requirements.NoError(st.UpsertAttachment(id, "AB4C9D.bin", "application/octet-stream", "dd/"+hash, hash, len(data)))
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}
	path, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
	requirements.NoError(err)
	requirements.NoError(os.MkdirAll(filepath.Dir(path), 0700))
	requirements.NoError(os.WriteFile(path, data, 0600))
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	for _, reveal := range []bool{false, true} {
		permissions := []agentgrant.Permission{agentgrant.PermissionAttachmentRead}
		if reveal {
			permissions = append(permissions, agentgrant.PermissionVerificationRead)
		}
		_, token, _, err := srv.agentGrants.Issue("synthetic reader", permissions, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
		requirements.NoError(err)
		r := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/"+hash+"/content", nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Equal(data, w.Body.Bytes())
		assertions.Equal(reveal, strings.Contains(w.Header().Get("Content-Disposition"), "AB4C9D"))
		assertions.NotEmpty(w.Header().Get("Content-Disposition"))
	}
}

func TestVerificationStoredCroppedSnippet(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "reader@example.test", "glacier", "Your verification code is AB4C9D")
	requirements.NoError(err)
	_, err = st.DB().Exec("UPDATE messages SET snippet = $1 WHERE id = $2", "AB4C...", id)
	requirements.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	_, token, _, err := srv.agentGrants.Issue("synthetic reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
	requirements.NoError(err)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
	r.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.NotContains(w.Body.String(), "AB4C")
	assertions.Contains(w.Body.String(), "[verification content masked]")
	assertions.Contains(w.Body.String(), `"masked":true`)
}

func TestVerificationDelegatedBodySearchPreservesVerifiedContexts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "synthetic-source")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		src.ID, "direct-chat", "direct_chat", "Synthetic direct chat",
	)
	requirements.NoError(err)
	body := "An ordinary message contains statusneedle for the body search."
	contextSnippet := body
	id, err := st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conversationID, SourceMessageID: "provider-message",
		MessageType: "email", Subject: sql.NullString{String: "Routine status", Valid: true},
		Snippet: sql.NullString{String: "Ordinary preview", Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(
		id, sql.NullString{String: body, Valid: true}, sql.NullString{},
	))
	requirements.NoError(st.UpsertFTS(id, "Routine status", body, "", "", ""))
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	cfg := &config.Config{
		HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()},
		Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg, Store: st, Engine: engine, Logger: testLogger(),
	})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	scope := []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}
	_, token, _, err := srv.agentGrants.Issue("synthetic reader", []agentgrant.Permission{
		agentgrant.PermissionMessageRead,
		agentgrant.PermissionSearchRead,
		agentgrant.PermissionVerificationRead,
	}, scope, time.Time{})
	requirements.NoError(err)

	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/search/deep?q=statusneedle&scope=body", nil)
	request.Header.Set(apiprotocol.AgentTokenHeader, token)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var got DeepSearchResponse
	requirements.NoError(json.NewDecoder(response.Body).Decode(&got))
	requirements.Len(got.BodyContexts, 1)
	assertions.Equal(id, got.BodyContexts[0].MessageID)
	requirements.Len(got.BodyContexts[0].ContextSnippets, 1)
	assertions.Equal(contextSnippet, got.BodyContexts[0].ContextSnippets[0])
	assertions.False(got.BodyContexts[0].ContextSnippetsTruncated)
}
