package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/requestsign"
)

const ingressFixtureAPIKey = "fixture-client-api-key-01234567890123456789"

func ingressFixture(t *testing.T, grants ...string) (*Server, http.Handler, *cliOriginalFixture) {
	t.Helper()
	f := newCLIOriginalFixture(t)
	home := t.TempDir()
	require.NoError(t, fileutil.SecureChmod(home, 0o700))
	apiFile := filepath.Join(home, "api-key")
	secretFile := filepath.Join(home, "secret")
	stateFile := filepath.Join(home, "replay.json")
	require.NoError(t, fileutil.SecureWriteFile(apiFile, []byte(ingressFixtureAPIKey), 0o600))
	require.NoError(t, fileutil.SecureWriteFile(secretFile, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 64))), 0o600))
	require.NoError(t, requestsign.InitReplayState(stateFile))
	f.srv.cfg.HomeDir = home
	f.srv.cfg.Data.DataDir = home
	f.srv.cfg.Server.APIKey = "fixture-private-owner-api-key"
	f.srv.cfg.Server.RemoteIngress = config.RemoteIngressConfig{
		Enabled: true, ExternalURL: "https://archive.example.test/msgvault", ReplayStateFile: stateFile,
		Clients: []config.RemoteIngressClient{{ClientID: "fixture-reader", APIKeyFile: apiFile, Grants: grants, Keys: []config.RequestSigningKey{{KeyID: "reader-1", SecretFile: secretFile}}}},
	}
	h, err := f.srv.RestrictedRouter()
	require.NoError(t, err)
	require.NoError(t, f.srv.remoteIngress.guard.Close())
	f.srv.remoteIngress.guard, err = requestsign.OpenReplayGuard(stateFile, time.Now().Add(-40*time.Second), 10000)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.srv.Shutdown(t.Context()) })
	return f.srv, h, f
}

