package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func inboxGmailFixture(t *testing.T) (*storeAPIAdapter, inboxcontrol.Target) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(source.ID, "thread-fixture", "Fixture")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "mail-fixture", ConversationID: conv, MessageType: "email"})
	require.NoError(t, err)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: "mail-fixture"}
	return &storeAPIAdapter{store: st, config: &config.Config{}, inboxKey: []byte(strings.Repeat("k", 32))}, target
}

func TestDaemonInboxResolutionRejectsInjectedBindingsBeforeProviderAccess(t *testing.T) {
	for _, field := range []string{"source", "identifier", "account", "item", "provider"} {
		t.Run(field, func(t *testing.T) {
			a, target := inboxGmailFixture(t)
			calls := 0
			a.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
				calls++
				return nil, errors.New("must not construct a provider")
			}
			switch field {
			case "source":
				target.SourceID++
			case "identifier":
				target.SourceIdentifier = "different@example.test"
			case "account":
				target.AccountID = "different@example.test"
			case "item":
				target.ItemID += 10000
			case "provider":
				target.ProviderID = "different-provider-id"
			}
			_, err := a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}, inboxcontrol.Principal{ID: "owner", Owner: true}, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, nil)
			require.ErrorIs(t, err, inboxcontrol.ErrDenied)
			assert.Zero(t, calls)
		})
	}
}

func TestDaemonInboxMissingKeyNeverProvisionedByRequest(t *testing.T) {
	a, target := inboxGmailFixture(t)
	a.inboxKey = nil
	calls := 0
	a.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
		calls++
		return nil, errors.New("must not construct a provider")
	}
	_, err := a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}, inboxcontrol.Principal{ID: "owner", Owner: true}, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, nil)
	require.ErrorIs(t, err, inboxcontrol.ErrInternal)
	assert.Zero(t, calls)
}

func TestDaemonInboxStartupProvisioningAndRecovery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	a, target := inboxGmailFixture(t)
	cfg := &config.Config{HomeDir: t.TempDir()}
	first, err := prepareInboxRuntime(t.Context(), cfg, a.store)
	require.NoError(err)
	before := inboxcontrol.State{Target: target, Inbox: new(true), ObservedAt: time.Now().UTC()}
	intent := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}
	intentHash, err := inboxcontrol.IntentFingerprint(intent)
	require.NoError(err)
	stateHash, err := inboxcontrol.SemanticFingerprint(before)
	require.NoError(err)
	receipt := inboxcontrol.Receipt{ID: "startup-receipt", PrincipalID: "owner", SourceID: target.SourceID, IdempotencyKey: "startup-operation", IntentHash: intentHash, StateHash: stateHash, Intent: intent, Before: before, Status: inboxcontrol.StatusPrepared, CreatedAt: time.Now().UTC()}
	_, claimed, err := a.store.PrepareInboxReceipt(t.Context(), receipt)
	require.NoError(err)
	require.True(claimed)
	require.NoError(a.store.MarkInboxDispatching(t.Context(), receipt.ID))
	second, err := prepareInboxRuntime(t.Context(), cfg, a.store)
	require.NoError(err)
	assert.Equal(first, second)
	require.Len(second, 32)
	got, err := a.store.GetInboxReceipt(t.Context(), receipt.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(inboxcontrol.StatusUnknown, got.Status)
}

func TestDaemonInboxGmailNativeSignedExecution(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	a, target := inboxGmailFixture(t)
	a.config = &config.Config{OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}
	labels := []string{"INBOX", "UNREAD", "STARRED"}
	native, err := a.store.EmailTagTargetContext(t.Context(), target.ItemID, "")
	requirements.NoError(err)
	requirements.NoError(a.store.SaveEmailTagsContext(t.Context(), native, &emailtags.MessageTagResult{Provider: "gmail", Tags: labels, Verified: true}))
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": target.AccountID})
		case "/gmail/v1/users/me/labels":
			_, _ = w.Write([]byte(`{"labels":[]}`))
		case "/gmail/v1/users/me/messages/mail-fixture":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": target.ProviderID, "labelIds": labels, "historyId": "12"})
		case "/gmail/v1/users/me/messages/mail-fixture/modify":
			posts++
			var delta struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&delta)) {
				return
			}
			assert.Empty(t, delta.Add)
			assert.Equal(t, []string{"INBOX"}, delta.Remove)
			labels = slices.DeleteFunc(labels, func(id string) bool { return id == "INBOX" })
			_ = json.NewEncoder(w).Encode(map[string]any{"id": target.ProviderID})
		default:
			assert.Fail(t, "unexpected native Gmail request", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	requirements.NoError(err)
	transport := tagGmailTransport(func(r *http.Request) (*http.Response, error) {
		clonedRequest := r.Clone(r.Context())
		u := *r.URL
		clonedRequest.URL = &u
		clonedRequest.URL.Scheme = endpoint.Scheme
		clonedRequest.URL.Host = endpoint.Host
		return http.DefaultTransport.RoundTrip(clonedRequest)
	})
	a.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture-token"}), gmail.WithTransport(transport), gmail.WithRateLimiter(gmail.NewRateLimiter(1000))), nil
	}
	principal := inboxcontrol.Principal{ID: "owner", Owner: true}
	authorize := func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }
	gates, releases := 0, 0
	gate := func(context.Context) (func(), error) { gates++; return func() { releases++ }, nil }
	request := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}
	preview, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
	requirements.NoError(err)
	requirements.NotNil(preview.Before)
	requirements.NotNil(preview.Projected)
	assertions.True(*preview.Before.Inbox)
	assertions.False(*preview.Projected.Inbox)
	assertions.Equal(0, posts)
	assertions.Equal(0, gates)
	request.DryRun = false
	request.Expected = preview.Before
	request.PreviewToken = preview.PreviewToken
	request.IdempotencyKey = "native-archive"
	result, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
	requirements.NoError(err)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	requirements.NotNil(result.After)
	assertions.False(*result.After.Inbox)
	assertions.False(*result.After.Read)
	assertions.Contains(result.After.Tags, "STARRED")
	local, err := a.store.GetMessage(target.ItemID)
	requirements.NoError(err)
	assertions.ElementsMatch([]string{"UNREAD", "STARRED"}, local.Labels)
	assertions.Equal(1, posts)
	assertions.Equal(1, gates)
	assertions.Equal(1, releases)
	observation, err := a.store.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(observation)
	assertions.False(*observation.Inbox)
	replay, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	assertions.Equal(1, posts)

	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	capabilities, err := a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source}, principal, authorize, gate)
	requirements.NoError(err)
	requirements.NotNil(capabilities.Capabilities)
	for _, capability := range capabilities.Capabilities.Operations {
		assertions.Equal(inboxcontrol.CapabilitySupported, capability.Status)
	}
	assertions.Equal(1, posts)
	assertions.Equal(1, gates)
}

