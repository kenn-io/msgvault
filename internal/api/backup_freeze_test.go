package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

// newBackupFreezeTestServer returns a server wired with a real serial
// operation gate, so freeze begin/end tests observe genuine gate contention
// against another gated endpoint instead of a canned recording double.
func newBackupFreezeTestServer(cfg *config.Config) *Server {
	if cfg == nil {
		cfg = &config.Config{Server: config.ServerConfig{APIPort: 8080}}
	}
	return NewServerWithOptions(ServerOptions{
		Config:        cfg,
		Logger:        testLogger(),
		OperationGate: NewSerialOperationGate(),
	})
}

func loopbackRequest(target string, body []byte) *http.Request {
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(http.MethodPost, target, nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "127.0.0.1:54321"
	return req
}

// beginBackupFreeze issues a loopback Begin request and returns the token.
func beginBackupFreeze(t *testing.T, srv *Server) string {
	t.Helper()
	req := loopbackRequest(backupFreezeBeginPath, nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, "begin status: %s", resp.Body.String())
	var out backupFreezeBeginResponse
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &out), "decode begin response")
	require.NotEmpty(t, out.Token, "begin token")
	return out.Token
}

func endBackupFreeze(t *testing.T, token string) *http.Request {
	t.Helper()
	body, err := json.Marshal(backupFreezeEndRequest{Token: token})
	require.NoError(t, err, "marshal end request")
	return loopbackRequest(backupFreezeEndPath, body)
}

// gatedProbeStatus posts to a cheap gated route (mirrors the existing
// operation_gate_test.go convention of probing POST /api/v1/accounts) and
// returns the response status.
func gatedProbeStatus(srv *Server) int {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	return resp.Code
}

func TestBackupFreezeBeginBlocksGateUntilEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newBackupFreezeTestServer(nil)

	// Before any freeze, the gated probe reaches the handler (400: no body).
	assert.Equal(http.StatusBadRequest, gatedProbeStatus(srv), "gate open before freeze")

	token := beginBackupFreeze(t, srv)

	probeResp := httptest.NewRecorder()
	probeReq := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", nil)
	srv.Router().ServeHTTP(probeResp, probeReq)
	require.Equal(http.StatusServiceUnavailable, probeResp.Code, "gated probe while frozen: %s", probeResp.Body.String())
	var errResp ErrorResponse
	require.NoError(json.Unmarshal(probeResp.Body.Bytes(), &errResp), "decode error envelope")
	assert.Equal("operation_in_progress", errResp.Error, "error code")
	assert.Contains(errResp.Message, "backup freeze", "message names the freeze holder")

	endResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(endResp, endBackupFreeze(t, token))
	require.Equal(http.StatusOK, endResp.Code, "end status: %s", endResp.Body.String())

	assert.Equal(http.StatusBadRequest, gatedProbeStatus(srv), "gate reopens after end")
}

// TestBackupFreezeBeginReturnsBusyWithinBoundWhenGateHeld pins the fix
// bounding handleBackupFreezeBegin's gate wait with operationGateWaitLimit,
// the same as the generic gate middleware's beginGateWorkBounded. Before the
// fix, Begin queued on the raw, unbounded request context and would hang for
// as long as the holder kept the gate, instead of failing fast with the
// gate-busy response.
func TestBackupFreezeBeginReturnsBusyWithinBoundWhenGateHeld(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })

	srv := newBackupFreezeTestServer(nil)
	lg, ok := srv.operationGate.(LabeledOperationGate)
	require.True(ok, "operation gate must implement LabeledOperationGate")
	release, ok := lg.BeginLabeledWorkContext(context.Background(), "msgvault embeddings build")
	require.True(ok, "occupy gate")
	defer release()

	resp := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.Router().ServeHTTP(resp, loopbackRequest(backupFreezeBeginPath, nil))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow("begin did not return within the bounded gate wait")
	}

	assert.Equal(http.StatusServiceUnavailable, resp.Code, "begin status: %s", resp.Body.String())
	var errResp ErrorResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &errResp), "decode error envelope")
	assert.Equal("operation_in_progress", errResp.Error, "error code")
	assert.Contains(errResp.Message, "msgvault embeddings build", "message names the holder")
}

func TestBackupFreezeSecondBeginWhileActiveFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newBackupFreezeTestServer(nil)

	token := beginBackupFreeze(t, srv)

	secondResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(secondResp, loopbackRequest(backupFreezeBeginPath, nil))
	assert.Equal(http.StatusConflict, secondResp.Code, "second begin status: %s", secondResp.Body.String())
	var errResp ErrorResponse
	require.NoError(json.Unmarshal(secondResp.Body.Bytes(), &errResp), "decode error envelope")
	assert.Equal("backup_freeze_active", errResp.Error, "error code")

	// Clean up: End with the original token still succeeds.
	endResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(endResp, endBackupFreeze(t, token))
	assert.Equal(http.StatusOK, endResp.Code, "end status: %s", endResp.Body.String())
}

func TestBackupFreezeEndRejectsBogusTokenThenSucceedsOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newBackupFreezeTestServer(nil)

	token := beginBackupFreeze(t, srv)

	bogusResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(bogusResp, endBackupFreeze(t, "not-the-real-token"))
	require.Equal(http.StatusBadRequest, bogusResp.Code, "bogus token status: %s", bogusResp.Body.String())
	var errResp ErrorResponse
	require.NoError(json.Unmarshal(bogusResp.Body.Bytes(), &errResp), "decode error envelope")
	assert.Equal("backup_freeze_not_active", errResp.Error, "error code")

	firstEndResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(firstEndResp, endBackupFreeze(t, token))
	require.Equal(http.StatusOK, firstEndResp.Code, "first end status: %s", firstEndResp.Body.String())

	secondEndResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(secondEndResp, endBackupFreeze(t, token))
	assert.Equal(http.StatusBadRequest, secondEndResp.Code, "second end with same token should fail")
}

