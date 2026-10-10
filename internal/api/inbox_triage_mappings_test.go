package api

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"golang.org/x/oauth2"
)

type mappingHTTPStore struct {
	inboxCandidateHTTPStore

	client *gmail.Client
}

func (s *mappingHTTPStore) InboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity) (map[string]string, int64, error) {
	return s.archive.InboxTriageMappings(ctx, source)
}
func (s *mappingHTTPStore) UpdateInboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity, entries map[string]string, expected int64, p inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, gate func(context.Context) (func(), error)) (int64, error) {
	service := inboxcontrol.Service{Ledger: s.archive, Authorize: authorize, Resolve: func(_ context.Context, r inboxcontrol.Request) (inboxcontrol.Provider, error) {
		return gmail.NewInboxProvider(s.client, *r.Source), nil
	}, AcquireSource: func(ctx context.Context, id int64) (func(), error) {
		lease, err := s.archive.AcquireSyncExecutionContext(ctx, id)
		if err != nil {
			return nil, err
		}
		return func() { _ = lease.Release() }, nil
	}}
	return service.UpdateTriageMappings(ctx, source, entries, expected, p, gate)
}

type mappingNativeTransport struct{ base *url.URL }

func (tr mappingNativeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copied := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = tr.base.Scheme
	u.Host = tr.base.Host
	copied.URL = &u
	return http.DefaultTransport.RoundTrip(copied)
}

