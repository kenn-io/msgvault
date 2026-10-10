package api

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

type triageHTTPStore struct {
	inboxCandidateHTTPStore

	client *gmail.Client
	key    []byte
}

func (s *triageHTTPStore) service(authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error) *inboxcontrol.Service {
	return &inboxcontrol.Service{Ledger: s.archive, Key: s.key, Authorize: authorize,
		Resolve: func(ctx context.Context, r inboxcontrol.Request) (inboxcontrol.Provider, error) {
			var source inboxcontrol.SourceIdentity
			if r.Target != nil {
				if err := s.archive.ValidateInboxTargetContext(ctx, *r.Target); err != nil {
					return nil, err
				}
				source = inboxcontrol.SourceIdentity{SourceID: r.Target.SourceID, SourceType: r.Target.SourceType, SourceIdentifier: r.Target.SourceIdentifier, AccountID: r.Target.AccountID}
			} else {
				source = *r.Source
			}
			return gmail.NewInboxProvider(s.client, source), nil
		},
		AcquireSource: func(ctx context.Context, id int64) (func(), error) {
			lease, err := s.archive.AcquireSyncExecutionContext(ctx, id)
			if err != nil {
				return nil, err
			}
			return func() { _ = lease.Release() }, nil
		},
		ReconcileState: func(ctx context.Context, before, after inboxcontrol.State) error {
			return s.archive.ReconcileInboxProviderState(ctx, before.Target, before, after)
		},
	}
}
func (s *triageHTTPStore) PreviewInboxTriage(ctx context.Context, input inboxcontrol.TriageInput, p inboxcontrol.Principal, auth func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error) (*inboxcontrol.TriageProposal, error) {
	return s.service(auth).PreviewTriage(ctx, input, p)
}
func (s *triageHTTPStore) ApplyInboxTriage(ctx context.Context, proposal inboxcontrol.TriageProposal, p inboxcontrol.Principal, auth func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, gate func(context.Context) (func(), error)) ([]inboxcontrol.Result, error) {
	return s.service(auth).ApplyTriage(ctx, proposal, p, gate)
}

type triageHTTPFixture struct {
	archive *storetest.Fixture
	srv     *Server
	input   inboxcontrol.TriageInput
	source  inboxcontrol.SourceIdentity
	writes  atomic.Int64
	mu      sync.Mutex
	labels  map[string][]string
	onRead  func()
	onWrite func()
	lost    bool
}

