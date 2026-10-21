package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
	matrixsource "go.kenn.io/msgvault/internal/matrix"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

func captureSyncOutages(t *testing.T, cfg *config.Config, sourceType string) (context.Context, *invocation, *int) {
	t.Helper()
	cfg.Analytics.AutoBuildCache = false
	state := testInvocationWithConfig(cfg)
	events := new(int)
	state.syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
		assert.Equal(t, "sync_failed", event)
		assert.Equal(t, sourceType, props["sync_source"])
		*events++
		return nil
	}}
	return withInvocation(t.Context(), state), state, events
}

func TestScheduledSlackMembershipErrorAfterGoodRead(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	cfg := lifecycleTestConfig(t.TempDir())
	ctx, state, events := captureSyncOutages(t, cfg, "slack")
	src, err := st.GetOrCreateSource("slack", "T01:U01")
	require.NoError(err)
	require.NoError(slack.SaveToken(cfg.TokensDir(), "T01", "example", "U01", "synthetic"))
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch strings.TrimPrefix(r.URL.Path, "/api/") {
		case "auth.test":
			body = `{"ok":true,"team_id":"T01","user_id":"U01"}`
		case "users.list":
			body = `{"ok":true,"members":[{"id":"U01","name":"user","profile":{"email":"user@example.com"}}]}`
		case "users.conversations", "conversations.list":
			body = `{"ok":true,"channels":[{"id":"C01","name":"general","is_channel":true,"is_member":true}]}`
		case "conversations.members":
			body = `{"ok":false,"error":"missing_scope"}`
		case "conversations.history":
			body = `{"ok":true,"messages":[{"type":"message","user":"U01","text":"Synthetic message","ts":"1700000000.000001"}],"has_more":false}`
		default:
			assert.Fail("unexpected Slack request", "%s", r.URL.Path)
			body = `{"ok":false,"error":"missing_scope"}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Oauth-Scopes": []string{"channels:read,channels:history,users:read,users:read.email"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	state.syncFailures.Record(ctx, "slack", "T01:U01", errors.New("prior outage"))
	err = runConfiguredSlackSync(ctx, st)
	require.Error(err)
	count, err := st.CountMessagesForSource(src.ID)
	require.NoError(err)
	require.Positive(count, "membership failure still allows message reads")
	assert.Equal(1, *events, "the partial pass starts no outage")
	state.syncFailures.Record(ctx, "slack", "T01:U01", errors.New("later outage"))
	assert.Equal(2, *events, "the partial pass cleared the previous outage")
}

func TestScheduledCalendarPartialReadClearsOutage(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	cfg := lifecycleTestConfig(t.TempDir())
	ctx, state, events := captureSyncOutages(t, cfg, "gcal")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users/me/calendarList":
			_, _ = io.WriteString(w, `{"items":[{"id":"good","accessRole":"owner"},{"id":"bad","accessRole":"owner"}]}`)
		case "/calendars/good/events":
			_, _ = io.WriteString(w, `{"items":[],"nextSyncToken":"good-token"}`)
		case "/calendars/bad/events":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"denied"}}`)
		default:
			assert.Fail("unexpected calendar request", "%s", r.URL.Path)
		}
	}))
	defer server.Close()
	build := func(context.Context, string, string, bool, ...bool) (gcal.API, error) {
		return gcal.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic"}), gcal.WithBaseURL(server.URL)), nil
	}
	state.syncFailures.Record(ctx, "gcal", "user@example.com", errors.New("prior outage"))
	require.Error(runConfiguredGCalSyncWithClientBuilder(ctx, st, config.GCalSource{Email: "user@example.com"}, build))
	assert.Equal(1, *events)
	state.syncFailures.Record(ctx, "gcal", "user@example.com", errors.New("later outage"))
	assert.Equal(2, *events, "a successful empty calendar read clears the outage")
}