func TestDaemonInboxCapabilitiesAllowReadonlyGmailCredential(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	tokenPath, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
	defer restore()
	home := filepath.Dir(filepath.Dir(tokenPath))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir, cfg.OAuth.ClientSecrets = home, home, filepath.Join(home, "client_secret.json")
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", scopeEscalationAccount)
	requirements.NoError(err)
	before, err := os.ReadFile(tokenPath)
	requirements.NoError(err)
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			http.Error(w, "unexpected write", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/gmail/v1/users/me/profile" {
			_ = json.NewEncoder(w).Encode(map[string]string{"emailAddress": source.Identifier})
		} else {
			_, _ = w.Write([]byte(`{"labels":[]}`))
		}
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	requirements.NoError(err)
	transport := tagGmailTransport(func(r *http.Request) (*http.Response, error) {
		clonedRequest := r.Clone(r.Context())
		u := *r.URL
		clonedRequest.URL = &u
		clonedRequest.URL.Scheme, clonedRequest.URL.Host = endpoint.Scheme, endpoint.Host
		return http.DefaultTransport.RoundTrip(clonedRequest)
	})
	adapter := &storeAPIAdapter{store: st, config: cfg, inboxKey: []byte(strings.Repeat("k", 32)), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture-token"}), gmail.WithTransport(transport)), nil
	}}
	binding := inboxcontrol.SourceIdentity{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier}
	gates := 0
	result, err := adapter.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &binding}, inboxcontrol.Principal{ID: "owner", Owner: true}, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, func(context.Context) (func(), error) { gates++; return func() {}, nil })
	requirements.NoError(err)
	requirements.NotNil(result.Capabilities)
	for _, capability := range result.Capabilities.Operations {
		want := inboxcontrol.CapabilityPermissionRequired
		if capability.Operation == inboxcontrol.OpGetCapabilities || capability.Operation == inboxcontrol.OpGetState || capability.Operation == inboxcontrol.OpListFolders {
			want = inboxcontrol.CapabilitySupported
		}
		assertions.Equal(want, capability.Status)
	}
	after, err := os.ReadFile(tokenPath)
	requirements.NoError(err)
	assertions.Equal(before, after)
	assertions.Equal(0, writes)
	assertions.Equal(0, gates)
}

func TestDaemonInboxUnknownGmailWritePermissionFailsBeforeClientCreation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	tokenPath, restore := seedTokenEnv(t, legacyTokenJSON)
	defer restore()
	home := filepath.Dir(filepath.Dir(tokenPath))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir, cfg.OAuth.ClientSecrets = home, home, filepath.Join(home, "client_secret.json")
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", scopeEscalationAccount)
	requirements.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "scope-unknown-thread", "Scope fixture")
	requirements.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "scope-unknown-message", MessageType: "email"})
	requirements.NoError(err)
	before, err := os.ReadFile(tokenPath)
	requirements.NoError(err)
	connects := 0
	// A real Store binding and saved-token parser must reject unknown authority
	// before constructing a provider client or requesting any broader credential.
	adapter := &storeAPIAdapter{store: st, config: cfg, inboxKey: []byte(strings.Repeat("k", 32)), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		connects++
		return nil, errors.New("must not connect")
	}}
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "scope-unknown-message"}
	_, err = adapter.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}, inboxcontrol.Principal{ID: "owner", Owner: true}, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, nil)
	requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
	assertions.Equal(0, connects)
	after, err := os.ReadFile(tokenPath)
	requirements.NoError(err)
	assertions.Equal(before, after)
}

type tagGmailTransport func(*http.Request) (*http.Response, error)

func (f tagGmailTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