func newTriageHTTPFixture(t *testing.T, count int) *triageHTTPFixture {
	t.Helper()
	a := storetest.New(t)
	f := &triageHTTPFixture{archive: a, labels: map[string][]string{}}
	f.source = inboxcontrol.SourceIdentity{SourceID: a.Source.ID, SourceType: "gmail", SourceIdentifier: a.Source.Identifier, AccountID: a.Source.Identifier}
	f.input.Source = f.source
	a.EnsureLabels(map[string]string{"TodoLabel": "Todo", "Unrelated": "Unrelated"}, "user")
	_, err := a.Store.ReplaceInboxTriageMappings(t.Context(), f.source, map[string]string{"todo": "TodoLabel"}, 0, inboxcontrol.Principal{ID: "owner", Owner: true})
	require.NoError(t, err)
	for i := range count {
		id := fmt.Sprintf("triage-%d", i)
		target := inboxcontrol.Target{SourceID: f.source.SourceID, SourceType: "gmail", SourceIdentifier: f.source.SourceIdentifier, AccountID: f.source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: a.CreateMessage(id), ProviderID: id}
		f.labels[id] = []string{"INBOX", "UNREAD", "Unrelated"}
		_, err := a.Store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), Tags: slices.Clone(f.labels[id]), Revision: "7", ObservedAt: time.Now().UTC()})
		require.NoError(t, err)
		f.input.Items = append(f.input.Items, inboxcontrol.TriageItemInput{Target: target, Categories: []string{"todo"}, EvidenceMessageIDs: []int64{target.ItemID}, IdempotencyKey: fmt.Sprintf("triage-key-%d", i)})
	}
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/profile":
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"emailAddress": f.source.AccountID}))
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/labels":
			_, err := fmt.Fprint(w, `{"labels":[{"id":"TodoLabel","name":"Todo","type":"user"}]}`)
			assert.NoError(t, err)
		case strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/messages/"):
			id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
			if r.Method == http.MethodPost && strings.HasSuffix(id, "/modify") {
				id = strings.TrimSuffix(id, "/modify")
				f.writes.Add(1)
				var change struct {
					Add    []string `json:"addLabelIds"`
					Remove []string `json:"removeLabelIds"`
				}
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &change)) {
					return
				}
				assert.Equal(t, []string{"TodoLabel"}, change.Add)
				assert.Empty(t, change.Remove)
				f.labels[id] = append(f.labels[id], change.Add...)
				if f.onWrite != nil {
					f.onWrite()
				}
				if f.lost {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			} else {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "metadata", r.URL.Query().Get("format"))
				if f.onRead != nil {
					f.onRead()
				}
			}
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"id": id, "labelIds": f.labels[id], "historyId": "7"}))
		default:
			assert.Fail(t, "unexpected native request", r.Method+" "+r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(native.Close)
	endpoint, err := url.Parse(native.URL)
	require.NoError(t, err)
	client := gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-token"}), gmail.WithTransport(mappingNativeTransport{endpoint}), gmail.WithRateLimiter(gmail.NewRateLimiter(10000)))
	triageStore := &triageHTTPStore{client: client, key: []byte(strings.Repeat("k", 32))}
	triageStore.archive = a.Store
	f.srv = NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, Store: triageStore, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
	f.srv.agentGrants = agentgrant.NewRegistry()
	f.srv.rateLimiter.rate = 1000
	f.srv.rateLimiter.burst = 1000
	t.Cleanup(func() {
		f.srv.rateLimiter.Close()
		f.srv.changesRateLimiter.Close()
		f.srv.documentSearchRateLimiter.Close()
	})
	return f
}
func (f *triageHTTPFixture) call(t *testing.T, path string, body any, owner, delegate string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(data)))
	r.Header.Set("Content-Type", "application/json")
	if owner != "" {
		r.Header.Set("Authorization", "Bearer "+owner)
	}
	if delegate != "" {
		r.Header.Set(apiprotocol.AgentTokenHeader, delegate)
	}
	response := httptest.NewRecorder()
	f.srv.Router().ServeHTTP(response, r)
	return response
}
func (f *triageHTTPFixture) grant(t *testing.T, permissions []agentgrant.Permission, sources []agentgrant.SourceRef) (string, string) {
	t.Helper()
	id, secret, _, err := f.srv.agentGrants.Issue("triage-fixture", permissions, sources)
	require.NoError(t, err)
	return id, secret
}

// Missing routes, write-gate middleware around readonly preview, scope expansion,
// direct writes without durable receipts, or automatic retry break native behavior.
func TestInboxTriageHTTPPreviewApplyAndReceiptReplay(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newTriageHTTPFixture(t, 2)
	_, secret := f.grant(t, []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxTag}, []agentgrant.SourceRef{{ID: f.source.SourceID, Type: "gmail", Identifier: f.source.SourceIdentifier}})
	// Preview must remain available while another operation owns the daemon gate.
	release, ok := f.srv.operationGate.BeginWork()
	requirements.True(ok)
	response := f.call(t, "/api/v1/inbox/triage/preview", f.input, "", secret)
	release()
	requirements.Equal(http.StatusOK, response.Code)
	assertions.Equal("no-store", response.Header().Get("Cache-Control"))
	var proposal inboxcontrol.TriageProposal
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &proposal))
	assertions.Zero(f.writes.Load())
	requirements.Len(proposal.Items, 2)
	assertions.Empty(proposal.Items[0].Request.PreviewToken)
	response = f.call(t, "/api/v1/inbox/triage/apply", proposal, "", secret)
	requirements.Equal(http.StatusOK, response.Code)
	var results []inboxcontrol.Result
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &results))
	requirements.Len(results, 2)
	for _, result := range results {
		requirements.NotNil(result.Receipt)
		assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
		assertions.True(*result.After.Inbox)
		assertions.False(*result.After.Read)
		assertions.Contains(result.After.Tags, "Unrelated")
	}
	assertions.Equal(int64(2), f.writes.Load())
	response = f.call(t, "/api/v1/inbox/triage/apply", proposal, "", secret)
	requirements.Equal(http.StatusOK, response.Code)
	var again []inboxcontrol.Result
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &again))
	requirements.Len(again, 2)
	for i := range results {
		assertions.Equal(results[i].Receipt.ID, again[i].Receipt.ID)
	}
	assertions.Equal(int64(2), f.writes.Load())
}

