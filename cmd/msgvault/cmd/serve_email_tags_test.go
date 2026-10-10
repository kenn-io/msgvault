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

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

// The transport exercises the production Gmail client through its native REST contract.
type tagGmailTransport func(*http.Request) (*http.Response, error)

func (f tagGmailTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
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
	transport := tagGmailTransport(func(r *http.Request) (*http.Response, error) {
		body := `{}`
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels"):
			body = `{"labels":[{"id":"Label_next","name":"Next","type":"user"}]}`
		case strings.HasSuffix(r.URL.Path, "/modify"):
			writes++
			var change struct {
				Add []string `json:"addLabelIds"`
			}
			if decodeErr := json.NewDecoder(r.Body).Decode(&change); decodeErr != nil {
				assert.Fail("decode Gmail tag change", "%v", decodeErr)
				return nil, io.ErrUnexpectedEOF
			}
			tags = append(tags, change.Add...)
		}
		if strings.Contains(r.URL.Path, "/messages/") {
			encoded, encodeErr := json.Marshal(map[string]any{"id": "gmail-1", "labelIds": tags})
			if encodeErr != nil {
				assert.Fail("encode Gmail metadata", "%v", encodeErr)
				return nil, io.ErrUnexpectedEOF
			}
			body = string(encoded)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient, &http.Client{Transport: transport})
	a := &storeAPIAdapter{store: st, config: &config.Config{OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(ctx context.Context, source *store.Source) (gmail.API, error) {
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}), gmail.WithTransport(transport)), nil
	}, draftCacheRefresh: func(context.Context, string) error { refreshes++; return nil }}
	change := &emailtags.Change{Add: []string{"Label_next"}, DryRun: true}
	preview, err := a.MessageTags(ctx, mid, change, "")
	require.NoError(err)
	assert.True(preview.DryRun)
	assert.Zero(writes)
	assert.Zero(refreshes)
	change.DryRun = false
	got, err := a.MessageTags(ctx, mid, change, "")
	require.NoError(err)
	assert.True(got.Verified)
	assert.Equal(mid, got.MessageID)
	msg, err := st.GetMessage(mid)
	require.NoError(err)
	assert.ElementsMatch([]string{"INBOX", "UNREAD", "Next"}, msg.Labels)
	assert.Equal(1, writes)
	assert.Equal(1, refreshes)
	_, err = a.MessageTags(ctx, mid, change, "")
	require.NoError(err)
	assert.Equal(1, writes)
	// Source execution ownership must reject provider writes during a sync.
	execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
	require.NoError(err)
	_, err = a.MessageTags(ctx, mid, change, "")
	var active *emailtags.Error
	require.ErrorAs(err, &active)
	assert.Equal("sync_active", active.Code)
	assert.Equal(1, writes)
	// Reads and previews write nothing, so they work during a sync.
	read, err := a.MessageTags(ctx, mid, nil, "")
	require.NoError(err)
	assert.True(read.Verified)
	_, err = a.MessageTags(ctx, mid, &emailtags.Change{Remove: []string{"Label_next"}, DryRun: true}, "")
	require.NoError(err)
	assert.Equal(1, writes)
	require.NoError(execution.Release())
	// A moved local identity must make persistence fail, retaining remote evidence.
	a.emailTagClientFactory = func(ctx context.Context, s *store.Source) (gmail.API, error) {
		_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET source_message_id=? WHERE id=?`), "changed-identity", mid)
		require.NoError(err)
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}), gmail.WithTransport(transport)), nil
	}
	failed, err := a.MessageTags(ctx, mid, change, "")
	require.Error(err)
	require.NotNil(failed)
	assert.True(failed.Verified)
	var failure *emailtags.Error
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
	refreshRequests := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshRequests++
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
	result, err := a.MessageTags(ctx, messageID, &emailtags.Change{Add: []string{"Label_next"}}, "")
	require.Error(err)
	assert.Nil(result)
	failure, ok := errors.AsType[*emailtags.Error](err)
	require.True(ok)
	assert.Equal("provider_unavailable", failure.Code)
	assert.Contains(failure.Message, "authorization has expired or been revoked")
	credentialErr, ok := errors.AsType[*provideridentity.GmailCredentialError](err)
	require.True(ok, "the default factory must return a typed credential failure")
	assert.Contains(credentialErr.Remediation(), "msgvault add-account")
	assert.GreaterOrEqual(refreshRequests, 1)
	assert.LessOrEqual(refreshRequests, 2)
	after, err := os.ReadFile(tokenPath)
	require.NoError(err)
	assert.JSONEq(tokenJSON, string(after))
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
	before, err := os.ReadFile(tokenPath)
	require.NoError(err)
	connects := 0
	a := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		connects++
		return nil, errors.New("must not connect")
	}}
	_, err = a.MessageTags(t.Context(), mid, &emailtags.Change{Add: []string{"Label_next"}}, "")
	require.Error(err)
	var failure *emailtags.Error
	require.ErrorAs(err, &failure)
	assert.Equal("insufficient_scope", failure.Code)
	assert.Zero(connects)
	after, err := os.ReadFile(tokenPath)
	require.NoError(err)
	assert.Equal(before, after)
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
	_, err = raw.Store(imapapi.UIDSetNum(1), &imapapi.StoreFlags{Op: imapapi.StoreFlagsAdd, Flags: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, "Old", "Unrelated"}}, nil).Collect()
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
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_message_memberships(source_id,mailbox,uidvalidity,uid,message_id,flags) VALUES(?,?,?,?,?,?)`), source.ID, "INBOX", selected.UIDValidity, 1, mid, `["Old"]`)
	require.NoError(err)
	a := &storeAPIAdapter{store: st, config: &config.Config{}, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		return imaplib.NewClient(cfg, testutil.IMAPTestPassword), nil
	}}
	result, err := a.MessageTags(t.Context(), mid, &emailtags.Change{Add: []string{"Next"}, Remove: []string{"old"}}, "")
	require.NoError(err)
	assert.True(result.Verified)
	assert.ElementsMatch([]string{"next", "unrelated"}, result.Tags)
	var encoded string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT flags FROM imap_message_memberships WHERE message_id=?`), mid).Scan(&encoded))
	var flags []string
	require.NoError(json.Unmarshal([]byte(encoded), &flags))
	assert.ElementsMatch([]string{"next", "unrelated", "\\Seen", "\\Flagged"}, flags)
}