func ingressRequest(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r, err := http.NewRequest(method, "https://archive.example.test/msgvault"+path, strings.NewReader(body))
	require.NoError(t, err)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-Api-Key", ingressFixtureAPIKey)
	require.NoError(t, requestsign.Sign(r, bytes.Repeat([]byte{0x42}, 64), "reader-1", time.Now(), requestsign.NewNonce()))
	defer func() { _ = r.Body.Close() }()
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/msgvault")
	r.RequestURI = r.URL.RequestURI()
	r.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRestrictedIngressValidSignedReadsAndDeniedRoutes(t *testing.T) {
	_, h, f := ingressFixture(t)
	for _, path := range []string{
		"/api/v1/health", "/api/v1/cli/stats", "/api/v1/cli/accounts", "/api/v1/cli/message?id=" + strconv.FormatInt(f.withRaw, 10),
		"/api/v1/cli/message/thread?id=" + strconv.FormatInt(f.withRaw, 10), "/api/v1/cli/message/original?id=" + strconv.FormatInt(f.withRaw, 10),
	} {
		w := ingressRequest(t, h, http.MethodGet, path, "")
		assert.Equal(t, http.StatusOK, w.Code, "%s %s", path, w.Body.String())
	}
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/", ""}, {"GET", "/api/v1/accounts", ""}, {"GET", "/api/v1/agent-tokens", ""},
		{"GET", "/debug/pprof/", ""}, {"GET", "/openapi.json", ""}, {"POST", "/api/v1/cli/run", `{"args":["stats"]}`},
		{"POST", "/api/v1/cli/sync", "{}"}, {"POST", "/api/v1/cli/collections", `{"name":"fixture"}`},
	} {
		w := ingressRequest(t, h, tc.method, tc.path, tc.body)
		assert.Equal(t, http.StatusForbidden, w.Code, "%s %s", tc.path, w.Body.String())
	}
	w := httptest.NewRecorder()
	unsigned := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	unsigned.RemoteAddr = "127.0.0.1:12345"
	h.ServeHTTP(w, unsigned)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRestrictedIngressCollectionGrantAndMainIsolation(t *testing.T) {
	assertions := assert.New(t)

	s, h, _ := ingressFixture(t, "collections-write")
	w := ingressRequest(t, h, http.MethodPost, "/api/v1/cli/collections", `{"name":"fixture-collection","accounts":["owner@example.com"]}`)
	assertions.Equal(http.StatusOK, w.Code, w.Body.String())
	w = ingressRequest(t, h, http.MethodDelete, "/api/v1/cli/collections/fixture-collection", "")
	assertions.Equal(http.StatusOK, w.Code, w.Body.String())
	privateRequest := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	privateRequest.Header.Set("X-Api-Key", ingressFixtureAPIKey)
	w = httptest.NewRecorder()
	s.Router().ServeHTTP(w, privateRequest)
	assertions.Equal(http.StatusUnauthorized, w.Code)
	w = ingressRequest(t, h, http.MethodPost, "/api/v1/cli/run", `{"args":["delete"]}`)
	assertions.Equal(http.StatusForbidden, w.Code)
}

func TestRestrictedIngressAttachmentExactBytes(t *testing.T) {
	requirements := require.New(t)

	_, h, f := ingressFixture(t)
	content := bytes.Repeat([]byte("synthetic attachment\n"), 100000)
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(f.srv.cfg.AttachmentsDir(), hash[:2], hash)
	requirements.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	requirements.NoError(os.WriteFile(path, content, 0o600))
	w := ingressRequest(t, h, http.MethodGet, "/api/v1/cli/attachment?content_hash="+hash, "")
	requirements.Equal(http.StatusOK, w.Code)
	got, err := io.ReadAll(w.Result().Body)
	requirements.NoError(err)
	assert.Equal(t, content, got)
}

type contextInspectingAttachmentStore struct {
	content         []byte
	contextDeadline bool
}

func (s *contextInspectingAttachmentStore) OpenStream(ctx context.Context, _ string) (io.ReadCloser, int64, error) {
	_, s.contextDeadline = ctx.Deadline()
	return io.NopCloser(bytes.NewReader(s.content)), int64(len(s.content)), nil
}

func TestRestrictedIngressAttachmentStreamHasNoTotalRequestDeadline(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	s, h, _ := ingressFixture(t)
	content := []byte("streamed attachment")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	store := &contextInspectingAttachmentStore{content: content}
	s.blobStore = store

	w := ingressRequest(t, h, http.MethodGet, "/api/v1/cli/attachment?content_hash="+hash, "")
	requirements.Equal(http.StatusOK, w.Code)
	assertions.False(store.contextDeadline)
	assertions.Equal(content, w.Body.Bytes())
}

type writeDeadlineRecorder struct {
	*httptest.ResponseRecorder

	deadlines []time.Time
}

func (w *writeDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func TestAttachmentStreamResponseWriterRefreshesIdleDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)

		recorder := &writeDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		_, responseWriter, cancel := newAttachmentStreamResponseWriter(t.Context(), recorder)
		defer cancel()

		started := time.Now()
		_, err := io.WriteString(responseWriter, "first chunk")
		requirements.NoError(err)
		firstDeadline := recorder.deadlines[0]
		time.Sleep(10 * time.Millisecond)
		_, err = io.WriteString(responseWriter, "second chunk")
		requirements.NoError(err)
		secondDeadline := recorder.deadlines[1]

		assertions.True(firstDeadline.After(started))
		assertions.True(secondDeadline.After(firstDeadline))
		assertions.Equal("first chunksecond chunk", recorder.Body.String())
	})
}

type ingressReadSpy struct{ reads int }

func (s *ingressReadSpy) Read(_ []byte) (int, error) { s.reads++; return 0, io.EOF }
func (s *ingressReadSpy) Close() error               { return nil }

func TestRestrictedIngressAuthenticatesBeforeReadingBody(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	_, h, _ := ingressFixture(t)
	r, err := http.NewRequest(http.MethodPost, "https://archive.example.test/msgvault/api/v1/cli/collections", strings.NewReader("{}"))
	requirements.NoError(err)
	r.Header.Set("X-Api-Key", ingressFixtureAPIKey)
	requirements.NoError(requestsign.Sign(r, bytes.Repeat([]byte{0x42}, 64), "reader-1", time.Now(), requestsign.NewNonce()))
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/msgvault")
	r.RequestURI = r.URL.RequestURI()
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Signature", "sig1=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:")
	spy := &ingressReadSpy{}
	r.Body = spy
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertions.Equal(http.StatusUnauthorized, w.Code)
	assertions.Zero(spy.reads)
}