func TestInboxTriageHTTPUsesExactCurrentSourceAndActionGrants(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newTriageHTTPFixture(t, 1)
	sources := []agentgrant.SourceRef{{ID: f.source.SourceID, Type: "gmail", Identifier: f.source.SourceIdentifier}}
	_, reader := f.grant(t, []agentgrant.Permission{agentgrant.PermissionInboxRead}, sources)
	_, tagOnly := f.grant(t, []agentgrant.Permission{agentgrant.PermissionInboxTag}, sources)
	foreignSource, err := f.archive.Store.GetOrCreateSource("gmail", "other@example.test")
	requirements.NoError(err)
	_, foreign := f.grant(t, []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxTag}, []agentgrant.SourceRef{{ID: foreignSource.ID, Type: "gmail", Identifier: foreignSource.Identifier}})
	id, writer := f.grant(t, []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxTag}, sources)
	for _, tc := range []struct {
		name, owner, delegate string
		want                  int
	}{
		{"unauthenticated", "", "", 401}, {"reader", "", reader, 403}, {"tag without read", "", tagOnly, 403}, {"foreign source", "", foreign, 403}, {"invalid token", "", "invalid-agent-token", 401}, {"owner", "synthetic-owner-key", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := f.call(t, "/api/v1/inbox/triage/preview", f.input, tc.owner, tc.delegate)
			assert.Equal(t, tc.want, response.Code)
		})
	}
	response := f.call(t, "/api/v1/inbox/triage/preview", f.input, "", writer)
	requirements.Equal(200, response.Code)
	var proposal inboxcontrol.TriageProposal
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &proposal))
	// Caller binding is enforced even for a second equally scoped delegate.
	_, other := f.grant(t, []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxTag}, sources)
	response = f.call(t, "/api/v1/inbox/triage/apply", proposal, "", other)
	assertions.Equal(400, response.Code)
	// Revoke after live observation; stale admission metadata cannot dispatch.
	f.onRead = func() { f.srv.agentGrants.Revoke(id) }
	response = f.call(t, "/api/v1/inbox/triage/apply", proposal, "", writer)
	assertions.Equal(403, response.Code)
	assertions.Zero(f.writes.Load())
}

func TestInboxTriageHTTPPreservesPartialAndUnknownReceipts(t *testing.T) {
	for _, mode := range []string{"arrival", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f := newTriageHTTPFixture(t, 2)
			response := f.call(t, "/api/v1/inbox/triage/preview", f.input, "synthetic-owner-key", "")
			requirements.Equal(200, response.Code)
			var proposal inboxcontrol.TriageProposal
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &proposal))
			status, code := 409, "inbox_conflict"
			if mode == "arrival" {
				f.onWrite = func() { f.archive.CreateMessage("new-arrival") }
			} else {
				f.lost = true
				status, code = 502, "inbox_outcome_unknown"
			}
			response = f.call(t, "/api/v1/inbox/triage/apply", proposal, "synthetic-owner-key", "")
			requirements.Equal(status, response.Code)
			var failure struct {
				Error   string                `json:"error"`
				Results []inboxcontrol.Result `json:"results"`
			}
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &failure))
			assertions.Equal(code, failure.Error)
			requirements.Len(failure.Results, 2)
			requirements.NotNil(failure.Results[0].Receipt)
			assertions.Nil(failure.Results[1].Receipt)
			assertions.Equal(int64(1), f.writes.Load())
			response = f.call(t, "/api/v1/inbox/triage/apply", proposal, "synthetic-owner-key", "")
			requirements.Equal(status, response.Code)
			var again struct {
				Results []inboxcontrol.Result `json:"results"`
			}
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &again))
			requirements.Len(again.Results, 2)
			requirements.NotNil(again.Results[0].Receipt)
			assertions.Equal(failure.Results[0].Receipt.ID, again.Results[0].Receipt.ID)
			assertions.Equal(int64(1), f.writes.Load())
		})
	}
}

