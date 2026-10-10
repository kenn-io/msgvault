package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

type inboxBindingProvider struct{ closed int }

func (p *inboxBindingProvider) Observe(_ context.Context, r inboxcontrol.Request) (inboxcontrol.State, error) {
	if r.Target != nil {
		return inboxcontrol.State{Target: *r.Target, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()}, nil
	}
	return inboxcontrol.State{Source: *r.Source, ObservedAt: time.Now().UTC()}, nil
}
func (*inboxBindingProvider) Preview(_ context.Context, _ inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	return before, nil
}
func (*inboxBindingProvider) Dispatch(context.Context, inboxcontrol.Request, inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	return inboxcontrol.DispatchResult{}, inboxcontrol.ErrUnavailable
}
func (*inboxBindingProvider) Verify(inboxcontrol.Request, inboxcontrol.State, inboxcontrol.State, inboxcontrol.State) error {
	return inboxcontrol.ErrUnavailable
}
func (*inboxBindingProvider) Folders(context.Context, inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
	return nil, nil
}
func (p *inboxBindingProvider) Close() error { p.closed++; return nil }

func inboxBindingFixture(t *testing.T, kind string) (*storeAPIAdapter, inboxcontrol.Target, *inboxBindingProvider, *int) {
	t.Helper()
	st := testutil.NewTestStore(t)
	identifier, account := "owner@example.test", "owner@example.test"
	imapCfg := imaplib.Config{Host: "mail.example.test", Username: account, TLS: true}
	if kind == "imap" {
		identifier = imapCfg.Identifier()
	}
	if kind == "beeper" {
		identifier, account = "account-fixture", "account-fixture"
	}
	source, err := st.GetOrCreateSource(kind, identifier)
	require.NoError(t, err)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: kind, SourceIdentifier: identifier, AccountID: account, Scope: inboxcontrol.ScopeMessage, ProviderID: "mail-fixture"}
	if kind == "" {
		target.SourceType = "gmail"
	}
	conversation, err := st.EnsureConversation(source.ID, "chat-fixture", "Fixture")
	require.NoError(t, err)
	if kind == "beeper" {
		target.Scope, target.ItemID, target.ProviderID = inboxcontrol.ScopeChat, conversation, "chat-fixture"
	} else {
		if kind == "imap" {
			target.ProviderID, target.Mailbox, target.UIDValidity, target.UID = "INBOX|7", "INBOX", 77, 7
		}
		target.ItemID, err = st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: target.ProviderID, ConversationID: conversation, MessageType: "email"})
		require.NoError(t, err)
	}
	if kind == "imap" {
		encoded, err := imapCfg.ToJSON()
		require.NoError(t, err)
		require.NoError(t, st.UpdateSourceSyncConfig(source.ID, encoded))
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext) VALUES(?,?,?,?)`), source.ID, target.Mailbox, target.UIDValidity, 8)
		require.NoError(t, err)
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_message_memberships(source_id,mailbox,uidvalidity,uid,message_id,flags) VALUES(?,?,?,?,?,?)`), source.ID, target.Mailbox, target.UIDValidity, target.UID, target.ItemID, `[]`)
		require.NoError(t, err)
	}
	provider, calls := &inboxBindingProvider{}, new(int)
	a := &storeAPIAdapter{store: st, config: &config.Config{}, inboxKey: []byte(strings.Repeat("k", 32)), inboxProviderFactory: func(context.Context, *store.Source, inboxcontrol.Request) (inboxcontrol.Provider, error) {
		*calls++
		return provider, nil
	}}
	return a, target, provider, calls
}

