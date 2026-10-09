package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

const (
	remoteClientTestOwnerKey  = "fixture-owner-api-key"
	remoteClientTestReaderKey = "fixture-reader-api-key"
)

type remoteClientFixture struct {
	*cliOriginalFixture
}

func newRemoteClientFixture(t *testing.T, collectionsWrite bool) *remoteClientFixture {
	t.Helper()
	gate := NewSerialOperationGate()
	home := t.TempDir()
	f := newCLIOriginalFixture(t, func(options *ServerOptions) {
		options.Config = &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Server: config.ServerConfig{
			APIPort: 8080,
			APIKey:  remoteClientTestOwnerKey,
			RemoteClients: []config.RemoteClientConfig{{
				ClientID: "fixture-reader", CollectionsWrite: collectionsWrite, APIKey: remoteClientTestReaderKey,
			}},
		}}
		options.OperationGate = gate
	})
	return &remoteClientFixture{cliOriginalFixture: f}
}

func (f *remoteClientFixture) do(t *testing.T, key, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, reader)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("X-Api-Key", key)
	}
	// Same-host HTTPS proxies reach the daemon over loopback.
	r.RemoteAddr = "127.0.0.1:4242"
	w := httptest.NewRecorder()
	f.srv.Router().ServeHTTP(w, r)
	return w
}

func TestRemoteClientRoutes(t *testing.T) { //nolint:paralleltest // swaps the package-level operationGateWaitLimit
	require := require.New(t)
	assert := assert.New(t)
	reader := newRemoteClientFixture(t, false)
	writer := newRemoteClientFixture(t, true)
	id := strconv.FormatInt(reader.withRaw, 10)
	create := `{"name":"reader","accounts":["owner@example.com"]}`
	type routeCase struct {
		f                         *remoteClientFixture
		key, method, target, body string
		want                      int
	}
	check := func(cases []routeCase) {
		for _, tc := range cases {
			w := tc.f.do(t, tc.key, tc.method, tc.target, tc.body)
			assert.Equal(tc.want, w.Code, "%s %s: %s", tc.method, tc.target, w.Body.String())
		}
	}
	var cases []routeCase
	for _, target := range []string{
		"/health",
		"/api/v1/health",
		"/api/v1/cli/stats",
		"/api/v1/cli/search?q=Original",
		"/api/v1/cli/accounts",
		"/api/v1/cli/cache-stats",
		"/api/v1/cli/message?id=" + id,
		"/api/v1/cli/message/original?id=" + id,
		"/api/v1/cli/message/thread?id=" + id + "&all=true",
		"/api/v1/cli/message/raw?id=" + id,
		"/api/v1/cli/collections",
		"/api/v1/cli/identities",
	} {
		cases = append(cases, routeCase{reader, remoteClientTestReaderKey, http.MethodGet, target, "", http.StatusOK})
	}
	sources := `{"accounts":["owner@example.com"]}`
	check(append(cases,
		// Huma routes deny by default; other paths are refused before routing.
		routeCase{reader, remoteClientTestReaderKey, http.MethodHead, "/health", "", http.StatusOK},
		routeCase{reader, remoteClientTestReaderKey, http.MethodGet, "/", "", http.StatusForbidden},
		routeCase{reader, remoteClientTestReaderKey, http.MethodPost, "/health", "", http.StatusForbidden},
		routeCase{reader, remoteClientTestReaderKey, http.MethodGet, "/api/v1/settings", "", http.StatusForbidden},
		routeCase{reader, remoteClientTestReaderKey, http.MethodGet, "/api/session", "", http.StatusForbidden},
		routeCase{writer, remoteClientTestReaderKey, http.MethodPost, "/api/v1/cli/collections", create, http.StatusOK},
		routeCase{writer, remoteClientTestReaderKey, http.MethodGet, "/api/v1/cli/collection?name=reader", "", http.StatusOK},
		routeCase{writer, remoteClientTestReaderKey, http.MethodPatch, "/api/v1/cli/collections/reader/sources", sources, http.StatusOK},
		routeCase{writer, remoteClientTestReaderKey, http.MethodDelete, "/api/v1/cli/collections/reader/sources", sources, http.StatusOK},
		routeCase{writer, remoteClientTestReaderKey, http.MethodDelete, "/api/v1/cli/collections/reader", "", http.StatusOK},
	))

	// Forbidden writes bypass the busy gate; permitted reader writes hide its holder.
	orig := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = orig })
	for _, f := range []*remoteClientFixture{reader, writer} {
		w := f.do(t, remoteClientTestOwnerKey, http.MethodPost, backupFreezeBeginPath, "")
		require.Equal(http.StatusOK, w.Code, w.Body.String())
		var freeze backupFreezeBeginResponse
		require.NoError(json.Unmarshal(w.Body.Bytes(), &freeze))
		t.Cleanup(func() {
			w := f.do(t, remoteClientTestOwnerKey, http.MethodPost, backupFreezeEndPath, `{"token":"`+freeze.Token+`"}`)
			require.Equal(http.StatusOK, w.Code, w.Body.String())
		})
	}
	w := writer.do(t, remoteClientTestReaderKey, http.MethodPost, "/api/v1/cli/collections", create)
	require.Equal(http.StatusServiceUnavailable, w.Code, w.Body.String())
	var busy ErrorResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &busy))
	assert.Equal("another operation is running", busy.Message)
	w = writer.do(t, remoteClientTestOwnerKey, http.MethodPost, "/api/v1/cli/collections", create)
	require.Equal(http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(w.Body.String(), "backup freeze has been running for")
	check([]routeCase{
		{reader, remoteClientTestReaderKey, http.MethodPost, "/api/v1/cli/sync", `{}`, http.StatusForbidden},
		{reader, remoteClientTestReaderKey, http.MethodPost, "/api/v1/cli/collections", create, http.StatusForbidden},
		{writer, remoteClientTestReaderKey, http.MethodPost, "/api/v1/cli/collections", create, http.StatusServiceUnavailable},
	})
}