func TestScheduledMatrixPartialReadClearsOutage(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	cfg := lifecycleTestConfig(t.TempDir())
	ctx, state, events := captureSyncOutages(t, cfg, "matrix")
	src, err := st.GetOrCreateSource("matrix", "@user:example.org")
	require.NoError(err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sync"):
			_, _ = io.WriteString(w, `{"next_batch":"next","rooms":{"join":{"!a:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.message","event_id":"$good","sender":"@user:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"Synthetic message"}}]}},"!z:example.org":{"state":{"events":[]},"timeline":{"events":[]}}}}}`)
		case strings.HasSuffix(r.URL.Path, "/account_data/m.direct"):
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "!a:example.org") && strings.HasSuffix(r.URL.Path, "/joined_members"):
			_, _ = io.WriteString(w, `{"joined":{}}`)
		case strings.Contains(r.URL.Path, "!z:example.org"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"errcode":"M_FORBIDDEN","error":"denied"}`)
		default:
			assert.Fail("unexpected Matrix request", "%s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	require.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), matrixsource.Credentials{Homeserver: server.URL, UserID: src.Identifier, DeviceID: "device", AccessToken: "synthetic"}))
	state.syncFailures.Record(ctx, "matrix", src.Identifier, errors.New("prior outage"))
	require.Error(runMatrixSync(ctx, st, cfg, "", false, nil, io.Discard))
	count, err := st.CountMessagesForSource(src.ID)
	require.NoError(err)
	require.Positive(count)
	assert.Equal(1, *events)
	state.syncFailures.Record(ctx, "matrix", src.Identifier, errors.New("later outage"))
	assert.Equal(2, *events)
}

func TestScheduledTeamsAllMessageFetchesFail(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Microsoft.ClientID = "synthetic"
	ctx, state, events := captureSyncOutages(t, cfg, "teams")
	const email = "user@example.com"
	_, err := st.GetOrCreateSource("teams", email)
	require.NoError(err)
	mgr := microsoft.NewGraphManager(cfg.Microsoft.ClientID, "common", cfg.Microsoft.EffectiveRedirectURI(), cfg.TokensDir(), state.logger)
	require.NoError(os.MkdirAll(cfg.TokensDir(), 0700))
	require.NoError(os.WriteFile(mgr.TokenPath(email), []byte(`{"access_token":"synthetic"}`), 0600))
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	fetches := 0
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		body, status := `{"value":[]}`, http.StatusOK
		switch r.URL.Path {
		case "/v1.0/me/chats":
			body = `{"value":[{"id":"chat-1","chatType":"group"}]}`
		case "/v1.0/me":
			body = `{"id":"self","userPrincipalName":"user@example.com"}`
		case "/v1.0/me/chats/chat-1/messages":
			fetches++
			status, body = http.StatusForbidden, `{"error":{"code":"Forbidden","message":"denied"}}`
		case "/v1.0/chats/chat-1/members", "/v1.0/me/joinedTeams":
		case "/v1.0/me/chats/48:notes/messages":
			status = http.StatusNotFound
		default:
			assert.Fail("unexpected Teams request", "%s", r.URL.Path)
			status = http.StatusBadRequest
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	for range 2 {
		_ = runScheduledSync(ctx, email, st, nil, state)
	}
	require.Positive(fetches)
	assert.Equal(1, *events, "failed message reads start one outage even when discovery succeeds")
}

func TestScheduledCalendarDeniedTokenReason(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.AutoBuildCache = false
	state := testInvocationWithConfig(cfg)
	var reasons []any
	state.syncFailures = &telemetry.SyncFailures{Capture: func(_ string, props map[string]any) error { reasons = append(reasons, props["reason"]); return nil }}
	ctx := withInvocation(t.Context(), state)
	err := runConfiguredGCalSyncWithClientBuilder(ctx, st, config.GCalSource{Email: "user@example.com"},
		func(context.Context, string, string, bool, ...bool) (gcal.API, error) {
			return nil, calendarTokenActionError("user@example.com")
		})
	require.Error(err)
	assert.Equal([]any{"auth"}, reasons)
}