func TestDaemonInboxResolutionRejectsInjectedBindingsBeforeProviderAccess(t *testing.T) {
	for _, kind := range []string{"gmail", "imap", "beeper"} {
		t.Run(kind, func(t *testing.T) {
			a, target, _, calls := inboxBindingFixture(t, kind)
			for _, field := range []string{"source", "identifier", "account", "item", "provider", "epoch", "uid"} {
				if kind != "imap" && (field == "epoch" || field == "uid") {
					continue
				}
				wrong := target
				switch field {
				case "source":
					wrong.SourceID++
				case "identifier":
					wrong.SourceIdentifier = "different@example.test"
				case "account":
					wrong.AccountID = "different@example.test"
				case "item":
					wrong.ItemID += 10000
				case "provider":
					wrong.ProviderID = "different-provider-id"
				case "epoch":
					wrong.UIDValidity++
				case "uid":
					wrong.UID++
				}
				_, err := a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &wrong}, inboxcontrol.Principal{ID: "owner", Owner: true}, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, nil)
				require.Error(t, err, field)
				assert.Equal(t, 0, *calls, field)
			}
		})
	}
}

func TestDaemonInboxValidBindingsAndProviderCleanup(t *testing.T) {
	for _, kind := range []string{"gmail", "", "imap", "beeper"} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			a, target, provider, calls := inboxBindingFixture(t, kind)
			got, err := a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}, inboxcontrol.Principal{ID: "owner", Owner: true}, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, nil)
			requirements.NoError(err)
			requirements.NotNil(got.Before)
			assertions.Equal(target, got.Before.Target)
			assertions.Equal(1, *calls)
			assertions.Equal(1, provider.closed)
		})
	}
}

func TestDaemonInboxMissingKeyNeverProvisionedByRequest(t *testing.T) {
	a, target, _, calls := inboxBindingFixture(t, "gmail")
	a.inboxKey = nil
	_, err := a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}, inboxcontrol.Principal{ID: "owner", Owner: true}, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, nil)
	require.ErrorIs(t, err, inboxcontrol.ErrInternal)
	assert.Equal(t, 0, *calls)
}

func TestDaemonInboxStartupProvisioningAndRecovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	a, target, _, _ := inboxBindingFixture(t, "gmail")
	cfg := &config.Config{HomeDir: t.TempDir()}
	first, err := prepareInboxRuntime(t.Context(), cfg, a.store)
	requirements.NoError(err)
	before := inboxcontrol.State{Target: target, Inbox: new(true), ObservedAt: time.Now().UTC()}
	intent := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}
	intentHash, err := inboxcontrol.IntentFingerprint(intent)
	requirements.NoError(err)
	stateHash, err := inboxcontrol.SemanticFingerprint(before)
	requirements.NoError(err)
	receipt := inboxcontrol.Receipt{ID: "startup-receipt", PrincipalID: "owner", SourceID: target.SourceID, IdempotencyKey: "startup-operation", IntentHash: intentHash, StateHash: stateHash, Intent: intent, Before: before, Status: inboxcontrol.StatusPrepared, CreatedAt: time.Now().UTC()}
	_, _, err = a.store.PrepareInboxReceipt(t.Context(), receipt)
	requirements.NoError(err)
	requirements.NoError(a.store.MarkInboxDispatching(t.Context(), receipt.ID))
	second, err := prepareInboxRuntime(t.Context(), cfg, a.store)
	requirements.NoError(err)
	assertions.Equal(first, second)
	requirements.Len(second, 32)
	got, err := a.store.GetInboxReceipt(t.Context(), receipt.ID)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusUnknown, got.Status)
}