func TestRestrictedIngressRejectsUntrustedPeerAndSpoofedPrefix(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	_, h, _ := ingressFixture(t)
	r, err := http.NewRequest(http.MethodGet, "https://archive.example.test/msgvault/api/v1/health", nil)
	requirements.NoError(err)
	r.Header.Set("X-Api-Key", ingressFixtureAPIKey)
	requirements.NoError(requestsign.Sign(r, bytes.Repeat([]byte{0x42}, 64), "reader-1", time.Now(), requestsign.NewNonce()))
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/msgvault")
	r.RequestURI = r.URL.RequestURI()
	r.RemoteAddr = "192.0.2.1:12345"
	r.Header.Set("Forwarded", "host=archive.example.test;proto=https")
	r.Header.Set("X-Forwarded-Prefix", "/msgvault")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertions.Equal(http.StatusForbidden, w.Code)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Forwarded", "host=attacker.example.test;proto=http")
	r.Header.Set("X-Forwarded-Prefix", "/admin")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertions.Equal(http.StatusOK, w.Code, w.Body.String())
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertions.Equal(http.StatusUnauthorized, w.Code, "a valid request cannot be replayed")
}

func TestRestrictedIngressBoundsRealArchiveReads(t *testing.T) {
	_, h, f := ingressFixture(t)
	_, err := f.st.DB().Exec(f.st.Rebind(`INSERT INTO message_bodies (message_id,body_text,body_html) VALUES (?,?,?) ON CONFLICT(message_id) DO UPDATE SET body_text=excluded.body_text`), f.withRaw, strings.Repeat("é", (8<<20)+1), "")
	require.NoError(t, err)
	w := ingressRequest(t, h, http.MethodGet, "/api/v1/cli/message?id="+strconv.FormatInt(f.withRaw, 10), "")
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
	w = ingressRequest(t, h, http.MethodGet, "/api/v1/cli/search?q=subject:fixture&limit=501", "")
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestRestrictedIngressNativeCredentialCannotBecomeOwner(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	s, h, _ := ingressFixture(t, "collections-write")
	for _, extra := range []http.Header{
		{"Authorization": {"Bearer fixture-owner"}},
		{"Cookie": {"session=fixture"}},
		{"X-Msgvault-Agent-Token": {"fixture-token"}},
	} {
		r, err := http.NewRequest(http.MethodGet, "https://archive.example.test/msgvault/api/v1/health", nil)
		requirements.NoError(err)
		r.Header.Set("X-Api-Key", ingressFixtureAPIKey)
		requirements.NoError(requestsign.Sign(r, bytes.Repeat([]byte{0x42}, 64), "reader-1", time.Now(), requestsign.NewNonce()))
		maps.Copy(r.Header, extra)
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/msgvault")
		r.RequestURI = r.URL.RequestURI()
		r.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assertions.Equal(http.StatusUnauthorized, w.Code, "extra authentication must fail closed")
	}
	r, err := http.NewRequest(http.MethodGet, "https://archive.example.test/msgvault/api/v1/health", nil)
	requirements.NoError(err)
	r.Header.Set("X-Api-Key", s.cfg.Server.APIKey)
	requirements.NoError(requestsign.Sign(r, bytes.Repeat([]byte{0x42}, 64), "reader-1", time.Now(), requestsign.NewNonce()))
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/msgvault")
	r.RequestURI = r.URL.RequestURI()
	r.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertions.Equal(http.StatusUnauthorized, w.Code, "a valid MAC cannot substitute the owner credential")
}

func TestRestrictedIngressFreshnessRotationAndLimits(t *testing.T) {
	assertions := assert.New(t)

	s, h, _ := ingressFixture(t, "collections-write")
	send := func(created time.Time, mutate func(*http.Request)) *httptest.ResponseRecorder {
		r, err := http.NewRequest(http.MethodGet, "https://archive.example.test/msgvault/api/v1/health", nil)
		require.NoError(t, err)
		r.Header.Set("X-Api-Key", ingressFixtureAPIKey)
		require.NoError(t, requestsign.Sign(r, bytes.Repeat([]byte{0x42}, 64), "reader-1", created, requestsign.NewNonce()))
		defer func() { _ = r.Body.Close() }()
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/msgvault")
		r.RequestURI = r.URL.RequestURI()
		r.RemoteAddr = "127.0.0.1:12345"
		if mutate != nil {
			mutate(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	assertions.Equal(http.StatusUnauthorized, send(time.Now().Add(-31*time.Second), nil).Code)
	assertions.Equal(http.StatusUnauthorized, send(time.Now().Add(6*time.Second), nil).Code)
	assertions.Equal(http.StatusUnauthorized, send(time.Now(), func(r *http.Request) { r.Header.Add("X-Api-Key", ingressFixtureAPIKey) }).Code)
	assertions.Equal(http.StatusRequestHeaderFieldsTooLarge, send(time.Now(), func(r *http.Request) { r.Header.Set("X-Padding", strings.Repeat("x", 17<<10)) }).Code)
	key := s.remoteIngress.keys["reader-1"]
	key.cfg.NotAfter = time.Now().Add(20 * time.Second).Unix()
	s.remoteIngress.keys["reader-1"] = key
	assertions.Equal(http.StatusUnauthorized, send(time.Now(), nil).Code, "signature lifetime must fit the accepted key window")
	key.cfg.NotAfter = 0
	key.cfg.NotBefore = time.Now().Add(20 * time.Second).Unix()
	s.remoteIngress.keys["reader-1"] = key
	assertions.Equal(http.StatusUnauthorized, send(time.Now(), nil).Code)
	delete(s.remoteIngress.keys, "reader-1")
	assertions.Equal(http.StatusUnauthorized, send(time.Now(), nil).Code, "revoked key")
}

type delayedIngressBody struct {
	io.Reader

	delay time.Duration
}

func (b delayedIngressBody) Read(p []byte) (int, error) {
	time.Sleep(b.delay) //nolint:kennlint // Wait for the signed request to expire during body verification.
	return b.Reader.Read(p)
}
func (b delayedIngressBody) Close() error { return nil }

func TestRestrictedIngressRechecksAfterBodyAndBoundsAdmission(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	s, h, _ := ingressFixture(t, "collections-write")
	r, err := http.NewRequest(http.MethodGet, "https://archive.example.test/msgvault/api/v1/health", strings.NewReader("fixture"))
	requirements.NoError(err)
	r.Header.Set("X-Api-Key", ingressFixtureAPIKey)
	requirements.NoError(requestsign.Sign(r, bytes.Repeat([]byte{0x42}, 64), "reader-1", time.Now().Add(-29*time.Second), requestsign.NewNonce()))
	requirements.NoError(r.Body.Close())
	r.Body = delayedIngressBody{Reader: strings.NewReader("fixture"), delay: 1100 * time.Millisecond}
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/msgvault")
	r.RequestURI = r.URL.RequestURI()
	r.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertions.Equal(http.StatusUnauthorized, w.Code, "signature expired during body verification")
	for range cap(s.remoteIngress.slots) {
		s.remoteIngress.slots <- struct{}{}
	}
	w = ingressRequest(t, h, http.MethodGet, "/api/v1/health", "")
	assertions.Equal(http.StatusTooManyRequests, w.Code)
	for range cap(s.remoteIngress.slots) {
		<-s.remoteIngress.slots
	}
	s.remoteIngress.cfg.MaxRequestBytes = 4
	w = ingressRequest(t, h, http.MethodPost, "/api/v1/cli/collections", `{"name":"fixture"}`)
	assertions.Equal(http.StatusRequestEntityTooLarge, w.Code)
}

func TestDedicatedRemoteCredentialRejectedByKeylessMain(t *testing.T) {
	s, _, _ := ingressFixture(t)
	s.cfg.Server.APIKey = ""
	for _, header := range []http.Header{
		{"X-Api-Key": {ingressFixtureAPIKey}},
		{"X-Api-Key": {"Bearer " + ingressFixtureAPIKey}},
		{"X-Api-Key": {"unrelated", ingressFixtureAPIKey}},
		{"x-api-key": {"Bearer " + ingressFixtureAPIKey}},
		{"Authorization": {"Bearer " + ingressFixtureAPIKey}},
		{"Signature-Input": {"sig1=invalid"}},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		r.Header = header
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, r)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "dedicated claims cannot acquire keyless owner access")
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code, "unsigned private keyless mode remains compatible")
}

func TestRestrictedIngressRejectsEncodedSigningSecretAsAPICredential(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", owner), func(t *testing.T) {
			s, _, _ := ingressFixture(t)
			require.NoError(t, s.remoteIngress.guard.Close())
			s.remoteIngress = nil
			if owner {
				s.cfg.Server.APIKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 64))
			} else {
				client := &s.cfg.Server.RemoteIngress.Clients[0]
				client.APIKeyFile = client.Keys[0].SecretFile
			}
			_, err := s.RestrictedRouter()
			require.Error(t, err, "encoding one secret twice must not count as independent credentials")
		})
	}
}