// Removing owner admission, source grants or CAS exposes or overwrites durable
// configuration. Real Gmail catalog reads and Store writes cover the boundary.
func TestInboxTriageMappingHTTPAuthorizationAndRevision(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	source := inboxcontrol.SourceIdentity{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier}
	var nativeWrites atomic.Int64
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			nativeWrites.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_, err := fmt.Fprintf(w, `{"emailAddress":%q}`, source.AccountID)
			assert.NoError(t, err)
		case "/gmail/v1/users/me/labels":
			_, err := fmt.Fprint(w, `{"labels":[{"id":"Label_1","name":"Todo","type":"user"},{"id":"INBOX","name":"Inbox","type":"system"}]}`)
			assert.NoError(t, err)
		default:
			http.NotFound(w, r)
		}
	}))
	defer native.Close()
	nativeURL, err := url.Parse(native.URL)
	requirements.NoError(err)
	client := gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-token"}), gmail.WithTransport(mappingNativeTransport{nativeURL}), gmail.WithRateLimiter(gmail.NewRateLimiter(10000)))
	mappingStore := &mappingHTTPStore{client: client}
	mappingStore.archive = f.Store
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, Store: mappingStore, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
	// This matrix exercises authorization, not the default per-IP burst limit.
	// Configure the real limiter before any request creates its token bucket.
	srv.rateLimiter.rate = 1000
	srv.rateLimiter.burst = 1000
	t.Cleanup(func() {
		srv.rateLimiter.Close()
		srv.changesRateLimiter.Close()
		srv.documentSearchRateLimiter.Close()
	})
	srv.agentGrants = agentgrant.NewRegistry()
	grantID, secret, _, err := srv.agentGrants.Issue("mapping-reader", []agentgrant.Permission{agentgrant.PermissionInboxRead}, []agentgrant.SourceRef{{ID: source.SourceID, Type: source.SourceType, Identifier: source.SourceIdentifier}})
	requirements.NoError(err)
	query := url.Values{"source_id": {strconv.FormatInt(source.SourceID, 10)}, "source_type": {source.SourceType}, "source_identifier": {source.SourceIdentifier}, "account_id": {source.AccountID}}
	call := func(method, body, owner, agent string) *httptest.ResponseRecorder {
		path := "/api/v1/inbox/triage/mappings"
		if method == http.MethodGet {
			path += "?" + query.Encode()
		}
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if owner != "" {
			r.Header.Set("Authorization", "Bearer "+owner)
		}
		if agent != "" {
			r.Header.Set(apiprotocol.AgentTokenHeader, agent)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	bodyFor := func(entries map[string]string, revision int64) string {
		encoded, err := json.Marshal(struct {
			Source           inboxcontrol.SourceIdentity `json:"source"`
			Entries          map[string]string           `json:"entries"`
			ExpectedRevision int64                       `json:"expected_revision"`
		}{source, entries, revision})
		require.NoError(t, err)
		return string(encoded)
	}
	entries := map[string]string{"todo": "Label_1"}
	body := bodyFor(entries, 0)
	response := call(http.MethodPut, body, "", secret)
	assertions.Equal(http.StatusUnauthorized, response.Code, response.Body.String())
	response = call(http.MethodPut, body, "", "")
	assertions.Equal(http.StatusUnauthorized, response.Code, response.Body.String())
	response = call(http.MethodPut, body, "synthetic-owner-key", "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	response = call(http.MethodGet, "", "", secret)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal("no-store", response.Header().Get("Cache-Control"))
	var stored struct {
		Source   inboxcontrol.SourceIdentity `json:"source"`
		Entries  map[string]string           `json:"entries"`
		Revision int64                       `json:"revision"`
	}
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &stored))
	assertions.Equal(source, stored.Source)
	assertions.Equal(entries, stored.Entries)
	assertions.Equal(int64(1), stored.Revision)
	response = call(http.MethodPut, body, "synthetic-owner-key", "")
	assertions.Equal(http.StatusConflict, response.Code, response.Body.String())
	for _, tag := range []string{"NewLabel", "INBOX"} {
		response = call(http.MethodPut, bodyFor(map[string]string{"todo": tag}, 1), "synthetic-owner-key", "")
		assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	}
	response = call(http.MethodPut, bodyFor(map[string]string{"autoarchive": "Label_1"}, 1), "synthetic-owner-key", "")
	assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	for _, bad := range []string{body + " {}", strings.TrimSuffix(body, "}") + `,"owner":true}`, strings.Replace(body, `"entries":{"todo":"Label_1"}`, `"entries":null`, 1), strings.Replace(body, `,"expected_revision":0`, "", 1)} {
		response = call(http.MethodPut, bad, "synthetic-owner-key", "")
		assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	}
	for _, field := range []string{"extra", "source_id"} {
		query.Add(field, "1")
		response = call(http.MethodGet, "", "synthetic-owner-key", "")
		assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
		query.Del(field)
		if field == "source_id" {
			query.Set(field, strconv.FormatInt(source.SourceID, 10))
		}
	}
	_, tagOnly, _, err := srv.agentGrants.Issue("tag-only", []agentgrant.Permission{agentgrant.PermissionInboxTag}, []agentgrant.SourceRef{{ID: source.SourceID, Type: source.SourceType, Identifier: source.SourceIdentifier}})
	requirements.NoError(err)
	response = call(http.MethodGet, "", "", tagOnly)
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())

	// A configured foreign source passes Store identity checks. Its denial
	// must come from this reader's exact source grant, not a malformed binding.
	foreignSource, err := f.Store.GetOrCreateSource("gmail", "foreign@example.test")
	requirements.NoError(err)
	query.Set("source_id", strconv.FormatInt(foreignSource.ID, 10))
	query.Set("source_identifier", foreignSource.Identifier)
	query.Set("account_id", foreignSource.Identifier)
	response = call(http.MethodGet, "", "synthetic-owner-key", "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	response = call(http.MethodGet, "", "", secret)
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	query.Set("source_id", strconv.FormatInt(source.SourceID, 10))
	query.Set("source_identifier", source.SourceIdentifier)
	query.Set("account_id", source.AccountID)
	query.Set("account_id", "foreign@example.test")
	response = call(http.MethodGet, "", "synthetic-owner-key", "")
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	query.Set("account_id", source.AccountID)
	query.Set("source_identifier", "foreign@example.test")
	response = call(http.MethodGet, "", "", secret)
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	query.Set("source_identifier", source.SourceIdentifier)
	response = call(http.MethodPut, bodyFor(map[string]string{}, 1), "synthetic-owner-key", "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	response = call(http.MethodGet, "", "", secret)
	requirements.Equal(http.StatusOK, response.Code)
	// JSON map decoding merges into an existing map; each response is fresh.
	stored.Entries = nil
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &stored))
	assertions.Empty(stored.Entries)
	assertions.Equal(int64(2), stored.Revision)
	for _, delegated := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
		if delegated {
			r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		} else {
			r.Header.Set("Authorization", "Bearer synthetic-owner-key")
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(http.StatusOK, w.Code)
		var descriptor apiprotocol.MCPCapabilities
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &descriptor))
		read, update := false, false
		for _, route := range descriptor.Routes {
			switch route.OperationID {
			case "getInboxTriageMappings":
				read = true
				assertions.Equal(http.MethodGet, route.Method)
				assertions.Equal("/api/v1/inbox/triage/mappings", route.Path)
			case "updateInboxTriageMappings":
				update = true
				assertions.Equal(http.MethodPut, route.Method)
				assertions.ElementsMatch([]string{"source", "entries", "expected_revision"}, route.RequestProperties)
			}
		}
		assertions.True(read)
		assertions.Equal(!delegated, update)
	}
	requirements.True(srv.agentGrants.Revoke(grantID))
	response = call(http.MethodGet, "", "", secret)
	assertions.Equal(http.StatusUnauthorized, response.Code)
	assertions.Zero(nativeWrites.Load(), "configuration cannot provision or mutate native state")
}