func TestDaemonInboxGmailNativeSignedExecution(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	a, target, _, _ := inboxBindingFixture(t, "gmail")
	a.inboxProviderFactory = nil
	a.config = &config.Config{OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}
	labels := []string{"INBOX", "UNREAD", "STARRED"}
	native, err := a.store.EmailTagTargetContext(t.Context(), target.ItemID, "")
	requirements.NoError(err)
	requirements.NoError(a.store.SaveEmailTagsContext(t.Context(), native, &emailtags.Result{Provider: "gmail", Tags: labels, Verified: true}))
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

func TestDaemonInboxDefaultFactoryUsesNativeIMAP(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{MessagesPerMailbox: map[string]int{"INBOX": 1}, Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapUIDPlus: {}}})
	host, portText, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	port, err := strconv.Atoi(portText)
	requirements.NoError(err)
	cfg := &imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
	raw, err := imapclient.DialInsecure(addr, nil)
	requirements.NoError(err)
	t.Cleanup(func() { _ = raw.Close() })
	requirements.NoError(raw.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	selected, err := raw.Select("INBOX", nil).Wait()
	requirements.NoError(err)
	_, err = raw.Store(imapapi.UIDSetNum(1), &imapapi.StoreFlags{Op: imapapi.StoreFlagsAdd, Flags: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, "Unrelated"}}, nil).Collect()
	requirements.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", cfg.Identifier())
	requirements.NoError(err)
	configJSON, err := cfg.ToJSON()
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(source.ID, configJSON))
	conv, err := st.EnsureConversation(source.ID, "native-inbox-thread", "Native inbox")
	requirements.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "INBOX|1", MessageType: "email"})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext) VALUES(?,?,?,?)`), source.ID, "INBOX", selected.UIDValidity, 2)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_message_memberships(source_id,mailbox,uidvalidity,uid,message_id,flags) VALUES(?,?,?,?,?,?)`), source.ID, "INBOX", selected.UIDValidity, 1, mid, `["\\Seen","\\Flagged","Unrelated"]`)
	requirements.NoError(err)
	adapter := &storeAPIAdapter{store: st, config: &config.Config{}, inboxKey: []byte(strings.Repeat("k", 32)), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		return imaplib.NewClient(cfg, testutil.IMAPTestPassword), nil
	}}
	binding := inboxcontrol.SourceIdentity{SourceID: source.ID, SourceType: "imap", SourceIdentifier: source.Identifier, AccountID: cfg.Username}
	owner := inboxcontrol.Principal{ID: "owner", Owner: true}
	authorize := func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }
	gates, releases := 0, 0
	gate := func(context.Context) (func(), error) { gates++; return func() { releases++ }, nil }
	capabilities, err := adapter.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &binding}, owner, authorize, gate)
	requirements.NoError(err)
	requirements.NotNil(capabilities.Capabilities)
	assertions.Equal("mailboxes", capabilities.Capabilities.LocationModel)
	assertions.False(capabilities.Capabilities.ConditionalWrite)
	assertions.Zero(gates)
	folders, err := adapter.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpListFolders, Source: &binding}, owner, authorize, gate)
	requirements.NoError(err)
	assertions.Equal([]inboxcontrol.Folder{{ID: "INBOX", Name: "INBOX", UIDValidity: selected.UIDValidity}}, folders.Folders)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "imap", SourceIdentifier: source.Identifier, AccountID: cfg.Username, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "INBOX|1", Mailbox: "INBOX", UIDValidity: selected.UIDValidity, UID: 1}
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetUnread, Target: &target, DryRun: true}
	preview, err := adapter.ControlInbox(t.Context(), request, owner, authorize, gate)
	requirements.NoError(err)
	assertions.True(*preview.Before.Read)
	assertions.False(*preview.Projected.Read)
	assertions.Zero(gates)
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "native-imap-unread"
	result, err := adapter.ControlInbox(t.Context(), request, owner, authorize, gate)
	requirements.NoError(err)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	assertions.False(*result.After.Read)
	assertions.True(*result.After.Inbox)
	assertions.Contains(result.After.Flags, string(imapapi.FlagFlagged))
	assertions.Equal([]string{"unrelated"}, result.After.Tags)
	assertions.Equal(1, gates)
	assertions.Equal(1, releases)
	observation, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(observation)
	assertions.False(*observation.Read)
	replay, err := adapter.ControlInbox(t.Context(), request, owner, authorize, gate)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	assertions.Equal(1, gates)
	create := inboxcontrol.Request{Operation: inboxcontrol.OpCreateFolder, Source: &binding, Destination: &inboxcontrol.Folder{Name: "Followups"}, DryRun: true}
	folderPreview, err := adapter.ControlInbox(t.Context(), create, owner, authorize, gate)
	requirements.NoError(err)
	assertions.Equal(1, gates)
	create.DryRun, create.Expected, create.PreviewToken, create.IdempotencyKey = false, folderPreview.Before, folderPreview.PreviewToken, "native-imap-folder"
	created, err := adapter.ControlInbox(t.Context(), create, owner, authorize, gate)
	requirements.NoError(err)
	requirements.NotNil(created.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, created.Receipt.Status)
	requirements.NotNil(created.After.ProvisionedFolder)
	assertions.Equal("Followups", created.After.ProvisionedFolder.ID)
	states, err := st.GetIMAPFolderStates(source.ID)
	requirements.NoError(err)
	assertions.Contains(states, store.IMAPFolderState{Mailbox: "Followups", UIDValidity: created.After.ProvisionedFolder.UIDValidity})
	assertions.Equal(2, gates)
	assertions.Equal(2, releases)
}