func TestRestrictedSearchReportsIndexStateWithoutMaintenance(t *testing.T) {
	s, h, _ := ingressFixture(t)
	s.ftsIndexState.Store(cliSearchIndexStateBuilding)
	w := ingressRequest(t, h, http.MethodGet, "/api/v1/cli/search?q=subject:Original", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"index_state":"building"`)
	assert.False(t, s.ftsEnsureRunning.Load(), "restricted reads must not start archive maintenance")
}

func TestRestrictedIngressConcurrentReplay(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	_, h, _ := ingressFixture(t)
	r, err := http.NewRequest(http.MethodGet, "https://archive.example.test/msgvault/api/v1/health", nil)
	requirements.NoError(err)
	r.Header.Set("X-Api-Key", ingressFixtureAPIKey)
	requirements.NoError(requestsign.Sign(r, bytes.Repeat([]byte{0x42}, 64), "reader-1", time.Now(), requestsign.NewNonce()))
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/msgvault")
	r.RequestURI = r.URL.RequestURI()
	r.RemoteAddr = "127.0.0.1:12345"
	var accepted, unavailable atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r.Clone(t.Context()))
			if w.Code == 200 {
				accepted.Add(1)
			}
			if w.Code == 503 {
				unavailable.Add(1)
			}
		})
	}
	wg.Wait()
	assertions.Equal(int64(1), accepted.Load())
	assertions.Zero(unavailable.Load(), "concurrent admission cannot falsely fence the wall clock")
}

func TestRestrictedIngressRejectsUndeclaredWireTrailers(t *testing.T) {
	requirements := require.New(t)

	_, handler, _ := ingressFixture(t)
	server := httptest.NewServer(handler)
	defer server.Close()
	target, err := url.Parse(server.URL)
	requirements.NoError(err)
	conn, err := net.Dial("tcp", target.Host)
	requirements.NoError(err)
	defer func() { _ = conn.Close() }()
	requirements.NoError(conn.SetDeadline(time.Now().Add(30 * time.Second)))
	r, err := http.NewRequest(http.MethodGet, "https://archive.example.test/msgvault/api/v1/health", strings.NewReader("fixture"))
	requirements.NoError(err)
	r.Header.Set("X-Api-Key", ingressFixtureAPIKey)
	requirements.NoError(requestsign.Sign(r, bytes.Repeat([]byte{0x42}, 64), "reader-1", time.Now(), requestsign.NewNonce()))
	defer func() { _ = r.Body.Close() }()
	_, err = fmt.Fprint(conn, "GET /api/v1/health HTTP/1.1\r\nHost: archive.example.test\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n")
	requirements.NoError(err)
	requirements.NoError(r.Header.Write(conn))
	_, err = io.WriteString(conn, "\r\n7\r\nfixture\r\n0\r\nX-Undeclared: fixture\r\n\r\n")
	requirements.NoError(err)
	response, err := http.ReadResponse(bufio.NewReader(conn), r)
	requirements.NoError(err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
}

func TestRestrictedNativeListenerTimeoutCancellationAndShutdown(t *testing.T) {
	requirements := require.New(t)

	const listenerPollEvery = 25 * time.Millisecond
	s, _, _ := ingressFixture(t)
	s.cfg.Server.RemoteIngress.Listen = "127.0.0.1:0"
	requirements.NoError(s.startRestrictedListener())
	address := s.remoteIngress.server.Addr
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	requirements.NoError(err)
	defer func() { _ = conn.Close() }()
	requirements.NoError(conn.SetReadDeadline(time.Now().Add(20 * time.Second)))
	_, err = io.WriteString(conn, "GET /api/v1/health HTTP/1.1\r\nHost: archive.example.test\r\n")
	requirements.NoError(err)
	_, err = bufio.NewReader(conn).ReadByte()
	requirements.ErrorIs(err, io.EOF, "native listener must close incomplete headers")

	conn, err = net.DialTimeout("tcp", address, 10*time.Second)
	requirements.NoError(err)
	r, err := http.NewRequest(http.MethodGet, "https://archive.example.test/msgvault/api/v1/health", nil)
	requirements.NoError(err)
	r.Header.Set("X-Api-Key", ingressFixtureAPIKey)
	requirements.NoError(requestsign.Sign(r, bytes.Repeat([]byte{0x42}, 64), "reader-1", time.Now(), requestsign.NewNonce()))
	_, err = fmt.Fprintf(conn, "GET /api/v1/health HTTP/1.1\r\nHost: archive.example.test\r\nTransfer-Encoding: chunked\r\nX-Api-Key: %s\r\nContent-Type: %s\r\nContent-Digest: %s\r\nSignature-Input: %s\r\nSignature: %s\r\n\r\n", ingressFixtureAPIKey, r.Header.Get("Content-Type"), r.Header.Get("Content-Digest"), r.Header.Get("Signature-Input"), r.Header.Get("Signature"))
	requirements.NoError(err)
	requirements.Eventually(func() bool { return len(s.remoteIngress.slots) == 1 }, 20*time.Second, listenerPollEvery, "valid headers should reserve a bounded body slot")
	requirements.NoError(conn.Close())
	requirements.Eventually(func() bool { return len(s.remoteIngress.slots) == 0 }, 20*time.Second, listenerPollEvery, "disconnect should cancel spooling and release the slot")
	requirements.NoError(s.closeRestrictedIngress(t.Context()))
	_, err = net.DialTimeout("tcp", address, 10*time.Second)
	requirements.Error(err, "shutdown must close the dedicated listener")
	guard, err := requestsign.OpenReplayGuard(s.cfg.Server.RemoteIngress.ReplayStateFile, time.Now(), 10)
	requirements.NoError(err, "clean drain must release the stable replay lock")
	requirements.NoError(guard.Close())
}

func TestRestrictedListenerStopsWhenMainServeFails(t *testing.T) {
	requirements := require.New(t)

	s, _, _ := ingressFixture(t)
	s.cfg.Server.RemoteIngress.Listen = "127.0.0.1:0"
	main, err := net.Listen("tcp", "127.0.0.1:0")
	requirements.NoError(err)
	defer func() { _ = main.Close() }()
	result := make(chan error, 1)
	go func() { result <- s.StartOnListener(main) }()
	requirements.NoError(s.WaitStarted(t.Context()))
	address := s.remoteIngress.server.Addr
	requirements.NoError(main.Close())
	select {
	case err := <-result:
		requirements.Error(err)
	case <-time.After(20 * time.Second):
		requirements.FailNow("main listener did not stop")
	}
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if conn != nil {
		requirements.NoError(conn.Close())
	}
	requirements.Error(err, "main listener failure must not leave remote ingress serving")
}
