package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/oauth2"
)

func TestDaemonInboxNativeMutationReconcilesUnsyncedUserLabel(t *testing.T) {
	for _, tc := range []struct {
		op            inboxcontrol.Operation
		before, after []string
		add, remove   []string
	}{
		{inboxcontrol.OpArchive, []string{"INBOX", "UNREAD", "Label_external"}, []string{"UNREAD", "Label_external"}, nil, []string{"INBOX"}},
		{inboxcontrol.OpUnarchive, []string{"UNREAD", "Label_external"}, []string{"INBOX", "UNREAD", "Label_external"}, []string{"INBOX"}, nil},
		{inboxcontrol.OpSetRead, []string{"INBOX", "UNREAD", "Label_external"}, []string{"INBOX", "Label_external"}, nil, []string{"UNREAD"}},
		{inboxcontrol.OpSetUnread, []string{"INBOX", "Label_external"}, []string{"INBOX", "UNREAD", "Label_external"}, []string{"UNREAD"}, nil},
	} {
		t.Run(string(tc.op), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			adapter, target := inboxGmailFixture(t)
			adapter.config = &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "catalog-owner"}, OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}
			native, err := adapter.store.EmailTagTargetContext(t.Context(), target.ItemID, "")
			requirements.NoError(err)
			requirements.NoError(adapter.store.SaveEmailTagsContext(t.Context(), native, &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"INBOX", "UNREAD"}, Verified: true}))
			var mu sync.Mutex
			labels, writes, readbacks := slices.Clone(tc.before), 0, 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/gmail/v1/users/me/profile":
					_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": target.AccountID})
				case "/gmail/v1/users/me/labels":
					_, _ = w.Write([]byte(`{"labels":[{"id":"Label_external","name":"External","type":"user"}]}`))
				case "/gmail/v1/users/me/messages/mail-fixture":
					if writes > 0 {
						readbacks++
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": target.ProviderID, "labelIds": labels, "historyId": "12"})
				case "/gmail/v1/users/me/messages/mail-fixture/modify":
					writes++
					assertions.Equal(http.MethodPost, r.Method)
					var delta struct {
						Add    []string `json:"addLabelIds"`
						Remove []string `json:"removeLabelIds"`
					}
					if !assertions.NoError(json.NewDecoder(r.Body).Decode(&delta)) {
						return
					}
					assertions.Equal(tc.add, delta.Add)
					assertions.Equal(tc.remove, delta.Remove)
					labels = slices.Clone(tc.after)
					_, _ = w.Write([]byte(`{"id":"mail-fixture"}`))
				default:
					assertions.Fail("unexpected Gmail request", r.URL.String())
					http.NotFound(w, r)
				}
			}))
			defer provider.Close()
			endpoint, err := url.Parse(provider.URL)
			requirements.NoError(err)
			adapter.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) {
				transport := tagGmailTransport(func(r *http.Request) (*http.Response, error) {
					clone := r.Clone(r.Context())
					u := *r.URL
					clone.URL = &u
					clone.URL.Scheme, clone.URL.Host = endpoint.Scheme, endpoint.Host
					return http.DefaultTransport.RoundTrip(clone)
				})
				return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "catalog-token"}), gmail.WithTransport(transport), gmail.WithRateLimiter(gmail.NewRateLimiter(1000))), nil
			}
			server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: adapter.config, Store: adapter, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: adapter.config.Server.APIKey, AllowInsecure: true})
			requirements.NoError(err)
			defer client.Close()
			request := inboxcontrol.Request{Operation: tc.op, Target: &target, DryRun: true}
			preview, err := client.ControlInbox(t.Context(), request)
			requirements.NoError(err)
			var count int
			require.NoError(t, adapter.store.DB().QueryRow(adapter.store.Rebind(`SELECT COUNT(*) FROM labels WHERE source_id = ? AND source_label_id = ?`), target.SourceID, "Label_external").Scan(&count))
			assertions.Zero(count, "preview must not create the missing local catalog row")
			mu.Lock()
			assertions.Zero(writes)
			mu.Unlock()
			request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "catalog-"+string(tc.op)
			result, err := client.ControlInbox(t.Context(), request)
			requirements.NoError(err)
			requirements.NotNil(result.Receipt)
			assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
			assertions.ElementsMatch(tc.after, result.After.Tags)
			message, err := adapter.store.GetMessage(target.ItemID)
			requirements.NoError(err)
			assertions.Contains(message.Labels, "External")
			mu.Lock()
			assertions.Equal(1, writes)
			assertions.Equal(1, readbacks)
			mu.Unlock()
		})
	}
}