// Inject only a lost readback after the real native MOVE and COPYUID response.
type moveReadbackFaultProvider struct{ *imaplib.InboxProvider }

func (p *moveReadbackFaultProvider) Observe(ctx context.Context, r inboxcontrol.Request) (inboxcontrol.State, error) {
	if r.Target != nil && r.Target.Mailbox == "Archive" {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	return p.InboxProvider.Observe(ctx, r)
}

// The provider returns a real COPYUID mapping, then the caller cancels before
// readback. The hook also attempts a real competing sync lease during dispatch.
type moveCancellationProvider struct {
	*imaplib.InboxProvider

	afterWrite func()
}

func (p *moveCancellationProvider) Dispatch(ctx context.Context, r inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	result, err := p.InboxProvider.Dispatch(ctx, r, before)
	if err != nil {
		return result, err
	}
	p.afterWrite()
	return result, ctx.Err()
}

func TestDaemonInboxReconcilesProvedIMAPMappingBeforeLocalMembership(t *testing.T) {
	testDaemonInboxNativeMoveRecovery(t, false)
}
func TestDaemonInboxCanceledNativeMoveKeepsMappingAndExcludesSync(t *testing.T) {
	testDaemonInboxNativeMoveRecovery(t, true)
}
func testDaemonInboxNativeMoveRecovery(t *testing.T, cancelAfterWrite bool) {
	t.Helper()
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Archive", nil))
	testutil.AppendIMAPMessage(t, user, "INBOX")
	testutil.AppendIMAPMessage(t, user, "Archive")
	mem := imapmemserver.New()
	mem.AddUser(user)
	server := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapMove: {}, imapapi.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return mem.NewSession(), nil, nil
	}})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	cfg := &imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
	raw, err := imapclient.DialInsecure(listener.Addr().String(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	require.NoError(t, raw.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	origin, err := raw.Select("INBOX", nil).Wait()
	require.NoError(t, err)
	destination, err := raw.Select("Archive", nil).Wait()
	require.NoError(t, err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", cfg.Identifier())
	require.NoError(t, err)
	configJSON, err := cfg.ToJSON()
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, configJSON))
	conv, err := st.EnsureConversation(source.ID, "move-recovery-thread", "Move recovery")
	require.NoError(t, err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "INBOX|1", MessageType: "email"})
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext) VALUES(?,?,?,?)`), source.ID, "INBOX", origin.UIDValidity, 2)
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_message_memberships(source_id,mailbox,uidvalidity,uid,message_id,flags) VALUES(?,?,?,?,?,?)`), source.ID, "INBOX", origin.UIDValidity, 1, mid, `[]`)
	require.NoError(t, err)
	binding := inboxcontrol.SourceIdentity{SourceID: source.ID, SourceType: "imap", SourceIdentifier: source.Identifier, AccountID: cfg.Username}
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "imap", SourceIdentifier: source.Identifier, AccountID: cfg.Username, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "INBOX|1", Mailbox: "INBOX", UIDValidity: origin.UIDValidity, UID: 1}
	adapter := &storeAPIAdapter{store: st, config: &config.Config{}, inboxKey: []byte(strings.Repeat("k", 32)), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		return imaplib.NewClient(cfg, testutil.IMAPTestPassword), nil
	}}
	executionCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var competingSyncErr error
	writes := 0
	adapter.inboxProviderFactory = func(context.Context, *store.Source, inboxcontrol.Request) (inboxcontrol.Provider, error) {
		if cancelAfterWrite {
			return &moveCancellationProvider{InboxProvider: imaplib.NewInboxProvider(imaplib.NewClient(cfg, testutil.IMAPTestPassword), binding), afterWrite: func() {
				writes++
				lease, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
				competingSyncErr = err
				if lease != nil {
					require.NoError(t, lease.Release())
				}
				cancel()
			}}, nil
		}
		return &moveReadbackFaultProvider{InboxProvider: imaplib.NewInboxProvider(imaplib.NewClient(cfg, testutil.IMAPTestPassword), binding)}, nil
	}
	owner := inboxcontrol.Principal{ID: "owner", Owner: true}
	authorize := func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }
	gate := func(context.Context) (func(), error) { return func() {}, nil }
	request := inboxcontrol.Request{Operation: inboxcontrol.OpMove, Target: &target, Destination: &inboxcontrol.Folder{ID: "Archive", UIDValidity: destination.UIDValidity}, DryRun: true}
	preview, err := adapter.ControlInbox(t.Context(), request, owner, authorize, gate)
	require.NoError(t, err)
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "native-move-lost-readback"
	uncertain, err := adapter.ControlInbox(executionCtx, request, owner, authorize, gate)
	require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
	require.NotNil(t, uncertain.Receipt)
	assert.Equal(t, inboxcontrol.StatusUnknown, uncertain.Receipt.Status)
	stored, storedErr := st.GetInboxReceipt(t.Context(), uncertain.Receipt.ID)
	require.NoError(t, storedErr)
	require.NotNil(t, stored.FinishedAt)
	if cancelAfterWrite {
		require.ErrorIs(t, competingSyncErr, store.ErrSyncAlreadyActive)
		assert.Equal(t, 1, writes)
	}
	require.NotNil(t, uncertain.Receipt.After)
	assert.Equal(t, uint32(2), uncertain.Receipt.After.Target.UID)
	_, err = st.EmailTagTargetContext(t.Context(), mid, "Archive")
	require.Error(t, err)
	adapter.inboxProviderFactory = nil
	mapped := uncertain.Receipt.After.Target
	_, err = adapter.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &mapped}, owner, authorize, gate)
	require.ErrorIs(t, err, inboxcontrol.ErrDenied, "ordinary requests cannot inject durable mapping authority")
	recovered, err := adapter.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: uncertain.Receipt.ID}, owner, authorize, gate)
	require.NoError(t, err)
	assert.Equal(t, inboxcontrol.StatusVerified, recovered.Receipt.Status)
	if cancelAfterWrite {
		assert.Equal(t, 1, writes, "recovery must not dispatch MOVE again")
	}
	lease, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(t, err, "completion releases the source lease")
	require.NoError(t, lease.Release())
	local, err := st.EmailTagTargetContext(t.Context(), mid, "Archive")
	require.NoError(t, err)
	assert.Equal(t, uint32(2), local.UID)
	_, err = st.EmailTagTargetContext(t.Context(), mid, "INBOX")
	assert.Error(t, err)
}

