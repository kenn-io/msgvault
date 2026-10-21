package daemonclient_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// The server fixture proves the native HTTP contract, including exact query
// binding and caller credentials; real candidate queries are tested by Store/API.
func TestInboxCandidateClientContract(t *testing.T) {
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test"}
	for _, mode := range []string{"success", "partial-read", "unknown-read", "hidden-unavailable", "chat-success", "chat-partial", "chat-unknown-marked-unread", "missing-discovery", "partial-discovery", "conflict", "foreign-page", "foreign-item", "unknown-inbox", "missing-unavailable", "null-unavailable", "redirect", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			source := source
			scope := inboxcontrol.ScopeMessage
			if mode == "chat-success" || mode == "chat-partial" || mode == "chat-unknown-marked-unread" {
				source.SourceType, source.SourceIdentifier, source.AccountID = "beeper", "synthetic-account", "synthetic-account"
				scope = inboxcontrol.ScopeChat
			}

			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertions.Equal(http.MethodGet, r.Method)
				assertions.Equal("synthetic-agent-token", r.Header.Get(apiprotocol.AgentTokenHeader))
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.15.0"}`))
					return
				case "/api/v1/mcp/capabilities":
					descriptor := apiprotocol.MCPCapabilities{Version: 1, Routes: []apiprotocol.MCPRouteDescriptor{}}
					if mode != "missing-discovery" {
						descriptor.Routes = append(descriptor.Routes, apiprotocol.MCPRouteDescriptor{OperationID: "listInboxCandidates", Method: "GET", Path: "/api/v1/inbox/candidates", QueryParameters: []string{"source_id", "source_type", "source_identifier", "account_id", "scope", "limit", "cursor"}})
					}
					if mode == "partial-discovery" {
						descriptor.Routes[0].QueryParameters = []string{"source_id"}
					}
					assertions.NoError(json.MarshalWrite(w, descriptor))
					return
				case "/api/v1/inbox/candidates":
					calls.Add(1)
					assertions.Equal("1", r.URL.Query().Get("source_id"))
					assertions.Equal(source.SourceType, r.URL.Query().Get("source_type"))
					assertions.Equal(source.SourceIdentifier, r.URL.Query().Get("source_identifier"))
					assertions.Equal(source.AccountID, r.URL.Query().Get("account_id"))
					assertions.Equal(string(scope), r.URL.Query().Get("scope"))
					assertions.Equal("2", r.URL.Query().Get("limit"))
					assertions.Equal("opaque-cursor", r.URL.Query().Get("cursor"))
					if mode == "redirect" {
						http.Redirect(w, r, "/unexpected-redirect", http.StatusTemporaryRedirect)
						return
					}
					if mode == "conflict" {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"error":"inbox_conflict","message":"untrusted detail"}`))
						return
					}
					page := inboxcontrol.CandidatePage{Source: source, Scope: scope, ArchiveRevision: "committed-revision", Candidates: []inboxcontrol.Candidate{{State: inboxcontrol.State{Target: inboxcontrol.Target{SourceID: source.SourceID, SourceType: source.SourceType, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: scope, ItemID: 7, ProviderID: "synthetic-item"}, Inbox: new(true), Read: new(false)}, Available: true}}, Unavailable: true}
					switch mode {
					case "chat-success":
						page.Candidates[0].State.MarkedUnread = new(false)
					case "chat-partial":
						page.Candidates[0].Available = false
					}
					if mode == "foreign-page" {
						page.Source.AccountID = "foreign@example.test"
					}
					if mode == "foreign-item" {
						page.Candidates[0].State.Target.AccountID = "foreign@example.test"
					}
					if mode == "unknown-inbox" {
						page.Candidates[0].State.Inbox = nil
					}
					if mode == "partial-read" || mode == "unknown-read" || mode == "hidden-unavailable" {
						page.Candidates[0].State.Read = nil
						page.Candidates[0].Available = mode == "unknown-read"
						page.Unavailable = mode != "hidden-unavailable"
					}
					if mode == "missing-unavailable" || mode == "null-unavailable" {
						data, err := json.Marshal(page)
						if !assertions.NoError(err) {
							return
						}
						var fields map[string]any
						if !assertions.NoError(json.Unmarshal(data, &fields)) {
							return
						}
						if mode == "missing-unavailable" {
							delete(fields, "unavailable")
						} else {
							fields["unavailable"] = nil
						}
						assertions.NoError(json.MarshalWrite(w, fields))
						return
					}
					if mode == "oversized" {
						data, err := json.Marshal(page)
						if !assertions.NoError(err) {
							return
						}
						if padding := (8 << 20) + 1 - len(data); padding > 0 {
							data = append(data, bytes.Repeat([]byte(" "), padding)...)
						}
						w.Header().Set("Content-Type", "application/json")
						_, err = w.Write(data)
						assertions.NoError(err)
						return
					}
					assertions.NoError(json.MarshalWrite(w, page))
					return
				default:
					assertions.Fail("unexpected native path")
				}
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, AgentToken: "synthetic-agent-token", HTTPClient: server.Client()})
			requirements.NoError(err)
			defer func() { assertions.NoError(client.Close()) }()
			page, err := client.InboxCandidates(t.Context(), source, scope, 2, "opaque-cursor")
			switch mode {
			case "success", "partial-read", "chat-success", "chat-partial":
				requirements.NoError(err)
				requirements.NotNil(page)
				requirements.Len(page.Candidates, 1)
				assertions.Equal(source, page.Source)
				assertions.Equal(scope, page.Scope)
				assertions.True(page.Unavailable)
				assertions.Equal("committed-revision", page.ArchiveRevision)
				if mode == "partial-read" {
					assertions.Nil(page.Candidates[0].State.Read)
					assertions.False(page.Candidates[0].Available)
				}
				if mode == "chat-partial" {
					assertions.Nil(page.Candidates[0].State.MarkedUnread)
					assertions.False(page.Candidates[0].Available)
				}
			case "conflict":
				requirements.ErrorIs(err, inboxcontrol.ErrConflict)
				assertions.NotContains(err.Error(), "untrusted detail")
			default:
				requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
				assertions.Nil(page)
			}
			want := int64(1)
			if mode == "missing-discovery" || mode == "partial-discovery" {
				want = 0
			}
			assertions.Equal(want, calls.Load())
		})
	}
}

func TestInboxCandidateClientRejectsInvalidBeforeHTTP(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	defer func() { assertions.NoError(client.Close()) }()
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test"}
	for _, limit := range []int{-1, 0, 101} {
		_, err := client.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, limit, "")
		requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
	}
	_, err = client.InboxCandidates(t.Context(), source, inboxcontrol.ScopeChat, 1, "")
	requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
	source.SourceID = 0
	_, err = client.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, "")
	requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
	assertions.Equal(int64(0), calls.Load())
}
