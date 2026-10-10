package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/store"
)

func TestDaemonInboxMicrosoftNativeController(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		legacy    bool
		oversized bool
	}{
		{"controller OK", http.StatusOK, false, false}, {"controller unknown", http.StatusServiceUnavailable, false, false},
		{"legacy client OK", http.StatusOK, true, false}, {"legacy client unknown", http.StatusServiceUnavailable, true, false},
		{"controller oversized success", http.StatusOK, false, true}, {"legacy client oversized success", http.StatusOK, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			status := tc.status
			a, target, _, _ := inboxBindingFixture(t, "msmail")
			a.inboxProviderFactory = nil
			a.config = lifecycleTestConfig(t.TempDir())
			folder, err := a.store.EnsureLabel(target.SourceID, "inbox-id", "Inbox", "system")
			requirements.NoError(err)
			_, err = a.store.ReconcileMessageLabels(target.ItemID, []int64{folder}, true)
			requirements.NoError(err)
			_, err = a.store.DB().Exec(a.store.Rebind(`UPDATE messages SET is_read = TRUE WHERE id = ?`), target.ItemID)
			requirements.NoError(err)
			tokenPath := microsoft.NewGraphMailManager("", "", "", a.config.TokensDir(), nil).TokenPath(target.AccountID)
			requirements.NoError(os.MkdirAll(filepath.Dir(tokenPath), 0700))
			saveScopes := func(scopes []string) {
				data, err := json.Marshal(map[string]any{"access_token": "synthetic-token", "scopes": scopes})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(tokenPath, data, 0600))
			}
			saveScopes(microsoft.GraphMailWriteScopes())
			var writes, clients atomic.Int32
			var mu sync.Mutex
			tags := []string{"Other"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/me":
					assert.NoError(t, json.MarshalWrite(w, map[string]string{"mail": target.AccountID}))
				case "/me/mailFolders/inbox":
					assert.NoError(t, json.MarshalWrite(w, map[string]string{"id": "inbox-id"}))
				case "/me/messages/" + target.ProviderID:
					if r.Method == http.MethodPatch {
						writes.Add(1)
						assert.Equal(t, `W/"v1"`, r.Header.Get("If-Match"))
						var body map[string][]string
						assert.NoError(t, json.UnmarshalRead(r.Body, &body))
						assert.Len(t, body, 1)
						assert.ElementsMatch(t, []string{"Other", "Next"}, body["categories"])
						if status != http.StatusOK {
							w.WriteHeader(status)
							return
						}
						tags = body["categories"]
						if tc.oversized {
							w.WriteHeader(http.StatusOK)
							// Closing an unused oversized body is expected; verification uses GET.
							_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
							return
						}
					}
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"id": target.ProviderID, "categories": tags, "isRead": false, "parentFolderId": "inbox-id", "@odata.etag": `W/"v1"`}))
				default:
					assert.Fail(t, "unexpected native Graph request", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			a.microsoftTagClientFactory = func(context.Context, *store.Source, bool) (*msmail.Client, error) {
				clients.Add(1)
				return msmail.NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), nil
			}
			if tc.legacy {
				daemon := httptest.NewServer(api.NewServer(a.config, a, nil, slog.New(slog.DiscardHandler)).Router())
				defer daemon.Close()
				client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: a.config.Server.AuthenticationKey(), AllowInsecure: true, HTTPClient: daemon.Client()})
				requirements.NoError(err)
				defer func() { assert.NoError(t, client.Close()) }()
				result, err := client.MessageTags(t.Context(), target.ItemID, &emailtags.Change{Add: []string{"Next"}}, "")
				requirements.NotNil(result)
				assertions.NotEmpty(result.ReceiptID)
				assertions.NotEmpty(result.IdempotencyKey)
				if status == http.StatusOK {
					requirements.NoError(err)
					assertions.True(result.Verified)
					assertions.Equal(string(inboxcontrol.StatusVerified), result.ReceiptStatus)
					local, err := a.store.GetMessage(target.ItemID)
					requirements.NoError(err)
					assertions.ElementsMatch([]string{"Inbox", "Category: Other", "Category: Next"}, local.Labels)
				} else {
					var failure *emailtags.Error
					requirements.ErrorAs(err, &failure)
					assertions.Equal("remote_unknown", failure.Code)
					assertions.Equal(string(inboxcontrol.StatusUnknown), result.ReceiptStatus)
					assertions.False(result.Verified)
				}
				receipt, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: result.ReceiptID})
				requirements.NoError(err)
				assertions.Equal(result.ReceiptStatus, string(receipt.Receipt.Status))
				assertions.Equal(int32(1), writes.Load())
				return
			}
			principal := inboxcontrol.Principal{ID: "owner", Owner: true}
			authorize := func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }
			var gates, releases atomic.Int32
			gate := func(context.Context) (func(), error) {
				gates.Add(1)
				return func() { releases.Add(1) }, nil
			}
			request := inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.Change{Add: []string{"Next"}}, DryRun: true}
			preview, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
			requirements.NoError(err)
			requirements.NotNil(preview.Before)
			assertions.Zero(writes.Load())
			assertions.Zero(gates.Load())
			request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "native-graph-category"
			result, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
			requirements.NotNil(result, "control error: %v", err)
			requirements.NotNil(result.Receipt)
			if status == http.StatusOK {
				requirements.NoError(err)
				assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
				local, err := a.store.GetMessage(target.ItemID)
				requirements.NoError(err)
				assertions.ElementsMatch([]string{"Inbox", "Category: Other", "Category: Next"}, local.Labels)
				var uiRead bool
				requirements.NoError(a.store.DB().QueryRow(a.store.Rebind(`SELECT is_read FROM messages WHERE id = ?`), target.ItemID).Scan(&uiRead))
				assertions.True(uiRead, "provider read state must not replace local UI read state")
				observed, err := a.store.GetInboxProviderState(t.Context(), target)
				requirements.NoError(err)
				requirements.NotNil(observed)
				assertions.Equal(result.After, observed)
			} else {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
				assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
			}
			replay, replayErr := a.ControlInbox(t.Context(), request, principal, authorize, gate)
			requirements.NoError(replayErr)
			requirements.NotNil(replay)
			requirements.NotNil(replay.Receipt)
			assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
			assertions.Equal(result.Receipt.Status, replay.Receipt.Status)
			assertions.Equal(int32(1), writes.Load())
			assertions.Equal(int32(1), gates.Load())
			assertions.Equal(gates.Load(), releases.Load())
			source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
			caps, err := a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source}, principal, authorize, gate)
			requirements.NoError(err)
			requirements.NotNil(caps.Capabilities)
			assertions.False(caps.Capabilities.ConditionalWrite, "fixture If-Match is not proof of a native CAS guarantee")
			for _, capability := range caps.Capabilities.Operations {
				want := inboxcontrol.CapabilityUnsupported
				if capability.Operation == inboxcontrol.OpTags || capability.Operation == inboxcontrol.OpGetState || capability.Operation == inboxcontrol.OpGetCapabilities {
					want = inboxcontrol.CapabilitySupported
				}
				assertions.Equal(want, capability.Status, capability.Operation)
			}
			saveScopes(microsoft.GraphMailScopes())
			clientCount := clients.Load()
			_, err = a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: request.Tags, DryRun: true}, principal, authorize, gate)
			requirements.ErrorIs(err, inboxcontrol.ErrDenied)
			assertions.Equal(clientCount, clients.Load(), "missing scope denies before native client access")
			caps, err = a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source}, principal, authorize, gate)
			requirements.NoError(err)
			for _, capability := range caps.Capabilities.Operations {
				if capability.Operation == inboxcontrol.OpTags {
					assertions.Equal(inboxcontrol.CapabilityPermissionRequired, capability.Status)
				}
			}
			requirements.NoError(os.Remove(tokenPath))
			clientCount = clients.Load()
			_, err = a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: request.Tags, DryRun: true}, principal, authorize, gate)
			requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
			assertions.Equal(clientCount, clients.Load(), "unknown saved grant must fail before native client access")
			assertions.Equal(int32(1), writes.Load())
		})
	}
}