func TestDaemonInboxMappingUsesNativeCatalogAndSourceLease(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	a, target, _, _ := inboxBindingFixture(t, "gmail")
	a.inboxProviderFactory = nil
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, http.MethodGet, r.Method) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": target.AccountID})
		case "/gmail/v1/users/me/labels":
			_, err := w.Write([]byte(`{"labels":[{"id":"Label_1","name":"Todo","type":"user"}]}`))
			assert.NoError(t, err)
		default:
			assert.Fail(t, "unexpected native mapping request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer native.Close()
	endpoint, err := url.Parse(native.URL)
	requirements.NoError(err)
	a.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-token"}), gmail.WithTransport(tagGmailTransport(func(r *http.Request) (*http.Response, error) {
			copied := r.Clone(r.Context())
			u := *r.URL
			u.Scheme = endpoint.Scheme
			u.Host = endpoint.Host
			copied.URL = &u
			return http.DefaultTransport.RoundTrip(copied)
		})), gmail.WithRateLimiter(gmail.NewRateLimiter(10000))), nil
	}
	updater, ok := any(a).(interface {
		UpdateInboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity, entries map[string]string, expectedRevision int64, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, acquireWrite func(context.Context) (func(), error)) (int64, error)
	})
	requirements.True(ok, "daemon must expose gated native mapping configuration")
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	gates, releases := 0, 0
	revision, err := updater.UpdateInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Label_1"}, 0, inboxcontrol.Principal{ID: "owner", Owner: true}, func(_ context.Context, p inboxcontrol.Principal, r inboxcontrol.Request) error {
		assert.True(t, p.Owner)
		require.NotNil(t, r.Source)
		assert.Equal(t, source, *r.Source)
		return nil
	}, func(context.Context) (func(), error) { gates++; return func() { releases++ }, nil })
	requirements.NoError(err)
	assertions.Equal(int64(1), revision)
	assertions.Equal(1, gates)
	assertions.Equal(gates, releases)
	stored, storedRevision, err := a.store.InboxTriageMappings(t.Context(), source)
	requirements.NoError(err)
	assertions.Equal(map[string]string{"todo": "Label_1"}, stored)
	assertions.Equal(revision, storedRevision)
	lease, err := a.store.AcquireSyncExecutionContext(t.Context(), source.SourceID)
	requirements.NoError(err)
	requirements.NoError(lease.Release())
}

