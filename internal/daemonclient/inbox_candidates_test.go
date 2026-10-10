package daemonclient_test

import (
	"context"
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

type inboxCandidateClient interface {
	InboxCandidates(ctx context.Context, source inboxcontrol.SourceIdentity, scope inboxcontrol.Scope, limit int, cursor string) (*inboxcontrol.CandidatePage, error)
}

// The server fixture proves the native HTTP contract, including exact query
// binding and caller credentials; real candidate queries are tested by Store/API.
func TestInboxCandidateClientContract(t *testing.T) {
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test"}
	for _, mode := range []string{"success", "missing-discovery", "partial-discovery", "conflict", "foreign-page", "foreign-item", "unknown-inbox", "redirect", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "synthetic-agent-token", r.Header.Get(apiprotocol.AgentTokenHeader))
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.3.0"}`))
					return
				case "/api/v1/mcp/capabilities":
					descriptor := apiprotocol.MCPCapabilities{Version: 1, Routes: []apiprotocol.MCPRouteDescriptor{}}
					if mode != "missing-discovery" {
						descriptor.Routes = append(descriptor.Routes, apiprotocol.MCPRouteDescriptor{OperationID: "listInboxCandidates", Method: "GET", Path: "/api/v1/inbox/candidates", QueryParameters: []string{"source_id", "source_type", "source_identifier", "account_id", "scope", "limit", "cursor"}})
					}
					if mode == "partial-discovery" {
						descriptor.Routes[0].QueryParameters = []string{"source_id"}
					}
					assert.NoError(t, json.MarshalWrite(w, descriptor))
					return
				case "/api/v1/inbox/candidates":
					calls.Add(1)
					assert.Equal(t, "1", r.URL.Query().Get("source_id"))
					assert.Equal(t, "gmail", r.URL.Query().Get("source_type"))
					assert.Equal(t, source.SourceIdentifier, r.URL.Query().Get("source_identifier"))
					assert.Equal(t, source.AccountID, r.URL.Query().Get("account_id"))
					assert.Equal(t, "message", r.URL.Query().Get("scope"))
					assert.Equal(t, "2", r.URL.Query().Get("limit"))
					assert.Equal(t, "opaque-cursor", r.URL.Query().Get("cursor"))
					if mode == "redirect" {
						http.Redirect(w, r, "/unexpected-redirect", http.StatusTemporaryRedirect)
						return
					}
					if mode == "conflict" {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"error":"inbox_conflict","message":"untrusted detail"}`))
						return
					}
					if mode == "oversized" {
						_, _ = w.Write(make([]byte, (8<<20)+1))
						return
					}
					page := inboxcontrol.CandidatePage{Source: source, Scope: inboxcontrol.ScopeMessage, ArchiveRevision: "committed-revision", Candidates: []inboxcontrol.Candidate{{State: inboxcontrol.State{Target: inboxcontrol.Target{SourceID: source.SourceID, SourceType: source.SourceType, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: 7, ProviderID: "synthetic-mail"}, Inbox: new(true), Read: new(false)}, Available: true}}, Unavailable: true}
					if mode == "foreign-page" {
						page.Source.AccountID = "foreign@example.test"
					}
					if mode == "foreign-item" {
						page.Candidates[0].State.Target.AccountID = "foreign@example.test"
					}
					if mode == "unknown-inbox" {
						page.Candidates[0].State.Inbox = nil
					}
					assert.NoError(t, json.MarshalWrite(w, page))
					return
				default:
					assert.Fail(t, "unexpected native path")
				}
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, AgentToken: "synthetic-agent-token", HTTPClient: server.Client()})
			requirements.NoError(err)
			defer func() { assert.NoError(t, client.Close()) }()
			reader, ok := any(client).(inboxCandidateClient)
			requirements.True(ok, "daemon client must expose candidate listing")
			page, err := reader.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 2, "opaque-cursor")
			switch mode {
			case "success":
				requirements.NoError(err)
				requirements.NotNil(page)
				assertions.Equal(source, page.Source)
				assertions.True(page.Unavailable)
				assertions.Equal("committed-revision", page.ArchiveRevision)
			case "conflict":
				require.ErrorIs(t, err, inboxcontrol.ErrConflict)
				assertions.NotContains(err.Error(), "untrusted detail")
			default:
				require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
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
	defer func() { assert.NoError(t, client.Close()) }()
	reader, ok := any(client).(inboxCandidateClient)
	requirements.True(ok)
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test"}
	for _, limit := range []int{-1, 0, 101} {
		_, err := reader.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, limit, "")
		require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	}
	_, err = reader.InboxCandidates(t.Context(), source, inboxcontrol.ScopeChat, 1, "")
	require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	source.SourceID = 0
	_, err = reader.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, "")
	require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	assertions.Equal(int64(0), calls.Load())
}