func TestMicrosoftTriageCatalogUsesDedicatedScopeAndResolver(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	a, target, _, _ := inboxBindingFixture(t, "msmail")
	a.inboxProviderFactory = nil
	cfg := lifecycleTestConfig(t.TempDir())
	a.config = cfg
	ctx := withStoreResolverConfig(t, cfg)
	tokenPath := microsoft.NewGraphMailManager("", "", "", cfg.TokensDir(), nil).TokenPath(target.AccountID)
	requirements.NoError(os.MkdirAll(filepath.Dir(tokenPath), 0700))
	saveScopes := func(scopes []string) {
		data, err := json.Marshal(map[string]any{"access_token": "synthetic-token", "scopes": scopes})
		requirements.NoError(err)
		requirements.NoError(os.WriteFile(tokenPath, data, 0600))
	}
	saveScopes(microsoft.GraphMailWriteScopes())
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	standard, triage, triageClients := 0, 0, 0
	a.microsoftTagClientFactory = func(context.Context, *store.Source, bool) (*msmail.Client, error) {
		standard++
		return msmail.NewClient("http://127.0.0.1:1", func(context.Context) (string, error) { return "synthetic-token", nil }, 1000), nil
	}
	a.microsoftTriageCatalogClientFactory = func(ctx context.Context, source *store.Source) (*msmail.Client, error) {
		triage++
		token, err := newGraphMailTriageManager(invocationFromContext(ctx)).TokenSource(ctx, source.Identifier)
		if err != nil {
			return nil, err
		}
		triageClients++
		return msmail.NewClient("http://127.0.0.1:1", token, 1000), nil
	}
	request := inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source, DryRun: true}
	provider, err := a.resolveInboxProvider(ctx, request)
	requirements.NoError(err)
	assertions.Equal(1, standard)
	assertions.Zero(triage)
	requirements.NotNil(provider)

	request.NativeTagCatalog = true
	_, err = a.resolveInboxProvider(ctx, request)
	requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
	assertions.Equal(1, standard)
	assertions.Equal(1, triage)
	assertions.Zero(triageClients, "the triage client must not open until the additional grant is present")

	saveScopes(microsoft.GraphMailTriageScopes())
	provider, err = a.resolveInboxProvider(ctx, request)
	requirements.NoError(err)
	assertions.Equal(1, standard)
	assertions.Equal(2, triage)
	assertions.Equal(1, triageClients)
	requirements.NotNil(provider)
}