func nativeInboxTriageFixture(t *testing.T) (*storeAPIAdapter, inboxcontrol.Target, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	a, target, _, _ := inboxBindingFixture(t, "gmail")
	a.inboxProviderFactory = nil
	// Native scope probes read a real saved synthetic token in this fixture home.
	home := t.TempDir()
	a.config = config.NewDefaultConfig()
	a.config.HomeDir, a.config.Data.DataDir = home, home
	a.config.OAuth.ClientSecrets = filepath.Join(home, "client_secret.json")
	a.logger = testDiscardLogger()
	require.NoError(t, os.WriteFile(a.config.OAuth.ClientSecrets, []byte(fakeClientSecrets), 0600))
	require.NoError(t, os.MkdirAll(a.config.TokensDir(), 0700))
	tokenPath := filepath.Join(a.config.TokensDir(), target.AccountID+".json")
	tokenData := []byte(fmt.Sprintf(`{"access_token":"synthetic-token","expiry":"2099-01-01T00:00:00Z","scopes":[%q]}`, oauth.ScopeGmailModify))
	require.NoError(t, os.WriteFile(tokenPath, tokenData, 0600))

	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	labels := []string{"INBOX", "UNREAD", "Unrelated"}
	var nativeMu sync.Mutex
	var writes atomic.Int64
	var lost atomic.Bool
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeMu.Lock()
		defer nativeMu.Unlock()
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"emailAddress": source.AccountID}))
		case "/gmail/v1/users/me/labels":
			assert.Equal(t, http.MethodGet, r.Method)
			_, err := fmt.Fprint(w, `{"labels":[{"id":"Label_1","name":"Todo","type":"user"}]}`)
			assert.NoError(t, err)
		case "/gmail/v1/users/me/messages/mail-fixture":
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "metadata", r.URL.Query().Get("format"))
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": target.ProviderID, "labelIds": labels, "historyId": "8"}))
		case "/gmail/v1/users/me/messages/mail-fixture/modify":
			assert.Equal(t, http.MethodPost, r.Method)
			writes.Add(1)
			lease, err := a.store.AcquireSyncExecutionContext(r.Context(), source.SourceID)
			assert.Error(t, err)
			if lease != nil {
				assert.NoError(t, lease.Release())
			}
			var change struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&change)) {
				return
			}
			assert.Equal(t, []string{"Label_1"}, change.Add)
			assert.Empty(t, change.Remove)
			labels = append(labels, change.Add...)
			if lost.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": target.ProviderID, "labelIds": labels, "historyId": "8"}))
		default:
			assert.Fail(t, "unexpected native request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(native.Close)
	endpoint, err := url.Parse(native.URL)
	require.NoError(t, err)
	a.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-token"}), gmail.WithTransport(tagGmailTransport(func(r *http.Request) (*http.Response, error) {
			copied := r.Clone(r.Context())
			u := *r.URL
			u.Scheme = endpoint.Scheme
			u.Host = endpoint.Host
			copied.URL = &u
			return http.DefaultTransport.RoundTrip(copied)
		})), gmail.WithRateLimiter(gmail.NewRateLimiter(10000))), nil
	}
	for _, id := range []string{"Label_1", "Unrelated"} {
		_, err := a.store.EnsureLabel(source.SourceID, id, id, "user")
		require.NoError(t, err)
	}
	_, err = a.store.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Label_1"}, 0, inboxcontrol.Principal{ID: "owner", Owner: true})
	require.NoError(t, err)
	_, err = a.store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), Tags: slices.Clone(labels), Revision: "8", ObservedAt: time.Now().UTC()})
	require.NoError(t, err)
	t.Cleanup(func() {
		saved, err := os.ReadFile(tokenPath)
		require.NoError(t, err)
		assert.Equal(t, tokenData, saved, "triage must preserve synthetic scope metadata")
	})
	return a, target, &writes, &lost
}