func TestBackupFreezeWatchdogAutoReleasesGateAndInvalidatesToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	oldTimeout := backupFreezeWatchdogTimeout
	backupFreezeWatchdogTimeout = 50 * time.Millisecond
	t.Cleanup(func() { backupFreezeWatchdogTimeout = oldTimeout })

	buf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Logger:        logger,
		OperationGate: NewSerialOperationGate(),
	})

	token := beginBackupFreeze(t, srv)

	require.Eventually(func() bool {
		return gatedProbeStatus(srv) == http.StatusBadRequest
	}, 2*time.Second, 10*time.Millisecond, "gate should auto-release after watchdog fires")

	require.Eventually(func() bool {
		return strings.Contains(buf.String(), "backup freeze watchdog fired")
	}, 2*time.Second, 10*time.Millisecond, "watchdog should log at error level")
	logLine := findJSONLogLine(t, buf.String(), "backup freeze watchdog fired; releasing operation gate")
	assert.Equal("ERROR", logLine["level"], "watchdog log level")
	assert.Equal(token, logLine["token"], "watchdog log token")

	endResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(endResp, endBackupFreeze(t, token))
	assert.Equal(http.StatusBadRequest, endResp.Code, "end after watchdog fire should fail: %s", endResp.Body.String())
	var errResp ErrorResponse
	require.NoError(json.Unmarshal(endResp.Body.Bytes(), &errResp), "decode error envelope")
	assert.Equal("backup_freeze_not_active", errResp.Error, "error code")
}

func TestBackupFreezeBeginSameHostOnly(t *testing.T) {
	srv := newBackupFreezeTestServer(nil)

	// Pretend this machine owns 192.168.50.5: a daemon bound to its LAN
	// address sees same-host freeze calls arrive from that address.
	oldAddrs := backupFreezeLocalAddrs
	backupFreezeLocalAddrs = func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("192.168.50.5"), Mask: net.CIDRMask(24, 32)},
		}, nil
	}
	t.Cleanup(func() { backupFreezeLocalAddrs = oldAddrs })

	cases := []struct {
		name       string
		remoteAddr string
		wantStatus int
	}{
		{"loopback allowed", "127.0.0.1:54321", http.StatusOK},
		{"ipv6 loopback allowed", "[::1]:54321", http.StatusOK},
		{"own interface address allowed", "192.168.50.5:54321", http.StatusOK},
		{"same subnet but not this host blocked", "192.168.50.6:54321", http.StatusNotFound},
		{"remote blocked", "8.8.8.8:54321", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			req := httptest.NewRequest(http.MethodPost, backupFreezeBeginPath, nil)
			req.RemoteAddr = tc.remoteAddr
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			assert.Equal(tc.wantStatus, resp.Code, "body: %s", resp.Body.String())

			// Clean up so a passing case does not leave a freeze active for
			// the next table row.
			if resp.Code == http.StatusOK {
				var out backupFreezeBeginResponse
				require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &out), "decode begin response")
				endResp := httptest.NewRecorder()
				srv.Router().ServeHTTP(endResp, endBackupFreeze(t, out.Token))
				require.Equal(t, http.StatusOK, endResp.Code, "cleanup end status")
			}
		})
	}
}

func TestBackupFreezeBeginRequiresAuthWhenKeyConfigured(t *testing.T) {
	const key = "secret-key"
	srv := newBackupFreezeTestServer(&config.Config{Server: config.ServerConfig{APIKey: key}})

	oldAddrs := backupFreezeLocalAddrs
	backupFreezeLocalAddrs = func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("192.168.50.5"), Mask: net.CIDRMask(24, 32)},
		}, nil
	}
	t.Cleanup(func() { backupFreezeLocalAddrs = oldAddrs })

	cases := []struct {
		name       string
		remoteAddr string
		reqKey     string
		wantStatus int
	}{
		{"loopback valid key allowed", "127.0.0.1:54321", key, http.StatusOK},
		{"loopback missing key blocked", "127.0.0.1:54321", "", http.StatusUnauthorized},
		{"loopback bad key blocked", "127.0.0.1:54321", "wrong", http.StatusUnauthorized},
		{"own interface valid key allowed", "192.168.50.5:54321", key, http.StatusOK},
		{"own interface missing key blocked", "192.168.50.5:54321", "", http.StatusUnauthorized},
		{"remote valid key blocked", "8.8.8.8:54321", key, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			req := httptest.NewRequest(http.MethodPost, backupFreezeBeginPath, nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.reqKey != "" {
				req.Header.Set("X-Api-Key", tc.reqKey)
			}
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			assert.Equal(tc.wantStatus, resp.Code, "body: %s", resp.Body.String())

			if resp.Code == http.StatusOK {
				var out backupFreezeBeginResponse
				require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &out), "decode begin response")
				endReq := endBackupFreeze(t, out.Token)
				endReq.Header.Set("X-Api-Key", tc.reqKey)
				endResp := httptest.NewRecorder()
				srv.Router().ServeHTTP(endResp, endReq)
				require.Equal(t, http.StatusOK, endResp.Code, "cleanup end status")
			}
		})
	}
}

func TestBackupFreezeEndRejectsInvalidJSON(t *testing.T) {
	assert := assert.New(t)
	srv := newBackupFreezeTestServer(nil)

	req := loopbackRequest(backupFreezeEndPath, []byte("not json"))
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Equal(http.StatusBadRequest, resp.Code, "status: %s", resp.Body.String())
}