type sizedAttachmentStore struct{ size int64 }

func (s sizedAttachmentStore) OpenStream(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader("attachment")), s.size, nil
}

func TestRemoteClientLimits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRemoteClientFixture(t, false)
	id := strconv.FormatInt(f.withRaw, 10)

	w := f.do(t, remoteClientTestReaderKey, http.MethodGet, "/api/v1/cli/search?q=Original&limit=501", "")
	assert.Equal(http.StatusBadRequest, w.Code, w.Body.String())
	w = f.do(t, remoteClientTestOwnerKey, http.MethodGet, "/api/v1/cli/search?q=Original&limit=501", "")
	assert.Equal(http.StatusOK, w.Code, w.Body.String())

	attachment := "/api/v1/cli/attachment?content_hash=" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		size int64
		want int
	}{
		{size: 10, want: http.StatusOK},
		{size: -1, want: http.StatusRequestEntityTooLarge},
		{size: remoteAttachmentBytes + 1, want: http.StatusRequestEntityTooLarge},
	} {
		f.srv.blobStore = sizedAttachmentStore{size: tc.size}
		w := f.do(t, remoteClientTestReaderKey, http.MethodGet, attachment, "")
		assert.Equal(tc.want, w.Code, "attachment size %d", tc.size)
	}

	var err error
	// Latin-1 doubles when decoded, so the body fallback exceeds the limit while the raw MIME fits.
	raw := []byte("From: owner@example.com\r\nSubject: Original\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\n" +
		strings.Repeat("\xe9", int(remoteMessageBytes)/2+1))
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE message_raw SET raw_data = ?, compression = NULL WHERE message_id = ?`), raw, f.withRaw)
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE message_bodies SET body_text = NULL, body_html = NULL WHERE message_id = ?`), f.withRaw)
	require.NoError(err)

	w = f.do(t, remoteClientTestReaderKey, http.MethodGet, "/api/v1/cli/message?id="+id, "")
	assert.Equal(http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
	w = f.do(t, remoteClientTestReaderKey, http.MethodGet, "/api/v1/cli/message/raw?id="+id, "")
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	assert.Len(w.Body.Bytes(), len(raw), "raw export ignores the decoded body size")

	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE message_raw SET raw_data = ?, compression = NULL WHERE message_id = ?`),
		[]byte(strings.Repeat("x", int(remoteMessageBytes)+1)), f.withRaw)
	require.NoError(err)
	for _, target := range []string{"/api/v1/cli/message/raw?id=" + id, "/api/v1/cli/message/original?id=" + id} {
		w := f.do(t, remoteClientTestReaderKey, http.MethodGet, target, "")
		assert.Equal(http.StatusRequestEntityTooLarge, w.Code, "%s: %s", target, w.Body.String())
	}
	w = f.do(t, remoteClientTestOwnerKey, http.MethodGet, "/api/v1/cli/message/raw?id="+id, "")
	assert.Equal(http.StatusOK, w.Code)

	var convID int64
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT conversation_id FROM messages WHERE id = ?`), f.withRaw).Scan(&convID))
	for i := 2; i <= query.ThreadMaxLimit; i++ {
		_, err := f.st.PersistMessage(&store.MessagePersistData{Message: &store.Message{
			SourceID: f.sourceID, ConversationID: convID, SourceMessageID: fmt.Sprintf("thread-member-%d", i),
			MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 2, 1, 0, 0, i, 0, time.UTC), Valid: true},
		}})
		require.NoError(err)
	}
	w = f.do(t, remoteClientTestReaderKey, http.MethodGet, "/api/v1/cli/message/thread?id="+id+"&all=true", "")
	assert.Equal(http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
	w = f.do(t, remoteClientTestOwnerKey, http.MethodGet, "/api/v1/cli/message/thread?id="+id+"&all=true", "")
	assert.Equal(http.StatusOK, w.Code)
}

func TestRemoteClientAuthentication(t *testing.T) {
	f := newRemoteClientFixture(t, false)
	f.srv.cfg.Server.RemoteClients[0].APIKey = ""
	w := f.do(t, "", http.MethodGet, "/api/v1/cli/stats", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "empty configured key never matches: %s", w.Body.String())
}