func TestDaemonInboxTriageUsesNativeRuntimeAuthorizationAndReceipts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	a, target, writes, _ := nativeInboxTriageFixture(t)
	backend, ok := any(a).(interface {
		PreviewInboxTriage(ctx context.Context, input inboxcontrol.TriageInput, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error) (*inboxcontrol.TriageProposal, error)
		ApplyInboxTriage(ctx context.Context, proposal inboxcontrol.TriageProposal, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, acquireWrite func(context.Context) (func(), error)) ([]inboxcontrol.Result, error)
	})
	requirements.True(ok, "daemon must expose native triage preview/apply")
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	principal := inboxcontrol.Principal{ID: "triage-delegate"}
	allowed := true
	authorize := func(_ context.Context, p inboxcontrol.Principal, r inboxcontrol.Request) error {
		if !allowed || p != principal {
			return inboxcontrol.ErrDenied
		}
		if r.Target != nil {
			if *r.Target != target {
				return inboxcontrol.ErrDenied
			}
		} else if r.Source == nil || *r.Source != source {
			return inboxcontrol.ErrDenied
		}
		return nil
	}
	input := inboxcontrol.TriageInput{Source: source, Items: []inboxcontrol.TriageItemInput{{Target: target, Categories: []string{"todo"}, EvidenceMessageIDs: []int64{target.ItemID}, IdempotencyKey: "daemon-triage-1"}}}
	proposal, err := backend.PreviewInboxTriage(t.Context(), input, principal, authorize)
	requirements.NoError(err)
	assertions.Zero(writes.Load())
	allowed = false
	_, err = backend.ApplyInboxTriage(t.Context(), *proposal, principal, authorize, nil)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	assertions.Zero(writes.Load())
	allowed = true
	gateCalls := 0
	results, err := backend.ApplyInboxTriage(t.Context(), *proposal, principal, authorize, func(context.Context) (func(), error) { gateCalls++; return func() {}, nil })
	requirements.NoError(err)
	requirements.Len(results, 1)
	requirements.NotNil(results[0].Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, results[0].Receipt.Status)
	assertions.True(*results[0].After.Inbox)
	assertions.False(*results[0].After.Read)
	assertions.Equal(int64(1), writes.Load())
	assertions.Equal(1, gateCalls)
	again, err := backend.ApplyInboxTriage(t.Context(), *proposal, principal, authorize, nil)
	requirements.NoError(err)
	requirements.Len(again, 1)
	requirements.NotNil(again[0].Receipt)
	assertions.Equal(results[0].Receipt.ID, again[0].Receipt.ID)
	assertions.Equal(int64(1), writes.Load())
}
