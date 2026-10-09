package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

// The capability fake isolates source ownership and guarded archive saves.
type daemonTagGmailClient struct {
	*gmail.MockAPI

	tags func(context.Context, string, *emailtags.MessageTagChange) (*emailtags.MessageTagResult, error)
}

func (c *daemonTagGmailClient) MessageTags(ctx context.Context, id string, change *emailtags.MessageTagChange) (*emailtags.MessageTagResult, error) {
	return c.tags(ctx, id, change)
}

func TestDaemonMessageTagsGmailReconciles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread-1", "Tags")
	require.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "gmail-1", MessageType: "email"})
	require.NoError(err)
	tags := []string{"INBOX", "UNREAD"}
	writes, refreshes := 0, 0
	client := &daemonTagGmailClient{MockAPI: gmail.NewMockAPI()}
	client.tags = func(ctx context.Context, id string, change *emailtags.MessageTagChange) (*emailtags.MessageTagResult, error) {
		assert.Equal("gmail-1", id)
		result := &emailtags.MessageTagResult{Provider: "gmail", Tags: append([]string{}, tags...), Before: append([]string{}, tags...), AvailableTags: []emailtags.MessageTag{{ID: "Label_next", Name: "Next"}}}
		if change != nil {
			result.DryRun = change.DryRun
			if change.DryRun {
				result.Tags = []string{"INBOX", "UNREAD"}
				return result, nil
			}
			if len(tags) == 2 {
				writes++
				tags = append(tags, "Label_next")
				result.Tags = append([]string{}, tags...)
			}
		}
		result.Verified = true
		return result, nil
	}
	ctx := t.Context()
	a := &storeAPIAdapter{store: st, config: &config.Config{OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(ctx context.Context, source *store.Source) (gmail.API, error) {
		return client, nil
	}, draftCacheRefresh: func(context.Context, string) error { refreshes++; return nil }}
	change := &emailtags.MessageTagChange{Add: []string{"Label_next"}}
	got, err := a.MessageTags(ctx, mid, change, "", nil)
	require.NoError(err)
	assert.True(got.Verified)
	assert.Equal(mid, got.MessageID)
	msg, err := st.GetMessage(mid)
	require.NoError(err)
	assert.ElementsMatch([]string{"INBOX", "UNREAD", "Next"}, msg.Labels)
	assert.Equal(1, writes)
	assert.Equal(1, refreshes)
	// Source execution ownership must reject provider writes during a sync.
	execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
	require.NoError(err)
	_, err = a.MessageTags(ctx, mid, change, "", nil)
	var active *emailtags.MessageTagError
	require.ErrorAs(err, &active)
	assert.Equal("sync_active", active.Code)
	assert.Equal(1, writes)
	// Reads and previews write nothing, so they work during a sync.
	read, err := a.MessageTags(ctx, mid, nil, "", nil)
	require.NoError(err)
	assert.True(read.Verified)
	_, err = a.MessageTags(ctx, mid, &emailtags.MessageTagChange{Remove: []string{"Label_next"}, DryRun: true}, "", nil)
	require.NoError(err)
	assert.Equal(1, writes)
	require.NoError(execution.Release())
	client.tags = func(context.Context, string, *emailtags.MessageTagChange) (*emailtags.MessageTagResult, error) {
		result := &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"INBOX", "Label_other"}, Before: append([]string{}, tags...)}
		return result, emailtags.Failure("remote_unknown", "readback failed", result, nil)
	}
	uncertain, err := a.MessageTags(ctx, mid, change, "", nil)
	require.Error(err)
	require.NotNil(uncertain)
	assert.Equal([]string{"INBOX", "Label_other"}, uncertain.Tags)
	assert.False(uncertain.Verified)
	msg, err = st.GetMessage(mid)
	require.NoError(err)
	assert.ElementsMatch([]string{"INBOX", "UNREAD", "Next"}, msg.Labels)
	client.tags = func(context.Context, string, *emailtags.MessageTagChange) (*emailtags.MessageTagResult, error) {
		return &emailtags.MessageTagResult{Provider: "gmail", Tags: append([]string{}, tags...), AvailableTags: []emailtags.MessageTag{{ID: "Label_next", Name: "Next"}}, Verified: true}, nil
	}
	// A moved local identity must make persistence fail, retaining remote evidence.
	a.emailTagClientFactory = func(ctx context.Context, s *store.Source) (gmail.API, error) {
		_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET source_message_id=? WHERE id=?`), "changed-identity", mid)
		require.NoError(err)
		return client, nil
	}
	failed, err := a.MessageTags(ctx, mid, change, "", nil)
	require.Error(err)
	require.NotNil(failed)
	assert.True(failed.Verified)
	var failure *emailtags.MessageTagError
	require.ErrorAs(err, &failure)
	assert.Equal("remote_accepted_local_failed", failure.Code)
}

func TestDaemonMessageTagsRevokedGrantUsesDefaultFactory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokenJSON := fmt.Sprintf(`{"access_token":"expired","refresh_token":"synthetic-refresh","expiry":"2000-01-01T00:00:00Z","scopes":[%q]}`, oauth.ScopeGmailModify)
	tokenPath, restore := seedTokenEnv(t, tokenJSON)
	defer restore()
	cfg := testConfigValue()
	cfg.OAuth.ClientSecrets = filepath.Join(filepath.Dir(filepath.Dir(tokenPath)), "client_secret.json")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	t.Cleanup(provider.Close)
	var secrets map[string]map[string]any
	require.NoError(json.Unmarshal([]byte(fakeClientSecrets), &secrets))
	secrets["installed"]["token_uri"] = provider.URL
	encoded, err := json.Marshal(secrets)
	require.NoError(err)
	require.NoError(os.WriteFile(cfg.OAuth.ClientSecrets, encoded, 0600))
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", scopeEscalationAccount)
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "thread-1", "Tags")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID, SourceMessageID: "gmail-revoked", MessageType: "email",
	})
	require.NoError(err)
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	ctx = context.WithValue(ctx, oauth2.HTTPClient, provider.Client())
	a := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
	result, err := a.MessageTags(ctx, messageID, &emailtags.MessageTagChange{Add: []string{"Label_next"}}, "", nil)
	require.Error(err)
	assert.Nil(result)
	failure, ok := errors.AsType[*emailtags.MessageTagError](err)
	require.True(ok)
	assert.Equal("provider_unavailable", failure.Code)
	assert.Contains(failure.Message, "msgvault add-account")
}

func TestDaemonMessageTagsReadonlyGrantDoesNotWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokenPath, restore := seedTokenEnv(t, gmailReadonlyTokenJSON)
	defer restore()
	home := filepath.Dir(filepath.Dir(tokenPath))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	cfg.OAuth.ClientSecrets = filepath.Join(home, "client_secret.json")
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", scopeEscalationAccount)
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread-1", "Tags")
	require.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "gmail-1", MessageType: "email"})
	require.NoError(err)
	connects := 0
	a := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		connects++
		return &daemonTagGmailClient{MockAPI: gmail.NewMockAPI(), tags: func(context.Context, string, *emailtags.MessageTagChange) (*emailtags.MessageTagResult, error) {
			return &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"Label_next"}, DryRun: true}, nil
		}}, nil
	}}
	_, err = a.MessageTags(t.Context(), mid, &emailtags.MessageTagChange{Add: []string{"Label_next"}}, "", nil)
	require.Error(err)
	var failure *emailtags.MessageTagError
	require.ErrorAs(err, &failure)
	assert.Equal("insufficient_scope", failure.Code)
	assert.Zero(connects)
	preview, err := a.MessageTags(t.Context(), mid, &emailtags.MessageTagChange{Add: []string{"Label_next"}, DryRun: true}, "", nil)
	require.NoError(err)
	assert.True(preview.DryRun)
	assert.False(preview.Verified)
	assert.Equal(1, connects)
}

func TestDaemonMessageTagsValidationBeforeAcquisition(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		change         emailtags.MessageTagChange
	}{
		{"empty", "gmail", emailtags.MessageTagChange{}},
		{"atom", "imap", emailtags.MessageTagChange{Add: []string{"two words"}}},
		{"comma", "msmail", emailtags.MessageTagChange{Add: []string{"Next,Later"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(tc.provider, "owner@example.test")
			require.NoError(err)
			conv, err := st.EnsureConversation(source.ID, "thread", "Tags")
			require.NoError(err)
			id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "INBOX|1", MessageType: "email"})
			require.NoError(err)
			if tc.provider == "imap" {
				_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext) VALUES(?,?,?,?)`), source.ID, "INBOX", 77, 2)
				require.NoError(err)
				_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_message_memberships(source_id,mailbox,uidvalidity,uid,message_id,flags) VALUES(?,?,?,?,?,?)`), source.ID, "INBOX", 77, 1, id, `[]`)
				require.NoError(err)
			}
			execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
			require.NoError(err)
			defer func() { require.NoError(execution.Release()) }()
			gate := api.NewSerialOperationGate()
			release, ok := gate.BeginLabeledWorkContext(t.Context(), "sync")
			require.True(ok)
			defer release()
			connects := 0
			a := &storeAPIAdapter{store: st, config: &config.Config{}, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
				connects++
				return nil, errors.New("unexpected credentials")
			}}
			srv := api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: a.logger, OperationGate: gate})
			body, err := json.Marshal(tc.change)
			require.NoError(err)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/messages/%d/tags", id), strings.NewReader(string(body))).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, req)
			assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
			assert.Contains(response.Body.String(), `"error":"invalid_tag"`)
			assert.Zero(connects)
		})
	}
}

func TestDaemonMessageTagsIMAPNativeKeywords(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{MessagesPerMailbox: map[string]int{"INBOX": 1}, Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapUIDPlus: {}}})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(err)
	port, err := strconv.Atoi(portText)
	require.NoError(err)
	cfg := &imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
	raw, err := imapclient.DialInsecure(addr, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = raw.Close() })
	require.NoError(raw.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	selected, err := raw.Select("INBOX", nil).Wait()
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", cfg.Identifier())
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread-1", "Tags")
	require.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "INBOX|1", MessageType: "email"})
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext) VALUES(?,?,?,?)`), source.ID, "INBOX", selected.UIDValidity, 2)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_message_memberships(source_id,mailbox,uidvalidity,uid,message_id,flags) VALUES(?,?,?,?,?,?)`), source.ID, "INBOX", selected.UIDValidity, 1, mid, `[]`)
	require.NoError(err)
	a := &storeAPIAdapter{store: st, config: &config.Config{}, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		return imaplib.NewClient(cfg, testutil.IMAPTestPassword), nil
	}}
	result, err := a.MessageTags(t.Context(), mid, &emailtags.MessageTagChange{Add: []string{"Next"}}, "", nil)
	require.NoError(err)
	assert.True(result.Verified)
	var encoded string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT flags FROM imap_message_memberships WHERE message_id=?`), mid).Scan(&encoded))
	assert.NotEqual("[]", encoded)
}