func TestInboxTriageHTTPRejectsMalformedOrUnboundedInput(t *testing.T) {
	f := newTriageHTTPFixture(t, 1)
	good, err := json.Marshal(f.input)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, path, body, content string
		want                      int
	}{
		{"empty", "preview", `{}`, "application/json", 400},
		{"null", "preview", `{"source":null,"items":[]}`, "application/json", 400},
		{"extra authority", "preview", strings.TrimSuffix(string(good), "}") + `,"owner":true}`, "application/json", 400},
		{"duplicate member", "preview", strings.TrimSuffix(string(good), "}") + `,"items":[]}`, "application/json", 400},
		{"trailing value", "preview", string(good) + `{}`, "application/json", 400},
		{"wrong media", "preview", string(good), "text/plain", 415},
		{"oversize", "preview", strings.Repeat(" ", (1<<20)+1), "application/json", 413},
		{"unsigned apply", "apply", `{}`, "application/json", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/inbox/triage/"+tc.path, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer synthetic-owner-key")
			r.Header.Set("Content-Type", tc.content)
			response := httptest.NewRecorder()
			f.srv.Router().ServeHTTP(response, r)
			assert.Equal(t, tc.want, response.Code)
		})
	}
	for _, count := range []int{0, 101} {
		input := f.input
		input.Items = make([]inboxcontrol.TriageItemInput, count)
		response := f.call(t, "/api/v1/inbox/triage/preview", input, "synthetic-owner-key", "")
		assert.Equal(t, 400, response.Code)
	}
	assert.Zero(t, f.writes.Load())
}

func TestInboxTriageDiscoveryAdmitsOnlyImplementedScopedTagRoutes(t *testing.T) {
	f := newTriageHTTPFixture(t, 1)
	sources := []agentgrant.SourceRef{{ID: f.source.SourceID, Type: "gmail", Identifier: f.source.SourceIdentifier}}
	_, reader := f.grant(t, []agentgrant.Permission{agentgrant.PermissionInboxRead}, sources)
	_, writer := f.grant(t, []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxTag}, sources)
	chatSource, err := f.archive.Store.GetOrCreateSource("beeper", "synthetic-account")
	require.NoError(t, err)
	_, chatWriter := f.grant(t, []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxTag}, []agentgrant.SourceRef{{ID: chatSource.ID, Type: "beeper", Identifier: chatSource.Identifier}})
	for _, tc := range []struct {
		name, owner, delegate string
		want                  bool
	}{{"owner", "synthetic-owner-key", "", true}, {"writer", "", writer, true}, {"reader", "", reader, false}, {"chat unsupported", "", chatWriter, false}, {"backend missing", "synthetic-owner-key", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			original := f.srv.store
			if tc.name == "backend missing" {
				f.srv.store = &mockStore{}
			}
			defer func() { f.srv.store = original }()
			r := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
			if tc.owner != "" {
				r.Header.Set("Authorization", "Bearer "+tc.owner)
			}
			if tc.delegate != "" {
				r.Header.Set(apiprotocol.AgentTokenHeader, tc.delegate)
			}
			response := httptest.NewRecorder()
			f.srv.Router().ServeHTTP(response, r)
			requirements.Equal(200, response.Code)
			var capabilities apiprotocol.MCPCapabilities
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &capabilities))
			preview, apply := false, false
			for _, route := range capabilities.Routes {
				if route.OperationID == "previewInboxTriage" {
					preview = true
					assertions.Equal("/api/v1/inbox/triage/preview", route.Path)
					assertions.Contains(route.RequestProperties, "items")
				}
				if route.OperationID == "applyInboxTriage" {
					apply = true
					assertions.Equal("/api/v1/inbox/triage/apply", route.Path)
					assertions.Contains(route.RequestProperties, "preview_token")
				}
			}
			assertions.Equal(tc.want, preview)
			assertions.Equal(tc.want, apply)
		})
	}
	assert.Zero(t, f.writes.Load())
}
