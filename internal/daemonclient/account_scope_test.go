package daemonclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

func TestAccountScopeDaemonTransport(t *testing.T) {
	source := int64(7)
	scopes := []search.AccountScope{{SourceID: &source, Groups: []string{"fastmail-masked:inbox@example.net"}}, {Addresses: []string{"work@example.org"}}}
	for _, version := range []string{"2.35.0", "2.36.0"} {
		t.Run(version, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": version})
					return
				}
				requests++
				assert.Equal("/api/v1/messages/filter", r.URL.Path)
				var got []search.AccountScope
				assert.NoError(json.Unmarshal([]byte(r.URL.Query().Get("account_scopes")), &got))
				assert.Equal(scopes, got)
				writeJSONResponse(t, w, map[string]any{"messages": []any{}, "count": 0, "offset": 0, "limit": 50, "has_more": false})
			}))
			defer server.Close()
			_, err := NewEngineAdapter(newTestStore(server, "")).ListMessages(t.Context(), query.MessageFilter{AccountScopes: scopes})
			if version == "2.35.0" {
				require.ErrorContains(err, "2.36.0")
				assert.Zero(requests)
			} else {
				require.NoError(err)
				assert.Equal(1, requests)
			}
		})
	}
}

func TestSourceAccountDaemonLookupWithoutVirtualTotals(t *testing.T) {
	for _, behavior := range []string{"metadata", "older-daemon", "failure"} {
		t.Run(behavior, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			catalogCalls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/cli/source-accounts":
					if behavior == "older-daemon" {
						http.NotFound(w, r)
						return
					}
					if behavior == "failure" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusServiceUnavailable)
						writeJSONResponse(t, w, map[string]any{"error": "store_unavailable", "message": "Database not available"})
						return
					}
					writeJSONResponse(t, w, map[string]any{"accounts": []any{map[string]any{"id": 7, "email": "inbox@example.net", "type": "imap"}}})
				case "/api/v1/cli/accounts":
					catalogCalls++
					writeJSONResponse(t, w, map[string]any{"accounts": []any{map[string]any{"id": 7, "email": "inbox@example.net", "type": "imap", "message_count": 9, "source_deleted_count": 0}}})
				default:
					assert.Fail("unexpected request", "%s", r.URL.Path)
				}
			}))
			defer server.Close()
			e := NewEngineAdapter(newTestStore(server, ""))
			lister, ok := any(e).(query.SourceAccountLister)
			require.True(ok, "daemon adapter must support the sources-only capability")
			accounts, err := lister.ListSourceAccounts(t.Context())
			if behavior == "failure" {
				require.Error(err)
				assert.Zero(catalogCalls)
				return
			}
			require.NoError(err)
			require.Len(accounts, 1)
			assert.Equal(int64(7), accounts[0].ID)
			assert.Equal("inbox@example.net", accounts[0].Identifier)
			assert.Equal("imap", accounts[0].SourceType)
			assert.Empty(accounts[0].VirtualAccounts)
			if behavior == "older-daemon" {
				assert.Equal(1, catalogCalls)
			} else {
				assert.Zero(catalogCalls)
			}
		})
	}
}

// The TLS server pins the versioned HTTP contract. It deliberately ignores the
// new account restriction like a pre-2.36 daemon; no live deletion is performed.
func TestAccountScopedDeletionSearchRequiresCapableDaemon(t *testing.T) {
	for _, version := range []string{"2.35.0", "2.36.0"} {
		for _, resolver := range []string{"search", "aggregate"} {
			for _, input := range []string{"filter", "operator", "transport"} {
				t.Run(version+"/"+resolver+"/"+input, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					scope := search.AccountScope{Addresses: []string{"work@example.org"}}
					filter := query.MessageFilter{}
					text := "invoice"
					switch input {
					case "filter":
						filter.AccountScopes = []search.AccountScope{scope}
					case "operator":
						text += " received:work@example.org"
					case "transport":
						source := int64(7)
						scope.SourceID = &source
						text += " " + strings.Join(search.FormatAccountScopes([]search.AccountScope{scope}), " ")
					}
					requests := 0
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/api/v1/health":
							writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": version})
						case "/api/v1/messages/gmail-ids":
							requests++
							if version == "2.36.0" && input == "filter" {
								var got []search.AccountScope
								assert.NoError(json.Unmarshal([]byte(r.URL.Query().Get("account_scopes")), &got))
								assert.Equal(filter.AccountScopes, got)
							}
							writeJSONResponse(t, w, map[string]any{
								"gmail_ids": []string{"gm-1"}, "search_query": r.URL.Query().Get("q"),
								"search_mode": r.URL.Query().Get("search_mode"),
								"targets":     []map[string]any{{"message_id": 1, "source_id": 7, "source_type": "gmail", "source_identifier": "inbox@example.net", "source_message_id": "gm-1"}},
							})
						default:
							http.NotFound(w, r)
						}
					}))
					defer server.Close()
					e := NewEngineAdapter(newTestStore(server, ""))
					var targets []query.DeletionTarget
					var err error
					if resolver == "search" {
						targets, err = e.GetDeletionTargetsBySearch(t.Context(), search.Parse(text), filter, query.DeletionSearchFast)
					} else {
						targets, err = e.GetDeletionTargetsByAggregateSearch(t.Context(), text, filter, query.ViewSenders, "sender@example.org")
					}
					if version == "2.35.0" {
						require.ErrorContains(err, "2.36.0")
						assert.Zero(requests, "unsupported account scopes must never reach a deletion resolver")
						assert.Empty(targets)
					} else {
						require.NoError(err)
						assert.Equal(1, requests)
						require.Len(targets, 1)
						assert.Equal("gm-1", targets[0].SourceMessageID)
					}
				})
			}
		}
	}
}

func TestDaemonVirtualAccountCatalogPreservesSelectorsAndCounts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/accounts", r.URL.Path)
		writeJSONResponse(t, w, map[string]any{"accounts": []map[string]any{{"id": 7, "email": "inbox@example.net", "type": "gmail", "display_name": "Inbox", "message_count": 12, "source_deleted_count": 3, "virtual_accounts": []map[string]any{
			{"key": "identity:7:d29ya0BleGFtcGxlLm9yZw", "source_id": 7, "account_address": "work@example.org", "message_count": 4, "source_deleted_count": 1, "pending_count": 2},
			{"key": "group:7:ZmFzdG1haWwtbWFza2VkOmluYm94QGV4YW1wbGUubmV0", "source_id": 7, "group": "fastmail-masked:inbox@example.net", "message_count": 6, "source_deleted_count": 2},
			{"key": "unattributed:7", "source_id": 7, "unattributed": true, "message_count": 2, "source_deleted_count": 0},
		}}}})
	}))
	defer server.Close()
	accounts, err := NewEngineAdapter(newTestStore(server, "")).ListAccounts(t.Context())
	require.NoError(err)
	require.Len(accounts, 1)
	require.Len(accounts[0].VirtualAccounts, 3)
	identity, group, unattributed := accounts[0].VirtualAccounts[0], accounts[0].VirtualAccounts[1], accounts[0].VirtualAccounts[2]
	assert.Equal("identity:7:d29ya0BleGFtcGxlLm9yZw", identity.Key)
	assert.Equal(int64(7), identity.SourceID)
	assert.Equal("work@example.org", identity.AccountAddress)
	assert.Equal(int64(4), identity.MessageCount)
	assert.Equal(int64(1), identity.SourceDeletedCount)
	assert.Equal(int64(2), identity.PendingCount)
	assert.Equal("fastmail-masked:inbox@example.net", group.Group)
	assert.Equal(int64(6), group.MessageCount)
	assert.True(unattributed.Unattributed)
	assert.Equal(int64(2), unattributed.MessageCount)
}

func TestSimilarDaemonAccountScopeTransport(t *testing.T) {
	source := int64(7)
	scopes := []search.AccountScope{{SourceID: &source, Addresses: []string{"work@example.org"}}}
	for _, version := range []string{"2.35.0", "2.36.0"} {
		t.Run(version, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": version})
					return
				}
				requests++
				assert.Equal("/api/v1/search/similar", r.URL.Path)
				var got []search.AccountScope
				assert.NoError(json.Unmarshal([]byte(r.URL.Query().Get("account_scopes")), &got))
				assert.Equal(scopes, got)
				writeJSONResponse(t, w, map[string]any{"seed_message_id": 11, "returned": 0, "generation": map[string]any{"id": 1, "model": "fake", "dimension": 4, "fingerprint": "fake:4", "state": "active"}, "messages": []any{}})
			}))
			defer server.Close()
			_, err := newTestStore(server, "").FindSimilarMessages(t.Context(), SimilarSearchRequest{MessageID: 11, AccountScopes: scopes})
			if version == "2.35.0" {
				require.ErrorContains(err, "2.36.0")
				assert.Zero(requests)
			} else {
				require.NoError(err)
				assert.Equal(1, requests)
			}
		})
	}
}
